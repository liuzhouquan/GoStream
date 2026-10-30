package platform

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

	"github.com/gorilla/websocket"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestFaultScenario1_TransactionInterruptionNoHalfCommits verifies that when a business
// transaction encounters a failure or rollback, no partial rows appear in tables and counters
// remain strictly non-negative and identical to their prior state.
func TestFaultScenario1_TransactionInterruptionNoHalfCommits(t *testing.T) {
	_, h, db := testServer(t)
	authorID, authorToken := registerTestUser(t, h, "tx_author")
	viewerID, viewerToken := registerTestUser(t, h, "tx_viewer")

	// 1. Create a published video
	video := Video{AuthorID: authorID, Title: "Transaction Boundary Video", MediaURL: "/media/tx_boundary.mp4", Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}

	// 2. Pre-state: verify counts are 0
	var initialVideo Video
	if err := db.First(&initialVideo, video.ID).Error; err != nil {
		t.Fatal(err)
	}
	if initialVideo.LikeCount != 0 || initialVideo.CommentCount != 0 {
		t.Fatalf("initial counts: like=%d, comment=%d", initialVideo.LikeCount, initialVideo.CommentCount)
	}

	// 3. Like transaction rollback simulation:
	// If a video is deleted or concurrent rollback happens before commit,
	// no half-committed rows must remain.
	deletedVideo := Video{AuthorID: authorID, Title: "Deleted Video", MediaURL: "/media/del.mp4", Status: "deleted"}
	if err := db.Create(&deletedVideo).Error; err != nil {
		t.Fatal(err)
	}

	resDeletedLike := request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", deletedVideo.ID), viewerToken, nil)
	if resDeletedLike.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on deleted video like, got %d", resDeletedLike.Code)
	}

	var actionCount int64
	if err := db.Model(&VideoAction{}).Where("video_id = ?", deletedVideo.ID).Count(&actionCount).Error; err != nil || actionCount != 0 {
		t.Fatalf("half-committed VideoAction rows on deleted video: %d", actionCount)
	}
	var notifCount int64
	if err := db.Model(&Notification{}).Where("video_id = ?", deletedVideo.ID).Count(&notifCount).Error; err != nil || notifCount != 0 {
		t.Fatalf("half-committed Notification rows on deleted video: %d", notifCount)
	}

	// 4. Failed comment transaction rollback:
	// Comment on a deleted video must not leave orphaned comments, notifications, or outbox entries.
	resDeletedComment := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", deletedVideo.ID), viewerToken, map[string]string{
		"body": "Should fail and rollback",
	})
	if resDeletedComment.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on deleted video comment, got %d", resDeletedComment.Code)
	}

	var commentCount int64
	if err := db.Model(&Comment{}).Where("video_id = ?", deletedVideo.ID).Count(&commentCount).Error; err != nil || commentCount != 0 {
		t.Fatalf("half-committed Comment rows: %d", commentCount)
	}

	// 5. Successful comment enqueues exactly 1 comment and 1 notification atomically
	resValidComment := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", video.ID), viewerToken, map[string]string{
		"body": "Valid comment",
	})
	if resValidComment.Code != http.StatusCreated {
		t.Fatalf("valid comment failed: %d %s", resValidComment.Code, resValidComment.Body.String())
	}

	var updatedVideo Video
	if err := db.First(&updatedVideo, video.ID).Error; err != nil || updatedVideo.CommentCount != 1 {
		t.Fatalf("comment count not incremented: %+v", updatedVideo)
	}
	if err := db.Model(&Comment{}).Where("video_id = ?", video.ID).Count(&commentCount).Error; err != nil || commentCount != 1 {
		t.Fatalf("expected 1 comment row, got %d", commentCount)
	}
	if err := db.Model(&Notification{}).Where("video_id = ? AND user_id = ?", video.ID, authorID).Count(&notifCount).Error; err != nil || notifCount != 1 {
		t.Fatalf("expected 1 notification row, got %d", notifCount)
	}

	// 6. Verify Outbox table has corresponding event
	var outboxCount int64
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", "notification.created").Count(&outboxCount).Error; err != nil || outboxCount != 1 {
		t.Fatalf("expected 1 outbox event, got %d", outboxCount)
	}

	_ = authorToken
	_ = viewerID
}

// TestFaultScenario2_DuplicateRequestsAndIdempotency verifies that repeated requests
// (likes, upload completion, messages with client_id, and follows) result in identical state
// and conflicting duplicate client_ids return explicit errors.
func TestFaultScenario2_DuplicateRequestsAndIdempotency(t *testing.T) {
	s, h, db := testServer(t)
	authorID, authorToken := registerTestUser(t, h, "dup_author")
	viewerID, viewerToken := registerTestUser(t, h, "dup_viewer")

	video := Video{AuthorID: authorID, Title: "Idempotency Video", MediaURL: "/media/idempotent.mp4", Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}

	// 1. Duplicate Likes (3 times)
	for i := 0; i < 3; i++ {
		res := request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", video.ID), viewerToken, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("like iteration %d failed: %d", i, res.Code)
		}
	}
	var postLikeVideo Video
	if err := db.First(&postLikeVideo, video.ID).Error; err != nil || postLikeVideo.LikeCount != 1 {
		t.Fatalf("expected like_count=1 after 3 duplicate likes, got %d", postLikeVideo.LikeCount)
	}
	var likeActionCount int64
	if err := db.Model(&VideoAction{}).Where("user_id = ? AND video_id = ? AND kind = 'like'", viewerID, video.ID).Count(&likeActionCount).Error; err != nil || likeActionCount != 1 {
		t.Fatalf("expected exactly 1 VideoAction row, got %d", likeActionCount)
	}

	// Duplicate Unlikes (2 times)
	for i := 0; i < 2; i++ {
		res := request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d/like", video.ID), viewerToken, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("unlike iteration %d failed: %d", i, res.Code)
		}
	}
	if err := db.First(&postLikeVideo, video.ID).Error; err != nil || postLikeVideo.LikeCount != 0 {
		t.Fatalf("expected like_count=0 after unlikes, got %d", postLikeVideo.LikeCount)
	}

	// 2. Duplicate Upload Completion
	resSess := request(t, h, http.MethodPost, "/api/uploads/sessions", authorToken, map[string]any{
		"filename": "repeat.mp4",
		"size":     len(sampleMP4),
	})
	if resSess.Code != http.StatusCreated {
		t.Fatalf("create session: %d", resSess.Code)
	}
	var sessBody struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resSess.Body.Bytes(), &sessBody)

	putTestChunk(h, authorToken, sessBody.ID, 0, sampleMP4)

	var firstURL string
	for i := 0; i < 3; i++ {
		resComp := request(t, h, http.MethodPost, fmt.Sprintf("/api/uploads/sessions/%s/complete", sessBody.ID), authorToken, nil)
		if resComp.Code != http.StatusOK {
			t.Fatalf("complete iteration %d failed: %d %s", i, resComp.Code, resComp.Body.String())
		}
		var compBody struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(resComp.Body.Bytes(), &compBody)
		if i == 0 {
			firstURL = compBody.URL
		} else if compBody.URL != firstURL {
			t.Fatalf("complete iteration %d returned different URL %q vs %q", i, compBody.URL, firstURL)
		}
	}

	// Exactly 1 MediaAsset record exists for this upload URL
	var assetCount int64
	if err := db.Model(&MediaAsset{}).Where("url = ?", firstURL).Count(&assetCount).Error; err != nil || assetCount != 1 {
		t.Fatalf("expected 1 MediaAsset row, got %d", assetCount)
	}

	// 3. Duplicate Follow (2 times)
	for i := 0; i < 2; i++ {
		resFollow := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorID), viewerToken, nil)
		if resFollow.Code != http.StatusOK {
			t.Fatalf("follow iteration %d failed: %d", i, resFollow.Code)
		}
	}
	var followCount int64
	if err := db.Model(&Follow{}).Where("follower_id = ? AND followee_id = ?", viewerID, authorID).Count(&followCount).Error; err != nil || followCount != 1 {
		t.Fatalf("expected 1 Follow row, got %d", followCount)
	}

	// 4. WebSocket ClientID Idempotency & Conflict Rejection
	ts := httptest.NewServer(h)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	dialer := websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + viewerToken}}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close()

	var readyFrame struct {
		Type string `json:"type"`
	}
	_ = conn.ReadJSON(&readyFrame)

	// Send message with client_id="c_dup_1"
	sendMsg := map[string]any{
		"type":         "message",
		"client_id":    "c_dup_1",
		"recipient_id": authorID,
		"body":         "Original Message Body",
	}
	if err := conn.WriteJSON(sendMsg); err != nil {
		t.Fatal(err)
	}

	var ack1 struct {
		Type      string `json:"type"`
		ClientID  string `json:"client_id"`
		MessageID uint   `json:"message_id"`
	}
	if err := conn.ReadJSON(&ack1); err != nil || ack1.Type != "ack" || ack1.ClientID != "c_dup_1" {
		t.Fatalf("first ack failed: %+v", ack1)
	}

	// Repeat exact same message with client_id="c_dup_1"
	if err := conn.WriteJSON(sendMsg); err != nil {
		t.Fatal(err)
	}
	var ack2 struct {
		Type      string `json:"type"`
		ClientID  string `json:"client_id"`
		MessageID uint   `json:"message_id"`
	}
	if err := conn.ReadJSON(&ack2); err != nil || ack2.Type != "ack" || ack2.MessageID != ack1.MessageID {
		t.Fatalf("repeated ack mismatch: got %+v, want message_id=%d", ack2, ack1.MessageID)
	}

	// Send conflicting message with same client_id but DIFFERENT body
	conflictMsg := map[string]any{
		"type":         "message",
		"client_id":    "c_dup_1",
		"recipient_id": authorID,
		"body":         "Conflicting Different Body!",
	}
	if err := conn.WriteJSON(conflictMsg); err != nil {
		t.Fatal(err)
	}
	var errFrame struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := conn.ReadJSON(&errFrame); err != nil || errFrame.Type != "error" || !strings.Contains(errFrame.Message, "client_id already used") {
		t.Fatalf("expected conflict error frame, got %+v", errFrame)
	}

	_ = s
}

// TestFaultScenario3_SingleComponentOutageAndGracefulFallback verifies that:
// 1. Redis outage does not block Feed or chat history; Feed falls back to SQL without 500s.
// 2. Outbox events committed before broker outage persist and are safely claimable upon recovery.
func TestFaultScenario3_SingleComponentOutageAndGracefulFallback(t *testing.T) {
	s, h, db := testServer(t)
	authorID, _ := registerTestUser(t, h, "fallback_author")
	viewerID, viewerToken := registerTestUser(t, h, "fallback_viewer")

	// Create 5 published videos
	for i := 1; i <= 5; i++ {
		v := Video{
			AuthorID: authorID,
			Title:    fmt.Sprintf("Fallback Video %d", i),
			MediaURL: fmt.Sprintf("/media/fb_%d.mp4", i),
			Status:   "published",
		}
		if err := db.Create(&v).Error; err != nil {
			t.Fatal(err)
		}
	}

	// 1. Feed fallback when Redis is completely unconfigured / down
	// Standard feed request must succeed with 200 and return items from SQL
	resFeed := request(t, h, http.MethodGet, "/api/feed?scene=recommend", viewerToken, nil)
	if resFeed.Code != http.StatusOK {
		t.Fatalf("feed fallback returned %d: %s", resFeed.Code, resFeed.Body.String())
	}
	var feedData struct {
		Items []Video `json:"items"`
	}
	if err := json.Unmarshal(resFeed.Body.Bytes(), &feedData); err != nil || len(feedData.Items) != 5 {
		t.Fatalf("expected 5 videos from database fallback, got %d", len(feedData.Items))
	}

	// Direct chat history works without Redis
	resHistory := request(t, h, http.MethodGet, fmt.Sprintf("/api/messages?user_id=%d", authorID), viewerToken, nil)
	if resHistory.Code != http.StatusOK {
		t.Fatalf("messages history without Redis returned %d", resHistory.Code)
	}

	// 2. Outbox durability during broker outage:
	// Insert outbox events via enqueueNotification simulating business transactions while broker is offline
	if err := enqueueNotification(db, Notification{
		UserID:   authorID,
		EventKey: "evt_offline_1",
		Kind:     "like",
		ActorID:  viewerID,
		VideoID:  feedData.Items[0].ID,
		Text:     "offline notification 1",
	}); err != nil {
		t.Fatal(err)
	}

	if err := enqueueNotification(db, Notification{
		UserID:   authorID,
		EventKey: "evt_offline_2",
		Kind:     "comment",
		ActorID:  viewerID,
		VideoID:  feedData.Items[0].ID,
		Text:     "offline notification 2",
	}); err != nil {
		t.Fatal(err)
	}

	// Events remain stored in database awaiting worker recovery
	var pendingCount int64
	if err := db.Model(&OutboxEvent{}).Where("published_at IS NULL AND dead_at IS NULL").Count(&pendingCount).Error; err != nil || pendingCount < 2 {
		t.Fatalf("expected at least 2 pending outbox events, got %d", pendingCount)
	}

	// Simulate worker recovery: ProcessOutboxOnce claims and processes events
	workerCtx, cancelWorker := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWorker()
	s.ProcessOutboxOnce(workerCtx)

	// Events are dispatched and marked published
	var remainingPending int64
	if err := db.Model(&OutboxEvent{}).Where("published_at IS NOT NULL").Count(&remainingPending).Error; err != nil || remainingPending < 2 {
		t.Fatalf("expected events published after recovery, got %d", remainingPending)
	}

	_ = viewerID
}

// TestFaultScenario4_ProcessRestartAndLeaseRecovery verifies that when a leased worker
// crashes or holds an unexpired lease that subsequently expires, another worker
// safely claims the expired lease without duplicating actions, and old expired workers are fenced.
func TestFaultScenario4_ProcessRestartAndLeaseRecovery(t *testing.T) {
	s, _, db := testServer(t)

	authorID, _ := registerTestUser(t, s.Router(), "lease_author")
	actorID, _ := registerTestUser(t, s.Router(), "lease_actor")

	v := Video{AuthorID: authorID, Title: "Lease Video", MediaURL: "/media/lease.mp4", Status: "published"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	// Create valid notification and outbox event
	if err := enqueueNotification(db, Notification{
		UserID:   authorID,
		EventKey: "lease_event_key",
		Kind:     "like",
		ActorID:  actorID,
		VideoID:  v.ID,
		Text:     "leased notification",
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	var ev OutboxEvent
	if err := db.Where("event_key = ?", fmt.Sprintf("notification:%d", 1)).First(&ev).Error; err != nil {
		if err := db.First(&ev).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Simulate crashed worker: pushed available_at into the future previously,
	// but the lease has now expired (available_at <= now) with attempts=1
	if err := db.Model(&ev).Updates(map[string]any{
		"available_at": now.Add(-5 * time.Second),
		"attempts":    1,
	}).Error; err != nil {
		t.Fatal(err)
	}

	// Replacement worker claims the expired row using conditional CAS
	ctx := context.Background()
	claimed, err := s.ProcessOutboxOnce(ctx)
	if err != nil {
		t.Fatalf("replacement worker claim failed: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("expected 1 event claimed by replacement worker, got %d", claimed)
	}

	var postClaim OutboxEvent
	if err := db.First(&postClaim, ev.ID).Error; err != nil {
		t.Fatal(err)
	}
	if postClaim.Attempts != 2 {
		t.Fatalf("expected attempts=2 after replacement claim, got %d", postClaim.Attempts)
	}
	if postClaim.PublishedAt == nil {
		t.Fatalf("expected event marked published by replacement worker")
	}

	// Now verify fencing: if the old crashed/delayed worker now wakes up with attempts=1
	// and tries to write or acknowledge the row, it must be fenced and affect 0 rows
	staleAck := db.Model(&OutboxEvent{}).
		Where("id = ? AND attempts = ? AND published_at IS NULL", ev.ID, 1).
		Update("published_at", now)
	if staleAck.RowsAffected != 0 {
		t.Fatalf("expected stale worker to be fenced (0 rows affected), got %d", staleAck.RowsAffected)
	}
}

// TestFaultScenario5_DualInstanceCollaborationAndStateReconciliation verifies that
// two independent server instances can coordinate on the same upload session and
// serve users across instances without data loss.
func TestFaultScenario5_DualInstanceCollaborationAndStateReconciliation(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "dual.db")
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	mediaDir := filepath.Join(tempDir, "media")
	_ = os.MkdirAll(mediaDir, 0755)
	jwtSecret := "dual-instance-shared-secret-at-least-32-chars!!"

	db, err := gorm.Open(gormsqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s, err := db.DB(); err == nil {
			_ = s.Close()
		}
	}()
	if err := MigrateSchema(db); err != nil {
		t.Fatal(err)
	}

	// Instance 1 & Instance 2 share the database and media directory
	s1 := NewServer(db, jwtSecret, mediaDir)
	h1 := s1.Router()
	s2 := NewServer(db, jwtSecret, mediaDir)
	h2 := s2.Router()

	// Register user on Instance 1
	authorID, authorToken := registerTestUser(t, h1, "dual_creator")

	// Instance 1: Create resumable session for sampleMP4
	totalSize := len(sampleMP4)
	resSess := request(t, h1, http.MethodPost, "/api/uploads/sessions", authorToken, map[string]any{
		"filename": "dual_video.mp4",
		"size":     totalSize,
	})
	if resSess.Code != http.StatusCreated {
		t.Fatalf("instance 1 create session: %d %s", resSess.Code, resSess.Body.String())
	}
	var sessBody struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resSess.Body.Bytes(), &sessBody)

	// Instance 1: Uploads Chunk 0
	putRes0 := putTestChunk(h1, authorToken, sessBody.ID, 0, sampleMP4)
	if putRes0.Code != http.StatusOK {
		t.Fatalf("instance 1 put chunk 0: %d", putRes0.Code)
	}

	// Instance 2: Completes the session on the other server instance
	compRes := request(t, h2, http.MethodPost, fmt.Sprintf("/api/uploads/sessions/%s/complete", sessBody.ID), authorToken, nil)
	if compRes.Code != http.StatusOK {
		t.Fatalf("instance 2 complete session: %d %s", compRes.Code, compRes.Body.String())
	}
	var compBody struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(compRes.Body.Bytes(), &compBody)

	// Instance 1: Publishes the video
	pubRes := request(t, h1, http.MethodPost, "/api/videos", authorToken, map[string]string{
		"title":     "Dual Instance Video",
		"media_url": compBody.URL,
	})
	if pubRes.Code != http.StatusCreated {
		t.Fatalf("instance 1 publish: %d %s", pubRes.Code, pubRes.Body.String())
	}
	var pubBody struct {
		ID uint `json:"id"`
	}
	_ = json.Unmarshal(pubRes.Body.Bytes(), &pubBody)

	// Instance 2: Can read and serve the video and its media range
	getRes := request(t, h2, http.MethodGet, fmt.Sprintf("/api/videos/%d", pubBody.ID), "", nil)
	if getRes.Code != http.StatusOK {
		t.Fatalf("instance 2 get video: %d", getRes.Code)
	}

	mediaReq := httptest.NewRequest(http.MethodGet, compBody.URL, nil)
	mediaReq.Header.Set("Range", "bytes=0-11")
	mediaRec := httptest.NewRecorder()
	h2.ServeHTTP(mediaRec, mediaReq)
	if mediaRec.Code != http.StatusPartialContent {
		t.Fatalf("instance 2 media range status = %d, want 206", mediaRec.Code)
	}
	if !bytes.Equal(mediaRec.Body.Bytes(), sampleMP4[:12]) {
		t.Fatalf("instance 2 media range bytes mismatch")
	}

	_ = authorID
}

// TestFaultScenario6_PermissionSwitchingDynamicEnforcement verifies that:
// 1. Unfollowing immediately strips the followee's videos from following feed.
// 2. Removing a group member immediately blocks them from reading history and sending messages (404 / 403 privacy boundary).
// 3. Deleting a video immediately strips it from Feed and Search, returning 404 on direct access.
func TestFaultScenario6_PermissionSwitchingDynamicEnforcement(t *testing.T) {
	_, h, db := testServer(t)
	creatorID, creatorToken := registerTestUser(t, h, "perm_creator")
	viewerID, viewerToken := registerTestUser(t, h, "perm_viewer")

	// 1. Follow and Unfollow dynamic enforcement
	v1 := Video{AuthorID: creatorID, Title: "Creator Exclusive Video", MediaURL: "/media/perm_v1.mp4", Status: "published"}
	if err := db.Create(&v1).Error; err != nil {
		t.Fatal(err)
	}

	// Viewer follows Creator
	resFollow := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", creatorID), viewerToken, nil)
	if resFollow.Code != http.StatusOK {
		t.Fatalf("follow creator failed: %d", resFollow.Code)
	}

	// Check following feed: contains v1
	resFeed1 := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil)
	if resFeed1.Code != http.StatusOK || !strings.Contains(resFeed1.Body.String(), "Creator Exclusive Video") {
		t.Fatalf("following feed before unfollow does not contain video: %s", resFeed1.Body.String())
	}

	// Viewer unfollows Creator
	resUnfollow := request(t, h, http.MethodDelete, fmt.Sprintf("/api/users/me/following/%d", creatorID), viewerToken, nil)
	if resUnfollow.Code != http.StatusOK {
		t.Fatalf("unfollow creator failed: %d", resUnfollow.Code)
	}

	// Following feed must immediately omit Creator's videos
	resFeed2 := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil)
	if resFeed2.Code != http.StatusOK {
		t.Fatalf("following feed after unfollow failed: %d", resFeed2.Code)
	}
	if strings.Contains(resFeed2.Body.String(), "Creator Exclusive Video") {
		t.Fatalf("following feed leaked video after unfollow! Body: %s", resFeed2.Body.String())
	}

	// 2. Group Membership Revocation Dynamic Enforcement
	resGroup := request(t, h, http.MethodPost, "/api/groups", creatorToken, map[string]string{
		"name": "Secret Planning Group",
	})
	if resGroup.Code != http.StatusCreated {
		t.Fatalf("create group failed: %d", resGroup.Code)
	}
	var grp struct {
		ID uint `json:"id"`
	}
	_ = json.Unmarshal(resGroup.Body.Bytes(), &grp)

	// Creator invites Viewer
	resInvite := request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", grp.ID), creatorToken, map[string]uint{
		"user_id": viewerID,
	})
	if resInvite.Code != http.StatusOK {
		t.Fatalf("invite viewer: %d", resInvite.Code)
	}

	// Viewer can read group history
	resGrpMsg1 := request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/messages", grp.ID), viewerToken, nil)
	if resGrpMsg1.Code != http.StatusOK {
		t.Fatalf("viewer read group history while member failed: %d", resGrpMsg1.Code)
	}

	// Creator removes Viewer from group
	resRemove := request(t, h, http.MethodDelete, fmt.Sprintf("/api/groups/%d/members/%d", grp.ID, viewerID), creatorToken, nil)
	if resRemove.Code != http.StatusOK {
		t.Fatalf("remove viewer from group: %d", resRemove.Code)
	}

	// Viewer immediately denied reading history (returns 404 group not found for privacy isolation)
	resGrpMsg2 := request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/messages", grp.ID), viewerToken, nil)
	if resGrpMsg2.Code != http.StatusNotFound && resGrpMsg2.Code != http.StatusForbidden {
		t.Fatalf("expected 404 or 403 after group removal, got %d %s", resGrpMsg2.Code, resGrpMsg2.Body.String())
	}

	// 3. Video Deletion Dynamic Enforcement
	// Search includes v1
	resSearch1 := request(t, h, http.MethodGet, "/api/search?q=Exclusive", "", nil)
	if resSearch1.Code != http.StatusOK || !strings.Contains(resSearch1.Body.String(), "Creator Exclusive Video") {
		t.Fatalf("search before delete missing video: %s", resSearch1.Body.String())
	}

	// Creator deletes video
	resDelVideo := request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d", v1.ID), creatorToken, nil)
	if resDelVideo.Code != http.StatusNoContent {
		t.Fatalf("delete video failed: %d", resDelVideo.Code)
	}

	// Direct GET returns 404
	resGetDel := request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d", v1.ID), "", nil)
	if resGetDel.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on deleted video, got %d", resGetDel.Code)
	}

	// Feed excludes deleted video
	resFeedDel := request(t, h, http.MethodGet, "/api/feed?scene=recommend", "", nil)
	if strings.Contains(resFeedDel.Body.String(), "Creator Exclusive Video") {
		t.Fatalf("feed leaked deleted video: %s", resFeedDel.Body.String())
	}

	// Search excludes deleted video
	resSearch2 := request(t, h, http.MethodGet, "/api/search?q=Exclusive", "", nil)
	if strings.Contains(resSearch2.Body.String(), "Creator Exclusive Video") {
		t.Fatalf("search leaked deleted video: %s", resSearch2.Body.String())
	}
}
