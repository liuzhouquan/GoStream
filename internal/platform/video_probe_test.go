package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestParseVideoProbe(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  videoMetadata
		code  string
	}{
		{"format duration", `{"streams":[{"width":160,"height":90,"duration":"0.9"}],"format":{"duration":"1.2345"}}`, videoMetadata{DurationMS: 1235, Width: 160, Height: 90}, ""},
		{"stream duration fallback", `{"streams":[{"width":3840,"height":2160,"duration":"600.000"}],"format":{"duration":"N/A"}}`, videoMetadata{DurationMS: 600000, Width: 3840, Height: 2160}, ""},
		{"no video stream", `{"streams":[],"format":{"duration":"1"}}`, videoMetadata{}, "missing_video"},
		{"too many pixels", `{"streams":[{"width":4096,"height":2160}],"format":{"duration":"1"}}`, videoMetadata{}, "resolution_limit"},
		{"side too large", `{"streams":[{"width":100,"height":4097}],"format":{"duration":"1"}}`, videoMetadata{}, "resolution_limit"},
		{"too long", `{"streams":[{"width":160,"height":90}],"format":{"duration":"600.001"}}`, videoMetadata{}, "duration_limit"},
		{"missing duration", `{"streams":[{"width":160,"height":90}],"format":{}}`, videoMetadata{}, "invalid_duration"},
		{"not a number", `{"streams":[{"width":160,"height":90}],"format":{"duration":"NaN"}}`, videoMetadata{}, "invalid_duration"},
		{"bad JSON", `{`, videoMetadata{}, "invalid_media"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVideoProbe([]byte(tc.input))
			if tc.code != "" {
				if !errors.Is(err, errUnsupportedVideo) {
					t.Fatalf("expected unsupported video, got %v", err)
				}
				var validation *videoValidationError
				if !errors.As(err, &validation) || validation.Code != tc.code {
					t.Fatalf("failure code = %v, want %s", err, tc.code)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("metadata = %+v, err = %v, want %+v", got, err, tc.want)
			}
		})
	}
}

func jsonContainsProcessingError(raw []byte, code string) bool {
	var video Video
	return json.Unmarshal(raw, &video) == nil && video.ProcessingError == code
}

func jsonContainsVideoError(raw []byte, id uint, code string) bool {
	var response struct {
		Items []Video `json:"items"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return false
	}
	for _, video := range response.Items {
		if video.ID == id {
			return video.ProcessingError == code
		}
	}
	return false
}

func TestInvalidVideoEndsProcessingAfterOneAttempt(t *testing.T) {
	s, h, db := testServer(t)
	userID, token := registerTestUser(t, h, "invalid_video_author")
	video := Video{AuthorID: userID, Title: "invalid source", MediaURL: "/media/invalid.mp4", Status: "processing"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	job := VideoProcessJob{VideoID: video.ID, SourceURL: video.MediaURL}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := s.claimVideoProcessJob()
	if err != nil || claimed == nil {
		t.Fatalf("claim = %+v, err = %v", claimed, err)
	}
	s.failVideoJob(*claimed, fmt.Errorf("inspect source: %w", invalidVideo("duration_limit", "duration exceeds 10 minutes")))
	if err := db.First(&job, job.ID).Error; err != nil || job.DeadAt == nil || job.Attempts != 1 || job.FailureCode != "duration_limit" {
		t.Fatalf("invalid job was retried: %+v, err = %v", job, err)
	}
	if err := db.First(&video, video.ID).Error; err != nil || video.Status != "failed" {
		t.Fatalf("invalid video did not fail: %+v, err = %v", video, err)
	}
	path := fmt.Sprintf("/api/videos/%d", video.ID)
	if got := request(t, h, "GET", path, "", nil); got.Code != 404 {
		t.Fatalf("public failure details leaked: %d %s", got.Code, got.Body.String())
	}
	owner := request(t, h, "GET", path, token, nil)
	if owner.Code != 200 || !jsonContainsProcessingError(owner.Body.Bytes(), "duration_limit") {
		t.Fatalf("owner failure code: %d %s", owner.Code, owner.Body.String())
	}
	list := request(t, h, "GET", fmt.Sprintf("/api/users/%d/videos", userID), token, nil)
	if list.Code != 200 || !jsonContainsVideoError(list.Body.Bytes(), video.ID, "duration_limit") {
		t.Fatalf("owner failure list: %d %s", list.Code, list.Body.String())
	}
}
