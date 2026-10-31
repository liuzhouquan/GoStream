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

// Pause the first receipt compaction inside its real MySQL transaction, then
// overlap a second API's ACK and verify an unrelated account can still progress.
func TestMySQLDeviceAckRanges(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("requires an isolated migrated MySQL database")
	}
	var pools []*gorm.DB
	for range 2 {
		db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := raw.Close(); err != nil {
				t.Error(err)
			}
		})
		pools = append(pools, db)
	}
	a := NewServer(pools[0], "range-test-secret-at-least-32-bytes", t.TempDir())
	b := NewServer(pools[1], "range-test-secret-at-least-32-bytes", t.TempDir())
	ha, hb := a.Router(), b.Router()
	prefix := fmt.Sprintf("ranges_%x", time.Now().UnixNano())
	users := []User{{Username: prefix + "s", PasswordHash: "fixture"}, {Username: prefix + "r", PasswordHash: "fixture"}, {Username: prefix + "i", PasswordHash: "fixture"}}
	if err := pools[0].Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, user := range users {
			for _, model := range []any{&MessageDeviceAckRange{}, &MessageDeviceAck{}, &MessageDevice{}} {
				if err := pools[1].Where("user_id = ?", user.ID).Delete(model).Error; err != nil {
					t.Error(err)
				}
			}
			if err := pools[1].Where("sender_id = ? OR recipient_id = ?", user.ID, user.ID).Delete(&ChatMessage{}).Error; err != nil {
				t.Error(err)
			}
			if err := pools[1].Delete(&user).Error; err != nil {
				t.Error(err)
			}
		}
	})
	token, err := a.issueToken(users[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	independentToken, err := a.issueToken(users[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids, independentIDs []uint
	for i := range 6 {
		recipient := users[1].ID
		if i >= 4 {
			recipient = users[2].ID
		}
		message := ChatMessage{SenderID: users[0].ID, RecipientID: recipient, ClientID: fmt.Sprintf("%s_%d", prefix, i), Body: "concurrent range"}
		if err := pools[0].Create(&message).Error; err != nil {
			t.Fatal(err)
		}
		if i < 4 {
			ids = append(ids, message.ID)
		} else {
			independentIDs = append(independentIDs, message.ID)
		}
	}
	for _, who := range []string{token, independentToken} {
		if r := requestOnDevice(t, hb, http.MethodGet, "/api/messages/unread", who, testDeviceA, nil); r.Code != 200 {
			t.Fatal(r.Body.String())
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, enteredOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	var tasks sync.WaitGroup
	defer func() { unlock(); tasks.Wait() }()
	const pause = "test:hold_device_range_commit"
	if err := pools[0].Callback().Create().Before("gorm:create").Register(pause, func(tx *gorm.DB) {
		if tx.Statement.Table == "message_device_ack_ranges" {
			enteredOnce.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-time.After(10 * time.Second):
					tx.AddError(fmt.Errorf("range barrier timed out"))
				}
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools[0].Callback().Create().Remove(pause) })
	first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	tasks.Go(func() {
		first <- requestOnDevice(t, ha, http.MethodPost, "/api/messages/acks", token, testDeviceA, map[string]any{"ids": ids[:2]})
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first ACK never reached compaction")
	}
	secondLock := make(chan struct{})
	var secondOnce sync.Once
	const observe = "test:observe_device_ack_user_lock"
	if err := pools[1].Callback().Query().Before("gorm:query").Register(observe, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			secondOnce.Do(func() { close(secondLock) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools[1].Callback().Query().Remove(observe) })
	tasks.Go(func() {
		second <- requestOnDevice(t, hb, http.MethodPost, "/api/messages/acks", token, testDeviceA, map[string]any{"ids": ids[1:3]})
	})
	select {
	case <-secondLock:
	case <-time.After(5 * time.Second):
		t.Fatal("second ACK never reached user transaction")
	}
	independent := make(chan *httptest.ResponseRecorder, 1)
	tasks.Go(func() {
		independent <- requestOnDevice(t, hb, http.MethodPost, "/api/messages/acks", independentToken, testDeviceA, map[string]any{"ids": independentIDs})
	})
	select {
	case r := <-independent:
		if r.Code != 200 {
			t.Fatalf("independent ACK: %d %s", r.Code, r.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unrelated account blocked by compaction")
	}
	select {
	case r := <-second:
		t.Fatalf("overlapping ACK escaped first transaction: %d %s", r.Code, r.Body.String())
	default:
	}
	unlock()
	for i, ch := range []chan *httptest.ResponseRecorder{first, second} {
		select {
		case r := <-ch:
			var reply struct {
				Count int `json:"acked_count"`
			}
			if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &reply) != nil || reply.Count != 2-i {
				t.Fatalf("ACK %d: %d %s", i, r.Code, r.Body.String())
			}
		case <-time.After(6 * time.Second):
			t.Fatal("ACK did not finish after release")
		}
	}
	var ranges []MessageDeviceAckRange
	if err := pools[1].Where("user_id = ?", users[1].ID).Find(&ranges).Error; err != nil || len(ranges) != 1 || ranges[0].FirstMessageID != ids[0] || ranges[0].LastMessageID != ids[2] {
		t.Fatalf("committed ranges=%+v err=%v", ranges, err)
	}
	var raw int64
	if err := pools[1].Model(&MessageDeviceAck{}).Where("user_id = ?", users[1].ID).Count(&raw).Error; err != nil || raw != 0 {
		t.Fatalf("raw receipts=%d err=%v", raw, err)
	}
	for _, tc := range []struct {
		device string
		count  int
	}{{testDeviceA, 1}, {testDeviceB, 4}, {"", 4}} {
		r := requestOnDevice(t, hb, http.MethodGet, "/api/messages/unread", token, tc.device, nil)
		var page struct {
			Items []ChatMessage `json:"items"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Items) != tc.count {
			t.Fatalf("device %q: %d %s", tc.device, r.Code, r.Body.String())
		}
	}
	// A lower message ID may commit after higher IDs are acknowledged. Only
	// explicitly ACKed IDs may enter a range; no empty-gap inference is safe.
	lateTx := pools[0].Begin()
	if lateTx.Error != nil {
		t.Fatal(lateTx.Error)
	}
	defer lateTx.Rollback()
	late := ChatMessage{SenderID: users[0].ID, RecipientID: users[1].ID, ClientID: prefix + "_late", Body: "late commit"}
	if err := lateTx.Create(&late).Error; err != nil {
		t.Fatal(err)
	}
	var laterIDs []uint
	for i := range 2 {
		m := ChatMessage{SenderID: users[0].ID, RecipientID: users[1].ID, ClientID: fmt.Sprintf("%s_later_%d", prefix, i), Body: "earlier commit"}
		if err := pools[1].Create(&m).Error; err != nil {
			t.Fatal(err)
		}
		laterIDs = append(laterIDs, m.ID)
	}
	if r := requestOnDevice(t, hb, http.MethodPost, "/api/messages/acks", token, testDeviceA, map[string]any{"ids": laterIDs}); r.Code != 200 {
		t.Fatalf("higher ACK: %d %s", r.Code, r.Body.String())
	}
	if err := lateTx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	r := requestOnDevice(t, hb, http.MethodGet, "/api/messages/unread", token, testDeviceA, nil)
	var page struct {
		Items []ChatMessage `json:"items"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.Items[0].ID != ids[3] || page.Items[1].ID != late.ID {
		t.Fatalf("late commit lost: %d %s", r.Code, r.Body.String())
	}
	t.Run("unread_snapshot_survives_concurrent_compaction", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once, releaseOnce sync.Once
		var snapshotTasks sync.WaitGroup
		unlock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unlock()
		const hook = "test:pause_unread_snapshot"
		if err := pools[0].Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
			if tx.Statement.Table == "chat_messages" {
				once.Do(func() {
					close(entered)
					select {
					case <-release:
					case <-time.After(8 * time.Second):
						tx.AddError(fmt.Errorf("snapshot barrier timed out"))
					}
				})
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer func() {
			unlock()
			snapshotTasks.Wait()
			if err := pools[0].Callback().Query().Remove(hook); err != nil {
				t.Error(err)
			}
		}()
		responses := make(chan *httptest.ResponseRecorder, 1)
		snapshotTasks.Go(func() {
			responses <- requestOnDevice(t, ha, http.MethodGet, "/api/messages/unread", token, testDeviceB, nil)
		})
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("reader did not establish snapshot")
		}
		allIDs := append(append(append([]uint{}, ids...), late.ID), laterIDs...)
		write := make(chan *httptest.ResponseRecorder, 1)
		snapshotTasks.Go(func() {
			write <- requestOnDevice(t, hb, http.MethodPost, "/api/messages/acks", token, testDeviceB, map[string]any{"ids": allIDs})
		})
		select {
		case r := <-write:
			if r.Code != 200 {
				t.Fatalf("concurrent ACK: %d %s", r.Code, r.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("read-only snapshot blocked ACK writer")
		}
		unlock()
		select {
		case r := <-responses:
			var snapshot struct {
				Items []ChatMessage `json:"items"`
			}
			if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &snapshot) != nil || len(snapshot.Items) != len(allIDs) {
				t.Fatalf("mixed read snapshots: %d %s", r.Code, r.Body.String())
			}
			for i, m := range snapshot.Items {
				if m.ID != allIDs[i] {
					t.Fatalf("snapshot ID %d want %d", m.ID, allIDs[i])
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("snapshot reader did not finish")
		}
		r := requestOnDevice(t, hb, http.MethodGet, "/api/messages/unread", token, testDeviceB, nil)
		var fresh struct {
			Items []ChatMessage `json:"items"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &fresh) != nil || len(fresh.Items) != 0 {
			t.Fatalf("next request missed committed ACK: %d %s", r.Code, r.Body.String())
		}
	})

}
