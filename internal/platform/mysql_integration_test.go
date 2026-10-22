package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

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
