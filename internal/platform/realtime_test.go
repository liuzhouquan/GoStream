package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

func TestRedisRealtimeCrossInstanceDelivery(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer redisServer.Close()
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), MaxRetries: 0})
	defer client.Close()
	senderServer, senderRouter, db := testServer(t)
	recipientServer := NewServer(db, "test-signing-secret-at-least-32-bytes", t.TempDir())
	recipientRouter := recipientServer.Router()
	if err := senderServer.UseRedisRealtime(client); err != nil {
		t.Fatal(err)
	}
	if err := recipientServer.UseRedisRealtime(client); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		recipientServer.runRealtimeBridge(ctx, ready)
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("Redis subscriber did not become ready")
	}
	defer func() { cancel(); <-done }()
	ownerID, ownerToken := registerTestUser(t, senderRouter, "bridgeowner")
	memberID, memberToken := registerTestUser(t, senderRouter, "bridgemember")
	first := httptest.NewServer(senderRouter)
	defer first.Close()
	second := httptest.NewServer(recipientRouter)
	defer second.Close()
	connect := func(base, token string) *websocket.Conn {
		u := "ws" + strings.TrimPrefix(base, "http") + "/api/ws"
		conn, _, err := (&websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}).Dial(u, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ready map[string]any
		if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
			t.Fatalf("WebSocket ready: %v %v", ready, err)
		}
		return conn
	}
	owner := connect(first.URL, ownerToken)
	member := connect(second.URL, memberToken)
	if err := owner.WriteJSON(socketEvent{Type: "message", RecipientID: memberID, ClientID: "cross-direct", Body: "remote hello"}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := owner.ReadJSON(&ack); err != nil || ack["type"] != "ack" {
		t.Fatalf("sender ACK: %v %v", ack, err)
	}
	var delivery struct {
		Type    string      `json:"type"`
		Message ChatMessage `json:"message"`
	}
	if err := member.ReadJSON(&delivery); err != nil || delivery.Type != "message" || delivery.Message.Body != "remote hello" {
		t.Fatalf("cross-instance direct delivery: %+v %v", delivery, err)
	}
	created := request(t, senderRouter, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "bridged group"})
	var group ChatGroup
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &group) != nil {
		t.Fatalf("group creation: %d %s", created.Code, created.Body.String())
	}
	if r := request(t, senderRouter, http.MethodPost, fmt.Sprintf("/api/groups/%d/members", group.ID), ownerToken, map[string]uint{"user_id": memberID}); r.Code != http.StatusOK {
		t.Fatalf("group invite: %d %s", r.Code, r.Body.String())
	}
	if err := owner.WriteJSON(socketEvent{Type: "group_message", GroupID: group.ID, ClientID: "cross-group", Body: "remote group"}); err != nil {
		t.Fatal(err)
	}
	if err := owner.ReadJSON(&ack); err != nil || ack["type"] != "ack" {
		t.Fatalf("group ACK: %v %v", ack, err)
	}
	var groupDelivery struct {
		Type    string       `json:"type"`
		Message GroupMessage `json:"message"`
	}
	if err := member.ReadJSON(&groupDelivery); err != nil || groupDelivery.Type != "group_message" || groupDelivery.Message.Body != "remote group" {
		t.Fatalf("cross-instance group delivery: %+v %v", groupDelivery, err)
	}
	if r := request(t, senderRouter, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", memberID), ownerToken, nil); r.Code != http.StatusOK {
		t.Fatalf("follow: %d %s", r.Code, r.Body.String())
	}
	if processed, err := senderServer.ProcessOutboxOnce(ctx); err != nil || processed < 1 {
		t.Fatalf("outbox dispatch: processed=%d err=%v", processed, err)
	}
	var notice struct {
		Type         string       `json:"type"`
		Notification Notification `json:"notification"`
	}
	foundFollow := false
	for i := 0; i < 4; i++ {
		if err := member.ReadJSON(&notice); err != nil {
			t.Fatalf("cross-instance notification: %+v %v", notice, err)
		}
		if notice.Type == "notification" && notice.Notification.Kind == "follow" && notice.Notification.ActorID == ownerID {
			foundFollow = true
			break
		}
	}
	if !foundFollow {
		t.Fatalf("follow notification missing across instances: %+v", notice)
	}
	presencePath := fmt.Sprintf("/api/users/%d/presence", memberID)
	if r := request(t, senderRouter, http.MethodGet, presencePath, "", nil); r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"online":true`)) {
		t.Fatalf("remote online presence: %d %s", r.Code, r.Body.String())
	}
	_ = member.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r := request(t, senderRouter, http.MethodGet, presencePath, "", nil)
		if r.Code == http.StatusOK && bytes.Contains(r.Body.Bytes(), []byte(`"online":false`)) {
			_ = owner.Close()
			for until := time.Now().Add(2 * time.Second); senderServer.Hub.online(ownerID) && time.Now().Before(until); {
				time.Sleep(10 * time.Millisecond)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("remote presence did not clear after disconnect")
}
