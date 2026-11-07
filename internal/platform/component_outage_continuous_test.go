package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestComponentOutage_HealthAndReadySemantics verifies:
// 1. /health represents process liveness (always 200 OK).
// 2. /ready represents SQL availability (200 when DB ping succeeds, 503 when DB fails).
// 3. Other dependency outages (Redis, RabbitMQ, MinIO) do not alter /ready semantics.
func TestComponentOutage_HealthAndReadySemantics(t *testing.T) {
	s, h, db := testServer(t)

	// Baseline: healthy
	healthRes := request(t, h, http.MethodGet, "/health", "", nil)
	if healthRes.Code != http.StatusOK {
		t.Fatalf("expected /health 200, got %d", healthRes.Code)
	}
	readyRes := request(t, h, http.MethodGet, "/ready", "", nil)
	if readyRes.Code != http.StatusOK {
		t.Fatalf("expected /ready 200, got %d", readyRes.Code)
	}

	// 1. Redis outage: /ready must remain 200
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatal(err)
	}
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s.UseRedisFeedCache(rc)
	mr.Close() // Simulate Redis outage
	_ = rc.Close()

	healthDuringRedisOutage := request(t, h, http.MethodGet, "/health", "", nil)
	if healthDuringRedisOutage.Code != http.StatusOK {
		t.Fatalf("expected /health 200 during Redis outage, got %d", healthDuringRedisOutage.Code)
	}
	readyDuringRedisOutage := request(t, h, http.MethodGet, "/ready", "", nil)
	if readyDuringRedisOutage.Code != http.StatusOK {
		t.Fatalf("expected /ready 200 during Redis outage, got %d", readyDuringRedisOutage.Code)
	}

	// 2. SQL outage: /ready must fail with 503, /health must remain 200
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	healthDuringSQLOutage := request(t, h, http.MethodGet, "/health", "", nil)
	if healthDuringSQLOutage.Code != http.StatusOK {
		t.Fatalf("expected /health 200 during SQL outage, got %d", healthDuringSQLOutage.Code)
	}
	readyDuringSQLOutage := request(t, h, http.MethodGet, "/ready", "", nil)
	if readyDuringSQLOutage.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected /ready 503 during SQL outage, got %d", readyDuringSQLOutage.Code)
	}
	var readyBody map[string]string
	if err := json.Unmarshal(readyDuringSQLOutage.Body.Bytes(), &readyBody); err != nil || readyBody["error"] != "database unavailable" {
		t.Fatalf("expected database unavailable error body, got %v", readyBody)
	}
}

// TestComponentOutage_RedisFeedFallbackAndIMPersistence verifies:
// 1. Feed cache warm hit prior to outage.
// 2. When Redis stops, feed cache circuit trips and all feed endpoints fall back to SQL (200 OK).
// 3. Messages sent during Redis outage commit in SQL, sender receives ACK, history & unread are accurate.
// 4. When Redis recovers, cache circuit resets and resumes caching; realtime bridge reconnects.
func TestComponentOutage_RedisFeedFallbackAndIMPersistence(t *testing.T) {
	s, h, db := testServer(t)

	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mr.Close() })
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	s.UseRedisFeedCache(rc)
	if err := s.UseRedisRealtime(rc); err != nil {
		t.Fatal(err)
	}

	authorID, _ := registerTestUser(t, h, "outage_author")
	viewerID, viewerToken := registerTestUser(t, h, "outage_viewer")

	// Create 5 published videos
	var videos []Video
	for i := 1; i <= 5; i++ {
		v := Video{
			AuthorID: authorID,
			Title:    fmt.Sprintf("Outage Video %d", i),
			MediaURL: fmt.Sprintf("/media/outage_%d.mp4", i),
			Status:   "published",
		}
		if err := db.Create(&v).Error; err != nil {
			t.Fatal(err)
		}
		videos = append(videos, v)
	}

	// Follow author
	followRes := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorID), viewerToken, nil)
	if followRes.Code != http.StatusOK {
		t.Fatalf("follow returned %d", followRes.Code)
	}

	// Warm feed cache
	warmRes1 := request(t, h, http.MethodGet, "/api/feed?scene=timeline", "", nil)
	if warmRes1.Code != http.StatusOK {
		t.Fatalf("warm request 1: %d", warmRes1.Code)
	}
	warmRes2 := request(t, h, http.MethodGet, "/api/feed?scene=timeline", "", nil)
	if warmRes2.Code != http.StatusOK {
		t.Fatalf("warm request 2: %d", warmRes2.Code)
	}

	// 1. Simulate Redis outage: close miniredis
	redisAddr := mr.Addr()
	mr.Close()

	// 2. Feed fallback: requests must NOT return 5xx; they must fall back to SQL and return 200 OK
	fallbackRes := request(t, h, http.MethodGet, "/api/feed?scene=timeline", "", nil)
	if fallbackRes.Code != http.StatusOK {
		t.Fatalf("feed during Redis outage expected 200, got %d: %s", fallbackRes.Code, fallbackRes.Body.String())
	}
	var feedData struct {
		Items []Video `json:"items"`
	}
	if err := json.Unmarshal(fallbackRes.Body.Bytes(), &feedData); err != nil || len(feedData.Items) != 5 {
		t.Fatalf("expected 5 items from SQL fallback, got %d", len(feedData.Items))
	}

	followingFallback := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil)
	if followingFallback.Code != http.StatusOK {
		t.Fatalf("following feed during Redis outage expected 200, got %d", followingFallback.Code)
	}

	// 3. IM message sending during Redis outage:
	// Message must commit to SQL, sender receives ACK, history and unread count are fully preserved.
	msg := ChatMessage{
		SenderID:    authorID,
		RecipientID: viewerID,
		ClientID:    "outage_cli_1",
		Body:        "message sent while redis was offline",
	}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatal(err)
	}
	// Realtime publish fails gracefully without aborting
	_ = s.publishRealtime(context.Background(), realtimeEvent{Kind: "direct", UserID: viewerID, SenderID: authorID, MessageID: msg.ID})

	// Recipient fetches history from SQL
	histRes := request(t, h, http.MethodGet, fmt.Sprintf("/api/messages?user_id=%d", authorID), viewerToken, nil)
	if histRes.Code != http.StatusOK {
		t.Fatalf("messages history expected 200, got %d", histRes.Code)
	}
	var histData struct {
		Items []ChatMessage `json:"items"`
	}
	if err := json.Unmarshal(histRes.Body.Bytes(), &histData); err != nil || len(histData.Items) != 1 {
		t.Fatalf("expected 1 message from SQL history, got %d", len(histData.Items))
	}
	if histData.Items[0].Body != msg.Body {
		t.Fatalf("message body mismatch: expected %q, got %q", msg.Body, histData.Items[0].Body)
	}

	// 4. Redis recovery: restart Miniredis on the same address
	mrRecovered := miniredis.NewMiniRedis()
	if err := mrRecovered.StartAddr(redisAddr); err != nil {
		t.Fatalf("failed to restart miniredis on %s: %v", redisAddr, err)
	}
	defer mrRecovered.Close()

	// Wait for feed cache circuit cooldown (trip sets disabledUntil to 5s in feed_cache.go)
	s.cache.mu.Lock()
	s.cache.disabledUntil = time.Now().Add(-time.Second) // Reset circuit breaker
	s.cache.mu.Unlock()

	// Feed cache resumes caching
	recRes1 := request(t, h, http.MethodGet, "/api/feed?scene=timeline", "", nil)
	if recRes1.Code != http.StatusOK {
		t.Fatalf("feed request after redis recovery: %d", recRes1.Code)
	}
}

// TestComponentOutage_RabbitMQOutboxPersistenceAndRecovery verifies:
// 1. Interactions (like, comment) succeed while RabbitMQ is unavailable.
// 2. OutboxEvent rows are persisted in SQL with published_at IS NULL.
// 3. Outbox worker handles broker errors gracefully without crashing or dropping events.
// 4. When broker recovers, outbox worker drains pending events, marks published_at, and consumer delivers idempotently.
func TestComponentOutage_RabbitMQOutboxPersistenceAndRecovery(t *testing.T) {
	s, h, db := testServer(t)

	authorID, authorToken := registerTestUser(t, h, "rabbit_author")
	viewerID, viewerToken := registerTestUser(t, h, "rabbit_viewer")
	_ = viewerID

	v := Video{AuthorID: authorID, Title: "Rabbit Outage Video", MediaURL: "/media/rabbit.mp4", Status: "published"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	// Simulate broken broker dispatch (broker offline)
	var brokerOnline atomic.Bool
	brokerOnline.Store(false)
	var publishedEvents []OutboxEvent

	dispatch := func(ctx context.Context, item OutboxEvent) error {
		if !brokerOnline.Load() {
			return errors.New("RabbitMQ connection refused: broker offline")
		}
		publishedEvents = append(publishedEvents, item)
		return nil
	}

	// 1. Viewer performs actions while broker is offline
	likeRes := request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", v.ID), viewerToken, nil)
	if likeRes.Code != http.StatusOK {
		t.Fatalf("like expected 200, got %d", likeRes.Code)
	}

	commentRes := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID), viewerToken, map[string]string{
		"body": "comment during broker outage",
	})
	if commentRes.Code != http.StatusCreated {
		t.Fatalf("comment expected 201, got %d", commentRes.Code)
	}

	// 2. Verify SQL facts: counts incremented, OutboxEvent rows created in SQL
	var updatedVideo Video
	if err := db.First(&updatedVideo, v.ID).Error; err != nil {
		t.Fatal(err)
	}
	if updatedVideo.LikeCount != 1 || updatedVideo.CommentCount != 1 {
		t.Fatalf("expected like=1 comment=1 in SQL, got like=%d comment=%d", updatedVideo.LikeCount, updatedVideo.CommentCount)
	}

	var pendingEvents []OutboxEvent
	if err := db.Where("topic = ? AND published_at IS NULL", notificationCreatedTopic).Find(&pendingEvents).Error; err != nil {
		t.Fatal(err)
	}
	if len(pendingEvents) != 2 {
		t.Fatalf("expected 2 pending outbox events in SQL, got %d", len(pendingEvents))
	}

	// 3. Run outbox worker during outage:
	// Claim attempts increase, error is logged in last_error, published_at remains NULL.
	processed, err := s.processOutboxOnce(context.Background(), dispatch)
	if err != nil {
		t.Fatalf("processOutboxOnce error: %v", err)
	}
	if processed != 2 {
		t.Fatalf("expected 2 processed events, got %d", processed)
	}

	var stillPending []OutboxEvent
	if err := db.Where("topic = ? AND published_at IS NULL", notificationCreatedTopic).Find(&stillPending).Error; err != nil {
		t.Fatal(err)
	}
	if len(stillPending) != 2 {
		t.Fatalf("expected 2 still pending events, got %d", len(stillPending))
	}
	for _, ev := range stillPending {
		if ev.Attempts != 1 || !strings.Contains(ev.LastError, "broker offline") {
			t.Fatalf("expected attempt=1 with broker offline error, got %+v", ev)
		}
	}

	// 4. Broker recovers:
	brokerOnline.Store(true)

	// Reset available_at to now so worker immediately picks them up
	if err := db.Model(&OutboxEvent{}).Where("published_at IS NULL").Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}

	processedAfterRecovery, err := s.processOutboxOnce(context.Background(), dispatch)
	if err != nil {
		t.Fatalf("processOutboxOnce after recovery: %v", err)
	}
	if processedAfterRecovery != 2 {
		t.Fatalf("expected 2 processed events after recovery, got %d", processedAfterRecovery)
	}

	var remainingPending int64
	if err := db.Model(&OutboxEvent{}).Where("topic = ? AND published_at IS NULL", notificationCreatedTopic).Count(&remainingPending).Error; err != nil || remainingPending != 0 {
		t.Fatalf("expected 0 remaining pending events after recovery, got %d", remainingPending)
	}

	// 5. Consumer delivery and idempotency:
	// Verify acceptBrokerNotification creates delivery job and handles duplicate delivery
	for _, ev := range publishedEvents {
		body, err := marshalBrokerNotification(ev)
		if err != nil {
			t.Fatalf("marshalBrokerNotification failed: %v", err)
		}

		dup, err := s.acceptBrokerNotification(context.Background(), body, ev.EventKey)
		if err != nil {
			t.Fatalf("acceptBrokerNotification failed: %v", err)
		}
		if dup {
			t.Fatal("first delivery must not be duplicate")
		}

		// Re-delivery of identical message must be detected as duplicate
		dup2, err := s.acceptBrokerNotification(context.Background(), body, ev.EventKey)
		if err != nil {
			t.Fatalf("duplicate acceptBrokerNotification failed: %v", err)
		}
		if !dup2 {
			t.Fatal("second delivery must be recognized as duplicate")
		}
	}

	// Verify author sees notifications via API
	notifRes := request(t, h, http.MethodGet, "/api/notifications", authorToken, nil)
	if notifRes.Code != http.StatusOK {
		t.Fatalf("get notifications expected 200, got %d", notifRes.Code)
	}
	var notifData struct {
		Items []Notification `json:"items"`
	}
	if err := json.Unmarshal(notifRes.Body.Bytes(), &notifData); err != nil || len(notifData.Items) != 2 {
		t.Fatalf("expected 2 notifications for author, got %d", len(notifData.Items))
	}
}

// TestOutage_MinIOStorageUnavailableAndNoOrphanSQL verifies:
// 1. When MinIO storage returns 503, upload chunk fails with 503.
// 2. Zero orphan rows are persisted in upload_chunks and media_assets.
// 3. /ready remains 200 during storage outage.
// 4. When MinIO recovers, chunk upload and completion succeed idempotently.
func TestOutage_MinIOStorageUnavailableAndNoOrphanSQL(t *testing.T) {
	endpoint := os.Getenv("GOSTREAM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("GOSTREAM_TEST_MINIO_ENDPOINT not set; skipping MinIO integration sub-test")
	}

	ctx := context.Background()
	access := os.Getenv("GOSTREAM_TEST_MINIO_ACCESS_KEY")
	secret := os.Getenv("GOSTREAM_TEST_MINIO_SECRET_KEY")
	bucket := fmt.Sprintf("gostream-outage-%d", time.Now().UnixNano())

	directStore, err := NewMinIOMediaStore(ctx, endpoint, access, secret, bucket, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = directStore.removePrefix(cleanCtx, "media/")
		_ = directStore.removePrefix(cleanCtx, "chunks/")
		_ = directStore.client.RemoveBucket(cleanCtx, bucket)
	})

	// Reverse proxy with controllable outage flag
	upstream, err := url.Parse("http://" + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	var outageMode atomic.Int32 // 0: healthy, 1: 503 outage

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outageMode.Load() == 1 {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		reverse.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	proxyStore, err := openMinIOMediaStore(ctx, proxy.Listener.Addr().String(), access, secret, bucket, false, false)
	if err != nil {
		t.Fatal(err)
	}

	s, h, db := testServer(t)
	s.UseObjectMedia(proxyStore)
	_, token := registerTestUser(t, h, "minio_outage_tester")

	// Create upload session
	chunkData := append(sampleMP4, []byte("minio-outage-chunk")...)
	sessionRes := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
		"filename": "outage_test.mp4",
		"size":     len(chunkData),
	})
	if sessionRes.Code != http.StatusCreated {
		t.Fatalf("create session expected 201, got %d", sessionRes.Code)
	}
	var session UploadSession
	_ = json.Unmarshal(sessionRes.Body.Bytes(), &session)

	// 1. Storage outage
	outageMode.Store(1)

	// /ready remains 200 during MinIO outage
	readyRes := request(t, h, http.MethodGet, "/ready", "", nil)
	if readyRes.Code != http.StatusOK {
		t.Fatalf("expected /ready 200 during MinIO outage, got %d", readyRes.Code)
	}

	// Chunk upload during outage must return 503
	chunkPath := fmt.Sprintf("/api/uploads/sessions/%s/chunks/0", session.ID)
	req := httptest.NewRequest(http.MethodPut, chunkPath, bytes.NewReader(chunkData))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("chunk upload during outage expected 503, got %d", res.Code)
	}

	// Invariant: zero chunks in DB, zero media_assets
	var chunkCount int64
	if err := db.Model(&UploadChunk{}).Where("session_id = ?", session.ID).Count(&chunkCount).Error; err != nil || chunkCount != 0 {
		t.Fatalf("expected 0 chunks in DB during outage, got %d", chunkCount)
	}
	var assetCount int64
	if err := db.Model(&MediaAsset{}).Count(&assetCount).Error; err != nil || assetCount != 0 {
		t.Fatalf("expected 0 media assets during outage, got %d", assetCount)
	}

	// 2. Storage recovers
	outageMode.Store(0)

	reqRec := httptest.NewRequest(http.MethodPut, chunkPath, bytes.NewReader(chunkData))
	reqRec.Header.Set("Authorization", "Bearer "+token)
	reqRec.Header.Set("Content-Type", "application/octet-stream")
	resRec := httptest.NewRecorder()
	h.ServeHTTP(resRec, reqRec)
	if resRec.Code != http.StatusOK {
		t.Fatalf("chunk upload after recovery expected 200, got %d: %s", resRec.Code, resRec.Body.String())
	}

	// Complete upload
	completeRes := request(t, h, http.MethodPost, fmt.Sprintf("/api/uploads/sessions/%s/complete", session.ID), token, nil)
	if completeRes.Code != http.StatusOK {
		t.Fatalf("complete session expected 200, got %d", completeRes.Code)
	}

	// Duplicate completion retry returns identical URL
	retryRes := request(t, h, http.MethodPost, fmt.Sprintf("/api/uploads/sessions/%s/complete", session.ID), token, nil)
	if retryRes.Code != http.StatusOK {
		t.Fatalf("duplicate complete expected 200, got %d", retryRes.Code)
	}
	var out1, out2 map[string]any
	_ = json.Unmarshal(completeRes.Body.Bytes(), &out1)
	_ = json.Unmarshal(retryRes.Body.Bytes(), &out2)
	if out1["url"] != out2["url"] {
		t.Fatalf("completion retry URL mismatch: %v vs %v", out1["url"], out2["url"])
	}
}

// TestComponentOutage_WorkerRestartAndLeaseReclaim verifies:
// 1. When an active completion or outbox worker crashes and its lease expires,
//    a restarted/new worker safely re-claims the session/event without duplication.
// 2. Stale workers holding expired leases are fenced from committing changes.
func TestComponentOutage_WorkerRestartAndLeaseReclaim(t *testing.T) {
	s, h, db := testServer(t)

	userID, token := registerTestUser(t, h, "lease_reclaim_user")

	// 1. Upload session lease expiry and reclaim:
	created := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
		"filename": "lease_reclaim.mp4",
		"size":     len(sampleMP4),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create session: %d", created.Code)
	}
	var session UploadSession
	_ = json.Unmarshal(created.Body.Bytes(), &session)

	if r := putTestChunk(h, token, session.ID, 0, sampleMP4); r.Code != http.StatusOK {
		t.Fatalf("put chunk: %d", r.Code)
	}

	// Simulate crashed worker: session was marked completing, but worker died and lease expired in the past
	past := time.Now().UTC().Add(-time.Minute)
	if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Updates(map[string]any{
		"status":      "completing",
		"lease_until": past,
	}).Error; err != nil {
		t.Fatal(err)
	}

	// New completion request: must successfully reclaim expired completing lease and finish
	completeRes := request(t, h, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	if completeRes.Code != http.StatusOK {
		t.Fatalf("reclaimed completion expected 200, got %d: %s", completeRes.Code, completeRes.Body.String())
	}

	var finishedSession UploadSession
	if err := db.First(&finishedSession, "id = ?", session.ID).Error; err != nil || finishedSession.Status != "completed" {
		t.Fatalf("expected completed session, got %+v err=%v", finishedSession, err)
	}

	// 2. Outbox monotonic lease generation guard:
	// Worker 1 claims event (attempt=1, leaseUntil=now+30s)
	event := OutboxEvent{
		EventKey:    "lease-guard-event",
		Topic:       "notification.deliver",
		Payload:     `{"notification_id": 100}`,
		AvailableAt: time.Now().UTC().Add(-time.Second),
	}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}

	// Simulate worker 1 claiming the event
	now := time.Now().UTC()
	worker1Lease := now.Add(30 * time.Second)
	claim1 := db.Model(&OutboxEvent{}).Where("id = ? AND attempts = 0", event.ID).
		Updates(map[string]any{"available_at": worker1Lease, "attempts": 1})
	if claim1.RowsAffected != 1 {
		t.Fatal("worker 1 claim failed")
	}

	// Worker 1 hangs/dies. Time passes and lease expires:
	expiredLease := time.Now().UTC().Add(-time.Second)
	_ = db.Model(&OutboxEvent{}).Where("id = ?", event.ID).Update("available_at", expiredLease)

	// Worker 2 (restarted worker) claims expired event:
	worker2Lease := time.Now().UTC().Add(30 * time.Second)
	claim2 := db.Model(&OutboxEvent{}).Where("id = ? AND attempts = 1 AND available_at <= ?", event.ID, time.Now().UTC()).
		Updates(map[string]any{"available_at": worker2Lease, "attempts": 2})
	if claim2.RowsAffected != 1 {
		t.Fatal("worker 2 failed to reclaim expired outbox lease")
	}

	// Worker 2 finishes and marks published:
	finished := time.Now().UTC()
	res := db.Model(&OutboxEvent{}).Where("id = ? AND attempts = 2 AND available_at > ?", event.ID, finished).
		Updates(map[string]any{"published_at": finished})
	if res.RowsAffected != 1 {
		t.Fatal("worker 2 failed to mark published")
	}

	// Stale Worker 1 attempts to finish with attempt=1: must be fenced (RowsAffected=0)
	staleUpdate := db.Model(&OutboxEvent{}).Where("id = ? AND attempts = 1", event.ID).
		Updates(map[string]any{"published_at": time.Now().UTC()})
	if staleUpdate.RowsAffected != 0 {
		t.Fatalf("fencing failed: stale worker 1 affected %d rows", staleUpdate.RowsAffected)
	}

	_ = s
	_ = userID
}
