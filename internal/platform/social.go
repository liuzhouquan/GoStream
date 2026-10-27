package platform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Server) follow(c *gin.Context) {
	target, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || uint(target) == uid(c) {
		respondError(c, 400, "invalid follow target")
		return
	}
	var u User
	if s.DB.First(&u, target).Error != nil {
		respondError(c, 404, "user not found")
		return
	}
	f := Follow{FollowerID: uid(c), FolloweeID: uint(target)}
	key := fmt.Sprintf("follow:%d:%d", f.FollowerID, f.FolloweeID)
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&f).Error; err != nil {
			return err
		}
		return enqueueNotification(tx, Notification{UserID: f.FolloweeID, EventKey: key, Kind: "follow", ActorID: f.FollowerID, Text: "started following you"})
	}); err != nil {
		respondError(c, 500, "could not follow user")
		return
	}
	s.invalidateFollowing(c.Request.Context(), uid(c))
	c.JSON(200, gin.H{"following": true})
}

func (s *Server) unfollow(c *gin.Context) {
	if err := s.DB.Where("follower_id = ? AND followee_id = ?", uid(c), c.Param("id")).Delete(&Follow{}).Error; err != nil {
		respondError(c, 500, "could not unfollow user")
		return
	}
	s.invalidateFollowing(c.Request.Context(), uid(c))
	c.JSON(200, gin.H{"following": false})
}

func (s *Server) following(c *gin.Context) { s.listRelations(c, "follower_id") }

func (s *Server) followers(c *gin.Context) { s.listRelations(c, "followee_id") }

func (s *Server) listRelations(c *gin.Context, field string) {
	var rows []Follow
	if err := s.DB.Where(field+" = ?", uid(c)).Order("created_at DESC").Limit(100).Find(&rows).Error; err != nil {
		respondError(c, 500, "could not load relationships")
		return
	}
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		if field == "follower_id" {
			ids = append(ids, r.FolloweeID)
		} else {
			ids = append(ids, r.FollowerID)
		}
	}
	var users []User
	if len(ids) > 0 {
		if err := s.DB.Where("id IN ?", ids).Find(&users).Error; err != nil {
			respondError(c, 500, "could not load related users")
			return
		}
	}
	c.JSON(200, gin.H{"items": users})
}

func (s *Server) action(kind string, active bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var v Video
		if s.DB.First(&v, c.Param("id")).Error != nil || v.Status != "published" {
			respondError(c, 404, "video not found")
			return
		}
		var changed bool
		err := transactionWithDeadlockRetry(c.Request.Context(), s.DB, func(tx *gorm.DB) error {
			changed = false
			if active {
				res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&VideoAction{UserID: uid(c), VideoID: v.ID, Kind: kind})
				if res.Error != nil {
					return res.Error
				}
				changed = res.RowsAffected == 1
			} else {
				res := tx.Where("user_id = ? AND video_id = ? AND kind = ?", uid(c), v.ID, kind).Delete(&VideoAction{})
				if res.Error != nil {
					return res.Error
				}
				changed = res.RowsAffected == 1
			}
			if !changed {
				return nil
			}
			column := "like_count"
			if kind == "favorite" {
				column = "favorite_count"
			}
			expression := gorm.Expr(column + " + 1")
			if !active {
				expression = gorm.Expr("CASE WHEN " + column + " > 0 THEN " + column + " - 1 ELSE 0 END")
			}
			updated := tx.Model(&Video{}).Where("id = ? AND status = ?", v.ID, "published").UpdateColumn(column, expression)
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
			if active && v.AuthorID != uid(c) {
				key := fmt.Sprintf("%s:%d:%d", kind, v.ID, uid(c))
				return enqueueNotification(tx, Notification{UserID: v.AuthorID, EventKey: key, Kind: kind, ActorID: uid(c), VideoID: v.ID, Text: "interacted with your video"})
			}
			return nil
		})
		if err != nil {
			respondError(c, 500, "could not update interaction")
			return
		}
		if changed {
			s.invalidateFeed(c.Request.Context())
		}
		c.JSON(200, gin.H{"active": active})
	}
}

func (s *Server) createComment(c *gin.Context) {
	var in struct {
		Body     string `json:"body"`
		ParentID *uint  `json:"parent_id"`
	}
	if c.ShouldBindJSON(&in) != nil {
		respondError(c, 400, "invalid JSON")
		return
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" || len(in.Body) > 2000 {
		respondError(c, 400, "comment must be 1-2000 characters")
		return
	}
	var v Video
	if s.DB.First(&v, c.Param("id")).Error != nil || v.Status != "published" {
		respondError(c, 404, "video not found")
		return
	}
	var parent Comment
	if in.ParentID != nil {
		if s.DB.Where("id = ? AND video_id = ?", *in.ParentID, v.ID).First(&parent).Error != nil {
			respondError(c, 404, "parent comment not found")
			return
		}
		if parent.ParentID != nil {
			respondError(c, 400, "replies must target a top-level comment")
			return
		}
	}
	cm := Comment{VideoID: v.ID, UserID: uid(c), ParentID: in.ParentID, Body: in.Body}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&cm).Error; err != nil {
			return err
		}
		updated := tx.Model(&Video{}).Where("id = ? AND status = ?", v.ID, "published").UpdateColumn("comment_count", gorm.Expr("comment_count + 1"))
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		if v.AuthorID != uid(c) {
			key := fmt.Sprintf("comment:%d", cm.ID)
			if err := enqueueNotification(tx, Notification{UserID: v.AuthorID, EventKey: key, Kind: "comment", ActorID: uid(c), VideoID: v.ID, Text: in.Body}); err != nil {
				return err
			}
		}
		if in.ParentID != nil && parent.UserID != uid(c) && parent.UserID != v.AuthorID {
			key := fmt.Sprintf("reply:%d", cm.ID)
			if err := enqueueNotification(tx, Notification{UserID: parent.UserID, EventKey: key, Kind: "reply", ActorID: uid(c), VideoID: v.ID, Text: in.Body}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		respondError(c, 500, "could not create comment")
		return
	}
	s.invalidateFeed(c.Request.Context())
	s.DB.Preload("User").First(&cm, cm.ID)
	c.JSON(201, cm)
}

type commentCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uint      `json:"id"`
	VideoID   uint      `json:"video_id"`
	ParentID  *uint     `json:"parent_id"`
}

func parseCommentPage(c *gin.Context, videoID uint, parentID *uint) (int, *commentCursor, bool) {
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 50 {
			respondError(c, 400, "limit must be 1-50")
			return 0, nil, false
		}
		limit = value
	}
	raw := c.Query("cursor")
	if raw == "" {
		return limit, nil, true
	}
	if len(raw) > 256 {
		respondError(c, 400, "invalid comment cursor")
		return 0, nil, false
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	var cursor commentCursor
	if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.ID == 0 || cursor.CreatedAt.IsZero() || cursor.VideoID != videoID || (cursor.ParentID == nil) != (parentID == nil) || (parentID != nil && *cursor.ParentID != *parentID) {
		respondError(c, 400, "invalid comment cursor")
		return 0, nil, false
	}
	return limit, &cursor, true
}

func (s *Server) commentVideo(c *gin.Context) (Video, bool) {
	var video Video
	if s.DB.Select("id").Where("id = ? AND status = ?", c.Param("id"), "published").First(&video).Error != nil {
		respondError(c, 404, "video not found")
		return video, false
	}
	return video, true
}

func (s *Server) comments(c *gin.Context) {
	video, ok := s.commentVideo(c)
	if !ok {
		return
	}
	s.listComments(c, video.ID, nil)
}

func (s *Server) commentReplies(c *gin.Context) {
	video, ok := s.commentVideo(c)
	if !ok {
		return
	}
	var parent Comment
	if s.DB.Select("id", "parent_id").Where("id = ? AND video_id = ? AND parent_id IS NULL", c.Param("comment_id"), video.ID).First(&parent).Error != nil {
		respondError(c, 404, "comment not found")
		return
	}
	s.listComments(c, video.ID, &parent.ID)
}

func (s *Server) listComments(c *gin.Context, videoID uint, parentID *uint) {
	limit, cursor, ok := parseCommentPage(c, videoID, parentID)
	if !ok {
		return
	}
	query := s.DB.Preload("User").Where("video_id = ?", videoID)
	if parentID == nil {
		query = query.Where("parent_id IS NULL")
	} else {
		query = query.Where("parent_id = ?", *parentID)
	}
	if cursor != nil {
		query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	var rows []Comment
	if err := query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		respondError(c, 500, "could not load comments")
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	if parentID == nil && len(rows) > 0 {
		ids := make([]uint, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID
		}
		var counts []struct {
			ParentID   uint
			ReplyCount int64
		}
		if err := s.DB.Model(&Comment{}).Select("parent_id, count(*) AS reply_count").Where("parent_id IN ?", ids).Group("parent_id").Scan(&counts).Error; err != nil {
			respondError(c, 500, "could not load reply counts")
			return
		}
		byParent := make(map[uint]int64, len(counts))
		for _, count := range counts {
			byParent[count.ParentID] = count.ReplyCount
		}
		for i := range rows {
			rows[i].ReplyCount = byParent[rows[i].ID]
		}
	}
	next := ""
	if hasMore {
		last := rows[len(rows)-1]
		encoded, _ := json.Marshal(commentCursor{CreatedAt: last.CreatedAt, ID: last.ID, VideoID: videoID, ParentID: parentID})
		next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	c.JSON(200, gin.H{"items": rows, "next_cursor": next, "has_more": hasMore})
}

func (s *Server) recordView(c *gin.Context) {
	var v Video
	if s.DB.First(&v, c.Param("id")).Error != nil || v.Status != "published" {
		respondError(c, 404, "video not found")
		return
	}
	if err := s.DB.Model(&v).UpdateColumn("play_count", gorm.Expr("play_count + 1")).Error; err != nil {
		respondError(c, 500, "could not record view")
		return
	}
	c.Status(204)
}
