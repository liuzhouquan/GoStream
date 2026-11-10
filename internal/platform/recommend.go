package platform

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Recommendation weights are deliberately small, inspectable integers.
// The anchor freezes time buckets across pages of one cursor traversal.
const recommendScoreSQL = `(videos.like_count * 3 + videos.comment_count * 5 + videos.favorite_count * 4 +
	CASE WHEN videos.created_at >= ? THEN 12 WHEN videos.created_at >= ? THEN 5 ELSE 0 END +
	CASE WHEN videos.author_id IN ? THEN 12 ELSE 0 END +
	CASE WHEN videos.author_id IN ? THEN 6 ELSE 0 END)`

type recommendRanking struct {
	anchor            time.Time
	viewerID          uint
	followedAuthors   []uint
	followedAuthorSet map[uint]bool
	likedAuthors      []uint
	likedAuthorSet    map[uint]bool
}

func newRecommendRanking(db *gorm.DB, viewerID uint, anchor time.Time) (*recommendRanking, error) {
	r := &recommendRanking{
		anchor:            anchor.UTC(),
		viewerID:          viewerID,
		followedAuthors:   []uint{0},
		followedAuthorSet: map[uint]bool{},
		likedAuthors:      []uint{0},
		likedAuthorSet:    map[uint]bool{},
	}
	if viewerID == 0 {
		return r, nil
	}
	var followedIDs []uint
	if err := db.Model(&Follow{}).Where("follower_id = ?", viewerID).Pluck("followee_id", &followedIDs).Error; err != nil {
		return nil, err
	}
	for _, fid := range followedIDs {
		if !r.followedAuthorSet[fid] {
			r.followedAuthors = append(r.followedAuthors, fid)
			r.followedAuthorSet[fid] = true
		}
	}
	// Keep the existing recent-200-action window while projecting only author IDs.
	recentActions := db.Model(&VideoAction{}).Select("video_id").
		Where("user_id = ? AND kind IN ?", viewerID, []string{"like", "favorite"}).
		Order("id DESC").Limit(200)
	var likedIDs []uint
	if err := db.Table("(?) AS recent_actions", recentActions).
		Joins("JOIN videos ON videos.id = recent_actions.video_id").
		Distinct("videos.author_id").Pluck("videos.author_id", &likedIDs).Error; err != nil {
		return nil, err
	}
	for _, aid := range likedIDs {
		if !r.likedAuthorSet[aid] {
			r.likedAuthors = append(r.likedAuthors, aid)
			r.likedAuthorSet[aid] = true
		}
	}
	return r, nil
}

func (r *recommendRanking) vars() []any {
	return []any{r.anchor.Add(-24 * time.Hour), r.anchor.Add(-7 * 24 * time.Hour), r.followedAuthors, r.likedAuthors}
}

func (r *recommendRanking) query(q *gorm.DB, cursor feedCursor) *gorm.DB {
	if cursor.ID > 0 {
		vars := append(r.vars(), cursor.Score)
		vars = append(vars, r.vars()...)
		vars = append(vars, cursor.Score, cursor.ID)
		q = q.Where(recommendScoreSQL+" < ? OR ("+recommendScoreSQL+" = ? AND videos.id < ?)", vars...)
	}
	return q.Clauses(clause.OrderBy{Expression: clause.Expr{SQL: recommendScoreSQL + " DESC, videos.id DESC", Vars: r.vars()}})
}

func (r *recommendRanking) score(db *gorm.DB, video Video) (int64, error) {
	score := video.LikeCount*3 + video.CommentCount*5 + video.FavoriteCount*4
	if !video.CreatedAt.Before(r.anchor.Add(-24 * time.Hour)) {
		score += 12
	} else if !video.CreatedAt.Before(r.anchor.Add(-7 * 24 * time.Hour)) {
		score += 5
	}
	if r.followedAuthorSet[video.AuthorID] {
		score += 12
	}
	if r.likedAuthorSet[video.AuthorID] {
		score += 6
	}
	return score, nil
}
