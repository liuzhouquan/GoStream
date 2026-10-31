package im_test

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"
)

func verifyLoadOffline(t *testing.T, ctx context.Context, db *sql.DB, a, b *process, users []account, devices []string, sockets []*socket, all map[string]*loadMessage, report *loadReport) {
	t.Helper()
	if err := sockets[0].conn.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "offline receiver socket removed", 5*time.Second, func() bool { r, err := loadResources(ctx, a.base, time.Now()); return err == nil && r.Connections == 9 })
	var ids []uint
	for i := range 125 {
		key := fmt.Sprintf("load-offline-%03d", i)
		body := fmt.Sprintf("offline-%03d", i)
		id := send(t, sockets[10], "message", key, body, users[0].User.ID)
		ids = append(ids, id)
		all[key] = &loadMessage{Sender: 10, Recipient: 0, Body: body, ID: id}
	}
	report.Offline = len(ids)
	// Drain notification work before reconnect so a batch of delayed notices does
	// not dominate the replay observation. Only explicit notification frames may
	// be ignored below; unexpected message/control frames always fail.
	eventually(t, "load outbox drained", 60*time.Second, func() bool {
		var pending, dead int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL AND dead_at IS NULL").Scan(&pending); err != nil {
			return false
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox_events WHERE dead_at IS NOT NULL").Scan(&dead); err != nil {
			return false
		}
		return pending == 0 && dead == 0
	})
	reconnect := connect(t, a.base, users[0], devices[0])
	reconnect.write(t, map[string]string{"type": "ping"})
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	replayed, synced, done := 0, false, false
	for !done {
		select {
		case e, ok := <-reconnect.read:
			if !ok {
				t.Fatal("replay socket closed")
			}
			switch e.Type {
			case "notification":
			case "message":
				if replayed >= 100 || synced || e.Message.ID != ids[replayed] || e.Message.SenderID != users[10].User.ID || e.Message.RecipientID != users[0].User.ID || e.Message.ClientID != fmt.Sprintf("load-offline-%03d", replayed) || e.Message.Body != fmt.Sprintf("offline-%03d", replayed) {
					t.Fatalf("unexpected replay message at index %d", replayed)
				}
				replayed++
			case "sync_required":
				if replayed != 100 || synced {
					t.Fatal("incorrect replay sync marker")
				}
				synced = true
			case "pong":
				if !synced || replayed != 100 {
					t.Fatal("pong before complete bounded replay")
				}
				done = true
			default:
				t.Fatalf("unexpected replay frame %q", e.Type)
			}
		case <-timeout.C:
			t.Fatal("bounded offline replay timed out")
		}
	}
	var unread []uint
	cursor := uint(0)
	for {
		var p page
		call(t, b.base, "GET", fmt.Sprintf("/api/messages/unread?limit=37&after_id=%d", cursor), users[0], devices[0], nil, 200, &p)
		if len(p.Items) > 37 {
			t.Fatal("unread page exceeds limit")
		}
		for _, m := range p.Items {
			if len(unread) >= len(ids) || m.ID != ids[len(unread)] || m.ID <= cursor || m.ClientID != fmt.Sprintf("load-offline-%03d", len(unread)) || m.Body != fmt.Sprintf("offline-%03d", len(unread)) || m.SenderID != users[10].User.ID || m.RecipientID != users[0].User.ID {
				t.Fatal("offline unread missing, duplicate or wrong message")
			}
			unread = append(unread, m.ID)
		}
		if len(p.Items) > 0 && p.Next != p.Items[len(p.Items)-1].ID {
			t.Fatal("wrong unread cursor")
		}
		if !p.More {
			break
		}
		if p.Next <= cursor {
			t.Fatal("unread cursor did not advance")
		}
		cursor = p.Next
	}
	if !slices.Equal(unread, ids) {
		t.Fatal("offline unread set incomplete")
	}
	var wantHistory []uint
	for _, m := range all {
		if m.Sender == 10 && m.Recipient == 0 {
			wantHistory = append(wantHistory, m.ID)
		}
	}
	slices.Sort(wantHistory)
	slices.Reverse(wantHistory)
	var history []uint
	cursor = 0
	for {
		var p page
		call(t, b.base, "GET", fmt.Sprintf("/api/messages?user_id=%d&limit=37&before_id=%d", users[10].User.ID, cursor), users[0], devices[0], nil, 200, &p)
		if len(p.Items) > 37 {
			t.Fatal("history page exceeds limit")
		}
		for _, m := range p.Items {
			if len(history) >= len(wantHistory) || m.ID != wantHistory[len(history)] || m.SenderID != users[10].User.ID || m.RecipientID != users[0].User.ID {
				t.Fatal("history page missing or wrong identity")
			}
			want, ok := all[m.ClientID]
			if !ok || want.ID != m.ID || want.Body != m.Body {
				t.Fatal("history payload mismatch")
			}
			history = append(history, m.ID)
		}
		if len(p.Items) > 0 && p.Next != p.Items[len(p.Items)-1].ID {
			t.Fatal("wrong history cursor")
		}
		if !p.More {
			break
		}
		if p.Next == 0 || (cursor > 0 && p.Next >= cursor) {
			t.Fatal("history cursor did not decrease")
		}
		cursor = p.Next
	}
	if !slices.Equal(history, wantHistory) {
		t.Fatal("history set incomplete")
	}
	for offset := 0; offset < len(ids); offset += 100 {
		batch := ids[offset:min(offset+100, len(ids))]
		for retry := range 2 {
			var ack struct {
				Count int `json:"acked_count"`
			}
			call(t, b.base, "POST", "/api/messages/acks", users[0], devices[0], map[string]any{"ids": batch}, 200, &ack)
			want := len(batch)
			if retry == 1 {
				want = 0
			}
			if ack.Count != want {
				t.Fatal("offline ACK lost idempotency")
			}
		}
	}
	var after page
	call(t, a.base, "GET", "/api/messages/unread?limit=100", users[0], devices[0], nil, 200, &after)
	if len(after.Items) != 0 || after.More || after.Next != 0 {
		t.Fatal("ACKed device still unread")
	}
	// New device inherits none of the 150 online receipts or 125 offline receipts.
	var expectedNew []uint
	for _, m := range all {
		if m.Recipient == 0 {
			expectedNew = append(expectedNew, m.ID)
		}
	}
	slices.Sort(expectedNew)
	newDevice := randomID(t)
	cursor = 0
	var gotNew []uint
	for {
		var p page
		call(t, b.base, "GET", fmt.Sprintf("/api/messages/unread?limit=37&after_id=%d", cursor), users[0], newDevice, nil, 200, &p)
		for _, m := range p.Items {
			gotNew = append(gotNew, m.ID)
		}
		if !p.More {
			break
		}
		if p.Next <= cursor {
			t.Fatal("new device cursor did not advance")
		}
		cursor = p.Next
	}
	if len(expectedNew) != 275 || !slices.Equal(gotNew, expectedNew) {
		t.Fatal("new device lost independent unread backlog")
	}
}
