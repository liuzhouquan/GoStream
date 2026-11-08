//go:build ignore

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

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
				dbDSN = fmt.Sprintf("gostream:gostream-local@tcp(%s)/gostream?charset=utf8mb4&parseTime=True&loc=Local", mysqlAddr)
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
		log.Fatalf("failed to connect to database (%s: %s): %v", *driver, dbDSN, err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	authorHash, err := bcrypt.GenerateFromPassword([]byte("AuthorSecret123!"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("bcrypt hash author: %v", err)
	}
	viewerHash, err := bcrypt.GenerateFromPassword([]byte("ViewerSecret123!"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("bcrypt hash viewer: %v", err)
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		// Clean up existing benchmark records if any
		var oldViewer platform.User
		if err := tx.Where("username = ?", "bench_viewer_prod").First(&oldViewer).Error; err == nil {
			_ = tx.Where("user_id = ?", oldViewer.ID).Delete(&platform.FeedInbox{}).Error
			_ = tx.Where("follower_id = ?", oldViewer.ID).Delete(&platform.Follow{}).Error
		}
		_ = tx.Where("title LIKE ?", "Bench Video %").Delete(&platform.Video{}).Error
		_ = tx.Where("username LIKE ? OR username = ?", "bench_auth_%", "bench_viewer_prod").Delete(&platform.User{}).Error

		// 1. Authors
		const authorCount = 10
		authors := make([]platform.User, authorCount)
		for i := 0; i < authorCount; i++ {
			authors[i] = platform.User{
				Username:     fmt.Sprintf("bench_auth_%02d", i),
				PasswordHash: string(authorHash),
				Nickname:     fmt.Sprintf("Bench Author %d", i),
				CreatedAt:    now.Add(-30 * 24 * time.Hour),
			}
		}
		if err := tx.Create(&authors).Error; err != nil {
			return fmt.Errorf("create authors: %w", err)
		}

		celebrityID := authors[0].ID

		// 2. Viewer
		viewer := platform.User{
			Username:     "bench_viewer_prod",
			PasswordHash: string(viewerHash),
			Nickname:     "Bench Viewer",
			CreatedAt:    now,
		}
		if err := tx.Create(&viewer).Error; err != nil {
			return fmt.Errorf("create viewer: %w", err)
		}

		// 3. Follows
		follows := make([]platform.Follow, authorCount)
		for i := 0; i < authorCount; i++ {
			follows[i] = platform.Follow{
				FollowerID: viewer.ID,
				FolloweeID: authors[i].ID,
				CreatedAt:  now.Add(-10 * 24 * time.Hour),
			}
		}
		if err := tx.Create(&follows).Error; err != nil {
			return fmt.Errorf("create follows: %w", err)
		}

		// 4. Videos (120 published videos)
		const videoCount = 120
		videos := make([]platform.Video, videoCount)
		for i := 0; i < videoCount; i++ {
			author := authors[i%authorCount]
			pull := author.ID == celebrityID
			createdAt := now.Add(-24 * time.Hour).Add(time.Duration(i) * time.Second)

			videos[i] = platform.Video{
				AuthorID:    author.ID,
				Title:       fmt.Sprintf("Bench Video %03d", i),
				Description: "Benchmark video payload for continuous warm cache verification",
				MediaURL:    fmt.Sprintf("/media/bench_%03d.mp4", i),
				Status:      "published",
				DurationMS:  15000,
				Width:       1280,
				Height:      720,
				FeedPull:    pull,
				CreatedAt:   createdAt,
			}
		}
		if err := tx.Create(&videos).Error; err != nil {
			return fmt.Errorf("create videos: %w", err)
		}

		// 5. Inboxes (for all non-celebrity videos)
		inboxes := make([]platform.FeedInbox, 0, videoCount)
		for _, v := range videos {
			if v.AuthorID != celebrityID {
				inboxes = append(inboxes, platform.FeedInbox{
					UserID:    viewer.ID,
					VideoID:   v.ID,
					CreatedAt: v.CreatedAt,
				})
			}
		}
		if err := tx.Create(&inboxes).Error; err != nil {
			return fmt.Errorf("create inboxes: %w", err)
		}

		fmt.Printf("SEED_FEED_BENCHMARK_OK viewer_id=%d authors=%d videos=%d inboxes=%d\n",
			viewer.ID, len(authors), len(videos), len(inboxes))
		return nil
	})

	if err != nil {
		log.Fatalf("seeding failed: %v", err)
	}
}

