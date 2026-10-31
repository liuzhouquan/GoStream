package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		var cleanupErrs []error
		for _, prefix := range []string{"media/", "chunks/"} {
			if err := directStore.removePrefix(cleanCtx, prefix); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("clean %s: %w", prefix, err))
			}
		}
		if err := directStore.client.RemoveBucket(cleanCtx, bucket); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove bucket: %w", err))
		}
		for _, err := range cleanupErrs {
			t.Errorf("minio outage cleanup failure: %v", err)
		}
	})

	// Proxy to inject complete outage
	upstream, err := url.Parse("http://" + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	var outageMode atomic.Int32 // 0: healthy, 1: HTTP 503 outage, 2: hard TCP drop (hijack & close)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch outageMode.Load() {
		case 1:
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		case 2:
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			http.Error(w, "connection reset by peer", http.StatusBadGateway)
			return
		default:
			reverse.ServeHTTP(w, r)
		}
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

	// 1. Direct upload during outage (HTTP 503)
	outageMode.Store(1)
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

	// 2. Chunk upload during outage (HTTP 503)
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
	outageMode.Store(0)
	chunkRetry := putTestChunk(h, token, session.ID, 0, sampleMP4)
	if chunkRetry.Code != http.StatusOK {
		t.Fatalf("chunk retry after recovery expected 200, got %d: %s", chunkRetry.Code, chunkRetry.Body.String())
	}

	// 4. Outage during completion assembly (HTTP 503)
	outageMode.Store(1)
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
	outageMode.Store(0)
	compRetry := request(t, h, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	if compRetry.Code != http.StatusOK {
		t.Fatalf("complete retry after recovery expected 200, got %d: %s", compRetry.Code, compRetry.Body.String())
	}
	var completedAsset struct{ URL string }
	_ = json.Unmarshal(compRetry.Body.Bytes(), &completedAsset)

	// 6. Media serving outage (HTTP 503)
	outageMode.Store(1)
	mediaFetch := request(t, h, http.MethodGet, completedAsset.URL, "", nil)
	if mediaFetch.Code != http.StatusServiceUnavailable {
		t.Fatalf("media fetch during outage expected 503, got %d", mediaFetch.Code)
	}

	// 7. Media serving recovery
	outageMode.Store(0)
	mediaFetchRec := request(t, h, http.MethodGet, completedAsset.URL, "", nil)
	if mediaFetchRec.Code != http.StatusOK {
		t.Fatalf("media fetch after recovery expected 200, got %d", mediaFetchRec.Code)
	}
	if !bytes.Equal(mediaFetchRec.Body.Bytes(), sampleMP4) {
		t.Fatal("fetched payload does not match original bytes")
	}

	// 8. Abrupt TCP socket drop (hijack & close connection)
	outageMode.Store(2)
	tcpDropRes := uploadTestMedia(t, h, token, "tcpdrop.mp4", sampleMP4)
	if tcpDropRes.Code != http.StatusServiceUnavailable && tcpDropRes.Code != http.StatusBadGateway {
		t.Fatalf("direct upload during TCP disconnect expected 503/502, got %d: %s", tcpDropRes.Code, tcpDropRes.Body.String())
	}
	// Assert zero leaked asset rows
	var assetsAfterTCPDrop int64
	if err := db.Model(&MediaAsset{}).Count(&assetsAfterTCPDrop).Error; err != nil || assetsAfterTCPDrop != assetsBefore+1 {
		t.Fatalf("TCP drop created corrupt asset: before=%d now=%d", assetsBefore+1, assetsAfterTCPDrop)
	}

	// Session chunk upload during TCP drop
	createSession2 := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
		"filename": "tcpdrop_resumable.mp4",
		"size":     len(sampleMP4),
		"sha256":   hash,
	})
	if createSession2.Code != http.StatusCreated {
		t.Fatalf("create session 2 expected 201, got %d", createSession2.Code)
	}
	var session2 struct{ ID string }
	_ = json.Unmarshal(createSession2.Body.Bytes(), &session2)

	chunkTCPDrop := putTestChunk(h, token, session2.ID, 0, sampleMP4)
	if chunkTCPDrop.Code != http.StatusServiceUnavailable && chunkTCPDrop.Code != http.StatusBadGateway {
		t.Fatalf("chunk upload during TCP disconnect expected 503/502, got %d: %s", chunkTCPDrop.Code, chunkTCPDrop.Body.String())
	}
	var sessRow2 UploadSession
	if err := db.First(&sessRow2, "id = ?", session2.ID).Error; err != nil || sessRow2.Status != "open" {
		t.Fatalf("session 2 state corrupted after TCP drop: status=%s", sessRow2.Status)
	}

	// 9. Recovery after TCP reconnect
	outageMode.Store(0)
	chunkRetry2 := putTestChunk(h, token, session2.ID, 0, sampleMP4)
	if chunkRetry2.Code != http.StatusOK {
		t.Fatalf("chunk retry after TCP recovery expected 200, got %d: %s", chunkRetry2.Code, chunkRetry2.Body.String())
	}
	compRetry2 := request(t, h, http.MethodPost, "/api/uploads/sessions/"+session2.ID+"/complete", token, nil)
	if compRetry2.Code != http.StatusOK {
		t.Fatalf("complete retry after TCP recovery expected 200, got %d: %s", compRetry2.Code, compRetry2.Body.String())
	}
}
