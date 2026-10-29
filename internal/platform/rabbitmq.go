package platform

import (
	"context"
	"errors"
	"log"
	"net"
	"regexp"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const rabbitOperationTimeout = 5 * time.Second

type rabbitSession struct {
	conn *amqp.Connection
	ch   *amqp.Channel
	wire net.Conn
}

func (s *rabbitSession) close() {
	_ = s.conn.CloseDeadline(time.Now().Add(time.Second))
	_ = s.wire.Close()
}

type rabbitNotificationBroker struct {
	url     string
	queue   string
	gate    chan struct{}
	pub     *rabbitSession // Owned by gate; consumer uses its own connection.
	returns <-chan amqp.Return
	closed  bool
	metrics *serverMetrics
}

// UseRabbitMQ configures reliable notification transport without requiring a
// live broker at API startup. SQL notifications/outbox remain available offline.
func (s *Server) UseRabbitMQ(rawURL, queue string) error {
	if _, err := amqp.ParseURI(rawURL); err != nil || strings.TrimSpace(rawURL) == "" {
		return errors.New("RABBITMQ_URL must be a valid amqp or amqps URI")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`).MatchString(queue) || strings.HasPrefix(queue, "amq.") {
		return errors.New("RABBITMQ_QUEUE must be 1-100 letters, digits, dots, underscores or hyphens and cannot start with amq.")
	}
	if s.broker != nil {
		return errors.New("RabbitMQ already configured")
	}
	s.broker = &rabbitNotificationBroker{url: rawURL, queue: queue, gate: make(chan struct{}, 1), metrics: s.metrics}
	s.broker.gate <- struct{}{}
	return nil
}

func (b *rabbitNotificationBroker) open(ctx context.Context) (*rabbitSession, error) {
	var wire net.Conn
	conn, err := amqp.DialConfig(b.url, amqp.Config{Heartbeat: 5 * time.Second, Dial: func(network, addr string) (net.Conn, error) {
		var err error
		wire, err = (&net.Dialer{}).DialContext(ctx, network, addr)
		if err == nil {
			deadline, _ := ctx.Deadline()
			err = wire.SetDeadline(deadline)
		}
		return wire, err
	}})
	if err != nil {
		if wire != nil {
			_ = wire.Close()
		}
		// Never return an AMQP URI containing credentials in logs/LastError.
		return nil, errors.New("RabbitMQ connection unavailable")
	}
	session := &rabbitSession{conn: conn, wire: wire}
	deadline, _ := ctx.Deadline()
	_ = wire.SetDeadline(deadline)
	ch, err := conn.Channel()
	if err == nil {
		session.ch = ch
		err = b.declare(ch)
	}
	if err != nil {
		session.close()
		return nil, errors.New("RabbitMQ channel or topology unavailable")
	}
	return session, nil
}

func (b *rabbitNotificationBroker) declare(ch *amqp.Channel) error {
	for _, name := range []string{b.queue + ".exchange", b.queue + ".dlx"} {
		if err := ch.ExchangeDeclare(name, "direct", true, false, false, false, nil); err != nil {
			return err
		}
	}
	if _, err := ch.QueueDeclare(b.queue+".dead", true, false, false, false, amqp.Table{"x-queue-type": "quorum", "x-max-length": int32(100000), "x-overflow": "reject-publish"}); err != nil {
		return err
	}
	if err := ch.QueueBind(b.queue+".dead", notificationCreatedTopic, b.queue+".dlx", false, nil); err != nil {
		return err
	}
	args := amqp.Table{
		"x-queue-type": "quorum", "x-max-length": int32(100000), "x-overflow": "reject-publish",
		"x-dead-letter-exchange": b.queue + ".dlx", "x-dead-letter-routing-key": notificationCreatedTopic,
		"x-dead-letter-strategy": "at-least-once", "x-delivery-limit": int32(8),
		"x-delayed-retry-type": "failed", "x-delayed-retry-min": int32(1000), "x-delayed-retry-max": int32(30000),
	}
	if _, err := ch.QueueDeclare(b.queue, true, false, false, false, args); err != nil {
		return err
	}
	return ch.QueueBind(b.queue, notificationCreatedTopic, b.queue+".exchange", false, nil)
}

func (b *rabbitNotificationBroker) resetPublisher() {
	if b.pub != nil {
		b.pub.close()
		b.pub = nil
	}
	b.metrics.brokerConnected.WithLabelValues("publisher").Set(0)
}

func (b *rabbitNotificationBroker) publish(ctx context.Context, item OutboxEvent) error {
	body, err := marshalBrokerNotification(item)
	if err != nil {
		return err
	}
	return b.publishBody(ctx, body, item.EventKey)
}

func (b *rabbitNotificationBroker) publishBody(parent context.Context, body []byte, eventID string) (err error) {
	ctx, cancel := context.WithTimeout(parent, rabbitOperationTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.gate:
	}
	defer func() { b.gate <- struct{}{} }()
	if b.closed {
		return errors.New("RabbitMQ publisher closed")
	}
	defer func() {
		if err != nil {
			b.resetPublisher()
		}
	}()
	if b.pub == nil || b.pub.conn.IsClosed() {
		b.resetPublisher()
		b.pub, err = b.open(ctx)
		if err != nil {
			return err
		}
		if err = b.pub.ch.Confirm(false); err != nil {
			return errors.New("RabbitMQ publisher confirms unavailable")
		}
		b.returns = b.pub.ch.NotifyReturn(make(chan amqp.Return, 1))
		b.metrics.brokerConnected.WithLabelValues("publisher").Set(1)
	}
	deadline, _ := ctx.Deadline()
	_ = b.pub.wire.SetDeadline(deadline)
	// The client's Publish context only checks cancellation before writing.
	// Closing this dedicated connection also interrupts a blocked write.
	wire := b.pub.wire
	stop := context.AfterFunc(ctx, func() { _ = wire.Close() })
	defer stop()
	confirmation, err := b.pub.ch.PublishWithDeferredConfirmWithContext(ctx, b.queue+".exchange", notificationCreatedTopic, true, false, amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: eventID,
		Type: notificationCreatedTopic, Timestamp: time.Now().UTC(), Body: body,
	})
	if err != nil {
		return errors.New("RabbitMQ publish interrupted")
	}
	acked, err := confirmation.WaitContext(ctx)
	if err != nil || !acked {
		return errors.New("RabbitMQ publication not confirmed")
	}
	// AMQP basic.return precedes its confirm; only one publication is in flight.
	select {
	case <-b.returns:
		return errors.New("RabbitMQ publication unroutable or channel closed")
	default:
	}
	_ = b.pub.wire.SetDeadline(time.Time{})
	b.metrics.brokerMessages.WithLabelValues("published").Inc()
	return nil
}

func (s *Server) CloseRabbitMQ() {
	if s.broker == nil {
		return
	}
	b := s.broker
	<-b.gate
	defer func() { b.gate <- struct{}{} }()
	b.closed = true
	b.resetPublisher()
}

func (s *Server) RunRabbitMQ(ctx context.Context) {
	if s.broker == nil {
		return
	}
	for ctx.Err() == nil {
		err := s.consumeRabbitSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.metrics.brokerMessages.WithLabelValues("reconnect").Inc()
			log.Print("RabbitMQ consumer interrupted; retrying connection")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) consumeRabbitSession(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, rabbitOperationTimeout)
	session, err := s.broker.open(connectCtx)
	cancel()
	if err != nil {
		return err
	}
	defer session.close()
	stop := context.AfterFunc(ctx, func() { _ = session.wire.Close() })
	defer stop()
	_ = session.wire.SetDeadline(time.Now().Add(rabbitOperationTimeout))
	if err := session.ch.Qos(16, 0, false); err != nil {
		return err
	}
	deliveries, err := session.ch.ConsumeWithContext(ctx, s.broker.queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	_ = session.wire.SetDeadline(time.Time{})
	s.metrics.brokerConnected.WithLabelValues("consumer").Set(1)
	defer s.metrics.brokerConnected.WithLabelValues("consumer").Set(0)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("RabbitMQ consumer channel closed")
			}
			workCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			duplicate, workErr := s.acceptBrokerNotification(workCtx, delivery.Body, delivery.MessageId)
			cancel()
			if workErr == nil {
				err = delivery.Ack(false)
				result := "accepted"
				if duplicate {
					result = "duplicate"
				}
				s.metrics.brokerMessages.WithLabelValues(result).Inc()
			} else if errors.Is(workErr, errInvalidBrokerEvent) {
				err = delivery.Reject(false)
				s.metrics.brokerMessages.WithLabelValues("dead").Inc()
			} else {
				// basic.reject counts as a failed delivery on RabbitMQ 4.3;
				// basic.nack would not advance its poison-message limit.
				err = delivery.Reject(true)
				s.metrics.brokerMessages.WithLabelValues("retry").Inc()
			}
			if err != nil {
				return err
			}
		}
	}
}
