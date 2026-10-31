package platform

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// BenchmarkTimelineFeed1000 measures a full HTTP handler plus indexed SQLite reads.
// It is an in-process baseline, not a production throughput measurement.
func BenchmarkTimelineFeed1000(b *testing.B) {
	db, err := gorm.Open(gormsqlite.Open(fmt.Sprintf("file:bench_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		b.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &Video{}); err != nil {
		b.Fatal(err)
	}
	author := User{Username: "benchmark", Nickname: "Benchmark", PasswordHash: "unused"}
	if err := db.Create(&author).Error; err != nil {
		b.Fatal(err)
	}
	items := make([]Video, 1000)
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for i := range items {
		items[i] = Video{AuthorID: author.ID, Title: fmt.Sprintf("Video %d", i), MediaURL: "/media/fixture.mp4", Status: "published", CreatedAt: base.Add(time.Duration(i) * time.Second)}
	}
	if err := db.CreateInBatches(items, 100).Error; err != nil {
		b.Fatal(err)
	}
	previousWriter := gin.DefaultWriter
	previousMode := gin.Mode()
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	defer func() {
		gin.DefaultWriter = previousWriter
		gin.SetMode(previousMode)
	}()
	s := NewServer(db, "benchmark-signing-secret-at-least-32-bytes", b.TempDir())
	h := s.Router()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/feed?scene=timeline&limit=20", nil)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			b.Fatalf("feed status %d: %s", res.Code, res.Body.String())
		}
	}
}

// BenchmarkFollowingFeed measures Following feed throughput and allocations under SQL-only vs Hybrid fanout.
func BenchmarkFollowingFeed(b *testing.B) {
	db, err := gorm.Open(gormsqlite.Open(fmt.Sprintf("file:bench_following_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		b.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := pool.Close(); err != nil {
			b.Error(err)
		}
	})
	if err := db.AutoMigrate(&User{}, &Video{}, &Follow{}, &FeedInbox{}, &FeedFanoutJob{}, &VideoAction{}); err != nil {
		b.Fatal(err)
	}
	previousWriter := gin.DefaultWriter
	previousMode := gin.Mode()
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	defer func() {
		gin.DefaultWriter = previousWriter
		gin.SetMode(previousMode)
	}()

	secret := "benchmark-signing-secret-at-least-32-bytes"
	s := NewServer(db, secret, b.TempDir())
	h := s.Router()

	author := User{Username: "author", Nickname: "Author", PasswordHash: "unused"}
	viewer := User{Username: "viewer", Nickname: "Viewer", PasswordHash: "unused"}
	if err := db.Create(&author).Error; err != nil {
		b.Fatal(err)
	}
	if err := db.Create(&viewer).Error; err != nil {
		b.Fatal(err)
	}
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	follow := Follow{FollowerID: viewer.ID, FolloweeID: author.ID, CreatedAt: base.Add(-time.Hour)}
	if err := db.Create(&follow).Error; err != nil {
		b.Fatal(err)
	}

	items := make([]Video, 500)
	for i := range items {
		items[i] = Video{AuthorID: author.ID, Title: fmt.Sprintf("Video %d", i), MediaURL: "/media/fixture.mp4", Status: "published", CreatedAt: base.Add(time.Duration(i) * time.Second), FeedPull: false}
	}
	if err := db.CreateInBatches(items, 100).Error; err != nil {
		b.Fatal(err)
	}
	// GORM's default:true replaces a zero bool on INSERT; set the completed
	// fanout state explicitly and verify the actual stored fixture.
	if err := db.Model(&Video{}).Where("author_id = ?", author.ID).UpdateColumn("feed_pull", false).Error; err != nil {
		b.Fatal(err)
	}
	var pullRows int64
	if err := db.Model(&Video{}).Where("feed_pull = ?", true).Count(&pullRows).Error; err != nil || pullRows != 0 {
		b.Fatalf("completed-push fixture has %d pull rows: %v", pullRows, err)
	}
	inboxes := make([]FeedInbox, 500)
	for i := range inboxes {
		inboxes[i] = FeedInbox{UserID: viewer.ID, VideoID: items[i].ID, CreatedAt: items[i].CreatedAt}
	}
	if err := db.CreateInBatches(inboxes, 100).Error; err != nil {
		b.Fatal(err)
	}

	token, err := s.issueToken(viewer.ID)
	if err != nil {
		b.Fatal(err)
	}

	checkPage := func(b *testing.B, r *httptest.ResponseRecorder) {
		b.Helper()
		var page struct {
			Items []Video `json:"items"`
			More  bool    `json:"has_more"`
			Next  string  `json:"next_cursor"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Items) != 20 || !page.More || page.Next == "" {
			b.Fatalf("invalid benchmark page: %d %s", r.Code, r.Body.String())
		}
		for i, item := range page.Items {
			if item.ID != items[499-i].ID {
				b.Fatalf("page ID=%d want=%d", item.ID, items[499-i].ID)
			}
		}
	}
	for _, mode := range []struct {
		name      string
		threshold int
	}{{"SQLOnly", 0}, {"Hybrid", 1000}} {
		b.Run(mode.name, func(b *testing.B) {
			s.UseFeedFanout(mode.threshold)
			read := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/api/feed?scene=following&limit=20", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				res := httptest.NewRecorder()
				h.ServeHTTP(res, req)
				return res
			}
			checkPage(b, read())
			b.ReportAllocs()
			b.ResetTimer()
			var last *httptest.ResponseRecorder
			for i := 0; i < b.N; i++ {
				last = read()
				if last.Code != 200 {
					b.Fatalf("status %d", last.Code)
				}
			}
			b.StopTimer()
			if last != nil {
				checkPage(b, last)
			}
		})
	}
}
