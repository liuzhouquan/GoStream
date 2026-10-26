package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/liuzhouquan/GoStream/internal/platform"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestHybridFixtureExercisesBothFeedBranches(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "fixture.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	opts := hybridOptions{users: 100, authors: 10, videos: 120, followsPerUser: 2, celebrityFollowers: 70, fanoutMaxFollowers: 50}
	if err := seedHybrid(db, opts); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"users": 100, "videos": 120, "feed_fanout_jobs": 108} {
		var got int64
		if err := db.Table(table).Count(&got).Error; err != nil || got != want {
			t.Fatalf("%s: count=%d want=%d err=%v", table, got, want, err)
		}
	}
	var inboxCount int64
	if err := db.Model(&platform.FeedInbox{}).Count(&inboxCount).Error; err != nil || inboxCount == 0 {
		t.Fatalf("inbox rows=%d err=%v", inboxCount, err)
	}
	server := platform.NewServer(db, "fixture-secret-at-least-thirty-two-characters", t.TempDir())
	server.UseFeedFanout(opts.fanoutMaxFollowers)
	loginBody := []byte(`{"username":"load-u-000010","password":"load-fixture-password"}`)
	login := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(loginBody))
	login.Header.Set("Content-Type", "application/json")
	loginResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	var session struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	feed := httptest.NewRequest(http.MethodGet, "/api/feed?scene=following&limit=30", nil)
	feed.Header.Set("Authorization", "Bearer "+session.AccessToken)
	feedResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(feedResponse, feed)
	if feedResponse.Code != http.StatusOK {
		t.Fatalf("feed status=%d body=%s", feedResponse.Code, feedResponse.Body.String())
	}
	var page struct {
		Items []platform.Video `json:"items"`
	}
	if err := json.Unmarshal(feedResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var high, ordinary bool
	var highAuthor platform.User
	if err := db.Where("username = ?", "load-u-000009").First(&highAuthor).Error; err != nil {
		t.Fatal(err)
	}
	for _, video := range page.Items {
		if video.AuthorID == highAuthor.ID {
			high = true
		} else {
			ordinary = true
		}
	}
	if !high || !ordinary {
		t.Fatalf("following page did not include both pull and push authors: high=%t ordinary=%t", high, ordinary)
	}
	if err := seedHybrid(db, opts); err == nil {
		t.Fatal("seeding a nonempty database must be rejected")
	}
}
