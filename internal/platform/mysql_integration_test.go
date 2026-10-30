package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestMySQLSearchLiteralPaginationIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	prefix := fmt.Sprintf("s%d", time.Now().UnixNano())
	user := User{Username: prefix + "_hit", Nickname: "search fixture", PasswordHash: "unused"}
	other := User{Username: prefix + "xhit", Nickname: "search fixture", PasswordHash: "unused"}
	for _, row := range []*User{&user, &other} {
		if err := tx.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	wanted := Video{AuthorID: user.ID, Title: prefix + "% clip", MediaURL: "/media/unused.mp4", Status: "published"}
	unwanted := Video{AuthorID: user.ID, Title: prefix + "x clip", MediaURL: "/media/unused.mp4", Status: "published"}
	for _, row := range []*Video{&wanted, &unwanted} {
		if err := tx.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	h := s.Router()
	response := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape(prefix+"_"), "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("MySQL user search: %d %s", response.Code, response.Body.String())
	}
	page := readSearchResponse(t, response.Body.Bytes())
	if len(page.Users) != 1 || page.Users[0].ID != user.ID {
		t.Fatalf("MySQL literal underscore: %+v", page.Users)
	}
	response = request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape(prefix+"%")+"&type=videos&limit=1", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("MySQL video search: %d %s", response.Code, response.Body.String())
	}
	page = readSearchResponse(t, response.Body.Bytes())
	if len(page.Videos) != 1 || page.Videos[0].ID != wanted.ID || page.HasMoreVideos {
		t.Fatalf("MySQL literal percent: %+v", page.Videos)
	}
}

func TestMySQLHybridFollowingIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	s.UseFeedFanout(1)
	h := s.Router()
	prefix := fmt.Sprintf("mf%x", time.Now().UnixNano()&0xffffff)
	lowAuthor, _ := registerTestUser(t, h, prefix+"low")
	highAuthor, _ := registerTestUser(t, h, prefix+"high")
	viewer, viewerToken := registerTestUser(t, h, prefix+"viewer")
	extra, _ := registerTestUser(t, h, prefix+"extra")
	for _, relation := range []Follow{{FollowerID: viewer, FolloweeID: lowAuthor}, {FollowerID: viewer, FolloweeID: highAuthor}, {FollowerID: extra, FolloweeID: highAuthor}} {
		if err := tx.Create(&relation).Error; err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Now().UTC().Truncate(time.Millisecond)
	low := Video{AuthorID: lowAuthor, Title: "low", MediaURL: "/media/mysql-low.mp4", Status: "published", CreatedAt: stamp}
	high := Video{AuthorID: highAuthor, Title: "high", MediaURL: "/media/mysql-high.mp4", Status: "published", CreatedAt: stamp}
	for _, video := range []*Video{&low, &high} {
		if err := tx.Create(video).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.enqueueFeedFanout(tx, *video); err != nil {
			t.Fatal(err)
		}
	}
	want := []uint{high.ID, low.ID}
	if got := collectFollowingIDs(t, h, viewerToken); !reflect.DeepEqual(got, want) {
		t.Fatalf("pending MySQL feed: got=%v want=%v", got, want)
	}
	// available_at is stored at MySQL millisecond precision. The next batch
	// can become eligible just after an immediate poll in the same millisecond.
	deadline := time.Now().Add(2 * time.Second)
	for {
		worked, err := s.ProcessFeedFanoutOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var job FeedFanoutJob
		if err := tx.Where("video_id = ?", low.ID).First(&job).Error; err != nil {
			t.Fatal(err)
		}
		if job.DoneAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("low-follower job not complete after bounded polling: %+v", job)
		}
		if !worked {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if got := collectFollowingIDs(t, h, viewerToken); !reflect.DeepEqual(got, want) {
		t.Fatalf("completed MySQL feed: got=%v want=%v", got, want)
	}
}

// This test commits its fixture because separate API instances must observe
// the same durable job and inbox rows through independent MySQL connections.
func TestMySQLFeedFanoutLeaseTakeoverIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}
	dbA, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	dbB, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	const followerCount = feedFanoutBatchSize + 2
	prefix := fmt.Sprintf("fan%x", time.Now().UnixNano())
	users := make([]User, followerCount+1)
	for i := range users {
		users[i] = User{Username: fmt.Sprintf("%s_%d", prefix, i), Nickname: "fanout fixture", PasswordHash: "unused"}
	}
	if err := dbA.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	userIDs := make([]uint, len(users))
	for i := range users {
		userIDs[i] = users[i].ID
	}
	var videoID uint
	t.Cleanup(func() {
		if err := dbA.Transaction(func(tx *gorm.DB) error {
			if videoID != 0 {
				for _, row := range []any{&FeedInbox{}, &FeedFanoutJob{}} {
					if err := tx.Where("video_id = ?", videoID).Delete(row).Error; err != nil {
						return err
					}
				}
				if err := tx.Delete(&Video{}, videoID).Error; err != nil {
					return err
				}
			}
			if err := tx.Where("followee_id = ?", users[0].ID).Delete(&Follow{}).Error; err != nil {
				return err
			}
			return tx.Where("id IN ?", userIDs).Delete(&User{}).Error
		}); err != nil {
			t.Errorf("clean fanout fixture: %v", err)
		}
	})
	follows := make([]Follow, followerCount)
	for i := range follows {
		follows[i] = Follow{FollowerID: users[i+1].ID, FolloweeID: users[0].ID}
	}
	if err := dbA.Create(&follows).Error; err != nil {
		t.Fatal(err)
	}
	sA := NewServer(dbA, "test-signing-secret-at-least-32-bytes", t.TempDir())
	sB := NewServer(dbB, "test-signing-secret-at-least-32-bytes", t.TempDir())
	sA.UseFeedFanout(followerCount)
	sB.UseFeedFanout(followerCount)
	video := Video{AuthorID: users[0].ID, Title: "lease takeover", MediaURL: "/media/fanout-fixture.mp4", Status: "published"}
	if err := dbA.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&video).Error; err != nil {
			return err
		}
		videoID = video.ID
		return sA.enqueueFeedFanout(tx, video)
	}); err != nil {
		t.Fatal(err)
	}
	token, err := sA.issueToken(users[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	hA, hB := sA.Router(), sB.Router()
	want := []uint{video.ID}
	if got := collectFollowingIDs(t, hB, token); !reflect.DeepEqual(got, want) {
		t.Fatalf("instance B pending feed: got=%v want=%v", got, want)
	}
	if worked, err := sA.ProcessFeedFanoutOnce(context.Background()); err != nil || !worked {
		t.Fatalf("instance A first batch: worked=%v err=%v", worked, err)
	}
	var job FeedFanoutJob
	if err := dbB.Where("video_id = ?", video.ID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.LastFollowerID == 0 || job.DoneAt != nil {
		t.Fatalf("expected partial durable progress: %+v", job)
	}
	var firstBatch int64
	if err := dbB.Model(&FeedInbox{}).Where("video_id = ?", video.ID).Count(&firstBatch).Error; err != nil || firstBatch != feedFanoutBatchSize {
		t.Fatalf("first committed batch: count=%d err=%v", firstBatch, err)
	}
	// Simulate A dying after a lease claim while its last batch is replayed.
	// B must wait for the lease, then safely insert only missing inbox rows.
	future := time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond)
	if err := dbA.Model(&FeedFanoutJob{}).Where("id = ?", job.ID).
		Updates(map[string]any{"last_follower_id": 0, "lease_until": future}).Error; err != nil {
		t.Fatal(err)
	}
	if worked, err := sB.ProcessFeedFanoutOnce(context.Background()); err != nil || worked {
		t.Fatalf("instance B reclaimed active lease: worked=%v err=%v", worked, err)
	}
	if got := collectFollowingIDs(t, hA, token); !reflect.DeepEqual(got, want) {
		t.Fatalf("instance A feed during abandoned lease: got=%v want=%v", got, want)
	}
	if err := dbA.Model(&FeedFanoutJob{}).Where("id = ?", job.ID).
		Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		worked, err := sB.ProcessFeedFanoutOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	if err := dbA.Where("id = ?", job.ID).First(&job).Error; err != nil || job.DoneAt == nil || job.Attempts < 4 {
		t.Fatalf("instance B did not finish replayed job: %+v err=%v", job, err)
	}
	var count, distinct int64
	if err := dbA.Model(&FeedInbox{}).Where("video_id = ?", video.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if err := dbA.Model(&FeedInbox{}).Where("video_id = ?", video.ID).
		Distinct("user_id").Count(&distinct).Error; err != nil {
		t.Fatal(err)
	}
	if count != followerCount || distinct != followerCount {
		t.Fatalf("takeover lost or duplicated inbox rows: count=%d distinct=%d want=%d", count, distinct, followerCount)
	}
	var completed Video
	if err := dbB.First(&completed, video.ID).Error; err != nil || completed.FeedPull {
		t.Fatalf("feed pull marker did not flip with completion: video=%+v err=%v", completed, err)
	}
	if got := collectFollowingIDs(t, hB, token); !reflect.DeepEqual(got, want) {
		t.Fatalf("instance B completed feed: got=%v want=%v", got, want)
	}
}

// Separate database handles model two API instances. The fixture is committed
// because MySQL connections cannot observe another connection's open tx.
func TestMySQLGroupMemberCapIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}
	dbA, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	dbB, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []*gorm.DB{dbA, dbB} {
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	const existingMembers, invitees = 199, 16
	prefix := fmt.Sprintf("gcap%x", time.Now().UnixNano())
	users := make([]User, existingMembers+invitees)
	for i := range users {
		users[i] = User{Username: fmt.Sprintf("%s_%d", prefix, i), Nickname: "group cap fixture", PasswordHash: "unused"}
	}
	var groupID uint
	t.Cleanup(func() {
		if err := dbA.Transaction(func(tx *gorm.DB) error {
			if groupID != 0 {
				var notices []Notification
				if err := tx.Where("event_key LIKE ?", fmt.Sprintf("groupinvite:%d:%%", groupID)).Find(&notices).Error; err != nil {
					return err
				}
				if len(notices) > 0 {
					noticeIDs := make([]uint, len(notices))
					outboxKeys := make([]string, len(notices))
					for i, notice := range notices {
						noticeIDs[i] = notice.ID
						outboxKeys[i] = fmt.Sprintf("notification:%d", notice.ID)
					}
					if err := tx.Where("event_key IN ?", outboxKeys).Delete(&OutboxEvent{}).Error; err != nil {
						return err
					}
					if err := tx.Where("id IN ?", noticeIDs).Delete(&Notification{}).Error; err != nil {
						return err
					}
				}
				for _, row := range []any{&GroupRead{}, &GroupMember{}} {
					if err := tx.Where("group_id = ?", groupID).Delete(row).Error; err != nil {
						return err
					}
				}
				if err := tx.Delete(&ChatGroup{}, groupID).Error; err != nil {
					return err
				}
			}
			ids := make([]uint, 0, len(users))
			for _, user := range users {
				if user.ID != 0 {
					ids = append(ids, user.ID)
				}
			}
			if len(ids) == 0 {
				return nil
			}
			return tx.Where("id IN ?", ids).Delete(&User{}).Error
		}); err != nil {
			t.Errorf("clean group cap fixture: %v", err)
		}
	})
	if err := dbA.CreateInBatches(&users, 100).Error; err != nil {
		t.Fatal(err)
	}
	group := ChatGroup{OwnerID: users[0].ID, Name: "member cap fixture"}
	if err := dbA.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	groupID = group.ID
	members := make([]GroupMember, existingMembers)
	for i := range members {
		members[i] = GroupMember{GroupID: group.ID, UserID: users[i].ID, JoinedAt: time.Now().UTC()}
	}
	if err := dbA.CreateInBatches(&members, 100).Error; err != nil {
		t.Fatal(err)
	}
	sA := NewServer(dbA, "test-signing-secret-at-least-32-bytes", t.TempDir())
	sB := NewServer(dbB, "test-signing-secret-at-least-32-bytes", t.TempDir())
	token, err := sA.issueToken(users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	routers := []http.Handler{sA.Router(), sB.Router()}
	path := fmt.Sprintf("/api/groups/%d/members", group.ID)
	start := make(chan struct{})
	statuses := make(chan int, invitees)
	var wg sync.WaitGroup
	for i := 0; i < invitees; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"user_id":%d}`, users[existingMembers+i].ID)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			routers[i%2].ServeHTTP(res, req)
			statuses <- res.Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(statuses)
	var admitted, conflicts, unexpected int
	for status := range statuses {
		switch status {
		case http.StatusOK:
			admitted++
		case http.StatusConflict:
			conflicts++
		default:
			unexpected++
		}
	}
	var count int64
	if err := dbA.Model(&GroupMember{}).Where("group_id = ?", group.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if admitted != 1 || conflicts != invitees-1 || unexpected != 0 || count != 200 {
		t.Fatalf("concurrent invites: admitted=%d conflicts=%d unexpected=%d members=%d", admitted, conflicts, unexpected, count)
	}
	duplicate := request(t, routers[0], http.MethodPost, path, token, map[string]uint{"user_id": users[0].ID})
	if duplicate.Code != http.StatusOK {
		t.Fatalf("duplicate at full capacity: %d %s", duplicate.Code, duplicate.Body.String())
	}
	var notices []Notification
	if err := dbA.Where("event_key LIKE ?", fmt.Sprintf("groupinvite:%d:%%", group.ID)).Find(&notices).Error; err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 {
		t.Fatalf("invitation notifications = %d, want 1", len(notices))
	}
	var readCount, outboxCount int64
	if err := dbA.Model(&GroupRead{}).Where("group_id = ?", group.ID).Count(&readCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := dbA.Model(&OutboxEvent{}).Where("event_key = ?", fmt.Sprintf("notification:%d", notices[0].ID)).Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if readCount != 1 || outboxCount != 1 {
		t.Fatalf("invitation side effects: reads=%d outbox=%d", readCount, outboxCount)
	}
}

func TestMySQLOutboxLeaseHandoffIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}
	dbA, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	dbB, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []*gorm.DB{dbA, dbB} {
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	checkOutboxLeaseHandoff(t, dbA, dbB)
}

func TestMySQLVideoSourceRecoveryWithoutProcessor(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	server := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	checkVideoSourceReplacementRequiresProcessor(t, server, server.Router(), tx)
}

func TestMySQLConcurrentVideoActionsIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}
	dbA, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	dbB, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	user := User{Username: fmt.Sprintf("action%x", time.Now().UnixNano()), Nickname: "action fixture", PasswordHash: "unused"}
	if err := dbA.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	var videoID uint
	t.Cleanup(func() {
		if err := dbA.Transaction(func(tx *gorm.DB) error {
			if videoID != 0 {
				if err := tx.Where("video_id = ?", videoID).Delete(&VideoAction{}).Error; err != nil {
					return err
				}
				if err := tx.Delete(&Video{}, videoID).Error; err != nil {
					return err
				}
			}
			return tx.Delete(&User{}, user.ID).Error
		}); err != nil {
			t.Errorf("clean action fixture: %v", err)
		}
	})
	video := Video{AuthorID: user.ID, Title: "action contention", MediaURL: "/media/action-fixture.mp4", Status: "published"}
	if err := dbA.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	videoID = video.ID
	sA := NewServer(dbA, "test-signing-secret-at-least-32-bytes", t.TempDir())
	sB := NewServer(dbB, "test-signing-secret-at-least-32-bytes", t.TempDir())
	token, err := sA.issueToken(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	handlers := []http.Handler{sA.Router(), sB.Router()}
	const workers, iterations = 8, 80
	failures := make(chan string, workers*iterations)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				kind := "like"
				if (worker+i)%4 >= 2 {
					kind = "favorite"
				}
				method := http.MethodPut
				if (worker+i)%2 == 1 {
					method = http.MethodDelete
				}
				req := httptest.NewRequest(method, fmt.Sprintf("/api/videos/%d/%s", video.ID, kind), nil)
				req.Header.Set("Authorization", "Bearer "+token)
				res := httptest.NewRecorder()
				handlers[worker%2].ServeHTTP(res, req)
				if res.Code != http.StatusOK {
					failures <- fmt.Sprintf("worker=%d attempt=%d method=%s kind=%s status=%d body=%s", worker, i, method, kind, res.Code, res.Body.String())
				}
			}
		}(worker)
	}
	wg.Wait()
	close(failures)
	var failed []string
	for failure := range failures {
		if len(failed) < 5 {
			failed = append(failed, failure)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("concurrent action returned errors: %v", failed)
	}
	var persisted Video
	if err := dbA.First(&persisted, video.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		kind string
		want int64
	}{{"like", persisted.LikeCount}, {"favorite", persisted.FavoriteCount}} {
		var rows int64
		if err := dbB.Model(&VideoAction{}).Where("video_id = ? AND kind = ?", video.ID, check.kind).Count(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if rows != check.want || rows < 0 || rows > 1 {
			t.Fatalf("%s count mismatch after concurrent toggles: rows=%d counter=%d", check.kind, rows, check.want)
		}
	}
}

func TestMySQLRegisteredMediaReconciliationIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []struct {
		model any
		name  string
	}{
		{&MediaAsset{}, "idx_media_assets_created_at"},
		{&Video{}, "idx_videos_media_url"},
		{&Video{}, "idx_videos_cover_url"},
		{&User{}, "idx_users_avatar_url"},
		{&VideoProcessJob{}, "idx_video_process_jobs_source_url"},
	} {
		if !db.Migrator().HasIndex(index.model, index.name) {
			t.Fatalf("media cleanup index %s is missing; start the migrated API first", index.name)
		}
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	old := now.Add(-mediaAssetGrace - time.Hour)
	user := User{Username: fmt.Sprintf("mysqlasset%d", now.UnixNano()), PasswordHash: "unused-test-credential", Nickname: "asset integration"}
	if err := tx.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	makeAsset := func(n int) (string, string) {
		t.Helper()
		name := fmt.Sprintf("%032x.mp4", now.UnixNano()+int64(n))
		path, url := filepath.Join(s.MediaDir, name), "/media/"+name
		if err := os.WriteFile(path, []byte("asset"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		if err := tx.Create(&MediaAsset{OwnerID: user.ID, URL: url, Kind: "video", Size: 5, CreatedAt: old}).Error; err != nil {
			t.Fatal(err)
		}
		return path, url
	}
	unusedPath, unusedURL := makeAsset(1)
	usedPath, usedURL := makeAsset(2)
	if err := tx.Create(&Video{AuthorID: user.ID, Title: "kept", MediaURL: usedURL, Status: "deleted"}).Error; err != nil {
		t.Fatal(err)
	}
	if removed, err := s.ReconcileUnusedAssets(context.Background(), now); err != nil || removed != 1 {
		t.Fatalf("MySQL media cleanup: removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(unusedPath); !os.IsNotExist(err) {
		t.Fatalf("unused file remains: %v", err)
	}
	if _, err := os.Stat(usedPath); err != nil {
		t.Fatalf("referenced file removed: %v", err)
	}
	var unused, used int64
	if err := tx.Model(&MediaAsset{}).Where("url = ?", unusedURL).Count(&unused).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Model(&MediaAsset{}).Where("url = ?", usedURL).Count(&used).Error; err != nil {
		t.Fatal(err)
	}
	if unused != 0 || used != 1 {
		t.Fatalf("MySQL media rows: unused=%d used=%d", unused, used)
	}
}

func TestMySQLMessageDeviceLifecycleIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable(&MessageDevice{}) || !db.Migrator().HasIndex(&GroupDeviceRead{}, "idx_group_device_owner") {
		t.Fatal("message device migration is missing; start the migrated API first")
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	user := User{Username: fmt.Sprintf("mysqldev%d", time.Now().UnixNano()), PasswordHash: "unused-test-credential", Nickname: "device integration"}
	if err := tx.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	if err := s.BackfillMessageDevices(context.Background()); err != nil {
		t.Fatalf("MySQL device backfill: %v", err)
	}
	if err := s.ensureMessageDevice(context.Background(), user.ID, testDeviceA); err != nil {
		t.Fatal(err)
	}
	if err := tx.Create(&MessageDeviceAck{UserID: user.ID, DeviceID: testDeviceA, MessageID: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Create(&GroupDeviceRead{GroupID: 1, UserID: user.ID, DeviceID: testDeviceA, LastAckID: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Model(&MessageDevice{}).Where("user_id = ?", user.ID).UpdateColumn("last_seen_at", time.Now().UTC().Add(-deviceExpirationAge-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := s.cleanupExpiredMessageDevices(context.Background(), time.Now(), 100)
	if err != nil || removed != 1 {
		t.Fatalf("MySQL device cleanup: removed=%d err=%v", removed, err)
	}
	for _, model := range []any{&MessageDevice{}, &MessageDeviceAck{}, &GroupDeviceRead{}} {
		var count int64
		if err := tx.Model(model).Where("user_id = ? AND device_id = ?", user.ID, testDeviceA).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("MySQL device state %T: count=%d err=%v", model, count, err)
		}
	}
}

func TestMySQLConcurrentDeviceRegistration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	s := NewServer(db, "test-signing-secret-at-least-32-bytes", t.TempDir())
	router := s.Router()
	username := fmt.Sprintf("mysqlconc%d", time.Now().UnixNano())
	userID, token := registerTestUser(t, router, username)
	t.Cleanup(func() {
		_ = db.Where("user_id = ?", userID).Delete(&MessageDevice{}).Error
		_ = db.Where("id = ?", userID).Delete(&User{}).Error
	})

	const totalAttempts = 40
	start := make(chan struct{})
	statuses := make(chan int, totalAttempts)
	var wg sync.WaitGroup

	for i := 1; i <= totalAttempts; i++ {
		wg.Add(1)
		deviceID := fmt.Sprintf("00000000-0000-4000-8000-%012x", i)
		go func(id string) {
			defer wg.Done()
			<-start
			res := requestOnDevice(t, router, http.MethodGet, "/api/messages/unread", token, id, nil)
			statuses <- res.Code
		}(deviceID)
	}

	close(start)
	wg.Wait()
	close(statuses)

	var okCount, rateLimitedCount, unexpectedCount int
	for status := range statuses {
		switch status {
		case http.StatusOK:
			okCount++
		case http.StatusTooManyRequests:
			rateLimitedCount++
		default:
			unexpectedCount++
		}
	}

	if unexpectedCount > 0 {
		t.Fatalf("unexpected statuses: %d (ok=%d, 429=%d)", unexpectedCount, okCount, rateLimitedCount)
	}
	if okCount != maxMessageDevices {
		t.Fatalf("okCount=%d want %d", okCount, maxMessageDevices)
	}
	if rateLimitedCount != totalAttempts-maxMessageDevices {
		t.Fatalf("rateLimitedCount=%d want %d", rateLimitedCount, totalAttempts-maxMessageDevices)
	}

	var count int64
	if err := db.Model(&MessageDevice{}).Where("user_id = ?", userID).Count(&count).Error; err != nil || count != maxMessageDevices {
		t.Fatalf("persisted message devices count=%d want %d, err=%v", count, maxMessageDevices, err)
	}
}

func TestMySQLNotificationPaginationIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasIndex(&Notification{}, "idx_notification_user_id") || !db.Migrator().HasIndex(&Notification{}, "idx_notification_unread") {
		t.Fatal("notification pagination index is missing; start the migrated API first")
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	h := s.Router()
	unique := time.Now().UnixNano()
	ownerID, token := registerTestUser(t, h, fmt.Sprintf("mn%d", unique))
	actorID, _ := registerTestUser(t, h, fmt.Sprintf("ma%d", unique))
	stamp := time.Now().UTC().Truncate(time.Second)
	var notices [3]Notification
	for i := range notices {
		notices[i] = Notification{UserID: ownerID, EventKey: fmt.Sprintf("mysql-notice-%d-%d", unique, i), Kind: "follow", ActorID: actorID, CreatedAt: stamp}
		if err := tx.Create(&notices[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	first := request(t, h, http.MethodGet, "/api/notifications?limit=2", token, nil)
	var page struct {
		Items      []Notification `json:"items"`
		NextCursor uint           `json:"next_cursor"`
		HasMore    bool           `json:"has_more"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Items) != 2 || !page.HasMore || page.Items[0].ID != notices[2].ID || page.Items[1].ID != notices[1].ID {
		t.Fatalf("MySQL first notification page: %d %s", first.Code, first.Body.String())
	}
	second := request(t, h, http.MethodGet, fmt.Sprintf("/api/notifications?limit=2&before_id=%d", page.NextCursor), token, nil)
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.HasMore || page.Items[0].ID != notices[0].ID {
		t.Fatalf("MySQL second notification page: %d %s", second.Code, second.Body.String())
	}
	read := request(t, h, http.MethodPatch, fmt.Sprintf("/api/notifications/read?up_to_id=%d", notices[1].ID), token, nil)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"read_count":2`) {
		t.Fatalf("MySQL bounded read: %d %s", read.Code, read.Body.String())
	}
}

// Run with GOSTREAM_TEST_MYSQL_DSN against a migrated, disposable MySQL database.
// All fixture rows are created inside one transaction and rolled back.
func TestMySQLRecommendationIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var existing int64
	if err := db.Model(&Video{}).Where("status = ?", "published").Count(&existing).Error; err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Fatalf("recommendation integration test needs an isolated database without published videos; found %d", existing)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	h := s.Router()
	stamp := time.Now().UnixNano() % 1_000_000_000
	users := make([]User, 4)
	for i := range users {
		users[i] = User{Username: fmt.Sprintf("mysqlrec%d%d", stamp, i), PasswordHash: "unused-test-credential", Nickname: "MySQL recommendation test"}
		if err := tx.Create(&users[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		if err := tx.Create(&Video{AuthorID: users[i].ID, Title: fmt.Sprintf("MySQL rank %d", i), MediaURL: "/media/test.mp4", Status: "published", CreatedAt: old}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Create(&Follow{FollowerID: users[3].ID, FolloweeID: users[0].ID}).Error; err != nil {
		t.Fatal(err)
	}
	history := Video{AuthorID: users[1].ID, Title: "prior action", MediaURL: "/media/prior.mp4", Status: "deleted", CreatedAt: old}
	if err := tx.Create(&history).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Create(&VideoAction{UserID: users[3].ID, VideoID: history.ID, Kind: "favorite"}).Error; err != nil {
		t.Fatal(err)
	}
	token, err := s.issueToken(users[3].ID)
	if err != nil {
		t.Fatal(err)
	}
	cursor := ""
	for i, want := range []uint{users[0].ID, users[1].ID, users[2].ID} {
		path := "/api/feed?scene=recommend&limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		res := request(t, h, http.MethodGet, path, token, nil)
		var page struct {
			Items      []Video `json:"items"`
			NextCursor string  `json:"next_cursor"`
		}
		if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].AuthorID != want {
			t.Fatalf("MySQL recommendation page %d: %d %s", i, res.Code, res.Body.String())
		}
		cursor = page.NextCursor
	}
}

// This catches MySQL strict-mode rejection of a zero DATETIME lease. The
// database schema must already be migrated by the application.
func TestMySQLResumableUploadLeaseIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	h := s.Router()
	_, token := registerTestUser(t, h, fmt.Sprintf("mysqlupload%d", time.Now().UnixNano()))
	created := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{"filename": "clip.mp4", "size": len(sampleMP4)})
	var session UploadSession
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &session) != nil || session.ID == "" {
		t.Fatalf("create upload session: %d %s", created.Code, created.Body.String())
	}
	if err := tx.First(&session, "id = ?", session.ID).Error; err != nil || session.LeaseUntil != nil {
		t.Fatalf("new session lease: %+v %v", session.LeaseUntil, err)
	}
	path := "/api/uploads/sessions/" + session.ID
	if res := request(t, h, http.MethodPost, path+"/complete", token, nil); res.Code != http.StatusConflict {
		t.Fatalf("incomplete session: %d %s", res.Code, res.Body.String())
	}
	if err := tx.First(&session, "id = ?", session.ID).Error; err != nil || session.Status != "open" || session.LeaseUntil != nil {
		t.Fatalf("failed completion did not clear lease: %+v %v", session, err)
	}
	if res := putTestChunk(h, token, session.ID, 0, sampleMP4); res.Code != http.StatusOK {
		t.Fatalf("upload chunk: %d %s", res.Code, res.Body.String())
	}
	if res := request(t, h, http.MethodPost, path+"/complete", token, nil); res.Code != http.StatusOK {
		t.Fatalf("complete upload: %d %s", res.Code, res.Body.String())
	}
	if err := tx.First(&session, "id = ?", session.ID).Error; err != nil || session.Status != "completed" || session.LeaseUntil != nil {
		t.Fatalf("completed session lease: %+v %v", session, err)
	}
}

// Exercises the comment thread index and equal-timestamp cursor against the
// MySQL datetime precision used by the Compose database.
func TestMySQLCommentThreadsIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasIndex(&Comment{}, "idx_comment_thread") {
		t.Fatal("comment thread index is missing; start the migrated API first")
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	h := s.Router()
	unique := time.Now().UnixNano()
	authorID, _ := registerTestUser(t, h, fmt.Sprintf("mysqlca%d", unique))
	_, commenterToken := registerTestUser(t, h, fmt.Sprintf("mysqlcb%d", unique))
	video := Video{AuthorID: authorID, Title: "thread", MediaURL: "/media/thread.mp4", Status: "published"}
	if err := tx.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/videos/%d/comments", video.ID)
	ids := make([]uint, 0, 3)
	for _, body := range []string{"root one", "root two"} {
		res := request(t, h, http.MethodPost, path, commenterToken, map[string]any{"body": body})
		var comment Comment
		if res.Code != http.StatusCreated || json.Unmarshal(res.Body.Bytes(), &comment) != nil {
			t.Fatalf("create root: %d %s", res.Code, res.Body.String())
		}
		ids = append(ids, comment.ID)
	}
	reply := request(t, h, http.MethodPost, path, commenterToken, map[string]any{"body": "reply", "parent_id": ids[0]})
	if reply.Code != http.StatusCreated {
		t.Fatalf("create reply: %d %s", reply.Code, reply.Body.String())
	}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := tx.Model(&Comment{}).Where("video_id = ?", video.ID).UpdateColumn("created_at", stamp).Error; err != nil {
		t.Fatal(err)
	}
	first := request(t, h, http.MethodGet, path+"?limit=1", "", nil)
	var page1 commentPageResponse
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page1) != nil || len(page1.Items) != 1 || page1.Items[0].ID != ids[1] || !page1.HasMore {
		t.Fatalf("first MySQL page: %d %s", first.Code, first.Body.String())
	}
	second := request(t, h, http.MethodGet, path+"?limit=1&cursor="+page1.NextCursor, "", nil)
	var page2 commentPageResponse
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &page2) != nil || len(page2.Items) != 1 || page2.Items[0].ID != ids[0] || page2.Items[0].ReplyCount != 1 || page2.HasMore {
		t.Fatalf("second MySQL page: %d %s", second.Code, second.Body.String())
	}
}

func TestMySQLMultiUserConcurrentInteractionsAndCounters(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
	}
	dbA, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	dbB, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	prefix := fmt.Sprintf("mxct%x", time.Now().UnixNano()&0xffffff)
	var userIDs []uint
	var videoIDs []uint

	t.Cleanup(func() {
		_ = dbA.Transaction(func(tx *gorm.DB) error {
			if len(videoIDs) > 0 {
				_ = tx.Where("video_id IN ?", videoIDs).Delete(&VideoAction{}).Error
				_ = tx.Where("video_id IN ?", videoIDs).Delete(&Comment{}).Error
				_ = tx.Where("id IN ?", videoIDs).Delete(&Video{}).Error
			}
			if len(userIDs) > 0 {
				_ = tx.Where("id IN ?", userIDs).Delete(&User{}).Error
			}
			return nil
		})
	})

	sA := NewServer(dbA, "test-signing-secret-at-least-32-bytes", t.TempDir())
	sB := NewServer(dbB, "test-signing-secret-at-least-32-bytes", t.TempDir())
	handlers := []http.Handler{sA.Router(), sB.Router()}

	// Register 1 author
	author := User{Username: prefix + "_author", Nickname: "Author", PasswordHash: "unused"}
	if err := dbA.Create(&author).Error; err != nil {
		t.Fatal(err)
	}
	userIDs = append(userIDs, author.ID)

	// Create 3 videos
	for i := 1; i <= 3; i++ {
		v := Video{
			AuthorID: author.ID,
			Title:    fmt.Sprintf("%s-video-%d", prefix, i),
			MediaURL: fmt.Sprintf("/media/%s-%d.mp4", prefix, i),
			Status:   "published",
		}
		if err := dbA.Create(&v).Error; err != nil {
			t.Fatal(err)
		}
		videoIDs = append(videoIDs, v.ID)
	}

	// Register 4 interacting users
	const numUsers = 4
	tokens := make([]string, numUsers)
	for i := 0; i < numUsers; i++ {
		u := User{Username: fmt.Sprintf("%s_u%d", prefix, i), Nickname: fmt.Sprintf("User %d", i), PasswordHash: "unused"}
		if err := dbA.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
		userIDs = append(userIDs, u.ID)
		tok, err := sA.issueToken(u.ID)
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = tok
	}

	// Concurrently perform like, unlike, favorite, unfavorite, and comments across videos
	const opsPerUser = 20
	failures := make(chan string, numUsers*opsPerUser)
	var wg sync.WaitGroup

	for uIdx, token := range tokens {
		wg.Add(1)
		go func(userToken string, u int) {
			defer wg.Done()
			for op := 0; op < opsPerUser; op++ {
				vid := videoIDs[(u+op)%len(videoIDs)]
				router := handlers[(u+op)%len(handlers)]

				switch op % 5 {
				case 0:
					req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/videos/%d/like", vid), nil)
					req.Header.Set("Authorization", "Bearer "+userToken)
					res := httptest.NewRecorder()
					router.ServeHTTP(res, req)
					if res.Code != http.StatusOK {
						failures <- fmt.Sprintf("u=%d op=%d PUT like vid=%d: %d %s", u, op, vid, res.Code, res.Body.String())
					}
				case 1:
					req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/videos/%d/like", vid), nil)
					req.Header.Set("Authorization", "Bearer "+userToken)
					res := httptest.NewRecorder()
					router.ServeHTTP(res, req)
					if res.Code != http.StatusOK {
						failures <- fmt.Sprintf("u=%d op=%d DELETE like vid=%d: %d %s", u, op, vid, res.Code, res.Body.String())
					}
				case 2:
					req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/videos/%d/favorite", vid), nil)
					req.Header.Set("Authorization", "Bearer "+userToken)
					res := httptest.NewRecorder()
					router.ServeHTTP(res, req)
					if res.Code != http.StatusOK {
						failures <- fmt.Sprintf("u=%d op=%d PUT fav vid=%d: %d %s", u, op, vid, res.Code, res.Body.String())
					}
				case 3:
					req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/videos/%d/favorite", vid), nil)
					req.Header.Set("Authorization", "Bearer "+userToken)
					res := httptest.NewRecorder()
					router.ServeHTTP(res, req)
					if res.Code != http.StatusOK {
						failures <- fmt.Sprintf("u=%d op=%d DELETE fav vid=%d: %d %s", u, op, vid, res.Code, res.Body.String())
					}
				case 4:
					body := fmt.Sprintf(`{"body":"mysql comment u%d op%d"}`, u, op)
					req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", vid), strings.NewReader(body))
					req.Header.Set("Authorization", "Bearer "+userToken)
					req.Header.Set("Content-Type", "application/json")
					res := httptest.NewRecorder()
					router.ServeHTTP(res, req)
					if res.Code != http.StatusCreated {
						failures <- fmt.Sprintf("u=%d op=%d POST comment vid=%d: %d %s", u, op, vid, res.Code, res.Body.String())
					}
				}
			}
		}(token, uIdx)
	}

	wg.Wait()
	close(failures)

	var failed []string
	for f := range failures {
		if len(failed) < 5 {
			failed = append(failed, f)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("MySQL concurrent interaction failures: %v", failed)
	}

	// Verify row-counter parity and non-negative counters on MySQL
	for _, vid := range videoIDs {
		var v Video
		if err := dbA.First(&v, vid).Error; err != nil {
			t.Fatal(err)
		}
		var actualLikes int64
		if err := dbB.Model(&VideoAction{}).Where("video_id = ? AND kind = ?", vid, "like").Count(&actualLikes).Error; err != nil {
			t.Fatal(err)
		}
		var actualFavs int64
		if err := dbB.Model(&VideoAction{}).Where("video_id = ? AND kind = ?", vid, "favorite").Count(&actualFavs).Error; err != nil {
			t.Fatal(err)
		}
		var actualComments int64
		if err := dbB.Model(&Comment{}).Where("video_id = ?", vid).Count(&actualComments).Error; err != nil {
			t.Fatal(err)
		}

		if v.LikeCount < 0 || v.FavoriteCount < 0 || v.CommentCount < 0 {
			t.Fatalf("negative counter for video %d: likes=%d favs=%d cmts=%d", vid, v.LikeCount, v.FavoriteCount, v.CommentCount)
		}
		if v.LikeCount != actualLikes {
			t.Fatalf("video %d like_count mismatch: counter=%d rows=%d", vid, v.LikeCount, actualLikes)
		}
		if v.FavoriteCount != actualFavs {
			t.Fatalf("video %d favorite_count mismatch: counter=%d rows=%d", vid, v.FavoriteCount, actualFavs)
		}
		if v.CommentCount != actualComments {
			t.Fatalf("video %d comment_count mismatch: counter=%d rows=%d", vid, v.CommentCount, actualComments)
		}
	}
}
