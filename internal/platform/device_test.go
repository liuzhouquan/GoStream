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

	"github.com/gorilla/websocket"
)

const testDeviceA = "00000000-0000-4000-8000-000000000001"
const testDeviceB = "00000000-0000-4000-8000-000000000002"

func requestOnDevice(t *testing.T, h http.Handler, method, path, token, deviceID string, body any) *httptest.ResponseRecorder {
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
	req.Header.Set("Authorization", "Bearer "+token)
	if deviceID != "" {
		req.Header.Set(deviceIDHeader, deviceID)
	}
	result := httptest.NewRecorder()
	h.ServeHTTP(result, req)
	return result
}

func connectDeviceSocket(t *testing.T, serverURL, token, deviceID string) *websocket.Conn {
	t.Helper()
	protocols := []string{"gostream", "gostream.jwt." + token}
	if deviceID != "" {
		protocols = append(protocols, deviceProtocolPrefix+deviceID)
	}
	conn, _, err := (&websocket.Dialer{Subprotocols: protocols}).Dial("ws"+strings.TrimPrefix(serverURL, "http")+"/api/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ready struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&ready); err != nil || ready.Type != "ready" {
		t.Fatalf("socket ready: %+v %v", ready, err)
	}
	return conn
}

func TestDeviceIDValidation(t *testing.T) {
	if normalized, ok := normalizeDeviceID(strings.ToUpper(testDeviceA)); !ok || normalized != testDeviceA {
		t.Fatalf("normalize device ID: %q %v", normalized, ok)
	}
	for _, id := range []string{"", "short", "00000000-0000-4000-8000-00000000000z", "00000000000040008000000000000001"} {
		if _, ok := normalizeDeviceID(id); ok {
			t.Fatalf("accepted invalid device ID %q", id)
		}
	}
}

func TestMessageDeviceLimitAndIdleEviction(t *testing.T) {
	_, h, db := testServer(t)
	userID, token := registerTestUser(t, h, "devicelimit")
	for i := 1; i <= maxMessageDevices; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012x", i)
		result := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, id, nil)
		if result.Code != http.StatusOK {
			t.Fatalf("register device %d: %d %s", i, result.Code, result.Body.String())
		}
	}
	newID := fmt.Sprintf("00000000-0000-4000-8000-%012x", maxMessageDevices+1)
	if result := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, newID, nil); result.Code != http.StatusTooManyRequests {
		t.Fatalf("device cap: %d %s", result.Code, result.Body.String())
	}
	web := httptest.NewServer(h)
	defer web.Close()
	protocols := []string{"gostream", "gostream.jwt." + token, deviceProtocolPrefix + newID}
	conn, response, err := (&websocket.Dialer{Subprotocols: protocols}).Dial("ws"+strings.TrimPrefix(web.URL, "http")+"/api/ws", nil)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("WebSocket device cap: response=%v err=%v", response, err)
	}
	firstID := fmt.Sprintf("00000000-0000-4000-8000-%012x", 1)
	old := time.Now().UTC().Add(-deviceEvictionAge - time.Hour)
	if err := db.Model(&MessageDevice{}).Where("user_id = ? AND device_id = ?", userID, firstID).UpdateColumn("last_seen_at", old).Error; err != nil {
		t.Fatal(err)
	}
	if result := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, newID, nil); result.Code != http.StatusOK {
		t.Fatalf("evict idle device: %d %s", result.Code, result.Body.String())
	}
	var count int64
	if err := db.Model(&MessageDevice{}).Where("user_id = ?", userID).Count(&count).Error; err != nil || count != maxMessageDevices {
		t.Fatalf("device count after eviction: %d %v", count, err)
	}
	if err := db.Model(&MessageDevice{}).Where("user_id = ? AND device_id = ?", userID, firstID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("oldest device remained: %d %v", count, err)
	}
}

func TestExpiredMessageDeviceStateCanReplay(t *testing.T) {
	s, h, db := testServer(t)
	userID, token := registerTestUser(t, h, "deviceexpiry")
	senderID, _ := registerTestUser(t, h, "deviceexpirysender")
	first := ChatMessage{SenderID: senderID, RecipientID: userID, ClientID: "expiry-first", Body: "previously read"}
	second := ChatMessage{SenderID: senderID, RecipientID: userID, ClientID: "expiry-second", Body: "still unread"}
	if err := db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{testDeviceA, testDeviceB} {
		if result := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, id, nil); result.Code != http.StatusOK {
			t.Fatalf("register %s: %d %s", id, result.Code, result.Body.String())
		}
		if result := requestOnDevice(t, h, http.MethodPost, "/api/messages/acks", token, id, map[string]any{"ids": []uint{first.ID}}); result.Code != http.StatusOK {
			t.Fatalf("ack %s: %d %s", id, result.Code, result.Body.String())
		}
	}
	group := ChatGroup{OwnerID: senderID, Name: "expiry group"}
	if err := db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&GroupRead{GroupID: group.ID, UserID: userID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&GroupDeviceRead{GroupID: group.ID, UserID: userID, DeviceID: testDeviceA, LastAckID: 42}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&GroupDeviceRead{GroupID: group.ID, UserID: userID, DeviceID: testDeviceB, LastAckID: 41}).Error; err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-deviceExpirationAge - time.Hour)
	if err := db.Model(&MessageDevice{}).Where("user_id = ? AND device_id = ?", userID, testDeviceA).UpdateColumn("last_seen_at", old).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := s.cleanupExpiredMessageDevices(context.Background(), time.Now(), 100)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup expired device: removed=%d err=%v", removed, err)
	}
	for _, model := range []any{&MessageDevice{}, &MessageDeviceAck{}, &GroupDeviceRead{}} {
		var count int64
		if err := db.Model(model).Where("user_id = ? AND device_id = ?", userID, testDeviceA).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("expired device state %T: count=%d err=%v", model, count, err)
		}
		if err := db.Model(model).Where("user_id = ? AND device_id = ?", userID, testDeviceB).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("active device state %T: count=%d err=%v", model, count, err)
		}
	}
	var page struct {
		Items []ChatMessage `json:"items"`
	}
	result := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, testDeviceA, nil)
	if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.Items[0].ID != first.ID || page.Items[1].ID != second.ID {
		t.Fatalf("expired device replay: %d %s", result.Code, result.Body.String())
	}
	result = requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, testDeviceB, nil)
	if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != second.ID {
		t.Fatalf("active device unread: %d %s", result.Code, result.Body.String())
	}
}

func TestMessageDeviceBackfillPreservesExistingReadState(t *testing.T) {
	s, h, db := testServer(t)
	userID, token := registerTestUser(t, h, "devicebackfill")
	senderID, _ := registerTestUser(t, h, "devicebackfillsender")
	message := ChatMessage{SenderID: senderID, RecipientID: userID, ClientID: "backfill-message", Body: "already read"}
	if err := db.Create(&message).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&MessageDeviceAck{UserID: userID, DeviceID: testDeviceA, MessageID: message.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&GroupDeviceRead{GroupID: 1, UserID: userID, DeviceID: testDeviceB, LastAckID: 7}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.BackfillMessageDevices(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Model(&MessageDevice{}).Where("user_id = ?", userID).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("backfilled device count: %d %v", count, err)
	}
	result := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", token, testDeviceA, nil)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"items":[]`) {
		t.Fatalf("read state after backfill: %d %s", result.Code, result.Body.String())
	}
}

func TestDirectDeviceAckIsolationAndReplay(t *testing.T) {
	_, h, db := testServer(t)
	alice, aliceToken := registerTestUser(t, h, "devicealice")
	bob, _ := registerTestUser(t, h, "devicebob")
	charlie, _ := registerTestUser(t, h, "devicecharlie")
	created := make([]ChatMessage, 3)
	for i, recipient := range []uint{alice, alice, charlie} {
		created[i] = ChatMessage{SenderID: bob, RecipientID: recipient, ClientID: fmt.Sprintf("device-direct-%d", i), Body: fmt.Sprintf("hello %d", i)}
		if err := db.Create(&created[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	unread := func(deviceID string, expected []uint) {
		t.Helper()
		result := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", aliceToken, deviceID, nil)
		if result.Code != http.StatusOK {
			t.Fatalf("unread device %q: %d %s", deviceID, result.Code, result.Body.String())
		}
		var page struct {
			Items []ChatMessage `json:"items"`
		}
		if err := json.Unmarshal(result.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != len(expected) {
			t.Fatalf("device %q unread IDs: %+v, want %v", deviceID, page.Items, expected)
		}
		for i := range expected {
			if page.Items[i].ID != expected[i] {
				t.Fatalf("device %q unread IDs: %+v, want %v", deviceID, page.Items, expected)
			}
		}
	}
	unread(testDeviceA, []uint{created[0].ID, created[1].ID})
	unread(testDeviceB, []uint{created[0].ID, created[1].ID})
	for _, body := range []map[string]any{
		{"ids": []uint{created[0].ID, created[2].ID}},
		{"ids": []uint{created[0].ID, created[0].ID}},
	} {
		result := requestOnDevice(t, h, http.MethodPost, "/api/messages/acks", aliceToken, testDeviceA, body)
		if result.Code != http.StatusNotFound && result.Code != http.StatusBadRequest {
			t.Fatalf("invalid device batch: %d %s", result.Code, result.Body.String())
		}
	}
	unread(testDeviceA, []uint{created[0].ID, created[1].ID})
	batch := map[string]any{"ids": []uint{created[0].ID}}
	for attempt, expected := range []string{`"acked_count":1`, `"acked_count":0`} {
		result := requestOnDevice(t, h, http.MethodPost, "/api/messages/acks", aliceToken, testDeviceA, batch)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), expected) {
			t.Fatalf("device ACK attempt %d: %d %s", attempt, result.Code, result.Body.String())
		}
	}
	unread(testDeviceA, []uint{created[1].ID})
	unread(testDeviceB, []uint{created[0].ID, created[1].ID})
	unread("", []uint{created[0].ID, created[1].ID})
	var globallyAcked int64
	if err := db.Model(&ChatMessage{}).Where("id IN ? AND acked_at IS NOT NULL", []uint{created[0].ID, created[1].ID}).Count(&globallyAcked).Error; err != nil || globallyAcked != 0 {
		t.Fatalf("device ACK changed global state: %d %v", globallyAcked, err)
	}
	if response := requestOnDevice(t, h, http.MethodGet, "/api/messages/unread", aliceToken, "invalid", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid device header: %d", response.Code)
	}

	server := httptest.NewServer(h)
	defer server.Close()
	invalidDialer := websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + aliceToken, deviceProtocolPrefix + "invalid"}}
	if conn, response, err := invalidDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/ws", nil); err == nil || response == nil || response.StatusCode != http.StatusBadRequest {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("invalid WebSocket device ID: response=%v err=%v", response, err)
	}
	first := connectDeviceSocket(t, server.URL, aliceToken, testDeviceA)
	var message struct {
		Type    string      `json:"type"`
		Message ChatMessage `json:"message"`
	}
	if err := first.ReadJSON(&message); err != nil || message.Type != "message" || message.Message.ID != created[1].ID {
		t.Fatalf("device A replay: %+v %v", message, err)
	}
	second := connectDeviceSocket(t, server.URL, aliceToken, testDeviceB)
	for _, expected := range []uint{created[0].ID, created[1].ID} {
		if err := second.ReadJSON(&message); err != nil || message.Type != "message" || message.Message.ID != expected {
			t.Fatalf("device B replay %d: %+v %v", expected, message, err)
		}
	}
	if result := requestOnDevice(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", created[1].ID), aliceToken, testDeviceA, nil); result.Code != http.StatusOK {
		t.Fatalf("single device ACK: %d %s", result.Code, result.Body.String())
	}
	unread(testDeviceA, nil)
	unread(testDeviceB, []uint{created[0].ID, created[1].ID})
	if result := request(t, h, http.MethodPost, fmt.Sprintf("/api/messages/%d/ack", created[0].ID), aliceToken, nil); result.Code != http.StatusOK {
		t.Fatalf("legacy direct ACK: %d %s", result.Code, result.Body.String())
	}
	unread(testDeviceB, []uint{created[1].ID})
	unread("", []uint{created[1].ID})
}

func TestGroupDeviceReadIsolationAndReplay(t *testing.T) {
	_, h, db := testServer(t)
	owner, ownerToken := registerTestUser(t, h, "groupdeviceowner")
	member, memberToken := registerTestUser(t, h, "groupdevicemember")
	created := request(t, h, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "Device group"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", created.Code, created.Body.String())
	}
	var group ChatGroup
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/groups/%d/members", group.ID)
	if result := request(t, h, http.MethodPost, path, ownerToken, map[string]uint{"user_id": member}); result.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", result.Code, result.Body.String())
	}
	ids := make([]uint, 2)
	for i := range ids {
		msg := GroupMessage{GroupID: group.ID, SenderID: owner, ClientID: fmt.Sprintf("group-device-%d", i), Body: "hello"}
		if err := db.Create(&msg).Error; err != nil {
			t.Fatal(err)
		}
		ids[i] = msg.ID
	}
	unread := func(deviceID string, wantCount, wantCursor uint) {
		t.Helper()
		result := requestOnDevice(t, h, http.MethodGet, fmt.Sprintf("/api/groups/%d/unread", group.ID), memberToken, deviceID, nil)
		if result.Code != http.StatusOK {
			t.Fatalf("group unread: %d %s", result.Code, result.Body.String())
		}
		var value struct {
			Count  uint `json:"unread_count"`
			Cursor uint `json:"last_ack_id"`
		}
		if err := json.Unmarshal(result.Body.Bytes(), &value); err != nil || value.Count != wantCount || value.Cursor != wantCursor {
			t.Fatalf("group device %q unread: %+v err %v, want %d/%d", deviceID, value, err, wantCount, wantCursor)
		}
	}
	unread(testDeviceA, 2, 0)
	unread(testDeviceB, 2, 0)
	ackPath := fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, ids[0])
	if result := requestOnDevice(t, h, http.MethodPost, ackPath, memberToken, testDeviceA, nil); result.Code != http.StatusOK {
		t.Fatalf("device group ACK: %d %s", result.Code, result.Body.String())
	}
	unread(testDeviceA, 1, ids[0])
	unread(testDeviceB, 2, 0)
	unread("", 2, 0)

	server := httptest.NewServer(h)
	defer server.Close()
	first := connectDeviceSocket(t, server.URL, memberToken, testDeviceA)
	var delivered struct {
		Type    string       `json:"type"`
		Message GroupMessage `json:"message"`
	}
	if err := first.ReadJSON(&delivered); err != nil || delivered.Type != "group_message" || delivered.Message.ID != ids[1] {
		t.Fatalf("device A group replay: %+v %v", delivered, err)
	}
	second := connectDeviceSocket(t, server.URL, memberToken, testDeviceB)
	for _, expected := range ids {
		if err := second.ReadJSON(&delivered); err != nil || delivered.Type != "group_message" || delivered.Message.ID != expected {
			t.Fatalf("device B group replay %d: %+v %v", expected, delivered, err)
		}
	}
	newerPath := fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, ids[1])
	if result := requestOnDevice(t, h, http.MethodPost, newerPath, memberToken, testDeviceA, nil); result.Code != http.StatusOK {
		t.Fatalf("newer group ACK: %d %s", result.Code, result.Body.String())
	}
	if result := requestOnDevice(t, h, http.MethodPost, ackPath, memberToken, testDeviceA, nil); result.Code != http.StatusOK {
		t.Fatalf("older group ACK: %d %s", result.Code, result.Body.String())
	}
	unread(testDeviceA, 0, ids[1])
	unread(testDeviceB, 2, 0)
	if result := request(t, h, http.MethodDelete, fmt.Sprintf("/api/groups/%d/members/%d", group.ID, member), ownerToken, nil); result.Code != http.StatusOK {
		t.Fatalf("remove group member: %d %s", result.Code, result.Body.String())
	}
	var remaining int64
	if err := db.Model(&GroupDeviceRead{}).Where("group_id = ? AND user_id = ?", group.ID, member).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("removed membership retained device cursor: %d %v", remaining, err)
	}
	if result := request(t, h, http.MethodPost, path, ownerToken, map[string]uint{"user_id": member}); result.Code != http.StatusOK {
		t.Fatalf("re-add group member: %d %s", result.Code, result.Body.String())
	}
	unread(testDeviceA, 0, ids[1])
	unread(testDeviceB, 0, ids[1])
}
