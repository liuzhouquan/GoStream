package platform

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const deviceIDHeader = "X-GoStream-Device-ID"
const deviceProtocolPrefix = "gostream.device."
const maxMessageDevices = 32
const deviceTouchInterval = time.Minute
const deviceEvictionAge = 30 * 24 * time.Hour
const deviceExpirationAge = 90 * 24 * time.Hour

var errDeviceLimit = errors.New("too many active message devices")

func normalizeDeviceID(value string) (string, bool) {
	if len(value) != 36 {
		return "", false
	}
	for i := 0; i < len(value); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return "", false
			}
			continue
		}
		if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f') || (value[i] >= 'A' && value[i] <= 'F')) {
			return "", false
		}
	}
	return strings.ToLower(value), true
}

// requestDeviceID returns an empty ID for legacy clients that omit the header.
func requestDeviceID(c *gin.Context) (string, bool) {
	raw := c.GetHeader(deviceIDHeader)
	if raw == "" {
		return "", true
	}
	id, ok := normalizeDeviceID(raw)
	if !ok {
		respondError(c, http.StatusBadRequest, "invalid device ID")
	}
	return id, ok
}

func (s *Server) requestMessageDevice(c *gin.Context) (string, bool) {
	id, ok := requestDeviceID(c)
	if !ok || id == "" {
		return id, ok
	}
	if err := s.ensureMessageDevice(c.Request.Context(), uid(c), id); err != nil {
		respondDeviceError(c, err)
		return "", false
	}
	return id, true
}

func respondDeviceError(c *gin.Context, err error) {
	if errors.Is(err, errDeviceLimit) {
		respondError(c, http.StatusTooManyRequests, "too many active message devices; retry after an older device expires")
		return
	}
	log.Printf("message device registration failed: %v", err)
	respondError(c, http.StatusInternalServerError, "could not register message device")
}

// Device registration serializes new IDs on the user row across API instances.
// Existing devices use an indexed lookup and refresh their inactivity clock at
// most once per minute.
func (s *Server) ensureMessageDevice(ctx context.Context, userID uint, deviceID string) error {
	now := time.Now().UTC()
	var device MessageDevice
	err := s.DB.WithContext(ctx).Select("id", "last_seen_at").Where("user_id = ? AND device_id = ?", userID, deviceID).Take(&device).Error
	if err == nil {
		if !device.LastSeenAt.Before(now.Add(-deviceTouchInterval)) {
			return nil
		}
		updated := s.DB.WithContext(ctx).Model(&MessageDevice{}).Where("id = ?", device.ID).UpdateColumn("last_seen_at", now)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected > 0 {
			return nil
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&user, userID).Error; err != nil {
			return err
		}
		var existing MessageDevice
		err := tx.Where("user_id = ? AND device_id = ?", userID, deviceID).Take(&existing).Error
		if err == nil {
			return tx.Model(&existing).UpdateColumn("last_seen_at", now).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var count int64
		if err := tx.Model(&MessageDevice{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
			return err
		}
		if count >= maxMessageDevices {
			var oldest MessageDevice
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ?", userID).Order("last_seen_at ASC, id ASC").First(&oldest).Error; err != nil {
				return err
			}
			if !oldest.LastSeenAt.Before(now.Add(-deviceEvictionAge)) {
				return errDeviceLimit
			}
			if err := deleteMessageDeviceState(tx, oldest); err != nil {
				return err
			}
		}
		return tx.Create(&MessageDevice{UserID: userID, DeviceID: deviceID, CreatedAt: now, LastSeenAt: now}).Error
	})
}

func deleteMessageDeviceState(tx *gorm.DB, device MessageDevice) error {
	if err := tx.Where("user_id = ? AND device_id = ?", device.UserID, device.DeviceID).Delete(&MessageDeviceAck{}).Error; err != nil {
		return err
	}
	if err := tx.Where("user_id = ? AND device_id = ?", device.UserID, device.DeviceID).Delete(&GroupDeviceRead{}).Error; err != nil {
		return err
	}
	return tx.Delete(&device).Error
}

// BackfillMessageDevices preserves read state written before the registry was
// introduced. The first migration treats those devices as recently active.
func (s *Server) BackfillMessageDevices(ctx context.Context) error {
	const migrationKey = "message_devices_v1"
	var applied int64
	if err := s.DB.WithContext(ctx).Model(&SchemaMigration{}).Where(&SchemaMigration{Key: migrationKey}).Count(&applied).Error; err != nil {
		return err
	}
	if applied > 0 {
		return nil
	}
	for _, table := range []string{"message_device_acks", "group_device_reads"} {
		var lastID uint
		for {
			var rows []struct {
				ID       uint
				UserID   uint
				DeviceID string
			}
			if err := s.DB.WithContext(ctx).Table(table).Select("id, user_id, device_id").Where("id > ?", lastID).
				Order("id ASC").Limit(500).Scan(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			now := time.Now().UTC()
			devices := make([]MessageDevice, 0, len(rows))
			seen := make(map[struct {
				UserID   uint
				DeviceID string
			}]struct{}, len(rows))
			for _, row := range rows {
				lastID = row.ID
				key := struct {
					UserID   uint
					DeviceID string
				}{row.UserID, row.DeviceID}
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				devices = append(devices, MessageDevice{UserID: row.UserID, DeviceID: row.DeviceID, CreatedAt: now, LastSeenAt: now})
			}
			if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&devices, 500).Error; err != nil {
				return err
			}
		}
	}
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&SchemaMigration{Key: migrationKey, AppliedAt: time.Now().UTC()}).Error
}

// Expiry removes only device-specific read state. Unacknowledged messages stay
// in SQL and can replay after a long-idle device reconnects.
func (s *Server) cleanupExpiredMessageDevices(ctx context.Context, now time.Time, limit int) (int, error) {
	cutoff := now.UTC().Add(-deviceExpirationAge)
	var candidates []MessageDevice
	if err := s.DB.WithContext(ctx).Where("last_seen_at < ?", cutoff).Order("last_seen_at ASC, id ASC").Limit(limit).Find(&candidates).Error; err != nil {
		return 0, err
	}
	removed := 0
	for _, candidate := range candidates {
		deleted := false
		err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var user User
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&user, candidate.UserID).Error; err != nil {
				return err
			}
			var current MessageDevice
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", candidate.ID).Take(&current).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if !current.LastSeenAt.Before(cutoff) {
				return nil
			}
			if err := deleteMessageDeviceState(tx, current); err != nil {
				return err
			}
			deleted = true
			return nil
		})
		if err != nil {
			return removed, err
		}
		if deleted {
			removed++
		}
	}
	return removed, nil
}

func (s *Server) RunMessageDeviceCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		for batch := 0; batch < 10; batch++ {
			removed, err := s.cleanupExpiredMessageDevices(ctx, time.Now(), 100)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Printf("message device cleanup failed: %v", err)
				}
				break
			}
			if removed < 100 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
