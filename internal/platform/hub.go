package platform

import (
	"errors"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
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
func (h *Hub) remove(id uint, c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[id] == c {
		delete(h.clients, id)
		delete(h.writeMu, id)
	}
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
	MessageID   uint   `json:"message_id,omitempty"`
	Body        string `json:"body,omitempty"`
}

func (s *Server) websocket(c *gin.Context) {
	raw := c.Query("token")
	token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected signing method")
		}
		return s.Secret, nil
	})
	if err != nil || !token.Valid {
		respondError(c, 401, "valid token query parameter required")
		return
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		respondError(c, 401, "invalid token")
		return
	}
	f, ok := claims["sub"].(float64)
	if !ok || f < 1 {
		respondError(c, 401, "invalid token")
		return
	}
	userID := uint(f)
	conn, err := s.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	s.Hub.add(userID, conn)
	defer func() { s.Hub.remove(userID, conn); _ = conn.Close() }()
	conn.SetReadLimit(8192)
	_ = conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(70 * time.Second)) })
	_ = s.Hub.send(userID, gin.H{"type": "ready"})
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
			if e.RecipientID == 0 || e.RecipientID == userID || len(e.Body) == 0 || len(e.Body) > 4000 || e.ClientID == "" {
				_ = s.Hub.send(userID, gin.H{"type": "error", "message": "invalid message"})
				continue
			}
			var msg ChatMessage
			tx := s.DB.Where("sender_id = ? AND client_id = ?", userID, e.ClientID).First(&msg)
			if tx.Error == gorm.ErrRecordNotFound {
				msg = ChatMessage{SenderID: userID, RecipientID: e.RecipientID, ClientID: e.ClientID, Body: e.Body}
				if err = s.DB.Create(&msg).Error; err != nil {
					_ = s.Hub.send(userID, gin.H{"type": "error", "message": "message could not be saved"})
					continue
				}
			} else if tx.Error != nil {
				_ = s.Hub.send(userID, gin.H{"type": "error", "message": "message could not be loaded"})
				continue
			}
			payload := gin.H{"type": "message", "message": msg}
			_ = s.Hub.send(userID, gin.H{"type": "ack", "client_id": msg.ClientID, "message_id": msg.ID})
			_ = s.Hub.send(msg.RecipientID, payload)
		default:
			_ = s.Hub.send(userID, gin.H{"type": "error", "message": "unknown event type"})
		}
	}
}
