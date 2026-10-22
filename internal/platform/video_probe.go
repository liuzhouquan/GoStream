package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	maxVideoDuration = 10 * time.Minute
	maxVideoSide     = 4096
	maxVideoPixels   = 3840 * 2160
)

var errUnsupportedVideo = errors.New("unsupported video")

type videoValidationError struct {
	Code   string
	Detail string
}

func (e *videoValidationError) Error() string { return errUnsupportedVideo.Error() + ": " + e.Detail }
func (e *videoValidationError) Unwrap() error { return errUnsupportedVideo }

func invalidVideo(code, detail string) error {
	return &videoValidationError{Code: code, Detail: detail}
}

type videoMetadata struct {
	DurationMS int64
	Width      int
	Height     int
}

func probeVideo(ctx context.Context, binary, path string) (videoMetadata, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, binary, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height,duration:format=duration", "-of", "json", path).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 400 {
			message = message[:400]
		}
		return videoMetadata{}, fmt.Errorf("ffprobe: %w: %s", err, message)
	}
	return parseVideoProbe(output)
}

func parseVideoProbe(output []byte) (videoMetadata, error) {
	var report struct {
		Streams []struct {
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Duration string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		return videoMetadata{}, invalidVideo("invalid_media", "invalid ffprobe output")
	}
	if len(report.Streams) != 1 {
		return videoMetadata{}, invalidVideo("missing_video", "a video stream is required")
	}
	stream := report.Streams[0]
	if stream.Width < 1 || stream.Height < 1 || stream.Width > maxVideoSide || stream.Height > maxVideoSide || int64(stream.Width)*int64(stream.Height) > maxVideoPixels {
		return videoMetadata{}, invalidVideo("resolution_limit", "frame dimensions exceed 4096 pixels per side or 3840x2160 pixels total")
	}
	duration := report.Format.Duration
	if duration == "" || duration == "N/A" {
		duration = stream.Duration
	}
	seconds, err := strconv.ParseFloat(duration, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return videoMetadata{}, invalidVideo("invalid_duration", "video duration is unavailable or invalid")
	}
	if seconds > maxVideoDuration.Seconds() {
		return videoMetadata{}, invalidVideo("duration_limit", "duration exceeds 10 minutes")
	}
	durationMS := int64(math.Round(seconds * 1000))
	if durationMS < 1 {
		durationMS = 1
	}
	return videoMetadata{DurationMS: durationMS, Width: stream.Width, Height: stream.Height}, nil
}
