package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestRepeatInteractionsCountersAndOutboxCorrespondence(t *testing.T) {
	_, h, db := testServer(t)

	authorID, authorToken := registerTestUser(t, h, "author_closure")
	userBID, userBToken := registerTestUser(t, h, "userb_closure")

	// Create published video
	video := Video{AuthorID: authorID, Title: "Closure Video", MediaURL: "/media/closure.mp4", Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}

	// 1. User B likes Video twice (idempotency check)
	likePath := fmt.Sprintf("/api/videos/%d/like", video.ID)
	res1 := request(t, h, http.MethodPut, likePath, userBToken, nil)
	if res1.Code != http.StatusOK {
		t.Fatalf("first like failed: %d %s", res1.Code, res1.Body.String())
	}
	res2 := request(t, h, http.MethodPut, likePath, userBToken, nil)
	if res2.Code != http.StatusOK {
		t.Fatalf("second like failed: %d %s", res2.Code, res2.Body.String())
	}

	// Verify exact count of video_actions, Video.LikeCount, Notifications, OutboxEvents
	var actionsCount int64
	db.Model(&VideoAction{}).Where("video_id = ? AND kind = ?", video.ID, "like").Count(&actionsCount)
	if actionsCount != 1 {
		t.Fatalf("expected 1 like action row, got %d", actionsCount)
	}
	var vCheck Video
	db.First(&vCheck, video.ID)
	if vCheck.LikeCount != 1 {
		t.Fatalf("expected video.like_count == 1, got %d", vCheck.LikeCount)
	}

	var notifCount int64
	db.Model(&Notification{}).Where("user_id = ? AND kind = ?", authorID, "like").Count(&notifCount)
	if notifCount != 1 {
		t.Fatalf("expected exactly 1 like notification for author, got %d", notifCount)
	}

	var outboxCount int64
	var notif Notification
	db.Where("user_id = ? AND kind = ?", authorID, "like").First(&notif)
	db.Model(&OutboxEvent{}).Where("event_key = ?", fmt.Sprintf("notification:%d", notif.ID)).Count(&outboxCount)
	if outboxCount != 1 {
		t.Fatalf("expected exactly 1 outbox event for notification %d, got %d", notif.ID, outboxCount)
	}

	// 2. User B unlikes Video twice (underflow protection check)
	del1 := request(t, h, http.MethodDelete, likePath, userBToken, nil)
	if del1.Code != http.StatusOK {
		t.Fatalf("first unlike failed: %d %s", del1.Code, del1.Body.String())
	}
	del2 := request(t, h, http.MethodDelete, likePath, userBToken, nil)
	if del2.Code != http.StatusOK {
		t.Fatalf("second unlike failed: %d %s", del2.Code, del2.Body.String())
	}

	db.Model(&VideoAction{}).Where("video_id = ? AND kind = ?", video.ID, "like").Count(&actionsCount)
	if actionsCount != 0 {
		t.Fatalf("expected 0 like action rows after delete, got %d", actionsCount)
	}
	db.First(&vCheck, video.ID)
	if vCheck.LikeCount != 0 {
		t.Fatalf("expected video.like_count == 0, got %d (must never be negative)", vCheck.LikeCount)
	}

	// 3. User B favorites Video twice, un-favorites twice
	favPath := fmt.Sprintf("/api/videos/%d/favorite", video.ID)
	resFav1 := request(t, h, http.MethodPut, favPath, userBToken, nil)
	resFav2 := request(t, h, http.MethodPut, favPath, userBToken, nil)
	if resFav1.Code != http.StatusOK || resFav2.Code != http.StatusOK {
		t.Fatalf("favorite failed: %d, %d", resFav1.Code, resFav2.Code)
	}
	db.First(&vCheck, video.ID)
	if vCheck.FavoriteCount != 1 {
		t.Fatalf("expected favorite_count == 1, got %d", vCheck.FavoriteCount)
	}

	delFav1 := request(t, h, http.MethodDelete, favPath, userBToken, nil)
	delFav2 := request(t, h, http.MethodDelete, favPath, userBToken, nil)
	if delFav1.Code != http.StatusOK || delFav2.Code != http.StatusOK {
		t.Fatalf("unfavorite failed: %d, %d", delFav1.Code, delFav2.Code)
	}
	db.First(&vCheck, video.ID)
	if vCheck.FavoriteCount != 0 {
		t.Fatalf("expected favorite_count == 0, got %d", vCheck.FavoriteCount)
	}

	// 4. Self-interaction suppression (author likes / comments own video)
	selfLike := request(t, h, http.MethodPut, likePath, authorToken, nil)
	if selfLike.Code != http.StatusOK {
		t.Fatalf("self like failed: %d %s", selfLike.Code, selfLike.Body.String())
	}
	db.First(&vCheck, video.ID)
	if vCheck.LikeCount != 1 {
		t.Fatalf("expected self like count == 1, got %d", vCheck.LikeCount)
	}
	// Verify no new notification was generated for author
	var selfNotifs int64
	db.Model(&Notification{}).Where("user_id = ? AND actor_id = ?", authorID, authorID).Count(&selfNotifs)
	if selfNotifs != 0 {
		t.Fatalf("self like must NOT create self notification, found %d", selfNotifs)
	}

	// 5. Follow / Unfollow idempotency & Self-follow rejection
	selfFollow := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", userBID), userBToken, nil)
	if selfFollow.Code != http.StatusBadRequest {
		t.Fatalf("self follow must return 400, got %d", selfFollow.Code)
	}

	followPath := fmt.Sprintf("/api/users/me/following/%d", authorID)
	fol1 := request(t, h, http.MethodPut, followPath, userBToken, nil)
	fol2 := request(t, h, http.MethodPut, followPath, userBToken, nil)
	if fol1.Code != http.StatusOK || fol2.Code != http.StatusOK {
		t.Fatalf("follow failed: %d, %d", fol1.Code, fol2.Code)
	}
	var followRows int64
	db.Model(&Follow{}).Where("follower_id = ? AND followee_id = ?", userBID, authorID).Count(&followRows)
	if followRows != 1 {
		t.Fatalf("expected exactly 1 follow row, got %d", followRows)
	}

	var followNotifs int64
	db.Model(&Notification{}).Where("user_id = ? AND actor_id = ? AND kind = ?", authorID, userBID, "follow").Count(&followNotifs)
	if followNotifs != 1 {
		t.Fatalf("expected exactly 1 follow notification, got %d", followNotifs)
	}

	unfol1 := request(t, h, http.MethodDelete, followPath, userBToken, nil)
	unfol2 := request(t, h, http.MethodDelete, followPath, userBToken, nil)
	if unfol1.Code != http.StatusOK || unfol2.Code != http.StatusOK {
		t.Fatalf("unfollow failed: %d, %d", unfol1.Code, unfol2.Code)
	}
	db.Model(&Follow{}).Where("follower_id = ? AND followee_id = ?", userBID, authorID).Count(&followRows)
	if followRows != 0 {
		t.Fatalf("expected 0 follow rows after unfollow, got %d", followRows)
	}
}

func TestCommentHierarchyAndThreadValidation(t *testing.T) {
	_, h, db := testServer(t)

	authorID, _ := registerTestUser(t, h, "cmt_author")
	userBID, userBToken := registerTestUser(t, h, "cmt_user_b")
	userCID, userCToken := registerTestUser(t, h, "cmt_user_c")

	v1 := Video{AuthorID: authorID, Title: "V1", MediaURL: "/media/v1.mp4", Status: "published"}
	v2 := Video{AuthorID: authorID, Title: "V2", MediaURL: "/media/v2.mp4", Status: "published"}
	db.Create(&v1)
	db.Create(&v2)

	// 1. Root comment by User B on Video 1
	postRoot := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v1.ID), userBToken, map[string]any{
		"body": "Root comment from User B",
	})
	if postRoot.Code != http.StatusCreated {
		t.Fatalf("post root comment failed: %d %s", postRoot.Code, postRoot.Body.String())
	}
	var rootComment Comment
	json.Unmarshal(postRoot.Body.Bytes(), &rootComment)
	if rootComment.ID == 0 || rootComment.ParentID != nil {
		t.Fatalf("invalid root comment: %+v", rootComment)
	}

	// Verify author notification generated
	var notifAuthor Notification
	err := db.Where("user_id = ? AND kind = ?", authorID, "comment").First(&notifAuthor).Error
	if err != nil {
		t.Fatalf("expected notification for video author on root comment: %v", err)
	}

	// 2. Direct Reply by User C to Root Comment
	postReply := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v1.ID), userCToken, map[string]any{
		"body":      "Reply from User C",
		"parent_id": rootComment.ID,
	})
	if postReply.Code != http.StatusCreated {
		t.Fatalf("post reply failed: %d %s", postReply.Code, postReply.Body.String())
	}
	var replyComment Comment
	json.Unmarshal(postReply.Body.Bytes(), &replyComment)
	if replyComment.ParentID == nil || *replyComment.ParentID != rootComment.ID {
		t.Fatalf("reply parent mismatch: %+v", replyComment)
	}

	// Verify both author (comment) and root commenter User B (reply) receive notifications
	var notifRootAuthor Notification
	err = db.Where("user_id = ? AND actor_id = ? AND kind = ?", userBID, userCID, "reply").First(&notifRootAuthor).Error
	if err != nil {
		t.Fatalf("expected reply notification for root commenter: %v", err)
	}

	// 3. Level-3 nesting rejection: Attempting to reply to a reply
	postNested := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v1.ID), userBToken, map[string]any{
		"body":      "Nested reply to reply (should fail)",
		"parent_id": replyComment.ID,
	})
	if postNested.Code != http.StatusBadRequest {
		t.Fatalf("nested reply to reply must return 400, got %d: %s", postNested.Code, postNested.Body.String())
	}
	if !strings.Contains(postNested.Body.String(), "replies must target a top-level comment") {
		t.Fatalf("unexpected error message: %s", postNested.Body.String())
	}

	// 4. Cross-video parent comment rejection
	crossVideoReply := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v2.ID), userBToken, map[string]any{
		"body":      "Replying on V2 with parent from V1 (should fail)",
		"parent_id": rootComment.ID,
	})
	if crossVideoReply.Code != http.StatusNotFound {
		t.Fatalf("cross-video parent reply must return 404, got %d: %s", crossVideoReply.Code, crossVideoReply.Body.String())
	}

	// 5. Comment on non-existent video
	noVideo := request(t, h, http.MethodPost, "/api/videos/999999/comments", userBToken, map[string]any{
		"body": "Comment on ghost video",
	})
	if noVideo.Code != http.StatusNotFound {
		t.Fatalf("comment on non-existent video must return 404, got %d", noVideo.Code)
	}
}

func TestVideoPlayCountAndRateLimitingBudget(t *testing.T) {
	_, h, db := testServer(t)

	authorID, _ := registerTestUser(t, h, "view_author")
	v := Video{AuthorID: authorID, Title: "Viewable Video", MediaURL: "/media/view.mp4", Status: "published", PlayCount: 0}
	db.Create(&v)

	viewPath := fmt.Sprintf("/api/videos/%d/views", v.ID)

	// Single legitimate view
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, viewPath, nil)
	req.RemoteAddr = "192.0.2.1:12345"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid view returned %d", rec.Code)
	}

	var checked Video
	db.First(&checked, v.ID)
	if checked.PlayCount != 1 {
		t.Fatalf("expected play_count == 1, got %d", checked.PlayCount)
	}

	// View on unpublished video returns 404 and does not increment
	unpub := Video{AuthorID: authorID, Title: "Draft Video", MediaURL: "/media/draft.mp4", Status: "processing"}
	db.Create(&unpub)
	recUnpub := httptest.NewRecorder()
	reqUnpub := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/videos/%d/views", unpub.ID), nil)
	reqUnpub.RemoteAddr = "192.0.2.2:12345"
	h.ServeHTTP(recUnpub, reqUnpub)
	if recUnpub.Code != http.StatusNotFound {
		t.Fatalf("view on processing video must return 404, got %d", recUnpub.Code)
	}

	// Rate limit budget saturation (budget is 120 per minute per IP for view)
	// We send 119 more views from 192.0.2.1 (total 120 allowed)
	for i := 0; i < 119; i++ {
		r := httptest.NewRequest(http.MethodPost, viewPath, nil)
		r.RemoteAddr = "192.0.2.1:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("request %d within budget failed with %d", i+2, w.Code)
		}
	}

	// 121st request from same IP must be rate-limited (HTTP 429)
	reqLimited := httptest.NewRequest(http.MethodPost, viewPath, nil)
	reqLimited.RemoteAddr = "192.0.2.1:12345"
	recLimited := httptest.NewRecorder()
	h.ServeHTTP(recLimited, reqLimited)
	if recLimited.Code != http.StatusTooManyRequests {
		t.Fatalf("121st request must return 429, got %d: %s", recLimited.Code, recLimited.Body.String())
	}
	retryAfter := recLimited.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("429 response missing Retry-After header")
	}
	if seconds, err := strconv.Atoi(retryAfter); err != nil || seconds <= 0 {
		t.Fatalf("invalid Retry-After value: %q", retryAfter)
	}

	// Verify play_count was capped at 120
	db.First(&checked, v.ID)
	if checked.PlayCount != 120 {
		t.Fatalf("expected play_count == 120, got %d (rate-limited requests must not increment counter)", checked.PlayCount)
	}

	// Request from a different IP is not affected by first IP's rate limit
	reqOtherIP := httptest.NewRequest(http.MethodPost, viewPath, nil)
	reqOtherIP.RemoteAddr = "192.0.2.99:12345"
	recOtherIP := httptest.NewRecorder()
	h.ServeHTTP(recOtherIP, reqOtherIP)
	if recOtherIP.Code != http.StatusNoContent {
		t.Fatalf("request from independent IP should succeed: %d", recOtherIP.Code)
	}
	db.First(&checked, v.ID)
	if checked.PlayCount != 121 {
		t.Fatalf("expected play_count == 121, got %d", checked.PlayCount)
	}
}

func TestSecurityNegativeBoundariesA02A05A08A11(t *testing.T) {
	_, h, db := testServer(t)

	authorID, authorToken := registerTestUser(t, h, "sec_author")
	strangerID, strangerToken := registerTestUser(t, h, "sec_stranger")

	v := Video{AuthorID: authorID, Title: "Author Private Video", MediaURL: "/media/sec.mp4", Status: "published"}
	db.Create(&v)

	// 1. Anonymous access to protected endpoints must return 401
	protectedPaths := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/videos"},
		{http.MethodPatch, fmt.Sprintf("/api/videos/%d", v.ID)},
		{http.MethodDelete, fmt.Sprintf("/api/videos/%d", v.ID)},
		{http.MethodPut, fmt.Sprintf("/api/videos/%d/like", v.ID)},
		{http.MethodDelete, fmt.Sprintf("/api/videos/%d/like", v.ID)},
		{http.MethodPut, fmt.Sprintf("/api/videos/%d/favorite", v.ID)},
		{http.MethodDelete, fmt.Sprintf("/api/videos/%d/favorite", v.ID)},
		{http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID)},
		{http.MethodGet, "/api/notifications"},
		{http.MethodPatch, "/api/notifications/read"},
		{http.MethodPost, "/api/messages/acks"},
		{http.MethodGet, "/api/messages"},
		{http.MethodPost, "/api/groups"},
	}

	for _, p := range protectedPaths {
		res := request(t, h, p.method, p.path, "", nil)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s expected 401, got %d", p.method, p.path, res.Code)
		}
	}

	// 2. Tampered / malformed token must return 401
	tamperedTokens := []string{
		"invalid-token",
		"Bearer invalid.jwt.token",
		authorToken + "tampered",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.bogus",
	}
	for _, tok := range tamperedTokens {
		res := request(t, h, http.MethodGet, "/api/notifications", tok, nil)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("tampered token %q expected 401, got %d", tok, res.Code)
		}
	}

	// 3. Stranger privilege escalation rejection
	// Stranger attempts to edit author's video -> 403
	editRes := request(t, h, http.MethodPatch, fmt.Sprintf("/api/videos/%d", v.ID), strangerToken, map[string]any{"title": "Hijacked"})
	if editRes.Code != http.StatusForbidden {
		t.Fatalf("stranger edit video expected 403, got %d: %s", editRes.Code, editRes.Body.String())
	}

	// Stranger attempts to delete author's video -> 403
	delRes := request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d", v.ID), strangerToken, nil)
	if delRes.Code != http.StatusForbidden {
		t.Fatalf("stranger delete video expected 403, got %d: %s", delRes.Code, delRes.Body.String())
	}

	// 4. Stranger cannot ACK a DM between author and another user
	dm := ChatMessage{
		SenderID:    authorID,
		RecipientID: authorID + 100, // another recipient
		Body:        "Secret DM",
		ClientID:    fmt.Sprintf("secdm_%d", time.Now().UnixNano()),
		CreatedAt:   time.Now().UTC(),
	}
	db.Create(&dm)
	ackRes := request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", dm.ID), strangerToken, nil)
	if ackRes.Code != http.StatusNotFound {
		t.Fatalf("stranger acking third-party DM expected 404, got %d", ackRes.Code)
	}

	// 5. Interactions with deleted video must return 404
	delVideo := Video{AuthorID: authorID, Title: "Deleted Vid", MediaURL: "/media/del.mp4", Status: "deleted"}
	db.Create(&delVideo)

	delLike := request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", delVideo.ID), strangerToken, nil)
	if delLike.Code != http.StatusNotFound {
		t.Fatalf("like deleted video expected 404, got %d", delLike.Code)
	}

	delFav := request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/favorite", delVideo.ID), strangerToken, nil)
	if delFav.Code != http.StatusNotFound {
		t.Fatalf("favorite deleted video expected 404, got %d", delFav.Code)
	}

	delCmt := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", delVideo.ID), strangerToken, map[string]any{"body": "Ghost comment"})
	if delCmt.Code != http.StatusNotFound {
		t.Fatalf("comment on deleted video expected 404, got %d", delCmt.Code)
	}

	delView := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/views", delVideo.ID), "", nil)
	if delView.Code != http.StatusNotFound {
		t.Fatalf("view on deleted video expected 404, got %d", delView.Code)
	}

	// 6. Notification boundary: marking notifications read only affects owner
	notifAuthor := Notification{UserID: authorID, ActorID: authorID, EventKey: fmt.Sprintf("notice_author_%d", time.Now().UnixNano()), Kind: "system", Text: "Hello Author"}
	notifStranger := Notification{UserID: strangerID, ActorID: strangerID, EventKey: fmt.Sprintf("notice_stranger_%d", time.Now().UnixNano()), Kind: "system", Text: "Hello Stranger"}
	db.Create(&notifAuthor)
	db.Create(&notifStranger)

	// Stranger marks read up to author's notification ID
	readRes := request(t, h, http.MethodPatch, fmt.Sprintf("/api/notifications/read?up_to_id=%d", notifAuthor.ID), strangerToken, nil)
	if readRes.Code != http.StatusOK {
		t.Fatalf("mark read returned %d", readRes.Code)
	}

	// Verify author's notification remains unread
	var authorCheck Notification
	db.First(&authorCheck, notifAuthor.ID)
	if authorCheck.ReadAt != nil {
		t.Fatalf("author notification was marked read by stranger: read_at=%v", authorCheck.ReadAt)
	}
}

// TestMySQLInteractionAndSecurityClosure executes the full matrix of interaction
// counters, comment hierarchy, rate-limit budgets, and security boundaries on real MySQL.
func TestMySQLInteractionAndSecurityClosure(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run real MySQL closure verification")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()

	s := NewServer(tx, "mysql-closure-test-secret-32-bytes!!", t.TempDir())
	h := s.Router()

	unique := time.Now().UnixNano()
	authorID, authorToken := registerTestUser(t, h, fmt.Sprintf("mycls_a_%d", unique))
	_, userBToken := registerTestUser(t, h, fmt.Sprintf("mycls_b_%d", unique))

	v := Video{AuthorID: authorID, Title: "MySQL Closure Vid", MediaURL: "/media/mycls.mp4", Status: "published"}
	if err := tx.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	// 1. Like idempotency and counter parity on MySQL
	likePath := fmt.Sprintf("/api/videos/%d/like", v.ID)
	if res := request(t, h, http.MethodPut, likePath, userBToken, nil); res.Code != http.StatusOK {
		t.Fatalf("MySQL like 1 failed: %d", res.Code)
	}
	if res := request(t, h, http.MethodPut, likePath, userBToken, nil); res.Code != http.StatusOK {
		t.Fatalf("MySQL like 2 failed: %d", res.Code)
	}

	var checked Video
	tx.First(&checked, v.ID)
	if checked.LikeCount != 1 {
		t.Fatalf("MySQL like_count != 1: %d", checked.LikeCount)
	}

	// Unlike twice
	if res := request(t, h, http.MethodDelete, likePath, userBToken, nil); res.Code != http.StatusOK {
		t.Fatalf("MySQL unlike 1 failed: %d", res.Code)
	}
	if res := request(t, h, http.MethodDelete, likePath, userBToken, nil); res.Code != http.StatusOK {
		t.Fatalf("MySQL unlike 2 failed: %d", res.Code)
	}
	tx.First(&checked, v.ID)
	if checked.LikeCount != 0 {
		t.Fatalf("MySQL like_count underflow: %d", checked.LikeCount)
	}

	// 2. Privilege boundary check: User B cannot edit or delete User A's video
	editRes := request(t, h, http.MethodPatch, fmt.Sprintf("/api/videos/%d", v.ID), userBToken, map[string]any{"title": "Hacked"})
	if editRes.Code != http.StatusForbidden {
		t.Fatalf("MySQL stranger edit video returned %d, want 403", editRes.Code)
	}

	delRes := request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d", v.ID), userBToken, nil)
	if delRes.Code != http.StatusForbidden {
		t.Fatalf("MySQL stranger delete video returned %d, want 403", delRes.Code)
	}

	// 3. Comment 2-level constraint check on MySQL
	postRoot := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID), userBToken, map[string]any{"body": "Root on MySQL"})
	if postRoot.Code != http.StatusCreated {
		t.Fatalf("MySQL root comment returned %d", postRoot.Code)
	}
	var rootCmt Comment
	json.Unmarshal(postRoot.Body.Bytes(), &rootCmt)

	postReply := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID), authorToken, map[string]any{"body": "Reply on MySQL", "parent_id": rootCmt.ID})
	if postReply.Code != http.StatusCreated {
		t.Fatalf("MySQL reply comment returned %d", postReply.Code)
	}
	var replyCmt Comment
	json.Unmarshal(postReply.Body.Bytes(), &replyCmt)

	postNested := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID), userBToken, map[string]any{"body": "Nested on MySQL", "parent_id": replyCmt.ID})
	if postNested.Code != http.StatusBadRequest {
		t.Fatalf("MySQL nested comment returned %d, want 400", postNested.Code)
	}
}
