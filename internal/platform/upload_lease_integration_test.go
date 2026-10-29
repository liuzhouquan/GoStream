package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Two independent MySQL connections share a real MinIO bucket. A proxy pauses
// the completing worker's chunk GET while another worker attempts cleanup.
func TestMinIOUploadCompletionCleanupLease(t *testing.T) {
	endpoint := os.Getenv("GOSTREAM_TEST_MINIO_ENDPOINT")
	dsn := os.Getenv("GOSTREAM_TEST_UPLOAD_LEASE_MYSQL_DSN")
	if endpoint == "" || dsn == "" {
		t.Skip("requires real MinIO and GOSTREAM_TEST_UPLOAD_LEASE_MYSQL_DSN pointing to an empty migrated disposable schema")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(nonce[:])
	bucket := "gostream-lease-" + tag
	ctx := context.Background()
	access, secret := os.Getenv("GOSTREAM_TEST_MINIO_ACCESS_KEY"), os.Getenv("GOSTREAM_TEST_MINIO_SECRET_KEY")
	store, err := NewMinIOMediaStore(ctx, endpoint, access, secret, bucket, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, prefix := range []string{"chunks/", "media/"} {
			if err := store.removePrefix(cleanCtx, prefix); err != nil {
				t.Errorf("remove %s: %v", prefix, err)
			}
		}
		if err := store.client.RemoveBucket(cleanCtx, bucket); err != nil {
			t.Errorf("remove bucket: %v", err)
		}
	})
	var dbs [2]*gorm.DB
	for i := range dbs {
		db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
		dbs[i] = db
	}
	var count int64
	if err := dbs[0].Model(&UploadSession{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("requires an empty migrated upload schema: sessions=%d err=%v", count, err)
	}
	upstream, err := url.Parse("http://" + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/"+bucket+"/chunks/") && blocked.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	t.Cleanup(unblock)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxied, err := OpenExistingMinIOMediaStore(ctx, proxyURL.Host, access, secret, bucket, false)
	if err != nil {
		t.Fatal(err)
	}
	s1 := NewServer(dbs[0], "test-signing-secret-at-least-32-bytes", t.TempDir())
	s1.uploadRenewEvery = 10 * time.Millisecond
	s1.UseObjectMedia(proxied)
	s2 := NewServer(dbs[1], string(s1.Secret), t.TempDir())
	s2.UseObjectMedia(store)
	h1, h2 := s1.Router(), s2.Router()
	owner, token := registerTestUser(t, h1, "lease_"+tag)
	var sessionIDs []string
	t.Cleanup(func() {
		for _, id := range sessionIDs {
			if err := dbs[0].Where("session_id = ?", id).Delete(&UploadChunk{}).Error; err != nil {
				t.Errorf("delete test chunks: %v", err)
			}
		}
		if err := dbs[0].Where("owner_id = ?", owner).Delete(&UploadSession{}).Error; err != nil {
			t.Errorf("delete test sessions: %v", err)
		}
		if err := dbs[0].Where("owner_id = ?", owner).Delete(&MediaAsset{}).Error; err != nil {
			t.Errorf("delete test assets: %v", err)
		}
		if err := dbs[0].Delete(&User{}, owner).Error; err != nil {
			t.Errorf("delete test user: %v", err)
		}
	})
	create := func() UploadSession {
		res := request(t, h1, http.MethodPost, "/api/uploads/sessions", token, map[string]any{"filename": "clip.mp4", "size": len(sampleMP4)})
		var session UploadSession
		if res.Code != http.StatusCreated || json.Unmarshal(res.Body.Bytes(), &session) != nil {
			t.Fatalf("create: %d %s", res.Code, res.Body.String())
		}
		sessionIDs = append(sessionIDs, session.ID)
		if res := putTestChunk(h2, token, session.ID, 0, sampleMP4); res.Code != http.StatusOK {
			t.Fatalf("chunk through other instance: %d %s", res.Code, res.Body.String())
		}
		return session
	}
	session := create()
	done := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- request(t, h1, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("completion did not exit")
		}
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("completion did not reach the MinIO chunk read")
	}
	var claimed UploadSession
	if err := dbs[1].First(&claimed, "id = ?", session.ID).Error; err != nil || claimed.LeaseUntil == nil {
		t.Fatalf("initial lease: %+v err=%v", claimed, err)
	}
	cutoff := claimed.LeaseUntil.Add(time.Millisecond)
	if err := dbs[1].Model(&UploadSession{}).Where("id = ?", session.ID).Update("expires_at", cutoff.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var current UploadSession
		if err := dbs[1].First(&current, "id = ?", session.ID).Error; err != nil {
			t.Fatal(err)
		}
		if current.LeaseUntil != nil && current.LeaseUntil.After(cutoff) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cleaned, err := s2.cleanupExpiredUploadsAt(ctx, cutoff); cleaned != 0 || err != nil {
		t.Fatalf("other instance cleaned active MinIO chunks: count=%d err=%v", cleaned, err)
	}
	sum := sha256.Sum256(sampleMP4)
	hash := hex.EncodeToString(sum[:])
	if err := verifyObject(ctx, store, chunkObjectKey(session.ID, 0, hash), int64(len(sampleMP4)), hash); err != nil {
		t.Fatalf("active chunk disappeared: %v", err)
	}
	unblock()
	select {
	case res := <-done:
		if res.Code != http.StatusOK {
			t.Fatalf("completion: %d %s", res.Code, res.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("completion did not finish")
	}
	if err := dbs[1].First(&session, "id = ?", session.ID).Error; err != nil || session.Status != "completed" || session.LeaseUntil != nil || session.CleanedAt == nil {
		t.Fatalf("completed session state: %+v err=%v", session, err)
	}
	if err := verifyObject(ctx, store, "media/"+session.ID+".mp4", int64(len(sampleMP4)), hash); err != nil {
		t.Fatalf("final MinIO media differs: %v", err)
	}
	if res := request(t, h2, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil); res.Code != http.StatusOK {
		t.Fatalf("completion retry on other instance: %d %s", res.Code, res.Body.String())
	}
	if err := dbs[1].Model(&MediaAsset{}).Where("owner_id = ?", owner).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("asset registration: count=%d err=%v", count, err)
	}
	abandoned := create()
	past := time.Now().UTC().Add(-time.Minute)
	if err := dbs[1].Model(&UploadSession{}).Where("id = ?", abandoned.ID).Updates(map[string]any{"status": "completing", "expires_at": past, "lease_until": past}).Error; err != nil {
		t.Fatal(err)
	}
	if cleaned, err := s2.CleanupExpiredUploads(ctx); cleaned != 1 || err != nil {
		t.Fatalf("abandoned completion cleanup: count=%d err=%v", cleaned, err)
	}
	if _, err := store.stat(ctx, chunkObjectKey(abandoned.ID, 0, hash)); !objectMissing(err) {
		t.Fatalf("abandoned MinIO chunk remains: %v", err)
	}
	if err := dbs[1].Model(&UploadSession{}).Where("id = ?", abandoned.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("abandoned session remains: count=%d err=%v", count, err)
	}
	if err := dbs[1].Model(&UploadChunk{}).Where("session_id IN ?", sessionIDs).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("chunk registrations remain: count=%d err=%v", count, err)
	}
	if err := verifyObject(ctx, store, "media/"+session.ID+".mp4", int64(len(sampleMP4)), hash); err != nil {
		t.Fatalf("abandoned cleanup damaged committed media: %v", err)
	}
}
