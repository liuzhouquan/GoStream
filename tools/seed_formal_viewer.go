//go:build ignore

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/liuzhouquan/GoStream/internal/platform"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	driver := flag.String("driver", "mysql", "database driver: mysql or sqlite")
	dsn := flag.String("dsn", "", "database DSN")
	flag.Parse()

	dbDSN := *dsn
	if dbDSN == "" {
		if *driver == "mysql" {
			dbDSN = os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
			if dbDSN == "" {
				mysqlAddr := os.Getenv("GOSTREAM_MYSQL_ADDR")
				if mysqlAddr == "" {
					mysqlAddr = "127.0.0.1:13307"
				}
				dbDSN = fmt.Sprintf("gostream:gostream-local@tcp(%s)/gostream?charset=utf8mb4&parseTime=True&loc=UTC", mysqlAddr)
			}
		} else {
			dbDSN = "feed.db"
		}
	}

	var dialector gorm.Dialector
	if *driver == "sqlite" {
		dialector = sqlite.Open(dbDSN)
	} else {
		dialector = mysql.Open(dbDSN)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatalf("failed to connect to database (%s: %s): %v", *driver, sanitizeDSN(*driver, dbDSN), err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	viewerHash, err := bcrypt.GenerateFromPassword([]byte("ViewerSecret123!"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("bcrypt hash viewer: %v", err)
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		// Clean up old bench_viewer_prod if present
		var oldViewer platform.User
		if err := tx.Where("username = ?", "bench_viewer_prod").First(&oldViewer).Error; err == nil {
			_ = tx.Where("user_id = ?", oldViewer.ID).Delete(&platform.FeedInbox{}).Error
			_ = tx.Where("follower_id = ?", oldViewer.ID).Delete(&platform.Follow{}).Error
			_ = tx.Where("user_id = ?", oldViewer.ID).Delete(&platform.VideoAction{}).Error
			_ = tx.Delete(&oldViewer).Error
		}

		// 1. Create viewer
		viewer := platform.User{
			Username:     "bench_viewer_prod",
			PasswordHash: string(viewerHash),
			Nickname:     "Bench Viewer",
			CreatedAt:    now,
		}
		if err := tx.Create(&viewer).Error; err != nil {
			return fmt.Errorf("create viewer: %w", err)
		}

		// 2. Find 10 ordinary authors and 1 celebrity author from formal hybrid dataset
		var ordinaryAuthors []platform.User
		if err := tx.Where("username LIKE 'load-u-%' AND username != 'load-u-000099'").Order("id ASC").Limit(10).Find(&ordinaryAuthors).Error; err != nil {
			return fmt.Errorf("find ordinary authors: %w", err)
		}
		if len(ordinaryAuthors) < 10 {
			if err := tx.Where("username != 'bench_viewer_prod'").Order("id ASC").Limit(10).Find(&ordinaryAuthors).Error; err != nil {
				return fmt.Errorf("fallback ordinary authors: %w", err)
			}
		}

		var celebrityAuthor platform.User
		if err := tx.Where("username = 'load-u-000099'").First(&celebrityAuthor).Error; err != nil {
			if err := tx.Where("username != 'bench_viewer_prod'").Order("id DESC").First(&celebrityAuthor).Error; err != nil {
				return fmt.Errorf("find celebrity author: %w", err)
			}
		}

		// 3. Insert follows (10 ordinary + 1 celebrity)
		followeeIDs := make([]uint, 0, len(ordinaryAuthors)+1)
		for _, a := range ordinaryAuthors {
			followeeIDs = append(followeeIDs, a.ID)
		}
		if celebrityAuthor.ID != 0 {
			followeeIDs = append(followeeIDs, celebrityAuthor.ID)
		}

		follows := make([]platform.Follow, len(followeeIDs))
		for i, fid := range followeeIDs {
			follows[i] = platform.Follow{
				FollowerID: viewer.ID,
				FolloweeID: fid,
				CreatedAt:  now.Add(-10 * 24 * time.Hour),
			}
		}
		if err := tx.Create(&follows).Error; err != nil {
			return fmt.Errorf("create follows: %w", err)
		}

		// 4. Inboxes for all videos by the ordinary authors
		var ordinaryIDs []uint
		for _, a := range ordinaryAuthors {
			ordinaryIDs = append(ordinaryIDs, a.ID)
		}

		var videos []platform.Video
		if err := tx.Where("author_id IN ? AND status = 'published'", ordinaryIDs).Order("id ASC").Find(&videos).Error; err != nil {
			return fmt.Errorf("find videos for inboxes: %w", err)
		}

		inboxes := make([]platform.FeedInbox, len(videos))
		for i, v := range videos {
			inboxes[i] = platform.FeedInbox{
				UserID:    viewer.ID,
				VideoID:   v.ID,
				CreatedAt: v.CreatedAt,
			}
		}
		if len(inboxes) > 0 {
			if err := tx.CreateInBatches(&inboxes, 500).Error; err != nil {
				return fmt.Errorf("create inboxes: %w", err)
			}
		}

		// 5. Interactions: 50 likes on videos so bench_viewer_prod has interest profile
		var likeVideos []platform.Video
		if err := tx.Where("status = 'published'").Order("id DESC").Limit(50).Find(&likeVideos).Error; err != nil {
			return fmt.Errorf("find videos for likes: %w", err)
		}

		actions := make([]platform.VideoAction, len(likeVideos))
		for i, v := range likeVideos {
			actions[i] = platform.VideoAction{
				UserID:    viewer.ID,
				VideoID:   v.ID,
				Kind:      "like",
				CreatedAt: now.Add(-time.Duration(50-i) * time.Hour),
			}
		}
		if len(actions) > 0 {
			if err := tx.CreateInBatches(&actions, 100).Error; err != nil {
				return fmt.Errorf("create video actions: %w", err)
			}
			for _, v := range likeVideos {
				if err := tx.Model(&platform.Video{}).Where("id = ?", v.ID).UpdateColumn("like_count", gorm.Expr("like_count + ?", 1)).Error; err != nil {
					return fmt.Errorf("increment like count: %w", err)
				}
			}
		}

		fmt.Printf("SEED_FORMAL_VIEWER_OK viewer_id=%d follows=%d inboxes=%d likes=%d\n",
			viewer.ID, len(follows), len(inboxes), len(actions))
		return nil
	})

	if err != nil {
		log.Fatalf("seeding formal viewer failed: %v", err)
	}
}

func sanitizeDSN(driver, raw string) string {
	if driver == "sqlite" {
		return raw
	}
	cfg, err := mysqlDriver.ParseDSN(raw)
	if err != nil {
		return "[invalid-mysql-dsn]"
	}
	cfg.Passwd = "***"
	return cfg.FormatDSN()
}
