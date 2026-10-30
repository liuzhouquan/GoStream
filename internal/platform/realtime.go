package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const realtimeChannel = "gostream:realtime:v1"

type realtimeBridge struct {
	client *redis.Client
	nodeID string
}

type realtimeEvent struct {
	NodeID    string `json:"node_id"`
	Kind      string `json:"kind"`
	UserID    uint   `json:"user_id,omitempty"`
	GroupID   uint   `json:"group_id,omitempty"`
	MessageID uint   `json:"message_id,omitempty"`
	SenderID  uint   `json:"sender_id,omitempty"`
}

// UseRedisRealtime enables cross-instance best-effort socket delivery.
// Messages and notifications remain in SQL for reconnect recovery.
func (s *Server) UseRedisRealtime(client *redis.Client) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	s.realtime = &realtimeBridge{client: client, nodeID: hex.EncodeToString(nonce[:])}
	return nil
}

func (s *Server) publishRealtime(ctx context.Context, e realtimeEvent) error {
	if s.realtime == nil {
		return nil
	}
	e.NodeID = s.realtime.nodeID
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	return s.realtime.client.Publish(ctx, realtimeChannel, data).Err()
}

func (s *Server) presenceKey(userID uint) string {
	return fmt.Sprintf("gostream:presence:%d", userID)
}

const defaultPresenceLease = 75 * time.Second
const defaultPresenceRenew = 30 * time.Second

func (s *Server) presenceLeaseDuration() time.Duration {
	if s.presenceLease > 0 {
		return s.presenceLease
	}
	return defaultPresenceLease
}

func (s *Server) presenceRenewDuration() time.Duration {
	if s.presenceRenew > 0 {
		return s.presenceRenew
	}
	return defaultPresenceRenew
}

func (s *Server) SetPresenceLease(lease, renew time.Duration) {
	s.presenceLease = lease
	s.presenceRenew = renew
}

func (s *Server) refreshPresence(ctx context.Context, userID uint) {
	if s.realtime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	key := s.presenceKey(userID)
	until := time.Now().Add(s.presenceLeaseDuration())
	pipe := s.realtime.client.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprint(time.Now().UnixMilli()))
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(until.UnixMilli()), Member: s.realtime.nodeID})
	pipe.Expire(ctx, key, s.presenceLeaseDuration()+15*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("Redis presence refresh failed for user %d: %v", userID, err)
	}
}

func (s *Server) renewPresence(ctx context.Context, userID uint) {
	ticker := time.NewTicker(s.presenceRenewDuration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshPresence(ctx, userID)
		}
	}
}

func (s *Server) clearPresence(userID uint) {
	if s.realtime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.realtime.client.ZRem(ctx, s.presenceKey(userID), s.realtime.nodeID).Err(); err != nil {
		log.Printf("Redis presence cleanup failed for user %d: %v", userID, err)
	}
}

func (s *Server) onlineAcrossInstances(ctx context.Context, userID uint) bool {
	if s.Hub.online(userID) {
		return true
	}
	if s.realtime == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	count, err := s.realtime.client.ZCount(ctx, s.presenceKey(userID), fmt.Sprint(time.Now().UnixMilli()+1), "+inf").Result()
	return err == nil && count > 0
}

// RunRealtimeBridge resubscribes after Redis interruptions. Pub/Sub itself can
// lose events; clients recover direct messages from unread/history APIs.
func (s *Server) RunRealtimeBridge(ctx context.Context) {
	s.runRealtimeBridge(ctx, nil)
}

func (s *Server) runRealtimeBridge(ctx context.Context, ready chan<- struct{}) {
	if s.realtime == nil {
		return
	}
	for ctx.Err() == nil {
		pubsub := s.realtime.client.Subscribe(ctx, realtimeChannel)
		_, err := pubsub.Receive(ctx)
		if err == nil {
			if ready != nil {
				close(ready)
				ready = nil
			}
			messages := pubsub.Channel()
		consume:
			for {
				var msg *redis.Message
				select {
				case <-ctx.Done():
					_ = pubsub.Close()
					return
				case next, ok := <-messages:
					if !ok {
						break consume
					}
					msg = next
				}
				var event realtimeEvent
				if json.Unmarshal([]byte(msg.Payload), &event) == nil && event.NodeID != s.realtime.nodeID {
					s.deliverRealtime(event)
				}
			}
		}
		_ = pubsub.Close()
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Redis realtime subscription interrupted: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) deliverRealtime(e realtimeEvent) {
	switch e.Kind {
	case "direct":
		if !s.Hub.online(e.UserID) && !s.Hub.online(e.SenderID) {
			return
		}
		var msg ChatMessage
		if s.DB.Where("id = ? AND recipient_id = ?", e.MessageID, e.UserID).First(&msg).Error == nil && (e.SenderID == 0 || msg.SenderID == e.SenderID) {
			_ = s.Hub.send(e.UserID, gin.H{"type": "message", "message": msg})
			if e.SenderID != 0 {
				_ = s.Hub.send(e.SenderID, gin.H{"type": "message", "message": msg})
			}
		}
	case "group":
		ids := s.Hub.onlineIDs()
		if len(ids) == 0 {
			return
		}
		var msg GroupMessage
		if s.DB.Where("id = ? AND group_id = ? AND sender_id = ?", e.MessageID, e.GroupID, e.SenderID).First(&msg).Error != nil {
			return
		}
		var members []GroupMember
		if s.DB.Where("group_id = ? AND user_id IN ?", e.GroupID, ids).Find(&members).Error != nil {
			return
		}
		for _, member := range members {
			_ = s.Hub.send(member.UserID, gin.H{"type": "group_message", "message": msg})
		}
	case "notification":
		if !s.Hub.online(e.UserID) {
			return
		}
		var notice Notification
		if s.DB.Preload("Actor").Where("id = ? AND user_id = ?", e.MessageID, e.UserID).First(&notice).Error == nil {
			_ = s.Hub.send(e.UserID, gin.H{"type": "notification", "notification": notice})
		}
	}
}
