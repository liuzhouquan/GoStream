package platform

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseVideoProbe(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    videoMetadata
		invalid bool
	}{
		{"format duration", `{"streams":[{"width":160,"height":90,"duration":"0.9"}],"format":{"duration":"1.2345"}}`, videoMetadata{DurationMS: 1235, Width: 160, Height: 90}, false},
		{"stream duration fallback", `{"streams":[{"width":3840,"height":2160,"duration":"600.000"}],"format":{"duration":"N/A"}}`, videoMetadata{DurationMS: 600000, Width: 3840, Height: 2160}, false},
		{"no video stream", `{"streams":[],"format":{"duration":"1"}}`, videoMetadata{}, true},
		{"too many pixels", `{"streams":[{"width":4096,"height":2160}],"format":{"duration":"1"}}`, videoMetadata{}, true},
		{"side too large", `{"streams":[{"width":100,"height":4097}],"format":{"duration":"1"}}`, videoMetadata{}, true},
		{"too long", `{"streams":[{"width":160,"height":90}],"format":{"duration":"600.001"}}`, videoMetadata{}, true},
		{"missing duration", `{"streams":[{"width":160,"height":90}],"format":{}}`, videoMetadata{}, true},
		{"not a number", `{"streams":[{"width":160,"height":90}],"format":{"duration":"NaN"}}`, videoMetadata{}, true},
		{"bad JSON", `{`, videoMetadata{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVideoProbe([]byte(tc.input))
			if tc.invalid {
				if !errors.Is(err, errUnsupportedVideo) {
					t.Fatalf("expected unsupported video, got %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("metadata = %+v, err = %v, want %+v", got, err, tc.want)
			}
		})
	}
}

func TestInvalidVideoEndsProcessingAfterOneAttempt(t *testing.T) {
	s, _, db := testServer(t)
	user := User{Username: "invalid_video_author", PasswordHash: "unused", Nickname: "Invalid video"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	video := Video{AuthorID: user.ID, Title: "invalid source", MediaURL: "/media/invalid.mp4", Status: "processing"}
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
	s.failVideoJob(*claimed, fmt.Errorf("inspect source: %w", errUnsupportedVideo))
	if err := db.First(&job, job.ID).Error; err != nil || job.DeadAt == nil || job.Attempts != 1 {
		t.Fatalf("invalid job was retried: %+v, err = %v", job, err)
	}
	if err := db.First(&video, video.ID).Error; err != nil || video.Status != "failed" {
		t.Fatalf("invalid video did not fail: %+v, err = %v", video, err)
	}
}
