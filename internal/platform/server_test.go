package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testServer(t *testing.T) (*Server, *gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	db, err := gorm.Open(gormsqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = MigrateSchema(db); err != nil {
		t.Fatal(err)
	}
	s := NewServer(db, "test-signing-secret-at-least-32-bytes", t.TempDir())
	return s, s.Router(), db
}

func request(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&raw).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &raw)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func registerTestUser(t *testing.T, h http.Handler, username string) (uint, string) {
	t.Helper()
	r := request(t, h, http.MethodPost, "/api/users", "", map[string]string{"username": username, "password": "correct-horse-123", "nickname": username})
	if r.Code != http.StatusCreated {
		t.Fatalf("register status %d: %s", r.Code, r.Body.String())
	}
	var out struct {
		User  User   `json:"user"`
		Token string `json:"access_token"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.User.ID, out.Token
}

func uploadTestMedia(t *testing.T, h http.Handler, token, filename string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func putTestChunk(h http.Handler, token, sessionID string, index int, payload []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/uploads/sessions/%s/chunks/%d", sessionID, index), bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

var sampleMP4 = []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '2'}

func TestAccountVideoFeedInteractionFlow(t *testing.T) {
	_, h, db := testServer(t)
	authorID, authorToken := registerTestUser(t, h, "creator")
	viewerID, viewerToken := registerTestUser(t, h, "viewer")
	upload := uploadTestMedia(t, h, authorToken, "clip.mp4", sampleMP4)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status %d: %s", upload.Code, upload.Body.String())
	}
	var uploaded struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(upload.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if r := request(t, h, http.MethodPost, "/api/videos", "", map[string]string{"title": "blocked", "media_url": "/media/blocked.mp4"}); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous publish status %d", r.Code)
	}
	created := request(t, h, http.MethodPost, "/api/videos", authorToken, map[string]string{"title": "A real clip", "description": "intro", "media_url": uploaded.URL})
	if created.Code != http.StatusCreated {
		t.Fatalf("publish status %d: %s", created.Code, created.Body.String())
	}
	var video Video
	if err := json.Unmarshal(created.Body.Bytes(), &video); err != nil {
		t.Fatal(err)
	}
	feed := request(t, h, http.MethodGet, "/api/feed?scene=timeline&limit=1", "", nil)
	if feed.Code != http.StatusOK {
		t.Fatalf("feed status %d: %s", feed.Code, feed.Body.String())
	}
	var page struct {
		Items []Video `json:"items"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != video.ID {
		t.Fatalf("unexpected feed page: %+v", page)
	}
	for i := 0; i < 2; i++ {
		r := request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", video.ID), viewerToken, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("like status %d: %s", r.Code, r.Body.String())
		}
	}
	var afterLike Video
	if err := db.First(&afterLike, video.ID).Error; err != nil {
		t.Fatal(err)
	}
	if afterLike.LikeCount != 1 {
		t.Fatalf("idempotent like count = %d, want 1", afterLike.LikeCount)
	}
	stateResponse := request(t, h, http.MethodGet, "/api/feed?scene=timeline", viewerToken, nil)
	var statePage struct {
		Items []struct {
			Liked     bool `json:"liked"`
			Favorited bool `json:"favorited"`
		} `json:"items"`
	}
	if err := json.Unmarshal(stateResponse.Body.Bytes(), &statePage); err != nil || len(statePage.Items) != 1 || !statePage.Items[0].Liked || statePage.Items[0].Favorited {
		t.Fatalf("viewer interaction state missing: %s (%v)", stateResponse.Body.String(), err)
	}
	comment := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", video.ID), viewerToken, map[string]string{"body": "great clip"})
	if comment.Code != http.StatusCreated {
		t.Fatalf("comment status %d: %s", comment.Code, comment.Body.String())
	}
	follow := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorID), viewerToken, nil)
	if follow.Code != http.StatusOK {
		t.Fatalf("follow status %d: %s", follow.Code, follow.Body.String())
	}
	followingFeed := request(t, h, http.MethodGet, "/api/feed?scene=following", viewerToken, nil)
	if followingFeed.Code != http.StatusOK {
		t.Fatalf("following feed status %d: %s", followingFeed.Code, followingFeed.Body.String())
	}
	var followingPage struct {
		Items []Video `json:"items"`
	}
	if err := json.Unmarshal(followingFeed.Body.Bytes(), &followingPage); err != nil {
		t.Fatal(err)
	}
	if len(followingPage.Items) != 1 || followingPage.Items[0].AuthorID != authorID {
		t.Fatalf("unexpected following feed: %+v (viewer=%d)", followingPage, viewerID)
	}
	notices := request(t, h, http.MethodGet, "/api/notifications", authorToken, nil)
	if notices.Code != http.StatusOK || !bytes.Contains(notices.Body.Bytes(), []byte(`"kind":"follow"`)) {
		t.Fatalf("expected follow notification: %d %s", notices.Code, notices.Body.String())
	}
}

func TestFeedCursorRoundTripAndRejectsMalformed(t *testing.T) {
	want := feedCursor{Created: 123456, ID: 99, Score: 42}
	got, err := decodeCursor(encodeCursor(want))
	if err != nil || got != want {
		t.Fatalf("cursor round trip = %+v, %v", got, err)
	}
	if _, err = decodeCursor("not-a-cursor!"); err == nil {
		t.Fatal("expected malformed cursor error")
	}
}

func TestTimelineCursorUsesIDForEqualTimestamps(t *testing.T) {
	_, h, db := testServer(t)
	userID, _ := registerTestUser(t, h, "cursor")
	stamp := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	for _, title := range []string{"first", "second", "third"} {
		if err := db.Create(&Video{AuthorID: userID, Title: title, MediaURL: "/media/clip.mp4", Status: "published", CreatedAt: stamp}).Error; err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uint]bool{}
	cursor := ""
	for i := 0; i < 3; i++ {
		path := "/api/feed?scene=timeline&limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		res := request(t, h, http.MethodGet, path, "", nil)
		var page struct {
			Items      []Video `json:"items"`
			NextCursor string  `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
			t.Fatalf("page %d: status=%d body=%s err=%v", i, res.Code, res.Body.String(), err)
		}
		if seen[page.Items[0].ID] {
			t.Fatalf("duplicate video %d across pages", page.Items[0].ID)
		}
		seen[page.Items[0].ID] = true
		cursor = page.NextCursor
		if page.HasMore != (i < 2) {
			t.Fatalf("page %d has_more=%v", i, page.HasMore)
		}
	}
}

func TestUploadWritesGeneratedMediaFile(t *testing.T) {
	s, h, _ := testServer(t)
	_, token := registerTestUser(t, h, "uploader")
	res := uploadTestMedia(t, h, token, "../../unsafe.mp4", sampleMP4)
	if res.Code != http.StatusCreated {
		t.Fatalf("upload status %d: %s", res.Code, res.Body.String())
	}
	var out struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out.Filename) != out.Filename || out.Filename == "unsafe.mp4" {
		t.Fatalf("server did not generate a safe name: %q", out.Filename)
	}
	if _, err := os.Stat(filepath.Join(s.MediaDir, out.Filename)); err != nil {
		t.Fatalf("uploaded file was not written to media storage: %v", err)
	}
}

func TestVideoPublicationRequiresOwnedUploadedVideo(t *testing.T) {
	_, h, _ := testServer(t)
	_, ownerToken := registerTestUser(t, h, "assetowner")
	_, otherToken := registerTestUser(t, h, "assetother")
	for _, payload := range [][]byte{[]byte("plain text"), []byte("<script>alert(1)</script>")} {
		if res := uploadTestMedia(t, h, ownerToken, "fake.mp4", payload); res.Code != http.StatusBadRequest {
			t.Fatalf("spoofed upload status %d: %s", res.Code, res.Body.String())
		}
	}
	upload := uploadTestMedia(t, h, ownerToken, "clip.mp4", sampleMP4)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload: %s", upload.Body.String())
	}
	var asset struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(upload.Body.Bytes(), &asset); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ token, url string }{
		{ownerToken, "https://example.com/video.mp4"},
		{ownerToken, "/media/other.mp4"},
		{otherToken, asset.URL},
	} {
		res := request(t, h, http.MethodPost, "/api/videos", tc.token, map[string]string{"title": "unowned", "media_url": tc.url})
		if res.Code != http.StatusBadRequest {
			t.Fatalf("unowned URL %q: %d %s", tc.url, res.Code, res.Body.String())
		}
	}
}

func TestVideoEditAndAvatarRequireOwnedMedia(t *testing.T) {
	_, h, _ := testServer(t)
	_, authorToken := registerTestUser(t, h, "editorauthor")
	_, otherToken := registerTestUser(t, h, "editorother")
	videoUpload := uploadTestMedia(t, h, authorToken, "clip.mp4", sampleMP4)
	if videoUpload.Code != http.StatusCreated {
		t.Fatalf("video upload: %d %s", videoUpload.Code, videoUpload.Body.String())
	}
	var media struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(videoUpload.Body.Bytes(), &media); err != nil {
		t.Fatal(err)
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	imageUpload := uploadTestMedia(t, h, authorToken, "cover.png", pngBytes.Bytes())
	if imageUpload.Code != http.StatusCreated {
		t.Fatalf("image upload: %d %s", imageUpload.Code, imageUpload.Body.String())
	}
	var cover struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(imageUpload.Body.Bytes(), &cover); err != nil {
		t.Fatal(err)
	}
	created := request(t, h, http.MethodPost, "/api/videos", authorToken, map[string]string{"title": "original", "media_url": media.URL})
	if created.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", created.Code, created.Body.String())
	}
	var video Video
	if err := json.Unmarshal(created.Body.Bytes(), &video); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/videos/%d", video.ID)
	if r := request(t, h, http.MethodPatch, path, otherToken, map[string]string{"title": "taken"}); r.Code != http.StatusForbidden {
		t.Fatalf("foreign edit: %d", r.Code)
	}
	if r := request(t, h, http.MethodPatch, path, authorToken, map[string]string{"cover_url": media.URL}); r.Code != http.StatusBadRequest {
		t.Fatalf("video as cover: %d", r.Code)
	}
	if r := request(t, h, http.MethodPatch, path, authorToken, map[string]string{"title": "updated", "cover_url": cover.URL}); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"title":"updated"`)) {
		t.Fatalf("valid edit: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodPatch, "/api/users/me", otherToken, map[string]string{"avatar_url": cover.URL}); r.Code != http.StatusBadRequest {
		t.Fatalf("foreign avatar: %d", r.Code)
	}
	if r := request(t, h, http.MethodPatch, "/api/users/me", authorToken, map[string]string{"avatar_url": cover.URL}); r.Code != http.StatusOK {
		t.Fatalf("owned avatar: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodDelete, path, authorToken, nil); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", r.Code)
	}
	if r := request(t, h, http.MethodPatch, path, authorToken, map[string]string{"title": "revived"}); r.Code != http.StatusNotFound {
		t.Fatalf("edit deleted video: %d", r.Code)
	}
	if r := request(t, h, http.MethodGet, path+"/comments", "", nil); r.Code != http.StatusNotFound {
		t.Fatalf("comments for deleted video: %d", r.Code)
	}
}

func TestResumableUploadOutOfOrderAndOwnership(t *testing.T) {
	s, h, db := testServer(t)
	_, token := registerTestUser(t, h, "resumer")
	_, otherToken := registerTestUser(t, h, "resumerother")
	payload := make([]byte, int(uploadChunkSize)+len(sampleMP4))
	copy(payload, sampleMP4)
	for i := len(sampleMP4); i < len(payload); i++ {
		payload[i] = byte(i % 251)
	}
	sum := sha256.Sum256(payload)
	create := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{
		"filename": "clip.mp4", "size": len(payload), "sha256": hex.EncodeToString(sum[:]),
	})
	if create.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", create.Code, create.Body.String())
	}
	var session struct {
		ID        string `json:"id"`
		ChunkSize int64  `json:"chunk_size"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.ChunkSize != uploadChunkSize {
		t.Fatalf("chunk size %d", session.ChunkSize)
	}
	if r := putTestChunk(h, otherToken, session.ID, 1, payload[uploadChunkSize:]); r.Code != http.StatusNotFound {
		t.Fatalf("foreign upload: %d", r.Code)
	}
	if r := putTestChunk(h, token, session.ID, 1, payload[uploadChunkSize:]); r.Code != http.StatusOK {
		t.Fatalf("second chunk first: %d %s", r.Code, r.Body.String())
	}
	completePath := "/api/uploads/sessions/" + session.ID + "/complete"
	if r := request(t, h, http.MethodPost, completePath, token, nil); r.Code != http.StatusConflict {
		t.Fatalf("incomplete session: %d %s", r.Code, r.Body.String())
	}
	if r := putTestChunk(h, token, session.ID, 0, payload[:uploadChunkSize]); r.Code != http.StatusOK {
		t.Fatalf("first chunk: %d %s", r.Code, r.Body.String())
	}
	if r := putTestChunk(h, token, session.ID, 0, payload[:uploadChunkSize]); r.Code != http.StatusOK {
		t.Fatalf("idempotent retry: %d %s", r.Code, r.Body.String())
	}
	badChunk := append([]byte(nil), payload[:uploadChunkSize]...)
	badChunk[100] ^= 0xff
	if r := putTestChunk(h, token, session.ID, 0, badChunk); r.Code != http.StatusConflict {
		t.Fatalf("conflicting retry: %d %s", r.Code, r.Body.String())
	}
	status := request(t, h, http.MethodGet, "/api/uploads/sessions/"+session.ID, token, nil)
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"uploaded_chunks":[0,1]`)) {
		t.Fatalf("resume status: %d %s", status.Code, status.Body.String())
	}
	completed := request(t, h, http.MethodPost, completePath, token, nil)
	if completed.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", completed.Code, completed.Body.String())
	}
	var asset struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(completed.Body.Bytes(), &asset); err != nil {
		t.Fatal(err)
	}
	if asset.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("assembled hash %s", asset.SHA256)
	}
	data, err := os.ReadFile(filepath.Join(s.MediaDir, filepath.Base(asset.URL)))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("assembled file differs: %v", err)
	}
	if r := request(t, h, http.MethodPost, completePath, token, nil); r.Code != http.StatusOK {
		t.Fatalf("repeat complete: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodPost, "/api/videos", token, map[string]string{"title": "resumed clip", "media_url": asset.URL}); r.Code != http.StatusCreated {
		t.Fatalf("publish resumed file: %d %s", r.Code, r.Body.String())
	}
	var count int64
	if err := db.Model(&MediaAsset{}).Where("url = ?", asset.URL).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("asset count %d err=%v", count, err)
	}
}

func TestExpiredUploadCleanupRemovesPartialFiles(t *testing.T) {
	s, h, db := testServer(t)
	_, token := registerTestUser(t, h, "expiredresumer")
	create := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{"filename": "clip.mp4", "size": len(sampleMP4)})
	if create.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", create.Code, create.Body.String())
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if r := putTestChunk(h, token, session.ID, 0, sampleMP4); r.Code != http.StatusOK {
		t.Fatalf("chunk: %d %s", r.Code, r.Body.String())
	}
	if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Update("expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if r := putTestChunk(h, token, session.ID, 0, sampleMP4); r.Code != http.StatusConflict {
		t.Fatalf("expired session accepted chunk: %d", r.Code)
	}
	cleaned, err := s.CleanupExpiredUploads(context.Background())
	if err != nil || cleaned != 1 {
		t.Fatalf("cleaned=%d err=%v", cleaned, err)
	}
	if _, err := os.Stat(s.uploadDir(session.ID)); !os.IsNotExist(err) {
		t.Fatalf("partial upload dir remains: %v", err)
	}
	if r := request(t, h, http.MethodGet, "/api/uploads/sessions/"+session.ID, token, nil); r.Code != http.StatusNotFound {
		t.Fatalf("expired session still visible: %d", r.Code)
	}
}

func TestExpiredUploadCleanupWaitsForCompletionLease(t *testing.T) {
	s, h, db := testServer(t)
	_, token := registerTestUser(t, h, "leasedresumer")
	created := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{"filename": "clip.mp4", "size": len(sampleMP4)})
	if created.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", created.Code, created.Body.String())
	}
	var session UploadSession
	if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if res := putTestChunk(h, token, session.ID, 0, sampleMP4); res.Code != http.StatusOK {
		t.Fatalf("upload chunk: %d %s", res.Code, res.Body.String())
	}
	now := time.Now().UTC()
	if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Updates(map[string]any{
		"status": "completing", "expires_at": now.Add(-time.Minute), "lease_until": now.Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if cleaned, err := s.CleanupExpiredUploads(context.Background()); err != nil || cleaned != 0 {
		t.Fatalf("active completion lease was cleaned: count=%d err=%v", cleaned, err)
	}
	if _, err := os.Stat(filepath.Join(s.uploadDir(session.ID), "000.part")); err != nil {
		t.Fatalf("leased chunk removed: %v", err)
	}
	if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Update("lease_until", now.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if cleaned, err := s.CleanupExpiredUploads(context.Background()); err != nil || cleaned != 1 {
		t.Fatalf("expired completion lease not cleaned: count=%d err=%v", cleaned, err)
	}
}

func TestInteractionTransitionsKeepCountAndNotificationConsistent(t *testing.T) {
	_, h, db := testServer(t)
	authorID, _ := registerTestUser(t, h, "countauthor")
	_, viewerToken := registerTestUser(t, h, "countviewer")
	v := Video{AuthorID: authorID, Title: "clip", MediaURL: "/media/clip.mp4", Status: "published"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/videos/%d/like", v.ID)
	for _, method := range []string{http.MethodPut, http.MethodPut, http.MethodDelete, http.MethodDelete, http.MethodPut} {
		res := request(t, h, method, path, viewerToken, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("%s interaction: %d %s", method, res.Code, res.Body.String())
		}
	}
	if err := db.First(&v, v.ID).Error; err != nil {
		t.Fatal(err)
	}
	if v.LikeCount != 1 {
		t.Fatalf("like count = %d, want 1", v.LikeCount)
	}
	var actions, notices int64
	if err := db.Model(&VideoAction{}).Where("video_id = ? AND kind = ?", v.ID, "like").Count(&actions).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Notification{}).Where("user_id = ? AND kind = ?", authorID, "like").Count(&notices).Error; err != nil {
		t.Fatal(err)
	}
	if actions != 1 || notices != 1 {
		t.Fatalf("actions=%d notices=%d, want one each", actions, notices)
	}
}

func TestLongCommentKeepsFullBodyAndBoundsNotification(t *testing.T) {
	_, h, db := testServer(t)
	authorID, _ := registerTestUser(t, h, "longauthor")
	_, commenterToken := registerTestUser(t, h, "longcommenter")
	v := Video{AuthorID: authorID, Title: "clip", MediaURL: "/media/clip.mp4", Status: "published"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("好", 300)
	res := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", v.ID), commenterToken, map[string]string{"body": body})
	if res.Code != http.StatusCreated {
		t.Fatalf("long comment: %d %s", res.Code, res.Body.String())
	}
	var comment Comment
	if err := db.Where("video_id = ?", v.ID).First(&comment).Error; err != nil {
		t.Fatal(err)
	}
	var notice Notification
	if err := db.Where("user_id = ? AND kind = ?", authorID, "comment").First(&notice).Error; err != nil {
		t.Fatal(err)
	}
	if comment.Body != body || len([]rune(notice.Text)) != 280 {
		t.Fatalf("comment length=%d notification length=%d", len([]rune(comment.Body)), len([]rune(notice.Text)))
	}
}

func TestAuthorizationRequiresBearerScheme(t *testing.T) {
	_, h, _ := testServer(t)
	_, token := registerTestUser(t, h, "beareruser")
	req := httptest.NewRequest(http.MethodGet, "/api/users/me", nil)
	req.Header.Set("Authorization", token)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("raw token accepted: %d", res.Code)
	}
}

func TestAuthenticationRejectsKnownAndExpiredSigningKeys(t *testing.T) {
	_, h, _ := testServer(t)
	userID, validToken := registerTestUser(t, h, "signingkeyuser")
	if res := request(t, h, http.MethodGet, "/api/users/me", validToken, nil); res.Code != http.StatusOK {
		t.Fatalf("valid token status=%d body=%s", res.Code, res.Body.String())
	}
	sign := func(secret string, expires time.Time) string {
		t.Helper()
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": userID, "exp": expires.Unix(),
		}).SignedString([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	for name, token := range map[string]string{
		"binary development key":  sign("local-development-secret-change-me-please", time.Now().Add(time.Hour)),
		"compose development key": sign("local-development-secret-change-before-deploy", time.Now().Add(time.Hour)),
		"expired real key":        sign("test-signing-secret-at-least-32-bytes", time.Now().Add(-time.Minute)),
	} {
		t.Run(name, func(t *testing.T) {
			if res := request(t, h, http.MethodGet, "/api/users/me", token, nil); res.Code != http.StatusUnauthorized {
				t.Fatalf("invalid token status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestWebSocketMessagePersistenceDeliveryAndAck(t *testing.T) {
	_, h, db := testServer(t)
	recipientID, recipientToken := registerTestUser(t, h, "recipient")
	_, senderToken := registerTestUser(t, h, "sender")
	ts := httptest.NewServer(h)
	defer ts.Close()
	connect := func(token string) *websocket.Conn {
		u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
		dialer := websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}
		conn, _, err := dialer.Dial(u, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var ready map[string]any
		if err = conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
			t.Fatalf("websocket ready response: %#v, %v", ready, err)
		}
		return conn
	}
	recipient := connect(recipientToken)
	sender := connect(senderToken)
	if err := sender.WriteJSON(socketEvent{Type: "message", ClientID: "client-msg-1", RecipientID: recipientID, Body: "hello over websocket"}); err != nil {
		t.Fatal(err)
	}
	var delivered struct {
		Type    string      `json:"type"`
		Message ChatMessage `json:"message"`
	}
	if err := recipient.ReadJSON(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.Type != "message" || delivered.Message.Body != "hello over websocket" {
		t.Fatalf("unexpected delivery: %+v", delivered)
	}
	var ack struct {
		Type      string `json:"type"`
		ClientID  string `json:"client_id"`
		MessageID uint   `json:"message_id"`
	}
	if err := sender.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	if ack.Type != "ack" || ack.ClientID != "client-msg-1" || ack.MessageID != delivered.Message.ID {
		t.Fatalf("unexpected sender ack: %+v", ack)
	}
	var count int64
	if err := db.Model(&ChatMessage{}).Where("client_id = ? AND sender_id = ?", "client-msg-1", delivered.Message.SenderID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted message count = %d, want 1", count)
	}
	acked := request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", delivered.Message.ID), recipientToken, nil)
	if acked.Code != http.StatusOK {
		t.Fatalf("message ACK status %d: %s", acked.Code, acked.Body.String())
	}
	if second := request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", delivered.Message.ID), recipientToken, nil); second.Code != http.StatusOK {
		t.Fatalf("repeated message ACK status %d: %s", second.Code, second.Body.String())
	}
}

func TestWebSocketOfflineReplayAndClientIDConflict(t *testing.T) {
	_, h, db := testServer(t)
	recipientID, recipientToken := registerTestUser(t, h, "offlinepeer")
	_, senderToken := registerTestUser(t, h, "offlinesender")
	ts := httptest.NewServer(h)
	defer ts.Close()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	if _, response, err := websocket.DefaultDialer.Dial(u+"?token="+senderToken, nil); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token must not authenticate: response=%v err=%v", response, err)
	}
	connect := func(token string) *websocket.Conn {
		dialer := websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}
		conn, _, err := dialer.Dial(u, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var ready map[string]any
		if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
			t.Fatalf("ready: %v %v", ready, err)
		}
		return conn
	}
	sender := connect(senderToken)
	event := socketEvent{Type: "message", ClientID: "offline-1", RecipientID: recipientID, Body: "saved while offline"}
	var firstID uint
	for i := 0; i < 2; i++ {
		if err := sender.WriteJSON(event); err != nil {
			t.Fatal(err)
		}
		var ack struct {
			Type      string `json:"type"`
			MessageID uint   `json:"message_id"`
		}
		if err := sender.ReadJSON(&ack); err != nil {
			t.Fatal(err)
		}
		if ack.Type != "ack" || ack.MessageID == 0 {
			t.Fatalf("sender ACK: %+v", ack)
		}
		if i == 0 {
			firstID = ack.MessageID
		} else if ack.MessageID != firstID {
			t.Fatalf("duplicate got new ID %d", ack.MessageID)
		}
	}
	conflict := event
	conflict.Body = "different content"
	if err := sender.WriteJSON(conflict); err != nil {
		t.Fatal(err)
	}
	var rejection map[string]any
	if err := sender.ReadJSON(&rejection); err != nil || rejection["type"] != "error" {
		t.Fatalf("client ID conflict: %v %v", rejection, err)
	}
	var messages, notices int64
	if err := db.Model(&ChatMessage{}).Count(&messages).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Notification{}).Where("kind = ?", "message").Count(&notices).Error; err != nil {
		t.Fatal(err)
	}
	if messages != 1 || notices != 1 {
		t.Fatalf("messages=%d notices=%d", messages, notices)
	}
	recipient := connect(recipientToken)
	var replay struct {
		Type    string      `json:"type"`
		Message ChatMessage `json:"message"`
	}
	if err := recipient.ReadJSON(&replay); err != nil {
		t.Fatal(err)
	}
	if replay.Type != "message" || replay.Message.ID != firstID {
		t.Fatalf("offline replay: %+v", replay)
	}
	if r := request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", firstID), recipientToken, nil); r.Code != http.StatusOK {
		t.Fatalf("ack: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodGet, "/api/messages/unread", recipientToken, nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"items":[]`)) {
		t.Fatalf("unread after ACK: %d %s", r.Code, r.Body.String())
	}
}

func TestGroupChatMembershipDeliveryUnreadAndAck(t *testing.T) {
	_, h, db := testServer(t)
	ownerID, ownerToken := registerTestUser(t, h, "groupowner")
	memberID, memberToken := registerTestUser(t, h, "groupmember")
	_, outsiderToken := registerTestUser(t, h, "groupoutsider")
	created := request(t, h, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "Study group"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", created.Code, created.Body.String())
	}
	var group ChatGroup
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/groups/%d", group.ID)
	if r := request(t, h, http.MethodGet, base+"/messages", outsiderToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("outsider history: %d", r.Code)
	}
	if r := request(t, h, http.MethodPost, base+"/members", ownerToken, map[string]any{"user_id": memberID}); r.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodPost, base+"/members", memberToken, map[string]any{"user_id": ownerID}); r.Code != http.StatusForbidden {
		t.Fatalf("member tried to invite: %d", r.Code)
	}
	if r := request(t, h, http.MethodGet, "/api/groups", memberToken, nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte("Study group")) {
		t.Fatalf("member group list: %d %s", r.Code, r.Body.String())
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	connect := func(token string) *websocket.Conn {
		u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
		dialer := websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}
		conn, _, err := dialer.Dial(u, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var ready map[string]any
		if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
			t.Fatalf("socket ready: %v %v", ready, err)
		}
		return conn
	}
	owner := connect(ownerToken)
	member := connect(memberToken)
	event := socketEvent{Type: "group_message", GroupID: group.ID, ClientID: "group-msg-1", Body: "hello group"}
	if err := owner.WriteJSON(event); err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Type      string `json:"type"`
		MessageID uint   `json:"message_id"`
	}
	if err := owner.ReadJSON(&ack); err != nil || ack.Type != "ack" || ack.MessageID == 0 {
		t.Fatalf("group sender ack: %+v %v", ack, err)
	}
	var delivery struct {
		Type    string       `json:"type"`
		Message GroupMessage `json:"message"`
	}
	if err := member.ReadJSON(&delivery); err != nil || delivery.Type != "group_message" || delivery.Message.ID != ack.MessageID {
		t.Fatalf("group delivery: %+v %v", delivery, err)
	}
	if r := request(t, h, http.MethodGet, base+"/unread", memberToken, nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"unread_count":1`)) {
		t.Fatalf("unread count: %d %s", r.Code, r.Body.String())
	}
	ackPath := fmt.Sprintf("%s/messages/%d/ack", base, ack.MessageID)
	if r := request(t, h, http.MethodPost, ackPath, memberToken, nil); r.Code != http.StatusOK {
		t.Fatalf("group ack: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodPost, ackPath, memberToken, nil); r.Code != http.StatusOK {
		t.Fatalf("repeated group ack: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodGet, base+"/unread", memberToken, nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"unread_count":0`)) {
		t.Fatalf("unread after ack: %d %s", r.Code, r.Body.String())
	}
	if err := owner.WriteJSON(event); err != nil {
		t.Fatal(err)
	}
	var duplicateAck struct {
		Type      string `json:"type"`
		MessageID uint   `json:"message_id"`
	}
	if err := owner.ReadJSON(&duplicateAck); err != nil || duplicateAck.MessageID != ack.MessageID {
		t.Fatalf("duplicate group ACK: %+v %v", duplicateAck, err)
	}
	var count int64
	if err := db.Model(&GroupMessage{}).Where("group_id = ?", group.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("group message count=%d err=%v", count, err)
	}
	later := GroupMessage{GroupID: group.ID, SenderID: ownerID, ClientID: "group-msg-2", Body: "later"}
	if err := db.Create(&later).Error; err != nil {
		t.Fatal(err)
	}
	if r := request(t, h, http.MethodPost, fmt.Sprintf("%s/messages/%d/ack", base, later.ID), memberToken, nil); r.Code != http.StatusOK {
		t.Fatalf("later group ack: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodPost, ackPath, memberToken, nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(fmt.Sprintf(`"last_ack_id":%d`, later.ID))) {
		t.Fatalf("older group ack moved cursor: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, memberID), ownerToken, nil); r.Code != http.StatusOK {
		t.Fatalf("remove member: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodGet, base+"/messages", memberToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("removed member read history: %d", r.Code)
	}
}

func TestGroupMessagesReplayOnReconnect(t *testing.T) {
	_, h, db := testServer(t)
	ownerID, ownerToken := registerTestUser(t, h, "replayowner")
	memberID, memberToken := registerTestUser(t, h, "replaymember")
	created := request(t, h, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "replay group"})
	var group ChatGroup
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &group) != nil {
		t.Fatalf("create group: %d %s", created.Code, created.Body.String())
	}
	base := fmt.Sprintf("/api/groups/%d", group.ID)
	if r := request(t, h, http.MethodPost, base+"/members", ownerToken, map[string]uint{"user_id": memberID}); r.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", r.Code, r.Body.String())
	}
	for i := 0; i < 2; i++ {
		if err := db.Create(&GroupMessage{GroupID: group.ID, SenderID: ownerID, ClientID: fmt.Sprintf("offline-%d", i), Body: fmt.Sprintf("message %d", i)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	conn, _, err := (&websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + memberToken}}).Dial(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var event struct {
		Type    string       `json:"type"`
		Message GroupMessage `json:"message"`
	}
	if err := conn.ReadJSON(&event); err != nil || event.Type != "ready" {
		t.Fatalf("socket ready: %+v %v", event, err)
	}
	var lastID uint
	for i := 0; i < 2; i++ {
		if err := conn.ReadJSON(&event); err != nil || event.Type != "group_message" || event.Message.GroupID != group.ID || event.Message.ID <= lastID {
			t.Fatalf("group replay %d: %+v %v", i, event, err)
		}
		lastID = event.Message.ID
	}
	if r := request(t, h, http.MethodPost, fmt.Sprintf("%s/messages/%d/ack", base, lastID), memberToken, nil); r.Code != http.StatusOK {
		t.Fatalf("group replay ACK: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, http.MethodGet, base+"/unread", memberToken, nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"unread_count":0`)) {
		t.Fatalf("unread after replay ACK: %d %s", r.Code, r.Body.String())
	}
}

func TestPresenceTracksConnectionAndLastSeen(t *testing.T) {
	s, h, db := testServer(t)
	userID, token := registerTestUser(t, h, "presenceuser")
	ts := httptest.NewServer(h)
	defer ts.Close()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	dialer := websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}
	conn, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ready map[string]any
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/users/%d/presence", userID)
	if r := request(t, h, http.MethodGet, path, "", nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"online":true`)) {
		t.Fatalf("online presence: %d %s", r.Code, r.Body.String())
	}
	// Hold a SQLite write lock while the socket closes. The first last-seen
	// update must fail and a later bounded retry must persist it.
	locked := db.Begin()
	if locked.Error != nil {
		t.Fatal(locked.Error)
	}
	defer locked.Rollback()
	if err := locked.Model(&User{}).Where("id = ?", userID).Update("bio", "held lock").Error; err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	removeDeadline := time.Now().Add(2 * time.Second)
	for s.Hub.online(userID) && time.Now().Before(removeDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Hub.online(userID) {
		t.Fatal("closed socket remained in the local hub")
	}
	time.Sleep(50 * time.Millisecond)
	if err := locked.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var stored User
		if err := db.Select("last_seen_at").First(&stored, userID).Error; err == nil && stored.LastSeenAt != nil {
			r := request(t, h, http.MethodGet, path, "", nil)
			if r.Code == http.StatusOK && bytes.Contains(r.Body.Bytes(), []byte(`"online":false`)) && !bytes.Contains(r.Body.Bytes(), []byte(`"last_seen_at":null`)) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("presence did not transition offline with a last_seen_at timestamp")
}

func TestOutboxDispatchAndDeadLetter(t *testing.T) {
	s, h, db := testServer(t)
	targetID, _ := registerTestUser(t, h, "outboxtarget")
	_, actorToken := registerTestUser(t, h, "outboxactor")
	if r := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", targetID), actorToken, nil); r.Code != http.StatusOK {
		t.Fatalf("follow: %d %s", r.Code, r.Body.String())
	}
	processed, err := s.ProcessOutboxOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("outbox processed=%d err=%v", processed, err)
	}
	var noticeEvent OutboxEvent
	if err := db.Where("topic = ?", "notification.created").First(&noticeEvent).Error; err != nil {
		t.Fatal(err)
	}
	if noticeEvent.PublishedAt == nil || noticeEvent.Attempts != 1 {
		t.Fatalf("notification event not acknowledged: %+v", noticeEvent)
	}
	bad := OutboxEvent{EventKey: "bad-topic", Topic: "missing.topic", Payload: "{}", AvailableAt: time.Now().UTC().Add(-time.Second)}
	if err := db.Create(&bad).Error; err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= outboxMaxAttempts; i++ {
		if err := db.Model(&bad).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		processed, err = s.ProcessOutboxOnce(context.Background())
		if err != nil || processed != 1 {
			var stored OutboxEvent
			_ = db.First(&stored, bad.ID).Error
			t.Fatalf("retry %d processed=%d err=%v row=%+v now=%s", i, processed, err, stored, time.Now().UTC().Format(time.RFC3339Nano))
		}
	}
	if err := db.First(&bad, bad.ID).Error; err != nil {
		t.Fatal(err)
	}
	if bad.DeadAt == nil || bad.PublishedAt != nil || bad.Attempts != outboxMaxAttempts || bad.LastError == "" {
		t.Fatalf("dead letter state: %+v", bad)
	}
}

func TestReadinessAndPrometheusMetrics(t *testing.T) {
	_, h, _ := testServer(t)
	if r := request(t, h, http.MethodGet, "/health", "", nil); r.Code != http.StatusOK {
		t.Fatalf("health: %d", r.Code)
	}
	if r := request(t, h, http.MethodGet, "/ready", "", nil); r.Code != http.StatusOK {
		t.Fatalf("readiness: %d %s", r.Code, r.Body.String())
	}
	metrics := request(t, h, http.MethodGet, "/metrics", "", nil)
	if metrics.Code != http.StatusOK {
		t.Fatalf("metrics: %d %s", metrics.Code, metrics.Body.String())
	}
	for _, marker := range []string{
		`gostream_http_requests_total{method="GET",route="/health",status="200"} 1`,
		`gostream_http_request_duration_seconds_bucket`,
		`gostream_websocket_connections`,
	} {
		if !strings.Contains(metrics.Body.String(), marker) {
			t.Fatalf("metrics missing %q", marker)
		}
	}
}

func TestLivenessSurvivesClosedSQLPool(t *testing.T) {
	_, h, db := testServer(t)
	if r := request(t, h, http.MethodGet, "/health", "", nil); r.Code != http.StatusOK {
		t.Fatalf("health: %d", r.Code)
	}
	if r := request(t, h, http.MethodGet, "/ready", "", nil); r.Code != http.StatusOK {
		t.Fatalf("ready: %d", r.Code)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	// /health must remain 200 OK (liveness probe independent of DB)
	if r := request(t, h, http.MethodGet, "/health", "", nil); r.Code != http.StatusOK {
		t.Fatalf("health during db failure expected 200, got %d", r.Code)
	}

	// /ready must return 503 (readiness probe reflects DB availability)
	if r := request(t, h, http.MethodGet, "/ready", "", nil); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready during db failure expected 503, got %d", r.Code)
	}
}
