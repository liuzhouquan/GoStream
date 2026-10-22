package platform

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Hub struct {
	mu      sync.RWMutex
	clients map[uint]map[*hubClient]struct{}
}

const maxConnectionsPerUser = 8

type hubClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func NewHub() *Hub { return &Hub{clients: map[uint]map[*hubClient]struct{}{}} }

func (h *Hub) add(id uint, c *websocket.Conn) *hubClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients[id]) >= maxConnectionsPerUser {
		return nil
	}
	if h.clients[id] == nil {
		h.clients[id] = make(map[*hubClient]struct{})
	}
	client := &hubClient{conn: c}
	h.clients[id][client] = struct{}{}
	return client
}

func (h *Hub) hasCapacity(id uint) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients[id]) < maxConnectionsPerUser
}

// remove reports whether the user has no local connections left.
func (h *Hub) remove(id uint, client *hubClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients[id], client)
	if len(h.clients[id]) == 0 {
		delete(h.clients, id)
		return true
	}
	return false
}

func (h *Hub) online(id uint) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients[id]) > 0
}

func (h *Hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	count := 0
	for _, clients := range h.clients {
		count += len(clients)
	}
	return count
}

func (h *Hub) onlineIDs() []uint {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]uint, 0, len(h.clients))
	for id := range h.clients {
		ids = append(ids, id)
	}
	return ids
}

func (client *hubClient) send(v any) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	_ = client.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := client.conn.WriteJSON(v); err != nil {
		_ = client.conn.Close()
		return err
	}
	return nil
}

func (h *Hub) send(id uint, v any) error { return h.sendExcept(id, nil, v) }

// sendExcept broadcasts a committed event to the user's other local devices.
func (h *Hub) sendExcept(id uint, excluded *hubClient, v any) error {
	h.mu.RLock()
	clients := make([]*hubClient, 0, len(h.clients[id]))
	for client := range h.clients[id] {
		if client != excluded {
			clients = append(clients, client)
		}
	}
	h.mu.RUnlock()
	var firstErr error
	for _, client := range clients {
		if err := client.send(v); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type socketEvent struct {
	Type        string `json:"type"`
	ClientID    string `json:"client_id"`
	RecipientID uint   `json:"recipient_id"`
	GroupID     uint   `json:"group_id"`
	MessageID   uint   `json:"message_id,omitempty"`
	Body        string `json:"body,omitempty"`
}

func (s *Server) websocket(c *gin.Context) {
	var raw, deviceID string
	for _, protocol := range websocket.Subprotocols(c.Request) {
		if strings.HasPrefix(protocol, "gostream.jwt.") {
			raw = strings.TrimPrefix(protocol, "gostream.jwt.")
		} else if strings.HasPrefix(protocol, deviceProtocolPrefix) {
			var ok bool
			deviceID, ok = normalizeDeviceID(strings.TrimPrefix(protocol, deviceProtocolPrefix))
			if !ok {
				respondError(c, http.StatusBadRequest, "invalid device ID")
				return
			}
		}
	}
	userID, err := s.userFromToken(raw)
	if err != nil {
		respondError(c, http.StatusUnauthorized, "valid WebSocket token protocol required")
		return
	}
	if !s.Hub.hasCapacity(userID) {
		respondError(c, http.StatusTooManyRequests, "too many WebSocket connections")
		return
	}
	conn, err := s.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	client := s.Hub.add(userID, conn)
	if client == nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "too many connections"), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	s.refreshPresence(context.Background(), userID)
	presenceCtx, stopPresence := context.WithCancel(context.Background())
	if s.realtime != nil {
		go s.renewPresence(presenceCtx, userID)
	}
	s.metrics.wsConnections.Set(float64(s.Hub.count()))
	defer func() {
		stopPresence()
		if s.Hub.remove(userID, client) {
			s.clearPresence(userID)
			_ = s.DB.Model(&User{}).Where("id = ?", userID).Update("last_seen_at", time.Now().UTC()).Error
		}
		s.metrics.wsConnections.Set(float64(s.Hub.count()))
		_ = conn.Close()
	}()
	conn.SetReadLimit(8192)
	_ = conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(70 * time.Second)) })
	_ = client.send(gin.H{"type": "ready"})
	s.replayPending(userID, deviceID, client)
	s.replayPendingGroups(userID, deviceID, client)
	for {
		var e socketEvent
		if err = conn.ReadJSON(&e); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		switch e.Type {
		case "ping":
			_ = client.send(gin.H{"type": "pong"})
		case "message":
			if e.RecipientID == 0 || e.RecipientID == userID || len(strings.TrimSpace(e.Body)) == 0 || len(e.Body) > 4000 || e.ClientID == "" || len(e.ClientID) > 80 {
				_ = client.send(gin.H{"type": "error", "message": "invalid message"})
				continue
			}
			var recipient User
			if err = s.DB.Select("id").First(&recipient, e.RecipientID).Error; err != nil {
				_ = client.send(gin.H{"type": "error", "message": "recipient not found"})
				continue
			}
			msg := ChatMessage{SenderID: userID, RecipientID: e.RecipientID, ClientID: e.ClientID, Body: e.Body}
			created, conflict := false, false
			err = s.DB.Transaction(func(tx *gorm.DB) error {
				res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&msg)
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 0 {
					if err := tx.Where("sender_id = ? AND client_id = ?", userID, e.ClientID).First(&msg).Error; err != nil {
						return err
					}
					conflict = msg.RecipientID != e.RecipientID || msg.Body != e.Body
					return nil
				}
				created = true
				return enqueueNotification(tx, Notification{
					UserID: e.RecipientID, EventKey: fmt.Sprintf("message:%d", msg.ID), Kind: "message",
					ActorID: userID, Text: "sent you a message",
				})
			})
			if err != nil {
				_ = client.send(gin.H{"type": "error", "message": "message could not be saved"})
				continue
			}
			if conflict {
				_ = client.send(gin.H{"type": "error", "message": "client_id already used for another message"})
				continue
			}
			_ = client.send(gin.H{"type": "ack", "client_id": msg.ClientID, "message_id": msg.ID})
			if created {
				_ = s.Hub.sendExcept(userID, client, gin.H{"type": "message", "message": msg})
				_ = s.Hub.send(msg.RecipientID, gin.H{"type": "message", "message": msg})
				if err := s.publishRealtime(c.Request.Context(), realtimeEvent{Kind: "direct", UserID: msg.RecipientID, SenderID: userID, MessageID: msg.ID}); err != nil {
					log.Printf("realtime direct publish failed for message %d: %v", msg.ID, err)
				}
			}
		case "group_message":
			s.sendGroupMessage(userID, client, e)
		default:
			_ = client.send(gin.H{"type": "error", "message": "unknown event type"})
		}
	}
}

func (s *Server) replayPending(userID uint, deviceID string, client *hubClient) {
	var rows []ChatMessage
	if err := s.pendingDirectMessages(userID, deviceID).Order("chat_messages.id ASC").Limit(101).Find(&rows).Error; err != nil {
		_ = client.send(gin.H{"type": "error", "message": "pending messages could not be loaded"})
		return
	}
	more := len(rows) > 100
	if more {
		rows = rows[:100]
	}
	for _, msg := range rows {
		if client.send(gin.H{"type": "message", "message": msg}) != nil {
			return
		}
	}
	if more {
		_ = client.send(gin.H{"type": "sync_required"})
	}
}

func (s *Server) replayPendingGroups(userID uint, deviceID string, client *hubClient) {
	var rows []GroupMessage
	q := s.DB.Model(&GroupMessage{}).
		Joins("JOIN group_members ON group_members.group_id = group_messages.group_id AND group_members.user_id = ?", userID).
		Joins("JOIN group_reads ON group_reads.group_id = group_messages.group_id AND group_reads.user_id = ?", userID)
	if deviceID != "" {
		q = q.Joins("LEFT JOIN group_device_reads ON group_device_reads.group_id = group_messages.group_id AND group_device_reads.user_id = ? AND group_device_reads.device_id = ?", userID, deviceID).
			Where("group_messages.id > COALESCE(group_device_reads.last_ack_id, group_reads.last_ack_id)").Select("group_messages.*")
	} else {
		q = q.Where("group_messages.id > group_reads.last_ack_id")
	}
	err := q.Where("group_messages.sender_id <> ?", userID).
		Order("group_messages.id ASC").Limit(101).Find(&rows).Error
	if err != nil {
		_ = client.send(gin.H{"type": "error", "message": "pending group messages could not be loaded"})
		return
	}
	more := len(rows) > 100
	if more {
		rows = rows[:100]
	}
	for _, msg := range rows {
		if client.send(gin.H{"type": "group_message", "message": msg}) != nil {
			return
		}
	}
	if more {
		_ = client.send(gin.H{"type": "group_sync_required"})
	}
}
