package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

var (
	sampleWebM = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x86, 0x81, 0x01, 0x42, 0xf7, 0x81, 0x01, 0x42, 0xf2, 0x81, 0x04, 0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}
	sampleMOV  = []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'q', 't', ' ', ' ', 0, 0, 0, 0, 'q', 't', ' ', ' '}
)

func makeTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestMediaFormatValidationAndNegativeMatrix checks format detection, magic bytes,
// extension matching, chunked session assembly, and rejected format invariants.
func TestMediaFormatValidationAndNegativeMatrix(t *testing.T) {
	_, h, db := testServer(t)
	testMediaFormatValidation(t, h, db)
}

func testMediaFormatValidation(t *testing.T, h http.Handler, db *gorm.DB) {
	t.Helper()
	_, token := registerTestUser(t, h, "fmt_user")

	validPNG := makeTestPNG(t)

	// 1. Direct upload of valid formats
	for _, tc := range []struct {
		name     string
		filename string
		payload  []byte
		wantKind string
	}{
		{"valid mp4", "valid.mp4", sampleMP4, "video"},
		{"valid webm", "valid.webm", sampleWebM, "video"},
		{"valid mov", "valid.mov", sampleMOV, "video"},
		{"valid png image", "valid.png", validPNG, "image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := uploadTestMedia(t, h, token, tc.filename, tc.payload)
			if res.Code != http.StatusCreated {
				t.Fatalf("expected 201 for %s, got %d: %s", tc.filename, res.Code, res.Body.String())
			}
			var asset struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &asset); err != nil || !strings.HasPrefix(asset.URL, "/media/") {
				t.Fatalf("unexpected asset URL: %v (%s)", err, res.Body.String())
			}
			var row MediaAsset
			if err := db.Where("url = ?", asset.URL).First(&row).Error; err != nil || row.Kind != tc.wantKind {
				t.Fatalf("persisted asset kind mismatch: row=%+v err=%v", row, err)
			}
		})
	}

	// 2. Direct upload of invalid / mismatched payloads
	for _, tc := range []struct {
		name     string
		filename string
		payload  []byte
		wantCode int
	}{
		{"plain text disguised as mp4", "fake.mp4", []byte("plain text not video"), http.StatusBadRequest},
		{"short truncated mp4 header", "short.mp4", []byte{0, 0, 0, 8}, http.StatusBadRequest},
		{"corrupt webm header", "fake.webm", []byte{0x1a, 0x45, 0xdf, 0xa3, 0x00, 0x00, 0x00, 0x00}, http.StatusBadRequest},
		{"corrupt mov header", "fake.mov", []byte{0, 0, 0, 20, 'n', 'o', 'p', 'e', 'q', 't', ' ', ' '}, http.StatusBadRequest},
		{"png image disguised as mp4", "video.mp4", validPNG, http.StatusBadRequest},
		{"mp4 video disguised as jpg", "photo.jpg", sampleMP4, http.StatusBadRequest},
		{"webm video disguised as png", "photo.png", sampleWebM, http.StatusBadRequest},
		{"jpeg magic disguised as webm", "video.webm", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, http.StatusBadRequest},
		{"unsupported avi extension", "clip.avi", []byte("RIFF1234AVI LIST"), http.StatusBadRequest},
		{"unsupported mkv extension", "clip.mkv", []byte("matroska"), http.StatusBadRequest},
		{"unsupported executable", "evil.exe", []byte("MZ\x90\x00"), http.StatusBadRequest},
		{"unsupported shell script", "script.sh", []byte("#!/bin/bash\necho 1"), http.StatusBadRequest},
		{"zero length payload", "empty.mp4", []byte{}, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := uploadTestMedia(t, h, token, tc.filename, tc.payload)
			if res.Code != tc.wantCode && res.Code != http.StatusBadRequest {
				t.Fatalf("expected rejection %d for %s, got %d: %s", tc.wantCode, tc.filename, res.Code, res.Body.String())
			}
		})
	}

	// 3. Chunked session validation for different video formats
	t.Run("chunked session webm success", func(t *testing.T) {
		sum := sha256.Sum256(sampleWebM)
		hash := hex.EncodeToString(sum[:])
		created := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
			"filename": "stream.webm",
			"size":     len(sampleWebM),
			"sha256":   hash,
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create session: %d %s", created.Code, created.Body.String())
		}
		var s struct{ ID string }
		_ = json.Unmarshal(created.Body.Bytes(), &s)

		if r := putTestChunk(h, token, s.ID, 0, sampleWebM); r.Code != http.StatusOK {
			t.Fatalf("put webm chunk: %d %s", r.Code, r.Body.String())
		}
		complete := request(t, h, http.MethodPost, "/api/uploads/sessions/"+s.ID+"/complete", token, nil)
		if complete.Code != http.StatusOK {
			t.Fatalf("complete webm session: %d %s", complete.Code, complete.Body.String())
		}
	})

	t.Run("chunked session mov success", func(t *testing.T) {
		sum := sha256.Sum256(sampleMOV)
		hash := hex.EncodeToString(sum[:])
		created := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
			"filename": "camera.mov",
			"size":     len(sampleMOV),
			"sha256":   hash,
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create session: %d %s", created.Code, created.Body.String())
		}
		var s struct{ ID string }
		_ = json.Unmarshal(created.Body.Bytes(), &s)

		if r := putTestChunk(h, token, s.ID, 0, sampleMOV); r.Code != http.StatusOK {
			t.Fatalf("put mov chunk: %d %s", r.Code, r.Body.String())
		}
		complete := request(t, h, http.MethodPost, "/api/uploads/sessions/"+s.ID+"/complete", token, nil)
		if complete.Code != http.StatusOK {
			t.Fatalf("complete mov session: %d %s", complete.Code, complete.Body.String())
		}
	})

	t.Run("chunked session corrupt payload rejected on completion", func(t *testing.T) {
		corrupt := []byte("plain text chunk pretending to be mp4 file")
		sum := sha256.Sum256(corrupt)
		hash := hex.EncodeToString(sum[:])
		created := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
			"filename": "bad.mp4",
			"size":     len(corrupt),
			"sha256":   hash,
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create session: %d %s", created.Code, created.Body.String())
		}
		var s struct{ ID string }
		_ = json.Unmarshal(created.Body.Bytes(), &s)

		if r := putTestChunk(h, token, s.ID, 0, corrupt); r.Code != http.StatusOK {
			t.Fatalf("put corrupt chunk: %d", r.Code)
		}
		complete := request(t, h, http.MethodPost, "/api/uploads/sessions/"+s.ID+"/complete", token, nil)
		if complete.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for corrupt mp4 completion, got %d: %s", complete.Code, complete.Body.String())
		}
	})
}

// TestMediaPermissionAndOwnershipMatrix checks authentication, cross-user session isolation,
// asset ownership enforcement, and media kind validation during publication.
func TestMediaPermissionAndOwnershipMatrix(t *testing.T) {
	_, h, db := testServer(t)
	testMediaPermissionAndOwnershipMatrix(t, h, db)
}

func testMediaPermissionAndOwnershipMatrix(t *testing.T, h http.Handler, db *gorm.DB) {
	t.Helper()
	_, tokenA := registerTestUser(t, h, "user_alpha")
	_, tokenB := registerTestUser(t, h, "user_beta")

	// 1. Anonymous access restrictions
	t.Run("anonymous upload and session rejected", func(t *testing.T) {
		res := uploadTestMedia(t, h, "", "test.mp4", sampleMP4)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous upload expected 401, got %d", res.Code)
		}
		res = request(t, h, http.MethodPost, "/api/uploads/sessions", "", map[string]any{"filename": "test.mp4", "size": len(sampleMP4)})
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous session expected 401, got %d", res.Code)
		}
		res = request(t, h, http.MethodPost, "/api/videos", "", map[string]string{"title": "test", "media_url": "/media/fake.mp4"})
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous publish expected 401, got %d", res.Code)
		}
	})

	// 2. Cross-user session isolation
	t.Run("user B cannot inspect, upload to, or complete user A session", func(t *testing.T) {
		created := request(t, h, http.MethodPost, "/api/uploads/sessions", tokenA, map[string]any{
			"filename": "private.mp4",
			"size":     len(sampleMP4),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create session A: %d", created.Code)
		}
		var s struct{ ID string }
		_ = json.Unmarshal(created.Body.Bytes(), &s)

		// User B attempts GET session
		if r := request(t, h, http.MethodGet, "/api/uploads/sessions/"+s.ID, tokenB, nil); r.Code != http.StatusNotFound {
			t.Fatalf("user B read session A: expected 404, got %d", r.Code)
		}
		// User B attempts PUT chunk
		if r := putTestChunk(h, tokenB, s.ID, 0, sampleMP4); r.Code != http.StatusNotFound {
			t.Fatalf("user B put chunk to session A: expected 404, got %d", r.Code)
		}
		// User B attempts complete session
		if r := request(t, h, http.MethodPost, "/api/uploads/sessions/"+s.ID+"/complete", tokenB, nil); r.Code != http.StatusNotFound {
			t.Fatalf("user B complete session A: expected 404, got %d", r.Code)
		}
	})

	// 3. Asset ownership & kind validation during video publication
	t.Run("video publish asset ownership and kind rules", func(t *testing.T) {
		// User A uploads video and image assets
		resVideo := uploadTestMedia(t, h, tokenA, "videoA.mp4", sampleMP4)
		if resVideo.Code != http.StatusCreated {
			t.Fatalf("upload videoA: %d", resVideo.Code)
		}
		var assetVideo struct{ URL string }
		_ = json.Unmarshal(resVideo.Body.Bytes(), &assetVideo)

		validPNG := makeTestPNG(t)
		resImage := uploadTestMedia(t, h, tokenA, "coverA.png", validPNG)
		if resImage.Code != http.StatusCreated {
			t.Fatalf("upload coverA: %d", resImage.Code)
		}
		var assetImage struct{ URL string }
		_ = json.Unmarshal(resImage.Body.Bytes(), &assetImage)

		// User B tries to publish using User A's video asset -> 400
		r := request(t, h, http.MethodPost, "/api/videos", tokenB, map[string]string{
			"title":     "Stolen Video",
			"media_url": assetVideo.URL,
		})
		if r.Code != http.StatusBadRequest {
			t.Fatalf("user B publishing user A asset: expected 400, got %d: %s", r.Code, r.Body.String())
		}

		// Publishing with non-existent media URL -> 400
		r = request(t, h, http.MethodPost, "/api/videos", tokenA, map[string]string{
			"title":     "Ghost Video",
			"media_url": "/media/0123456789abcdef0123456789abcdef.mp4",
		})
		if r.Code != http.StatusBadRequest {
			t.Fatalf("publishing non-existent media URL: expected 400, got %d: %s", r.Code, r.Body.String())
		}

		// User A tries to publish video using an image asset as media_url -> 400
		r = request(t, h, http.MethodPost, "/api/videos", tokenA, map[string]string{
			"title":     "Image As Video",
			"media_url": assetImage.URL,
		})
		if r.Code != http.StatusBadRequest {
			t.Fatalf("publishing image as media_url: expected 400, got %d: %s", r.Code, r.Body.String())
		}

		// User A tries to publish video using a video asset as cover_url -> 400
		r = request(t, h, http.MethodPost, "/api/videos", tokenA, map[string]string{
			"title":     "Video As Cover",
			"media_url": assetVideo.URL,
			"cover_url": assetVideo.URL,
		})
		if r.Code != http.StatusBadRequest {
			t.Fatalf("publishing video as cover_url: expected 400, got %d: %s", r.Code, r.Body.String())
		}

		// User A publishes with valid media_url and cover_url -> 201 Created
		r = request(t, h, http.MethodPost, "/api/videos", tokenA, map[string]string{
			"title":     "Legitimate Video",
			"media_url": assetVideo.URL,
			"cover_url": assetImage.URL,
		})
		if r.Code != http.StatusCreated {
			t.Fatalf("valid video publish: expected 201, got %d: %s", r.Code, r.Body.String())
		}
	})
}

// TestMediaEndpointAccessAndPrivacySemantics validates path traversal protection,
// invalid name filtering, public accessibility of published video content, and
// documents the direct 128-bit unguessable nonce media URL semantics for unpublished uploads.
func TestMediaEndpointAccessAndPrivacySemantics(t *testing.T) {
	s, h, db := testServer(t)
	_, token := registerTestUser(t, h, "privacy_author")

	// 1. Path traversal and malformed name rejection
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"path traversal parent", "/media/../../etc/passwd"},
		{"encoded traversal", "/media/%2e%2e%2f%2e%2e%2fetc%2fpasswd"},
		{"short hex name", "/media/12345.mp4"},
		{"non-hex characters", "/media/zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.mp4"},
		{"unsupported extension", "/media/0123456789abcdef0123456789abcdef.exe"},
		{"nonexistent valid hex", "/media/00000000000000000000000000000000.mp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := request(t, h, http.MethodGet, tc.url, "", nil)
			if res.Code != http.StatusNotFound {
				t.Fatalf("expected 404 for %s, got %d", tc.url, res.Code)
			}
		})
	}

	// 2. Published video accessibility
	res := uploadTestMedia(t, h, token, "clip.mp4", sampleMP4)
	if res.Code != http.StatusCreated {
		t.Fatalf("upload: %d", res.Code)
	}
	var asset struct{ URL string }
	_ = json.Unmarshal(res.Body.Bytes(), &asset)

	pubRes := request(t, h, http.MethodPost, "/api/videos", token, map[string]string{
		"title":     "Public Clip",
		"media_url": asset.URL,
	})
	if pubRes.Code != http.StatusCreated {
		t.Fatalf("publish: %d", pubRes.Code)
	}

	// Guest can read the published video media directly
	mediaRes := request(t, h, http.MethodGet, asset.URL, "", nil)
	if mediaRes.Code != http.StatusOK {
		t.Fatalf("guest media fetch expected 200, got %d", mediaRes.Code)
	}
	if !bytes.Equal(mediaRes.Body.Bytes(), sampleMP4) {
		t.Fatal("fetched media payload differs from uploaded sample")
	}

	// 3. Unpublished source media access semantics (P2 audit verification):
	// When a video is in "processing" state (or failed), it must NOT appear in feeds or user profile,
	// but the underlying asset URL is content-addressed by a 128-bit unguessable nonce token.
	s.UseFFmpeg("ffmpeg-mock")
	resUnpublished := uploadTestMedia(t, h, token, "source.mp4", sampleMP4)
	var assetUnpub struct{ URL string }
	_ = json.Unmarshal(resUnpublished.Body.Bytes(), &assetUnpub)

	createProcessing := request(t, h, http.MethodPost, "/api/videos", token, map[string]string{
		"title":     "Processing Video",
		"media_url": assetUnpub.URL,
	})
	if createProcessing.Code != http.StatusCreated {
		t.Fatalf("create processing: %d", createProcessing.Code)
	}
	var unpubVideo Video
	_ = json.Unmarshal(createProcessing.Body.Bytes(), &unpubVideo)
	if unpubVideo.Status != "processing" {
		t.Fatalf("expected status processing, got %s", unpubVideo.Status)
	}

	// Assert absent from public timeline feed
	feed := request(t, h, http.MethodGet, "/api/feed?scene=timeline", "", nil)
	if feed.Code != http.StatusOK || jsonContainsVideo(feed.Body.Bytes(), unpubVideo.ID) {
		t.Fatal("processing video leaked into public timeline feed")
	}

	// Assert absent from author's public video profile for guests
	profileVideos := request(t, h, http.MethodGet, fmt.Sprintf("/api/users/%d/videos", unpubVideo.AuthorID), "", nil)
	if profileVideos.Code != http.StatusOK || jsonContainsVideo(profileVideos.Body.Bytes(), unpubVideo.ID) {
		t.Fatal("processing video leaked into guest profile view")
	}

	// Direct access with known 128-bit nonce URL succeeds (standard CDN/static media model)
	unpubMedia := request(t, h, http.MethodGet, assetUnpub.URL, "", nil)
	if unpubMedia.Code != http.StatusOK {
		t.Fatalf("direct media URL with 128-bit nonce expected 200, got %d", unpubMedia.Code)
	}
	_ = db
}

// TestMinIOMediaFormatAndPermissionMatrix repeats the format and permission matrix against MinIO
// when GOSTREAM_TEST_MINIO_ENDPOINT is configured.
func TestMinIOMediaFormatAndPermissionMatrix(t *testing.T) {
	endpoint := os.Getenv("GOSTREAM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GOSTREAM_TEST_MINIO_ENDPOINT to a disposable MinIO server")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "gostream-matrix-" + hex.EncodeToString(nonce[:])
	ctx := context.Background()
	store, err := NewMinIOMediaStore(ctx, endpoint, os.Getenv("GOSTREAM_TEST_MINIO_ACCESS_KEY"), os.Getenv("GOSTREAM_TEST_MINIO_SECRET_KEY"), bucket, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, prefix := range []string{"chunks/", "media/"} {
			_ = store.removePrefix(cleanCtx, prefix)
		}
		_ = store.client.RemoveBucket(cleanCtx, bucket)
	})

	s, _, db := testServer(t)
	s.UseObjectMedia(store)
	h := s.Router()

	testMediaFormatValidation(t, h, db)
	testMediaPermissionAndOwnershipMatrix(t, h, db)
}
