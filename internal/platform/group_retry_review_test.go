package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	sqlite3 "github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
)

func TestRetryableDBLockUsesTypedDriverErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"mysql deadlock", &mysqlDriver.MySQLError{Number: 1213}, true},
		{"mysql lock timeout", &mysqlDriver.MySQLError{Number: 1205}, true},
		{"wrapped sqlite busy", fmt.Errorf("write: %w", sqlite3.Error{Code: sqlite3.ErrBusy}), true},
		{"sqlite locked", sqlite3.Error{Code: sqlite3.ErrLocked}, true},
		{"domain busy", errors.New("worker is busy"), false},
		{"text mimics sqlite", errors.New("database is locked"), false},
		{"sqlite constraint", sqlite3.Error{Code: sqlite3.ErrConstraint}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableDBLock(tc.err); got != tc.want {
				t.Fatalf("retryable(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func reviewGroupFixture(t *testing.T, db *gorm.DB) (User, User, ChatGroup) {
	t.Helper()
	owner, reader := User{Username: "retry_owner", PasswordHash: "fixture"}, User{Username: "retry_reader", PasswordHash: "fixture"}
	for _, user := range []*User{&owner, &reader} {
		if err := db.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	group := ChatGroup{OwnerID: owner.ID, Name: "Retry ordering"}
	if err := db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint{owner.ID, reader.ID} {
		if err := db.Create(&GroupMember{GroupID: group.ID, UserID: id, JoinedAt: time.Now()}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&GroupRead{GroupID: group.ID, UserID: id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return owner, reader, group
}

func TestGroupRetryAllocatesIDAfterInterveningCommittedMessage(t *testing.T) {
	s, _, db := testServer(t)
	owner, reader, group := reviewGroupFixture(t, db)
	var failedID uint
	queries := 0
	var fixtureErr error
	intervening := GroupMessage{GroupID: group.ID, SenderID: owner.ID, ClientID: "intervening", Body: "committed first"}
	if err := db.Callback().Create().After("gorm:create").Register("review:fail_insert", func(tx *gorm.DB) {
		msg, ok := tx.Statement.Dest.(*GroupMessage)
		if ok && msg.ClientID == "retry-target" && failedID == 0 && tx.Error == nil {
			failedID = msg.ID
			tx.AddError(&mysqlDriver.MySQLError{Number: 1213, Message: "injected after insert"})
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove("review:fail_insert") })
	if err := db.Callback().Query().Before("gorm:query").Register("review:intervening_commit", func(tx *gorm.DB) {
		if tx.Statement.Table != "chat_groups" {
			return
		}
		queries++
		if queries == 2 {
			intervening.ID = failedID + 10
			fixtureErr = db.Create(&intervening).Error
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove("review:intervening_commit") })
	serverConn := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			serverConn <- conn
		}
	}))
	defer ts.Close()
	clientConn, _, err := websocket.DefaultDialer.Dial("ws"+ts.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	writer := <-serverConn
	defer writer.Close()
	s.sendGroupMessage(owner.ID, &hubClient{conn: writer}, socketEvent{GroupID: group.ID, ClientID: "retry-target", Body: "retried after rollback"})
	if fixtureErr != nil || failedID == 0 || intervening.ID == 0 {
		t.Fatalf("fixture did not reach rollback/intervening commit: %v failed=%d intervening=%d", fixtureErr, failedID, intervening.ID)
	}
	var response struct {
		Type      string `json:"type"`
		MessageID uint   `json:"message_id"`
	}
	if err := clientConn.ReadJSON(&response); err != nil {
		t.Fatal(err)
	}
	if response.Type != "ack" || response.MessageID <= intervening.ID {
		t.Fatalf("retry reused rolled-back ID or failed: response=%+v, failedID=%d intervening=%d", response, failedID, intervening.ID)
	}
	if err := db.Model(&GroupRead{}).Where("group_id=? AND user_id=?", group.ID, reader.ID).Update("last_ack_id", intervening.ID).Error; err != nil {
		t.Fatal(err)
	}
	var unread int64
	if err := db.Model(&GroupMessage{}).Where("group_id=? AND id>? AND sender_id<>?", group.ID, intervening.ID, reader.ID).Count(&unread).Error; err != nil || unread != 1 {
		t.Fatalf("ACK of earlier committed message hid retry: unread=%d err=%v", unread, err)
	}
}

func TestGroupDeviceAckHonorsCancellationBeforeTransaction(t *testing.T) {
	s, h, db := testServer(t)
	owner, reader, group := reviewGroupFixture(t, db)
	msg := GroupMessage{GroupID: group.ID, SenderID: owner.ID, ClientID: "ack-target", Body: "unread"}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatal(err)
	}
	deviceID := "01234567-89ab-cdef-0123-456789abcdef"
	if err := db.Create(&MessageDevice{UserID: reader.ID, DeviceID: deviceID, LastSeenAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	token, err := s.issueToken(reader.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := db.Callback().Query().After("gorm:query").Register("review:cancel_ack", func(tx *gorm.DB) {
		if tx.Statement.Table == "group_messages" {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove("review:cancel_ack") })
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, msg.ID), nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(deviceIDHeader, deviceID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var response map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	if rec.Code == http.StatusOK {
		t.Fatalf("cancelled ACK succeeded: %v", response)
	}
	var count int64
	if err := db.Model(&GroupDeviceRead{}).Where("group_id=? AND user_id=?", group.ID, reader.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("cancelled request persisted device ACK: count=%d err=%v", count, err)
	}
}
