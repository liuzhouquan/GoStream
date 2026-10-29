package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const notificationCreatedTopic = "notification.created"
const notificationDeliverTopic = "notification.deliver"
const maxBrokerEventBytes = 16 << 10

var errInvalidBrokerEvent = errors.New("invalid notification broker event")

type brokerNotification struct {
	Version        int    `json:"version"`
	EventID        string `json:"event_id"`
	Topic          string `json:"topic"`
	NotificationID uint   `json:"notification_id"`
}

func (e brokerNotification) validate() error {
	if e.Version != 1 || e.Topic != notificationCreatedTopic || e.NotificationID == 0 || e.EventID != fmt.Sprintf("notification:%d", e.NotificationID) {
		return errInvalidBrokerEvent
	}
	return nil
}

func marshalBrokerNotification(item OutboxEvent) ([]byte, error) {
	var payload notificationPayload
	if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
		return nil, errInvalidBrokerEvent
	}
	e := brokerNotification{Version: 1, EventID: item.EventKey, Topic: item.Topic, NotificationID: payload.NotificationID}
	if err := e.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// The delivery outbox is the consumer's durable effect and deduplication record.
// Committing it before broker ACK makes commit-before-ACK redelivery harmless.
func (s *Server) acceptBrokerNotification(ctx context.Context, body []byte, messageID string) (bool, error) {
	if len(body) == 0 || len(body) > maxBrokerEventBytes {
		return false, errInvalidBrokerEvent
	}
	var e brokerNotification
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&e); err != nil {
		return false, errInvalidBrokerEvent
	}
	if decoder.Decode(new(any)) != io.EOF || e.validate() != nil || messageID != e.EventID {
		return false, errInvalidBrokerEvent
	}
	duplicate := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var source OutboxEvent
		if err := tx.Where("event_key = ? AND topic = ?", e.EventID, notificationCreatedTopic).First(&source).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errInvalidBrokerEvent
			}
			return err
		}
		var payload notificationPayload
		if json.Unmarshal([]byte(source.Payload), &payload) != nil || payload.NotificationID != e.NotificationID {
			return errInvalidBrokerEvent
		}
		var notice Notification
		if err := tx.Select("id").First(&notice, e.NotificationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errInvalidBrokerEvent
			}
			return err
		}
		data, _ := json.Marshal(payload)
		job := OutboxEvent{EventKey: "broker-delivery:" + e.EventID, Topic: notificationDeliverTopic, Payload: string(data), AvailableAt: time.Now().UTC()}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&job)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 0 {
			var existing OutboxEvent
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("event_key = ?", job.EventKey).First(&existing).Error; err != nil {
				return err
			}
			if existing.Topic != job.Topic || existing.Payload != job.Payload {
				return errInvalidBrokerEvent
			}
			duplicate = true
		}
		return nil
	})
	return duplicate, err
}
