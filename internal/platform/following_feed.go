package platform

import (
	"context"
	"strings"

	"gorm.io/gorm"
)

// Each branch is capped at one page before the UNION. Completed fanout jobs
// leave the ordinary pull index, while a separate indexed range keeps videos
// visible to followers who joined after publication.
func followingCandidateQuery(db *gorm.DB, viewerID uint, cursor feedCursor, limit int, hybrid bool) *gorm.DB {
	if !hybrid {
		if cursor.ID > 0 {
			cursorTime := cursor.createdTime()
			db = db.Where("videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?)", cursorTime, cursorTime, cursor.ID)
		}
		return db.Joins("JOIN follows ON follows.followee_id = videos.author_id AND follows.follower_id = ?", viewerID).
			Order("videos.created_at DESC, videos.id DESC")
	}

	cursorTime := cursor.createdTime()
	pushJoin := "JOIN"
	if db.Dialector.Name() == "mysql" {
		// The viewer/time/video index can scan this branch in page order and
		// stop after filling the page. Without this order, MySQL starts from
		// follows and videos and probes the inbox once for every candidate.
		pushJoin = "STRAIGHT_JOIN"
	}
	// Keep the three candidate sources disjoint: unfinished fanout stays in
	// pull, completed fanout uses inbox rows, and late follows only fill gaps.
	// UNION ALL relies on these publication and inbox invariants.
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
UNION ALL
SELECT id, created_at FROM (
  SELECT videos.id, videos.created_at FROM follows
  JOIN videos ON videos.author_id = follows.followee_id
  WHERE follows.follower_id = ? AND videos.status = ? AND videos.feed_pull = ?
    AND follows.created_at >= videos.created_at
    AND NOT EXISTS (
      SELECT 1 FROM feed_inboxes
      WHERE feed_inboxes.user_id = ? AND feed_inboxes.video_id = videos.id
    )`)
	args = append(args, limit+1, viewerID, "published", false, viewerID)
	if cursor.ID > 0 {
		sql.WriteString(` AND (videos.created_at < ? OR (videos.created_at = ? AND videos.id < ?))`)
		args = append(args, cursorTime, cursorTime, cursor.ID)
	}
	sql.WriteString(` ORDER BY videos.created_at DESC, videos.id DESC LIMIT ?
) AS late_follow_page
UNION ALL
SELECT id, created_at FROM (
SELECT feed_inboxes.video_id AS id, feed_inboxes.created_at FROM feed_inboxes ` + pushJoin + ` videos ON videos.id = feed_inboxes.video_id
  ` + pushJoin + ` follows ON follows.followee_id = videos.author_id AND follows.follower_id = ?
  WHERE feed_inboxes.user_id = ? AND videos.status = ? AND videos.feed_pull = ?`)
	args = append(args, limit+1, viewerID, viewerID, "published", false)
	if cursor.ID > 0 {
		sql.WriteString(` AND (feed_inboxes.created_at < ? OR (feed_inboxes.created_at = ? AND feed_inboxes.video_id < ?))`)
		args = append(args, cursorTime, cursorTime, cursor.ID)
	}
	sql.WriteString(` ORDER BY feed_inboxes.created_at DESC, feed_inboxes.video_id DESC LIMIT ?
) AS push_page
) AS feed_page ON feed_page.id = videos.id`)
	args = append(args, limit+1)
	return db.Joins(sql.String(), args...).Order("feed_page.created_at DESC, feed_page.id DESC")
}

func (s *Server) preloadVideoAuthors(ctx context.Context, videos []Video) error {
	if len(videos) == 0 {
		return nil
	}
	authorIDs := make([]uint, 0, len(videos))
	seen := make(map[uint]struct{}, len(videos))
	for _, video := range videos {
		if video.AuthorID == 0 {
			continue
		}
		if _, ok := seen[video.AuthorID]; ok {
			continue
		}
		seen[video.AuthorID] = struct{}{}
		authorIDs = append(authorIDs, video.AuthorID)
	}
	if len(authorIDs) == 0 {
		return nil
	}
	var authors []User
	if err := s.DB.WithContext(ctx).Where("id IN ?", authorIDs).Find(&authors).Error; err != nil {
		return err
	}
	byID := make(map[uint]User, len(authors))
	for _, author := range authors {
		byID[author.ID] = author
	}
	for i := range videos {
		videos[i].Author = byID[videos[i].AuthorID]
	}
	return nil
}
