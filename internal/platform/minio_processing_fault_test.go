package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// The proxy stores each MP4 output in real MinIO, then drops the successful
// response. This reproduces the uncertain result of an interrupted output PUT.
func TestMinIOProcessedOutputLostResponse(t *testing.T) {
	endpoint := os.Getenv("GOSTREAM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GOSTREAM_TEST_MINIO_ENDPOINT to a disposable MinIO server")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gostream-output-" + hex.EncodeToString(nonce[:])
	ctx := context.Background()
	access := os.Getenv("GOSTREAM_TEST_MINIO_ACCESS_KEY")
	secret := os.Getenv("GOSTREAM_TEST_MINIO_SECRET_KEY")
	direct, err := NewMinIOMediaStore(ctx, endpoint, access, secret, bucket, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := direct.removePrefix(cleanCtx, "media/"); err != nil {
			t.Errorf("remove media: %v", err)
		}
		if err := direct.client.RemoveBucket(cleanCtx, bucket); err != nil {
			t.Errorf("remove bucket: %v", err)
		}
	})
	upstream, err := url.Parse("http://" + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	// 1 fails the video PUT, 2 fails the cover PUT, and 0 allows recovery.
	var failKind atomic.Int32
	var committed atomic.Int32
	failKind.Store(1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := failKind.Load()
		if kind != 0 && r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/"+bucket+"/media/") &&
			((kind == 1 && strings.HasSuffix(r.URL.Path, ".mp4")) || (kind == 2 && strings.HasSuffix(r.URL.Path, ".jpg"))) {
			response := httptest.NewRecorder()
			reverse.ServeHTTP(response, r)
			if response.Code >= 200 && response.Code < 300 {
				committed.Add(1)
				http.Error(w, "injected lost PutObject response", http.StatusServiceUnavailable)
				return
			}
			for name, values := range response.Header() {
				w.Header()[name] = values
			}
			w.WriteHeader(response.Code)
			_, _ = response.Body.WriteTo(w)
			return
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	throughProxy, err := OpenExistingMinIOMediaStore(ctx, proxyURL.Host, access, secret, bucket, false)
	if err != nil {
		t.Fatal(err)
	}
	var server *Server
	var db *gorm.DB
	if dsn := os.Getenv("GOSTREAM_TEST_MINIO_MYSQL_DSN"); dsn != "" {
		db, err = gorm.Open(gormmysql.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
		if !db.Migrator().HasTable(&VideoProcessJob{}) {
			t.Fatal("MySQL schema is not migrated")
		}
		server = NewServer(db, "test-signing-secret-at-least-32-bytes", t.TempDir())
	} else {
		server, _, db = testServer(t)
	}
	server.UseObjectMedia(throughProxy)
	ownerID, _ := registerTestUser(t, server.Router(), "output_"+hex.EncodeToString(nonce[:]))
	video := Video{AuthorID: ownerID, Title: "Interrupted output", MediaURL: "/media/source.mp4", Status: "processing"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	job := VideoProcessJob{VideoID: video.ID, SourceURL: video.MediaURL, AvailableAt: time.Now().UTC().Add(-time.Second)}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := server.claimVideoProcessJob()
	if err != nil || claimed == nil {
		t.Fatalf("claim first attempt: %v %+v", err, claimed)
	}
	workDir := t.TempDir()
	output := workDir + "/video.mp4"
	cover := workDir + "/cover.jpg"
	var coverData bytes.Buffer
	if err := jpeg.Encode(&coverData, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, sampleMP4, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cover, coverData.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := videoMetadata{DurationMS: 1000, Width: 16, Height: 16}
	failedName, err := randomMediaName(".mp4")
	if err != nil {
		t.Fatal(err)
	}
	failedCover, err := randomMediaName(".jpg")
	if err != nil {
		t.Fatal(err)
	}
	err = server.finishProcessedVideoJob(ctx, *claimed, output, cover, failedName, failedCover, int64(len(sampleMP4)), int64(coverData.Len()), metadata)
	if err == nil || committed.Load() == 0 {
		t.Fatalf("lost output response was not exercised: err=%v commits=%d", err, committed.Load())
	}
	if _, err := direct.stat(ctx, "media/"+failedName); !objectMissing(err) {
		t.Fatalf("unregistered MP4 output remained: %v", err)
	}
	if _, err := direct.stat(ctx, "media/"+failedCover); !objectMissing(err) {
		t.Fatalf("unregistered cover remained: %v", err)
	}
	server.failVideoJob(*claimed, err)
	if err := db.First(&video, video.ID).Error; err != nil || video.Status != "processing" || video.MediaURL != job.SourceURL {
		t.Fatalf("failed output became public: %+v %v", video, err)
	}
	if err := db.First(&job, job.ID).Error; err != nil || job.Attempts != 1 || job.LeaseUntil != nil || job.DoneAt != nil || job.DeadAt != nil {
		t.Fatalf("first attempt state: %+v %v", job, err)
	}
	var assets int64
	if err := db.Model(&MediaAsset{}).Where("owner_id = ?", ownerID).Count(&assets).Error; err != nil || assets != 0 {
		t.Fatalf("failed output registered assets: %d %v", assets, err)
	}
	failKind.Store(2)
	if err := db.Model(&VideoProcessJob{}).Where("id = ?", job.ID).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err = server.claimVideoProcessJob()
	if err != nil || claimed == nil || claimed.Attempts != 2 {
		t.Fatalf("claim retry: %v %+v", err, claimed)
	}
	failedName, err = randomMediaName(".mp4")
	if err != nil {
		t.Fatal(err)
	}
	failedCover, err = randomMediaName(".jpg")
	if err != nil {
		t.Fatal(err)
	}
	err = server.finishProcessedVideoJob(ctx, *claimed, output, cover, failedName, failedCover, int64(len(sampleMP4)), int64(coverData.Len()), metadata)
	if err == nil || committed.Load() < 2 {
		t.Fatalf("lost cover response was not exercised: err=%v commits=%d", err, committed.Load())
	}
	if _, err := direct.stat(ctx, "media/"+failedName); !objectMissing(err) {
		t.Fatalf("video output remained after cover failure: %v", err)
	}
	if _, err := direct.stat(ctx, "media/"+failedCover); !objectMissing(err) {
		t.Fatalf("unregistered cover remained after failed PUT: %v", err)
	}
	server.failVideoJob(*claimed, err)
	if err := db.First(&video, video.ID).Error; err != nil || video.Status != "processing" {
		t.Fatalf("cover failure became public: %+v %v", video, err)
	}
	if err := db.Model(&MediaAsset{}).Where("owner_id = ?", ownerID).Count(&assets).Error; err != nil || assets != 0 {
		t.Fatalf("cover failure registered assets: %d %v", assets, err)
	}
	failKind.Store(0)
	if err := db.Model(&VideoProcessJob{}).Where("id = ?", job.ID).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err = server.claimVideoProcessJob()
	if err != nil || claimed == nil || claimed.Attempts != 3 {
		t.Fatalf("claim final retry: %v %+v", err, claimed)
	}
	finalName, err := randomMediaName(".mp4")
	if err != nil {
		t.Fatal(err)
	}
	finalCover, err := randomMediaName(".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.finishProcessedVideoJob(ctx, *claimed, output, cover, finalName, finalCover, int64(len(sampleMP4)), int64(coverData.Len()), metadata); err != nil {
		t.Fatalf("retry output: %v", err)
	}
	if err := db.First(&video, video.ID).Error; err != nil || video.Status != "published" || video.MediaURL != "/media/"+finalName || video.CoverURL != "/media/"+finalCover {
		t.Fatalf("retry publication: %+v %v", video, err)
	}
	if err := db.Model(&MediaAsset{}).Where("owner_id = ?", ownerID).Count(&assets).Error; err != nil || assets != 2 {
		t.Fatalf("retry assets: %d %v", assets, err)
	}
	if err := db.First(&job, job.ID).Error; err != nil || job.Attempts != 3 || job.DoneAt == nil || job.DeadAt != nil {
		t.Fatalf("retry job: %+v %v", job, err)
	}
	videoHash := sha256.Sum256(sampleMP4)
	if err := verifyObject(ctx, direct, "media/"+finalName, int64(len(sampleMP4)), hex.EncodeToString(videoHash[:])); err != nil {
		t.Fatalf("published MP4 bytes: %v", err)
	}
	coverHash := sha256.Sum256(coverData.Bytes())
	if err := verifyObject(ctx, direct, "media/"+finalCover, int64(coverData.Len()), hex.EncodeToString(coverHash[:])); err != nil {
		t.Fatalf("published cover bytes: %v", err)
	}
}
