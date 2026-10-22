package platform

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const mediaOrphanGrace = 24 * time.Hour
const mediaReconcileBatch = 100

var generatedMediaName = regexp.MustCompile(`^[0-9a-f]{32}\.(mp4|mov|webm|jpg|jpeg|png|webp)$`)

type orphanCandidate struct {
	name    string
	url     string
	modTime time.Time
	size    int64
}

// ReconcileOrphanMedia removes old generated files that no database row references.
// A 24-hour grace period excludes uploads and transcodes still being committed.
func (s *Server) ReconcileOrphanMedia(ctx context.Context, now time.Time) (int, error) {
	entries, err := os.ReadDir(s.MediaDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-mediaOrphanGrace)
	removed := 0
	batch := make([]orphanCandidate, 0, mediaReconcileBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		count, err := s.removeUnreferencedMedia(ctx, batch, cutoff)
		removed += count
		batch = batch[:0]
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !generatedMediaName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		batch = append(batch, orphanCandidate{name: entry.Name(), url: "/media/" + entry.Name(), modTime: info.ModTime(), size: info.Size()})
		if len(batch) == mediaReconcileBatch {
			if err := flush(); err != nil {
				return removed, err
			}
		}
	}
	return removed, flush()
}

func (s *Server) removeUnreferencedMedia(ctx context.Context, batch []orphanCandidate, cutoff time.Time) (int, error) {
	urls := make([]string, len(batch))
	for i := range batch {
		urls[i] = batch[i].url
	}
	referenced := make(map[string]bool, len(batch))
	var assets []string
	if err := s.DB.WithContext(ctx).Model(&MediaAsset{}).Where("url IN ?", urls).Pluck("url", &assets).Error; err != nil {
		return 0, err
	}
	for _, url := range assets {
		referenced[url] = true
	}
	// Protect legacy videos whose files predate media_assets and deleted videos
	// whose URLs may still be needed for audit or a later restore.
	var videos []struct{ MediaURL, CoverURL string }
	if err := s.DB.WithContext(ctx).Model(&Video{}).Select("media_url", "cover_url").Where("media_url IN ? OR cover_url IN ?", urls, urls).Find(&videos).Error; err != nil {
		return 0, err
	}
	for _, video := range videos {
		referenced[video.MediaURL] = true
		referenced[video.CoverURL] = true
	}
	var jobSources []string
	if err := s.DB.WithContext(ctx).Model(&VideoProcessJob{}).Where("source_url IN ? AND done_at IS NULL AND dead_at IS NULL", urls).Pluck("source_url", &jobSources).Error; err != nil {
		return 0, err
	}
	for _, url := range jobSources {
		referenced[url] = true
	}
	var avatars []string
	if err := s.DB.WithContext(ctx).Model(&User{}).Where("avatar_url IN ?", urls).Pluck("avatar_url", &avatars).Error; err != nil {
		return 0, err
	}
	for _, url := range avatars {
		referenced[url] = true
	}
	removed := 0
	for _, candidate := range batch {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if referenced[candidate.url] {
			continue
		}
		path := filepath.Join(s.MediaDir, candidate.name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Equal(candidate.modTime) || info.Size() != candidate.size || !info.ModTime().Before(cutoff) {
			continue
		}
		err = os.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		if err == nil {
			removed++
		}
	}
	return removed, nil
}

func (s *Server) RunMediaReconciliation(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		removed, err := s.ReconcileOrphanMedia(ctx, time.Now().UTC())
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("media reconciliation failed: %v", err)
			s.metrics.mediaReconcile.WithLabelValues("error").Inc()
		} else if err == nil {
			s.metrics.mediaReconcile.WithLabelValues("removed").Add(float64(removed))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
