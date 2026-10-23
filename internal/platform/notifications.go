package platform

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) notifications(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "30"))
	if err != nil || limit < 1 || limit > 100 {
		respondError(c, http.StatusBadRequest, "limit must be 1-100")
		return
	}
	before, err := strconv.ParseUint(c.DefaultQuery("before_id", "0"), 10, strconv.IntSize)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid before_id")
		return
	}
	q := s.DB.WithContext(c.Request.Context()).Preload("Actor").Where("user_id = ?", uid(c))
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	rows := make([]Notification, 0)
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		respondError(c, http.StatusInternalServerError, "could not load notifications")
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
	c.JSON(http.StatusOK, gin.H{"items": rows, "next_cursor": next, "has_more": more})
}

func (s *Server) unread(c *gin.Context) {
	var n int64
	if err := s.DB.WithContext(c.Request.Context()).Model(&Notification{}).Where("user_id = ? AND read_at IS NULL", uid(c)).Count(&n).Error; err != nil {
		respondError(c, http.StatusInternalServerError, "could not count unread notifications")
		return
	}
	c.JSON(http.StatusOK, gin.H{"unread_count": n})
}

func (s *Server) markNotificationsRead(c *gin.Context) {
	q := s.DB.WithContext(c.Request.Context()).Model(&Notification{}).Where("user_id = ? AND read_at IS NULL", uid(c))
	if raw, exists := c.GetQuery("up_to_id"); exists {
		upTo, err := strconv.ParseUint(raw, 10, strconv.IntSize)
		if err != nil || upTo == 0 {
			respondError(c, http.StatusBadRequest, "invalid up_to_id")
			return
		}
		q = q.Where("id <= ?", upTo)
	}
	result := q.Update("read_at", time.Now().UTC())
	if result.Error != nil {
		respondError(c, http.StatusInternalServerError, "could not mark notifications read")
		return
	}
	c.JSON(http.StatusOK, gin.H{"read": true, "read_count": result.RowsAffected})
}
