package platform

import (
	"context"
	"sort"
	"time"

	"gorm.io/gorm"
)

type followingCandidate struct {
	ID        uint
	CreatedAt time.Time
}

// Each branch fetches at most one page. Any row in the union's first page
// must be in the first page of at least one branch, so the merge is bounded.
func (s *Server) followingVideos(ctx context.Context, viewerID uint, cursor feedCursor, limit int) ([]Video, error) {
	cursorTime := cursor.createdTime()
	base := func() *gorm.DB {
		return s.DB.WithContext(ctx).Table("videos").Select("videos.id, videos.created_at").
			Joins("JOIN follows ON follows.followee_id = videos.author_id AND follows.follower_id = ?", viewerID).
			Where("videos.status = ?", "published")
	}
	pull := base().Joins("LEFT JOIN feed_fanout_jobs ON feed_fanout_jobs.video_id = videos.id").
		Where("feed_fanout_jobs.id IS NULL OR feed_fanout_jobs.done_at IS NULL OR follows.created_at >= videos.created_at")
	if cursor.ID > 0 {
		pull = pull.Where("videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?)", cursorTime, cursorTime, cursor.ID)
	}
	var pullRows []followingCandidate
	if err := pull.Order("videos.created_at DESC").Order("videos.id DESC").Limit(limit + 1).Find(&pullRows).Error; err != nil {
		return nil, err
	}
	push := base().Joins("JOIN feed_inboxes ON feed_inboxes.video_id = videos.id AND feed_inboxes.user_id = ?", viewerID)
	if cursor.ID > 0 {
		push = push.Where("feed_inboxes.created_at < ? OR (feed_inboxes.created_at = ? AND feed_inboxes.video_id < ?)", cursorTime, cursorTime, cursor.ID)
	}
	var pushRows []followingCandidate
	if err := push.Order("feed_inboxes.created_at DESC").Order("feed_inboxes.video_id DESC").Limit(limit + 1).Find(&pushRows).Error; err != nil {
		return nil, err
	}
	seen := make(map[uint]bool, len(pullRows)+len(pushRows))
	merged := make([]followingCandidate, 0, len(pullRows)+len(pushRows))
	for _, row := range append(pullRows, pushRows...) {
		if !seen[row.ID] {
			seen[row.ID] = true
			merged = append(merged, row)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].CreatedAt.Equal(merged[j].CreatedAt) {
			return merged[i].ID > merged[j].ID
		}
		return merged[i].CreatedAt.After(merged[j].CreatedAt)
	})
	if len(merged) > limit+1 {
		merged = merged[:limit+1]
	}
	if len(merged) == 0 {
		return nil, nil
	}
	ids := make([]uint, len(merged))
	for i, row := range merged {
		ids[i] = row.ID
	}
	var videos []Video
	if err := s.DB.WithContext(ctx).Preload("Author").Where("id IN ? AND status = ?", ids, "published").Find(&videos).Error; err != nil {
		return nil, err
	}
	byID := make(map[uint]Video, len(videos))
	for _, video := range videos {
		byID[video.ID] = video
	}
	ordered := make([]Video, 0, len(videos))
	for _, id := range ids {
		if video, ok := byID[id]; ok {
			ordered = append(ordered, video)
		}
	}
	return ordered, nil
}
