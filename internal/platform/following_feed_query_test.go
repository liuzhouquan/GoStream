package platform

import (
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestHybridPushPageUsesInboxFirstOnlyForMySQL(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "gostream:gostream-ci-only@tcp(127.0.0.1:3306)/feed",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	query := db.Model(&Video{}).Where("videos.status = ?", "published")
	query = followingCandidateQuery(query, 501, feedCursor{}, 20, true)
	var videos []Video
	statement := query.Limit(21).Find(&videos).Statement
	if statement.Error != nil {
		t.Fatal(statement.Error)
	}

	querySQL := strings.Join(strings.Fields(statement.SQL.String()), " ")
	pushPage := "FROM feed_inboxes STRAIGHT_JOIN videos ON videos.id = feed_inboxes.video_id STRAIGHT_JOIN follows ON follows.followee_id = videos.author_id"
	if !strings.Contains(querySQL, pushPage) {
		t.Fatalf("MySQL hybrid push page must preserve inbox-first join order for its ordered index; query=%s", querySQL)
	}

	s, _, _ := testServer(t)
	sqliteQuery := s.DB.Session(&gorm.Session{DryRun: true}).Model(&Video{}).Where("videos.status = ?", "published")
	sqliteQuery = followingCandidateQuery(sqliteQuery, 501, feedCursor{}, 20, true)
	statement = sqliteQuery.Limit(21).Find(&videos).Statement
	if statement.Error != nil {
		t.Fatal(statement.Error)
	}
	querySQL = strings.Join(strings.Fields(statement.SQL.String()), " ")
	if strings.Contains(querySQL, "STRAIGHT_JOIN") {
		t.Fatalf("SQLite hybrid query must not emit a MySQL-only join operator: %s", querySQL)
	}
}
