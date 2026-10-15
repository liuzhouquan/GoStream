package platform

import (
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) notifications(c *gin.Context) {
	var rows []Notification
	s.DB.Preload("Actor").Where("user_id = ?", uid(c)).Order("created_at DESC").Limit(50).Find(&rows)
	c.JSON(200, gin.H{"items": rows})
}

func (s *Server) unread(c *gin.Context) {
	var n int64
	s.DB.Model(&Notification{}).Where("user_id = ? AND read_at IS NULL", uid(c)).Count(&n)
	c.JSON(200, gin.H{"unread_count": n})
}

func (s *Server) markNotificationsRead(c *gin.Context) {
	now := time.Now()
	s.DB.Model(&Notification{}).Where("user_id = ? AND read_at IS NULL", uid(c)).Update("read_at", now)
	c.JSON(200, gin.H{"read": true})
}
