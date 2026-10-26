package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/liuzhouquan/GoStream/internal/migrations"
	"github.com/liuzhouquan/GoStream/internal/platform"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func main() {
	driver := flag.String("driver", "sqlite", "database driver: sqlite or mysql")
	dsn := flag.String("dsn", "data/gostream.db", "database DSN used by the API")
	videos := flag.Int("videos", 1000, "published videos to insert")
	scenario := flag.String("scenario", "single-author", "fixture shape: single-author or hybrid")
	users := flag.Int("users", 2000, "hybrid fixture user count")
	authors := flag.Int("authors", 100, "hybrid fixture author count, including one high-follower author")
	followsPerUser := flag.Int("follows-per-user", 5, "ordinary authors followed by each hybrid viewer")
	celebrityFollowers := flag.Int("celebrity-followers", 1500, "users following the hybrid high-follower author")
	fanoutMaxFollowers := flag.Int("fanout-max-followers", 1000, "ordinary author threshold used to classify hybrid fixtures")
	likes := flag.Int("likes", 50000, "hybrid fixture like rows, spread across videos with matching counters")
	flag.Parse()
	if *videos < 1 || *videos > 100000 {
		log.Fatal("videos must be 1-100000")
	}
	var dialector gorm.Dialector
	switch *driver {
	case "sqlite":
		dialector = sqlite.Open(*dsn)
	case "mysql":
		dialector = mysql.Open(*dsn)
	default:
		log.Fatal("driver must be sqlite or mysql")
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	if *scenario == "hybrid" {
		if err := seedHybrid(db, hybridOptions{users: *users, authors: *authors, videos: *videos, followsPerUser: *followsPerUser, celebrityFollowers: *celebrityFollowers, fanoutMaxFollowers: *fanoutMaxFollowers, likes: *likes}); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *scenario != "single-author" {
		log.Fatal("scenario must be single-author or hybrid")
	}
	if err := db.AutoMigrate(&platform.User{}, &platform.Video{}); err != nil {
		log.Fatal(err)
	}
	author := platform.User{Username: fmt.Sprintf("load-author-%d", time.Now().UnixNano()), PasswordHash: "disabled-load-fixture", Nickname: "Load fixture"}
	if err := db.Create(&author).Error; err != nil {
		log.Fatal(err)
	}
	batch := make([]platform.Video, 0, 200)
	now := time.Now().UTC()
	for i := 0; i < *videos; i++ {
		batch = append(batch, platform.Video{AuthorID: author.ID, Title: fmt.Sprintf("load video %06d", i), MediaURL: "/media/load-fixture.mp4", Status: "published", CreatedAt: now.Add(-time.Duration(i) * time.Second)})
		if len(batch) == cap(batch) || i == *videos-1 {
			if err := db.Create(&batch).Error; err != nil {
				log.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	fmt.Printf("inserted %d synthetic published video rows for author %d using %s\n", *videos, author.ID, *driver)
}

type hybridOptions struct {
	users, authors, videos, followsPerUser, celebrityFollowers, fanoutMaxFollowers, likes int
}

// seedHybrid writes only to an empty, isolated database. It materializes the
// completed inbox index so read comparisons do not include initial backfill.
// Worker drain time must be measured separately through publication.
func seedHybrid(db *gorm.DB, opts hybridOptions) error {
	if opts.users < 3 || opts.users > 10000 || opts.authors < 2 || opts.authors >= opts.users ||
		opts.videos < 1 || opts.videos > 100000 || opts.followsPerUser < 1 || opts.followsPerUser > opts.authors-1 ||
		opts.celebrityFollowers <= opts.authors || opts.celebrityFollowers > opts.users || opts.fanoutMaxFollowers < 1 ||
		opts.celebrityFollowers-1 <= opts.fanoutMaxFollowers || opts.likes < 0 || opts.likes > 50*opts.videos ||
		(opts.likes > 0 && opts.users < 50) {
		return fmt.Errorf("invalid hybrid shape: require 2 <= authors < users, 1 <= ordinary follows < authors, and the first viewer to follow a high-follower author")
	}
	tables, err := db.Migrator().GetTables()
	if err != nil {
		return fmt.Errorf("inspect fixture database: %w", err)
	}
	for _, table := range tables {
		if table == "schema_migrations" || table == "sqlite_sequence" {
			continue
		}
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("refusing hybrid seed: %s already contains %d rows; use an empty isolated database", table, count)
		}
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		return fmt.Errorf("migrate isolated fixture database: %w", err)
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("load-fixture-password"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	accounts := make([]platform.User, opts.users)
	for i := range accounts {
		accounts[i] = platform.User{Username: fmt.Sprintf("load-u-%06d", i), Nickname: fmt.Sprintf("Load user %d", i), PasswordHash: string(passwordHash), CreatedAt: now.Add(-45 * 24 * time.Hour)}
	}
	if err := db.CreateInBatches(&accounts, 200).Error; err != nil {
		return fmt.Errorf("insert users: %w", err)
	}
	celebrityID := accounts[opts.authors-1].ID
	followers := make(map[uint][]uint, opts.authors)
	follows := make([]platform.Follow, 0, opts.users*opts.followsPerUser+opts.celebrityFollowers)
	followedAt := now.Add(-30 * 24 * time.Hour)
	for i, account := range accounts {
		if i >= opts.authors {
			for slot := 0; slot < opts.followsPerUser; slot++ {
				authorIndex := (i + slot) % (opts.authors - 1)
				authorID := accounts[authorIndex].ID
				follows = append(follows, platform.Follow{FollowerID: account.ID, FolloweeID: authorID, CreatedAt: followedAt})
				followers[authorID] = append(followers[authorID], account.ID)
			}
		}
		if i < opts.celebrityFollowers && account.ID != celebrityID {
			follows = append(follows, platform.Follow{FollowerID: account.ID, FolloweeID: celebrityID, CreatedAt: followedAt})
			followers[celebrityID] = append(followers[celebrityID], account.ID)
		}
	}
	if len(followers[celebrityID]) <= opts.fanoutMaxFollowers {
		return fmt.Errorf("actual high-follower count %d does not exceed threshold %d", len(followers[celebrityID]), opts.fanoutMaxFollowers)
	}
	for i := 0; i < opts.authors-1; i++ {
		if len(followers[accounts[i].ID]) > opts.fanoutMaxFollowers {
			return fmt.Errorf("ordinary author %d has %d followers above threshold", i, len(followers[accounts[i].ID]))
		}
	}
	if err := db.CreateInBatches(&follows, 500).Error; err != nil {
		return fmt.Errorf("insert follows: %w", err)
	}
	likeGroups := (opts.likes + 49) / 50
	likesByVideo := make(map[int]int64, likeGroups)
	for group := 0; group < likeGroups; group++ {
		videoIndex := group * opts.videos / likeGroups
		likesByVideo[videoIndex] = int64(min(50, opts.likes-group*50))
	}
	videoIDs := make([]uint, opts.videos)
	const videoBatch = 100
	inboxRows := 0
	jobRows := 0
	for offset := 0; offset < opts.videos; offset += videoBatch {
		count := min(videoBatch, opts.videos-offset)
		batch := make([]platform.Video, count)
		for j := range batch {
			i := offset + j
			batch[j] = platform.Video{AuthorID: accounts[i%opts.authors].ID, Title: fmt.Sprintf("load video %06d", i), MediaURL: "/media/load-fixture.mp4", Status: "published", FeedPull: accounts[i%opts.authors].ID == celebrityID, LikeCount: likesByVideo[i], CreatedAt: now.Add(-time.Duration(opts.videos-i) * time.Second)}
		}
		if err := db.CreateInBatches(&batch, videoBatch).Error; err != nil {
			return fmt.Errorf("insert videos at offset %d: %w", offset, err)
		}
		// GORM treats a false bool as the default on INSERT, so explicitly mark
		// completed ordinary-author rows after their IDs have been assigned.
		ordinaryIDs := make([]uint, 0, len(batch))
		for _, video := range batch {
			if video.AuthorID != celebrityID {
				ordinaryIDs = append(ordinaryIDs, video.ID)
			}
		}
		if err := db.Model(&platform.Video{}).Where("id IN ?", ordinaryIDs).Update("feed_pull", false).Error; err != nil {
			return fmt.Errorf("mark completed ordinary videos at offset %d: %w", offset, err)
		}
		for j, video := range batch {
			videoIDs[offset+j] = video.ID
		}
		jobs := make([]platform.FeedFanoutJob, 0, count)
		inboxes := make([]platform.FeedInbox, 0, count*opts.followsPerUser*opts.users/opts.authors)
		for _, video := range batch {
			if video.AuthorID == celebrityID {
				continue
			}
			completedAt := video.CreatedAt.Add(time.Second)
			jobs = append(jobs, platform.FeedFanoutJob{VideoID: video.ID, AuthorID: video.AuthorID, AvailableAt: video.CreatedAt, DoneAt: &completedAt, CreatedAt: video.CreatedAt})
			for _, followerID := range followers[video.AuthorID] {
				inboxes = append(inboxes, platform.FeedInbox{UserID: followerID, VideoID: video.ID, CreatedAt: video.CreatedAt})
			}
		}
		if len(jobs) > 0 {
			if err := db.CreateInBatches(&jobs, 500).Error; err != nil {
				return fmt.Errorf("insert fanout jobs at offset %d: %w", offset, err)
			}
		}
		if len(inboxes) > 0 {
			if err := db.CreateInBatches(&inboxes, 500).Error; err != nil {
				return fmt.Errorf("insert inbox rows at offset %d: %w", offset, err)
			}
		}
		jobRows += len(jobs)
		inboxRows += len(inboxes)
	}
	for offset := 0; offset < opts.likes; offset += 500 {
		batch := make([]platform.VideoAction, min(500, opts.likes-offset))
		for j := range batch {
			i := offset + j
			group := i / 50
			videoIndex := group * opts.videos / likeGroups
			userIndex := (group*53 + i%50) % opts.users
			batch[j] = platform.VideoAction{UserID: accounts[userIndex].ID, VideoID: videoIDs[videoIndex], Kind: "like", CreatedAt: now.Add(-time.Duration(opts.likes-i) * time.Millisecond)}
		}
		if err := db.CreateInBatches(&batch, 500).Error; err != nil {
			return fmt.Errorf("insert likes at offset %d: %w", offset, err)
		}
	}
	fmt.Printf("hybrid fixture users=%d authors=%d follows=%d videos=%d likes=%d completed_jobs=%d inbox_rows=%d high_author_followers=%d threshold=%d\n", opts.users, opts.authors, len(follows), opts.videos, opts.likes, jobRows, inboxRows, len(followers[celebrityID]), opts.fanoutMaxFollowers)
	fmt.Printf("viewer username=%s password=load-fixture-password; use only in this isolated database\n", accounts[opts.authors].Username)
	return nil
}
