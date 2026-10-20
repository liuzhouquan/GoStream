package platform

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) history(c *gin.Context) {
	other, err := strconv.ParseUint(c.Query("user_id"), 10, 32)
	if err != nil || other == 0 {
		respondError(c, 400, "user_id is required")
		return
	}
	rows := make([]ChatMessage, 0)
	s.DB.Where("(sender_id = ? AND recipient_id = ?) OR (sender_id = ? AND recipient_id = ?)", uid(c), other, other, uid(c)).Order("created_at DESC").Limit(100).Find(&rows)
	c.JSON(200, gin.H{"items": rows})
}

func (s *Server) unreadMessages(c *gin.Context) {
	after, err := strconv.ParseUint(c.DefaultQuery("after_id", "0"), 10, 32)
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
	if err := s.DB.Where("recipient_id = ? AND acked_at IS NULL AND id > ?", uid(c), after).Order("id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
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
	id := c.Param("id")
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
