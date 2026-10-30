package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestJWTSecurityAndTampering validates that invalid, expired, forged, or malformed
// JWT tokens are consistently rejected with HTTP 401 across protected routes.
func TestJWTSecurityAndTampering(t *testing.T) {
	s, h, _ := testServer(t)

	userID, validToken := registerTestUser(t, h, "jwt_sec_user")

	// 1. Valid token succeeds
	res := request(t, h, http.MethodGet, "/api/users/me", validToken, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("valid token returned %d: %s", res.Code, res.Body.String())
	}

	// 2. Empty Authorization header
	res = request(t, h, http.MethodGet, "/api/users/me", "", nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("empty token returned %d", res.Code)
	}

	// 3. Malformed header formats
	for _, malformed := range []string{
		"Token " + validToken,
		"Bearer",
		"Bearer ",
		"Bearer not-a-jwt-string",
		"Basic dXNlcjpwYXNz",
		"Bearer a.b",
		"Bearer a.b.c.d",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/users/me", nil)
		req.Header.Set("Authorization", malformed)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("malformed header %q returned %d", malformed, rec.Code)
		}
	}

	// 4. Token signed with wrong secret (forged token)
	wrongSecret := []byte("completely-wrong-secret-at-least-32-bytes")
	forgedClaims := jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(24 * time.Hour).Unix(),
	}
	forgedToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, forgedClaims).SignedString(wrongSecret)
	if err != nil {
		t.Fatal(err)
	}
	res = request(t, h, http.MethodGet, "/api/users/me", forgedToken, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("forged token with wrong secret returned %d", res.Code)
	}

	// 5. Expired token
	expiredClaims := jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	}
	expiredToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, expiredClaims).SignedString(s.Secret)
	if err != nil {
		t.Fatal(err)
	}
	res = request(t, h, http.MethodGet, "/api/users/me", expiredToken, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expired token returned %d", res.Code)
	}

	// 6. Token with 'none' signing method (alg=none attack)
	unsafeToken := jwt.NewWithClaims(jwt.SigningMethodNone, forgedClaims)
	unsafeStr, _ := unsafeToken.SignedString(jwt.UnsafeAllowNoneSignatureType)
	res = request(t, h, http.MethodGet, "/api/users/me", unsafeStr, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("none-alg token returned %d", res.Code)
	}
}

// TestStaticMediaServingAndPathTraversal validates that static media serving
// cannot be used to read files outside the media directory or list directory contents.
func TestStaticMediaServingAndPathTraversal(t *testing.T) {
	s, h, _ := testServer(t)

	// Create a secret file outside mediaDir in parent temp dir
	parentDir := filepath.Dir(s.MediaDir)
	secretFile := filepath.Join(parentDir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("super-sensitive-secret"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create a valid media file in mediaDir
	validMediaName := "0123456789abcdef0123456789abcdef.mp4"
	validMediaPath := filepath.Join(s.MediaDir, validMediaName)
	if err := os.WriteFile(validMediaPath, []byte("fake-mp4-data"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Reading the valid media succeeds
	res := request(t, h, http.MethodGet, "/media/"+validMediaName, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("read valid media returned %d: %s", res.Code, res.Body.String())
	}

	// 2. Traversal attempts to escape MediaDir must NOT return the secret file
	traversalPaths := []string{
		"/media/../secret.txt",
		"/media/%2e%2e/secret.txt",
		"/media/%2e%2e%2fsecret.txt",
		"/media/..%2fsecret.txt",
		"/media/%252e%252e%252fsecret.txt",
		"/media/....//secret.txt",
		"/media/..",
		"/media/",
	}

	for _, p := range traversalPaths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "super-sensitive-secret") {
			t.Fatalf("path traversal leaked file contents for path %s!", p)
		}
	}
}

// TestUploadPathTraversalAndOwnership validates that client-provided filenames
// cannot escape the upload directory and that upload sessions are strictly owned.
func TestUploadPathTraversalAndOwnership(t *testing.T) {
	s, h, _ := testServer(t)

	_, user1Token := registerTestUser(t, h, "up_user_one")
	_, user2Token := registerTestUser(t, h, "up_user_two")

	// 1. Malicious filename path traversal in resumable upload session creation
	for _, evilName := range []string{
		"../../../../etc/passwd.mp4",
		"..\\..\\..\\windows\\system32\\cmd.mp4",
		"/var/log/evil.mp4",
		"foo/bar/clip.mp4",
	} {
		res := request(t, h, http.MethodPost, "/api/uploads/sessions", user1Token, map[string]any{
			"filename": evilName,
			"size":     1024 * 1024,
		})
		if res.Code != http.StatusCreated {
			t.Fatalf("creation with filename %q returned %d: %s", evilName, res.Code, res.Body.String())
		}
		var session struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &session); err != nil {
			t.Fatal(err)
		}
		// Ensure the session upload directory is within clean chunks folder
		dir := s.uploadDir(session.ID)
		expectedPrefix := filepath.Clean(s.MediaDir) + ".chunks"
		if !strings.HasPrefix(dir, expectedPrefix) {
			t.Fatalf("upload dir %q escaped expected prefix %q", dir, expectedPrefix)
		}
	}

	// 2. Multipart single upload path traversal
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "../../../../etc/test.mp4")
	if err != nil {
		t.Fatal(err)
	}
	// Write valid MP4 header
	mp4Header := []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	payload := append(mp4Header, bytes.Repeat([]byte{0x00}, 500)...)
	part.Write(payload)
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/uploads", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+user1Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("multipart upload with traversal filename returned %d: %s", rec.Code, rec.Body.String())
	}
	var uploadResp struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &uploadResp)
	if !strings.HasPrefix(uploadResp.URL, "/media/") || strings.Contains(uploadResp.URL, "..") {
		t.Fatalf("malicious url returned: %s", uploadResp.URL)
	}

	// 3. Cross-user upload session isolation
	res := request(t, h, http.MethodPost, "/api/uploads/sessions", user1Token, map[string]any{
		"filename": "private.mp4",
		"size":     1024 * 1024,
	})
	var sess struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &sess)

	// User 2 cannot inspect User 1's upload session
	res = request(t, h, http.MethodGet, "/api/uploads/sessions/"+sess.ID, user2Token, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("user 2 inspect user 1 session: expected 404, got %d", res.Code)
	}

	// User 2 cannot upload chunks to User 1's upload session
	reqChunk := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/uploads/sessions/%s/chunks/0", sess.ID), bytes.NewReader([]byte("chunk data")))
	reqChunk.Header.Set("Authorization", "Bearer "+user2Token)
	recChunk := httptest.NewRecorder()
	h.ServeHTTP(recChunk, reqChunk)
	if recChunk.Code != http.StatusNotFound {
		t.Fatalf("user 2 put chunk to user 1 session: expected 404, got %d", recChunk.Code)
	}

	// User 2 cannot complete User 1's upload session
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/uploads/sessions/%s/complete", sess.ID), user2Token, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("user 2 complete user 1 session: expected 404, got %d", res.Code)
	}
}

// TestVideoAndResourceOwnershipIsolation validates that users cannot modify,
// delete, or view unpublished resources belonging to another user.
func TestVideoAndResourceOwnershipIsolation(t *testing.T) {
	_, h, db := testServer(t)

	authorID, authorToken := registerTestUser(t, h, "author_iso")
	_, strangerToken := registerTestUser(t, h, "stranger_iso")

	// Create published video
	pubVideo := Video{
		AuthorID: authorID,
		Title:    "Author Published",
		MediaURL: "/media/pub1.mp4",
		Status:   "published",
	}
	if err := db.Create(&pubVideo).Error; err != nil {
		t.Fatal(err)
	}

	// Create unpublished video (processing)
	procVideo := Video{
		AuthorID: authorID,
		Title:    "Author Processing",
		MediaURL: "/media/proc1.mp4",
		Status:   "processing",
	}
	if err := db.Create(&procVideo).Error; err != nil {
		t.Fatal(err)
	}

	// 1. Stranger cannot edit Author's video
	editBody := map[string]any{"title": "Hacked Title"}
	res := request(t, h, http.MethodPatch, fmt.Sprintf("/api/videos/%d", pubVideo.ID), strangerToken, editBody)
	if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
		t.Fatalf("stranger edit video: expected 404/403, got %d", res.Code)
	}

	// Verify title did not change
	var persisted Video
	if err := db.First(&persisted, pubVideo.ID).Error; err != nil {
		t.Fatalf("owned row missing after rejected request: %v", err)
	}
	if persisted.Title != "Author Published" {
		t.Fatalf("video title was maliciously altered: %s", persisted.Title)
	}

	// 2. Stranger cannot delete Author's video
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d", pubVideo.ID), strangerToken, nil)
	if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
		t.Fatalf("stranger delete video: expected 404/403, got %d", res.Code)
	}
	if err := db.First(&persisted, pubVideo.ID).Error; err != nil {
		t.Fatalf("owned row missing after rejected request: %v", err)
	}
	if persisted.Status == "deleted" {
		t.Fatalf("video was maliciously deleted by stranger")
	}

	// 3. Unpublished video privacy:
	// Stranger cannot view unpublished video
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d", procVideo.ID), strangerToken, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("stranger get unpublished video: expected 404, got %d", res.Code)
	}

	// Stranger user profile view does NOT contain unpublished video
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/users/%d/videos", authorID), strangerToken, nil)
	var strangerView struct {
		Items []Video `json:"items"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &strangerView)
	for _, v := range strangerView.Items {
		if v.ID == procVideo.ID {
			t.Fatalf("unpublished video leaked in user videos to stranger")
		}
	}

	// Author CAN view own unpublished video in profile
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/users/%d/videos", authorID), authorToken, nil)
	var authorView struct {
		Items []Video `json:"items"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &authorView)
	found := false
	for _, v := range authorView.Items {
		if v.ID == procVideo.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("author cannot see own processing video in profile")
	}
}

// TestChatAndGroupSecurityBoundaries validates that chat and group resources
// enforce strict member and owner checks.
func TestChatAndGroupSecurityBoundaries(t *testing.T) {
	_, h, db := testServer(t)

	ownerID, ownerToken := registerTestUser(t, h, "grp_owner")
	memberID, memberToken := registerTestUser(t, h, "grp_member")
	otherID, _ := registerTestUser(t, h, "grp_other")
	_, strangerToken := registerTestUser(t, h, "grp_stranger")

	// Owner creates a group
	res := request(t, h, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "Private Group"})
	if res.Code != http.StatusCreated {
		t.Fatalf("create group returned %d: %s", res.Code, res.Body.String())
	}
	var group ChatGroup
	_ = json.Unmarshal(res.Body.Bytes(), &group)

	// Owner invites Member and Other
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", group.ID), ownerToken, map[string]any{"user_id": memberID})
	if res.Code != http.StatusOK {
		t.Fatalf("owner invite member returned %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", group.ID), ownerToken, map[string]any{"user_id": otherID})
	if res.Code != http.StatusOK {
		t.Fatalf("owner invite other returned %d: %s", res.Code, res.Body.String())
	}

	// 1. Stranger (non-member) cannot read group history
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/messages", group.ID), strangerToken, nil)
	if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
		t.Fatalf("stranger read group messages: expected 404/403, got %d", res.Code)
	}

	// Exercise the real WebSocket send protocol, with an authorized control.
	web := httptest.NewServer(h)
	t.Cleanup(web.Close)
	outsider := connectDeviceSocket(t, web.URL, strangerToken, "")
	if err := outsider.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: "stranger-attempt", Body: "spam"}); err != nil {
		t.Fatal(err)
	}
	var denied struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := outsider.ReadJSON(&denied); err != nil || denied.Type != "error" || denied.Message != "group not found" {
		t.Fatalf("outsider send: %+v %v", denied, err)
	}
	var count int64
	if err := db.Model(&GroupMessage{}).Where("client_id = ?", "stranger-attempt").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unauthorized message rows=%d err=%v", count, err)
	}
	member := connectDeviceSocket(t, web.URL, memberToken, "")
	if err := member.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: "member-control", Body: "allowed"}); err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Type string `json:"type"`
		ID   uint   `json:"message_id"`
	}
	if err := member.ReadJSON(&ack); err != nil || ack.Type != "ack" || ack.ID == 0 {
		t.Fatalf("member control: %+v %v", ack, err)
	}
	if err := db.Model(&GroupMessage{}).Where("id = ? AND sender_id = ?", ack.ID, memberID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("authorized message rows=%d err=%v", count, err)
	}

	// 3. Ordinary member cannot invite others (only owner can)
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", group.ID), memberToken, map[string]any{"user_id": 9999})
	if res.Code != http.StatusForbidden && res.Code != http.StatusNotFound {
		t.Fatalf("non-owner invite member: expected 403/404, got %d", res.Code)
	}

	// 4. Ordinary member cannot remove other members (must return 403)
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/groups/%d/members/%d", group.ID, otherID), memberToken, nil)
	if res.Code != http.StatusForbidden {
		t.Fatalf("non-owner remove another member: expected 403, got %d", res.Code)
	}

	// 5. Member cannot remove owner (must return 400)
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/groups/%d/members/%d", group.ID, ownerID), memberToken, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("remove owner: expected 400, got %d", res.Code)
	}

	// 6. Member CAN leave own membership (target == memberID)
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/groups/%d/members/%d", group.ID, memberID), memberToken, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("member self-leave: expected 200, got %d", res.Code)
	}

	_ = db
}

// TestSensitiveDataSanitizationAndInformationLeakage asserts that passwords,
// internal error traces, and SQL queries are NEVER leaked in API responses.
func TestSensitiveDataSanitizationAndInformationLeakage(t *testing.T) {
	_, h, db := testServer(t)

	authorID, authorToken := registerTestUser(t, h, "sec_author")

	// 1. User registration & profile responses MUST NOT leak PasswordHash
	endpoints := []struct {
		method string
		path   string
		token  string
	}{
		{http.MethodGet, "/api/users/me", authorToken},
		{http.MethodGet, fmt.Sprintf("/api/users/%d", authorID), ""},
		{http.MethodGet, "/api/search?q=sec_author&type=users", ""},
	}

	for _, ep := range endpoints {
		res := request(t, h, ep.method, ep.path, ep.token, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("%s %s returned %d", ep.method, ep.path, res.Code)
		}
		body := res.Body.String()
		if strings.Contains(body, "password_hash") || strings.Contains(body, "PasswordHash") || strings.Contains(body, "$2a$") || strings.Contains(body, "$2b$") {
			t.Fatalf("%s %s leaked password hash in body: %s", ep.method, ep.path, body)
		}
	}

	// 2. Video author preload must not leak PasswordHash
	v := Video{AuthorID: authorID, Title: "Test V", MediaURL: "/media/test.mp4", Status: "published"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	res := request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d", v.ID), "", nil)
	if strings.Contains(res.Body.String(), "password_hash") || strings.Contains(res.Body.String(), "$2a$") {
		t.Fatalf("video author preload leaked password hash: %s", res.Body.String())
	}

	// 3. Comments author preload must not leak PasswordHash
	cmt := Comment{VideoID: v.ID, UserID: authorID, Body: "Test comment"}
	if err := db.Create(&cmt).Error; err != nil {
		t.Fatal(err)
	}
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d/comments", v.ID), "", nil)
	if strings.Contains(res.Body.String(), "password_hash") || strings.Contains(res.Body.String(), "$2a$") {
		t.Fatalf("comments author preload leaked password hash: %s", res.Body.String())
	}
}

// TestDirectMessagePrivacyAndReceiptIsolation validates that direct messages
// cannot be viewed or acknowledged by unauthorized third parties.
func TestDirectMessagePrivacyAndReceiptIsolation(t *testing.T) {
	_, h, db := testServer(t)

	aliceID, aliceToken := registerTestUser(t, h, "dm_alice")
	bobID, bobToken := registerTestUser(t, h, "dm_bob")
	_, charlieToken := registerTestUser(t, h, "dm_charlie")
	_ = aliceToken

	// Alice sends Bob a private message
	msg := ChatMessage{
		SenderID:    aliceID,
		RecipientID: bobID,
		Body:        "Secret between Alice and Bob",
		ClientID:    "msg-1",
		CreatedAt:   time.Now().UTC(),
	}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatal(err)
	}

	// 1. Charlie (stranger) querying history with Alice must NOT see Alice's message to Bob
	res := request(t, h, http.MethodGet, fmt.Sprintf("/api/messages?user_id=%d", aliceID), charlieToken, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("charlie query history returned %d", res.Code)
	}
	var charlieHistory struct {
		Items []ChatMessage `json:"items"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &charlieHistory)
	if len(charlieHistory.Items) != 0 {
		t.Fatalf("charlie history leaked private message: %+v", charlieHistory.Items)
	}

	// 2. Charlie querying history with Bob must NOT see Bob's message from Alice
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/messages?user_id=%d", bobID), charlieToken, nil)
	var charlieBobHistory struct {
		Items []ChatMessage `json:"items"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &charlieBobHistory)
	if len(charlieBobHistory.Items) != 0 {
		t.Fatalf("charlie-bob history leaked private message: %+v", charlieBobHistory.Items)
	}

	// 3. Charlie cannot acknowledge the message sent to Bob
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", msg.ID), charlieToken, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("charlie acking bob's message returned %d, expected 404", res.Code)
	}

	// 4. Bob CAN acknowledge own message
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", msg.ID), bobToken, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("bob acking own message returned %d: %s", res.Code, res.Body.String())
	}
}

// TestFollowSelfAndRepeatFollowIdempotency validates that self-follow is forbidden
// and repeat follow/unfollow operations are strictly idempotent.
func TestFollowSelfAndRepeatFollowIdempotency(t *testing.T) {
	_, h, _ := testServer(t)

	user1ID, user1Token := registerTestUser(t, h, "fol_user_one")
	user2ID, user2Token := registerTestUser(t, h, "fol_user_two")

	// 1. User 1 cannot follow themselves
	res := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", user1ID), user1Token, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("self-follow returned %d, expected 400", res.Code)
	}

	// 2. User 1 follows User 2
	res = request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", user2ID), user1Token, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("follow returned %d: %s", res.Code, res.Body.String())
	}

	// 3. User 1 follows User 2 AGAIN (idempotent PUT)
	res = request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", user2ID), user1Token, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("repeat follow returned %d: %s", res.Code, res.Body.String())
	}

	// 4. User 1 unfollows User 2
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/users/me/following/%d", user2ID), user1Token, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("unfollow returned %d: %s", res.Code, res.Body.String())
	}

	// 5. User 1 unfollows User 2 AGAIN (idempotent DELETE)
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/users/me/following/%d", user2ID), user1Token, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("repeat unfollow returned %d: %s", res.Code, res.Body.String())
	}

	_ = user2Token
}

// TestAccountValidationBoundaries validates input limits on user registration,
// login, and profile modification.
func TestAccountValidationBoundaries(t *testing.T) {
	_, h, db := testServer(t)

	// 1. Username length and character validation
	invalidUsernames := []string{
		"ab", // too short (<3)
		"a_very_long_username_exceeding_32_characters", // too long (>32)
		"user name",   // space
		"user@domain", // @ symbol
		"user.name",   // dot
		"user$name",   // symbol
		"-invalid-",   // hyphen
	}
	for i, un := range invalidUsernames {
		body := map[string]string{
			"username": un,
			"password": "validpassword123",
		}
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = fmt.Sprintf("10.10.1.%d:1234", i+1)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid username %q accepted: %d %s", un, rec.Code, rec.Body.String())
		}
	}

	// 2. Password length validation
	invalidPasswords := []string{
		"short",                 // < 8 bytes
		strings.Repeat("a", 73), // > 72 bytes (bcrypt max)
	}
	for i, pw := range invalidPasswords {
		body := map[string]string{
			"username": "valid_user_pw",
			"password": pw,
		}
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = fmt.Sprintf("10.10.2.%d:1234", i+1)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid password length accepted: %d %s", rec.Code, rec.Body.String())
		}
	}

	// 3. Profile update boundary checks
	userID, token := registerTestUser(t, h, "val_profile_user")

	// Nickname too long (>48)
	res := request(t, h, http.MethodPatch, "/api/users/me", token, map[string]string{
		"nickname": strings.Repeat("n", 49),
	})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("overlong nickname accepted: %d", res.Code)
	}

	// Bio too long (>280)
	res = request(t, h, http.MethodPatch, "/api/users/me", token, map[string]string{
		"bio": strings.Repeat("b", 281),
	})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("overlong bio accepted: %d", res.Code)
	}

	// Unowned avatar media URL rejected
	res = request(t, h, http.MethodPatch, "/api/users/me", token, map[string]string{
		"avatar_url": "/media/unowned_avatar.jpg",
	})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("unowned avatar accepted: %d", res.Code)
	}

	// Video asset used as avatar rejected (must be kind='image')
	asset := MediaAsset{
		OwnerID:   userID,
		URL:       "/media/my_video.mp4",
		Kind:      "video", // not image
		CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	res = request(t, h, http.MethodPatch, "/api/users/me", token, map[string]string{
		"avatar_url": asset.URL,
	})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("video asset as avatar accepted: %d", res.Code)
	}
}

// TestMySQLSecurityAndOwnershipBoundaries validates the security and resource
// ownership invariants against a real MySQL instance when configured.
func TestMySQLSecurityAndOwnershipBoundaries(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()

	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	h := s.Router()

	unique := time.Now().UnixNano()
	user1ID, user1Token := registerTestUser(t, h, fmt.Sprintf("mysecu1_%d", unique))
	user2ID, user2Token := registerTestUser(t, h, fmt.Sprintf("mysecu2_%d", unique))

	// 1. User 1 video ownership check on MySQL
	v := Video{
		AuthorID: user1ID,
		Title:    "MySQL Sec Video",
		MediaURL: "/media/mysql_sec.mp4",
		Status:   "published",
	}
	if err := tx.Create(&v).Error; err != nil {
		t.Fatal(err)
	}

	// User 2 cannot edit User 1's video
	res := request(t, h, http.MethodPatch, fmt.Sprintf("/api/videos/%d", v.ID), user2Token, map[string]any{"title": "Hacked"})
	if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
		t.Fatalf("MySQL user 2 edit user 1 video returned %d", res.Code)
	}

	// User 2 cannot delete User 1's video
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d", v.ID), user2Token, nil)
	if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
		t.Fatalf("MySQL user 2 delete user 1 video returned %d", res.Code)
	}

	// 2. Self follow check on MySQL
	res = request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", user1ID), user1Token, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("MySQL self follow returned %d", res.Code)
	}

	// 3. User 1 cannot acknowledge direct message sent to User 2
	msg := ChatMessage{
		SenderID:    user1ID,
		RecipientID: user2ID,
		Body:        "Hello MySQL",
		ClientID:    fmt.Sprintf("mysecmsg_%d", unique),
		CreatedAt:   time.Now().UTC(),
	}
	if err := tx.Create(&msg).Error; err != nil {
		t.Fatal(err)
	}
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", msg.ID), user1Token, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("MySQL sender acking recipient message returned %d, expected 404", res.Code)
	}
}
