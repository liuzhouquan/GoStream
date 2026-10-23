package platform

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const searchDefaultLimit = 20
const searchMaxLimit = 50

// A dedicated escape character gives SQLite and MySQL the same literal LIKE
// semantics for user input containing %, _, or !.
func searchPattern(query string) string {
	replacer := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return "%" + replacer.Replace(query) + "%"
}

func searchBeforeID(c *gin.Context, key string) (uint64, bool) {
	raw := c.Query(key)
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		respondError(c, http.StatusBadRequest, "invalid "+key)
		return 0, false
	}
	return id, true
}

func (s *Server) search(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) < 2 || utf8.RuneCountInString(query) > 80 {
		respondError(c, http.StatusBadRequest, "q must be 2-80 characters")
		return
	}
	kind := c.DefaultQuery("type", "all")
	if kind != "all" && kind != "users" && kind != "videos" {
		respondError(c, http.StatusBadRequest, "type must be all, users, or videos")
		return
	}
	limit := searchDefaultLimit
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > searchMaxLimit {
			respondError(c, http.StatusBadRequest, "limit must be 1-50")
			return
		}
		limit = parsed
	}
	userBefore, ok := searchBeforeID(c, "users_before_id")
	if !ok {
		return
	}
	videoBefore, ok := searchBeforeID(c, "videos_before_id")
	if !ok {
		return
	}
	pattern := searchPattern(query)
	users := make([]User, 0)
	videos := make([]Video, 0)
	moreUsers, moreVideos := false, false
	nextUser, nextVideo := uint(0), uint(0)
	if kind != "videos" {
		q := s.DB.WithContext(c.Request.Context()).Model(&User{}).
			Where("username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!'", pattern, pattern)
		if userBefore > 0 {
			q = q.Where("id < ?", userBefore)
		}
		if err := q.Order("id DESC").Limit(limit + 1).Find(&users).Error; err != nil {
			respondError(c, http.StatusInternalServerError, "could not search users")
			return
		}
		moreUsers = len(users) > limit
		if moreUsers {
			users = users[:limit]
		}
		if len(users) > 0 {
			nextUser = users[len(users)-1].ID
		}
	}
	if kind != "users" {
		q := s.DB.WithContext(c.Request.Context()).Model(&Video{}).Preload("Author").
			Where("status = ? AND (title LIKE ? ESCAPE '!' OR description LIKE ? ESCAPE '!')", "published", pattern, pattern)
		if videoBefore > 0 {
			q = q.Where("id < ?", videoBefore)
		}
		if err := q.Order("id DESC").Limit(limit + 1).Find(&videos).Error; err != nil {
			respondError(c, http.StatusInternalServerError, "could not search videos")
			return
		}
		moreVideos = len(videos) > limit
		if moreVideos {
			videos = videos[:limit]
		}
		if len(videos) > 0 {
			nextVideo = videos[len(videos)-1].ID
		}
	}
	videoResults, err := s.videoCardsForViewer(c.Request.Context(), uid(c), videos)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "could not load interaction state")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"users": users, "videos": videoResults,
		"next_user_cursor": nextUser, "next_video_cursor": nextVideo,
		"has_more_users": moreUsers, "has_more_videos": moreVideos,
	})
}
