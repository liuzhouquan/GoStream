package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// This opt-in test uses a real disposable MinIO bucket. It checks that rejected
// requests do not create SQL registrations and that a retry repairs a chunk
// whose object bytes changed without changing its size.
func TestMinIOMediaValidationAndRecovery(t *testing.T) {
	endpoint := os.Getenv("GOSTREAM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GOSTREAM_TEST_MINIO_ENDPOINT to a disposable MinIO server")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gostream-fault-" + hex.EncodeToString(nonce[:])
	ctx := context.Background()
	store, err := NewMinIOMediaStore(ctx, endpoint, os.Getenv("GOSTREAM_TEST_MINIO_ACCESS_KEY"), os.Getenv("GOSTREAM_TEST_MINIO_SECRET_KEY"), bucket, false)
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
	var s1 *Server
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
		if !db.Migrator().HasTable(&UploadSession{}) {
			t.Fatal("MySQL schema is not migrated; start an API against the disposable database first")
		}
		s1 = NewServer(db, "test-signing-secret-at-least-32-bytes", t.TempDir())
	} else {
		s1, _, db = testServer(t)
	}
	s1.UseObjectMedia(store)
	h1 := s1.Router()
	s2 := NewServer(db, string(s1.Secret), t.TempDir())
	s2.UseObjectMedia(store)
	h2 := s2.Router()
	ownerID, ownerToken := registerTestUser(t, h1, "faultown_"+hex.EncodeToString(nonce[:]))
	_, otherToken := registerTestUser(t, h1, "faultother_"+hex.EncodeToString(nonce[:]))
	path := "/api/uploads/sessions"
	valid := map[string]any{"filename": "clip.mp4", "size": len(sampleMP4)}
	if r := request(t, h1, http.MethodPost, path, "", valid); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous session: %d", r.Code)
	}
	for _, input := range []map[string]any{
		{"filename": "clip.mp4", "size": 0},
		{"filename": "clip.mp4", "size": maxMediaSize + 1},
		{"filename": "clip.exe", "size": len(sampleMP4)},
		{"filename": "clip.mp4", "size": len(sampleMP4), "sha256": strings.Repeat("z", 64)},
	} {
		if r := request(t, h1, http.MethodPost, path, ownerToken, input); r.Code != http.StatusBadRequest {
			t.Fatalf("invalid session accepted: %d %s", r.Code, r.Body.String())
		}
	}
	var sessionCount int64
	if err := db.Model(&UploadSession{}).Where("owner_id = ?", ownerID).Count(&sessionCount).Error; err != nil || sessionCount != 0 {
		t.Fatalf("rejected sessions persisted: %d %v", sessionCount, err)
	}
	createSession := func(hash string) string {
		t.Helper()
		body := map[string]any{"filename": "clip.mp4", "size": len(sampleMP4), "sha256": hash}
		r := request(t, h1, http.MethodPost, path, ownerToken, body)
		if r.Code != http.StatusCreated {
			t.Fatalf("create session: %d %s", r.Code, r.Body.String())
		}
		var result struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || len(result.ID) != 32 {
			t.Fatalf("decode session: %v %s", err, r.Body.String())
		}
		return result.ID
	}
	sum := sha256.Sum256(sampleMP4)
	hash := hex.EncodeToString(sum[:])
	id := createSession(hash)
	base := path + "/" + id
	for _, methodPath := range []struct{ method, url string }{
		{http.MethodGet, base},
		{http.MethodPost, base + "/complete"},
	} {
		if r := request(t, h2, methodPath.method, methodPath.url, otherToken, nil); r.Code != http.StatusNotFound {
			t.Fatalf("foreign session %s: %d", methodPath.url, r.Code)
		}
	}
	if r := putTestChunk(h2, otherToken, id, 0, sampleMP4); r.Code != http.StatusNotFound {
		t.Fatalf("foreign chunk: %d", r.Code)
	}
	if r := putTestChunk(h1, ownerToken, id, 0, sampleMP4[:len(sampleMP4)-1]); r.Code != http.StatusBadRequest {
		t.Fatalf("short chunk: %d", r.Code)
	}
	if r := putTestChunk(h1, ownerToken, id, 0, append(bytes.Clone(sampleMP4), 0)); r.Code != http.StatusBadRequest {
		t.Fatalf("long chunk: %d", r.Code)
	}
	if r := putTestChunk(h1, ownerToken, id, 1, sampleMP4); r.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range chunk: %d", r.Code)
	}
	if r := request(t, h2, http.MethodPost, base+"/complete", ownerToken, nil); r.Code != http.StatusConflict {
		t.Fatalf("incomplete upload completed: %d", r.Code)
	}
	var chunkCount int64
	if err := db.Model(&UploadChunk{}).Where("session_id = ?", id).Count(&chunkCount).Error; err != nil || chunkCount != 0 {
		t.Fatalf("rejected chunks persisted: %d %v", chunkCount, err)
	}
	key := chunkObjectKey(id, 0, hash)
	if _, err := store.stat(ctx, key); !objectMissing(err) {
		t.Fatalf("rejected chunk object exists: %v", err)
	}
	if r := putTestChunk(h1, ownerToken, id, 0, sampleMP4); r.Code != http.StatusOK {
		t.Fatalf("valid chunk: %d %s", r.Code, r.Body.String())
	}
	wrong := bytes.Clone(sampleMP4)
	wrong[len(wrong)-1] ^= 1
	if r := putTestChunk(h2, ownerToken, id, 0, wrong); r.Code != http.StatusConflict {
		t.Fatalf("conflicting chunk retry: %d", r.Code)
	}
	if err := store.putBytes(ctx, key, wrong); err != nil {
		t.Fatal(err)
	}
	if err := verifyObject(ctx, store, key, int64(len(sampleMP4)), hash); err == nil {
		t.Fatal("same-size object corruption was not detected")
	}
	if r := putTestChunk(h2, ownerToken, id, 0, sampleMP4); r.Code != http.StatusOK {
		t.Fatalf("retry after same-size corruption: %d %s", r.Code, r.Body.String())
	}
	if err := verifyObject(ctx, store, key, int64(len(sampleMP4)), hash); err != nil {
		t.Fatalf("retry did not repair object: %v", err)
	}
	if err := store.remove(ctx, key); err != nil {
		t.Fatal(err)
	}
	if r := putTestChunk(h1, ownerToken, id, 0, sampleMP4); r.Code != http.StatusOK {
		t.Fatalf("retry after missing object: %d %s", r.Code, r.Body.String())
	}
	if err := verifyObject(ctx, store, key, int64(len(sampleMP4)), hash); err != nil {
		t.Fatalf("retry did not replace missing object: %v", err)
	}
	if err := db.Model(&UploadChunk{}).Where("session_id = ?", id).Count(&chunkCount).Error; err != nil || chunkCount != 1 {
		t.Fatalf("chunk registration was duplicated: %d %v", chunkCount, err)
	}
	completed := request(t, h2, http.MethodPost, base+"/complete", ownerToken, nil)
	if completed.Code != http.StatusOK {
		t.Fatalf("complete repaired upload: %d %s", completed.Code, completed.Body.String())
	}
	var asset struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(completed.Body.Bytes(), &asset); err != nil || asset.URL != "/media/"+id+".mp4" {
		t.Fatalf("completed asset: %v %s", err, completed.Body.String())
	}
	if r := request(t, h1, http.MethodPost, base+"/complete", ownerToken, nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(asset.URL)) {
		t.Fatalf("completion retry: %d %s", r.Code, r.Body.String())
	}
	if r := putTestChunk(h1, ownerToken, id, 0, sampleMP4); r.Code != http.StatusConflict {
		t.Fatalf("completed session accepted a chunk: %d", r.Code)
	}
	var assetCount int64
	if err := db.Model(&MediaAsset{}).Where("url = ?", asset.URL).Count(&assetCount).Error; err != nil || assetCount != 1 {
		t.Fatalf("completion duplicated asset: %d %v", assetCount, err)
	}
	if r := request(t, h2, http.MethodPost, "/api/videos", otherToken, map[string]string{"title": "stolen", "media_url": asset.URL}); r.Code != http.StatusBadRequest {
		t.Fatalf("foreign asset published: %d", r.Code)
	}
	if r := request(t, h1, http.MethodPost, "/api/videos", ownerToken, map[string]string{"title": "owned", "media_url": asset.URL}); r.Code != http.StatusCreated {
		t.Fatalf("owner could not publish: %d %s", r.Code, r.Body.String())
	}
	if r := uploadTestMedia(t, h1, ownerToken, "fake.mp4", []byte("not a video")); r.Code != http.StatusBadRequest {
		t.Fatalf("spoofed direct media accepted: %d", r.Code)
	}
	badID := createSession(strings.Repeat("0", 64))
	if r := putTestChunk(h1, ownerToken, badID, 0, sampleMP4); r.Code != http.StatusOK {
		t.Fatalf("bad-hash fixture chunk: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h2, http.MethodPost, path+"/"+badID+"/complete", ownerToken, nil); r.Code != http.StatusConflict {
		t.Fatalf("bad full-file hash completed: %d", r.Code)
	}
	if _, err := store.stat(ctx, "media/"+badID+".mp4"); !objectMissing(err) {
		t.Fatalf("bad-hash media object exists: %v", err)
	}
	if err := db.Model(&MediaAsset{}).Where("url = ?", "/media/"+badID+".mp4").Count(&assetCount).Error; err != nil || assetCount != 0 {
		t.Fatalf("bad-hash asset persisted: %d %v", assetCount, err)
	}
	var failedSession UploadSession
	if err := db.First(&failedSession, "id = ?", badID).Error; err != nil || failedSession.Status != "open" || failedSession.LeaseUntil != nil {
		t.Fatalf("failed completion did not release the lease: status=%q lease=%v error=%v", failedSession.Status, failedSession.LeaseUntil, err)
	}
}
