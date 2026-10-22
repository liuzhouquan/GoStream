package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestDirectHistoryPaginationAndBatchAck(t *testing.T) {
	_, h, db := testServer(t)
	alice, aliceToken := registerTestUser(t, h, "historyalice")
	bob, bobToken := registerTestUser(t, h, "historybob")
	charlie, _ := registerTestUser(t, h, "historycharlie")
	participants := [][2]uint{
		{bob, alice}, {alice, bob}, {bob, charlie},
		{bob, alice}, {alice, bob}, {bob, alice},
	}
	ids := make([]uint, len(participants))
	stamp := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	for i, pair := range participants {
		message := ChatMessage{SenderID: pair[0], RecipientID: pair[1], ClientID: fmt.Sprintf("history-%d", i), Body: fmt.Sprintf("message %d", i), CreatedAt: stamp}
		if err := db.Create(&message).Error; err != nil {
			t.Fatal(err)
		}
		ids[i] = message.ID
	}
	var got []uint
	before := uint(0)
	for page := 0; page < 3; page++ {
		path := fmt.Sprintf("/api/messages?user_id=%d&limit=2&before_id=%d", bob, before)
		response := request(t, h, http.MethodGet, path, aliceToken, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("history page %d: %d %s", page, response.Code, response.Body.String())
		}
		var result struct {
			Items      []ChatMessage `json:"items"`
			NextCursor uint          `json:"next_cursor"`
			HasMore    bool          `json:"has_more"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, message := range result.Items {
			got = append(got, message.ID)
		}
		if result.HasMore != (page < 2) || len(result.Items) != []int{2, 2, 1}[page] {
			t.Fatalf("page %d metadata: %+v", page, result)
		}
		before = result.NextCursor
	}
	want := []uint{ids[5], ids[4], ids[3], ids[1], ids[0]}
	if len(got) != len(want) {
		t.Fatalf("history IDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history IDs = %v, want %v", got, want)
		}
	}
	for _, path := range []string{
		"/api/messages?user_id=bad", fmt.Sprintf("/api/messages?user_id=%d", alice),
		fmt.Sprintf("/api/messages?user_id=%d&before_id=-1", bob),
		fmt.Sprintf("/api/messages?user_id=%d&limit=101", bob),
	} {
		if result := request(t, h, http.MethodGet, path, aliceToken, nil); result.Code != http.StatusBadRequest {
			t.Fatalf("invalid history %s: %d", path, result.Code)
		}
	}
	if result := request(t, h, http.MethodGet, fmt.Sprintf("/api/messages?user_id=%d", bob), "", nil); result.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated history: %d", result.Code)
	}

	ack := func(token string, batch []uint, status int, expectedCount int64) {
		t.Helper()
		result := request(t, h, http.MethodPost, "/api/messages/acks", token, map[string]any{"ids": batch})
		if result.Code != status {
			t.Fatalf("batch ACK %v: %d %s, want %d", batch, result.Code, result.Body.String(), status)
		}
		if status == http.StatusOK {
			var body struct {
				Acked      bool  `json:"acked"`
				AckedCount int64 `json:"acked_count"`
			}
			if err := json.Unmarshal(result.Body.Bytes(), &body); err != nil || !body.Acked || body.AckedCount != expectedCount {
				t.Fatalf("ACK response: %+v, err %v", body, err)
			}
		}
	}
	ack(aliceToken, []uint{ids[0], ids[1]}, http.StatusNotFound, 0) // Own outgoing message.
	ack(aliceToken, []uint{ids[0], ids[2]}, http.StatusNotFound, 0) // Another recipient.
	ack(bobToken, []uint{ids[0], ids[3]}, http.StatusNotFound, 0)
	ack(aliceToken, []uint{ids[0], ids[0]}, http.StatusBadRequest, 0)
	ack(aliceToken, []uint{}, http.StatusBadRequest, 0)
	ack(aliceToken, []uint{ids[0], ids[3]}, http.StatusOK, 2)
	ack(aliceToken, []uint{ids[0], ids[3]}, http.StatusOK, 0)
	var acked, unacked int64
	if err := db.Model(&ChatMessage{}).Where("id IN ? AND acked_at IS NOT NULL", []uint{ids[0], ids[3]}).Count(&acked).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&ChatMessage{}).Where("recipient_id = ? AND acked_at IS NULL", alice).Count(&unacked).Error; err != nil {
		t.Fatal(err)
	}
	if acked != 2 || unacked != 1 {
		t.Fatalf("acked=%d unacked=%d, want 2 and 1", acked, unacked)
	}
}
