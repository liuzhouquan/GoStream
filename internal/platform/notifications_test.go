package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNotificationPagesAndBoundedRead(t *testing.T) {
	_, h, db := testServer(t)
	ownerID, ownerToken := registerTestUser(t, h, "noticeowner")
	otherID, otherToken := registerTestUser(t, h, "noticeother")
	stamp := time.Now().UTC().Truncate(time.Second)
	items := make([]Notification, 5)
	for i := range items {
		items[i] = Notification{UserID: ownerID, EventKey: fmt.Sprintf("notice-page-%d", i), Kind: "follow", ActorID: otherID, Text: fmt.Sprintf("notice %d", i), CreatedAt: stamp}
		if err := db.Create(&items[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	foreign := Notification{UserID: otherID, EventKey: "notice-foreign", Kind: "follow", ActorID: ownerID, CreatedAt: stamp}
	if err := db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items      []Notification `json:"items"`
		NextCursor uint           `json:"next_cursor"`
		HasMore    bool           `json:"has_more"`
	}
	readPage := func(path string, want []uint, more bool) uint {
		t.Helper()
		result := request(t, h, http.MethodGet, path, ownerToken, nil)
		if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &page) != nil {
			t.Fatalf("notification page: %d %s", result.Code, result.Body.String())
		}
		if len(page.Items) != len(want) || page.HasMore != more {
			t.Fatalf("notification page IDs=%+v more=%v, want %v more=%v", page.Items, page.HasMore, want, more)
		}
		for i, id := range want {
			if page.Items[i].ID != id || page.Items[i].Actor.ID != otherID {
				t.Fatalf("notification page item %d: %+v", i, page.Items[i])
			}
		}
		if len(want) > 0 && page.NextCursor != want[len(want)-1] {
			t.Fatalf("notification next cursor: %d, want %d", page.NextCursor, want[len(want)-1])
		}
		return page.NextCursor
	}
	firstCursor := readPage("/api/notifications?limit=2", []uint{items[4].ID, items[3].ID}, true)
	newNotice := Notification{UserID: ownerID, EventKey: "notice-new", Kind: "comment", ActorID: otherID, CreatedAt: stamp}
	if err := db.Create(&newNotice).Error; err != nil {
		t.Fatal(err)
	}
	secondCursor := readPage(fmt.Sprintf("/api/notifications?limit=2&before_id=%d", firstCursor), []uint{items[2].ID, items[1].ID}, true)
	readPage(fmt.Sprintf("/api/notifications?limit=2&before_id=%d", secondCursor), []uint{items[0].ID}, false)
	for _, path := range []string{"/api/notifications?limit=0", "/api/notifications?limit=101", "/api/notifications?limit=bad", "/api/notifications?before_id=bad"} {
		if result := request(t, h, http.MethodGet, path, ownerToken, nil); result.Code != http.StatusBadRequest {
			t.Fatalf("invalid notification page %q: %d", path, result.Code)
		}
	}
	for _, path := range []string{"/api/notifications/read?up_to_id=0", "/api/notifications/read?up_to_id=bad", "/api/notifications/read?up_to_id="} {
		if result := request(t, h, http.MethodPatch, path, ownerToken, nil); result.Code != http.StatusBadRequest {
			t.Fatalf("invalid read bound %q: %d", path, result.Code)
		}
	}
	markPath := fmt.Sprintf("/api/notifications/read?up_to_id=%d", items[4].ID)
	if result := request(t, h, http.MethodPatch, markPath, ownerToken, nil); result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"read_count":5`) {
		t.Fatalf("bounded mark read: %d %s", result.Code, result.Body.String())
	}
	if result := request(t, h, http.MethodPatch, markPath, ownerToken, nil); result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"read_count":0`) {
		t.Fatalf("repeat bounded mark: %d %s", result.Code, result.Body.String())
	}
	if result := request(t, h, http.MethodGet, "/api/notifications/unread", ownerToken, nil); result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"unread_count":1`) {
		t.Fatalf("new notice unread: %d %s", result.Code, result.Body.String())
	}
	if result := request(t, h, http.MethodGet, "/api/notifications/unread", otherToken, nil); result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"unread_count":1`) {
		t.Fatalf("other account unread: %d %s", result.Code, result.Body.String())
	}
	if result := request(t, h, http.MethodPatch, "/api/notifications/read", ownerToken, nil); result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"read_count":1`) {
		t.Fatalf("legacy mark all: %d %s", result.Code, result.Body.String())
	}
	if result := request(t, h, http.MethodGet, "/api/notifications/unread", otherToken, nil); result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"unread_count":1`) {
		t.Fatalf("other account changed: %d %s", result.Code, result.Body.String())
	}
}

func TestNotificationDatabaseErrors(t *testing.T) {
	_, h, db := testServer(t)
	_, token := registerTestUser(t, h, "noticeerrors")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/notifications", "/api/notifications/unread", "/api/notifications/read"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/read") {
			method = http.MethodPatch
		}
		if result := request(t, h, method, path, token, nil); result.Code != http.StatusInternalServerError {
			t.Fatalf("database error %s: %d %s", path, result.Code, result.Body.String())
		}
	}
}
