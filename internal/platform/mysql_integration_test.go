package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
