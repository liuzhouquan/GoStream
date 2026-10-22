package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketMultipleDevicesReceiveCommittedEvents(t *testing.T) {
	s, h, db := testServer(t)
	ownerID, ownerToken := registerTestUser(t, h, "multideviceowner")
	memberID, memberToken := registerTestUser(t, h, "multidevicemember")
	created := request(t, h, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "Multiple devices"})
	var group ChatGroup
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &group) != nil {
		t.Fatalf("create group: %d %s", created.Code, created.Body.String())
	}
	if added := request(t, h, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", group.ID), ownerToken, map[string]uint{"user_id": memberID}); added.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", added.Code, added.Body.String())
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	connect := func(token string) *websocket.Conn {
		t.Helper()
		conn, _, err := (&websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}).Dial(wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		var ready struct {
			Type string `json:"type"`
		}
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := conn.ReadJSON(&ready); err != nil || ready.Type != "ready" {
			t.Fatalf("ready: %+v %v", ready, err)
		}
		return conn
	}
	type directEvent struct {
		Type      string
		MessageID uint
		Message   ChatMessage
	}
	read := func(conn *websocket.Conn, wantType string) directEvent {
		t.Helper()
		var wire struct {
			Type      string          `json:"type"`
			MessageID uint            `json:"message_id"`
			Message   json.RawMessage `json:"message"`
		}
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := conn.ReadJSON(&wire); err != nil || wire.Type != wantType {
			t.Fatalf("event type %q: %+v %v", wantType, wire, err)
		}
		event := directEvent{Type: wire.Type, MessageID: wire.MessageID}
		if wire.Type == "message" {
			if err := json.Unmarshal(wire.Message, &event.Message); err != nil {
				t.Fatal(err)
			}
		}
		return event
	}
	ownerA := connect(ownerToken)
	ownerB := connect(ownerToken)
	memberA := connect(memberToken)
	memberB := connect(memberToken)
	if s.Hub.count() != 4 || !s.Hub.online(ownerID) || !s.Hub.online(memberID) {
		t.Fatalf("initial connections=%d", s.Hub.count())
	}
	if err := ownerA.WriteJSON(socketEvent{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	read(ownerA, "pong")
	if err := ownerA.WriteJSON(socketEvent{Type: "message", ClientID: "multi-invalid", RecipientID: ownerID, Body: "self"}); err != nil {
		t.Fatal(err)
	}
	read(ownerA, "error")
	if err := ownerA.WriteJSON(socketEvent{Type: "message", ClientID: "multi-direct-1", RecipientID: memberID, Body: "first"}); err != nil {
		t.Fatal(err)
	}
	ack := read(ownerA, "ack")
	echo := read(ownerB, "message")
	firstA := read(memberA, "message")
	firstB := read(memberB, "message")
	if ack.MessageID == 0 || echo.Message.ID != ack.MessageID || firstA.Message.ID != ack.MessageID || firstB.Message.ID != ack.MessageID {
		t.Fatalf("direct fanout ids: ack=%d echo=%d memberA=%d memberB=%d", ack.MessageID, echo.Message.ID, firstA.Message.ID, firstB.Message.ID)
	}
	memberC := connect(memberToken)
	if replay := read(memberC, "message"); replay.Message.ID != ack.MessageID {
		t.Fatalf("new device replayed message %d, want %d", replay.Message.ID, ack.MessageID)
	}
	_ = memberA.Close()
	for until := time.Now().Add(2 * time.Second); s.Hub.count() != 4 && time.Now().Before(until); {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Hub.count() != 4 || !s.Hub.online(memberID) {
		t.Fatalf("one device disconnected: connections=%d online=%t", s.Hub.count(), s.Hub.online(memberID))
	}
	var member User
	if err := db.First(&member, memberID).Error; err != nil || member.LastSeenAt != nil {
		t.Fatalf("last-seen changed while another device is online: %+v %v", member.LastSeenAt, err)
	}
	if err := ownerA.WriteJSON(socketEvent{Type: "message", ClientID: "multi-direct-2", RecipientID: memberID, Body: "second"}); err != nil {
		t.Fatal(err)
	}
	ack2 := read(ownerA, "ack")
	if ack2.MessageID == ack.MessageID || read(ownerB, "message").Message.ID != ack2.MessageID || read(memberB, "message").Message.ID != ack2.MessageID || read(memberC, "message").Message.ID != ack2.MessageID {
		t.Fatal("second message did not reach each remaining device exactly once")
	}
	if err := ownerA.WriteJSON(socketEvent{Type: "group_message", ClientID: "multi-group-1", GroupID: group.ID, Body: "group hello"}); err != nil {
		t.Fatal(err)
	}
	groupAck := read(ownerA, "ack")
	if groupAck.MessageID == 0 {
		t.Fatal("missing group ACK")
	}
	for _, conn := range []*websocket.Conn{ownerB, memberB, memberC} {
		var delivery struct {
			Type    string       `json:"type"`
			Message GroupMessage `json:"message"`
		}
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := conn.ReadJSON(&delivery); err != nil || delivery.Type != "group_message" || delivery.Message.ID != groupAck.MessageID {
			t.Fatalf("group fanout: %+v %v", delivery, err)
		}
	}
	_ = memberB.Close()
	_ = memberC.Close()
	for until := time.Now().Add(2 * time.Second); s.Hub.online(memberID) && time.Now().Before(until); {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Hub.online(memberID) || s.Hub.count() != 2 {
		t.Fatalf("all member devices closed: count=%d online=%t", s.Hub.count(), s.Hub.online(memberID))
	}
	if err := db.First(&member, memberID).Error; err != nil || member.LastSeenAt == nil {
		t.Fatalf("last-seen missing after final disconnect: %+v %v", member.LastSeenAt, err)
	}
}

func TestWebSocketPerUserConnectionLimit(t *testing.T) {
	s, h, _ := testServer(t)
	_, token := registerTestUser(t, h, "socketlimituser")
	ts := httptest.NewServer(h)
	defer ts.Close()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	dialer := websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}
	connections := make([]*websocket.Conn, 0, maxConnectionsPerUser)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for i := 0; i < maxConnectionsPerUser; i++ {
		conn, _, err := dialer.Dial(u, nil)
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		connections = append(connections, conn)
		var ready map[string]any
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
			t.Fatalf("ready %d: %+v %v", i, ready, err)
		}
	}
	if s.Hub.count() != maxConnectionsPerUser {
		t.Fatalf("connection count=%d", s.Hub.count())
	}
	if conn, response, err := dialer.Dial(u, nil); err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("ninth connection: response=%v err=%v", response, err)
	}
	_ = connections[0].Close()
	for until := time.Now().Add(2 * time.Second); s.Hub.count() != maxConnectionsPerUser-1 && time.Now().Before(until); {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Hub.count() != maxConnectionsPerUser-1 {
		t.Fatalf("closed connection not released: count=%d", s.Hub.count())
	}
	conn, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect after close: %v", err)
	}
	connections = append(connections, conn)
	var ready map[string]any
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
		t.Fatalf("reconnect ready: %+v %v", ready, err)
	}
}
