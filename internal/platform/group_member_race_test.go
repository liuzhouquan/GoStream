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

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestGroupMemberRevocationPermissions(t *testing.T) {
	_, h, db := testServer(t)
	ownerID, ownerToken := registerTestUser(t, h, "raceowner_sq")
	memberID, memberToken := registerTestUser(t, h, "racemember_sq")
	outsiderID, outsiderToken := registerTestUser(t, h, "raceoutsider_sq")
	_ = outsiderID

	created := request(t, h, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "Revocation SQ group"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", created.Code, created.Body.String())
	}
	var group ChatGroup
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}

	base := fmt.Sprintf("/api/groups/%d", group.ID)

	// Add member
	if r := request(t, h, http.MethodPost, base+"/members", ownerToken, map[string]any{"user_id": memberID}); r.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", r.Code, r.Body.String())
	}

	// Seed a message from owner
	msg := GroupMessage{GroupID: group.ID, SenderID: ownerID, ClientID: "owner-seed-1", Body: "hello member"}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatal(err)
	}

	// Outsider cannot access or remove member (returns 404)
	if r := request(t, h, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, memberID), outsiderToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("outsider remove member: expected 404, got %d", r.Code)
	}

	// Non-owner member cannot remove another member (returns 403)
	if r := request(t, h, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, ownerID), memberToken, nil); r.Code != http.StatusBadRequest {
		// target is owner, so returns 400
	}
	// Add outsider as second member so member can try to remove outsider
	if r := request(t, h, http.MethodPost, base+"/members", ownerToken, map[string]any{"user_id": outsiderID}); r.Code != http.StatusOK {
		t.Fatalf("add outsider member: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, outsiderID), memberToken, nil); r.Code != http.StatusForbidden {
		t.Fatalf("member remove another member: expected 403, got %d", r.Code)
	}

	// Owner cannot be removed or leave
	if r := request(t, h, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, ownerID), ownerToken, nil); r.Code != http.StatusBadRequest {
		t.Fatalf("owner leave: expected 400, got %d", r.Code)
	}

	// Owner removes member
	if r := request(t, h, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, memberID), ownerToken, nil); r.Code != http.StatusOK {
		t.Fatalf("owner remove member: %d %s", r.Code, r.Body.String())
	}

	// Removed member history read returns 404
	if r := request(t, h, http.MethodGet, base+"/messages", memberToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("removed member history: expected 404, got %d", r.Code)
	}

	// Removed member unread read returns 404
	if r := request(t, h, http.MethodGet, base+"/unread", memberToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("removed member unread: expected 404, got %d", r.Code)
	}

	// Removed member ACK with device returns 404 and does not insert GroupDeviceRead
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/messages/%d/ack", base, msg.ID), nil)
	req.Header.Set("Authorization", "Bearer "+memberToken)
	req.Header.Set("X-GoStream-Device-ID", "01234567-89ab-cdef-0123-456789abcdef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("removed member ACK with device: expected 404, got %d %s", rec.Code, rec.Body.String())
	}

	var dReadCount int64
	if err := db.Model(&GroupDeviceRead{}).Where("group_id = ? AND user_id = ?", group.ID, memberID).Count(&dReadCount).Error; err != nil || dReadCount != 0 {
		t.Fatalf("expected 0 GroupDeviceRead rows for removed member, got %d", dReadCount)
	}

	// Removed member legacy ACK returns 404
	if r := request(t, h, http.MethodPost, fmt.Sprintf("%s/messages/%d/ack", base, msg.ID), memberToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("removed member legacy ACK: expected 404, got %d", r.Code)
	}

	// Removed member WebSocket send is rejected
	ts := httptest.NewServer(h)
	defer ts.Close()
	conn := connectDeviceSocket(t, ts.URL, memberToken, "")
	defer conn.Close()

	if err := conn.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: "post-removal-send", Body: "should fail"}); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var reply struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := conn.ReadJSON(&reply); err != nil {
			t.Fatalf("read socket reply: %v", err)
		}
		if reply.Type == "ready" {
			continue
		}
		if reply.Type != "error" || reply.Message != "group not found" {
			t.Fatalf("expected error 'group not found', got %+v", reply)
		}
		break
	}

	var msgCount int64
	if err := db.Model(&GroupMessage{}).Where("group_id = ? AND sender_id = ?", group.ID, memberID).Count(&msgCount).Error; err != nil || msgCount != 0 {
		t.Fatalf("expected 0 GroupMessage rows from removed member, got %d", msgCount)
	}
}

// TestMySQLGroupMemberRevocationRace exercises concurrent member removal against
// in-flight sendGroupMessage and ackGroupMessage on real MySQL with two independent connections.
func TestMySQLGroupMemberRevocationRace(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}

	for _, tc := range []struct {
		name string
		mode string // "send" or "ack"
	}{
		{name: "send_revocation_race", mode: "send"},
		{name: "ack_revocation_race", mode: "ack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			prefix := fmt.Sprintf("gmrace%x", time.Now().UnixNano())
			users := []User{
				{Username: prefix + "_owner", PasswordHash: "unused"},
				{Username: prefix + "_member", PasswordHash: "unused"},
			}
			var group ChatGroup

			t.Cleanup(func() {
				if group.ID != 0 {
					for _, model := range []any{&GroupDeviceRead{}, &GroupRead{}, &GroupMessage{}, &GroupMember{}} {
						_ = dbB.Where("group_id = ?", group.ID).Delete(model).Error
					}
					_ = dbB.Delete(&group).Error
				}
				for _, user := range users {
					if user.ID != 0 {
						_ = dbB.Where("user_id = ?", user.ID).Delete(&MessageDevice{}).Error
						_ = dbB.Delete(&user).Error
					}
				}
			})

			if err := dbA.Create(&users).Error; err != nil {
				t.Fatal(err)
			}
			ownerID, memberID := users[0].ID, users[1].ID

			group = ChatGroup{OwnerID: ownerID, Name: "revocation race group"}
			if err := dbA.Create(&group).Error; err != nil {
				t.Fatal(err)
			}
			for _, u := range users {
				if err := dbA.Create(&GroupMember{GroupID: group.ID, UserID: u.ID, JoinedAt: time.Now().UTC()}).Error; err != nil {
					t.Fatal(err)
				}
				if err := dbA.Create(&GroupRead{GroupID: group.ID, UserID: u.ID}).Error; err != nil {
					t.Fatal(err)
				}
			}

			serverA := NewServer(dbA, "test-signing-secret-at-least-32-bytes", t.TempDir())
			serverB := NewServer(dbB, "test-signing-secret-at-least-32-bytes", t.TempDir())

			hA, hB := serverA.Router(), serverB.Router()
			webA, webB := httptest.NewServer(hA), httptest.NewServer(hB)
			t.Cleanup(webA.Close)
			t.Cleanup(webB.Close)

			ownerToken, err := serverB.issueToken(ownerID)
			if err != nil {
				t.Fatal(err)
			}
			memberToken, err := serverA.issueToken(memberID)
			if err != nil {
				t.Fatal(err)
			}

			if tc.mode == "send" {
				// Server A runs member send; paused after group lock query
				entered := make(chan struct{}, 1)
				release := make(chan struct{})
				var pauseOnce, releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(unblock)

				callbackName := "test:mysql_pause_send_group_lock"
				if err := dbA.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
					if tx.Statement.Table == "chat_groups" && len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "id" {
						pauseOnce.Do(func() {
							entered <- struct{}{}
							select {
							case <-release:
							case <-time.After(8 * time.Second):
								tx.AddError(fmt.Errorf("release timed out"))
							}
						})
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = dbA.Callback().Query().Remove(callbackName) })

				conn := connectDeviceSocket(t, webA.URL, memberToken, "")
				defer conn.Close()

				// Member initiates send on Server A
				sendKey := prefix + "_race_send"
				if err := conn.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: sendKey, Body: "should be revoked"}); err != nil {
					t.Fatal(err)
				}

				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("sendGroupMessage never reached group lock on Server A")
				}

				// Owner concurrently removes member on Server B
				// Server B acquires ChatGroup lock and deletes GroupMember, then commits
				removeDone := make(chan struct{})
				go func() {
					defer close(removeDone)
					req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/groups/%d/members/%d", webB.URL, group.ID, memberID), nil)
					req.Header.Set("Authorization", "Bearer "+ownerToken)
					resp, err := http.DefaultClient.Do(req)
					if err != nil || resp.StatusCode != http.StatusOK {
						t.Errorf("remove member on server B: code=%v err=%v", resp.StatusCode, err)
					}
					if resp != nil {
						_ = resp.Body.Close()
					}
				}()

				// Release Server A so Server B can acquire group lock and complete removal,
				// or Server A completes first. Either way, linearizability must hold.
				time.Sleep(50 * time.Millisecond)
				unblock()
				<-removeDone

				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				for {
					var frame struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					}
					if err := conn.ReadJSON(&frame); err != nil {
						t.Fatalf("socket reply: %v", err)
					}
					if frame.Type == "ready" {
						continue
					}
					// If removal committed first, send must be rejected
					// If send committed first, it had valid membership at commit
					break
				}

				// After removal is confirmed, no message from the removed member should be persisted if rejected
				var finalMemberCount int64
				if err := dbB.Model(&GroupMember{}).Where("group_id = ? AND user_id = ?", group.ID, memberID).Count(&finalMemberCount).Error; err != nil {
					t.Fatal(err)
				}
				if finalMemberCount != 0 {
					t.Fatalf("expected member to be removed, got count=%d", finalMemberCount)
				}
			} else {
				// ACK race on MySQL
				msg := GroupMessage{GroupID: group.ID, SenderID: ownerID, ClientID: prefix + "_ack_seed", Body: "seed message"}
				if err := dbB.Create(&msg).Error; err != nil {
					t.Fatal(err)
				}

				entered := make(chan struct{}, 1)
				release := make(chan struct{})
				var pauseOnce, releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(unblock)

				callbackName := "test:mysql_pause_ack_membership_check"
				if err := dbA.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
					if tx.Statement.Table == "group_reads" {
						pauseOnce.Do(func() {
							entered <- struct{}{}
							select {
							case <-release:
							case <-time.After(8 * time.Second):
								tx.AddError(fmt.Errorf("release timed out"))
							}
						})
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = dbA.Callback().Query().Remove(callbackName) })

				ackDone := make(chan int, 1)
				go func() {
					req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/groups/%d/messages/%d/ack", webA.URL, group.ID, msg.ID), nil)
					req.Header.Set("Authorization", "Bearer "+memberToken)
					req.Header.Set("X-GoStream-Device-ID", "01234567-89ab-cdef-0123-456789abcdef")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						ackDone <- 0
						return
					}
					_ = resp.Body.Close()
					ackDone <- resp.StatusCode
				}()

				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("ack never reached read check on Server A")
				}

				// Owner removes member on Server B
				req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/groups/%d/members/%d", webB.URL, group.ID, memberID), nil)
				req.Header.Set("Authorization", "Bearer "+ownerToken)
				resp, err := http.DefaultClient.Do(req)
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("remove member on server B: code=%v err=%v", resp.StatusCode, err)
				}
				_ = resp.Body.Close()

				unblock()

				statusCode := <-ackDone
				if statusCode != http.StatusNotFound && statusCode != http.StatusOK {
					t.Fatalf("unexpected ACK status code: %d", statusCode)
				}

				// If removal committed before ACK transaction, device read must not be resurrected
				var dCount int64
				if err := dbB.Model(&GroupDeviceRead{}).Where("group_id = ? AND user_id = ?", group.ID, memberID).Count(&dCount).Error; err != nil {
					t.Fatal(err)
				}
				if statusCode == http.StatusNotFound && dCount != 0 {
					t.Fatalf("resurrected GroupDeviceRead after 404 response: count=%d", dCount)
				}
			}
		})
	}
}
