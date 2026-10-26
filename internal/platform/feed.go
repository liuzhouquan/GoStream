package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type feedCursor struct {
	Created int64  `json:"c"`
	ID      uint   `json:"i"`
	Score   int64  `json:"s"`
	Anchor  int64  `json:"a,omitempty"`
	Scene   string `json:"v,omitempty"`
	Zone    int    `json:"z,omitempty"`
}

func (c feedCursor) createdTime() time.Time {
	stamp := time.Unix(0, c.Created).UTC()
	if c.Zone != 0 {
		return stamp.In(time.FixedZone("", c.Zone))
	}
	return stamp
}

type feedItemResponse struct {
	Video
	Liked     bool `json:"liked"`
	Favorited bool `json:"favorited"`
}

func (s *Server) videoCardsForViewer(ctx context.Context, viewerID uint, videos []Video) ([]feedItemResponse, error) {
	entries := make([]feedItemResponse, 0, len(videos))
	for _, video := range videos {
		entries = append(entries, feedItemResponse{Video: video})
	}
	if viewerID == 0 || len(videos) == 0 {
		return entries, nil
	}
	ids := make([]uint, 0, len(videos))
	for _, video := range videos {
		ids = append(ids, video.ID)
	}
	var actions []VideoAction
	if err := s.DB.WithContext(ctx).Where("user_id = ? AND video_id IN ?", viewerID, ids).Find(&actions).Error; err != nil {
		return nil, err
	}
	states := make(map[uint]map[string]bool, len(videos))
	for _, action := range actions {
		if states[action.VideoID] == nil {
			states[action.VideoID] = make(map[string]bool)
		}
		states[action.VideoID][action.Kind] = true
	}
	for i := range entries {
		entries[i].Liked = states[entries[i].ID]["like"]
		entries[i].Favorited = states[entries[i].ID]["favorite"]
	}
	return entries, nil
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
	if scene != "timeline" && scene != "following" && scene != "recommend" && scene != "hot" {
		respondError(c, 400, "scene must be timeline, following, recommend, or hot")
		return
	}
	if scene == "following" && uid(c) == 0 {
		respondError(c, 401, "sign in to view following feed")
		return
	}
	rawCursor := c.Query("cursor")
	if len(rawCursor) > 512 {
		respondError(c, 400, "invalid cursor")
		return
	}
	cur, err := decodeCursor(rawCursor)
	if err != nil {
		respondError(c, 400, "invalid cursor")
		return
	}
	if cur.Scene != "" && cur.Scene != scene {
		respondError(c, 400, "cursor belongs to another feed scene")
		return
	}
	if cur.Zone < -14*3600 || cur.Zone > 14*3600 {
		respondError(c, 400, "invalid cursor time zone")
		return
	}
	if scene == "recommend" && cur.ID > 0 {
		anchor := time.Unix(0, cur.Anchor).UTC()
		if cur.Scene != "recommend" || cur.Anchor <= 0 || anchor.Before(time.Now().UTC().Add(-24*time.Hour)) || anchor.After(time.Now().UTC().Add(time.Minute)) {
			respondError(c, 400, "recommendation cursor is invalid or expired")
			return
		}
	}
	cacheVersion := ""
	cacheViewerID := uint(0)
	cacheEligible := s.cache != nil && ((uid(c) == 0 && scene != "following") || (scene == "following" && rawCursor == ""))
	if cacheEligible {
		if scene == "following" {
			cacheViewerID = uid(c)
		}
		cached, version, err := s.cache.load(c.Request.Context(), scene, limit, rawCursor, cacheViewerID)
		if err != nil {
			s.metrics.feedCache.WithLabelValues("error").Inc()
		} else {
			cacheVersion = version
			if cached != nil {
				s.metrics.feedCache.WithLabelValues("hit").Inc()
				c.Data(200, "application/json; charset=utf-8", cached)
				return
			}
			s.metrics.feedCache.WithLabelValues("miss").Inc()
		}
	}
	q := s.DB.Model(&Video{}).Preload("Author").Where("videos.status = ?", "published")
	cursorTime := cur.createdTime()
	var recommendation *recommendRanking
	var items []Video
	switch scene {
	case "timeline":
		if cur.ID > 0 {
			q = q.Where("videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?)", cursorTime, cursorTime, cur.ID)
		}
		q = q.Order("videos.created_at DESC").Order("videos.id DESC")
	case "following":
		if s.fanoutMaxFollowers > 0 {
			items, err = s.followingVideos(c.Request.Context(), uid(c), cur, limit)
		} else {
			q = q.Joins("JOIN follows ON follows.followee_id = videos.author_id AND follows.follower_id = ?", uid(c))
			if cur.ID > 0 {
				q = q.Where("videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?)", cursorTime, cursorTime, cur.ID)
			}
			q = q.Order("videos.created_at DESC").Order("videos.id DESC")
		}
	case "recommend":
		anchor := time.Now().UTC()
		if cur.Anchor > 0 {
			anchor = time.Unix(0, cur.Anchor).UTC()
		}
		recommendation, err = newRecommendRanking(s.DB, uid(c), anchor)
		if err != nil {
			respondError(c, 500, "could not load recommendation preferences")
			return
		}
		q = recommendation.query(q, cur)
	case "hot":
		if cur.ID > 0 {
			q = q.Where("(videos.like_count * 3 + videos.comment_count * 5 + videos.favorite_count * 4) < ? OR ((videos.like_count * 3 + videos.comment_count * 5 + videos.favorite_count * 4) = ? AND videos.id < ?)", cur.Score, cur.Score, cur.ID)
		}
		q = q.Order("(videos.like_count * 3 + videos.comment_count * 5 + videos.favorite_count * 4) DESC").Order("videos.id DESC")
	default:
		respondError(c, 400, "scene must be timeline, following, recommend, or hot")
		return
	}
	if err == nil && !(scene == "following" && s.fanoutMaxFollowers > 0) {
		err = q.Limit(limit + 1).Find(&items).Error
	}
	if err != nil {
		respondError(c, 500, "could not load feed")
		return
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	entries, err := s.videoCardsForViewer(c.Request.Context(), uid(c), items)
	if err != nil {
		respondError(c, 500, "could not load interaction state")
		return
	}
	next := ""
	if more && len(items) > 0 {
		last := items[len(items)-1]
		score := last.LikeCount*3 + last.CommentCount*5 + last.FavoriteCount*4
		anchor := int64(0)
		if recommendation != nil {
			score, err = recommendation.score(s.DB, last)
			if err != nil {
				respondError(c, 500, "could not build recommendation cursor")
				return
			}
			anchor = recommendation.anchor.UnixNano()
		}
		_, zone := last.CreatedAt.Zone()
		next = encodeCursor(feedCursor{Created: last.CreatedAt.UnixNano(), ID: last.ID, Score: score, Anchor: anchor, Scene: scene, Zone: zone})
	}
	response := gin.H{"scene": scene, "items": entries, "next_cursor": next, "has_more": more}
	if cacheVersion != "" {
		if data, err := json.Marshal(response); err == nil {
			if err := s.cache.store(c.Request.Context(), cacheVersion, scene, limit, rawCursor, cacheViewerID, data); err != nil {
				s.metrics.feedCache.WithLabelValues("error").Inc()
			}
			c.Data(200, "application/json; charset=utf-8", data)
			return
		}
	}
	c.JSON(200, response)
}
