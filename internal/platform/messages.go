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
	var rows []ChatMessage
	s.DB.Where("(sender_id = ? AND recipient_id = ?) OR (sender_id = ? AND recipient_id = ?)", uid(c), other, other, uid(c)).Order("created_at DESC").Limit(100).Find(&rows)
	c.JSON(200, gin.H{"items": rows})
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
		respondError(c, 404, "message not found")
		return
	}
	c.JSON(200, gin.H{"acked": true})
}
