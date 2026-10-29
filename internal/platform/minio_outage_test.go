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
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestMinIOCompleteStorageOutage tests the end-to-end failure boundaries when
// the underlying object storage (MinIO) suffers a complete outage (HTTP 503 at the API storage proxy).
// It verifies:
// 1. Direct upload returns 503 and persists no MediaAsset.
// 2. Chunk upload returns 503, persists no UploadChunk, and leaves the session retriable.
// 3. Completion during outage returns 503, creates no asset, and keeps the session open.
// 4. Media retrieval during outage returns 503 without crashing.
// 5. Full recovery after storage restores connectivity.
func TestMinIOCompleteStorageOutage(t *testing.T) {
	endpoint := os.Getenv("GOSTREAM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GOSTREAM_TEST_MINIO_ENDPOINT to a disposable MinIO server")
	}

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gostream-outage-" + hex.EncodeToString(nonce[:])
	ctx := context.Background()
	access := os.Getenv("GOSTREAM_TEST_MINIO_ACCESS_KEY")
	secret := os.Getenv("GOSTREAM_TEST_MINIO_SECRET_KEY")

	// Direct store to create bucket and clean up
	directStore, err := NewMinIOMediaStore(ctx, endpoint, access, secret, bucket, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := directStore.removePrefix(cleanCtx, "media/"); err != nil {
			t.Errorf("clean media: %v", err)
		}
		if err := directStore.removePrefix(cleanCtx, "chunks/"); err != nil {
			t.Errorf("clean chunks: %v", err)
		}
		if err := directStore.client.RemoveBucket(cleanCtx, bucket); err != nil {
			t.Errorf("remove bucket: %v", err)
		}
	})

	// Proxy to inject complete outage
	upstream, err := url.Parse("http://" + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	var outage atomic.Bool

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outage.Load() {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		reverse.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	proxyEndpoint := proxy.Listener.Addr().String()
	store, err := openMinIOMediaStore(ctx, proxyEndpoint, access, secret, bucket, false, false)
	if err != nil {
		t.Fatal(err)
	}

	s, _, db := testServer(t)
	s.UseObjectMedia(store)
	h := s.Router()
	_, token := registerTestUser(t, h, "outage_tester")

	// 1. Direct upload during outage
	outage.Store(true)
	var assetsBefore int64
	if err := db.Model(&MediaAsset{}).Count(&assetsBefore).Error; err != nil {
		t.Fatal(err)
	}

	directRes := uploadTestMedia(t, h, token, "outage.mp4", sampleMP4)
	if directRes.Code != http.StatusServiceUnavailable {
		t.Fatalf("direct upload during outage expected 503, got %d: %s", directRes.Code, directRes.Body.String())
	}
	var assetsAfter int64
	if err := db.Model(&MediaAsset{}).Count(&assetsAfter).Error; err != nil || assetsAfter != assetsBefore {
		t.Fatalf("failed upload created asset row: before=%d after=%d", assetsBefore, assetsAfter)
	}

	// 2. Chunk upload during outage
	sum := sha256.Sum256(sampleMP4)
	hash := hex.EncodeToString(sum[:])
	createSessionRes := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
		"filename": "resumable.mp4",
		"size":     len(sampleMP4),
		"sha256":   hash,
	})
	if createSessionRes.Code != http.StatusCreated {
		t.Fatalf("create session expected 201, got %d", createSessionRes.Code)
	}
	var session struct{ ID string }
	_ = json.Unmarshal(createSessionRes.Body.Bytes(), &session)

	// Attempt chunk upload during outage
	chunkRes := putTestChunk(h, token, session.ID, 0, sampleMP4)
	if chunkRes.Code != http.StatusServiceUnavailable {
		t.Fatalf("chunk upload during outage expected 503, got %d: %s", chunkRes.Code, chunkRes.Body.String())
	}
	var chunkCount int64
	if err := db.Model(&UploadChunk{}).Where("session_id = ?", session.ID).Count(&chunkCount).Error; err != nil || chunkCount != 0 {
		t.Fatalf("failed chunk persisted in DB: %d", chunkCount)
	}

	// Session must remain open and retriable
	var sessRow UploadSession
	if err := db.First(&sessRow, "id = ?", session.ID).Error; err != nil || sessRow.Status != "open" {
		t.Fatalf("session state corrupted after failed chunk: status=%s", sessRow.Status)
	}

	// 3. Storage recovers connectivity
	outage.Store(false)
	chunkRetry := putTestChunk(h, token, session.ID, 0, sampleMP4)
	if chunkRetry.Code != http.StatusOK {
		t.Fatalf("chunk retry after recovery expected 200, got %d: %s", chunkRetry.Code, chunkRetry.Body.String())
	}

	// 4. Outage during completion assembly
	outage.Store(true)
	compRes := request(t, h, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	if compRes.Code != http.StatusServiceUnavailable {
		t.Fatalf("complete during outage expected 503, got %d: %s", compRes.Code, compRes.Body.String())
	}
	if err := db.Model(&MediaAsset{}).Count(&assetsAfter).Error; err != nil || assetsAfter != assetsBefore {
		t.Fatalf("failed completion created asset row: %d", assetsAfter)
	}
	if err := db.First(&sessRow, "id = ?", session.ID).Error; err != nil || sessRow.Status != "open" {
		t.Fatalf("failed completion marked session non-open: %s", sessRow.Status)
	}

	// 5. Completion succeeds after recovery
	outage.Store(false)
	compRetry := request(t, h, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	if compRetry.Code != http.StatusOK {
		t.Fatalf("complete retry after recovery expected 200, got %d: %s", compRetry.Code, compRetry.Body.String())
	}
	var completedAsset struct{ URL string }
	_ = json.Unmarshal(compRetry.Body.Bytes(), &completedAsset)

	// 6. Media serving outage
	outage.Store(true)
	mediaFetch := request(t, h, http.MethodGet, completedAsset.URL, "", nil)
	if mediaFetch.Code != http.StatusServiceUnavailable {
		t.Fatalf("media fetch during outage expected 503, got %d", mediaFetch.Code)
	}

	// 7. Media serving recovery
	outage.Store(false)
	mediaFetchRec := request(t, h, http.MethodGet, completedAsset.URL, "", nil)
	if mediaFetchRec.Code != http.StatusOK {
		t.Fatalf("media fetch after recovery expected 200, got %d", mediaFetchRec.Code)
	}
	if !bytes.Equal(mediaFetchRec.Body.Bytes(), sampleMP4) {
		t.Fatal("fetched payload does not match original bytes")
	}
}
