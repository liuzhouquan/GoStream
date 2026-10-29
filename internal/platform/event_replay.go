package platform

import (
	"context"
	"errors"
	"time"
)

// RequeueDeadOutbox explicitly resets a dead, unpublished SQL event. Active or
// completed events cannot be changed by this operator action.
func (s *Server) RequeueDeadOutbox(ctx context.Context, key string) error {
	if len(key) == 0 || len(key) > 100 {
		return errors.New("event key must contain 1-100 bytes")
	}
	res := s.DB.WithContext(ctx).Model(&OutboxEvent{}).
		Where("event_key = ? AND dead_at IS NOT NULL AND published_at IS NULL", key).
		Updates(map[string]any{"dead_at": nil, "attempts": 0, "last_error": "", "available_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errors.New("dead unpublished outbox event not found")
	}
	return nil
}

// ReplayRabbitDeadLetters confirms each republish before acknowledging its DLQ
// delivery. An interruption can repeat an event; consumer deduplication applies.
func (s *Server) ReplayRabbitDeadLetters(ctx context.Context, limit int) (int, error) {
	if s.broker == nil || limit < 1 || limit > 1000 {
		return 0, errors.New("RabbitMQ must be configured and replay limit must be 1-1000")
	}
	openCtx, cancel := context.WithTimeout(ctx, rabbitOperationTimeout)
	session, err := s.broker.open(openCtx)
	cancel()
	if err != nil {
		return 0, err
	}
	defer session.close()
	stop := context.AfterFunc(ctx, func() { _ = session.wire.Close() })
	defer stop()
	for count := 0; count < limit; count++ {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		_ = session.wire.SetDeadline(time.Now().Add(rabbitOperationTimeout))
		delivery, ok, err := session.ch.Get(s.broker.queue+".dead", false)
		if err != nil || !ok {
			return count, err
		}
		if err := s.broker.publishBody(ctx, delivery.Body, delivery.MessageId); err != nil {
			return count, err
		}
		if err := delivery.Ack(false); err != nil {
			return count, err
		}
	}
	return limit, nil
}
