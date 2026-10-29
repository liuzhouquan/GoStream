package platform

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errGroupFull = errors.New("group member limit reached")

func groupID(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		respondError(c, 400, "invalid group id")
		return 0, false
	}
	return uint(n), true
}

func respondGroupLookupError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		respondError(c, 404, "group not found")
		return
	}
	log.Printf("load group membership failed: %v", err)
	respondError(c, 500, "could not load group")
}

func (s *Server) memberGroup(c *gin.Context) (ChatGroup, GroupMember, bool) {
	id, ok := groupID(c)
	if !ok {
		return ChatGroup{}, GroupMember{}, false
	}
	var group ChatGroup
	var member GroupMember
	if err := s.DB.First(&group, id).Error; err != nil {
		respondGroupLookupError(c, err)
		return ChatGroup{}, GroupMember{}, false
	}
	if err := s.DB.Where("group_id = ? AND user_id = ?", id, uid(c)).First(&member).Error; err != nil {
		respondGroupLookupError(c, err)
		return ChatGroup{}, GroupMember{}, false
	}
	return group, member, true
}

func (s *Server) createGroup(c *gin.Context) {
	var in struct {
		Name string `json:"name"`
	}
	if c.ShouldBindJSON(&in) != nil {
		respondError(c, 400, "invalid JSON")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 80 {
		respondError(c, 400, "group name must be 1-80 bytes")
		return
	}
	group := ChatGroup{OwnerID: uid(c), Name: in.Name}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&group).Error; err != nil {
			return err
		}
		if err := tx.Create(&GroupMember{GroupID: group.ID, UserID: uid(c), JoinedAt: time.Now().UTC()}).Error; err != nil {
			return err
		}
		return tx.Create(&GroupRead{GroupID: group.ID, UserID: uid(c)}).Error
	}); err != nil {
		respondError(c, 500, "could not create group")
		return
	}
	c.JSON(http.StatusCreated, group)
}

func (s *Server) listGroups(c *gin.Context) {
	rows := make([]ChatGroup, 0)
	if err := s.DB.Joins("JOIN group_members ON group_members.group_id = chat_groups.id").
		Where("group_members.user_id = ?", uid(c)).Order("chat_groups.created_at DESC, chat_groups.id DESC").Limit(100).Find(&rows).Error; err != nil {
		respondError(c, 500, "could not load groups")
		return
	}
	c.JSON(200, gin.H{"items": rows})
}

func (s *Server) addGroupMember(c *gin.Context) {
	group, _, ok := s.memberGroup(c)
	if !ok {
		return
	}
	if group.OwnerID != uid(c) {
		respondError(c, 403, "only the group owner can add members")
		return
	}
	var in struct {
		UserID uint `json:"user_id"`
	}
	if c.ShouldBindJSON(&in) != nil || in.UserID == 0 {
		respondError(c, 400, "user_id is required")
		return
	}
	var user User
	if s.DB.Select("id").First(&user, in.UserID).Error != nil {
		respondError(c, 404, "user not found")
		return
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		// Every invitation locks the same group row before counting. A later
		// invitation sees the preceding transaction's committed member.
		var locked ChatGroup
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&locked, group.ID).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&GroupMember{}).Where("group_id = ?", group.ID).Count(&count).Error; err != nil {
			return err
		}
		if count >= 200 {
			var existing GroupMember
			result := tx.Select("id").Where("group_id = ? AND user_id = ?", group.ID, in.UserID).Limit(1).Find(&existing)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected > 0 {
				return nil
			}
			return errGroupFull
		}
		var last uint
		if err := tx.Model(&GroupMessage{}).Where("group_id = ?", group.ID).Select("COALESCE(MAX(id), 0)").Scan(&last).Error; err != nil {
			return err
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&GroupMember{GroupID: group.ID, UserID: in.UserID, JoinedAt: time.Now().UTC()})
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&GroupRead{GroupID: group.ID, UserID: in.UserID, LastAckID: last}).Error; err != nil {
			return err
		}
		return enqueueNotification(tx, Notification{UserID: in.UserID, EventKey: fmt.Sprintf("groupinvite:%d:%d", group.ID, in.UserID), Kind: "group", ActorID: uid(c), Text: "added you to a group"})
	}); err != nil {
		if errors.Is(err, errGroupFull) {
			respondError(c, 409, "group has reached its 200-member limit")
			return
		}
		respondError(c, 500, "could not add group member")
		return
	}
	c.JSON(200, gin.H{"member": true})
}

func (s *Server) removeGroupMember(c *gin.Context) {
	group, _, ok := s.memberGroup(c)
	if !ok {
		return
	}
	target, err := strconv.ParseUint(c.Param("user_id"), 10, 32)
	if err != nil || target == 0 {
		respondError(c, 400, "invalid user id")
		return
	}
	if uint(target) == group.OwnerID {
		respondError(c, 400, "group owner cannot leave or be removed")
		return
	}
	if uid(c) != group.OwnerID && uid(c) != uint(target) {
		respondError(c, 403, "cannot remove another member")
		return
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		var locked ChatGroup
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&locked, group.ID).Error; err != nil {
			return err
		}
		if err := tx.Where("group_id = ? AND user_id = ?", group.ID, target).Delete(&GroupMember{}).Error; err != nil {
			return err
		}
		if err := tx.Where("group_id = ? AND user_id = ?", group.ID, target).Delete(&GroupDeviceRead{}).Error; err != nil {
			return err
		}
		return tx.Where("group_id = ? AND user_id = ?", group.ID, target).Delete(&GroupRead{}).Error
	}); err != nil {
		respondError(c, 500, "could not remove group member")
		return
	}
	c.JSON(200, gin.H{"member": false})
}

func (s *Server) groupHistory(c *gin.Context) {
	group, _, ok := s.memberGroup(c)
	if !ok {
		return
	}
	before, err := strconv.ParseUint(c.DefaultQuery("before_id", "0"), 10, 32)
	if err != nil {
		respondError(c, 400, "invalid before_id")
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 1 || limit > 100 {
		respondError(c, 400, "limit must be 1-100")
		return
	}
	q := s.DB.Where("group_id = ?", group.ID)
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	rows := make([]GroupMessage, 0)
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		respondError(c, 500, "could not load group messages")
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

func (s *Server) groupUnread(c *gin.Context) {
	deviceID, valid := s.requestMessageDevice(c)
	if !valid {
		return
	}
	group, member, ok := s.memberGroup(c)
	if !ok {
		return
	}
	var read GroupRead
	if err := s.DB.Where("group_id = ? AND user_id = ?", group.ID, member.UserID).First(&read).Error; err != nil {
		respondError(c, 500, "could not load read cursor")
		return
	}
	lastAckID := read.LastAckID
	if deviceID != "" {
		var deviceRead GroupDeviceRead
		err := s.DB.Where("group_id = ? AND user_id = ? AND device_id = ?", group.ID, member.UserID, deviceID).First(&deviceRead).Error
		if err == nil {
			lastAckID = deviceRead.LastAckID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			respondError(c, 500, "could not load device read cursor")
			return
		}
	}
	var count int64
	if err := s.DB.Model(&GroupMessage{}).Where("group_id = ? AND id > ? AND sender_id <> ?", group.ID, lastAckID, uid(c)).Count(&count).Error; err != nil {
		respondError(c, 500, "could not count unread messages")
		return
	}
	c.JSON(200, gin.H{"unread_count": count, "last_ack_id": lastAckID})
}

func (s *Server) ackGroupMessage(c *gin.Context) {
	deviceID, valid := s.requestMessageDevice(c)
	if !valid {
		return
	}
	group, _, ok := s.memberGroup(c)
	if !ok {
		return
	}
	messageID, err := strconv.ParseUint(c.Param("message_id"), 10, 32)
	if err != nil || messageID == 0 {
		respondError(c, 400, "invalid message id")
		return
	}
	var msg GroupMessage
	if err := s.DB.Where("id = ? AND group_id = ?", messageID, group.ID).First(&msg).Error; err != nil {
		respondError(c, 404, "group message not found")
		return
	}
	if deviceID != "" {
		err := s.DB.Transaction(func(tx *gorm.DB) error {
			var membership GroupMember
			if err := tx.Where("group_id = ? AND user_id = ?", group.ID, uid(c)).First(&membership).Error; err != nil {
				return err
			}
			var shared GroupRead
			if err := tx.Where("group_id = ? AND user_id = ?", group.ID, uid(c)).First(&shared).Error; err != nil {
				return err
			}
			initial := shared.LastAckID
			if msg.ID > initial {
				initial = msg.ID
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&GroupDeviceRead{GroupID: group.ID, UserID: uid(c), DeviceID: deviceID, LastAckID: initial}).Error; err != nil {
				return err
			}
			return tx.Model(&GroupDeviceRead{}).Where("group_id = ? AND user_id = ? AND device_id = ?", group.ID, uid(c), deviceID).
				UpdateColumn("last_ack_id", gorm.Expr("CASE WHEN last_ack_id < ? THEN ? ELSE last_ack_id END", msg.ID, msg.ID)).Error
		})
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				respondError(c, 404, "group membership not found")
				return
			}
			respondError(c, 500, "could not acknowledge group message")
			return
		}
		var deviceRead GroupDeviceRead
		if err := s.DB.Where("group_id = ? AND user_id = ? AND device_id = ?", group.ID, uid(c), deviceID).First(&deviceRead).Error; err != nil {
			respondError(c, 500, "could not load device read cursor")
			return
		}
		c.JSON(200, gin.H{"acked": true, "last_ack_id": deviceRead.LastAckID})
		return
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var membership GroupMember
		if err := tx.Where("group_id = ? AND user_id = ?", group.ID, uid(c)).First(&membership).Error; err != nil {
			return err
		}
		return tx.Model(&GroupRead{}).Where("group_id = ? AND user_id = ?", group.ID, uid(c)).
			UpdateColumn("last_ack_id", gorm.Expr("CASE WHEN last_ack_id < ? THEN ? ELSE last_ack_id END", msg.ID, msg.ID)).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respondError(c, 404, "group membership not found")
			return
		}
		respondError(c, 500, "could not acknowledge group message")
		return
	}
	var read GroupRead
	if err := s.DB.Where("group_id = ? AND user_id = ?", group.ID, uid(c)).First(&read).Error; err != nil {
		respondError(c, 404, "group membership not found")
		return
	}
	c.JSON(200, gin.H{"acked": true, "last_ack_id": read.LastAckID})
}

func (s *Server) sendGroupMessage(userID uint, client *hubClient, e socketEvent) {
	if e.GroupID == 0 || e.ClientID == "" || len(e.ClientID) > 80 || strings.TrimSpace(e.Body) == "" || len(e.Body) > 4000 {
		_ = client.send(gin.H{"type": "error", "message": "invalid group message"})
		return
	}
	var membership GroupMember
	if err := s.DB.Where("group_id = ? AND user_id = ?", e.GroupID, userID).First(&membership).Error; err != nil {
		_ = client.send(gin.H{"type": "error", "message": "group not found"})
		return
	}
	msg := GroupMessage{GroupID: e.GroupID, SenderID: userID, ClientID: e.ClientID, Body: e.Body}
	created, conflict := false, false
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// Allocate IDs and serialize against invitations and member removal
		// using the same group row lock.
		// A later same-group message must not commit ahead of an earlier insert:
		// cumulative ACKs would otherwise hide the earlier message on commit.
		// Re-check membership under this lock so in-flight sends fail if member removal committed.
		var group ChatGroup
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&group, e.GroupID).Error; err != nil {
			return err
		}
		var currentMember GroupMember
		if err := tx.Where("group_id = ? AND user_id = ?", e.GroupID, userID).First(&currentMember).Error; err != nil {
			return err
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&msg)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			if err := tx.Where("sender_id = ? AND client_id = ?", userID, e.ClientID).First(&msg).Error; err != nil {
				return err
			}
			conflict = msg.GroupID != e.GroupID || msg.Body != e.Body
			return nil
		}
		created = true
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			_ = client.send(gin.H{"type": "error", "message": "group not found"})
			return
		}
		_ = client.send(gin.H{"type": "error", "message": "group message could not be saved"})
		return
	}
	if conflict {
		_ = client.send(gin.H{"type": "error", "message": "client_id already used for another group message"})
		return
	}
	_ = client.send(gin.H{"type": "ack", "client_id": msg.ClientID, "message_id": msg.ID, "group_id": msg.GroupID})
	if !created {
		return
	}
	_ = s.Hub.sendExcept(userID, client, gin.H{"type": "group_message", "message": msg})
	var members []GroupMember
	if err := s.DB.Where("group_id = ? AND user_id <> ?", e.GroupID, userID).Find(&members).Error; err != nil {
		return
	}
	for _, member := range members {
		_ = s.Hub.send(member.UserID, gin.H{"type": "group_message", "message": msg})
	}
	if err := s.publishRealtime(context.Background(), realtimeEvent{Kind: "group", GroupID: msg.GroupID, MessageID: msg.ID, SenderID: msg.SenderID}); err != nil {
		log.Printf("realtime group publish failed for message %d: %v", msg.ID, err)
	}
}
