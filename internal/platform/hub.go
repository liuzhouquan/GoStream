package platform

import (
	"fmt"
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
	clients map[uint]*websocket.Conn
	writeMu map[uint]*sync.Mutex
}

func NewHub() *Hub { return &Hub{clients: map[uint]*websocket.Conn{}, writeMu: map[uint]*sync.Mutex{}} }
func (h *Hub) add(id uint, c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.clients[id]; old != nil {
		_ = old.Close()
	}
	h.clients[id] = c
	h.writeMu[id] = &sync.Mutex{}
}
func (h *Hub) remove(id uint, c *websocket.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[id] == c {
		delete(h.clients, id)
		delete(h.writeMu, id)
		return true
	}
	return false
}
func (h *Hub) online(id uint) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.clients[id] != nil
}
func (h *Hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
func (h *Hub) send(id uint, v any) error {
	h.mu.RLock()
	c, mu := h.clients[id], h.writeMu[id]
	h.mu.RUnlock()
	if c == nil {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.WriteJSON(v)
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
	var raw string
	for _, protocol := range websocket.Subprotocols(c.Request) {
		if strings.HasPrefix(protocol, "gostream.jwt.") {
			raw = strings.TrimPrefix(protocol, "gostream.jwt.")
			break
		}
	}
	userID, err := s.userFromToken(raw)
	if err != nil {
		respondError(c, http.StatusUnauthorized, "valid WebSocket token protocol required")
		return
	}
	conn, err := s.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	s.Hub.add(userID, conn)
	s.metrics.wsConnections.Set(float64(s.Hub.count()))
	defer func() {
		if s.Hub.remove(userID, conn) {
			_ = s.DB.Model(&User{}).Where("id = ?", userID).Update("last_seen_at", time.Now().UTC()).Error
		}
		s.metrics.wsConnections.Set(float64(s.Hub.count()))
		_ = conn.Close()
	}()
	conn.SetReadLimit(8192)
	_ = conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(70 * time.Second)) })
	_ = s.Hub.send(userID, gin.H{"type": "ready"})
	s.replayPending(userID)
	for {
		var e socketEvent
		if err = conn.ReadJSON(&e); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		switch e.Type {
		case "ping":
			_ = s.Hub.send(userID, gin.H{"type": "pong"})
		case "message":
			if e.RecipientID == 0 || e.RecipientID == userID || len(strings.TrimSpace(e.Body)) == 0 || len(e.Body) > 4000 || e.ClientID == "" || len(e.ClientID) > 80 {
				_ = s.Hub.send(userID, gin.H{"type": "error", "message": "invalid message"})
				continue
			}
			var recipient User
			if err = s.DB.Select("id").First(&recipient, e.RecipientID).Error; err != nil {
				_ = s.Hub.send(userID, gin.H{"type": "error", "message": "recipient not found"})
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
				_ = s.Hub.send(userID, gin.H{"type": "error", "message": "message could not be saved"})
				continue
			}
			if conflict {
				_ = s.Hub.send(userID, gin.H{"type": "error", "message": "client_id already used for another message"})
				continue
			}
			_ = s.Hub.send(userID, gin.H{"type": "ack", "client_id": msg.ClientID, "message_id": msg.ID})
			if created {
				_ = s.Hub.send(msg.RecipientID, gin.H{"type": "message", "message": msg})
			}
		case "group_message":
			s.sendGroupMessage(userID, e)
		default:
			_ = s.Hub.send(userID, gin.H{"type": "error", "message": "unknown event type"})
		}
	}
}

func (s *Server) replayPending(userID uint) {
	var rows []ChatMessage
	if err := s.DB.Where("recipient_id = ? AND acked_at IS NULL", userID).Order("id ASC").Limit(101).Find(&rows).Error; err != nil {
		_ = s.Hub.send(userID, gin.H{"type": "error", "message": "pending messages could not be loaded"})
		return
	}
	more := len(rows) > 100
	if more {
		rows = rows[:100]
	}
	for _, msg := range rows {
		if s.Hub.send(userID, gin.H{"type": "message", "message": msg}) != nil {
			return
		}
	}
	if more {
		_ = s.Hub.send(userID, gin.H{"type": "sync_required"})
	}
}
