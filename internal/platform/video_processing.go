package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
)

const videoProcessAttempts = 3

func (s *Server) UseFFmpeg(binary string) {
	s.ffmpegBin = binary
	s.ffprobeBin = "ffprobe"
}

func (s *Server) UseFFprobe(binary string) { s.ffprobeBin = binary }

// RunVideoProcessing claims durable jobs. A crashed worker leaves a lease that
// another instance can reclaim. Only one process handles a job at a time.
func (s *Server) RunVideoProcessing(ctx context.Context) {
	if s.ffmpegBin == "" {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	cleanupTicker := time.NewTicker(time.Hour)
	defer cleanupTicker.Stop()
	s.cleanupAbandonedProcessingDirs()
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := s.claimVideoProcessJob()
		if err != nil {
			log.Printf("video processing claim: %v", err)
		} else if job != nil {
			if err := s.processVideoJob(ctx, *job); err != nil {
				log.Printf("video processing job %d: %v", job.ID, err)
				s.failVideoJob(*job, err)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-cleanupTicker.C:
			s.cleanupAbandonedProcessingDirs()
		}
	}
}

func (s *Server) cleanupAbandonedProcessingDirs() {
	entries, err := os.ReadDir(filepath.Dir(s.MediaDir))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".gostream-process-") {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(filepath.Dir(s.MediaDir), entry.Name()))
		}
	}
}

func (s *Server) claimVideoProcessJob() (*VideoProcessJob, error) {
	for attempt := 0; attempt < 3; attempt++ {
		now := time.Now().UTC()
		var jobs []VideoProcessJob
		pending := "done_at IS NULL AND dead_at IS NULL AND available_at <= ? AND (lease_until IS NULL OR lease_until < ?)"
		err := s.DB.Where(pending, now, now).Order("id ASC").Limit(1).Find(&jobs).Error
		if err != nil {
			return nil, err
		}
		if len(jobs) == 0 {
			return nil, nil
		}
		job := jobs[0]
		lease := now.Add(5 * time.Minute).Truncate(time.Millisecond)
		res := s.DB.Model(&VideoProcessJob{}).Where("id = ? AND "+pending, job.ID, now, now).
			Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "lease_until": lease})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 1 {
			job.Attempts++
			job.LeaseUntil = &lease
			return &job, nil
		}
	}
	return nil, nil
}

func (s *Server) processVideoJob(ctx context.Context, job VideoProcessJob) error {
	var video Video
	if err := s.DB.First(&video, job.VideoID).Error; err != nil {
		return err
	}
	if video.Status == "deleted" || video.MediaURL != job.SourceURL {
		_, _, err := s.completeVideoJob(job, "", "", 0, 0, videoMetadata{})
		return err
	}
	name := strings.TrimPrefix(job.SourceURL, "/media/")
	if !strings.HasPrefix(job.SourceURL, "/media/") || name == "" || name != filepath.Base(name) {
		return errors.New("invalid media source path")
	}
	workDir, err := os.MkdirTemp(filepath.Dir(s.MediaDir), ".gostream-process-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	source := filepath.Join(s.MediaDir, name)
	if s.objectMedia != nil {
		source = filepath.Join(workDir, "source"+filepath.Ext(name))
		if err := s.downloadObjectFile(ctx, "media/"+name, source); err != nil {
			return fmt.Errorf("source media unavailable: %w", err)
		}
	} else if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("source media unavailable: %w", err)
	}
	if _, err := probeVideo(ctx, s.ffprobeBin, source); err != nil {
		return fmt.Errorf("inspect source media: %w", err)
	}
	output := filepath.Join(workDir, "video.mp4")
	thumbnail := filepath.Join(workDir, "cover.jpg")
	commandCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	if err := runFFmpeg(commandCtx, s.ffmpegBin, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", source,
		"-map", "0:v:0", "-map", "0:a?", "-vf", "scale=trunc(min(iw\\,720)/2)*2:-2",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28", "-threads", "2", "-c:a", "aac", "-b:a", "128k",
		"-movflags", "+faststart", "-y", output); err != nil {
		return err
	}
	if err := runFFmpeg(commandCtx, s.ffmpegBin, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", output,
		"-vf", "thumbnail,scale=320:-2", "-frames:v", "1", "-y", thumbnail); err != nil {
		return err
	}
	metadata, err := probeVideo(commandCtx, s.ffprobeBin, output)
	if err != nil {
		return fmt.Errorf("inspect transcoded media: %w", err)
	}
	videoInfo, err := os.Stat(output)
	if err != nil || videoInfo.Size() == 0 {
		return errors.New("transcoded video is empty")
	}
	coverInfo, err := os.Stat(thumbnail)
	if err != nil || coverInfo.Size() == 0 {
		return errors.New("generated thumbnail is empty")
	}
	videoName, err := randomMediaName(".mp4")
	if err != nil {
		return err
	}
	coverName, err := randomMediaName(".jpg")
	if err != nil {
		return err
	}
	return s.finishProcessedVideoJob(ctx, job, output, thumbnail, videoName, coverName, videoInfo.Size(), coverInfo.Size(), metadata)
}

func (s *Server) finishProcessedVideoJob(ctx context.Context, job VideoProcessJob, output, thumbnail, videoName, coverName string, videoSize, coverSize int64, metadata videoMetadata) error {
	videoPath := filepath.Join(s.MediaDir, videoName)
	coverPath := filepath.Join(s.MediaDir, coverName)
	// PutObject can commit bytes and still return an error if its response is
	// lost. Register cleanup before the first write so that the normal error
	// path attempts to remove an unregistered object immediately. The orphan
	// reaper remains the fallback when storage is unavailable during cleanup.
	defer func() {
		if videoPath != "" {
			if s.objectMedia != nil {
				s.objectMedia.removeBestEffort("media/" + videoName)
			} else {
				_ = os.Remove(videoPath)
			}
		}
		if coverPath != "" {
			if s.objectMedia != nil {
				s.objectMedia.removeBestEffort("media/" + coverName)
			} else {
				_ = os.Remove(coverPath)
			}
		}
	}()
	if s.objectMedia != nil {
		if err := s.objectMedia.putFile(ctx, "media/"+videoName, output); err != nil {
			return err
		}
	} else if err := os.Rename(output, videoPath); err != nil {
		return err
	}
	if s.objectMedia != nil {
		if err := s.objectMedia.putFile(ctx, "media/"+coverName, thumbnail); err != nil {
			return err
		}
	} else if err := os.Rename(thumbnail, coverPath); err != nil {
		return err
	}
	published, coverUsed, err := s.completeVideoJob(job, "/media/"+videoName, "/media/"+coverName, videoSize, coverSize, metadata)
	if err != nil {
		return err
	}
	if published {
		videoPath = ""
		if coverUsed {
			coverPath = ""
		}
		s.invalidateFeed(context.Background())
	}
	return nil
}

func (s *Server) downloadObjectFile(ctx context.Context, key, path string) error {
	object, err := s.objectMedia.get(ctx, key)
	if err != nil {
		return err
	}
	defer object.Close()
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, object)
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func runFFmpeg(ctx context.Context, binary string, args ...string) error {
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 400 {
			message = message[:400]
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, message)
	}
	return nil
}

func randomMediaName(ext string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]) + ext, nil
}

func (s *Server) completeVideoJob(job VideoProcessJob, videoURL, coverURL string, videoSize, coverSize int64, metadata videoMetadata) (published, coverUsed bool, err error) {
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		res := tx.Model(&VideoProcessJob{}).Where("id = ? AND lease_until = ? AND done_at IS NULL AND dead_at IS NULL", job.ID, job.LeaseUntil).
			Updates(map[string]any{"done_at": now, "lease_until": nil, "last_error": "", "failure_code": ""})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("video processing lease lost")
		}
		var video Video
		if err := tx.First(&video, job.VideoID).Error; err != nil {
			return err
		}
		if video.Status == "deleted" || video.MediaURL != job.SourceURL || videoURL == "" {
			return nil
		}
		if err := tx.Create(&MediaAsset{OwnerID: video.AuthorID, URL: videoURL, Kind: "video", Size: videoSize}).Error; err != nil {
			return err
		}
		updates := map[string]any{"media_url": videoURL, "status": "published", "duration_ms": metadata.DurationMS, "width": metadata.Width, "height": metadata.Height}
		if video.CoverURL == "" {
			if err := tx.Create(&MediaAsset{OwnerID: video.AuthorID, URL: coverURL, Kind: "image", Size: coverSize}).Error; err != nil {
				return err
			}
			updates["cover_url"] = coverURL
			coverUsed = true
		}
		updated := tx.Model(&Video{}).Where("id = ? AND media_url = ? AND status = ?", video.ID, job.SourceURL, "processing").Updates(updates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("video source changed during processing")
		}
		published = true
		return nil
	})
	return published, coverUsed, err
}

func (s *Server) failVideoJob(job VideoProcessJob, cause error) {
	message := cause.Error()
	if len(message) > 500 {
		message = message[:500]
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		code := "processing_failed"
		var validation *videoValidationError
		if errors.As(cause, &validation) {
			code = validation.Code
		}
		updates := map[string]any{"last_error": message, "failure_code": code, "lease_until": nil}
		permanent := errors.Is(cause, errUnsupportedVideo)
		if permanent || job.Attempts >= videoProcessAttempts {
			updates["dead_at"] = now
		} else {
			updates["available_at"] = now.Add(time.Duration(job.Attempts*job.Attempts) * 5 * time.Second)
		}
		res := tx.Model(&VideoProcessJob{}).Where("id = ? AND lease_until = ? AND done_at IS NULL AND dead_at IS NULL", job.ID, job.LeaseUntil).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 1 && (permanent || job.Attempts >= videoProcessAttempts) {
			return tx.Model(&Video{}).Where("id = ? AND media_url = ? AND status = ?", job.VideoID, job.SourceURL, "processing").Update("status", "failed").Error
		}
		return nil
	}); err != nil {
		log.Printf("video processing failure update %d: %v", job.ID, err)
	}
}
