package platform

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type feedCursor struct {
	Created int64 `json:"c"`
	ID      uint  `json:"i"`
	Score   int64 `json:"s"`
}

type feedItemResponse struct {
	Video
	Liked     bool `json:"liked"`
	Favorited bool `json:"favorited"`
}

func encodeCursor(v feedCursor) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(v string) (feedCursor, error) {
	var c feedCursor
	if v == "" {
		return c, nil
	}
	b, e := base64.RawURLEncoding.DecodeString(v)
	if e != nil {
		return c, e
	}
	e = json.Unmarshal(b, &c)
	return c, e
}

func (s *Server) feed(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	scene := c.DefaultQuery("scene", "timeline")
	cur, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		respondError(c, 400, "invalid cursor")
		return
	}
	q := s.DB.Model(&Video{}).Preload("Author").Where("videos.status = ?", "published")
	cursorTime := time.Unix(0, cur.Created).UTC()
	switch scene {
	case "timeline":
		if cur.ID > 0 {
			q = q.Where("videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?)", cursorTime, cursorTime, cur.ID)
		}
		q = q.Order("videos.created_at DESC").Order("videos.id DESC")
	case "following":
		raw, ok := c.Get("userID")
		_ = raw
		if !ok {
			respondError(c, 401, "sign in to view following feed")
			return
		}
		q = q.Joins("JOIN follows ON follows.followee_id = videos.author_id AND follows.follower_id = ?", uid(c))
		if cur.ID > 0 {
			q = q.Where("videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?)", cursorTime, cursorTime, cur.ID)
		}
		q = q.Order("videos.created_at DESC").Order("videos.id DESC")
	case "recommend", "hot":
		if cur.ID > 0 {
			q = q.Where("(videos.like_count * 3 + videos.comment_count * 5 + videos.favorite_count * 4) < ? OR ((videos.like_count * 3 + videos.comment_count * 5 + videos.favorite_count * 4) = ? AND videos.id < ?)", cur.Score, cur.Score, cur.ID)
		}
		q = q.Order("(videos.like_count * 3 + videos.comment_count * 5 + videos.favorite_count * 4) DESC").Order("videos.id DESC")
	default:
		respondError(c, 400, "scene must be timeline, following, recommend, or hot")
		return
	}
	var items []Video
	if q.Limit(limit+1).Find(&items).Error != nil {
		respondError(c, 500, "could not load feed")
		return
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	entries := make([]feedItemResponse, 0, len(items))
	for _, item := range items {
		entries = append(entries, feedItemResponse{Video: item})
	}
	if raw, ok := c.Get("userID"); ok && len(items) > 0 {
		viewerID, _ := raw.(uint)
		ids := make([]uint, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		var actions []VideoAction
		if err := s.DB.Where("user_id = ? AND video_id IN ?", viewerID, ids).Find(&actions).Error; err != nil {
			respondError(c, 500, "could not load interaction state")
			return
		}
		states := make(map[uint]map[string]bool)
		for _, action := range actions {
			if states[action.VideoID] == nil {
				states[action.VideoID] = map[string]bool{}
			}
			states[action.VideoID][action.Kind] = true
		}
		for i := range entries {
			entries[i].Liked = states[entries[i].ID]["like"]
			entries[i].Favorited = states[entries[i].ID]["favorite"]
		}
	}
	next := ""
	if more && len(items) > 0 {
		last := items[len(items)-1]
		next = encodeCursor(feedCursor{Created: last.CreatedAt.UnixNano(), ID: last.ID, Score: last.LikeCount*3 + last.CommentCount*5 + last.FavoriteCount*4})
	}
	c.JSON(200, gin.H{"scene": scene, "items": entries, "next_cursor": next, "has_more": more})
}
