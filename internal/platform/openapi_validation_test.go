package platform

import (
	"context"
	"encoding/json"
	"fmt"
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
	read := func(conn *websocket.Conn, name string) map[string]any {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatal(err)
		}
		validate(name, frame)
		return frame
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
	read(b, "WSServerDirectMessage")
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
	read(b, "WSServerGroupMessage")
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
