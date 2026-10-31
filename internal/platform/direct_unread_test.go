package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDeviceUnreadAcrossGloballyAckedRanges(t *testing.T) {
	_, h, db := testServer(t)
	recipient, token := registerTestUser(t, h, "sparse_global_reader")
	sender, _ := registerTestUser(t, h, "sparse_global_sender")
	now := time.Now().UTC()
	messages := make([]ChatMessage, 902)
	for i := range messages {
		messages[i] = ChatMessage{SenderID: sender, RecipientID: recipient, ClientID: fmt.Sprintf("global-gap-%d", i), Body: "global gap", AckedAt: &now}
	}
	for _, i := range []int{0, 899, 901} {
		messages[i].AckedAt = nil
	}
	if err := db.CreateInBatches(&messages, 100).Error; err != nil {
		t.Fatal(err)
	}
	var ranges []MessageDeviceAckRange
	for i := 1; i < 900; i += 3 {
		ranges = append(ranges, MessageDeviceAckRange{UserID: recipient, DeviceID: testDeviceA, FirstMessageID: messages[i].ID, LastMessageID: messages[i+1].ID})
	}
	if err := db.CreateInBatches(&ranges, 100).Error; err != nil {
		t.Fatal(err)
	}
	check := func(status int) {
		t.Helper()
		r := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread?limit=2", token, testDeviceA, nil)
		if r.Code != status {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
		if status != 200 {
			if json.Valid(r.Body.Bytes()) == false {
				t.Fatal("error response is not JSON")
			}
			return
		}
		var page struct {
			Items []ChatMessage `json:"items"`
			More  bool          `json:"has_more"`
			Next  uint          `json:"next_cursor"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 || page.Items[0].ID != messages[0].ID || page.Items[1].ID != messages[901].ID || page.More || page.Next != messages[901].ID {
			t.Fatalf("lost boundary or leaked compacted message: %+v", page)
		}
	}
	check(200)
	// Fail only after a candidate page has already been read. No partial unread
	// page may escape when its receipt lookup fails; retry must recover fully.
	const failure = "test:fail_unread_range_lookup"
	if err := db.Callback().Query().Before("gorm:query").Register(failure, func(tx *gorm.DB) {
		if tx.Statement.Table == "message_device_ack_ranges" {
			tx.AddError(errors.New("injected interval read error"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(failure) })
	check(500)
	if err := db.Callback().Query().Remove(failure); err != nil {
		t.Fatal(err)
	}
	check(200)
}
