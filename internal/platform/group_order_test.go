package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Pause the actual send transaction after MySQL allocates an ID, before commit.
// Two servers use independent pools: an in-process mutex cannot satisfy this test.
func TestMySQLGroupCommitOrder(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}
	for _, scenario := range []struct {
		name                 string
		rollback, otherGroup bool
	}{
		{name: "commit_before_cursor"}, {name: "rollback_releases_waiter", rollback: true}, {name: "independent_groups", otherGroup: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			pools := make([]*gorm.DB, 2)
			for i := range pools {
				db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
				if err != nil {
					t.Fatal(err)
				}
				sqlDB, err := db.DB()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = sqlDB.Close() })
				pools[i] = db
			}
			dbA, dbB := pools[0], pools[1]
			prefix := fmt.Sprintf("gorder%x", time.Now().UnixNano())
			users := []User{{Username: prefix + "a", PasswordHash: "unused"}, {Username: prefix + "b", PasswordHash: "unused"}, {Username: prefix + "r", PasswordHash: "unused"}}
			var groups []ChatGroup
			t.Cleanup(func() {
				for _, group := range groups {
					for _, model := range []any{&GroupDeviceRead{}, &GroupRead{}, &GroupMessage{}, &GroupMember{}} {
						if err := dbB.Where("group_id = ?", group.ID).Delete(model).Error; err != nil {
							t.Error(err)
						}
					}
					if err := dbB.Delete(&group).Error; err != nil {
						t.Error(err)
					}
				}
				for _, user := range users {
					if user.ID == 0 {
						continue
					}
					if err := dbB.Where("user_id = ?", user.ID).Delete(&MessageDevice{}).Error; err != nil {
						t.Error(err)
					}
					if err := dbB.Delete(&user).Error; err != nil {
						t.Error(err)
					}
				}
			})
			if err := dbA.Create(&users).Error; err != nil {
				t.Fatal(err)
			}
			for range 2 {
				group := ChatGroup{OwnerID: users[0].ID, Name: "commit order"}
				if err := dbA.Create(&group).Error; err != nil {
					t.Fatal(err)
				}
				groups = append(groups, group)
				for _, user := range users {
					if err := dbA.Create(&GroupMember{GroupID: group.ID, UserID: user.ID, JoinedAt: time.Now().UTC()}).Error; err != nil {
						t.Fatal(err)
					}
					if err := dbA.Create(&GroupRead{GroupID: group.ID, UserID: user.ID}).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			servers := []*Server{NewServer(dbA, "test-signing-secret-at-least-32-bytes", t.TempDir()), NewServer(dbB, "test-signing-secret-at-least-32-bytes", t.TempDir())}
			hA, hB := servers[0].Router(), servers[1].Router()
			webA, webB := httptest.NewServer(hA), httptest.NewServer(hB)
			t.Cleanup(webA.Close)
			t.Cleanup(webB.Close)
			token := func(id uint) string {
				value, err := servers[0].issueToken(id)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			a := connectDeviceSocket(t, webA.URL, token(users[0].ID), "")
			b := connectDeviceSocket(t, webB.URL, token(users[1].ID), "")
			recipientToken := token(users[2].ID)
			inserted, release := make(chan uint, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			if err := dbA.Callback().Create().After("gorm:create").Before("gorm:commit_or_rollback_transaction").Register("test:pause_group_insert", func(tx *gorm.DB) {
				msg, ok := tx.Statement.Dest.(*GroupMessage)
				if !ok || msg.ClientID != "slow" || tx.Error != nil {
					return
				}
				inserted <- msg.ID
				select {
				case <-release:
				case <-time.After(8 * time.Second):
					tx.AddError(fmt.Errorf("test release timed out"))
				}
				if scenario.rollback {
					tx.AddError(fmt.Errorf("injected rollback after ID allocation"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			send := func(conn *websocket.Conn, key string, group uint) {
				t.Helper()
				if err := conn.WriteJSON(socketEvent{Type: "group_message", ClientID: key, GroupID: group, Body: key}); err != nil {
					t.Fatal(err)
				}
			}
			readReply := func(conn *websocket.Conn, key, want string) uint {
				t.Helper()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				for {
					var frame struct {
						Type      string `json:"type"`
						ClientID  string `json:"client_id"`
						MessageID uint   `json:"message_id"`
					}
					if err := conn.ReadJSON(&frame); err != nil {
						t.Fatal(err)
					}
					if frame.Type == "group_message" {
						continue
					}
					if frame.Type != want || (want == "ack" && frame.ClientID != key) {
						t.Fatalf("reply %+v, want %s/%s", frame, want, key)
					}
					return frame.MessageID
				}
			}
			send(a, "slow", groups[0].ID)
			var low uint
			select {
			case low = <-inserted:
			case <-time.After(5 * time.Second):
				t.Fatal("first transaction never reached insert")
			}
			target := groups[0].ID
			if scenario.otherGroup {
				target = groups[1].ID
			}
			attempted := make(chan struct{})
			var attemptOnce sync.Once
			if err := dbB.Callback().Query().After("gorm:query").Register("test:second_send_started", func(tx *gorm.DB) {
				if tx.Statement.Table == "group_members" {
					attemptOnce.Do(func() { close(attempted) })
				}
			}); err != nil {
				t.Fatal(err)
			}
			send(b, "fast", target)
			select {
			case <-attempted:
			case <-time.After(3 * time.Second):
				t.Fatal("second server did not start the send")
			}
			var fast GroupMessage
			wait := 500 * time.Millisecond
			if scenario.otherGroup {
				wait = 3 * time.Second
			}
			for until := time.Now().Add(wait); time.Now().Before(until); {
				if err := dbB.Where("sender_id = ? AND client_id = ?", users[1].ID, "fast").Find(&fast).Error; err != nil {
					t.Fatal(err)
				}
				if fast.ID != 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			premature := fast.ID != 0 && !scenario.otherGroup
			ack := func(id uint) {
				t.Helper()
				path := fmt.Sprintf("/api/groups/%d/messages/%d/ack", groups[0].ID, id)
				r := requestOnDevice(t, hB, http.MethodPost, path, recipientToken, testDeviceA, nil)
				if r.Code != 200 {
					t.Fatalf("ACK: %d %s", r.Code, r.Body.String())
				}
			}
			if premature {
				ack(fast.ID)
			}
			if scenario.otherGroup && fast.ID == 0 {
				t.Error("unrelated group could not commit while first group paused")
			}
			unblock()
			want := "ack"
			if scenario.rollback {
				want = "error"
			}
			if id := readReply(a, "slow", want); !scenario.rollback && id != low {
				t.Fatalf("first sender ACK=%d want=%d", id, low)
			}
			high := readReply(b, "fast", "ack")
			if high <= low {
				t.Fatalf("allocated IDs low=%d high=%d", low, high)
			}
			var count int64
			if err := dbB.Model(&GroupMessage{}).Where("id = ?", low).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if (count == 0) != scenario.rollback {
				t.Fatalf("slow persisted rows=%d rollback=%t", count, scenario.rollback)
			}
			if premature {
				r := requestOnDevice(t, hB, http.MethodGet, fmt.Sprintf("/api/groups/%d/unread", groups[0].ID), recipientToken, testDeviceA, nil)
				t.Fatalf("higher ID %d committed and was ACKed before %d resolved; late row count=%d; unread response %d %s", high, low, count, r.Code, r.Body.String())
			}
			if !scenario.rollback && !scenario.otherGroup {
				ack(low)
				r := requestOnDevice(t, hB, http.MethodGet, fmt.Sprintf("/api/groups/%d/unread", groups[0].ID), recipientToken, testDeviceA, nil)
				var result struct {
					Unread int  `json:"unread_count"`
					Last   uint `json:"last_ack_id"`
				}
				if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil || result.Unread != 1 || result.Last != low {
					t.Fatalf("unread after low ACK: %d %s", r.Code, r.Body.String())
				}
				ack(high)
			}
			send(b, "fast", target)
			if retry := readReply(b, "fast", "ack"); retry != high {
				t.Fatalf("retry ID=%d want=%d", retry, high)
			}
			if err := dbB.Model(&GroupMessage{}).Where("sender_id = ? AND client_id = ?", users[1].ID, "fast").Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("retry rows=%d err=%v", count, err)
			}
		})
	}
}
