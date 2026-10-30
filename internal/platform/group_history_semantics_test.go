package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestGroupMembershipLifecycleAndHistorySemantics verifies the complete membership
// boundary lifecycle and product history visibility semantics:
// 1. Non-members receive 404/403 across history, unread, ack, and sending routes.
// 2. Newly invited members have unread cursor advanced to latest message (initial unread = 0).
// 3. Removed members immediately lose all permissions and access to group history and state.
// 4. Messages sent while a member is removed do not cause backlog unread upon re-invitation.
// 5. Re-invited members have their unread cursor again advanced to the newest message.
func TestGroupMembershipLifecycleAndHistorySemantics(t *testing.T) {
	_, h, db := testServer(t)

	ownerID, ownerToken := registerTestUser(t, h, "hist_owner")
	memberID, memberToken := registerTestUser(t, h, "hist_member")
	_, outsiderToken := registerTestUser(t, h, "hist_outsider")

	// 1. Owner creates group
	created := request(t, h, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "History Semantics Group"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", created.Code, created.Body.String())
	}
	var group ChatGroup
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/groups/%d", group.ID)

	// 2. Outsider (non-member) cannot read history, unread, or ack
	if r := request(t, h, http.MethodGet, base+"/messages", outsiderToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("outsider history: expected 404, got %d", r.Code)
	}
	if r := request(t, h, http.MethodGet, base+"/unread", outsiderToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("outsider unread: expected 404, got %d", r.Code)
	}
	if r := request(t, h, http.MethodPost, base+"/messages/1/ack", outsiderToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("outsider ack: expected 404, got %d", r.Code)
	}

	// 3. Owner sends initial message 1 before member joins
	ts := httptest.NewServer(h)
	defer ts.Close()

	connectWS := func(token string) *websocket.Conn {
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
		// ready precedes offline replay. A normal ping is handled only after
		// replay, making later live-message assertions deterministic.
		if err := conn.WriteJSON(socketEvent{Type: "ping"}); err != nil {
			t.Fatal(err)
		}
		var pong map[string]any
		if err := conn.ReadJSON(&pong); err != nil || pong["type"] != "pong" {
			t.Fatalf("replay barrier: %v %v", pong, err)
		}
		return conn
	}

	ownerWS := connectWS(ownerToken)
	if err := ownerWS.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: "pre-join-1", Body: "message 1 before member joins"}); err != nil {
		t.Fatal(err)
	}
	var ack1 struct {
		Type      string `json:"type"`
		MessageID uint   `json:"message_id"`
	}
	if err := ownerWS.ReadJSON(&ack1); err != nil || ack1.Type != "ack" || ack1.MessageID == 0 {
		t.Fatalf("ack 1: %+v %v", ack1, err)
	}

	// Wait for the sender's post-commit fanout before changing membership.
	if err := ownerWS.WriteJSON(socketEvent{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	var ownerPong map[string]any
	if err := ownerWS.ReadJSON(&ownerPong); err != nil || ownerPong["type"] != "pong" {
		t.Fatalf("sender fanout barrier: %v %v", ownerPong, err)
	}

	// 4. Owner invites member
	if r := request(t, h, http.MethodPost, base+"/members", ownerToken, map[string]any{"user_id": memberID}); r.Code != http.StatusOK {
		t.Fatalf("invite member: %d %s", r.Code, r.Body.String())
	}

	// 5. Initial unread count for newly invited member must be 0 (cursor advanced to msg 1)
	unreadRes := request(t, h, http.MethodGet, base+"/unread", memberToken, nil)
	if unreadRes.Code != http.StatusOK || !bytes.Contains(unreadRes.Body.Bytes(), []byte(`"unread_count":0`)) {
		t.Fatalf("initial unread count for new member must be 0, got %d: %s", unreadRes.Code, unreadRes.Body.String())
	}

	// 6. Member can read group history (as a current group member)
	histRes := request(t, h, http.MethodGet, base+"/messages", memberToken, nil)
	if histRes.Code != http.StatusOK {
		t.Fatalf("member history query expected 200, got %d: %s", histRes.Code, histRes.Body.String())
	}
	var history struct {
		Items []GroupMessage `json:"items"`
	}
	if err := json.Unmarshal(histRes.Body.Bytes(), &history); err != nil || len(history.Items) != 1 || history.Items[0].ID != ack1.MessageID {
		t.Fatalf("member history items: %+v", history)
	}

	// 7. Owner sends message 2 while member is active
	memberWS := connectWS(memberToken)
	if err := ownerWS.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: "active-msg-2", Body: "message 2 while member active"}); err != nil {
		t.Fatal(err)
	}
	var ack2 struct {
		Type      string `json:"type"`
		MessageID uint   `json:"message_id"`
	}
	if err := ownerWS.ReadJSON(&ack2); err != nil || ack2.Type != "ack" {
		t.Fatalf("ack 2: %+v", ack2)
	}

	// Member receives live delivery
	var deliv2 struct {
		Type    string       `json:"type"`
		Message GroupMessage `json:"message"`
	}
	if err := memberWS.ReadJSON(&deliv2); err != nil || deliv2.Type != "group_message" || deliv2.Message.ID != ack2.MessageID {
		t.Fatalf("member live delivery: %+v", deliv2)
	}

	// Member unread is now 1
	unreadRes = request(t, h, http.MethodGet, base+"/unread", memberToken, nil)
	if unreadRes.Code != http.StatusOK || !bytes.Contains(unreadRes.Body.Bytes(), []byte(`"unread_count":1`)) {
		t.Fatalf("unread count expected 1, got: %s", unreadRes.Body.String())
	}

	// Member ACKs message 2
	if r := request(t, h, http.MethodPost, fmt.Sprintf("%s/messages/%d/ack", base, ack2.MessageID), memberToken, nil); r.Code != http.StatusOK {
		t.Fatalf("member ack msg 2: %d", r.Code)
	}

	// 8. Owner removes member from group
	remRes := request(t, h, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, memberID), ownerToken, nil)
	if remRes.Code != http.StatusOK {
		t.Fatalf("remove member: %d %s", remRes.Code, remRes.Body.String())
	}

	// 9. Removed member immediately loses all access
	if r := request(t, h, http.MethodGet, base+"/messages", memberToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("removed member history: expected 404, got %d", r.Code)
	}
	if r := request(t, h, http.MethodGet, base+"/unread", memberToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("removed member unread: expected 404, got %d", r.Code)
	}
	if r := request(t, h, http.MethodPost, fmt.Sprintf("%s/messages/%d/ack", base, ack2.MessageID), memberToken, nil); r.Code != http.StatusNotFound {
		t.Fatalf("removed member ack: expected 404, got %d", r.Code)
	}

	// Removed member WebSocket sending is rejected with error
	if err := memberWS.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: "removed-msg", Body: "should be rejected"}); err != nil {
		t.Fatal(err)
	}
	var rawEvent map[string]any
	if err := memberWS.ReadJSON(&rawEvent); err != nil {
		t.Fatal(err)
	}
	if rawEvent["type"] != "error" || rawEvent["message"] != "group not found" {
		t.Fatalf("removed member send: expected group not found error, got %+v", rawEvent)
	}

	// 10. Owner sends messages 3 and 4 while member is absent
	for i, body := range []string{"message 3 while absent", "message 4 while absent"} {
		if err := ownerWS.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: fmt.Sprintf("absent-msg-%d", i+3), Body: body}); err != nil {
			t.Fatal(err)
		}
		var ack struct {
			Type string `json:"type"`
		}
		if err := ownerWS.ReadJSON(&ack); err != nil || ack.Type != "ack" {
			t.Fatalf("owner absent send ack: %+v", ack)
		}
	}

	// 11. Owner re-invites member back into group
	reinviteRes := request(t, h, http.MethodPost, base+"/members", ownerToken, map[string]any{"user_id": memberID})
	if reinviteRes.Code != http.StatusOK {
		t.Fatalf("re-invite member: %d %s", reinviteRes.Code, reinviteRes.Body.String())
	}

	// 12. Re-invited member must have unread count = 0 (cursor advanced to newest message ID 4)
	unreadAfterReinvite := request(t, h, http.MethodGet, base+"/unread", memberToken, nil)
	if unreadAfterReinvite.Code != http.StatusOK || !bytes.Contains(unreadAfterReinvite.Body.Bytes(), []byte(`"unread_count":0`)) {
		t.Fatalf("re-invited member unread count must be 0, got: %s", unreadAfterReinvite.Body.String())
	}

	// 13. Re-invited member can again read history (as current member)
	histAfterReinvite := request(t, h, http.MethodGet, base+"/messages", memberToken, nil)
	if histAfterReinvite.Code != http.StatusOK {
		t.Fatalf("re-invited member history expected 200, got %d", histAfterReinvite.Code)
	}
	var reHist struct {
		Items []GroupMessage `json:"items"`
	}
	if err := json.Unmarshal(histAfterReinvite.Body.Bytes(), &reHist); err != nil || len(reHist.Items) != 4 {
		t.Fatalf("re-invited member history count expected 4, got %d", len(reHist.Items))
	}

	_ = db
	_ = ownerID
}
