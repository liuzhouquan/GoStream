package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	amqp "github.com/rabbitmq/amqp091-go"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func awaitRabbit(t *testing.T, label string, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

func rabbitGet(t *testing.T, session *rabbitSession, queue string, timeout time.Duration) amqp.Delivery {
	t.Helper()
	var delivery amqp.Delivery
	awaitRabbit(t, "delivery in "+queue, timeout, func() bool {
		_ = session.wire.SetDeadline(time.Now().Add(rabbitOperationTimeout))
		var ok bool
		var err error
		delivery, ok, err = session.ch.Get(queue, false)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	})
	_ = session.wire.SetDeadline(time.Time{})
	return delivery
}

func TestRabbitMQNotificationIntegration(t *testing.T) {
	brokerURL, dsn := os.Getenv("GOSTREAM_TEST_RABBITMQ_URL"), os.Getenv("GOSTREAM_TEST_RABBITMQ_MYSQL_DSN")
	if brokerURL == "" || dsn == "" {
		t.Skip("requires RabbitMQ 4.3.7 and an empty migrated disposable GOSTREAM_TEST_RABBITMQ_MYSQL_DSN")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(nonce[:])
	queue := "gostream.test." + tag
	var dbs [2]*gorm.DB
	for i := range dbs {
		db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
		dbs[i] = db
	}
	var initial int64
	if err := dbs[0].Model(&OutboxEvent{}).Count(&initial).Error; err != nil || initial != 0 {
		t.Fatalf("requires an empty migrated outbox schema: count=%d err=%v", initial, err)
	}
	a := NewServer(dbs[0], "test-signing-secret-at-least-32-bytes", t.TempDir())
	b := NewServer(dbs[1], string(a.Secret), t.TempDir())
	for _, server := range []*Server{a, b} {
		if err := server.UseRabbitMQ(brokerURL, queue); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(server.CloseRabbitMQ)
	}
	open := func(t *testing.T) *rabbitSession {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), rabbitOperationTimeout)
		defer cancel()
		session, err := a.broker.open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = session.wire.SetDeadline(time.Time{})
		return session
	}
	admin := open(t)
	admin.close()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), rabbitOperationTimeout)
		defer cancel()
		session, err := a.broker.open(ctx)
		if err != nil {
			t.Errorf("cleanup broker topology: %v", err)
			return
		}
		defer session.close()
		for _, name := range []string{queue, queue + ".dead"} {
			if _, err := session.ch.QueueDelete(name, false, false, false); err != nil {
				t.Errorf("delete test queue: %v", err)
			}
		}
		for _, name := range []string{queue + ".exchange", queue + ".dlx"} {
			if err := session.ch.ExchangeDelete(name, false, false); err != nil {
				t.Errorf("delete test exchange: %v", err)
			}
		}
	})
	h := b.Router()
	user, token := registerTestUser(t, h, "rabbit_"+tag)
	t.Cleanup(func() {
		if err := dbs[0].Delete(&User{}, user).Error; err != nil {
			t.Errorf("delete test user: %v", err)
		}
	})
	var sequence int
	fixture := func(t *testing.T) OutboxEvent {
		t.Helper()
		sequence++
		key := fmt.Sprintf("rabbit-%s-%d", tag, sequence)
		if err := dbs[0].Transaction(func(tx *gorm.DB) error {
			return enqueueNotification(tx, Notification{UserID: user, ActorID: user, Kind: "follow", Text: key, EventKey: key})
		}); err != nil {
			t.Fatal(err)
		}
		var notice Notification
		if err := dbs[0].Where("event_key = ?", key).First(&notice).Error; err != nil {
			t.Fatal(err)
		}
		var source OutboxEvent
		if err := dbs[0].Where("event_key = ?", fmt.Sprintf("notification:%d", notice.ID)).First(&source).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := dbs[0].Where("event_key IN ?", []string{source.EventKey, "broker-delivery:" + source.EventKey}).Delete(&OutboxEvent{}).Error; err != nil {
				t.Errorf("clean source/delivery: %v", err)
			}
			if err := dbs[0].Delete(&Notification{}, notice.ID).Error; err != nil {
				t.Errorf("clean notification: %v", err)
			}
		})
		return source
	}
	consumer := func(t *testing.T) func() {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); b.RunRabbitMQ(ctx) }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("consumer did not shut down")
				}
			})
		}
		t.Cleanup(stop)
		awaitRabbit(t, "consumer connection", 10*time.Second, func() bool { return testutil.ToFloat64(b.metrics.brokerConnected.WithLabelValues("consumer")) == 1 })
		return stop
	}
	jobCount := func(t *testing.T, source OutboxEvent) int64 {
		t.Helper()
		var count int64
		if err := dbs[0].Model(&OutboxEvent{}).Where("event_key = ?", "broker-delivery:"+source.EventKey).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		return count
	}
	publish := func(t *testing.T) {
		t.Helper()
		if n, err := a.ProcessOutboxOnce(context.Background()); err != nil || n != 1 {
			t.Fatalf("source outbox: n=%d err=%v", n, err)
		}
	}
	t.Run("confirm_and_sql_ack_loss_then_consumer_ack_loss", func(t *testing.T) {
		source := fixture(t)
		// A confirmed publish whose SQL acknowledgement was lost is retried.
		if err := a.broker.publish(context.Background(), source); err != nil {
			t.Fatal(err)
		}
		publish(t)
		var stored OutboxEvent
		if err := dbs[0].First(&stored, source.ID).Error; err != nil || stored.PublishedAt == nil {
			t.Fatalf("confirmed source state: %+v err=%v", stored, err)
		}
		session := open(t)
		delivery := rabbitGet(t, session, queue, 5*time.Second)
		if delivery.DeliveryMode != amqp.Persistent || delivery.MessageId != source.EventKey {
			t.Fatalf("publication metadata: mode=%d id=%q", delivery.DeliveryMode, delivery.MessageId)
		}
		if duplicate, err := b.acceptBrokerNotification(context.Background(), delivery.Body, delivery.MessageId); err != nil || duplicate {
			t.Fatalf("first consumer commit: duplicate=%v err=%v", duplicate, err)
		}
		// Close without ACK after the SQL commit, then start a new consumer.
		session.close()
		before := testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("duplicate"))
		stop := consumer(t)
		defer stop()
		awaitRabbit(t, "both duplicate deliveries", 10*time.Second, func() bool {
			return testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("duplicate")) >= before+2
		})
		if jobCount(t, source) != 1 {
			t.Fatal("consumer duplicated its durable effect")
		}
		web := httptest.NewServer(h)
		defer web.Close()
		ws, _, err := (&websocket.Dialer{Subprotocols: []string{"gostream", "gostream.jwt." + token}}).Dial("ws"+strings.TrimPrefix(web.URL, "http")+"/api/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		var frame struct {
			Type         string       `json:"type"`
			Notification Notification `json:"notification"`
		}
		if err := ws.ReadJSON(&frame); err != nil || frame.Type != "ready" {
			t.Fatalf("socket ready: %+v %v", frame, err)
		}
		if n, err := b.ProcessOutboxOnce(context.Background()); err != nil || n != 1 {
			t.Fatalf("durable delivery: n=%d err=%v", n, err)
		}
		var payload notificationPayload
		if err := json.Unmarshal([]byte(source.Payload), &payload); err != nil {
			t.Fatal(err)
		}
		if err := ws.ReadJSON(&frame); err != nil || frame.Type != "notification" || frame.Notification.ID != payload.NotificationID || frame.Notification.ActorID != user || !strings.Contains(frame.Notification.Text, tag) {
			t.Fatalf("broker-to-socket notification: %+v %v", frame, err)
		}
		if err := a.broker.publish(context.Background(), source); err != nil {
			t.Fatal(err)
		}
		awaitRabbit(t, "late duplicate", 5*time.Second, func() bool {
			return testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("duplicate")) >= before+3
		})
		var job OutboxEvent
		if err := dbs[0].Where("event_key = ?", "broker-delivery:"+source.EventKey).First(&job).Error; err != nil || job.PublishedAt == nil {
			t.Fatalf("late duplicate reset delivery: %+v err=%v", job, err)
		}
	})
	t.Run("two_consumers_deduplicate_concurrent_deliveries", func(t *testing.T) {
		source := fixture(t)
		stopB := consumer(t)
		defer stopB()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); a.RunRabbitMQ(ctx) }()
		defer func() { cancel(); <-done }()
		awaitRabbit(t, "second consumer", 5*time.Second, func() bool { return testutil.ToFloat64(a.metrics.brokerConnected.WithLabelValues("consumer")) == 1 })
		accepted := func() float64 {
			return testutil.ToFloat64(a.metrics.brokerMessages.WithLabelValues("accepted")) + testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("accepted"))
		}
		duplicates := func() float64 {
			return testutil.ToFloat64(a.metrics.brokerMessages.WithLabelValues("duplicate")) + testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("duplicate"))
		}
		initialAccepted, initialDuplicates := accepted(), duplicates()
		publish(t)
		for i := 0; i < 15; i++ {
			if err := a.broker.publish(context.Background(), source); err != nil {
				t.Fatal(err)
			}
		}
		awaitRabbit(t, "16 deliveries across two consumers", 15*time.Second, func() bool { return accepted() == initialAccepted+1 && duplicates() == initialDuplicates+15 })
		if jobCount(t, source) != 1 {
			t.Fatal("concurrent consumers created multiple delivery jobs")
		}
	})
	t.Run("mandatory_unroutable_is_not_published", func(t *testing.T) {
		source := fixture(t)
		session := open(t)
		defer session.close()
		if err := session.ch.QueueUnbind(queue, notificationCreatedTopic, queue+".exchange", nil); err != nil {
			t.Fatal(err)
		}
		publish(t)
		var stored OutboxEvent
		if err := dbs[0].First(&stored, source.ID).Error; err != nil || stored.PublishedAt != nil || !strings.Contains(stored.LastError, "unroutable") {
			t.Fatalf("unroutable source marked sent: %+v err=%v", stored, err)
		}
		if err := dbs[0].Model(&stored).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		stop := consumer(t) // Reconnection reestablishes the binding.
		defer stop()
		publish(t)
		awaitRabbit(t, "rerouted delivery", 5*time.Second, func() bool { return jobCount(t, source) == 1 })
	})
	t.Run("invalid_event_goes_to_durable_dead_letter_queue", func(t *testing.T) {
		stop := consumer(t)
		defer stop()
		if err := a.broker.publishBody(context.Background(), []byte("{bad json"), "invalid-test"); err != nil {
			t.Fatal(err)
		}
		session := open(t)
		defer session.close()
		dead := rabbitGet(t, session, queue+".dead", 10*time.Second)
		if dead.MessageId != "invalid-test" || dead.Headers["x-first-death-reason"] != "rejected" || dead.DeliveryMode != amqp.Persistent {
			t.Fatalf("invalid event dead letter: id=%q headers=%v mode=%d", dead.MessageId, dead.Headers, dead.DeliveryMode)
		}
		if err := dead.Ack(false); err != nil {
			t.Fatal(err)
		}
	})
	for _, exhaust := range []bool{false, true} {
		t.Run(fmt.Sprintf("sql_failure_exhaust_%v", exhaust), func(t *testing.T) {
			source := fixture(t)
			var fail atomic.Bool
			fail.Store(true)
			const callback = "test:real_rabbit_sql_failure"
			if err := dbs[1].Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if item, ok := tx.Statement.Dest.(*OutboxEvent); ok && item.EventKey == "broker-delivery:"+source.EventKey && fail.Load() {
					tx.AddError(errors.New("injected SQL outage"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = dbs[1].Callback().Create().Remove(callback) })
			before := testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("retry"))
			stop := consumer(t)
			defer stop()
			publish(t)
			awaitRabbit(t, "SQL failure rejected for retry", 5*time.Second, func() bool { return testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("retry")) > before })
			if jobCount(t, source) != 0 {
				t.Fatal("failed transaction left a delivery row")
			}
			if exhaust {
				session := open(t)
				dead := rabbitGet(t, session, queue+".dead", 70*time.Second)
				if dead.MessageId != source.EventKey || dead.Headers["x-first-death-reason"] != "delivery_limit" {
					t.Fatalf("retry exhaustion: id=%q headers=%v", dead.MessageId, dead.Headers)
				}
				// Put it back in DLQ, then exercise the operator's confirmed replay.
				session.close()
				fail.Store(false)
				if n, err := a.ReplayRabbitDeadLetters(context.Background(), 1); err != nil || n != 1 {
					t.Fatalf("DLQ replay: n=%d err=%v", n, err)
				}
			} else {
				fail.Store(false)
			}
			awaitRabbit(t, "SQL recovery commit", 10*time.Second, func() bool { return jobCount(t, source) == 1 })
		})
	}
	t.Run("broker_restart_keeps_committed_sql_event", func(t *testing.T) {
		container := os.Getenv("GOSTREAM_TEST_RABBITMQ_CONTAINER")
		if container == "" {
			t.Skip("set disposable GOSTREAM_TEST_RABBITMQ_CONTAINER for stop/start recovery")
		}
		if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(container) {
			t.Fatal("invalid test container name")
		}
		docker := func(args ...string) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
				t.Fatalf("docker %s: %v %s", args[0], err, out)
			}
		}
		stop := consumer(t)
		defer stop()
		source := fixture(t)
		docker("stop", "--time=1", container)
		t.Cleanup(func() { docker("start", container) })
		publish(t)
		var stored OutboxEvent
		if err := dbs[0].First(&stored, source.ID).Error; err != nil || stored.PublishedAt != nil || stored.LastError == "" {
			t.Fatalf("broker outage lost pending event: %+v err=%v", stored, err)
		}
		docker("start", container)
		awaitRabbit(t, "broker ready after restart", 30*time.Second, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			session, err := a.broker.open(ctx)
			if err == nil {
				session.close()
			}
			return err == nil
		})
		if err := dbs[0].Model(&stored).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		publish(t)
		awaitRabbit(t, "consumer automatically recovered", 15*time.Second, func() bool { return jobCount(t, source) == 1 })
	})
	t.Run("confirmed_publish_before_sql_completion_retry", func(t *testing.T) {
		source := fixture(t)
		// Simulate publish reaching broker but publisher dropping connection before SQL confirmation
		if err := a.broker.publish(context.Background(), source); err != nil {
			t.Fatal(err)
		}
		var stored OutboxEvent
		if err := dbs[0].First(&stored, source.ID).Error; err != nil || stored.PublishedAt != nil {
			t.Fatalf("expected unconfirmed source: %+v", stored)
		}
		// Worker retries publishing with same MessageId/EventKey
		publish(t)
		if err := dbs[0].First(&stored, source.ID).Error; err != nil || stored.PublishedAt == nil {
			t.Fatalf("expected confirmed source after retry: %+v", stored)
		}
		// Consumer handles potential duplicate and ensures exactly 1 delivery job
		stop := consumer(t)
		defer stop()
		awaitRabbit(t, "deduplicated delivery after confirm window retry", 10*time.Second, func() bool {
			return jobCount(t, source) == 1
		})
	})
	t.Run("dlq_target_outage_and_recovery", func(t *testing.T) {
		stop := consumer(t)
		defer stop()
		session := open(t)
		defer session.close()
		// The previous confirmed-publish test leaves a connected publisher.
		// Opening it here would redeclare/rebind the target, defeating the outage.
		if a.broker.pub == nil || a.broker.pub.conn.IsClosed() {
			t.Fatal("publisher must be warm before removing DLQ binding")
		}
		if err := session.ch.QueueUnbind(queue+".dead", notificationCreatedTopic, queue+".dlx", nil); err != nil {
			t.Fatal(err)
		}
		before := testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("dead"))
		if err := a.broker.publishBody(context.Background(), []byte("{corrupt-pre-recovery"), "dlq-outage-test-1"); err != nil {
			t.Fatal(err)
		}
		awaitRabbit(t, "consumer rejected poison while target unbound", 10*time.Second, func() bool { return testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("dead")) >= before+1 })
		// The launcher verifies a one-second broker retry interval. Keep the
		// target absent through multiple retries, and preserve same-ID recovery.
		for until := time.Now().Add(2 * time.Second); time.Now().Before(until); {
			q, err := session.ch.QueueInspect(queue + ".dead")
			if err != nil || q.Messages != 0 {
				t.Fatalf("unbound target received message: %+v %v", q, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err := session.ch.QueueBind(queue+".dead", notificationCreatedTopic, queue+".dlx", false, nil); err != nil {
			t.Fatal(err)
		}
		wait := 210 * time.Second // RabbitMQ 4.3.7 default retry is 180 seconds.
		if raw := os.Getenv("GOSTREAM_TEST_RABBITMQ_DLQ_WAIT"); raw != "" {
			var err error
			wait, err = time.ParseDuration(raw)
			if err != nil || wait <= 0 {
				t.Fatal("invalid DLQ wait duration")
			}
		}
		dead := rabbitGet(t, session, queue+".dead", wait)
		if dead.MessageId != "dlq-outage-test-1" || dead.Headers["x-first-death-reason"] != "rejected" || dead.DeliveryMode != amqp.Persistent {
			t.Fatalf("lost or changed original poison: id=%q headers=%v", dead.MessageId, dead.Headers)
		}
		if err := dead.Ack(false); err != nil {
			t.Fatal(err)
		}
		if err := a.broker.publishBody(context.Background(), []byte("{corrupt-post-recovery"), "dlq-outage-test-2"); err != nil {
			t.Fatal(err)
		}
		dead = rabbitGet(t, session, queue+".dead", 10*time.Second)
		if dead.MessageId != "dlq-outage-test-2" {
			t.Fatalf("post-recovery poison id=%q", dead.MessageId)
		}
		if err := dead.Ack(false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("finite_backlog_drain_50_events", func(t *testing.T) {
		const backlogCount = 50
		sources := make([]OutboxEvent, backlogCount)
		for i := 0; i < backlogCount; i++ {
			sources[i] = fixture(t)
			if err := a.broker.publish(context.Background(), sources[i]); err != nil {
				t.Fatalf("publish backlog item %d: %v", i, err)
			}
			// Mark source outbox as published to RabbitMQ
			if err := dbs[0].Model(&sources[i]).Update("published_at", time.Now().UTC()).Error; err != nil {
				t.Fatal(err)
			}
		}

		var countBefore int64
		if err := dbs[0].Model(&OutboxEvent{}).Where("topic = ?", notificationDeliverTopic).Count(&countBefore).Error; err != nil {
			t.Fatal(err)
		}

		// Start consumer to drain sustained backlog
		stop := consumer(t)
		defer stop()

		awaitRabbit(t, "50 sustained backlog delivery jobs in MySQL", 30*time.Second, func() bool {
			var count int64
			if err := dbs[0].Model(&OutboxEvent{}).Where("topic = ?", notificationDeliverTopic).Count(&count).Error; err != nil {
				return false
			}
			return count >= countBefore+backlogCount
		})

		// Drain delivery outbox in local worker
		drained := 0
		for drained < backlogCount {
			n, err := b.ProcessOutboxOnce(context.Background())
			if err != nil {
				t.Fatalf("drain local delivery outbox: %v", err)
			}
			if n == 0 {
				break
			}
			drained += n
		}

		// Replay deliveries: assert all 50 deduplicated
		beforeDup := testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("duplicate"))
		for i := 0; i < backlogCount; i++ {
			if err := a.broker.publish(context.Background(), sources[i]); err != nil {
				t.Fatalf("republish backlog item %d: %v", i, err)
			}
		}
		awaitRabbit(t, "all 50 sustained redeliveries detected as duplicates", 20*time.Second, func() bool {
			return testutil.ToFloat64(b.metrics.brokerMessages.WithLabelValues("duplicate")) >= beforeDup+float64(backlogCount)
		})
	})
}
