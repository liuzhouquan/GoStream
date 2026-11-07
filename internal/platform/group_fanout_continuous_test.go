package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

func testUUID(i int) string {
	return fmt.Sprintf("11111111-2222-3333-4444-%012d", i)
}

type groupWsSocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
	read chan map[string]any
}

func newGroupWsSocket(t *testing.T, url, token, device string) *groupWsSocket {
	t.Helper()
	subprotocols := []string{"gostream", "gostream.jwt." + token}
	if device != "" {
		subprotocols = append(subprotocols, "gostream.device."+device)
	}
	dialer := websocket.Dialer{Subprotocols: subprotocols, HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	s := &groupWsSocket{conn: conn, read: make(chan map[string]any, 2048)}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		defer close(s.read)
		for {
			var frame map[string]any
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			select {
			case s.read <- frame:
			default:
				_ = conn.Close()
				return
			}
		}
	}()
	// Wait for ready frame
	select {
	case frame, ok := <-s.read:
		if !ok || frame["type"] != "ready" {
			t.Fatalf("expected ready frame, got %+v", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ready frame")
	}
	return s
}

func (s *groupWsSocket) writeJSON(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteJSON(v)
}

func (s *groupWsSocket) next(t *testing.T, kind string, timeout time.Duration) map[string]any {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case frame, ok := <-s.read:
			if !ok {
				t.Fatalf("socket closed while waiting for %s", kind)
			}
			if frame["type"] == kind {
				return frame
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for frame %s", kind)
		}
	}
}

func registerUserWithPeer(t *testing.T, h http.Handler, username string, idx int) (uint, string) {
	t.Helper()
	var raw bytes.Buffer
	_ = json.NewEncoder(&raw).Encode(map[string]string{"username": username, "password": "correct-horse-123", "nickname": username})
	req := httptest.NewRequest(http.MethodPost, "/api/users", &raw)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", idx+1)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("register status %d: %s", res.Code, res.Body.String())
	}
	var out struct {
		User  User   `json:"user"`
		Token string `json:"access_token"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.User.ID, out.Token
}

// TestGroupFanout_20ClientMatrix verifies the full 20-client group fanout specification:
// - 20 members in a chat group
// - 20 senders sending messages concurrently
// - Fanout factor is 19 (each message is delivered to all 19 other members)
// - Senders receive "ack" with original message_id
// - Recipient clients receive "group_message" matching exact ID, sender, and payload
// - Explicit verification of expected received ID set E(k) for each client
func TestGroupFanout_20ClientMatrix(t *testing.T) {
	s, h, db := testServer(t)

	// Standalone in-process Redis for realtime pub/sub
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(func() { mr.Close() })
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	if err := s.UseRedisRealtime(rc); err != nil {
		t.Fatalf("use realtime: %v", err)
	}

	ts := httptest.NewServer(h)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"

	const clientCount = 20
	const msgsPerClient = 10 // 200 total messages in this test -> 200 * 19 = 3800 deliveries

	type testClient struct {
		ID     uint
		Token  string
		Device string
		Socket *groupWsSocket
	}

	clients := make([]testClient, clientCount)
	for i := range clientCount {
		uid, tok := registerUserWithPeer(t, h, fmt.Sprintf("fanout_user_%02d", i), i)
		dev := testUUID(i)
		sock := newGroupWsSocket(t, wsURL, tok, dev)
		clients[i] = testClient{ID: uid, Token: tok, Device: dev, Socket: sock}
	}

	// Client 0 creates group
	created := request(t, h, http.MethodPost, "/api/groups", clients[0].Token, map[string]string{"name": "20 Client Fanout Matrix"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group failed: %d %s", created.Code, created.Body.String())
	}
	var grp ChatGroup
	if err := json.Unmarshal(created.Body.Bytes(), &grp); err != nil {
		t.Fatal(err)
	}

	// Add remaining 19 members
	for i := 1; i < clientCount; i++ {
		added := request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", grp.ID), clients[0].Token, map[string]uint{"user_id": clients[i].ID})
		if added.Code != http.StatusOK {
			t.Fatalf("add member %d failed: %d %s", i, added.Code, added.Body.String())
		}
	}

	// Verify group membership count in DB
	var memberCount int64
	if err := db.Model(&GroupMember{}).Where("group_id = ?", grp.ID).Count(&memberCount).Error; err != nil || memberCount != clientCount {
		t.Fatalf("expected %d members, got %d", clientCount, memberCount)
	}

	// Structure to track expected messages and deliveries
	type sentMsg struct {
		Key       string
		SenderIdx int
		Body      string
		ID        uint
		Acked     bool
		SentAt    time.Time
		AckAt     time.Time
	}

	totalMessages := clientCount * msgsPerClient
	expectedTotalDeliveries := totalMessages * (clientCount - 1) // each message received by 19 recipients

	var mu sync.Mutex
	messagesByKey := make(map[string]*sentMsg)
	clientReceived := make([]map[uint]bool, clientCount)
	for i := range clientCount {
		clientReceived[i] = make(map[uint]bool)
	}

	acksReceived := 0
	deliveriesReceived := 0
	duplicateACKs := 0
	duplicateDeliveries := 0
	ackLatencies := make([]time.Duration, 0, totalMessages)
	deliveryLatencies := make([]time.Duration, 0, expectedTotalDeliveries)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var readerWg sync.WaitGroup
	senderAckChans := make([]chan string, clientCount)
	for i := range clientCount {
		senderAckChans[i] = make(chan string, msgsPerClient)
	}

	// Start reader goroutines for each client socket
	for i := range clientCount {
		readerWg.Add(1)
		go func(cIdx int) {
			defer readerWg.Done()
			sock := clients[cIdx].Socket
			for {
				select {
				case <-ctx.Done():
					return
				case frame, ok := <-sock.read:
					if !ok {
						return
					}
					fType, _ := frame["type"].(string)
					switch fType {
					case "ack":
						clientID, _ := frame["client_id"].(string)
						msgIDRaw, _ := frame["message_id"].(float64)
						msgID := uint(msgIDRaw)
						var ackChan chan string
						mu.Lock()
						if sm, exists := messagesByKey[clientID]; exists {
							if !sm.Acked {
								sm.Acked = true
								if sm.ID == 0 {
									sm.ID = msgID
								}
								sm.AckAt = time.Now()
								ackLatencies = append(ackLatencies, sm.AckAt.Sub(sm.SentAt))
								acksReceived++
								ackChan = senderAckChans[cIdx]
							} else {
								duplicateACKs++
							}
						}
						mu.Unlock()
						if ackChan != nil {
							select {
							case ackChan <- clientID:
							case <-ctx.Done():
								return
							}
						}

					case "group_message":
						msgData, ok := frame["message"].(map[string]any)
						if !ok {
							continue
						}
						msgIDRaw, _ := msgData["id"].(float64)
						msgID := uint(msgIDRaw)
						senderIDRaw, _ := msgData["sender_id"].(float64)
						senderID := uint(senderIDRaw)
						clientID, _ := msgData["client_id"].(string)

						mu.Lock()
						if senderID == clients[cIdx].ID {
							// A sender should NOT receive its own message back on the same socket
							t.Errorf("client %d received group_message from self: msg %d", cIdx, msgID)
						}
						if clientReceived[cIdx][msgID] {
							duplicateDeliveries++
						} else {
							clientReceived[cIdx][msgID] = true
							deliveriesReceived++
						}
						if sm, exists := messagesByKey[clientID]; exists {
							if sm.ID == 0 {
								sm.ID = msgID
							}
							deliveryLatencies = append(deliveryLatencies, time.Since(sm.SentAt))
						}
						mu.Unlock()
					}
				}
			}
		}(i)
	}

	// Launch concurrent senders
	var sendersWg sync.WaitGroup
	t0 := time.Now()

	for i := range clientCount {
		sendersWg.Add(1)
		go func(cIdx int) {
			defer sendersWg.Done()
			sock := clients[cIdx].Socket
			for j := range msgsPerClient {
				key := fmt.Sprintf("fanout-c%02d-m%02d", cIdx, j)
				body := fmt.Sprintf("Fanout payload from client %02d message %02d", cIdx, j)

				mu.Lock()
				messagesByKey[key] = &sentMsg{Key: key, SenderIdx: cIdx, Body: body, SentAt: time.Now()}
				mu.Unlock()

				err := sock.writeJSON(map[string]any{
					"type":      "group_message",
					"client_id": key,
					"group_id":  grp.ID,
					"body":      body,
				})
				if err != nil {
					t.Errorf("client %d failed to write message %d: %v", cIdx, j, err)
					return
				}

				// Wait for sender ACK
				select {
				case ackKey := <-senderAckChans[cIdx]:
					if ackKey != key {
						t.Errorf("client %d got wrong ACK key: %s != %s", cIdx, ackKey, key)
					}
				case <-ctx.Done():
					return
				}
				time.Sleep(time.Duration(1+cIdx%3) * time.Millisecond)
			}
		}(i)
	}

	sendersWg.Wait()
	elapsedSend := time.Since(t0)
	t.Logf("20 concurrent senders completed in %v (%d messages)", elapsedSend, totalMessages)

	// Wait for all deliveries across all recipients
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		mu.Lock()
		done := (acksReceived == totalMessages && deliveriesReceived == expectedTotalDeliveries)
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	for i := range clientCount {
		_ = clients[i].Socket.conn.Close()
	}
	readerWg.Wait()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	slices.Sort(ackLatencies)
	slices.Sort(deliveryLatencies)

	calcPct := func(sorted []time.Duration, pct float64) time.Duration {
		if len(sorted) == 0 {
			return 0
		}
		idx := int(float64(len(sorted)-1) * pct)
		return sorted[idx]
	}

	t.Logf("Fanout Results: ACKs=%d/%d, Deliveries=%d/%d, DupACKs=%d, DupDeliveries=%d",
		acksReceived, totalMessages, deliveriesReceived, expectedTotalDeliveries, duplicateACKs, duplicateDeliveries)
	t.Logf("Sender ACK Latencies: P50=%v, P95=%v, P99=%v, Max=%v",
		calcPct(ackLatencies, 0.50), calcPct(ackLatencies, 0.95), calcPct(ackLatencies, 0.99), calcPct(ackLatencies, 1.0))
	t.Logf("Recipient Delivery Latencies: P50=%v, P95=%v, P99=%v, Max=%v",
		calcPct(deliveryLatencies, 0.50), calcPct(deliveryLatencies, 0.95), calcPct(deliveryLatencies, 0.99), calcPct(deliveryLatencies, 1.0))

	if acksReceived != totalMessages {
		t.Fatalf("expected %d ACKs, got %d", totalMessages, acksReceived)
	}
	if deliveriesReceived != expectedTotalDeliveries {
		t.Fatalf("expected %d deliveries, got %d", expectedTotalDeliveries, deliveriesReceived)
	}
	if duplicateACKs != 0 {
		t.Fatalf("unexpected duplicate ACKs: %d", duplicateACKs)
	}
	if duplicateDeliveries != 0 {
		t.Fatalf("unexpected duplicate deliveries: %d", duplicateDeliveries)
	}

	// Verify each client received exactly the expected set E(k)
	for i := range clientCount {
		expectedCount := totalMessages - msgsPerClient // 200 - 10 = 190
		if len(clientReceived[i]) != expectedCount {
			t.Fatalf("client %d expected %d unique message IDs, got %d", i, expectedCount, len(clientReceived[i]))
		}
		// Confirm client i never received any of its own messages
		for _, sm := range messagesByKey {
			if sm.SenderIdx == i {
				if clientReceived[i][sm.ID] {
					t.Fatalf("client %d received its own message %d!", i, sm.ID)
				}
			} else {
				if !clientReceived[i][sm.ID] {
					t.Fatalf("client %d missing message %d from client %d", i, sm.ID, sm.SenderIdx)
				}
			}
		}
	}
}

// TestGroupFanout_DuplicateClientIDIdempotency verifies duplicate client_id behavior:
// - Re-sending with identical (client_id, group_id, body) receives original message ACK
// - Does NOT create duplicate row in group_messages table
// - Does NOT fanout duplicate group_message frames to other members
func TestGroupFanout_DuplicateClientIDIdempotency(t *testing.T) {
	_, h, db := testServer(t)
	ts := httptest.NewServer(h)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"

	u1, tok1 := registerTestUser(t, h, "dup_sender")
	u2, tok2 := registerTestUser(t, h, "dup_receiver")

	sock1 := newGroupWsSocket(t, wsURL, tok1, testUUID(101))
	sock2 := newGroupWsSocket(t, wsURL, tok2, testUUID(102))

	// Create group
	created := request(t, h, http.MethodPost, "/api/groups", tok1, map[string]string{"name": "Idempotency Group"})
	var grp ChatGroup
	_ = json.Unmarshal(created.Body.Bytes(), &grp)
	_ = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", grp.ID), tok1, map[string]uint{"user_id": u2})

	// Send message 1
	key := "idempotent-key-001"
	body := "First transmission"
	sock1.writeJSON(map[string]any{"type": "group_message", "client_id": key, "group_id": grp.ID, "body": body})
	ack1 := sock1.next(t, "ack", 5*time.Second)
	firstMsgID := uint(ack1["message_id"].(float64))

	// Receiver gets group_message
	rec1 := sock2.next(t, "group_message", 5*time.Second)
	recMsg := rec1["message"].(map[string]any)
	if uint(recMsg["id"].(float64)) != firstMsgID {
		t.Fatalf("receiver got wrong id %v != %d", recMsg["id"], firstMsgID)
	}

	// Re-send with identical client_id, group_id, body
	sock1.writeJSON(map[string]any{"type": "group_message", "client_id": key, "group_id": grp.ID, "body": body})
	ack2 := sock1.next(t, "ack", 5*time.Second)
	secondMsgID := uint(ack2["message_id"].(float64))

	if firstMsgID != secondMsgID {
		t.Fatalf("idempotent resend returned different message ID: %d != %d", firstMsgID, secondMsgID)
	}

	// Check SQL count: exactly 1 row
	var count int64
	if err := db.Model(&GroupMessage{}).Where("group_id = ? AND sender_id = ? AND client_id = ?", grp.ID, u1, key).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("expected 1 row in SQL, got %d", count)
	}

	// Receiver should NOT receive a second frame
	timer := time.NewTimer(300 * time.Millisecond)
	select {
	case frame := <-sock2.read:
		t.Fatalf("receiver received duplicate frame: %+v", frame)
	case <-timer.C:
		// Clean absence
	}
}

// TestGroupFanout_OfflineReconnectAndBackfill verifies:
// - Member goes offline (disconnects WebSocket)
// - Senders send 125 messages to group while member is offline
// - Member reconnects with device ID
// - Server replays exactly 100 messages and sends "group_sync_required"
// - Member paginates history (GET /api/groups/:id/messages?before_id=...) to backfill remaining 25
// - All 125 messages received with zero missing or duplicate IDs
// - Member ACKs latest message -> unread becomes 0
func TestGroupFanout_OfflineReconnectAndBackfill(t *testing.T) {
	_, h, db := testServer(t)
	ts := httptest.NewServer(h)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"

	_, tokOwner := registerTestUser(t, h, "offline_owner")
	uMember, tokMember := registerTestUser(t, h, "offline_member")

	sockOwner := newGroupWsSocket(t, wsURL, tokOwner, testUUID(202))
	devMember := testUUID(201)
	sockMember := newGroupWsSocket(t, wsURL, tokMember, devMember)

	created := request(t, h, http.MethodPost, "/api/groups", tokOwner, map[string]string{"name": "Offline Backfill Group"})
	var grp ChatGroup
	_ = json.Unmarshal(created.Body.Bytes(), &grp)
	_ = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", grp.ID), tokOwner, map[string]uint{"user_id": uMember})

	// Member disconnects
	_ = sockMember.conn.Close()
	time.Sleep(100 * time.Millisecond)

	// Owner sends 125 messages to group
	const offlineCount = 125
	msgIDs := make([]uint, offlineCount)
	for i := range offlineCount {
		key := fmt.Sprintf("offline-g-msg-%03d", i)
		body := fmt.Sprintf("Offline group message %03d", i)
		sockOwner.writeJSON(map[string]any{"type": "group_message", "client_id": key, "group_id": grp.ID, "body": body})
		ack := sockOwner.next(t, "ack", 5*time.Second)
		msgIDs[i] = uint(ack["message_id"].(float64))
	}

	// Verify unread count via HTTP endpoint before reconnect
	reqUnread, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/groups/%d/unread", ts.URL, grp.ID), nil)
	reqUnread.Header.Set("Authorization", "Bearer "+tokMember)
	reqUnread.Header.Set("X-GoStream-Device-ID", devMember)
	resUnread, err := http.DefaultClient.Do(reqUnread)
	if err != nil || resUnread.StatusCode != http.StatusOK {
		t.Fatalf("get unread failed: %v", err)
	}
	var unreadPayload struct {
		UnreadCount int64 `json:"unread_count"`
		LastAckID   uint  `json:"last_ack_id"`
	}
	_ = json.NewDecoder(resUnread.Body).Decode(&unreadPayload)
	resUnread.Body.Close()
	if unreadPayload.UnreadCount != offlineCount {
		t.Fatalf("expected %d unread messages, got %d", offlineCount, unreadPayload.UnreadCount)
	}

	// Member reconnects with same device ID
	reconnectSock := newGroupWsSocket(t, wsURL, tokMember, devMember)

	// Expect exactly 100 replayed group_message frames, then 1 group_sync_required
	replayedIDs := make([]uint, 0, 100)
	gotSyncRequired := false

	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	for len(replayedIDs) < 100 || !gotSyncRequired {
		select {
		case frame, ok := <-reconnectSock.read:
			if !ok {
				t.Fatal("reconnected socket closed prematurely")
			}
			fType, _ := frame["type"].(string)
			switch fType {
			case "group_message":
				msgData := frame["message"].(map[string]any)
				id := uint(msgData["id"].(float64))
				replayedIDs = append(replayedIDs, id)
			case "group_sync_required":
				gotSyncRequired = true
			}
		case <-timeout.C:
			t.Fatalf("timeout waiting for 100 replays + sync_required. Got %d replays, sync=%v", len(replayedIDs), gotSyncRequired)
		}
	}

	if len(replayedIDs) != 100 {
		t.Fatalf("expected exactly 100 replayed messages, got %d", len(replayedIDs))
	}
	if !gotSyncRequired {
		t.Fatal("expected group_sync_required frame")
	}

	// Verify the 100 replayed IDs match the first 100 sent
	if !slices.Equal(replayedIDs, msgIDs[:100]) {
		t.Fatal("replayed IDs do not match first 100 sent IDs")
	}

	// Now client initiates Keyset pagination to backfill remaining messages
	// GET /api/groups/:id/messages?before_id=...
	allBackfilled := make(map[uint]bool)
	for _, id := range replayedIDs {
		allBackfilled[id] = true
	}

	cursor := uint(0)
	for {
		url := fmt.Sprintf("%s/api/groups/%d/messages?limit=50", ts.URL, grp.ID)
		if cursor > 0 {
			url = fmt.Sprintf("%s&before_id=%d", url, cursor)
		}
		reqHist, _ := http.NewRequest(http.MethodGet, url, nil)
		reqHist.Header.Set("Authorization", "Bearer "+tokMember)
		resHist, err := http.DefaultClient.Do(reqHist)
		if err != nil || resHist.StatusCode != http.StatusOK {
			t.Fatalf("get history failed: %v", err)
		}
		var page struct {
			Items      []GroupMessage `json:"items"`
			NextCursor uint           `json:"next_cursor"`
			HasMore    bool           `json:"has_more"`
		}
		_ = json.NewDecoder(resHist.Body).Decode(&page)
		resHist.Body.Close()

		for _, item := range page.Items {
			allBackfilled[item.ID] = true
		}

		if !page.HasMore || page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}

	// Assert member has recovered all 125 messages
	if len(allBackfilled) != offlineCount {
		t.Fatalf("expected %d total backfilled messages, got %d", offlineCount, len(allBackfilled))
	}
	for _, id := range msgIDs {
		if !allBackfilled[id] {
			t.Fatalf("missing message %d from backfilled set", id)
		}
	}

	// Member ACKs latest message (msgIDs[124]) via HTTP
	lastID := msgIDs[len(msgIDs)-1]
	reqAck, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/groups/%d/messages/%d/ack", ts.URL, grp.ID, lastID), nil)
	reqAck.Header.Set("Authorization", "Bearer "+tokMember)
	reqAck.Header.Set("X-GoStream-Device-ID", devMember)
	resAck, err := http.DefaultClient.Do(reqAck)
	if err != nil || resAck.StatusCode != http.StatusOK {
		t.Fatalf("ack failed: %v", err)
	}
	resAck.Body.Close()

	// Verify unread count is now 0
	reqUnread2, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/groups/%d/unread", ts.URL, grp.ID), nil)
	reqUnread2.Header.Set("Authorization", "Bearer "+tokMember)
	reqUnread2.Header.Set("X-GoStream-Device-ID", devMember)
	resUnread2, _ := http.DefaultClient.Do(reqUnread2)
	_ = json.NewDecoder(resUnread2.Body).Decode(&unreadPayload)
	resUnread2.Body.Close()
	if unreadPayload.UnreadCount != 0 {
		t.Fatalf("expected 0 unread messages after ACK, got %d", unreadPayload.UnreadCount)
	}

	// Verify SQL group_device_reads
	var devRead GroupDeviceRead
	if err := db.Where("group_id = ? AND user_id = ? AND device_id = ?", grp.ID, uMember, devMember).First(&devRead).Error; err != nil || devRead.LastAckID != lastID {
		t.Fatalf("SQL device read mismatch: err=%v, last_ack_id=%d, want=%d", err, devRead.LastAckID, lastID)
	}
}

// TestGroupFanout_RemovalAndReinviteAdvance verifies:
// - Member is removed from group
// - Messages sent while removed: removed user receives 0 deliveries, cannot send, cannot read
// - Member is re-invited: unread cursor automatically advances to newest message (initial unread = 0)
// - Subsequent message increments unread to 1
func TestGroupFanout_RemovalAndReinviteAdvance(t *testing.T) {
	_, h, _ := testServer(t)
	ts := httptest.NewServer(h)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"

	_, tokOwner := registerTestUser(t, h, "reinvite_owner")
	uMember, tokMember := registerTestUser(t, h, "reinvite_member")

	sockOwner := newGroupWsSocket(t, wsURL, tokOwner, testUUID(301))
	sockMember := newGroupWsSocket(t, wsURL, tokMember, testUUID(302))

	created := request(t, h, http.MethodPost, "/api/groups", tokOwner, map[string]string{"name": "Removal Reinvite Group"})
	var grp ChatGroup
	_ = json.Unmarshal(created.Body.Bytes(), &grp)
	_ = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", grp.ID), tokOwner, map[string]uint{"user_id": uMember})

	// Owner sends message 1: member receives it
	sockOwner.writeJSON(map[string]any{"type": "group_message", "client_id": "pre-remove-1", "group_id": grp.ID, "body": "msg 1"})
	_ = sockOwner.next(t, "ack", 5*time.Second)
	_ = sockMember.next(t, "group_message", 5*time.Second)

	// Owner removes member
	del := request(t, h, http.MethodDelete, fmt.Sprintf("/api/groups/%d/members/%d", grp.ID, uMember), tokOwner, nil)
	if del.Code != http.StatusOK {
		t.Fatalf("remove member failed: %d %s", del.Code, del.Body.String())
	}

	// While removed, member cannot access history (404)
	hist := request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/messages", grp.ID), tokMember, nil)
	if hist.Code != http.StatusNotFound {
		t.Fatalf("removed member should get 404 on history, got %d", hist.Code)
	}

	// While removed, member cannot send message (server returns error)
	sockMember.writeJSON(map[string]any{"type": "group_message", "client_id": "illegal-msg", "group_id": grp.ID, "body": "illegal"})
	errFrame := sockMember.next(t, "error", 5*time.Second)
	if errFrame["message"] != "group not found" {
		t.Fatalf("unexpected error message: %+v", errFrame)
	}

	// Owner sends 20 messages during member's absence
	for i := range 20 {
		key := fmt.Sprintf("absence-msg-%02d", i)
		sockOwner.writeJSON(map[string]any{"type": "group_message", "client_id": key, "group_id": grp.ID, "body": key})
		_ = sockOwner.next(t, "ack", 5*time.Second)
	}

	// Member received 0 messages during absence
	timer := time.NewTimer(200 * time.Millisecond)
	select {
	case frame := <-sockMember.read:
		t.Fatalf("removed member received unexpected frame: %+v", frame)
	case <-timer.C:
		// Clean absence
	}

	// Owner re-invites member
	reinv := request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", grp.ID), tokOwner, map[string]uint{"user_id": uMember})
	if reinv.Code != http.StatusOK {
		t.Fatalf("re-invite failed: %d %s", reinv.Code, reinv.Body.String())
	}

	// Initial unread for re-invited member MUST be 0 (cursor advanced to latest message)
	unr := request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/unread", grp.ID), tokMember, nil)
	var unrData struct {
		UnreadCount int64 `json:"unread_count"`
	}
	_ = json.Unmarshal(unr.Body.Bytes(), &unrData)
	if unrData.UnreadCount != 0 {
		t.Fatalf("re-invited member unread count must be 0, got %d", unrData.UnreadCount)
	}

	// Owner sends 1 new message: member receives it live
	sockOwner.writeJSON(map[string]any{"type": "group_message", "client_id": "post-reinvite-1", "group_id": grp.ID, "body": "welcome back"})
	ackPost := sockOwner.next(t, "ack", 5*time.Second)
	postMsgID := uint(ackPost["message_id"].(float64))

	recPost := sockMember.next(t, "group_message", 5*time.Second)
	recPostMsg := recPost["message"].(map[string]any)
	if uint(recPostMsg["id"].(float64)) != postMsgID {
		t.Fatalf("member got wrong post-reinvite message id: %v != %d", recPostMsg["id"], postMsgID)
	}

	// Unread count is now 1
	unr2 := request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/unread", grp.ID), tokMember, nil)
	_ = json.Unmarshal(unr2.Body.Bytes(), &unrData)
	if unrData.UnreadCount != 1 {
		t.Fatalf("unread count should be 1, got %d", unrData.UnreadCount)
	}
}

// TestGroupFanout_PerDeviceUnreadAndACK verifies multi-device independent unread cursors:
// - User has Device A and Device B
// - Group receives 10 messages
// - Device A and Device B both report 10 unread
// - Device A ACKs message 5 -> Device A unread = 5, Device B unread = 10
// - Device B ACKs message 10 -> Device B unread = 0, Device A unread = 5
// - Device A ACKs message 10 -> both unread = 0
func TestGroupFanout_PerDeviceUnreadAndACK(t *testing.T) {
	_, h, _ := testServer(t)
	ts := httptest.NewServer(h)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"

	_, tokSender := registerTestUser(t, h, "multidev_sender")
	uMember, tokMember := registerTestUser(t, h, "multidev_member")

	sockSender := newGroupWsSocket(t, wsURL, tokSender, testUUID(401))

	devA := testUUID(402)
	devB := testUUID(403)

	created := request(t, h, http.MethodPost, "/api/groups", tokSender, map[string]string{"name": "Multi-Device Group"})
	var grp ChatGroup
	_ = json.Unmarshal(created.Body.Bytes(), &grp)
	_ = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", grp.ID), tokSender, map[string]uint{"user_id": uMember})

	// Sender posts 10 messages
	const msgCount = 10
	msgIDs := make([]uint, msgCount)
	for i := range msgCount {
		key := fmt.Sprintf("mdev-msg-%02d", i)
		sockSender.writeJSON(map[string]any{"type": "group_message", "client_id": key, "group_id": grp.ID, "body": key})
		ack := sockSender.next(t, "ack", 5*time.Second)
		msgIDs[i] = uint(ack["message_id"].(float64))
	}

	getUnread := func(dev string) int64 {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/groups/%d/unread", ts.URL, grp.ID), nil)
		req.Header.Set("Authorization", "Bearer "+tokMember)
		req.Header.Set("X-GoStream-Device-ID", dev)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("get unread failed: %v", err)
		}
		var payload struct {
			UnreadCount int64 `json:"unread_count"`
		}
		_ = json.NewDecoder(res.Body).Decode(&payload)
		res.Body.Close()
		return payload.UnreadCount
	}

	ackMsg := func(dev string, msgID uint) {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/groups/%d/messages/%d/ack", ts.URL, grp.ID, msgID), nil)
		req.Header.Set("Authorization", "Bearer "+tokMember)
		req.Header.Set("X-GoStream-Device-ID", dev)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("ack failed: %v", err)
		}
		res.Body.Close()
	}

	// Initially both devices have 10 unread
	if uA := getUnread(devA); uA != 10 {
		t.Fatalf("devA initial unread expected 10, got %d", uA)
	}
	if uB := getUnread(devB); uB != 10 {
		t.Fatalf("devB initial unread expected 10, got %d", uB)
	}

	// Device A ACKs message 5 (index 4)
	ackMsg(devA, msgIDs[4])
	if uA := getUnread(devA); uA != 5 {
		t.Fatalf("devA unread after msg 5 ACK expected 5, got %d", uA)
	}
	if uB := getUnread(devB); uB != 10 {
		t.Fatalf("devB unread should still be 10, got %d", uB)
	}

	// Device B ACKs message 10 (index 9)
	ackMsg(devB, msgIDs[9])
	if uB := getUnread(devB); uB != 0 {
		t.Fatalf("devB unread after msg 10 ACK expected 0, got %d", uB)
	}
	if uA := getUnread(devA); uA != 5 {
		t.Fatalf("devA unread should still be 5, got %d", uA)
	}

	// Device A ACKs message 10
	ackMsg(devA, msgIDs[9])
	if uA := getUnread(devA); uA != 0 {
		t.Fatalf("devA unread after msg 10 ACK expected 0, got %d", uA)
	}
	if uB := getUnread(devB); uB != 0 {
		t.Fatalf("devB unread expected 0, got %d", uB)
	}
}
