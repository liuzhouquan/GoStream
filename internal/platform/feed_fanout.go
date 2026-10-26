package platform

import (
	"context"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const feedFanoutBatchSize = 128

// Zero keeps every author on the SQL pull path and is the current default.
func (s *Server) UseFeedFanout(maxFollowers int) { s.fanoutMaxFollowers = maxFollowers }

// The job is inserted in the same transaction that makes the video public.
// Until it completes, the read path includes the author's video table.
func (s *Server) enqueueFeedFanout(tx *gorm.DB, video Video) error {
	if s.fanoutMaxFollowers <= 0 {
		return nil
	}
	// A high-follower author needs only a threshold decision, not an exact
	// count over the full relationship index on every publication.
	var followers int64
	if err := tx.Raw("SELECT count(*) FROM (SELECT follower_id FROM follows WHERE followee_id = ? LIMIT ?) AS sample", video.AuthorID, s.fanoutMaxFollowers+1).Scan(&followers).Error; err != nil {
		return err
	}
	if followers > int64(s.fanoutMaxFollowers) {
		return nil
	}
	return tx.Create(&FeedFanoutJob{VideoID: video.ID, AuthorID: video.AuthorID, AvailableAt: time.Now().UTC()}).Error
}

// One batch is leased with a compare-and-swap. An expired lease can be taken
// over by another API instance; inbox uniqueness makes retries idempotent.
func (s *Server) ProcessFeedFanoutOnce(ctx context.Context) (bool, error) {
	now := time.Now().UTC()
	pending := "done_at IS NULL AND available_at <= ? AND (lease_until IS NULL OR lease_until < ?)"
	var candidates []FeedFanoutJob
	if err := s.DB.WithContext(ctx).Where(pending, now, now).Order("available_at ASC").Order("id ASC").Limit(8).Find(&candidates).Error; err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		leaseUntil := now.Add(time.Minute).Truncate(time.Millisecond)
		claim := s.DB.WithContext(ctx).Model(&FeedFanoutJob{}).Where("id = ? AND "+pending, candidate.ID, now, now).
			Updates(map[string]any{"lease_until": leaseUntil, "attempts": gorm.Expr("attempts + 1")})
		if claim.Error != nil {
			return false, claim.Error
		}
		if claim.RowsAffected == 0 {
			continue
		}
		candidate.LeaseUntil = &leaseUntil
		candidate.Attempts++
		completed, err := s.processFeedFanoutBatch(ctx, candidate)
		if err != nil {
			s.metrics.feedFanout.WithLabelValues("error").Inc()
			delay := time.Duration(1<<min(candidate.Attempts, 6)) * time.Second
			message := err.Error()
			if len(message) > 500 {
				message = message[:500]
			}
			_ = s.DB.WithContext(ctx).Model(&FeedFanoutJob{}).
				Where("id = ? AND lease_until = ? AND done_at IS NULL", candidate.ID, leaseUntil).
				Updates(map[string]any{"lease_until": nil, "available_at": time.Now().UTC().Add(delay), "last_error": message}).Error
			return true, err
		}
		if completed {
			s.metrics.feedFanout.WithLabelValues("completed").Inc()
			if !candidate.CreatedAt.IsZero() {
				s.metrics.feedFanoutAge.Observe(time.Since(candidate.CreatedAt).Seconds())
			}
		} else {
			s.metrics.feedFanout.WithLabelValues("batch").Inc()
		}
		return true, nil
	}
	return false, nil
}

func (s *Server) processFeedFanoutBatch(ctx context.Context, job FeedFanoutJob) (bool, error) {
	completed := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var followers []Follow
		if err := tx.Select("follower_id").Where("followee_id = ? AND follower_id > ?", job.AuthorID, job.LastFollowerID).
			Order("follower_id ASC").Limit(feedFanoutBatchSize).Find(&followers).Error; err != nil {
			return err
		}
		updates := map[string]any{"lease_until": nil, "last_error": ""}
		if len(followers) == 0 {
			updates["done_at"] = time.Now().UTC()
			completed = true
		} else {
			var video Video
			if err := tx.Select("created_at").Where("id = ?", job.VideoID).First(&video).Error; err != nil {
				return err
			}
			inboxes := make([]FeedInbox, 0, len(followers))
			for _, follow := range followers {
				inboxes = append(inboxes, FeedInbox{UserID: follow.FollowerID, VideoID: job.VideoID, CreatedAt: video.CreatedAt})
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&inboxes).Error; err != nil {
				return err
			}
			updates["last_follower_id"] = followers[len(followers)-1].FollowerID
			// Move a long job behind other ready jobs after each batch.
			updates["available_at"] = time.Now().UTC()
		}
		res := tx.Model(&FeedFanoutJob{}).Where("id = ? AND lease_until = ? AND done_at IS NULL", job.ID, job.LeaseUntil).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("feed fanout lease lost for job %d", job.ID)
		}
		return nil
	})
	return completed && err == nil, err
}

func (s *Server) RunFeedFanout(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastPendingSample := time.Time{}
	for ctx.Err() == nil {
		if time.Since(lastPendingSample) >= 30*time.Second {
			var pending int64
			if err := s.DB.WithContext(ctx).Model(&FeedFanoutJob{}).Where("done_at IS NULL").Count(&pending).Error; err == nil {
				s.metrics.feedFanoutJobs.Set(float64(pending))
			} else if ctx.Err() == nil {
				log.Printf("feed fanout pending count: %v", err)
			}
			lastPendingSample = time.Now()
		}
		worked, err := s.ProcessFeedFanoutOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("feed fanout: %v", err)
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
