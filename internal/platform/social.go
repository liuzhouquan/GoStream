package platform

import (
	"fmt"
	"strconv"
	"strings"

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
		err := s.DB.Transaction(func(tx *gorm.DB) error {
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
	if in.ParentID != nil {
		var parent Comment
		if s.DB.Where("id = ? AND video_id = ?", *in.ParentID, v.ID).First(&parent).Error != nil {
			respondError(c, 404, "parent comment not found")
			return
		}
	}
	cm := Comment{VideoID: v.ID, UserID: uid(c), ParentID: in.ParentID, Body: in.Body}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&cm).Error; err != nil {
			return err
		}
		if err := tx.Model(&Video{}).Where("id = ?", v.ID).UpdateColumn("comment_count", gorm.Expr("comment_count + 1")).Error; err != nil {
			return err
		}
		if v.AuthorID != uid(c) {
			key := fmt.Sprintf("comment:%d", cm.ID)
			if err := enqueueNotification(tx, Notification{UserID: v.AuthorID, EventKey: key, Kind: "comment", ActorID: uid(c), VideoID: v.ID, Text: in.Body}); err != nil {
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

func (s *Server) comments(c *gin.Context) {
	var video Video
	if s.DB.Select("id").Where("id = ? AND status = ?", c.Param("id"), "published").First(&video).Error != nil {
		respondError(c, 404, "video not found")
		return
	}
	var rows []Comment
	if err := s.DB.Preload("User").Where("video_id = ?", video.ID).Order("created_at DESC").Limit(100).Find(&rows).Error; err != nil {
		respondError(c, 500, "could not load comments")
		return
	}
	c.JSON(200, gin.H{"items": rows})
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

func (s *Server) search(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if len(q) < 2 || len(q) > 80 {
		respondError(c, 400, "q must be 2-80 characters")
		return
	}
	like := "%" + strings.ReplaceAll(strings.ReplaceAll(q, "%", "\\%"), "_", "\\_") + "%"
	var users []User
	var videos []Video
	if err := s.DB.Where("username LIKE ? OR nickname LIKE ?", like, like).Limit(20).Find(&users).Error; err != nil {
		respondError(c, 500, "could not search users")
		return
	}
	if err := s.DB.Preload("Author").Where("status = ? AND (title LIKE ? OR description LIKE ?)", "published", like, like).Limit(30).Find(&videos).Error; err != nil {
		respondError(c, 500, "could not search videos")
		return
	}
	c.JSON(200, gin.H{"users": users, "videos": videos})
}
