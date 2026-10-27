package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const outboxMaxAttempts = 8

type notificationPayload struct {
	NotificationID uint `json:"notification_id"`
}

// enqueueNotification must be called inside the transaction that commits its cause.
func enqueueNotification(tx *gorm.DB, notice Notification) error {
	notice.Text = notificationPreview(notice.Text)
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&notice)
	if res.Error != nil || res.RowsAffected == 0 {
		return res.Error
	}
	payload, err := json.Marshal(notificationPayload{NotificationID: notice.ID})
	if err != nil {
		return err
	}
	return tx.Create(&OutboxEvent{
		EventKey: fmt.Sprintf("notification:%d", notice.ID),
		Topic:    "notification.created", Payload: string(payload), AvailableAt: time.Now().UTC(),
	}).Error
}

func notificationPreview(value string) string {
	runes := []rune(value)
	if len(runes) > 280 {
		return string(runes[:280])
	}
	return value
}

// ProcessOutboxOnce atomically leases ready rows, then dispatches and acknowledges them.
// A crashed worker leaves the row eligible for another attempt after the lease expires.
func (s *Server) ProcessOutboxOnce(ctx context.Context) (int, error) {
	return s.processOutboxOnce(ctx, s.dispatchOutbox)
}

func (s *Server) processOutboxOnce(ctx context.Context, dispatch func(context.Context, OutboxEvent) error) (int, error) {
	now := time.Now().UTC()
	var candidates []OutboxEvent
	if err := s.DB.WithContext(ctx).Where("published_at IS NULL AND dead_at IS NULL AND available_at <= ?", now).
		Order("id ASC").Limit(32).Find(&candidates).Error; err != nil {
		return 0, err
	}
	processed := 0
	for _, item := range candidates {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		leaseUntil := time.Now().UTC().Add(30 * time.Second)
		claim := s.DB.WithContext(ctx).Model(&OutboxEvent{}).
			Where("id = ? AND attempts = ? AND published_at IS NULL AND dead_at IS NULL AND available_at <= ?", item.ID, item.Attempts, now).
			Updates(map[string]any{"available_at": leaseUntil, "attempts": gorm.Expr("attempts + 1")})
		if claim.Error != nil {
			return processed, claim.Error
		}
		if claim.RowsAffected == 0 {
			continue
		}
		s.metrics.outbox.WithLabelValues("claimed").Inc()
		item.Attempts++
		err := dispatch(ctx, item)
		finished := time.Now().UTC()
		// attempts is a monotonic lease generation. An expired worker cannot
		// acknowledge a later worker's claim or change a terminal state.
		final := s.DB.WithContext(ctx).Model(&OutboxEvent{}).
			Where("id = ? AND attempts = ? AND published_at IS NULL AND dead_at IS NULL AND available_at > ?", item.ID, item.Attempts, finished)
		if err == nil {
			result := final.Updates(map[string]any{"published_at": finished, "last_error": ""})
			if result.Error != nil {
				return processed, result.Error
			}
			if result.RowsAffected == 0 {
				s.metrics.outbox.WithLabelValues("stale").Inc()
			} else {
				s.metrics.outbox.WithLabelValues("dispatched").Inc()
			}
		} else {
			updates := map[string]any{"last_error": err.Error(), "available_at": finished.Add(retryDelay(item.Attempts))}
			if item.Attempts >= outboxMaxAttempts {
				updates["dead_at"] = finished
			}
			result := final.Updates(updates)
			if result.Error != nil {
				return processed, result.Error
			}
			if result.RowsAffected == 0 {
				s.metrics.outbox.WithLabelValues("stale").Inc()
			} else {
				s.metrics.outbox.WithLabelValues("failed").Inc()
				if item.Attempts >= outboxMaxAttempts {
					s.metrics.outbox.WithLabelValues("dead").Inc()
				}
				log.Printf("outbox event %d attempt %d failed: %v", item.ID, item.Attempts, err)
			}
		}
		processed++
	}
	return processed, nil
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Second * time.Duration(1<<uint(attempt-1))
}

func (s *Server) dispatchOutbox(ctx context.Context, item OutboxEvent) error {
	if item.Topic != "notification.created" {
		return fmt.Errorf("unsupported outbox topic %q", item.Topic)
	}
	var payload notificationPayload
	if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
		return err
	}
	if payload.NotificationID == 0 {
		return errors.New("missing notification_id")
	}
	var notice Notification
	if err := s.DB.WithContext(ctx).Preload("Actor").First(&notice, payload.NotificationID).Error; err != nil {
		return err
	}
	// The database inbox is durable. Socket delivery is an immediate convenience for online users.
	if err := s.Hub.send(notice.UserID, gin.H{"type": "notification", "notification": notice}); err != nil {
		log.Printf("local notification delivery failed for %d: %v", notice.ID, err)
	}
	return s.publishRealtime(ctx, realtimeEvent{Kind: "notification", UserID: notice.UserID, MessageID: notice.ID})
}

func (s *Server) RunOutbox(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := s.ProcessOutboxOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("outbox poll failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
