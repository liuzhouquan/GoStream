package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt in with GOSTREAM_TEST_MINIO_ENDPOINT and credentials. This test uses a
// randomly named bucket on a real S3 server and deletes it on completion.
func TestMinIOMediaAcrossInstances(t *testing.T) {
	endpoint := os.Getenv("GOSTREAM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GOSTREAM_TEST_MINIO_ENDPOINT to a disposable MinIO server")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gostream-test-" + hex.EncodeToString(nonce[:])
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
				t.Errorf("clean test prefix %s: %v", prefix, err)
			}
		}
		if err := store.client.RemoveBucket(cleanCtx, bucket); err != nil {
			t.Errorf("remove test bucket: %v", err)
		}
	})
	s1, _, db := testServer(t)
	s1.UseObjectMedia(store)
	h1 := s1.Router()
	s2 := NewServer(db, string(s1.Secret), t.TempDir())
	s2.UseObjectMedia(store)
	h2 := s2.Router()
	_, token := registerTestUser(t, h1, "minio_author")

	direct := uploadTestMedia(t, h1, token, "direct.mp4", sampleMP4)
	if direct.Code != http.StatusCreated {
		t.Fatalf("direct object upload: %d %s", direct.Code, direct.Body.String())
	}
	var directAsset struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(direct.Body.Bytes(), &directAsset); err != nil {
		t.Fatal(err)
	}
	got := request(t, h2, http.MethodGet, directAsset.URL, "", nil)
	if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), sampleMP4) {
		t.Fatalf("cross-instance object read: %d %q", got.Code, got.Body.String())
	}

	payload := make([]byte, int(uploadChunkSize)+len(sampleMP4))
	copy(payload, sampleMP4)
	for i := len(sampleMP4); i < len(payload); i++ {
		payload[i] = byte(i)
	}
	sum := sha256.Sum256(payload)
	created := request(t, h1, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
		"filename": "resumed.mp4", "size": len(payload), "sha256": hex.EncodeToString(sum[:]),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create object upload session: %d %s", created.Code, created.Body.String())
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if res := putTestChunk(h1, token, session.ID, 0, payload[:uploadChunkSize]); res.Code != http.StatusOK {
		t.Fatalf("first instance chunk: %d %s", res.Code, res.Body.String())
	}
	if res := putTestChunk(h2, token, session.ID, 1, payload[uploadChunkSize:]); res.Code != http.StatusOK {
		t.Fatalf("second instance chunk: %d %s", res.Code, res.Body.String())
	}
	wrong := append([]byte(nil), payload[:uploadChunkSize]...)
	wrong[100] ^= 1
	if res := putTestChunk(h2, token, session.ID, 0, wrong); res.Code != http.StatusConflict {
		t.Fatalf("conflicting cross-instance chunk: %d %s", res.Code, res.Body.String())
	}
	completed := request(t, h2, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	if completed.Code != http.StatusOK {
		t.Fatalf("complete on second instance: %d %s", completed.Code, completed.Body.String())
	}
	var assembled struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(completed.Body.Bytes(), &assembled); err != nil {
		t.Fatal(err)
	}
	if assembled.URL != "/media/"+session.ID+".mp4" {
		t.Fatalf("unexpected completed URL %q", assembled.URL)
	}
	repeated := request(t, h1, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	if repeated.Code != http.StatusOK {
		t.Fatalf("repeat completion: %d %s", repeated.Code, repeated.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, assembled.URL, nil)
	get.Header.Set("Range", "bytes=5-9")
	ranged := httptest.NewRecorder()
	h1.ServeHTTP(ranged, get)
	if ranged.Code != http.StatusPartialContent || !bytes.Equal(ranged.Body.Bytes(), payload[5:10]) {
		t.Fatalf("range read: %d %q", ranged.Code, ranged.Body.String())
	}
	chunkHash := sha256.Sum256(payload[:uploadChunkSize])
	if _, err := store.stat(ctx, chunkObjectKey(session.ID, 0, hex.EncodeToString(chunkHash[:]))); !objectMissing(err) {
		t.Fatalf("completed chunks were not removed: %v", err)
	}
	video := request(t, h1, http.MethodPost, "/api/videos", token, map[string]string{"title": "MinIO clip", "media_url": assembled.URL})
	if video.Code != http.StatusCreated {
		t.Fatalf("publish object media: %d %s", video.Code, video.Body.String())
	}

	orphanName, err := randomMediaName(".mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.putBytes(ctx, "media/"+orphanName, sampleMP4); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(25 * time.Hour)
	removed, err := s2.ReconcileOrphanMedia(ctx, future)
	if err != nil || removed != 1 {
		t.Fatalf("orphan object cleanup: removed=%d err=%v", removed, err)
	}
	if _, err := store.stat(ctx, "media/"+orphanName); !objectMissing(err) {
		t.Fatalf("orphan object still exists: %v", err)
	}
	if _, err := store.stat(ctx, "media/"+strings.TrimPrefix(assembled.URL, "/media/")); err != nil {
		t.Fatalf("published object removed: %v", err)
	}
	if err := db.Model(&MediaAsset{}).Where("url = ?", directAsset.URL).Update("created_at", time.Now().UTC().Add(-8*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	removed, err = s2.ReconcileUnusedAssets(ctx, time.Now().UTC().Add(8*24*time.Hour+time.Minute))
	if err != nil || removed != 1 {
		t.Fatalf("unreferenced object asset cleanup: removed=%d err=%v", removed, err)
	}
	if _, err := store.stat(ctx, "media/"+filepath.Base(directAsset.URL)); !objectMissing(err) {
		t.Fatalf("unreferenced direct upload still exists: %v", err)
	}
	if got := request(t, h2, http.MethodGet, assembled.URL, "", nil); got.Code != http.StatusOK || len(got.Body.Bytes()) != len(payload) {
		t.Fatalf("published object after cleanup: status=%d size=%d", got.Code, got.Body.Len())
	}
	expiring := request(t, h1, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
		"filename": "expired.mp4", "size": len(sampleMP4),
	})
	if expiring.Code != http.StatusCreated {
		t.Fatalf("create expiring session: %d %s", expiring.Code, expiring.Body.String())
	}
	var expiredSession struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(expiring.Body.Bytes(), &expiredSession); err != nil {
		t.Fatal(err)
	}
	if res := putTestChunk(h2, token, expiredSession.ID, 0, sampleMP4); res.Code != http.StatusOK {
		t.Fatalf("upload expiring chunk: %d %s", res.Code, res.Body.String())
	}
	if err := db.Model(&UploadSession{}).Where("id = ?", expiredSession.ID).Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	cleaned, err := s2.CleanupExpiredUploads(ctx)
	if err != nil || cleaned != 1 {
		t.Fatalf("expired object chunk cleanup: cleaned=%d err=%v", cleaned, err)
	}
	expiredHash := sha256.Sum256(sampleMP4)
	if _, err := store.stat(ctx, chunkObjectKey(expiredSession.ID, 0, hex.EncodeToString(expiredHash[:]))); !objectMissing(err) {
		t.Fatalf("expired chunk object remains: %v", err)
	}
	var remaining int64
	if err := db.Model(&UploadSession{}).Where("id = ?", expiredSession.ID).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("expired session remains: count=%d err=%v", remaining, err)
	}
}
