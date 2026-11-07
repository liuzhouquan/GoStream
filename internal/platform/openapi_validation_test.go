package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gorilla/websocket"
)

func validatedOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return doc
}

func responseContract(t *testing.T, doc *openapi3.T, next http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := httptest.NewRecorder()
		next.ServeHTTP(result, r)
		var operation *openapi3.Operation
		if item := doc.Paths.Value(r.URL.Path); item != nil {
			operation = item.GetOperation(r.Method)
		}
		for template, item := range doc.Paths.Map() {
			if operation != nil {
				break
			}
			a, b := strings.Split(template, "/"), strings.Split(r.URL.Path, "/")
			if len(a) != len(b) {
				continue
			}
			match := true
			for i := range a {
				if !strings.HasPrefix(a[i], "{") && a[i] != b[i] {
					match = false
					break
				}
			}
			if match {
				operation = item.GetOperation(r.Method)
				if operation != nil {
					break
				}
			}
		}
		if operation == nil {
			t.Errorf("undocumented operation %s %s", r.Method, r.URL.Path)
		} else {
			response := operation.Responses.Value(fmt.Sprint(result.Code))
			if response == nil {
				response = operation.Responses.Value("default")
			}
			if response == nil {
				t.Errorf("undocumented response %s %s %d", r.Method, r.URL.Path, result.Code)
			} else if len(result.Body.Bytes()) > 0 {
				media := response.Value.Content.Get("application/json")
				if media == nil || media.Schema == nil {
					t.Errorf("missing JSON response schema for %s %s %d", r.Method, r.URL.Path, result.Code)
				} else {
					var body any
					if err := json.Unmarshal(result.Body.Bytes(), &body); err != nil {
						t.Error(err)
					} else if err := media.Schema.Value.VisitJSON(body); err != nil {
						t.Errorf("response violates %s %s %d: %v", r.Method, r.URL.Path, result.Code, err)
					}
				}
			}
		}
		for key, values := range result.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(result.Code)
		_, _ = w.Write(result.Body.Bytes())
	})
}

func TestOpenAPIEmptyAndErrorResponses(t *testing.T) {
	_, router, _ := testServer(t)
	doc := validatedOpenAPI(t)
	h := responseContract(t, doc, router)
	_, token := registerTestUser(t, h, "contract_empty")
	for _, path := range []string{"/api/users/me/following", "/api/users/me/followers", "/api/messages/unread", "/api/groups", "/api/notifications", "/api/feed?scene=timeline"} {
		r := request(t, h, http.MethodGet, path, token, nil)
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
		}
	}
	r := request(t, h, http.MethodGet, "/api/users/me", "", nil)
	if r.Code != 401 {
		t.Fatalf("unauthorized=%d", r.Code)
	}
}

func TestOpenAPISchemasValidateActualSocketFrames(t *testing.T) {
	s, h, db := testServer(t)
	doc := validatedOpenAPI(t)
	sender, senderToken := registerTestUser(t, h, "frame_sender")
	recipient, recipientToken := registerTestUser(t, h, "frame_recipient")
	web := httptest.NewServer(h)
	t.Cleanup(web.Close)
	validate := func(name string, frame any) {
		t.Helper()
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Fatalf("missing %s", name)
		}
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		var wire any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if err := ref.Value.VisitJSON(wire); err != nil {
			t.Fatalf("%s: %v; actual=%v", name, err, frame)
		}
	}
	readFrame := func(conn *websocket.Conn) map[string]any {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	read := func(conn *websocket.Conn, name string) map[string]any {
		t.Helper()
		frame := readFrame(conn)
		validate(name, frame)
		return frame
	}
	readGroupAfterDirectReplay := func(conn *websocket.Conn, directMessageID uint) {
		t.Helper()
		for frames := 0; frames < 10; frames++ {
			frame := readFrame(conn)
			switch frame["type"] {
			case "message":
				validate("WSServerDirectMessage", frame)
				message, ok := frame["message"].(map[string]any)
				messageID, hasID := message["id"].(float64)
				if !ok || !hasID || uint(messageID) != directMessageID {
					t.Fatalf("unexpected direct replay before group delivery: %+v", frame)
				}
			case "group_message":
				validate("WSServerGroupMessage", frame)
				return
			default:
				t.Fatalf("unexpected frame before group delivery: %+v", frame)
			}
		}
		t.Fatalf("group message did not arrive after duplicate direct frames")
	}
	connect := func(token string) *websocket.Conn {
		t.Helper()
		conn, _, err := (&websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}).Dial("ws"+strings.TrimPrefix(web.URL, "http")+"/api/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		read(conn, "WSServerReady")
		return conn
	}
	a, b := connect(senderToken), connect(recipientToken)
	ping := map[string]any{"type": "ping"}
	validate("WSClientPing", ping)
	if err := a.WriteJSON(ping); err != nil {
		t.Fatal(err)
	}
	read(a, "WSServerPong")
	direct := map[string]any{"type": "message", "recipient_id": recipient, "client_id": "contract_direct", "body": "hello"}
	validate("WSClientDirectMessage", direct)
	if err := a.WriteJSON(direct); err != nil {
		t.Fatal(err)
	}
	read(a, "WSServerDirectAck")
	directFrame := read(b, "WSServerDirectMessage")
	directMessage, ok := directFrame["message"].(map[string]any)
	if !ok {
		t.Fatalf("direct frame has no message object: %+v", directFrame)
	}
	directMessageIDValue, ok := directMessage["id"].(float64)
	if !ok || directMessageIDValue <= 0 {
		t.Fatalf("direct frame has no persisted message ID: %+v", directFrame)
	}
	directMessageID := uint(directMessageIDValue)
	// Real-time hints and reconnect replay can deliver the same stored row twice.
	s.deliverRealtime(realtimeEvent{Kind: "direct", UserID: recipient, MessageID: directMessageID})
	r := request(t, h, http.MethodPost, "/api/groups", senderToken, map[string]string{"name": "Frames"})
	var group ChatGroup
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &group) != nil {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	r = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", group.ID), senderToken, map[string]uint{"user_id": recipient})
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	grouped := map[string]any{"type": "group_message", "group_id": group.ID, "client_id": "contract_group", "body": "group"}
	validate("WSClientGroupMessage", grouped)
	if err := a.WriteJSON(grouped); err != nil {
		t.Fatal(err)
	}
	read(a, "WSServerGroupAck")
	readGroupAfterDirectReplay(b, directMessageID)
	if err := a.WriteJSON(map[string]string{"type": "unsupported"}); err != nil {
		t.Fatal(err)
	}
	read(a, "WSServerError")
	if _, err := s.ProcessOutboxOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	read(b, "WSServerNotification")
	_ = b.Close()
	// Populate a bounded backlog to validate real replay and both sync markers.
	for i := 0; i < 105; i++ {
		if err := db.Create(&ChatMessage{SenderID: sender, RecipientID: recipient, ClientID: fmt.Sprintf("frame_d%d", i), Body: "pending"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&GroupMessage{GroupID: group.ID, SenderID: sender, ClientID: fmt.Sprintf("frame_g%d", i), Body: "pending"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	replay := connect(recipientToken)
	for range 100 {
		read(replay, "WSServerDirectMessage")
	}
	read(replay, "WSServerSyncRequired")
	for range 100 {
		read(replay, "WSServerGroupMessage")
	}
	read(replay, "WSServerGroupSyncRequired")
	// Demonstrate that a schema mutation is rejected, rather than self-round-tripping JSON.
	ref := doc.Components.Schemas["WSServerReady"].Value
	if err := ref.VisitJSON(map[string]any{"type": 5}); err == nil {
		t.Fatal("schema accepted incorrect frame type")
	}
}

func requestRawWithHeaders(t *testing.T, h http.Handler, method, path, token string, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func TestOpenAPIComprehensiveEndpointVariants(t *testing.T) {
	_, router, db := testServer(t)
	doc := validatedOpenAPI(t)
	h := responseContract(t, doc, router)

	// 1. Register users
	userA, tokenA := registerTestUser(t, h, "oapi_comp_a")
	userB, tokenB := registerTestUser(t, h, "oapi_comp_b")
	userC, tokenC := registerTestUser(t, h, "oapi_comp_c")

	// 2. POST /api/sessions (Login)
	res := request(t, h, http.MethodPost, "/api/sessions", "", map[string]any{"username": "oapi_comp_a", "password": "correct-horse-123"})
	if res.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, "/api/sessions", "", map[string]any{"username": "oapi_comp_a", "password": "wrong-password"})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("bad login expected 401, got %d: %s", res.Code, res.Body.String())
	}

	// 3. PATCH /api/users/me
	res = request(t, h, http.MethodPatch, "/api/users/me", tokenA, map[string]any{"nickname": "Updated Alice", "bio": "Software Engineer"})
	if res.Code != http.StatusOK {
		t.Fatalf("update profile expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPatch, "/api/users/me", tokenA, map[string]any{"nickname": ""})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("empty nickname update expected 400, got %d: %s", res.Code, res.Body.String())
	}

	// 4. Upload Session Lifecycle
	mp4Payload := make([]byte, 1024)
	copy(mp4Payload, []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2mp41"))
	mp4Sum := sha256.Sum256(mp4Payload)
	mp4Hash := hex.EncodeToString(mp4Sum[:])

	res = request(t, h, http.MethodPost, "/api/uploads/sessions", tokenA, map[string]any{"filename": "invalid.exe", "size": int64(len(mp4Payload))})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("bad ext upload session expected 400, got %d: %s", res.Code, res.Body.String())
	}

	res = request(t, h, http.MethodPost, "/api/uploads/sessions", tokenA, map[string]any{"filename": "video.mp4", "size": int64(len(mp4Payload)), "sha256": mp4Hash})
	if res.Code != http.StatusCreated {
		t.Fatalf("create upload session expected 201, got %d: %s", res.Code, res.Body.String())
	}
	var sessResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &sessResp); err != nil {
		t.Fatal(err)
	}
	sessID := sessResp["id"].(string)

	res = request(t, h, http.MethodGet, "/api/uploads/sessions/"+sessID, tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("get upload session expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodGet, "/api/uploads/sessions/0123456789abcdef0123456789abcdef", tokenA, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("nonexistent upload session expected 404, got %d: %s", res.Code, res.Body.String())
	}

	chunkHeaders := map[string]string{"Content-Type": "application/octet-stream"}
	res = requestRawWithHeaders(t, h, http.MethodPut, fmt.Sprintf("/api/uploads/sessions/%s/chunks/0", sessID), tokenA, chunkHeaders, mp4Payload)
	if res.Code != http.StatusOK {
		t.Fatalf("put chunk expected 200, got %d: %s", res.Code, res.Body.String())
	}

	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/uploads/sessions/%s/complete", sessID), tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("complete upload expected 200, got %d: %s", res.Code, res.Body.String())
	}
	var compResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &compResp); err != nil {
		t.Fatal(err)
	}
	videoMediaURL := compResp["url"].(string)

	// 5. Video Publishing & Management
	res = request(t, h, http.MethodPost, "/api/videos", tokenA, map[string]any{"media_url": videoMediaURL})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("publish video missing title expected 400, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, "/api/videos", tokenA, map[string]any{"title": "OpenAPI Parity Video", "description": "Verification payload", "media_url": videoMediaURL})
	if res.Code != http.StatusCreated {
		t.Fatalf("publish video expected 201, got %d: %s", res.Code, res.Body.String())
	}
	var vidResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &vidResp); err != nil {
		t.Fatal(err)
	}
	vidID := uint(vidResp["id"].(float64))

	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d", vidID), "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("get video expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodGet, "/api/videos/99999", "", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("get nonexistent video expected 404, got %d: %s", res.Code, res.Body.String())
	}

	res = request(t, h, http.MethodPatch, fmt.Sprintf("/api/videos/%d", vidID), tokenB, map[string]any{"title": "Hacked Title"})
	if res.Code != http.StatusForbidden {
		t.Fatalf("patch video by non-author expected 403, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPatch, "/api/videos/99999", tokenA, map[string]any{"title": "Nonexistent"})
	if res.Code != http.StatusNotFound {
		t.Fatalf("patch nonexistent video expected 404, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPatch, fmt.Sprintf("/api/videos/%d", vidID), tokenA, map[string]any{"title": "Updated Title", "description": "Updated desc"})
	if res.Code != http.StatusOK {
		t.Fatalf("patch video expected 200, got %d: %s", res.Code, res.Body.String())
	}

	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/users/%d/videos", userA), "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("get creator videos expected 200, got %d: %s", res.Code, res.Body.String())
	}

	// 6. Comments & Replies
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", vidID), tokenB, map[string]any{"body": "  "})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("empty comment body expected 400, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, "/api/videos/99999/comments", tokenB, map[string]any{"body": "Hello"})
	if res.Code != http.StatusNotFound {
		t.Fatalf("comment on nonexistent video expected 404, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", vidID), tokenB, map[string]any{"body": "First top-level comment"})
	if res.Code != http.StatusCreated {
		t.Fatalf("create top-level comment expected 201, got %d: %s", res.Code, res.Body.String())
	}
	var commentResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &commentResp); err != nil {
		t.Fatal(err)
	}
	topCommentID := uint(commentResp["id"].(float64))

	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", vidID), tokenA, map[string]any{"body": "First reply", "parent_id": topCommentID})
	if res.Code != http.StatusCreated {
		t.Fatalf("create reply comment expected 201, got %d: %s", res.Code, res.Body.String())
	}

	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d/comments", vidID), "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("list comments expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d/comments/%d/replies", vidID, topCommentID), "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("list replies expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d/comments/99999/replies", vidID), "", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("list replies on nonexistent comment expected 404, got %d: %s", res.Code, res.Body.String())
	}

	// 7. Search
	res = request(t, h, http.MethodGet, "/api/search?q=a", "", nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("short search query expected 400, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodGet, "/api/search?q=Parity", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("search query expected 200, got %d: %s", res.Code, res.Body.String())
	}

	// 8. Unread Notifications Count
	res = request(t, h, http.MethodGet, "/api/notifications/unread", tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("unread notifications expected 200, got %d: %s", res.Code, res.Body.String())
	}

	// 9. Direct IM History & Acks
	chatMsg := ChatMessage{SenderID: userA, RecipientID: userB, ClientID: "comp_dir_1", Body: "contract test im", CreatedAt: time.Now().UTC()}
	if err := db.Create(&chatMsg).Error; err != nil {
		t.Fatal(err)
	}
	res = request(t, h, http.MethodGet, "/api/messages?user_id=0", tokenB, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("invalid user_id messages expected 400, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/messages?user_id=%d", userA), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("get direct messages expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", chatMsg.ID), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("ack direct message expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, "/api/messages/acks", tokenB, map[string]any{"ids": []uint{chatMsg.ID}})
	if res.Code != http.StatusOK {
		t.Fatalf("batch ack direct messages expected 200, got %d: %s", res.Code, res.Body.String())
	}

	// 10. Groups & Group Messages
	res = request(t, h, http.MethodPost, "/api/groups", tokenA, map[string]any{"name": ""})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("empty group name expected 400, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, "/api/groups", tokenA, map[string]any{"name": "Parity Group"})
	if res.Code != http.StatusCreated {
		t.Fatalf("create group expected 201, got %d: %s", res.Code, res.Body.String())
	}
	var compGrpResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &compGrpResp); err != nil {
		t.Fatal(err)
	}
	compGrpID := uint(compGrpResp["id"].(float64))

	res = request(t, h, http.MethodGet, "/api/groups", tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("list groups expected 200, got %d: %s", res.Code, res.Body.String())
	}

	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", compGrpID), tokenA, map[string]any{"user_id": userB})
	if res.Code != http.StatusOK {
		t.Fatalf("owner add member expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", compGrpID), tokenB, map[string]any{"user_id": userC})
	if res.Code != http.StatusForbidden {
		t.Fatalf("non-owner add member expected 403, got %d: %s", res.Code, res.Body.String())
	}

	grpMsg := GroupMessage{GroupID: compGrpID, SenderID: userA, ClientID: "comp_grp_1", Body: "contract test group im", CreatedAt: time.Now().UTC()}
	if err := db.Create(&grpMsg).Error; err != nil {
		t.Fatal(err)
	}

	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/messages", compGrpID), tokenC, nil)
	if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
		t.Fatalf("non-member get group messages expected 403/404, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/messages", compGrpID), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("member get group messages expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/messages/%d/ack", compGrpID, grpMsg.ID), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("ack group message expected 200, got %d: %s", res.Code, res.Body.String())
	}

	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/groups/%d/members/%d", compGrpID, userA), tokenA, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("group owner leaving expected 400, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/groups/%d/members/%d", compGrpID, userB), tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("remove group member expected 200, got %d: %s", res.Code, res.Body.String())
	}

	// 11. Follows and Interactions
	res = request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", userA), tokenA, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("self follow expected 400, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", userB), tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("follow expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodGet, "/api/users/me/followers", tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("get followers expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/users/me/following/%d", userB), tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("unfollow expected 200, got %d: %s", res.Code, res.Body.String())
	}

	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d/favorite", vidID), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("unfavorite expected 200, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodDelete, "/api/videos/99999/like", tokenB, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("unlike nonexistent video expected 404, got %d: %s", res.Code, res.Body.String())
	}

	// 12. Video Deletion
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d", vidID), tokenB, nil)
	if res.Code != http.StatusForbidden {
		t.Fatalf("delete video by non-author expected 403, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d", vidID), tokenA, nil)
	if res.Code != http.StatusNoContent {
		t.Fatalf("delete video expected 204, got %d: %s", res.Code, res.Body.String())
	}
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d", vidID), tokenA, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("delete already deleted video expected 404, got %d: %s", res.Code, res.Body.String())
	}

	// 13. Unauthenticated Protected Endpoints (401 Unauthorized)
	unauthRoutes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/notifications"},
		{http.MethodPost, "/api/videos"},
		{http.MethodGet, "/api/groups"},
		{http.MethodGet, "/api/messages?user_id=1"},
		{http.MethodGet, "/api/users/me/following"},
		{http.MethodGet, "/api/users/me/followers"},
		{http.MethodPost, "/api/uploads/sessions"},
	}
	for _, target := range unauthRoutes {
		r := request(t, h, target.method, target.path, "", nil)
		if r.Code != http.StatusUnauthorized {
			t.Errorf("%s %s expected 401 without auth, got %d: %s", target.method, target.path, r.Code, r.Body.String())
		}
	}
}
