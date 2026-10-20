package platform

import (
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
	gin.DefaultWriter = io.Discard
	defer func() { gin.DefaultWriter = previousWriter }()
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
