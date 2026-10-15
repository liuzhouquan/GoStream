package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testServer(t *testing.T) (*Server, *gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(gormsqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&User{}, &Video{}, &Follow{}, &VideoAction{}, &Comment{}, &Notification{}, &ChatMessage{}, &OutboxEvent{}); err != nil {
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

func TestAccountVideoFeedInteractionFlow(t *testing.T) {
	_, h, db := testServer(t)
	authorID, authorToken := registerTestUser(t, h, "creator")
	viewerID, viewerToken := registerTestUser(t, h, "viewer")
	if r := request(t, h, http.MethodPost, "/api/videos", "", map[string]string{"title": "blocked", "media_url": "/media/blocked.mp4"}); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous publish status %d", r.Code)
	}
	created := request(t, h, http.MethodPost, "/api/videos", authorToken, map[string]string{"title": "A real clip", "description": "intro", "media_url": "/media/clip.mp4"})
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
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "../../unsafe.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(part, "media payload"); err != nil {
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
	if res.Code != http.StatusCreated {
		t.Fatalf("upload status %d: %s", res.Code, res.Body.String())
	}
	var out struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
	}
	if err = json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out.Filename) != out.Filename || out.Filename == "unsafe.mp4" {
		t.Fatalf("server did not generate a safe name: %q", out.Filename)
	}
	if _, err = os.Stat(filepath.Join(s.MediaDir, out.Filename)); err != nil {
		t.Fatalf("uploaded file was not written to media storage: %v", err)
	}
}

func TestWebSocketMessagePersistenceDeliveryAndAck(t *testing.T) {
	_, h, db := testServer(t)
	recipientID, recipientToken := registerTestUser(t, h, "recipient")
	_, senderToken := registerTestUser(t, h, "sender")
	ts := httptest.NewServer(h)
	defer ts.Close()
	connect := func(token string) *websocket.Conn {
		u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws?token=" + token
		conn, _, err := websocket.DefaultDialer.Dial(u, nil)
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
}
