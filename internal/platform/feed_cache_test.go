package platform

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisFeedCacheHitInvalidationAndFallback(t *testing.T) {
	mock, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	client := redis.NewClient(&redis.Options{Addr: mock.Addr(), MaxRetries: 0})
	t.Cleanup(func() { _ = client.Close() })
	s, h, db := testServer(t)
	s.UseRedisFeedCache(client)
	authorID, token := registerTestUser(t, h, "cacheauthor")
	video := Video{AuthorID: authorID, Title: "first", MediaURL: "/media/owned.mp4", Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&MediaAsset{OwnerID: authorID, URL: video.MediaURL, Kind: "video", Size: 24}).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/feed?scene=timeline"
	first := request(t, h, http.MethodGet, path, "", nil)
	if first.Code != http.StatusOK || !bytes.Contains(first.Body.Bytes(), []byte(`"title":"first"`)) {
		t.Fatalf("first feed: %d %s", first.Code, first.Body.String())
	}
	if err := db.Model(&video).Update("title", "database only").Error; err != nil {
		t.Fatal(err)
	}
	cached := request(t, h, http.MethodGet, path, "", nil)
	if !bytes.Equal(first.Body.Bytes(), cached.Body.Bytes()) {
		t.Fatalf("expected a cached page, got %s", cached.Body.String())
	}
	// A normal update advances the version and the next anonymous read sees the change.
	updated := request(t, h, http.MethodPatch, fmt.Sprintf("/api/videos/%d", video.ID), token, map[string]string{"title": "updated"})
	if updated.Code != http.StatusOK {
		t.Fatalf("update: %d %s", updated.Code, updated.Body.String())
	}
	fresh := request(t, h, http.MethodGet, path, "", nil)
	if fresh.Code != http.StatusOK || !bytes.Contains(fresh.Body.Bytes(), []byte(`"title":"updated"`)) {
		t.Fatalf("feed was not invalidated: %d %s", fresh.Code, fresh.Body.String())
	}
	// Redis errors cannot take the Feed down; the database remains the source of truth.
	mock.Close()
	if err := db.Model(&video).Update("title", "fallback").Error; err != nil {
		t.Fatal(err)
	}
	fallback := request(t, h, http.MethodGet, path, "", nil)
	if fallback.Code != http.StatusOK || !bytes.Contains(fallback.Body.Bytes(), []byte(`"title":"fallback"`)) {
		t.Fatalf("database fallback: %d %s", fallback.Code, fallback.Body.String())
	}
}
