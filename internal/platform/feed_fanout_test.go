package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func collectFollowingIDs(t *testing.T, h http.Handler, token string) []uint {
	t.Helper()
	path := "/api/feed?scene=following&limit=1"
	var ids []uint
	seen := map[uint]bool{}
	for pageNumber := 0; pageNumber < 20; pageNumber++ {
		res := request(t, h, http.MethodGet, path, token, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("following page: %d %s", res.Code, res.Body.String())
		}
		var page struct {
			Items      []Video `json:"items"`
			NextCursor string  `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, video := range page.Items {
			if seen[video.ID] {
				t.Fatalf("duplicate video %d across pages", video.ID)
			}
			seen[video.ID] = true
			ids = append(ids, video.ID)
		}
		if !page.HasMore {
			return ids
		}
		if page.NextCursor == "" {
			t.Fatal("has_more without cursor")
		}
		path = "/api/feed?scene=following&limit=1&cursor=" + page.NextCursor
	}
	t.Fatal("following pagination did not finish")
	return nil
}

func TestHybridFollowingPublicationAndRelationshipChanges(t *testing.T) {
	s, h, db := testServer(t)
	s.UseFeedFanout(1)
	lowAuthor, lowToken := registerTestUser(t, h, "hybrid_low")
	highAuthor, highToken := registerTestUser(t, h, "hybrid_high")
	viewer, viewerToken := registerTestUser(t, h, "hybrid_viewer")
	_, extraToken := registerTestUser(t, h, "hybrid_extra")
	lateID, lateToken := registerTestUser(t, h, "hybrid_late")
	old := Video{AuthorID: lowAuthor, Title: "old", MediaURL: "/media/hybrid-old.mp4", Status: "published", CreatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		token string
		id    uint
	}{{viewerToken, lowAuthor}, {viewerToken, highAuthor}, {extraToken, highAuthor}} {
		res := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", pair.id), pair.token, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("follow: %d %s", res.Code, res.Body.String())
		}
	}
	publish := func(author uint, token, title string) Video {
		t.Helper()
		url := "/media/" + title + ".mp4"
		if err := db.Create(&MediaAsset{OwnerID: author, URL: url, Kind: "video", Size: 10}).Error; err != nil {
			t.Fatal(err)
		}
		res := request(t, h, http.MethodPost, "/api/videos", token, map[string]string{"title": title, "media_url": url})
		if res.Code != http.StatusCreated {
			t.Fatalf("publish: %d %s", res.Code, res.Body.String())
		}
		var video Video
		if err := json.Unmarshal(res.Body.Bytes(), &video); err != nil {
			t.Fatal(err)
		}
		return video
	}
	low := publish(lowAuthor, lowToken, "low-new")
	high := publish(highAuthor, highToken, "high-new")
	var job FeedFanoutJob
	if err := db.Where("video_id = ?", low.ID).First(&job).Error; err != nil {
		t.Fatal("low-follower publication has no durable job:", err)
	}
	var highJobs int64
	if err := db.Model(&FeedFanoutJob{}).Where("video_id = ?", high.ID).Count(&highJobs).Error; err != nil || highJobs != 0 {
		t.Fatalf("high-follower publication used push: count=%d err=%v", highJobs, err)
	}
	before := collectFollowingIDs(t, h, viewerToken)
	if len(before) != 3 {
		t.Fatalf("pending job hid a video: %v", before)
	}
	for i := 0; i < 5; i++ {
		worked, err := s.ProcessFeedFanoutOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	if err := db.First(&job, job.ID).Error; err != nil || job.DoneAt == nil {
		t.Fatalf("fanout did not complete: %+v err=%v", job, err)
	}
	var completedVideo Video
	if err := db.First(&completedVideo, low.ID).Error; err != nil || completedVideo.FeedPull {
		t.Fatalf("completed fanout must leave pull index: %+v err=%v", completedVideo, err)
	}
	after := collectFollowingIDs(t, h, viewerToken)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("feed changed across fanout: before=%v after=%v", before, after)
	}
	if res := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", lowAuthor), lateToken, nil); res.Code != http.StatusOK {
		t.Fatalf("late follow: %d %s", res.Code, res.Body.String())
	}
	// A follower added during a batch may be in both the inbox and the
	// late-follow pull branch. UNION must return that video once.
	if err := db.Create(&FeedInbox{UserID: lateID, VideoID: low.ID, CreatedAt: low.CreatedAt}).Error; err != nil {
		t.Fatal(err)
	}
	if got := collectFollowingIDs(t, h, lateToken); !reflect.DeepEqual(got, []uint{low.ID, old.ID}) {
		t.Fatalf("late follower lost older published videos: %v", got)
	}
	if res := request(t, h, http.MethodDelete, fmt.Sprintf("/api/users/me/following/%d", lowAuthor), viewerToken, nil); res.Code != http.StatusOK {
		t.Fatalf("unfollow: %d %s", res.Code, res.Body.String())
	}
	if got := collectFollowingIDs(t, h, viewerToken); !reflect.DeepEqual(got, []uint{high.ID}) {
		t.Fatalf("unfollow leaked inbox video to viewer %d: %v", viewer, got)
	}
}

func TestFeedFanoutBatchResumeAndIdempotency(t *testing.T) {
	s, h, db := testServer(t)
	s.UseFeedFanout(200)
	author := User{Username: "batch_author", Nickname: "Batch", PasswordHash: "test"}
	if err := db.Create(&author).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < feedFanoutBatchSize+2; i++ {
		user := User{Username: fmt.Sprintf("batch_user_%d", i), Nickname: "Viewer", PasswordHash: "test"}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&Follow{FollowerID: user.ID, FolloweeID: author.ID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var video Video
	if err := db.Transaction(func(tx *gorm.DB) error {
		video = Video{AuthorID: author.ID, Title: "batch", MediaURL: "/media/batch.mp4", Status: "published"}
		if err := tx.Create(&video).Error; err != nil {
			return err
		}
		return s.enqueueFeedFanout(tx, video)
	}); err != nil {
		t.Fatal(err)
	}
	worked, err := s.ProcessFeedFanoutOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("first batch: worked=%v err=%v", worked, err)
	}
	var count int64
	if err := db.Model(&FeedInbox{}).Where("video_id = ?", video.ID).Count(&count).Error; err != nil || count != feedFanoutBatchSize {
		t.Fatalf("first batch size: count=%d err=%v", count, err)
	}
	// Force a repeated batch as though an older cursor were replayed. The
	// unique key must preserve one inbox row per follower and video.
	if err := db.Model(&FeedFanoutJob{}).Where("video_id = ?", video.ID).Update("last_follower_id", 0).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&FeedFanoutJob{}).Where("video_id = ?", video.ID).Update("lease_until", time.Now().UTC().Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if worked, err := s.ProcessFeedFanoutOnce(context.Background()); err != nil || worked {
		t.Fatalf("active lease was reclaimed: worked=%v err=%v", worked, err)
	}
	if err := db.Model(&FeedFanoutJob{}).Where("video_id = ?", video.ID).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		worked, err = s.ProcessFeedFanoutOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	if err := db.Model(&FeedInbox{}).Where("video_id = ?", video.ID).Count(&count).Error; err != nil || count != feedFanoutBatchSize+2 {
		t.Fatalf("retry multiplied or lost inbox rows: count=%d err=%v", count, err)
	}
	var job FeedFanoutJob
	if err := db.Where("video_id = ?", video.ID).First(&job).Error; err != nil || job.DoneAt == nil {
		t.Fatalf("job not complete: %+v err=%v", job, err)
	}
	metrics := request(t, h, http.MethodGet, "/metrics", "", nil)
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "gostream_feed_fanout_batches_total{result=\"completed\"} 1") ||
		!strings.Contains(metrics.Body.String(), "gostream_feed_fanout_completion_seconds_count 1") {
		t.Fatalf("fanout completion metrics missing: status=%d body=%s", metrics.Code, metrics.Body.String())
	}
}

func TestHybridFollowingZeroFollowsIgnoresStaleInbox(t *testing.T) {
	s, h, db := testServer(t)
	s.UseFeedFanout(1000)
	viewer, token := registerTestUser(t, h, "empty_hybrid_viewer")
	author, _ := registerTestUser(t, h, "empty_hybrid_author")
	video := Video{AuthorID: author, Title: "stale inbox", MediaURL: "/media/stale.mp4", Status: "published", CreatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&video).UpdateColumn("feed_pull", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&FeedInbox{UserID: viewer, VideoID: video.ID, CreatedAt: video.CreatedAt}).Error; err != nil {
		t.Fatal(err)
	}
	if ids := collectFollowingIDs(t, h, token); len(ids) != 0 {
		t.Fatalf("zero-follow viewer sees stale IDs: %v", ids)
	}
	path := fmt.Sprintf("/api/users/me/following/%d", author)
	if r := request(t, h, http.MethodPut, path, token, nil); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if ids := collectFollowingIDs(t, h, token); len(ids) != 1 || ids[0] != video.ID {
		t.Fatalf("follow did not reveal exact video: %v", ids)
	}
	if r := request(t, h, http.MethodDelete, path, token, nil); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if ids := collectFollowingIDs(t, h, token); len(ids) != 0 {
		t.Fatalf("unfollow leaked stale inbox: %v", ids)
	}
}
