package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	opts := hybridOptions{users: 100, authors: 10, videos: 120, followsPerUser: 2, celebrityFollowers: 70, fanoutMaxFollowers: 50, likes: 1000}
	if err := seedHybrid(db, opts); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"users": 100, "videos": 120, "feed_fanout_jobs": 108, "video_actions": 1000, "media_assets": 10} {
		var got int64
		if err := db.Table(table).Count(&got).Error; err != nil || got != want {
			t.Fatalf("%s: count=%d want=%d err=%v", table, got, want, err)
		}
	}
	var pullCount int64
	if err := db.Model(&platform.Video{}).Where("feed_pull = ?", true).Count(&pullCount).Error; err != nil || pullCount != 12 {
		t.Fatalf("pull indexed videos=%d want=12 err=%v", pullCount, err)
	}
	var likeCount int64
	if err := db.Model(&platform.Video{}).Select("COALESCE(SUM(like_count), 0)").Scan(&likeCount).Error; err != nil || likeCount != 1000 {
		t.Fatalf("video like counts=%d want=1000 err=%v", likeCount, err)
	}
	var inboxCount int64
	if err := db.Model(&platform.FeedInbox{}).Count(&inboxCount).Error; err != nil || inboxCount == 0 {
		t.Fatalf("inbox rows=%d err=%v", inboxCount, err)
	}
	var assets []platform.MediaAsset
	if err := db.Order("id ASC").Find(&assets).Error; err != nil {
		t.Fatal(err)
	}
	if len(assets) != opts.authors {
		t.Fatalf("publishable fixture assets=%d want=%d", len(assets), opts.authors)
	}
	for i, asset := range assets {
		var owner platform.User
		if err := db.Where("username = ?", fmt.Sprintf("load-u-%06d", i)).First(&owner).Error; err != nil {
			t.Fatal(err)
		}
		if asset.OwnerID != owner.ID || asset.Kind != "video" {
			t.Errorf("asset %d ownership/kind = %d/%q, want %d/video", i, asset.OwnerID, asset.Kind, owner.ID)
		}
	}
	server := platform.NewServer(db, "fixture-secret-at-least-thirty-two-characters", t.TempDir())
	server.UseFeedFanout(opts.fanoutMaxFollowers)
	ownerToken := fixtureToken(t, server.Router(), "load-u-000000")
	mediaURL := assets[0].URL
	publish := httptest.NewRequest(http.MethodPost, "/api/videos", bytes.NewBufferString(fmt.Sprintf(`{"title":"load publish","media_url":%q}`, mediaURL)))
	publish.Header.Set("Content-Type", "application/json")
	publish.Header.Set("Authorization", "Bearer "+ownerToken)
	publishResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(publishResponse, publish)
	if publishResponse.Code != http.StatusCreated {
		t.Fatalf("fixture owner publish status=%d body=%s", publishResponse.Code, publishResponse.Body.String())
	}
	var published struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(publishResponse.Body.Bytes(), &published); err != nil || published.ID == 0 {
		t.Fatalf("published response=%s id=%d err=%v", publishResponse.Body.String(), published.ID, err)
	}
	var newFanoutJob int64
	if err := db.Model(&platform.FeedFanoutJob{}).Where("video_id = ? AND done_at IS NULL", published.ID).Count(&newFanoutJob).Error; err != nil || newFanoutJob != 1 {
		t.Fatalf("published video pending fanout jobs=%d want=1 err=%v", newFanoutJob, err)
	}
	wrongOwnerToken := fixtureToken(t, server.Router(), "load-u-000001")
	wrongOwner := httptest.NewRequest(http.MethodPost, "/api/videos", bytes.NewBufferString(fmt.Sprintf(`{"title":"wrong owner","media_url":%q}`, mediaURL)))
	wrongOwner.Header.Set("Content-Type", "application/json")
	wrongOwner.Header.Set("Authorization", "Bearer "+wrongOwnerToken)
	wrongOwnerResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(wrongOwnerResponse, wrongOwner)
	if wrongOwnerResponse.Code != http.StatusBadRequest {
		t.Fatalf("non-owner fixture publish status=%d body=%s", wrongOwnerResponse.Code, wrongOwnerResponse.Body.String())
	}
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

func fixtureToken(t *testing.T, handler http.Handler, username string) string {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"username":%q,"password":"load-fixture-password"}`, username))
	login := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
	login.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, login)
	if response.Code != http.StatusOK {
		t.Fatalf("login %s status=%d body=%s", username, response.Code, response.Body.String())
	}
	var session struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil || session.AccessToken == "" {
		t.Fatalf("login %s token=%q err=%v", username, session.AccessToken, err)
	}
	return session.AccessToken
}
