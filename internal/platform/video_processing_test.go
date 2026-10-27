package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
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

func TestVideoSourceReplacementRequiresProcessorForUnpublishedVideos(t *testing.T) {
	processingServer, processingRouter, db := testServer(t)
	checkVideoSourceReplacementRequiresProcessor(t, processingServer, processingRouter, db)
}

func checkVideoSourceReplacementRequiresProcessor(t *testing.T, processingServer *Server, processingRouter http.Handler, db *gorm.DB) {
	t.Helper()
	processingServer.UseFFmpeg("ffmpeg-test-only")
	authorID, authorToken := registerTestUser(t, processingRouter, "recovery_author")
	_, otherToken := registerTestUser(t, processingRouter, "recovery_other")
	uploadURL := func(token string) string {
		t.Helper()
		res := uploadTestMedia(t, processingRouter, token, "clip.mp4", sampleMP4)
		var result struct {
			URL string `json:"url"`
		}
		if res.Code != http.StatusCreated || json.Unmarshal(res.Body.Bytes(), &result) != nil || result.URL == "" {
			t.Fatalf("upload: %d %s", res.Code, res.Body.String())
		}
		return result.URL
	}
	source, replacement, foreignSource := uploadURL(authorToken), uploadURL(authorToken), uploadURL(otherToken)
	createProcessing := func(title string) Video {
		t.Helper()
		res := request(t, processingRouter, http.MethodPost, "/api/videos", authorToken, map[string]string{"title": title, "media_url": source})
		var video Video
		if res.Code != http.StatusCreated || json.Unmarshal(res.Body.Bytes(), &video) != nil || video.Status != "processing" {
			t.Fatalf("create processing video: %d %s", res.Code, res.Body.String())
		}
		return video
	}
	failed := createProcessing("failed source")
	for attempt := 1; attempt <= videoProcessAttempts; attempt++ {
		if err := db.Model(&VideoProcessJob{}).Where("video_id = ?", failed.ID).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		job, err := processingServer.claimVideoProcessJob()
		if err != nil || job == nil || job.VideoID != failed.ID {
			t.Fatalf("claim failed video attempt %d: job=%+v err=%v", attempt, job, err)
		}
		processingServer.failVideoJob(*job, errors.New("controlled processing failure"))
	}
	processing := createProcessing("pending source")
	// A new server on the same database represents the next process with the
	// processor disabled, while preserving the original durable jobs.
	disabled := NewServer(db, "test-signing-secret-at-least-32-bytes", processingServer.MediaDir)
	disabledRouter := disabled.Router()
	for _, tc := range []struct {
		video  Video
		status string
	}{{failed, "failed"}, {processing, "processing"}} {
		video := tc.video
		path := fmt.Sprintf("/api/videos/%d", video.ID)
		for _, url := range []string{source, replacement} {
			res := request(t, disabledRouter, http.MethodPatch, path, authorToken, map[string]string{"title": "should not be saved", "media_url": url})
			if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "video processing is unavailable") {
				t.Errorf("video %d source %s: got %d, want actionable 409: %s", video.ID, url, res.Code, res.Body.String())
			}
		}
		owner := request(t, disabledRouter, http.MethodGet, path, authorToken, nil)
		var current Video
		if owner.Code != http.StatusOK || json.Unmarshal(owner.Body.Bytes(), &current) != nil || current.MediaURL != source || current.Title != video.Title || current.Status != tc.status {
			t.Fatalf("rejected edit changed video %d: %d %s", video.ID, owner.Code, owner.Body.String())
		}
		var jobCount int64
		if err := db.Model(&VideoProcessJob{}).Where("video_id = ?", video.ID).Count(&jobCount).Error; err != nil || jobCount != 1 {
			t.Fatalf("rejected edit changed jobs for video %d: count=%d err=%v", video.ID, jobCount, err)
		}
		if public := request(t, disabledRouter, http.MethodGet, path, "", nil); public.Code != http.StatusNotFound {
			t.Fatalf("unpublished video %d became public: %d", video.ID, public.Code)
		}
		if feed := request(t, disabledRouter, http.MethodGet, "/api/feed", "", nil); feed.Code != http.StatusOK || jsonContainsVideo(feed.Body.Bytes(), video.ID) {
			t.Fatalf("unpublished video %d in feed: %d %s", video.ID, feed.Code, feed.Body.String())
		}
	}
	failedPath := fmt.Sprintf("/api/videos/%d", failed.ID)
	if res := request(t, disabledRouter, http.MethodPatch, failedPath, otherToken, map[string]string{"media_url": foreignSource}); res.Code != http.StatusForbidden {
		t.Fatalf("non-author edit: %d %s", res.Code, res.Body.String())
	}
	if res := request(t, disabledRouter, http.MethodPatch, failedPath, authorToken, map[string]string{"media_url": foreignSource}); res.Code != http.StatusBadRequest {
		t.Fatalf("unowned replacement: %d %s", res.Code, res.Body.String())
	}
	if res := request(t, disabledRouter, http.MethodPatch, failedPath, authorToken, map[string]string{"title": "metadata edit"}); res.Code != http.StatusOK {
		t.Fatalf("metadata-only edit: %d %s", res.Code, res.Body.String())
	}
	created := request(t, disabledRouter, http.MethodPost, "/api/videos", authorToken, map[string]string{"title": "immediate", "media_url": source})
	var published Video
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &published) != nil || published.Status != "published" {
		t.Fatalf("no-processor publication: %d %s", created.Code, created.Body.String())
	}
	if res := request(t, disabledRouter, http.MethodPatch, fmt.Sprintf("/api/videos/%d", published.ID), authorToken, map[string]string{"media_url": replacement}); res.Code != http.StatusOK || !jsonContainsStatus(res.Body.Bytes(), "published") {
		t.Fatalf("published video replacement: %d %s", res.Code, res.Body.String())
	}
	if feed := request(t, disabledRouter, http.MethodGet, "/api/feed", "", nil); !jsonContainsVideo(feed.Body.Bytes(), published.ID) {
		t.Fatalf("published video missing from feed: %s", feed.Body.String())
	}
	resumed := NewServer(db, "test-signing-secret-at-least-32-bytes", processingServer.MediaDir)
	resumed.UseFFmpeg("ffmpeg-test-only")
	resumedRouter := resumed.Router()
	for _, video := range []Video{failed, processing} {
		path := fmt.Sprintf("/api/videos/%d", video.ID)
		if res := request(t, resumedRouter, http.MethodPatch, path, authorToken, map[string]string{"media_url": replacement}); res.Code != http.StatusOK || !jsonContainsStatus(res.Body.Bytes(), "processing") {
			t.Fatalf("retry video %d after enabling processor: %d %s", video.ID, res.Code, res.Body.String())
		}
		var jobCount int64
		if err := db.Model(&VideoProcessJob{}).Where("video_id = ?", video.ID).Count(&jobCount).Error; err != nil || jobCount != 2 {
			t.Fatalf("video %d retry job count = %d, err=%v", video.ID, jobCount, err)
		}
	}
	oldJob, err := resumed.claimVideoProcessJob()
	if err != nil || oldJob == nil || oldJob.VideoID != processing.ID || oldJob.SourceURL != source {
		t.Fatalf("claim obsolete processing job: %+v, err=%v", oldJob, err)
	}
	if err := resumed.processVideoJob(context.Background(), *oldJob); err != nil {
		t.Fatalf("retire obsolete processing job: %v", err)
	}
	current := request(t, resumedRouter, http.MethodGet, fmt.Sprintf("/api/videos/%d", processing.ID), authorToken, nil)
	var processingVideo Video
	if current.Code != http.StatusOK || json.Unmarshal(current.Body.Bytes(), &processingVideo) != nil || processingVideo.Status != "processing" || processingVideo.MediaURL != replacement {
		t.Fatalf("obsolete job changed replacement: %d %s", current.Code, current.Body.String())
	}
	if profile := request(t, disabledRouter, http.MethodGet, fmt.Sprintf("/api/users/%d/videos", authorID), "", nil); jsonContainsVideo(profile.Body.Bytes(), failed.ID) {
		t.Fatal("processing retry leaked into public profile")
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
