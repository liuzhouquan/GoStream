package platform

import (
	"context"
	"strings"
)

// Each branch is capped at one page before the UNION. Completed fanout jobs
// leave the ordinary pull index, while a separate indexed range keeps videos
// visible to followers who joined after publication.
func (s *Server) followingVideos(ctx context.Context, viewerID uint, cursor feedCursor, limit int) ([]Video, error) {
	cursorTime := cursor.createdTime()
	var sql strings.Builder
	sql.Grow(1024)
	args := make([]any, 0, 16)
	args = append(args, viewerID, "published", true)
	sql.WriteString(`JOIN (
SELECT id, created_at FROM (
  SELECT videos.id, videos.created_at FROM videos
  JOIN follows ON follows.followee_id = videos.author_id AND follows.follower_id = ?
  WHERE videos.status = ? AND videos.feed_pull = ?`)
	if cursor.ID > 0 {
		sql.WriteString(` AND (videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?))`)
		args = append(args, cursorTime, cursorTime, cursor.ID)
	}
	sql.WriteString(` ORDER BY videos.created_at DESC, videos.id DESC LIMIT ?
) AS pull_page
UNION
SELECT id, created_at FROM (
  SELECT videos.id, videos.created_at FROM follows
  JOIN videos ON videos.author_id = follows.followee_id
  WHERE follows.follower_id = ? AND videos.status = ? AND videos.feed_pull = ?
    AND follows.created_at >= videos.created_at`)
	args = append(args, limit+1, viewerID, "published", false)
	if cursor.ID > 0 {
		sql.WriteString(` AND (videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?))`)
		args = append(args, cursorTime, cursorTime, cursor.ID)
	}
	sql.WriteString(` ORDER BY videos.created_at DESC, videos.id DESC LIMIT ?
) AS late_follow_page
UNION
SELECT id, created_at FROM (
  SELECT feed_inboxes.video_id AS id, feed_inboxes.created_at FROM feed_inboxes
  JOIN videos ON videos.id = feed_inboxes.video_id
  JOIN follows ON follows.followee_id = videos.author_id AND follows.follower_id = ?
  WHERE feed_inboxes.user_id = ? AND videos.status = ?`)
	args = append(args, limit+1, viewerID, viewerID, "published")
	if cursor.ID > 0 {
		sql.WriteString(` AND (feed_inboxes.created_at < ? OR (feed_inboxes.created_at = ? AND feed_inboxes.video_id < ?))`)
		args = append(args, cursorTime, cursorTime, cursor.ID)
	}
	sql.WriteString(` ORDER BY feed_inboxes.created_at DESC, feed_inboxes.video_id DESC LIMIT ?
) AS push_page
) AS feed_page ON feed_page.id = videos.id`)
	args = append(args, limit+1)
	var videos []Video
	err := s.DB.WithContext(ctx).Model(&Video{}).Preload("Author").
		Joins(sql.String(), args...).Where("videos.status = ?", "published").
		Order("feed_page.created_at DESC, feed_page.id DESC").Limit(limit + 1).Find(&videos).Error
	if err != nil {
		return nil, err
	}
	if videos == nil {
		videos = []Video{}
	}
	return videos, nil
}
