package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRecommendationUsesPreferencesAndCursor(t *testing.T) {
	_, h, db := testServer(t)
	authorA, _ := registerTestUser(t, h, "recommend_a")
	authorB, _ := registerTestUser(t, h, "recommend_b")
	authorC, _ := registerTestUser(t, h, "recommend_c")
	viewer, token := registerTestUser(t, h, "recommend_viewer")
	stamp := time.Now().UTC().Add(-8 * 24 * time.Hour)
	for _, author := range []uint{authorA, authorB, authorC} {
		if err := db.Create(&Video{AuthorID: author, Title: fmt.Sprintf("author %d", author), MediaURL: "/media/test.mp4", Status: "published", CreatedAt: stamp}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&Follow{FollowerID: viewer, FolloweeID: authorB}).Error; err != nil {
		t.Fatal(err)
	}
	history := Video{AuthorID: authorC, Title: "previously liked", MediaURL: "/media/old.mp4", Status: "deleted", CreatedAt: stamp}
	if err := db.Create(&history).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&VideoAction{UserID: viewer, VideoID: history.ID, Kind: "like"}).Error; err != nil {
		t.Fatal(err)
	}
	seen := map[uint]bool{}
	want := []uint{authorB, authorC, authorA}
	cursor := ""
	for pageNumber := 0; pageNumber < 3; pageNumber++ {
		path := "/api/feed?scene=recommend&limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		res := request(t, h, http.MethodGet, path, token, nil)
		var page struct {
			Items      []Video `json:"items"`
			NextCursor string  `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		}
		if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
			t.Fatalf("recommend page %d: %d %s", pageNumber, res.Code, res.Body.String())
		}
		if page.Items[0].AuthorID != want[pageNumber] || seen[page.Items[0].ID] {
			t.Fatalf("recommend order page %d: got author %d, want %d", pageNumber, page.Items[0].AuthorID, want[pageNumber])
		}
		seen[page.Items[0].ID] = true
		if page.HasMore != (pageNumber < 2) {
			t.Fatalf("unexpected has_more on page %d: %v", pageNumber, page.HasMore)
		}
		if pageNumber == 0 {
			if wrong := request(t, h, http.MethodGet, "/api/feed?scene=hot&cursor="+page.NextCursor, token, nil); wrong.Code != http.StatusBadRequest {
				t.Fatalf("cross-scene cursor status %d", wrong.Code)
			}
		}
		cursor = page.NextCursor
	}
}

func TestRecommendationFavorsFreshContentWithoutPreferences(t *testing.T) {
	_, h, db := testServer(t)
	author, _ := registerTestUser(t, h, "fresh_author")
	old := Video{AuthorID: author, Title: "old", MediaURL: "/media/old.mp4", Status: "published", CreatedAt: time.Now().UTC().Add(-8 * 24 * time.Hour)}
	fresh := Video{AuthorID: author, Title: "fresh", MediaURL: "/media/fresh.mp4", Status: "published", CreatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	res := request(t, h, http.MethodGet, "/api/feed?scene=recommend&limit=2", "", nil)
	var page struct {
		Items []Video `json:"items"`
	}
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.Items[0].ID != fresh.ID {
		t.Fatalf("fresh recommendation: %d %s", res.Code, res.Body.String())
	}
	for _, bad := range []feedCursor{
		{ID: fresh.ID, Score: 12, Scene: "recommend"},
		{ID: fresh.ID, Score: 12, Scene: "recommend", Anchor: time.Now().Add(-48 * time.Hour).UnixNano()},
	} {
		if r := request(t, h, http.MethodGet, "/api/feed?scene=recommend&cursor="+encodeCursor(bad), "", nil); r.Code != http.StatusBadRequest {
			t.Fatalf("invalid recommendation cursor returned %d", r.Code)
		}
	}
}
