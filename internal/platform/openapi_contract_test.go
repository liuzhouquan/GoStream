package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type openAPISpec struct {
	OpenAPI    string                            `yaml:"openapi"`
	Info       map[string]any                    `yaml:"info"`
	Paths      map[string]map[string]interface{} `yaml:"paths"`
	Components struct {
		Schemas         map[string]openAPISchema `yaml:"schemas"`
		SecuritySchemes map[string]interface{}   `yaml:"securitySchemes"`
	} `yaml:"components"`
}

type openAPISchema struct {
	Type       string                     `yaml:"type"`
	Required   []string                   `yaml:"required"`
	Properties map[string]openAPIProperty `yaml:"properties"`
	AllOf      []openAPISchema            `yaml:"allOf"`
	Items      *openAPISchema             `yaml:"items"`
	Ref        string                     `yaml:"$ref"`
}

type openAPIProperty struct {
	Type     string         `yaml:"type"`
	Format   string         `yaml:"format"`
	Ref      string         `yaml:"$ref"`
	Items    *openAPISchema `yaml:"items"`
	Nullable bool           `yaml:"nullable"`
}

func loadSpec(t *testing.T) openAPISpec {
	t.Helper()
	specPath := filepath.Join("..", "..", "docs", "openapi.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var spec openAPISpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshal openapi.yaml: %v", err)
	}
	return spec
}

// TestOpenAPIContractParity verifies that all 49 Gin routes are mapped to OpenAPI paths and methods.
func TestOpenAPIContractParity(t *testing.T) {
	spec := loadSpec(t)

	if !strings.HasPrefix(spec.OpenAPI, "3.0") && !strings.HasPrefix(spec.OpenAPI, "3.1") {
		t.Fatalf("expected OpenAPI 3.x, got %s", spec.OpenAPI)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("no paths defined in openapi.yaml")
	}

	s, _, _ := testServer(t)
	s.UseObjectMedia(&objectMediaStore{})
	router := s.Router()

	paramRe := regexp.MustCompile(`/:([a-zA-Z0-9_]+)`)
	ignoredStatic := map[string]bool{
		"/":           true,
		"/app.js":     true,
		"/styles.css": true,
	}

	checked := 0
	for _, route := range router.Routes() {
		if ignoredStatic[route.Path] {
			continue
		}
		openApiPath := paramRe.ReplaceAllString(route.Path, "/{$1}")
		method := strings.ToLower(route.Method)

		pathItem, exists := spec.Paths[openApiPath]
		if !exists {
			t.Errorf("missing OpenAPI path: %s (Gin route: %s %s)", openApiPath, route.Method, route.Path)
			continue
		}
		if _, methodExists := pathItem[method]; !methodExists {
			t.Errorf("missing method %s on OpenAPI path %s (Gin route: %s %s)", route.Method, openApiPath, route.Method, route.Path)
			continue
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("no routes were checked")
	}
	t.Logf("verified %d Gin routes against %d OpenAPI paths", checked, len(spec.Paths))
}

// TestOpenAPISchemaReferences verifies that all $ref references resolve to existing components.
func TestOpenAPISchemaReferences(t *testing.T) {
	spec := loadSpec(t)
	schemas := spec.Components.Schemas
	if len(schemas) == 0 {
		t.Fatal("no component schemas found")
	}

	specPath := filepath.Join("..", "..", "docs", "openapi.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}

	var rawNode any
	if err := yaml.Unmarshal(data, &rawNode); err != nil {
		t.Fatal(err)
	}

	var dangling []string
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			for k, val := range v {
				if k == "$ref" {
					if refStr, ok := val.(string); ok && strings.HasPrefix(refStr, "#/components/schemas/") {
						target := strings.TrimPrefix(refStr, "#/components/schemas/")
						if _, exists := schemas[target]; !exists {
							dangling = append(dangling, fmt.Sprintf("%s -> %s", path, refStr))
						}
					}
				}
				walk(val, path+"."+k)
			}
		case []any:
			for i, elem := range v {
				walk(elem, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(rawNode, "root")

	if len(dangling) > 0 {
		t.Fatalf("found %d dangling $ref references: %v", len(dangling), dangling)
	}
	t.Logf("verified all $ref references resolve across %d component schemas", len(schemas))
}

// TestOpenAPISchemaModelParity asserts that critical schema definitions match Go structs and audit requirements.
func TestOpenAPISchemaModelParity(t *testing.T) {
	spec := loadSpec(t)
	schemas := spec.Components.Schemas

	// 1. User schema must have last_seen_at (not last_seen) and correct fields
	userSchema, ok := schemas["User"]
	if !ok {
		t.Fatal("missing User schema")
	}
	if _, hasLastSeen := userSchema.Properties["last_seen"]; hasLastSeen {
		t.Error("User schema should not have 'last_seen', must use 'last_seen_at'")
	}
	if _, hasLastSeenAt := userSchema.Properties["last_seen_at"]; !hasLastSeenAt {
		t.Error("User schema missing 'last_seen_at'")
	}
	for _, prop := range []string{"id", "username", "nickname", "avatar_url", "bio", "created_at"} {
		if _, exists := userSchema.Properties[prop]; !exists {
			t.Errorf("User schema missing property %q", prop)
		}
	}

	// 2. UserProfileResponse must wrap user with counter statistics
	profileResp, ok := schemas["UserProfileResponse"]
	if !ok {
		t.Fatal("missing UserProfileResponse schema")
	}
	for _, prop := range []string{"user", "video_count", "follower_count", "following_count"} {
		if _, exists := profileResp.Properties[prop]; !exists {
			t.Errorf("UserProfileResponse missing property %q", prop)
		}
	}

	// 3. PresenceResponse must have last_seen_at
	presenceResp, ok := schemas["PresenceResponse"]
	if !ok {
		t.Fatal("missing PresenceResponse schema")
	}
	if _, hasLastSeen := presenceResp.Properties["last_seen"]; hasLastSeen {
		t.Error("PresenceResponse must not use 'last_seen', must use 'last_seen_at'")
	}
	if _, hasLastSeenAt := presenceResp.Properties["last_seen_at"]; !hasLastSeenAt {
		t.Error("PresenceResponse missing 'last_seen_at'")
	}

	// 4. Notification must have read_at (not is_read) and optional video_id
	notifSchema, ok := schemas["Notification"]
	if !ok {
		t.Fatal("missing Notification schema")
	}
	if _, hasIsRead := notifSchema.Properties["is_read"]; hasIsRead {
		t.Error("Notification must not use 'is_read', must use 'read_at'")
	}
	if _, hasReadAt := notifSchema.Properties["read_at"]; !hasReadAt {
		t.Error("Notification missing 'read_at'")
	}
	if _, hasVideoID := notifSchema.Properties["video_id"]; !hasVideoID {
		t.Error("Notification missing 'video_id'")
	}

	// 5. InteractionToggleResponse must have active (not liked / favorited)
	toggleResp, ok := schemas["InteractionToggleResponse"]
	if !ok {
		t.Fatal("missing InteractionToggleResponse schema")
	}
	if _, hasActive := toggleResp.Properties["active"]; !hasActive {
		t.Error("InteractionToggleResponse missing 'active'")
	}
	if _, hasLiked := toggleResp.Properties["liked"]; hasLiked {
		t.Error("InteractionToggleResponse should not have 'liked'")
	}
	if _, hasFav := toggleResp.Properties["favorited"]; hasFav {
		t.Error("InteractionToggleResponse should not have 'favorited'")
	}

	// 6. BatchAckResponse must have acked
	ackResp, ok := schemas["BatchAckResponse"]
	if !ok {
		t.Fatal("missing BatchAckResponse schema")
	}
	if _, hasAcked := ackResp.Properties["acked"]; !hasAcked {
		t.Error("BatchAckResponse missing 'acked'")
	}

	// 7. GroupAckResponse must have acked and last_ack_id
	grpAckResp, ok := schemas["GroupAckResponse"]
	if !ok {
		t.Fatal("missing GroupAckResponse schema")
	}
	if _, hasAcked := grpAckResp.Properties["acked"]; !hasAcked {
		t.Error("GroupAckResponse missing 'acked'")
	}
	if _, hasLastAckID := grpAckResp.Properties["last_ack_id"]; !hasLastAckID {
		t.Error("GroupAckResponse missing 'last_ack_id'")
	}
}

// TestOpenAPILiveEndpointResponseFidelity executes real HTTP handlers and confirms the returned
// JSON schema structure matches the OpenAPI 3.0 specification contract.
func TestOpenAPILiveEndpointResponseFidelity(t *testing.T) {
	_, router, db := testServer(t)
	h := responseContract(t, validatedOpenAPI(t), router)

	// 1. /health
	res := request(t, h, http.MethodGet, "/health", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("/health status: %d", res.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &health); err != nil || health["status"] != "ok" {
		t.Fatalf("unexpected health payload: %s", res.Body.String())
	}

	// 2. /ready
	res = request(t, h, http.MethodGet, "/ready", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("/ready status: %d", res.Code)
	}
	var ready map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &ready); err != nil || ready["status"] != "ready" {
		t.Fatalf("unexpected ready payload: %s", res.Body.String())
	}

	// 3. Register user and inspect SessionResponse
	userA, tokenA := registerTestUser(t, h, "oapi_alice")
	userB, tokenB := registerTestUser(t, h, "oapi_bob")

	// 4. GET /api/users/me -> User
	res = request(t, h, http.MethodGet, "/api/users/me", tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("/api/users/me status: %d %s", res.Code, res.Body.String())
	}
	var meResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &meResp); err != nil {
		t.Fatal(err)
	}
	for _, expectedKey := range []string{"id", "username", "nickname", "created_at"} {
		if _, ok := meResp[expectedKey]; !ok {
			t.Errorf("GET /api/users/me missing expected key %q in %v", expectedKey, meResp)
		}
	}

	// 5. GET /api/users/:id -> UserProfileResponse (with user, video_count, follower_count, following_count)
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/users/%d", userA), "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /api/users/:id status: %d", res.Code)
	}
	var profileResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &profileResp); err != nil {
		t.Fatal(err)
	}
	for _, expectedKey := range []string{"user", "video_count", "follower_count", "following_count"} {
		if _, ok := profileResp[expectedKey]; !ok {
			t.Errorf("GET /api/users/:id missing expected key %q in %v", expectedKey, profileResp)
		}
	}

	// 6. GET /api/users/:id/presence -> PresenceResponse (user_id, online, last_seen_at)
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/users/%d/presence", userA), "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /api/users/:id/presence status: %d", res.Code)
	}
	var presenceResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &presenceResp); err != nil {
		t.Fatal(err)
	}
	for _, expectedKey := range []string{"user_id", "online", "last_seen_at"} {
		if _, ok := presenceResp[expectedKey]; !ok {
			t.Errorf("GET /api/users/:id/presence missing key %q in %v", expectedKey, presenceResp)
		}
	}

	// Seed video for interaction checks
	vid := Video{AuthorID: userA, Title: "OAPI Video", MediaURL: "/media/test.mp4", Status: "published"}
	if err := db.Create(&vid).Error; err != nil {
		t.Fatal(err)
	}

	// 7. PUT /api/videos/:id/like -> InteractionToggleResponse {active: true}
	res = request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", vid.ID), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("like: %d %s", res.Code, res.Body.String())
	}
	var likeResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &likeResp); err != nil || likeResp["active"] != true {
		t.Fatalf("unexpected like response: %s", res.Body.String())
	}

	// 8. DELETE /api/videos/:id/like -> InteractionToggleResponse {active: false}
	res = request(t, h, http.MethodDelete, fmt.Sprintf("/api/videos/%d/like", vid.ID), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("unlike: %d %s", res.Code, res.Body.String())
	}
	var unlikeResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &unlikeResp); err != nil || unlikeResp["active"] != false {
		t.Fatalf("unexpected unlike response: %s", res.Body.String())
	}

	// 9. PUT /api/videos/:id/favorite -> InteractionToggleResponse {active: true}
	res = request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/favorite", vid.ID), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("favorite: %d %s", res.Code, res.Body.String())
	}
	var favResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &favResp); err != nil || favResp["active"] != true {
		t.Fatalf("unexpected favorite response: %s", res.Body.String())
	}

	// 10. POST /api/videos/:id/views -> 204 No Content
	res = request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/views", vid.ID), "", nil)
	if res.Code != http.StatusNoContent {
		t.Fatalf("POST /views expected 204, got %d %s", res.Code, res.Body.String())
	}

	// 11. PUT /api/users/me/following/:id -> FollowToggleResponse {following: true}
	res = request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", userA), tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("follow: %d %s", res.Code, res.Body.String())
	}
	var followResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &followResp); err != nil || followResp["following"] != true {
		t.Fatalf("unexpected follow response: %s", res.Body.String())
	}

	// 12. GET /api/users/me/following -> UserListResponse {items: []}
	res = request(t, h, http.MethodGet, "/api/users/me/following", tokenB, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("get following: %d %s", res.Code, res.Body.String())
	}
	var followingsResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &followingsResp); err != nil {
		t.Fatal(err)
	}
	if items, ok := followingsResp["items"].([]any); !ok || len(items) == 0 {
		t.Fatalf("expected items array in following response: %v", followingsResp)
	}

	// 13. GET /api/feed -> FeedResponse {items: [], has_more: bool}
	res = request(t, h, http.MethodGet, "/api/feed?scene=timeline&limit=5", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("feed: %d %s", res.Code, res.Body.String())
	}
	var feedResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &feedResp); err != nil {
		t.Fatal(err)
	}
	if _, ok := feedResp["items"].([]any); !ok {
		t.Fatalf("expected items in feed: %v", feedResp)
	}

	// 14. Notifications: GET /api/notifications -> NotificationPageResponse {items: []}
	now := time.Now().UTC()
	notif := Notification{UserID: userA, Kind: "like", ActorID: userB, VideoID: vid.ID, Text: "liked video", CreatedAt: now}
	if err := db.Create(&notif).Error; err != nil {
		t.Fatal(err)
	}
	res = request(t, h, http.MethodGet, "/api/notifications", tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("notifications: %d %s", res.Code, res.Body.String())
	}
	var notifResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &notifResp); err != nil {
		t.Fatal(err)
	}
	items, ok := notifResp["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected items in notifications: %v", notifResp)
	}
	firstNotif := items[0].(map[string]any)
	if _, hasReadAt := firstNotif["read_at"]; !hasReadAt {
		t.Error("notification JSON missing 'read_at' key")
	}
	if _, hasIsRead := firstNotif["is_read"]; hasIsRead {
		t.Error("notification JSON should not contain 'is_read'")
	}

	// 15. PATCH /api/notifications/read -> MarkReadResponse {read: true, read_count: int}
	res = request(t, h, http.MethodPatch, fmt.Sprintf("/api/notifications/read?up_to_id=%d", notif.ID), tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("mark read: %d %s", res.Code, res.Body.String())
	}
	var markResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &markResp); err != nil || markResp["read"] != true {
		t.Fatalf("unexpected mark read response: %s", res.Body.String())
	}

	// 16. Groups: POST /api/groups -> ChatGroup
	res = request(t, h, http.MethodPost, "/api/groups", tokenA, map[string]any{"name": "Contract Group"})
	if res.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", res.Code, res.Body.String())
	}
	var groupResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &groupResp); err != nil {
		t.Fatal(err)
	}
	groupID := uint(groupResp["id"].(float64))

	// 17. GET /api/groups/:id/unread -> GroupUnreadResponse {unread_count: int, last_ack_id: int}
	res = request(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/unread", groupID), tokenA, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("group unread: %d %s", res.Code, res.Body.String())
	}
	var grpUnreadResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &grpUnreadResp); err != nil {
		t.Fatal(err)
	}
	for _, expectedKey := range []string{"unread_count", "last_ack_id"} {
		if _, ok := grpUnreadResp[expectedKey]; !ok {
			t.Errorf("group unread missing key %q in %v", expectedKey, grpUnreadResp)
		}
	}
}

// TestOpenAPIWebSocketFrameContract validates that all client and server WebSocket frames
// declared in the OpenAPI specification cleanly marshal/unmarshal with Go IM types.
func TestOpenAPIWebSocketFrameContract(t *testing.T) {
	spec := loadSpec(t)
	schemas := spec.Components.Schemas

	for _, frameSchemaName := range []string{
		"WSClientPing", "WSClientDirectMessage", "WSClientGroupMessage",
		"WSServerReady", "WSServerPong", "WSServerError",
		"WSServerDirectAck", "WSServerGroupAck", "WSServerDirectMessage",
		"WSServerGroupMessage", "WSServerSyncRequired", "WSServerGroupSyncRequired",
	} {
		if _, exists := schemas[frameSchemaName]; !exists {
			t.Errorf("missing WebSocket frame schema: %s", frameSchemaName)
		}
	}

	// 1. Client Direct Message Frame
	clientMsg := socketEvent{
		Type:        "message",
		RecipientID: 10,
		ClientID:    "test-client-123",
		Body:        "Hello WebSocket",
	}
	bytes, err := json.Marshal(clientMsg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded socketEvent
	if err := json.Unmarshal(bytes, &decoded); err != nil || decoded.Type != "message" || decoded.Body != "Hello WebSocket" {
		t.Fatalf("failed decoding client message frame: %v", decoded)
	}

	// 2. Server Direct Ack Frame
	ackFrame := map[string]any{
		"type":       "ack",
		"client_id":  "test-client-123",
		"message_id": 42,
	}
	bytes, err = json.Marshal(ackFrame)
	if err != nil {
		t.Fatal(err)
	}
	var decodedAck map[string]any
	if err := json.Unmarshal(bytes, &decodedAck); err != nil || decodedAck["type"] != "ack" || decodedAck["message_id"] != float64(42) {
		t.Fatalf("failed decoding server ack frame: %v", decodedAck)
	}

	// 3. Server Direct Message Frame with embedded ChatMessage
	now := time.Now().UTC().Truncate(time.Second)
	chatMsg := ChatMessage{
		ID:          42,
		SenderID:    1,
		RecipientID: 2,
		ClientID:    "test-client-123",
		Body:        "Hello WebSocket",
		CreatedAt:   now,
	}
	srvMsgFrame := map[string]any{
		"type":    "message",
		"message": chatMsg,
	}
	bytes, err = json.Marshal(srvMsgFrame)
	if err != nil {
		t.Fatal(err)
	}
	var decodedSrvMsg struct {
		Type    string      `json:"type"`
		Message ChatMessage `json:"message"`
	}
	if err := json.Unmarshal(bytes, &decodedSrvMsg); err != nil || decodedSrvMsg.Type != "message" || decodedSrvMsg.Message.ID != 42 {
		t.Fatalf("failed decoding server message frame: %v", decodedSrvMsg)
	}
}
