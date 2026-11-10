package platform

import (
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"net/http"
	"testing"
	"time"
)

func TestRecommendationIncludesAllFollowedAuthors(t *testing.T) {
	_, h, db := testServer(t)
	viewer, token := registerTestUser(t, h, "reviewviewer")
	authors := make([]User, 502)
	for i := range authors {
		authors[i] = User{Username: fmt.Sprintf("reviewauthor%d", i), Nickname: "review", PasswordHash: "unused"}
	}
	if err := db.Create(&authors).Error; err != nil {
		t.Fatal(err)
	}
	follows := make([]Follow, 501)
	for i := range follows {
		follows[i] = Follow{FollowerID: viewer, FolloweeID: authors[i].ID}
	}
	if err := db.Create(&follows).Error; err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	vids := []Video{{AuthorID: authors[500].ID, Title: "followed-501", MediaURL: "/media/x.mp4", Status: "published", CreatedAt: old}, {AuthorID: authors[501].ID, Title: "stranger", MediaURL: "/media/y.mp4", Status: "published", CreatedAt: old}}
	if err := db.Create(&vids).Error; err != nil {
		t.Fatal(err)
	}
	res := request(t, h, http.MethodGet, "/api/feed?scene=recommend&limit=1", token, nil)
	var page struct {
		Items []Video `json:"items"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != vids[0].ID {
		t.Fatalf("followed author #501 lost +12 preference: got %s", res.Body.String())
	}
}
func TestRecommendationRechecksPublishedStateDuringHydration(t *testing.T) {
	_, h, db := testServer(t)
	author, _ := registerTestUser(t, h, "reviewboundary")
	video := Video{AuthorID: author, Title: "private-after-selection", MediaURL: "/media/private.mp4", Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := db.Callback().Query().After("gorm:query").Register("review_publication_change", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]uint); ok && tx.Statement.Table == "videos" && !changed {
			changed = true
			if err := db.Model(&Video{}).Where("id = ?", video.ID).Update("status", "failed").Error; err != nil {
				t.Fatal(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	res := request(t, h, http.MethodGet, "/api/feed?scene=recommend&limit=20", "", nil)
	var page struct {
		Items []Video `json:"items"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusOK {
		t.Fatalf("recommendation request failed: %d %s", res.Code, res.Body.String())
	}
	if !changed {
		t.Fatal("publication-change seam not reached")
	}
	if len(page.Items) > 0 {
		t.Fatalf("unpublished video returned after candidate selection: %s", res.Body.String())
	}
}

func TestRecommendationUsesRecentActionsRatherThanLifetimeAuthors(t *testing.T) {
	_, h, db := testServer(t)
	oldAuthor, _ := registerTestUser(t, h, "recent_old")
	newAuthor, _ := registerTestUser(t, h, "recent_new")
	viewer, token := registerTestUser(t, h, "recent_viewer")
	stamp := time.Now().UTC().Add(-8 * 24 * time.Hour)
	older := Video{AuthorID: oldAuthor, Title: "old preference", MediaURL: "/media/old.mp4", Status: "published", CreatedAt: stamp}
	newer := Video{AuthorID: newAuthor, Title: "new preference", MediaURL: "/media/new.mp4", Status: "published", CreatedAt: stamp}
	if err := db.Create(&older).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&newer).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&VideoAction{UserID: viewer, VideoID: older.ID, Kind: "like"}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		v := Video{AuthorID: newAuthor, Title: fmt.Sprintf("history%d", i), MediaURL: "/media/history.mp4", Status: "deleted", CreatedAt: stamp}
		if err := db.Create(&v).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&VideoAction{UserID: viewer, VideoID: v.ID, Kind: "like"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// The old author's popularity wins only if stale, out-of-window preference is wrongly retained.
	if err := db.Model(&older).Update("like_count", 1).Error; err != nil {
		t.Fatal(err)
	}
	res := request(t, h, http.MethodGet, "/api/feed?scene=recommend&limit=1", token, nil)
	var page struct {
		Items []Video `json:"items"`
	}
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != newer.ID {
		t.Fatalf("recent preferences lost: %d %s", res.Code, res.Body.String())
	}
}

func TestRecommendationRetainsCursorWhenCandidateIsWithdrawn(t *testing.T) {
	_, h, db := testServer(t)
	author, _ := registerTestUser(t, h, "withdrawn_author")
	videos := []Video{
		{AuthorID: author, Title: "oldest", MediaURL: "/media/a.mp4", Status: "published"},
		{AuthorID: author, Title: "middle", MediaURL: "/media/b.mp4", Status: "published"},
		{AuthorID: author, Title: "withdrawn", MediaURL: "/media/c.mp4", Status: "published"},
	}
	if err := db.Create(&videos).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := db.Callback().Query().After("gorm:query").Register("withdraw_recommendation", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]uint); ok && tx.Statement.Table == "videos" && !changed {
			changed = true
			if err := db.Model(&Video{}).Where("id = ?", videos[2].ID).Update("status", "failed").Error; err != nil {
				t.Fatal(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items []Video `json:"items"`
		Next  string  `json:"next_cursor"`
		More  bool    `json:"has_more"`
	}
	first := request(t, h, http.MethodGet, "/api/feed?scene=recommend&limit=1", "", nil)
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != videos[1].ID || !page.More || page.Next == "" {
		t.Fatalf("withdrawal prematurely ended nonempty page: %d %s", first.Code, first.Body.String())
	}
	last := request(t, h, http.MethodGet, "/api/feed?scene=recommend&limit=1&cursor="+page.Next, "", nil)
	if last.Code != 200 || json.Unmarshal(last.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != videos[0].ID || page.More {
		t.Fatalf("remaining recommendation lost: %d %s", last.Code, last.Body.String())
	}
}
