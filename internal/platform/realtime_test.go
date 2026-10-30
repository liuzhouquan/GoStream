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
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), MaxRetries: 0})
	t.Cleanup(func() { _ = client.Close() })
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
	ownerOtherDevice := connect(second.URL, ownerToken)
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
	if err := ownerOtherDevice.ReadJSON(&delivery); err != nil || delivery.Type != "message" || delivery.Message.Body != "remote hello" || delivery.Message.SenderID != ownerID {
		t.Fatalf("cross-instance sender-device echo: %+v %v", delivery, err)
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
	if err := ownerOtherDevice.ReadJSON(&groupDelivery); err != nil || groupDelivery.Type != "group_message" || groupDelivery.Message.Body != "remote group" || groupDelivery.Message.SenderID != ownerID {
		t.Fatalf("cross-instance sender group echo: %+v %v", groupDelivery, err)
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

func TestRealtimeDirectEventStillReachesDeviceAfterUserACK(t *testing.T) {
	s, h, db := testServer(t)
	senderID, _ := registerTestUser(t, h, "ackedbridgesender")
	recipientID, recipientToken := registerTestUser(t, h, "ackedbridgerecipient")
	ts := httptest.NewServer(h)
	defer ts.Close()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
	conn, _, err := (&websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + recipientToken}}).Dial(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var ready map[string]any
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
		t.Fatalf("ready: %+v %v", ready, err)
	}
	ackedAt := time.Now().UTC()
	message := ChatMessage{ClientID: "acked-before-bridge", SenderID: senderID, RecipientID: recipientID, Body: "still deliver", AckedAt: &ackedAt}
	if err := db.Create(&message).Error; err != nil {
		t.Fatal(err)
	}
	s.deliverRealtime(realtimeEvent{Kind: "direct", UserID: recipientID, SenderID: senderID, MessageID: message.ID})
	var event struct {
		Type    string      `json:"type"`
		Message ChatMessage `json:"message"`
	}
	if err := conn.ReadJSON(&event); err != nil || event.Type != "message" || event.Message.ID != message.ID {
		t.Fatalf("delivery after another device ACK: %+v %v", event, err)
	}
}

func TestRedisCrashedNodePresenceExpiry(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), MaxRetries: 0})
	t.Cleanup(func() { _ = client.Close() })

	s1, _, _ := testServer(t)
	if err := s1.UseRedisRealtime(client); err != nil {
		t.Fatal(err)
	}
	s2, _, _ := testServer(t)
	if err := s2.UseRedisRealtime(client); err != nil {
		t.Fatal(err)
	}

	// Configure short 80ms lease for test speed
	s1.SetPresenceLease(80*time.Millisecond, 25*time.Millisecond)

	userID := uint(42)
	ctx := context.Background()

	// 1. Initially offline
	if s2.onlineAcrossInstances(ctx, userID) {
		t.Fatal("expected offline initially")
	}

	// 2. Refreshed presence on node 1 reports online on node 2
	s1.refreshPresence(ctx, userID)
	if !s2.onlineAcrossInstances(ctx, userID) {
		t.Fatal("expected online across instances immediately after refresh")
	}

	// 3. Simulate node 1 crash: no clean disconnect (no clearPresence), renewal stopped.
	// Wait past the 80ms lease duration so time.Now() advances past the stored lease score.
	time.Sleep(100 * time.Millisecond)

	// 4. Node 2 observes crashed node presence expiry
	if s2.onlineAcrossInstances(ctx, userID) {
		t.Fatal("expected user to be reported offline after presence lease expired")
	}

	// 5. Node 1 comes back up and refreshes
	s1.refreshPresence(ctx, userID)
	if !s2.onlineAcrossInstances(ctx, userID) {
		t.Fatal("expected online after node recovery")
	}
}
