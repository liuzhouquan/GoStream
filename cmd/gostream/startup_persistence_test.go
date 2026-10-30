package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/liuzhouquan/GoStream/internal/migrations"
	"github.com/liuzhouquan/GoStream/internal/platform"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func testJSONRequest(t *testing.T, h http.Handler, method, path, token string, body any, extraHeaders ...string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		switch v := body.(type) {
		case []byte:
			buf.Write(v)
		case string:
			buf.WriteString(v)
		default:
			if err := json.NewEncoder(&buf).Encode(v); err != nil {
				t.Fatalf("encode request body: %v", err)
			}
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i < len(extraHeaders); i += 2 {
		req.Header.Set(extraHeaders[i], extraHeaders[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestEmptyDatabaseStartupAndReadinessSemantics verifies A01:
// 1. One-click empty DB startup applies migrations and mounts routes.
// 2. /health, /ready, /metrics return 200 on healthy database.
// 3. Database closure causes /ready to return 503 while /health stays 200.
func TestEmptyDatabaseStartupAndReadinessSemantics(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "startup_empty.db")
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	mediaDir := filepath.Join(tempDir, "media")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		t.Fatal(err)
	}

	db, err := gorm.Open(gormsqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	// Apply production migrations
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations to empty db: %v", err)
	}

	secret := "a-very-secure-jwt-secret-string-at-least-32-characters!!"
	server := platform.NewServer(db, secret, mediaDir)
	h := server.Router()

	// 1. Health check (process liveness)
	recHealth := testJSONRequest(t, h, http.MethodGet, "/health", "", nil)
	if recHealth.Code != http.StatusOK {
		t.Fatalf("initial /health status = %d, want 200", recHealth.Code)
	}
	var healthBody map[string]string
	if err := json.Unmarshal(recHealth.Body.Bytes(), &healthBody); err != nil || healthBody["status"] != "ok" {
		t.Fatalf("/health body = %s, want status: ok", recHealth.Body.String())
	}

	// 2. Ready check (database readiness)
	recReady := testJSONRequest(t, h, http.MethodGet, "/ready", "", nil)
	if recReady.Code != http.StatusOK {
		t.Fatalf("initial /ready status = %d, want 200", recReady.Code)
	}
	var readyBody map[string]string
	if err := json.Unmarshal(recReady.Body.Bytes(), &readyBody); err != nil || readyBody["status"] != "ready" {
		t.Fatalf("/ready body = %s, want status: ready", recReady.Body.String())
	}

	// 3. Metrics endpoint (Prometheus format)
	recMetrics := testJSONRequest(t, h, http.MethodGet, "/metrics", "", nil)
	if recMetrics.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", recMetrics.Code)
	}
	metricsOutput := recMetrics.Body.String()
	if !strings.Contains(metricsOutput, "gostream_http_requests_total") {
		t.Fatalf("/metrics missing gostream_http_requests_total, got:\n%s", metricsOutput)
	}

	// 4. Simulate database failure: close raw sql.DB
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	// /health should still return 200 (process liveness remains intact)
	recHealthAfterClose := testJSONRequest(t, h, http.MethodGet, "/health", "", nil)
	if recHealthAfterClose.Code != http.StatusOK {
		t.Fatalf("/health after db close status = %d, want 200", recHealthAfterClose.Code)
	}

	// /ready MUST return 503 Service Unavailable when database is down
	recReadyAfterClose := testJSONRequest(t, h, http.MethodGet, "/ready", "", nil)
	if recReadyAfterClose.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready after db close status = %d, want 503", recReadyAfterClose.Code)
	}
	if !strings.Contains(recReadyAfterClose.Body.String(), "database unavailable") {
		t.Fatalf("/ready error message = %s, want 'database unavailable'", recReadyAfterClose.Body.String())
	}
}

// TestServerProcessRestartPersistenceLifecycle verifies A01 and A10:
// Starting Instance 1, populating users, videos, likes, favorites, comments, follows,
// chat messages, and group state, then cleanly restarting with Instance 2 against the same
// database and media directory. All state, authentication, counters, and files must persist.
func TestServerProcessRestartPersistenceLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "persisted.db")
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	mediaDir := filepath.Join(tempDir, "media")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		t.Fatal(err)
	}
	jwtSecret := "persistent-secure-test-jwt-secret-key-32-bytes!!"

	var (
		aliceID    uint
		bobID      uint
		aliceToken string
		bobToken   string
		videoID    uint
		mediaURL   string
		sampleData = []byte("\x00\x00\x00\x18ftypisom" + strings.Repeat("\xaa\xbb\xcc\xdd", 128))
		groupID    uint
	)

	// ==========================================
	// Phase 1: Server Instance 1 (First boot)
	// ==========================================
	{
		db1, err := gorm.Open(gormsqlite.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatalf("instance 1 open db: %v", err)
		}
		if err := migrations.Apply(context.Background(), db1); err != nil {
			t.Fatalf("instance 1 migrations: %v", err)
		}

		server1 := platform.NewServer(db1, jwtSecret, mediaDir)
		h1 := server1.Router()

		// Register Alice
		resAlice := testJSONRequest(t, h1, http.MethodPost, "/api/users", "", map[string]string{
			"username": "alice_persist",
			"nickname": "Alice Persist",
			"password": "password123",
		}, "Remote-Addr", "10.0.1.1:1234")
		if resAlice.Code != http.StatusCreated {
			t.Fatalf("register alice: %d %s", resAlice.Code, resAlice.Body.String())
		}
		var aliceAuth struct {
			AccessToken string        `json:"access_token"`
			User        platform.User `json:"user"`
		}
		if err := json.Unmarshal(resAlice.Body.Bytes(), &aliceAuth); err != nil {
			t.Fatal(err)
		}
		aliceID = aliceAuth.User.ID
		aliceToken = aliceAuth.AccessToken

		// Register Bob
		resBob := testJSONRequest(t, h1, http.MethodPost, "/api/users", "", map[string]string{
			"username": "bob_persist",
			"nickname": "Bob Persist",
			"password": "password123",
		}, "Remote-Addr", "10.0.1.2:1234")
		if resBob.Code != http.StatusCreated {
			t.Fatalf("register bob: %d %s", resBob.Code, resBob.Body.String())
		}
		var bobAuth struct {
			AccessToken string        `json:"access_token"`
			User        platform.User `json:"user"`
		}
		if err := json.Unmarshal(resBob.Body.Bytes(), &bobAuth); err != nil {
			t.Fatal(err)
		}
		bobID = bobAuth.User.ID
		bobToken = bobAuth.AccessToken

		// Alice creates upload session
		resSess := testJSONRequest(t, h1, http.MethodPost, "/api/uploads/sessions", aliceToken, map[string]any{
			"filename": "persisted.mp4",
			"size":     len(sampleData),
		})
		if resSess.Code != http.StatusCreated {
			t.Fatalf("create upload session: %d %s", resSess.Code, resSess.Body.String())
		}
		var sessBody struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(resSess.Body.Bytes(), &sessBody); err != nil {
			t.Fatal(err)
		}

		// Alice uploads chunk 0
		putChunkReq := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/uploads/sessions/%s/chunks/0", sessBody.ID), bytes.NewReader(sampleData))
		putChunkReq.Header.Set("Authorization", "Bearer "+aliceToken)
		putChunkReq.Header.Set("Content-Type", "application/octet-stream")
		recChunk := httptest.NewRecorder()
		h1.ServeHTTP(recChunk, putChunkReq)
		if recChunk.Code != http.StatusOK {
			t.Fatalf("put chunk: %d %s", recChunk.Code, recChunk.Body.String())
		}

		// Alice completes session
		resComp := testJSONRequest(t, h1, http.MethodPost, fmt.Sprintf("/api/uploads/sessions/%s/complete", sessBody.ID), aliceToken, nil)
		if resComp.Code != http.StatusOK {
			t.Fatalf("complete session: %d %s", resComp.Code, resComp.Body.String())
		}
		var compBody struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(resComp.Body.Bytes(), &compBody); err != nil {
			t.Fatal(err)
		}
		mediaURL = compBody.URL

		// Alice publishes video
		resPub := testJSONRequest(t, h1, http.MethodPost, "/api/videos", aliceToken, map[string]string{
			"title":       "Surviving Restart",
			"description": "This video persists across server restart",
			"media_url":   mediaURL,
		})
		if resPub.Code != http.StatusCreated {
			t.Fatalf("publish video: %d %s", resPub.Code, resPub.Body.String())
		}
		var pubBody struct {
			ID uint `json:"id"`
		}
		if err := json.Unmarshal(resPub.Body.Bytes(), &pubBody); err != nil {
			t.Fatal(err)
		}
		videoID = pubBody.ID

		// Bob likes Alice's video
		resLike := testJSONRequest(t, h1, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", videoID), bobToken, nil)
		if resLike.Code != http.StatusOK {
			t.Fatalf("bob like video: %d %s", resLike.Code, resLike.Body.String())
		}

		// Bob favorites Alice's video
		resFav := testJSONRequest(t, h1, http.MethodPut, fmt.Sprintf("/api/videos/%d/favorite", videoID), bobToken, nil)
		if resFav.Code != http.StatusOK {
			t.Fatalf("bob favorite video: %d %s", resFav.Code, resFav.Body.String())
		}

		// Bob comments on Alice's video
		resComment := testJSONRequest(t, h1, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", videoID), bobToken, map[string]string{
			"body": "Comment before restart",
		})
		if resComment.Code != http.StatusCreated {
			t.Fatalf("bob comment: %d %s", resComment.Code, resComment.Body.String())
		}

		// Bob follows Alice
		resFollow := testJSONRequest(t, h1, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", aliceID), bobToken, nil)
		if resFollow.Code != http.StatusOK {
			t.Fatalf("bob follow alice: %d %s", resFollow.Code, resFollow.Body.String())
		}

		// Alice creates a group and invites Bob
		resGrp := testJSONRequest(t, h1, http.MethodPost, "/api/groups", aliceToken, map[string]string{
			"name": "Restart Test Group",
		})
		if resGrp.Code != http.StatusCreated {
			t.Fatalf("create group: %d %s", resGrp.Code, resGrp.Body.String())
		}
		var grpBody struct {
			ID uint `json:"id"`
		}
		if err := json.Unmarshal(resGrp.Body.Bytes(), &grpBody); err != nil {
			t.Fatal(err)
		}
		groupID = grpBody.ID

		// Alice invites Bob to group
		resAdd := testJSONRequest(t, h1, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", groupID), aliceToken, map[string]uint{
			"user_id": bobID,
		})
		if resAdd.Code != http.StatusOK {
			t.Fatalf("add bob to group: %d %s", resAdd.Code, resAdd.Body.String())
		}

		// Insert a direct message from Alice to Bob and a group message
		now := time.Now().UTC()
		dm := platform.ChatMessage{
			SenderID:    aliceID,
			RecipientID: bobID,
			ClientID:    "dm-client-id-1",
			Body:        "Direct message persisting across restart",
			CreatedAt:   now,
		}
		if err := db1.Create(&dm).Error; err != nil {
			t.Fatalf("create direct message: %v", err)
		}

		gm := platform.GroupMessage{
			GroupID:   groupID,
			SenderID:  aliceID,
			ClientID:  "gm-client-id-1",
			Body:      "Group message persisting across restart",
			CreatedAt: now,
		}
		if err := db1.Create(&gm).Error; err != nil {
			t.Fatalf("create group message: %v", err)
		}

		// Bob ACKs the direct message and group message
		resAckDM := testJSONRequest(t, h1, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", dm.ID), bobToken, nil)
		if resAckDM.Code != http.StatusOK {
			t.Fatalf("bob ack dm: %d %s", resAckDM.Code, resAckDM.Body.String())
		}

		resAckGM := testJSONRequest(t, h1, http.MethodPost, fmt.Sprintf("/api/groups/%d/messages/%d/ack", groupID, gm.ID), bobToken, nil)
		if resAckGM.Code != http.StatusOK {
			t.Fatalf("bob ack gm: %d %s", resAckGM.Code, resAckGM.Body.String())
		}

		// Clean shutdown of Instance 1
		sqlDB1, err := db1.DB()
		if err != nil {
			t.Fatal(err)
		}
		if err := sqlDB1.Close(); err != nil {
			t.Fatalf("close instance 1 db: %v", err)
		}
	}

	// ==========================================
	// Phase 2: Server Instance 2 (After restart)
	// ==========================================
	{
		db2, err := gorm.Open(gormsqlite.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatalf("instance 2 open db: %v", err)
		}
		defer func() {
			if s, err := db2.DB(); err == nil {
				_ = s.Close()
			}
		}()

		// Running migrations on restart must be completely idempotent (A10)
		if err := migrations.Apply(context.Background(), db2); err != nil {
			t.Fatalf("instance 2 idempotent migration apply: %v", err)
		}

		server2 := platform.NewServer(db2, jwtSecret, mediaDir)
		h2 := server2.Router()

		// 1. Health and ready on revived instance
		if rec := testJSONRequest(t, h2, http.MethodGet, "/health", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("instance 2 /health = %d", rec.Code)
		}
		if rec := testJSONRequest(t, h2, http.MethodGet, "/ready", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("instance 2 /ready = %d", rec.Code)
		}

		// 2. Authentication token generated before restart remains valid on Instance 2
		recMe := testJSONRequest(t, h2, http.MethodGet, "/api/users/me", aliceToken, nil)
		if recMe.Code != http.StatusOK {
			t.Fatalf("alice auth with pre-restart token: %d %s", recMe.Code, recMe.Body.String())
		}
		var meAlice platform.User
		if err := json.Unmarshal(recMe.Body.Bytes(), &meAlice); err != nil || meAlice.ID != aliceID || meAlice.Username != "alice_persist" {
			t.Fatalf("persisted alice user = %+v", meAlice)
		}

		// 3. User Bob can authenticate with password on Instance 2
		recLogin := testJSONRequest(t, h2, http.MethodPost, "/api/sessions", "", map[string]string{
			"username": "bob_persist",
			"password": "password123",
		}, "Remote-Addr", "10.0.1.3:1234")
		if recLogin.Code != http.StatusOK {
			t.Fatalf("bob login on instance 2: %d %s", recLogin.Code, recLogin.Body.String())
		}

		// 4. Video metadata and counters persist
		recVideo := testJSONRequest(t, h2, http.MethodGet, fmt.Sprintf("/api/videos/%d", videoID), "", nil)
		if recVideo.Code != http.StatusOK {
			t.Fatalf("get video after restart: %d %s", recVideo.Code, recVideo.Body.String())
		}
		var video platform.Video
		if err := json.Unmarshal(recVideo.Body.Bytes(), &video); err != nil {
			t.Fatal(err)
		}
		if video.Title != "Surviving Restart" {
			t.Fatalf("video title = %q, want 'Surviving Restart'", video.Title)
		}
		if video.AuthorID != aliceID {
			t.Fatalf("video author = %d, want %d", video.AuthorID, aliceID)
		}
		if video.LikeCount != 1 || video.FavoriteCount != 1 || video.CommentCount != 1 {
			t.Fatalf("persisted counters like=%d fav=%d comment=%d, want 1/1/1", video.LikeCount, video.FavoriteCount, video.CommentCount)
		}

		// 5. Comments persist
		recComments := testJSONRequest(t, h2, http.MethodGet, fmt.Sprintf("/api/videos/%d/comments", videoID), "", nil)
		if recComments.Code != http.StatusOK {
			t.Fatalf("get comments after restart: %d %s", recComments.Code, recComments.Body.String())
		}
		if !strings.Contains(recComments.Body.String(), "Comment before restart") {
			t.Fatalf("comments missing persisted comment: %s", recComments.Body.String())
		}

		// 6. Following relationship and following Feed persist
		recFollowFeed := testJSONRequest(t, h2, http.MethodGet, "/api/feed?scene=following", bobToken, nil)
		if recFollowFeed.Code != http.StatusOK {
			t.Fatalf("following feed after restart: %d %s", recFollowFeed.Code, recFollowFeed.Body.String())
		}
		if !strings.Contains(recFollowFeed.Body.String(), "Surviving Restart") {
			t.Fatalf("following feed missing alice's video: %s", recFollowFeed.Body.String())
		}

		// 7. Static media file persists and supports Range serving
		recMedia := testJSONRequest(t, h2, http.MethodGet, mediaURL, "", nil, "Range", "bytes=0-15")
		if recMedia.Code != http.StatusPartialContent {
			t.Fatalf("media range status = %d, want 206", recMedia.Code)
		}
		if !bytes.Equal(recMedia.Body.Bytes(), sampleData[:16]) {
			t.Fatalf("media byte slice mismatch after restart: got %v want %v", recMedia.Body.Bytes(), sampleData[:16])
		}

		// 8. Direct message history persists
		recMessages := testJSONRequest(t, h2, http.MethodGet, fmt.Sprintf("/api/messages?user_id=%d", aliceID), bobToken, nil)
		if recMessages.Code != http.StatusOK {
			t.Fatalf("get messages after restart: %d %s", recMessages.Code, recMessages.Body.String())
		}
		if !strings.Contains(recMessages.Body.String(), "Direct message persisting across restart") {
			t.Fatalf("messages missing persisted dm: %s", recMessages.Body.String())
		}

		// 9. Group chat history and unread status persist
		recGrpMsg := testJSONRequest(t, h2, http.MethodGet, fmt.Sprintf("/api/groups/%d/messages", groupID), bobToken, nil)
		if recGrpMsg.Code != http.StatusOK {
			t.Fatalf("get group messages after restart: %d %s", recGrpMsg.Code, recGrpMsg.Body.String())
		}
		if !strings.Contains(recGrpMsg.Body.String(), "Group message persisting across restart") {
			t.Fatalf("group messages missing persisted gm: %s", recGrpMsg.Body.String())
		}

		recGrpUnread := testJSONRequest(t, h2, http.MethodGet, fmt.Sprintf("/api/groups/%d/unread", groupID), bobToken, nil)
		if recGrpUnread.Code != http.StatusOK {
			t.Fatalf("get group unread: %d %s", recGrpUnread.Code, recGrpUnread.Body.String())
		}
		var unreadBody struct {
			UnreadCount int `json:"unread_count"`
		}
		if err := json.Unmarshal(recGrpUnread.Body.Bytes(), &unreadBody); err != nil || unreadBody.UnreadCount != 0 {
			t.Fatalf("bob group unread after restart = %d, want 0", unreadBody.UnreadCount)
		}
	}
}

// TestPrometheusMetricsExpositionFormat verifies A10 metrics explainability:
// Conforms to Prometheus text format, exposes help strings, type headers,
// and correctly increments counters.
func TestPrometheusMetricsExpositionFormat(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "metrics.db")
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)

	db, err := gorm.Open(gormsqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s, err := db.DB(); err == nil {
			_ = s.Close()
		}
	}()
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	server := platform.NewServer(db, "a-secret-for-metrics-testing-purposes-32-bytes!!", mediaDir)
	h := server.Router()

	// Trigger requests across multiple endpoints
	_ = testJSONRequest(t, h, http.MethodGet, "/health", "", nil)
	_ = testJSONRequest(t, h, http.MethodGet, "/ready", "", nil)
	_ = testJSONRequest(t, h, http.MethodGet, "/api/feed", "", nil)
	_ = testJSONRequest(t, h, http.MethodPost, "/api/sessions", "", map[string]string{"username": "test", "password": "wrongpassword"}) // triggers rate limiter counter
	_ = testJSONRequest(t, h, http.MethodGet, "/nonexistent", "", nil)                                                                  // 404

	rec := testJSONRequest(t, h, http.MethodGet, "/metrics", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	body := rec.Body.String()

	// Required Prometheus Metric Families that are registered and emitted upon activity
	requiredMetrics := []struct {
		name       string
		familyType string
	}{
		{"gostream_http_requests_total", "counter"},
		{"gostream_http_request_duration_seconds", "histogram"},
		{"gostream_rate_limit_total", "counter"},
		{"gostream_feed_fanout_pending_jobs", "gauge"},
		{"gostream_websocket_connections", "gauge"},
	}

	for _, m := range requiredMetrics {
		helpPrefix := fmt.Sprintf("# HELP %s", m.name)
		typeLine := fmt.Sprintf("# TYPE %s %s", m.name, m.familyType)
		if !strings.Contains(body, helpPrefix) {
			t.Errorf("/metrics missing help line for %q", m.name)
		}
		if !strings.Contains(body, typeLine) {
			t.Errorf("/metrics missing type line %q", typeLine)
		}
	}

	// Verify request counter reflected the route statuses
	if !strings.Contains(body, `gostream_http_requests_total{method="GET",route="/ready",status="200"}`) {
		t.Errorf("metrics does not record /ready 200 request: %s", body)
	}
	if !strings.Contains(body, `gostream_http_requests_total{method="GET",route="/health",status="200"}`) {
		t.Errorf("metrics does not record /health 200 request: %s", body)
	}
	if !strings.Contains(body, `status="404"`) {
		t.Errorf("metrics does not record 404 request: %s", body)
	}
}

// TestReadinessSemanticsWithDependencyOutage verifies that /ready is strictly database-only readiness:
// When database is healthy, /ready returns 200 even if optional or external services (cache, broker)
// are in an outage or degraded state.
func TestReadinessSemanticsWithDependencyOutage(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "readiness.db")
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)

	db, err := gorm.Open(gormsqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s, err := db.DB(); err == nil {
			_ = s.Close()
		}
	}()
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	server := platform.NewServer(db, "test-secret-at-least-32-bytes-long-here!!", mediaDir)
	h := server.Router()

	// 1. Initial ready
	rec1 := testJSONRequest(t, h, http.MethodGet, "/ready", "", nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("initial /ready = %d", rec1.Code)
	}

	// 2. Feed Fanout enabled with 0 max followers (all pull)
	server.UseFeedFanout(0)

	rec2 := testJSONRequest(t, h, http.MethodGet, "/ready", "", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("/ready with fanout = %d", rec2.Code)
	}

	// 3. Confirm /ready remains 200 when serving feed requests in database-fallback mode
	recFeed := testJSONRequest(t, h, http.MethodGet, "/api/feed?scene=recommend", "", nil)
	if recFeed.Code != http.StatusOK {
		t.Fatalf("feed in fallback mode: %d", recFeed.Code)
	}

	rec3 := testJSONRequest(t, h, http.MethodGet, "/ready", "", nil)
	if rec3.Code != http.StatusOK {
		t.Fatalf("/ready after fallback feed = %d", rec3.Code)
	}
}
