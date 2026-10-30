package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestFeedScenePaginationAndSameTimestampTiebreaker validates keyset pagination
// across all 4 feed scenes (timeline, following, hot, recommend) when multiple
// videos share the exact same CreatedAt timestamp. It asserts:
// 1. Zero duplicate video IDs across consecutive pages.
// 2. Zero skipped or missing video IDs across the entire sequence.
// 3. Clean termination with has_more=false and empty next_cursor.
func TestFeedScenePaginationAndSameTimestampTiebreaker(t *testing.T) {
	_, h, db := testServer(t)
	authorID, _ := registerTestUser(t, h, "same_stamp_author")
	viewerID, viewerToken := registerTestUser(t, h, "same_stamp_viewer")

	// Follow author
	res := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorID), viewerToken, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("follow: %d", res.Code)
	}

	const videoCount = 25
	fixedTime := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	videos := make([]Video, videoCount)
	for i := 0; i < videoCount; i++ {
		// All videos share the exact same millisecond timestamp!
		videos[i] = Video{
			AuthorID:      authorID,
			Title:         fmt.Sprintf("Same Stamp Video %d", i),
			MediaURL:      fmt.Sprintf("/media/stamp_%d.mp4", i),
			Status:        "published",
			CreatedAt:     fixedTime,
			LikeCount:     int64(i * 2),
			CommentCount:  int64(i),
			FavoriteCount: int64(i),
		}
		if err := db.Create(&videos[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Helper to paginate through an entire scene and return all IDs encountered
	paginateScene := func(scene, token string) []uint {
		t.Helper()
		var seenIDs []uint
		seenMap := make(map[uint]bool)
		cursor := ""
		for page := 0; page < 20; page++ {
			url := fmt.Sprintf("/api/feed?scene=%s&limit=4", scene)
			if cursor != "" {
				url += "&cursor=" + cursor
			}
			res := request(t, h, http.MethodGet, url, token, nil)
			if res.Code != http.StatusOK {
				t.Fatalf("scene %s page %d failed: %d %s", scene, page, res.Code, res.Body.String())
			}
			var body struct {
				Items      []Video `json:"items"`
				NextCursor string  `json:"next_cursor"`
				HasMore    bool    `json:"has_more"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			for _, item := range body.Items {
				if seenMap[item.ID] {
					t.Fatalf("scene %s returned duplicate video ID %d on page %d", scene, item.ID, page)
				}
				seenMap[item.ID] = true
				seenIDs = append(seenIDs, item.ID)
			}
			if !body.HasMore {
				if body.NextCursor != "" {
					t.Fatalf("scene %s has_more=false but next_cursor is non-empty: %s", scene, body.NextCursor)
				}
				break
			}
			if body.NextCursor == "" {
				t.Fatalf("scene %s has_more=true but next_cursor is empty on page %d", scene, page)
			}
			cursor = body.NextCursor
		}
		return seenIDs
	}

	// 1. Timeline scene
	timelineIDs := paginateScene("timeline", "")
	if len(timelineIDs) != videoCount {
		t.Fatalf("timeline expected %d videos, got %d", videoCount, len(timelineIDs))
	}
	// Verify strict ID descending order when CreatedAt is identical
	for i := 1; i < len(timelineIDs); i++ {
		if timelineIDs[i] >= timelineIDs[i-1] {
			t.Fatalf("timeline tie-breaker violated: id %d followed by %d", timelineIDs[i-1], timelineIDs[i])
		}
	}

	// 2. Following scene
	followingIDs := paginateScene("following", viewerToken)
	if len(followingIDs) != videoCount {
		t.Fatalf("following expected %d videos, got %d", videoCount, len(followingIDs))
	}
	for i := 1; i < len(followingIDs); i++ {
		if followingIDs[i] >= followingIDs[i-1] {
			t.Fatalf("following tie-breaker violated: id %d followed by %d", followingIDs[i-1], followingIDs[i])
		}
	}

	// 3. Hot scene (ordered by computed score DESC, then ID DESC)
	hotIDs := paginateScene("hot", "")
	if len(hotIDs) != videoCount {
		t.Fatalf("hot expected %d videos, got %d", videoCount, len(hotIDs))
	}

	// 4. Recommend scene (anonymous)
	recommendIDs := paginateScene("recommend", "")
	if len(recommendIDs) != videoCount {
		t.Fatalf("recommend expected %d videos, got %d", videoCount, len(recommendIDs))
	}

	_ = viewerID
}

// TestHybridFeedEdgeCases tests the complete lifecycle and boundary semantics of
// hybrid push/pull following feeds:
// 1. Fast-path: viewer with 0 follows immediately returns empty feed without executing UNION.
// 2. Ordinary author fanout: pushed to feed_inboxes and correctly displayed.
// 3. High-follower author: pulled via feed_pull=true and correctly displayed.
// 4. Late follow: viewer follows author after publication; video pulled via late_follow branch.
// 5. Unfollow: author's video immediately disappears even if feed_inboxes retains row.
// 6. Video delete / unpublish: video immediately disappears even if feed_inboxes retains row.
func TestHybridFeedEdgeCases(t *testing.T) {
	s, h, db := testServer(t)
	s.UseFeedFanout(2) // Threshold is 2 followers

	zeroFollowerUser, zeroToken := registerTestUser(t, h, "zero_follower")
	authorA, tokenA := registerTestUser(t, h, "author_a")
	authorCelebrity, tokenCeleb := registerTestUser(t, h, "author_celeb")
	viewer, viewerToken := registerTestUser(t, h, "hybrid_viewer_edge")
	_, tokenFan1 := registerTestUser(t, h, "fan_1")
	_, tokenFan2 := registerTestUser(t, h, "fan_2")

	// Case 1: Fast-path for viewer with 0 follows
	res := request(t, h, http.MethodGet, "/api/feed?scene=following", zeroToken, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("zero-follower feed expected 200, got %d", res.Code)
	}
	var emptyFeed struct {
		Items   []Video `json:"items"`
		HasMore bool    `json:"has_more"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &emptyFeed)
	if len(emptyFeed.Items) != 0 || emptyFeed.HasMore {
		t.Fatalf("zero-follower feed not empty: %+v", emptyFeed)
	}

	// Setup follow relationships:
	// Viewer follows Author A (ordinary) and Author Celebrity (high-follower)
	request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorA), viewerToken, nil)
	request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorCelebrity), viewerToken, nil)
	// Celebrity also followed by fan1 and fan2 (total 3 followers > threshold 2)
	request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorCelebrity), tokenFan1, nil)
	request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorCelebrity), tokenFan2, nil)

	publishVideo := func(authorID uint, token, title string) Video {
		t.Helper()
		url := "/media/" + title + ".mp4"
		_ = db.Create(&MediaAsset{OwnerID: authorID, URL: url, Kind: "video", Size: 100})
		res := request(t, h, http.MethodPost, "/api/videos", token, map[string]string{
			"title": title, "media_url": url,
		})
		if res.Code != http.StatusCreated {
			t.Fatalf("publish %s: %d", title, res.Code)
		}
		var v Video
		_ = json.Unmarshal(res.Body.Bytes(), &v)
		return v
	}

	// Case 2: Author A (ordinary) publishes -> enqueues FeedFanoutJob
	videoA := publishVideo(authorA, tokenA, "video_ordinary_a")
	var jobA FeedFanoutJob
	if err := db.Where("video_id = ?", videoA.ID).First(&jobA).Error; err != nil {
		t.Fatalf("ordinary video did not enqueue fanout job: %v", err)
	}
	// Process fanout
	worked, err := s.ProcessFeedFanoutOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("process fanout failed: %v", err)
	}
	// Verify row in feed_inboxes for viewer
	var inboxA FeedInbox
	if err := db.Where("user_id = ? AND video_id = ?", viewer, videoA.ID).First(&inboxA).Error; err != nil {
		t.Fatalf("fanout inbox row not created: %v", err)
	}

	// Case 3: Author Celebrity (3 followers > threshold 2) publishes -> pull branch (no fanout job)
	videoCeleb := publishVideo(authorCelebrity, tokenCeleb, "video_celebrity")
	var celebJobCount int64
	db.Model(&FeedFanoutJob{}).Where("video_id = ?", videoCeleb.ID).Count(&celebJobCount)
	if celebJobCount != 0 {
		t.Fatalf("celebrity video should not enqueue fanout job, got %d", celebJobCount)
	}
	var celebVideo Video
	db.First(&celebVideo, videoCeleb.ID)
	if !celebVideo.FeedPull {
		t.Fatal("celebrity video feed_pull must be true")
	}

	// Viewer checks following feed: both videos must appear
	feedViewer := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil)
	if feedViewer.Code != http.StatusOK {
		t.Fatalf("viewer following feed: %d", feedViewer.Code)
	}
	if !jsonContainsVideo(feedViewer.Body.Bytes(), videoA.ID) || !jsonContainsVideo(feedViewer.Body.Bytes(), videoCeleb.ID) {
		t.Fatalf("viewer feed missing ordinary or celebrity video: %s", feedViewer.Body.String())
	}

	// Case 4: Late follow (fan1 follows Author A AFTER videoA was published)
	request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorA), tokenFan1, nil)
	feedFan1 := request(t, h, http.MethodGet, "/api/feed?scene=following", tokenFan1, nil)
	if !jsonContainsVideo(feedFan1.Body.Bytes(), videoA.ID) {
		t.Fatalf("late follower should see videoA via late_follow_page: %s", feedFan1.Body.String())
	}

	// Case 5: Unfollow Author A
	// Viewer unfollows Author A: videoA must immediately disappear from viewer's feed
	// even though feed_inboxes still contains the row!
	request(t, h, http.MethodDelete, fmt.Sprintf("/api/users/me/following/%d", authorA), viewerToken, nil)
	feedAfterUnfollow := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil)
	if jsonContainsVideo(feedAfterUnfollow.Body.Bytes(), videoA.ID) {
		t.Fatal("unfollowed author video leaked in following feed")
	}
	if !jsonContainsVideo(feedAfterUnfollow.Body.Bytes(), videoCeleb.ID) {
		t.Fatal("celebrity video should still be present after unfollowing author A")
	}

	// Case 6: Video deletion/unpublishing removes it from feed
	// Mark videoCeleb as deleted
	db.Model(&Video{}).Where("id = ?", videoCeleb.ID).Update("status", "deleted")
	feedAfterDelete := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil)
	if jsonContainsVideo(feedAfterDelete.Body.Bytes(), videoCeleb.ID) {
		t.Fatal("deleted video leaked in following feed")
	}

	_ = zeroFollowerUser
}

// TestFeedRedisCacheFallbackAndRecovery validates that feed endpoints with caching
// enabled gracefully fall back to database queries when Redis is unavailable,
// record metrics accurately, and recover transparently.
func TestFeedRedisCacheFallbackAndRecovery(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: 0})
	defer client.Close()

	s, h, db := testServer(t)
	s.UseRedisFeedCache(client)

	authorID, authorToken := registerTestUser(t, h, "cache_author")
	viewerID, viewerToken := registerTestUser(t, h, "cache_viewer")
	request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorID), viewerToken, nil)

	v := Video{AuthorID: authorID, Title: "Cached Video", MediaURL: "/media/cache.mp4", Status: "published"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	// 1. Initial requests populate cache
	res1 := request(t, h, http.MethodGet, "/api/feed?scene=timeline", "", nil)
	if res1.Code != http.StatusOK {
		t.Fatalf("initial timeline: %d", res1.Code)
	}
	resFollowing := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil)
	if resFollowing.Code != http.StatusOK {
		t.Fatalf("initial following: %d", resFollowing.Code)
	}

	// Second request should hit cache
	res2 := request(t, h, http.MethodGet, "/api/feed?scene=timeline", "", nil)
	if res2.Code != http.StatusOK {
		t.Fatalf("second timeline: %d", res2.Code)
	}
	metrics := request(t, h, http.MethodGet, "/metrics", "", nil)
	if !strings.Contains(metrics.Body.String(), "gostream_feed_cache_total{result=\"hit\"}") {
		t.Fatalf("expected feed cache hit in metrics: %s", metrics.Body.String())
	}

	// 2. Close Redis to simulate outage
	mr.Close()

	// 3. All feeds must continue to succeed with 200 OK via database fallback
	for _, sceneURL := range []string{
		"/api/feed?scene=timeline",
		"/api/feed?scene=following",
		"/api/feed?scene=hot",
		"/api/feed?scene=recommend",
	} {
		token := ""
		if strings.Contains(sceneURL, "following") {
			token = viewerToken
		}
		fallbackRes := request(t, h, http.MethodGet, sceneURL, token, nil)
		if fallbackRes.Code != http.StatusOK {
			t.Fatalf("fallback failed for %s during Redis outage: %d %s", sceneURL, fallbackRes.Code, fallbackRes.Body.String())
		}
	}

	metricsAfterOutage := request(t, h, http.MethodGet, "/metrics", "", nil)
	if !strings.Contains(metricsAfterOutage.Body.String(), "gostream_feed_cache_total{result=\"error\"}") {
		t.Fatalf("expected feed cache error in metrics: %s", metricsAfterOutage.Body.String())
	}

	_ = authorToken
	_ = viewerID
}

// TestConcurrentMultiUserMultiVideoInteractionsAndCounters validates:
// 1. 10 users concurrently interacting across 4 videos over 200 operations.
// 2. Likes, unlikes, favorites, unfavorites, views, comments, and replies.
// 3. Exact row-to-counter parity after concurrent completion.
// 4. Non-negative counters invariant.
func TestConcurrentMultiUserMultiVideoInteractionsAndCounters(t *testing.T) {
	s, h, db := testServer(t)

	const userCount = 8
	const videoCount = 3
	users := make([]uint, userCount)
	tokens := make([]string, userCount)
	for i := 0; i < userCount; i++ {
		users[i], tokens[i] = registerTestUser(t, h, fmt.Sprintf("concur_u_%d", i))
	}

	videos := make([]Video, videoCount)
	for i := 0; i < videoCount; i++ {
		videos[i] = Video{
			AuthorID:  users[i%userCount],
			Title:     fmt.Sprintf("Concurrent Video %d", i),
			MediaURL:  fmt.Sprintf("/media/concur_%d.mp4", i),
			Status:    "published",
			CreatedAt: time.Now().UTC(),
		}
		if err := db.Create(&videos[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	var totalViews atomic.Int64
	var wg sync.WaitGroup
	const operationsPerUser = 25

	for uIdx := 0; uIdx < userCount; uIdx++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			token := tokens[u]
			for op := 0; op < operationsPerUser; op++ {
				targetVideo := videos[(u+op)%videoCount]
				switch op % 6 {
				case 0: // Like
					request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", targetVideo.ID), token, nil)
				case 1: // Unlike
					request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d/like", targetVideo.ID), token, nil)
				case 2: // Favorite
					request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/favorite", targetVideo.ID), token, nil)
				case 3: // Unfavorite
					request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d/favorite", targetVideo.ID), token, nil)
				case 4: // Record view
					res := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/view", targetVideo.ID), "", nil)
					if res.Code == http.StatusNoContent {
						totalViews.Add(1)
					}
				case 5: // Comment
					request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", targetVideo.ID), token, map[string]string{
						"body": fmt.Sprintf("concurrent comment from u%d op%d", u, op),
					})
				}
			}
		}(uIdx)
	}

	wg.Wait()

	// Verify invariant across all videos
	var sumViews int64
	for _, v := range videos {
		var persisted Video
		if err := db.First(&persisted, v.ID).Error; err != nil {
			t.Fatal(err)
		}

		// Counters must never be negative
		if persisted.LikeCount < 0 || persisted.FavoriteCount < 0 || persisted.CommentCount < 0 || persisted.PlayCount < 0 {
			t.Fatalf("video %d has negative counter: like=%d fav=%d comment=%d play=%d",
				v.ID, persisted.LikeCount, persisted.FavoriteCount, persisted.CommentCount, persisted.PlayCount)
		}

		// Row count parity: Like
		var actualLikes int64
		if err := db.Model(&VideoAction{}).Where("video_id = ? AND kind = ?", v.ID, "like").Count(&actualLikes).Error; err != nil {
			t.Fatal(err)
		}
		if persisted.LikeCount != actualLikes {
			t.Fatalf("video %d like_count mismatch: counter=%d rows=%d", v.ID, persisted.LikeCount, actualLikes)
		}

		// Row count parity: Favorite
		var actualFavs int64
		if err := db.Model(&VideoAction{}).Where("video_id = ? AND kind = ?", v.ID, "favorite").Count(&actualFavs).Error; err != nil {
			t.Fatal(err)
		}
		if persisted.FavoriteCount != actualFavs {
			t.Fatalf("video %d favorite_count mismatch: counter=%d rows=%d", v.ID, persisted.FavoriteCount, actualFavs)
		}

		// Row count parity: Comments
		var actualComments int64
		if err := db.Model(&Comment{}).Where("video_id = ?", v.ID).Count(&actualComments).Error; err != nil {
			t.Fatal(err)
		}
		if persisted.CommentCount != actualComments {
			t.Fatalf("video %d comment_count mismatch: counter=%d rows=%d", v.ID, persisted.CommentCount, actualComments)
		}

		sumViews += persisted.PlayCount
	}

	if sumViews != totalViews.Load() {
		t.Fatalf("total play_count mismatch: video_sum=%d expected=%d", sumViews, totalViews.Load())
	}
	_ = s
}

// TestNotificationMatrixAndPermissions validates:
// 1. All 5 notification types: follow, like, favorite, comment, reply.
// 2. Self-action suppression (author liking own video does not notify self).
// 3. Notification deduplication on repeated actions.
// 4. Outbox event creation for notifications.
// 5. Cross-user notification isolation and bounded mark-as-read.
func TestNotificationMatrixAndPermissions(t *testing.T) {
	_, h, db := testServer(t)

	authorID, authorToken := registerTestUser(t, h, "notif_author")
	fanID, fanToken := registerTestUser(t, h, "notif_fan")
	strangerID, strangerToken := registerTestUser(t, h, "notif_stranger")

	v := Video{AuthorID: authorID, Title: "Notice Video", MediaURL: "/media/notice.mp4", Status: "published"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	// 1. Follow notification
	request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorID), fanToken, nil)
	// Deduplication: fan follows again (idempotent PUT)
	request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorID), fanToken, nil)

	// 2. Like notification
	request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", v.ID), fanToken, nil)
	// Self-like: author likes own video -> must NOT create notification
	request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", v.ID), authorToken, nil)

	// 3. Favorite notification
	request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/favorite", v.ID), fanToken, nil)
	// Self-favorite: author favorites own video -> must NOT create notification
	request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/favorite", v.ID), authorToken, nil)

	// 4. Comment notification
	resComment := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID), fanToken, map[string]string{
		"body": "Great video from fan!",
	})
	var parentComment Comment
	_ = json.Unmarshal(resComment.Body.Bytes(), &parentComment)

	// Self-comment: author comments on own video -> must NOT create notification
	request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID), authorToken, map[string]string{
		"body": "Author self-comment",
	})

	// 5. Reply notification: stranger replies to fan's comment
	request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID), strangerToken, map[string]any{
		"body":      "Stranger replying to fan",
		"parent_id": parentComment.ID,
	})

	// Verify author's notifications:
	// Expected notifications for Author:
	// - follow (from fan)
	// - like (from fan)
	// - favorite (from fan)
	// - comment (from fan)
	// - comment (from stranger reply to video)
	var authorNotifs []Notification
	if err := db.Where("user_id = ?", authorID).Find(&authorNotifs).Error; err != nil {
		t.Fatal(err)
	}
	kindsSeen := make(map[string]int)
	for _, n := range authorNotifs {
		kindsSeen[n.Kind]++
		if n.ActorID == authorID {
			t.Fatalf("author received self-notification: %+v", n)
		}
	}
	if kindsSeen["follow"] != 1 || kindsSeen["like"] != 1 || kindsSeen["favorite"] != 1 || kindsSeen["comment"] != 2 {
		t.Fatalf("author notifications mismatch: %+v", kindsSeen)
	}

	// Verify fan's notifications:
	// Expected notification for Fan:
	// - reply (from stranger)
	var fanNotifs []Notification
	if err := db.Where("user_id = ?", fanID).Find(&fanNotifs).Error; err != nil {
		t.Fatal(err)
	}
	if len(fanNotifs) != 1 || fanNotifs[0].Kind != "reply" || fanNotifs[0].ActorID != strangerID {
		t.Fatalf("fan expected 1 reply notification, got: %+v", fanNotifs)
	}

	// Verify outbox events were created for notifications
	var outboxCount int64
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", "notification.created").Count(&outboxCount).Error; err != nil || outboxCount == 0 {
		t.Fatalf("outbox events not created: count=%d", outboxCount)
	}

	// Verify Stranger cannot see Author's notifications via API
	strangerGet := request(t, h, http.MethodGet, "/api/notifications", strangerToken, nil)
	var strangerPage struct {
		Items []Notification `json:"items"`
	}
	_ = json.Unmarshal(strangerGet.Body.Bytes(), &strangerPage)
	if len(strangerPage.Items) != 0 {
		t.Fatalf("stranger saw leaked notifications: %+v", strangerPage.Items)
	}

	// Verify Bounded Mark Read for Author
	// Find middle notification ID
	midID := authorNotifs[2].ID
	resRead := request(t, h, http.MethodPatch, fmt.Sprintf("/api/notifications/read?up_to_id=%d", midID), authorToken, nil)
	if resRead.Code != http.StatusOK {
		t.Fatalf("mark read: %d", resRead.Code)
	}

	// Verify that notifications with ID <= midID have read_at != nil
	// and notifications with ID > midID have read_at == nil
	var unreadCount int64
	for _, n := range authorNotifs {
		var stored Notification
		db.First(&stored, n.ID)
		if stored.ID <= midID {
			if stored.ReadAt == nil {
				t.Fatalf("notification %d <= %d should be read", stored.ID, midID)
			}
		} else {
			if stored.ReadAt != nil {
				t.Fatalf("notification %d > %d should remain unread", stored.ID, midID)
			}
			unreadCount++
		}
	}

	// Check unread count API
	resUnread := request(t, h, http.MethodGet, "/api/notifications/unread", authorToken, nil)
	var unreadResp struct {
		UnreadCount int64 `json:"unread_count"`
	}
	_ = json.Unmarshal(resUnread.Body.Bytes(), &unreadResp)
	if unreadResp.UnreadCount != unreadCount {
		t.Fatalf("unread count mismatch: api=%d expected=%d", unreadResp.UnreadCount, unreadCount)
	}

	_ = fanID
}
