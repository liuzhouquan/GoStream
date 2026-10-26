package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestVideoProcessingStateRetriesAndPrivacy(t *testing.T) {
	s, h, db := testServer(t)
	s.UseFFmpeg("ffmpeg-test-only")
	authorID, token := registerTestUser(t, h, "processor_author")
	upload := uploadTestMedia(t, h, token, "clip.mp4", sampleMP4)
	var asset struct {
		URL string `json:"url"`
	}
	if upload.Code != http.StatusCreated || json.Unmarshal(upload.Body.Bytes(), &asset) != nil {
		t.Fatalf("upload: %d %s", upload.Code, upload.Body.String())
	}
	created := request(t, h, http.MethodPost, "/api/videos", token, map[string]string{"title": "processing clip", "media_url": asset.URL})
	var video Video
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &video) != nil || video.Status != "processing" {
		t.Fatalf("publish: %d %s", created.Code, created.Body.String())
	}
	path := fmt.Sprintf("/api/videos/%d", video.ID)
	if got := request(t, h, http.MethodGet, path, "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("anonymous processing video: %d", got.Code)
	}
	if got := request(t, h, http.MethodGet, path, token, nil); got.Code != http.StatusOK {
		t.Fatalf("owner processing video: %d", got.Code)
	}
	if got := request(t, h, http.MethodGet, "/api/feed", "", nil); got.Code != http.StatusOK || jsonContainsVideo(got.Body.Bytes(), video.ID) {
		t.Fatalf("processing video leaked into feed: %d %s", got.Code, got.Body.String())
	}
	listPath := fmt.Sprintf("/api/users/%d/videos", authorID)
	if got := request(t, h, http.MethodGet, listPath, "", nil); jsonContainsVideo(got.Body.Bytes(), video.ID) {
		t.Fatal("processing video leaked into public profile")
	}
	if got := request(t, h, http.MethodGet, listPath, token, nil); !jsonContainsVideo(got.Body.Bytes(), video.ID) {
		t.Fatal("owner could not see processing video")
	}
	for attempt := 1; attempt <= videoProcessAttempts; attempt++ {
		if err := db.Model(&VideoProcessJob{}).Where("video_id = ?", video.ID).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		job, err := s.claimVideoProcessJob()
		if err != nil || job == nil || job.Attempts != attempt {
			var stored VideoProcessJob
			_ = db.Where("video_id = ?", video.ID).First(&stored).Error
			t.Fatalf("claim %d: job=%+v stored=%+v err=%v", attempt, job, stored, err)
		}
		s.failVideoJob(*job, errors.New("invalid codec"))
	}
	if err := db.First(&video, video.ID).Error; err != nil || video.Status != "failed" {
		t.Fatalf("failed video state: %+v %v", video, err)
	}
	if owner := request(t, h, http.MethodGet, path, token, nil); owner.Code != http.StatusOK || !jsonContainsProcessingError(owner.Body.Bytes(), "processing_failed") {
		t.Fatalf("generic failure code: %d %s", owner.Code, owner.Body.String())
	}
	var job VideoProcessJob
	if err := db.Where("video_id = ?", video.ID).First(&job).Error; err != nil || job.DeadAt == nil || job.Attempts != videoProcessAttempts {
		t.Fatalf("dead job state: %+v %v", job, err)
	}
	if err := db.Model(&video).Updates(map[string]any{"duration_ms": 2000, "width": 640, "height": 360}).Error; err != nil {
		t.Fatal(err)
	}
	retry := request(t, h, http.MethodPatch, path, token, map[string]string{"media_url": asset.URL})
	if retry.Code != http.StatusOK || !jsonContainsStatus(retry.Body.Bytes(), "processing") {
		t.Fatalf("retry failed video: %d %s", retry.Code, retry.Body.String())
	}
	if err := db.First(&video, video.ID).Error; err != nil || video.DurationMS != 0 || video.Width != 0 || video.Height != 0 {
		t.Fatalf("retry retained old metadata: %+v %v", video, err)
	}
	claimed, err := s.claimVideoProcessJob()
	if err != nil || claimed == nil {
		t.Fatalf("retry claim: %+v %v", claimed, err)
	}
	if deleted := request(t, h, http.MethodDelete, path, token, nil); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete processing video: %d %s", deleted.Code, deleted.Body.String())
	}
	published, _, err := s.completeVideoJob(*claimed, "/media/late.mp4", "/media/late.jpg", 10, 10, videoMetadata{DurationMS: 1000, Width: 160, Height: 90})
	if err != nil || published {
		t.Fatalf("deleted video was republished: published=%v err=%v", published, err)
	}
	if got := request(t, h, http.MethodGet, path, token, nil); got.Code != http.StatusNotFound {
		t.Fatalf("deleted processing video visible: %d", got.Code)
	}
}

func TestVideoProcessingCompletionPublishesAtomically(t *testing.T) {
	s, h, db := testServer(t)
	s.UseFFmpeg("ffmpeg-test-only")
	authorID, token := registerTestUser(t, h, "processor_success")
	_, viewerToken := registerTestUser(t, h, "processor_viewer")
	if res := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorID), viewerToken, nil); res.Code != http.StatusOK {
		t.Fatalf("follow processing author: %d %s", res.Code, res.Body.String())
	}
	upload := uploadTestMedia(t, h, token, "clip.mp4", sampleMP4)
	var asset struct {
		URL string `json:"url"`
	}
	if upload.Code != http.StatusCreated || json.Unmarshal(upload.Body.Bytes(), &asset) != nil {
		t.Fatalf("upload: %d %s", upload.Code, upload.Body.String())
	}
	created := request(t, h, http.MethodPost, "/api/videos", token, map[string]string{"title": "processed clip", "media_url": asset.URL})
	var video Video
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &video) != nil {
		t.Fatalf("publish: %d %s", created.Code, created.Body.String())
	}
	job, err := s.claimVideoProcessJob()
	if err != nil || job == nil {
		t.Fatalf("claim: %+v %v", job, err)
	}
	var fanoutCount int64
	if err := db.Model(&FeedFanoutJob{}).Where("video_id = ?", video.ID).Count(&fanoutCount).Error; err != nil || fanoutCount != 0 {
		t.Fatalf("processing video queued fanout early: count=%d err=%v", fanoutCount, err)
	}
	published, coverUsed, err := s.completeVideoJob(*job, "/media/processed-test.mp4", "/media/cover-test.jpg", 200, 20, videoMetadata{DurationMS: 1000, Width: 160, Height: 90})
	if err != nil || !published || !coverUsed {
		t.Fatalf("complete: published=%v cover=%v err=%v", published, coverUsed, err)
	}
	if err := db.First(&video, video.ID).Error; err != nil || video.Status != "published" || video.MediaURL != "/media/processed-test.mp4" || video.CoverURL != "/media/cover-test.jpg" || video.DurationMS != 1000 || video.Width != 160 || video.Height != 90 {
		t.Fatalf("published state: %+v %v", video, err)
	}
	if got := request(t, h, http.MethodGet, "/api/feed", "", nil); !jsonContainsVideo(got.Body.Bytes(), video.ID) {
		t.Fatal("processed video missing from public feed")
	}
	if err := db.Model(&FeedFanoutJob{}).Where("video_id = ?", video.ID).Count(&fanoutCount).Error; err != nil || fanoutCount != 1 {
		t.Fatalf("published video has no fanout job: count=%d err=%v", fanoutCount, err)
	}
	if got := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil); !jsonContainsVideo(got.Body.Bytes(), video.ID) {
		t.Fatal("unfinished fanout hid processed video from follower")
	}
	var count int64
	if err := db.Model(&MediaAsset{}).Where("url IN ?", []string{video.MediaURL, video.CoverURL}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("generated assets not registered: %d %v", count, err)
	}
	if _, _, err := s.completeVideoJob(*job, "/media/other.mp4", "/media/other.jpg", 1, 1, videoMetadata{}); err == nil {
		t.Fatal("completed job accepted duplicate completion")
	}
}

func jsonContainsVideo(raw []byte, id uint) bool {
	var page struct {
		Items []Video `json:"items"`
	}
	if json.Unmarshal(raw, &page) != nil {
		return false
	}
	for _, video := range page.Items {
		if video.ID == id {
			return true
		}
	}
	return false
}

func jsonContainsStatus(raw []byte, status string) bool {
	var video Video
	return json.Unmarshal(raw, &video) == nil && video.Status == status
}
