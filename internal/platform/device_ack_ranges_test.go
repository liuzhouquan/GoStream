package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDeviceAckRangesPreserveExactUnread(t *testing.T) {
	_, h, db := testServer(t)
	recipient, token := registerTestUser(t, h, "range_recipient")
	sender, _ := registerTestUser(t, h, "range_sender")
	var messages []ChatMessage
	for i := range 8 {
		message := ChatMessage{SenderID: sender, RecipientID: recipient, ClientID: fmt.Sprintf("range-%d", i), Body: "exact receipt"}
		if err := db.Create(&message).Error; err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	ack := func(ids []uint, count int) {
		t.Helper()
		res := requestOnDevice(t, h, http.MethodPost, "/api/messages/acks", token, testDeviceA, map[string]any{"ids": ids})
		var reply struct {
			AckedCount int `json:"acked_count"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &reply) != nil || reply.AckedCount != count {
			t.Fatalf("ACK: %d %s want count %d", res.Code, res.Body.String(), count)
		}
	}
	ack([]uint{messages[0].ID, messages[1].ID, messages[3].ID, messages[4].ID}, 4)
	var ranges []MessageDeviceAckRange
	if err := db.Where("user_id = ? AND device_id = ?", recipient, testDeviceA).Order("first_message_id").Find(&ranges).Error; err != nil || len(ranges) != 2 || ranges[0].FirstMessageID != messages[0].ID || ranges[0].LastMessageID != messages[1].ID || ranges[1].FirstMessageID != messages[3].ID || ranges[1].LastMessageID != messages[4].ID {
		t.Fatalf("initial ranges=%+v err=%v", ranges, err)
	}
	var raw int64
	if err := db.Model(&MessageDeviceAck{}).Where("user_id = ?", recipient).Count(&raw).Error; err != nil || raw != 0 {
		t.Fatalf("adjacent receipts not compacted: raw=%d err=%v", raw, err)
	}
	check := func(device string, want []uint) {
		t.Helper()
		res := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread?limit=100", token, device, nil)
		var page struct {
			Items []ChatMessage `json:"items"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil {
			t.Fatalf("unread: %d %s", res.Code, res.Body.String())
		}
		if len(page.Items) != len(want) {
			t.Fatalf("device %s unread=%+v want=%v", device, page.Items, want)
		}
		for i, id := range want {
			if page.Items[i].ID != id {
				t.Fatalf("unread=%+v want=%v", page.Items, want)
			}
		}
	}
	check(testDeviceA, []uint{messages[2].ID, messages[5].ID, messages[6].ID, messages[7].ID})
	ack([]uint{messages[0].ID, messages[1].ID}, 0)
	ack([]uint{messages[2].ID}, 1)
	ranges = nil
	if err := db.Where("user_id = ? AND device_id = ?", recipient, testDeviceA).Find(&ranges).Error; err != nil || len(ranges) != 1 || ranges[0].FirstMessageID != messages[0].ID || ranges[0].LastMessageID != messages[4].ID {
		t.Fatalf("merged ranges=%+v err=%v", ranges, err)
	}
	check(testDeviceA, []uint{messages[5].ID, messages[6].ID, messages[7].ID})
	check(testDeviceB, []uint{messages[0].ID, messages[1].ID, messages[2].ID, messages[3].ID, messages[4].ID, messages[5].ID, messages[6].ID, messages[7].ID})
	check("", []uint{messages[0].ID, messages[1].ID, messages[2].ID, messages[3].ID, messages[4].ID, messages[5].ID, messages[6].ID, messages[7].ID})
	web := httptest.NewServer(h)
	defer web.Close()
	conn := connectDeviceSocket(t, web.URL, token, testDeviceA)
	defer conn.Close()
	if err := conn.WriteJSON(socketEvent{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range messages[5:] {
		var frame struct {
			Type    string      `json:"type"`
			Message ChatMessage `json:"message"`
		}
		if err := conn.ReadJSON(&frame); err != nil || frame.Type != "message" || frame.Message.ID != message.ID {
			t.Fatalf("range-aware replay=%+v err=%v want=%d", frame, err, message.ID)
		}
	}
	var pong map[string]any
	if err := conn.ReadJSON(&pong); err != nil || pong["type"] != "pong" {
		t.Fatalf("replay end=%v err=%v", pong, err)
	}
}

func TestConcurrentDeviceAckRangesSQLite(t *testing.T) {
	_, h, db := testServer(t)
	recipient, token := registerTestUser(t, h, "concurrent_range_reader")
	sender, _ := registerTestUser(t, h, "concurrent_range_sender")
	var ids []uint
	for i := range 6 {
		m := ChatMessage{SenderID: sender, RecipientID: recipient, ClientID: fmt.Sprintf("parallel-%d", i), Body: "parallel"}
		if err := db.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	if r := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, testDeviceA, nil); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			results <- requestOnDevice(t, h, http.MethodPost, "/api/messages/acks", token, testDeviceA, map[string]any{"ids": ids})
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var created int
	for r := range results {
		var reply struct {
			Count int `json:"acked_count"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &reply) != nil {
			t.Fatalf("concurrent ACK: %d %s", r.Code, r.Body.String())
		}
		created += reply.Count
	}
	if created != 6 {
		t.Fatalf("created=%d want 6", created)
	}
	var ranges []MessageDeviceAckRange
	if err := db.Where("user_id = ?", recipient).Find(&ranges).Error; err != nil || len(ranges) != 1 || ranges[0].FirstMessageID != ids[0] || ranges[0].LastMessageID != ids[5] {
		t.Fatalf("ranges=%+v err=%v", ranges, err)
	}
}

func TestDeviceAckRangesCompactIncrementalBatches(t *testing.T) {
	_, h, db := testServer(t)
	recipient, token := registerTestUser(t, h, "range_batch_reader")
	sender, _ := registerTestUser(t, h, "range_batch_sender")
	messages := make([]ChatMessage, 1000)
	for i := range messages {
		messages[i] = ChatMessage{SenderID: sender, RecipientID: recipient, ClientID: fmt.Sprintf("batch-%d", i), Body: "batch"}
	}
	if err := db.CreateInBatches(&messages, 100).Error; err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(messages); offset += 100 {
		ids := make([]uint, 100)
		for i := range ids {
			ids[i] = messages[offset+i].ID
		}
		r := requestOnDevice(t, h, http.MethodPost, "/api/messages/acks", token, testDeviceA, map[string]any{"ids": ids})
		var reply struct {
			Count int `json:"acked_count"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &reply) != nil || reply.Count != 100 {
			t.Fatalf("batch %d: %d %s", offset, r.Code, r.Body.String())
		}
	}
	var ranges []MessageDeviceAckRange
	if err := db.Where("user_id = ?", recipient).Find(&ranges).Error; err != nil || len(ranges) != 1 || ranges[0].FirstMessageID != messages[0].ID || ranges[0].LastMessageID != messages[999].ID {
		t.Fatalf("1000 receipts: %+v err=%v", ranges, err)
	}
	var count int64
	if err := db.Model(&MessageDeviceAck{}).Where("user_id = ?", recipient).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("raw=%d err=%v", count, err)
	}
	if err := db.Model(&ChatMessage{}).Where("recipient_id = ? AND acked_at IS NULL", recipient).Count(&count).Error; err != nil || count != 1000 {
		t.Fatalf("messages changed=%d err=%v", count, err)
	}
	for _, tc := range []struct {
		device string
		want   int
	}{{testDeviceA, 0}, {testDeviceB, 100}} {
		r := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread?limit=100", token, tc.device, nil)
		var page struct {
			Items []ChatMessage `json:"items"`
			More  bool          `json:"has_more"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Items) != tc.want || page.More != (tc.want == 100) {
			t.Fatalf("device %q: %d %s", tc.device, r.Code, r.Body.String())
		}
	}
	// The single-message endpoint shares idempotence with the batch endpoint.
	r := requestOnDevice(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", messages[999].ID), token, testDeviceA, nil)
	var reply struct {
		Count int `json:"acked_count"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &reply) != nil || reply.Count != 0 {
		t.Fatalf("single retry: %d %s", r.Code, r.Body.String())
	}
}

func TestDeviceAckRangeRollbackAndExpiry(t *testing.T) {
	s, h, db := testServer(t)
	recipient, token := registerTestUser(t, h, "range_rollback")
	sender, _ := registerTestUser(t, h, "range_source")
	var messages []ChatMessage
	for i := range 4 {
		m := ChatMessage{SenderID: sender, RecipientID: recipient, ClientID: fmt.Sprintf("rollback-%d", i), Body: "keep unread"}
		if err := db.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
	}
	ack := func(ids []uint, wantStatus int) {
		t.Helper()
		r := requestOnDevice(t, h, http.MethodPost, "/api/messages/acks", token, testDeviceA, map[string]any{"ids": ids})
		if r.Code != wantStatus {
			t.Fatalf("ACK %d: %s", r.Code, r.Body.String())
		}
	}
	ack([]uint{messages[0].ID, messages[1].ID}, 200)
	injected := 0
	const callback = "test:range_receipt_delete_failure"
	if err := db.Callback().Delete().Before("gorm:delete").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "message_device_acks" {
			injected++
			tx.AddError(errors.New("injected receipt deletion failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Delete().Remove(callback) })
	ack([]uint{messages[2].ID, messages[3].ID}, 500)
	if injected != 1 {
		t.Fatalf("injection count=%d", injected)
	}
	var compact MessageDeviceAckRange
	if err := db.Where("user_id = ?", recipient).Take(&compact).Error; err != nil || compact.FirstMessageID != messages[0].ID || compact.LastMessageID != messages[1].ID {
		t.Fatalf("rollback range=%+v err=%v", compact, err)
	}
	var count int64
	if err := db.Model(&MessageDeviceAck{}).Where("user_id = ?", recipient).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial receipts=%d err=%v", count, err)
	}
	r := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, testDeviceA, nil)
	var page struct {
		Items []ChatMessage `json:"items"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.Items[0].ID != messages[2].ID || page.Items[1].ID != messages[3].ID {
		t.Fatalf("rollback unread %d %s", r.Code, r.Body.String())
	}
	if err := db.Callback().Delete().Remove(callback); err != nil {
		t.Fatal(err)
	}
	ack([]uint{messages[2].ID, messages[3].ID}, 200)
	old := time.Now().UTC().Add(-deviceExpirationAge - time.Hour)
	if err := db.Model(&MessageDevice{}).Where("user_id = ?", recipient).UpdateColumn("last_seen_at", old).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := s.cleanupExpiredMessageDevices(context.Background(), time.Now(), 100)
	if err != nil || removed != 1 {
		t.Fatalf("expiry removed=%d err=%v", removed, err)
	}
	if err := db.Model(&MessageDeviceAckRange{}).Where("user_id = ?", recipient).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("expired ranges=%d err=%v", count, err)
	}
	r = requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, testDeviceA, nil)
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Items) != 4 {
		t.Fatalf("expired device replay: %d %s", r.Code, r.Body.String())
	}
}
