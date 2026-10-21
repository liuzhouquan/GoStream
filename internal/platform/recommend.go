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
	CASE WHEN EXISTS (SELECT 1 FROM follows WHERE follows.follower_id = ? AND follows.followee_id = videos.author_id) THEN 12 ELSE 0 END +
	CASE WHEN videos.author_id IN ? THEN 6 ELSE 0 END)`

type recommendRanking struct {
	anchor         time.Time
	viewerID       uint
	likedAuthors   []uint
	likedAuthorSet map[uint]bool
}

func newRecommendRanking(db *gorm.DB, viewerID uint, anchor time.Time) (*recommendRanking, error) {
	r := &recommendRanking{anchor: anchor.UTC(), viewerID: viewerID, likedAuthors: []uint{0}, likedAuthorSet: map[uint]bool{}}
	if viewerID == 0 {
		return r, nil
	}
	var actions []VideoAction
	if err := db.Where("user_id = ? AND kind IN ?", viewerID, []string{"like", "favorite"}).Order("id DESC").Limit(200).Find(&actions).Error; err != nil {
		return nil, err
	}
	if len(actions) == 0 {
		return r, nil
	}
	videoIDs := make([]uint, 0, len(actions))
	for _, action := range actions {
		videoIDs = append(videoIDs, action.VideoID)
	}
	var videos []Video
	if err := db.Select("author_id").Where("id IN ?", videoIDs).Find(&videos).Error; err != nil {
		return nil, err
	}
	for _, video := range videos {
		if !r.likedAuthorSet[video.AuthorID] {
			r.likedAuthors = append(r.likedAuthors, video.AuthorID)
			r.likedAuthorSet[video.AuthorID] = true
		}
	}
	return r, nil
}

func (r *recommendRanking) vars() []any {
	return []any{r.anchor.Add(-24 * time.Hour), r.anchor.Add(-7 * 24 * time.Hour), r.viewerID, r.likedAuthors}
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
	if r.viewerID > 0 {
		var count int64
		if err := db.Model(&Follow{}).Where("follower_id = ? AND followee_id = ?", r.viewerID, video.AuthorID).Count(&count).Error; err != nil {
			return 0, err
		}
		if count > 0 {
			score += 12
		}
	}
	if r.likedAuthorSet[video.AuthorID] {
		score += 6
	}
	return score, nil
}
