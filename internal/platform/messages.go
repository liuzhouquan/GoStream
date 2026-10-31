package platform

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (s *Server) pendingDirectMessages(userID uint, deviceID string) *gorm.DB {
	q := s.DB.Model(&ChatMessage{}).Where("chat_messages.recipient_id = ? AND chat_messages.acked_at IS NULL", userID)
	if deviceID != "" {
		q = q.Joins("LEFT JOIN message_device_acks ON message_device_acks.message_id = chat_messages.id AND message_device_acks.user_id = ? AND message_device_acks.device_id = ?", userID, deviceID).
			Where("message_device_acks.id IS NULL").
			Where("NOT EXISTS (SELECT 1 FROM message_device_ack_ranges r WHERE r.user_id = ? AND r.device_id = ? AND chat_messages.id BETWEEN r.first_message_id AND r.last_message_id)", userID, deviceID).
			Select("chat_messages.*")
	}
	return q
}

func (s *Server) history(c *gin.Context) {
	other, err := strconv.ParseUint(c.Query("user_id"), 10, 64)
	if err != nil || other == 0 || other == uint64(uid(c)) {
		respondError(c, 400, "user_id is required")
		return
	}
	before, err := strconv.ParseUint(c.DefaultQuery("before_id", "0"), 10, 64)
	if err != nil {
		respondError(c, 400, "invalid before_id")
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 1 || limit > 100 {
		respondError(c, 400, "limit must be 1-100")
		return
	}
	q := s.DB.Where("(sender_id = ? AND recipient_id = ?) OR (sender_id = ? AND recipient_id = ?)", uid(c), other, other, uid(c))
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	rows := make([]ChatMessage, 0)
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		respondError(c, 500, "could not load messages")
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	next := uint(0)
	if len(rows) > 0 {
		next = rows[len(rows)-1].ID
	}
	c.JSON(200, gin.H{"items": rows, "next_cursor": next, "has_more": more})
}

func (s *Server) unreadMessages(c *gin.Context) {
	deviceID, ok := s.requestMessageDevice(c)
	if !ok {
		return
	}
	after, err := strconv.ParseUint(c.DefaultQuery("after_id", "0"), 10, 64)
	if err != nil {
		respondError(c, 400, "invalid after_id")
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil || limit < 1 || limit > 100 {
		respondError(c, 400, "limit must be 1-100")
		return
	}
	rows := make([]ChatMessage, 0)
	q := s.pendingDirectMessages(uid(c), deviceID).Where("chat_messages.id > ?", after)
	if err := q.Order("chat_messages.id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
		respondError(c, 500, "could not load unread messages")
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	next := uint(0)
	if len(rows) > 0 {
		next = rows[len(rows)-1].ID
	}
	c.JSON(200, gin.H{"items": rows, "next_cursor": next, "has_more": more})
}

func (s *Server) ackMessage(c *gin.Context) {
	deviceID, ok := s.requestMessageDevice(c)
	if !ok {
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		respondError(c, http.StatusBadRequest, "invalid message ID")
		return
	}
	if deviceID != "" {
		var count int64
		if err := s.DB.Model(&ChatMessage{}).Where("id = ? AND recipient_id = ?", id, uid(c)).Count(&count).Error; err != nil {
			respondError(c, 500, "could not check message")
			return
		}
		if count == 0 {
			respondError(c, 404, "message not found")
			return
		}
		created, err := s.ackDeviceMessages(c.Request.Context(), uid(c), deviceID, []uint{uint(id)})
		if err != nil {
			if errors.Is(err, errDeviceLimit) {
				respondDeviceError(c, err)
				return
			}
			respondError(c, 500, "could not acknowledge message")
			return
		}
		c.JSON(200, gin.H{"acked": true, "acked_count": created})
		return
	}
	now := time.Now()
	res := s.DB.Model(&ChatMessage{}).Where("id = ? AND recipient_id = ? AND acked_at IS NULL", id, uid(c)).Update("acked_at", now)
	if res.Error != nil {
		respondError(c, 500, "could not acknowledge message")
		return
	}
	if res.RowsAffected == 0 {
		var msg ChatMessage
		if err := s.DB.Where("id = ? AND recipient_id = ?", id, uid(c)).First(&msg).Error; err != nil {
			respondError(c, 404, "message not found")
			return
		}
	}
	c.JSON(200, gin.H{"acked": true})
}

func (s *Server) ackMessages(c *gin.Context) {
	deviceID, ok := s.requestMessageDevice(c)
	if !ok {
		return
	}
	var in struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || len(in.IDs) == 0 || len(in.IDs) > 100 {
		respondError(c, http.StatusBadRequest, "ids must contain 1-100 message IDs")
		return
	}
	seen := make(map[uint]struct{}, len(in.IDs))
	for _, id := range in.IDs {
		if id == 0 {
			respondError(c, http.StatusBadRequest, "invalid message ID")
			return
		}
		if _, exists := seen[id]; exists {
			respondError(c, http.StatusBadRequest, "duplicate message ID")
			return
		}
		seen[id] = struct{}{}
	}
	var count int64
	if err := s.DB.Model(&ChatMessage{}).Where("id IN ? AND recipient_id = ?", in.IDs, uid(c)).Count(&count).Error; err != nil {
		respondError(c, http.StatusInternalServerError, "could not check messages")
		return
	}
	if count != int64(len(in.IDs)) {
		respondError(c, http.StatusNotFound, "message not found")
		return
	}
	if deviceID != "" {
		created, err := s.ackDeviceMessages(c.Request.Context(), uid(c), deviceID, in.IDs)
		if err != nil {
			if errors.Is(err, errDeviceLimit) {
				respondDeviceError(c, err)
				return
			}
			respondError(c, http.StatusInternalServerError, "could not acknowledge messages")
			return
		}
		c.JSON(http.StatusOK, gin.H{"acked": true, "acked_count": created})
		return
	}
	res := s.DB.Model(&ChatMessage{}).Where("id IN ? AND recipient_id = ? AND acked_at IS NULL", in.IDs, uid(c)).Update("acked_at", time.Now().UTC())
	if res.Error != nil {
		respondError(c, http.StatusInternalServerError, "could not acknowledge messages")
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": true, "acked_count": res.RowsAffected})
}

func (s *Server) ackDeviceMessages(ctx context.Context, userID uint, deviceID string, ids []uint) (int64, error) {
	var created int64
	err := transactionWithDeadlockRetry(ctx, s.DB, func(tx *gorm.DB) error {
		created = 0
		if err := ensureMessageDeviceTx(tx, userID, deviceID, time.Now().UTC()); err != nil {
			return err
		}
		var err error
		created, err = storeDeviceReceipts(tx, userID, deviceID, ids)
		return err
	})
	return created, err
}
