package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func brokerEventFixture(t *testing.T, db *gorm.DB) OutboxEvent {
	t.Helper()
	u := User{Username: "broker-target", PasswordHash: "fixture", Nickname: "Target"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return enqueueNotification(tx, Notification{UserID: u.ID, ActorID: u.ID, Kind: "follow", EventKey: "broker-fixture", Text: "follow fixture"})
	}); err != nil {
		t.Fatal(err)
	}
	var item OutboxEvent
	if err := db.Where("topic = ?", notificationCreatedTopic).First(&item).Error; err != nil {
		t.Fatal(err)
	}
	return item
}

func TestBrokerNotificationCommitAndDeduplication(t *testing.T) {
	s, _, db := testServer(t)
	item := brokerEventFixture(t, db)
	body, err := marshalBrokerNotification(item)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		duplicate, err := s.acceptBrokerNotification(context.Background(), body, item.EventKey)
		if err != nil || duplicate != (i > 0) {
			t.Fatalf("delivery %d: duplicate=%v err=%v", i, duplicate, err)
		}
	}
	var job OutboxEvent
	if err := db.Where("event_key = ?", "broker-delivery:"+item.EventKey).First(&job).Error; err != nil || job.Topic != notificationDeliverTopic || job.Payload != item.Payload {
		t.Fatalf("durable delivery: %+v err=%v", job, err)
	}
	now := time.Now().UTC()
	if err := db.Model(&job).Update("published_at", now).Error; err != nil {
		t.Fatal(err)
	}
	if duplicate, err := s.acceptBrokerNotification(context.Background(), body, item.EventKey); err != nil || !duplicate {
		t.Fatalf("late duplicate: duplicate=%v err=%v", duplicate, err)
	}
	if err := db.First(&job, job.ID).Error; err != nil || job.PublishedAt == nil {
		t.Fatalf("late duplicate reset delivered job: %+v err=%v", job, err)
	}
	var notices, jobs int64
	if err := db.Model(&Notification{}).Count(&notices).Error; err != nil || notices != 1 {
		t.Fatalf("inbox changed: count=%d err=%v", notices, err)
	}
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", notificationDeliverTopic).Count(&jobs).Error; err != nil || jobs != 1 {
		t.Fatalf("delivery duplicated: count=%d err=%v", jobs, err)
	}
}

func TestBrokerNotificationRejectsMalformedAndConflictingEvents(t *testing.T) {
	s, _, db := testServer(t)
	item := brokerEventFixture(t, db)
	body, err := marshalBrokerNotification(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "{", string(body) + " {}", strings.Replace(string(body), `"version":1`, `"version":2`, 1), strings.Replace(string(body), notificationCreatedTopic, "other", 1), strings.TrimSuffix(string(body), "}") + `,"extra":true}`, strings.Repeat("x", maxBrokerEventBytes+1)} {
		if _, err := s.acceptBrokerNotification(context.Background(), []byte(bad), item.EventKey); !errors.Is(err, errInvalidBrokerEvent) {
			t.Fatalf("invalid body accepted: len=%d err=%v", len(bad), err)
		}
	}
	if _, err := s.acceptBrokerNotification(context.Background(), body, "wrong-id"); !errors.Is(err, errInvalidBrokerEvent) {
		t.Fatalf("message ID mismatch accepted: %v", err)
	}
	conflict := OutboxEvent{EventKey: "broker-delivery:" + item.EventKey, Topic: notificationDeliverTopic, Payload: `{"notification_id":999}`, AvailableAt: time.Now().UTC()}
	if err := db.Create(&conflict).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.acceptBrokerNotification(context.Background(), body, item.EventKey); !errors.Is(err, errInvalidBrokerEvent) {
		t.Fatalf("conflicting receipt accepted: %v", err)
	}
}

func TestBrokerNotificationTransactionFailureCanRetry(t *testing.T) {
	s, _, db := testServer(t)
	item := brokerEventFixture(t, db)
	body, _ := marshalBrokerNotification(item)
	failure := errors.New("injected consumer write failure")
	var fail atomic.Bool
	fail.Store(true)
	const callback = "test:broker_consumer_failure"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if job, ok := tx.Statement.Dest.(*OutboxEvent); ok && job.Topic == notificationDeliverTopic && fail.Load() {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
	if _, err := s.acceptBrokerNotification(context.Background(), body, item.EventKey); !errors.Is(err, failure) || errors.Is(err, errInvalidBrokerEvent) {
		t.Fatalf("SQL failure misclassified: %v", err)
	}
	var count int64
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", notificationDeliverTopic).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed transaction left delivery: count=%d err=%v", count, err)
	}
	fail.Store(false)
	if duplicate, err := s.acceptBrokerNotification(context.Background(), body, item.EventKey); err != nil || duplicate {
		t.Fatalf("retry: duplicate=%v err=%v", duplicate, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.acceptBrokerNotification(ctx, body, item.EventKey); err == nil || errors.Is(err, errInvalidBrokerEvent) {
		t.Fatalf("cancellation misclassified: %v", err)
	}
}

func TestBrokerUnavailablePreservesNotificationAndOutbox(t *testing.T) {
	s, h, db := testServer(t)
	owner, token := registerTestUser(t, h, "brokeroffline")
	_, other := registerTestUser(t, h, "brokeractor")
	res := request(t, h, http.MethodPut, "/api/users/me/following/"+strconv.FormatUint(uint64(owner), 10), other, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("follow: %d %s", res.Code, res.Body.String())
	}
	if err := s.UseRabbitMQ("amqp://fixture:never-log-this@127.0.0.1:1/", "gostream.unit"); err != nil {
		t.Fatal(err)
	}
	defer s.CloseRabbitMQ()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if n, err := s.ProcessOutboxOnce(ctx); err != nil || n != 1 {
		t.Fatalf("offline publish: n=%d err=%v", n, err)
	}
	var item OutboxEvent
	if err := db.Where("topic = ?", notificationCreatedTopic).First(&item).Error; err != nil || item.PublishedAt != nil || item.Attempts != 1 || item.LastError == "" || strings.Contains(item.LastError, "never-log-this") {
		t.Fatalf("offline outbox: %+v err=%v", item, err)
	}
	if res := request(t, h, http.MethodGet, "/api/notifications", token, nil); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "brokeractor") {
		t.Fatalf("durable inbox unavailable: %d %s", res.Code, res.Body.String())
	}
}

func TestDeadOutboxReplayOnlyChangesDeadUnpublishedRows(t *testing.T) {
	s, _, db := testServer(t)
	source := brokerEventFixture(t, db)
	if err := s.RequeueDeadOutbox(context.Background(), source.EventKey); err == nil {
		t.Fatal("operator reset an active event")
	}
	now := time.Now().UTC()
	if err := db.Model(&source).Updates(map[string]any{"dead_at": now, "attempts": 8, "last_error": "fixture failure"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueDeadOutbox(context.Background(), source.EventKey); err != nil {
		t.Fatal(err)
	}
	var stored OutboxEvent
	if err := db.First(&stored, source.ID).Error; err != nil || stored.Attempts != 0 || stored.DeadAt != nil || stored.LastError != "" || stored.PublishedAt != nil {
		t.Fatalf("dead event recovery: %+v err=%v", stored, err)
	}
	if err := db.Model(&source).Updates(map[string]any{"published_at": now, "dead_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueDeadOutbox(context.Background(), source.EventKey); err == nil {
		t.Fatal("operator reset a published event")
	}
}

func TestBrokerNotificationBacklogDrain(t *testing.T) {
	s, _, db := testServer(t)
	ctx := context.Background()

	u := User{Username: "backlog-target", PasswordHash: "fixture", Nickname: "Target"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}

	items := make([]OutboxEvent, 10)
	bodies := make([][]byte, 10)
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("backlog-event-%d", i)
		if err := db.Transaction(func(tx *gorm.DB) error {
			return enqueueNotification(tx, Notification{UserID: u.ID, ActorID: u.ID, Kind: "follow", EventKey: key, Text: "backlog " + key})
		}); err != nil {
			t.Fatal(err)
		}
		var n Notification
		if err := db.Where("event_key = ?", key).First(&n).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Where("event_key = ?", fmt.Sprintf("notification:%d", n.ID)).First(&items[i]).Error; err != nil {
			t.Fatal(err)
		}
		var err error
		bodies[i], err = marshalBrokerNotification(items[i])
		if err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 10; i++ {
		duplicate, err := s.acceptBrokerNotification(ctx, bodies[i], items[i].EventKey)
		if err != nil || duplicate {
			t.Fatalf("backlog item %d: duplicate=%v err=%v", i, duplicate, err)
		}
	}

	var deliveryCount int64
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", notificationDeliverTopic).Count(&deliveryCount).Error; err != nil || deliveryCount != 10 {
		t.Fatalf("expected 10 delivery events, got %d", deliveryCount)
	}

	var noticeCount int64
	if err := db.Model(&Notification{}).Where("user_id = ?", u.ID).Count(&noticeCount).Error; err != nil || noticeCount != 10 {
		t.Fatalf("expected 10 notifications, got %d", noticeCount)
	}
}

func TestBrokerNotificationSustainedBacklogDrain(t *testing.T) {
	s, _, db := testServer(t)
	ctx := context.Background()

	// 5 distinct targets receiving notifications from 5 distinct actors
	const userCount = 5
	const eventsPerUser = 10
	const totalEvents = userCount * eventsPerUser

	users := make([]User, userCount)
	for i := 0; i < userCount; i++ {
		users[i] = User{Username: fmt.Sprintf("sustained-target-%d", i), PasswordHash: "fixture", Nickname: fmt.Sprintf("Target%d", i)}
		if err := db.Create(&users[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	items := make([]OutboxEvent, totalEvents)
	bodies := make([][]byte, totalEvents)
	idx := 0
	for uIdx := 0; uIdx < userCount; uIdx++ {
		for eIdx := 0; eIdx < eventsPerUser; eIdx++ {
			key := fmt.Sprintf("sustained-event-u%d-e%d", uIdx, eIdx)
			if err := db.Transaction(func(tx *gorm.DB) error {
				return enqueueNotification(tx, Notification{
					UserID:   users[uIdx].ID,
					ActorID:  users[(uIdx+1)%userCount].ID,
					Kind:     "follow",
					EventKey: key,
					Text:     fmt.Sprintf("sustained notice %d for user %d", eIdx, uIdx),
				})
			}); err != nil {
				t.Fatal(err)
			}
			var n Notification
			if err := db.Where("event_key = ?", key).First(&n).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Where("event_key = ?", fmt.Sprintf("notification:%d", n.ID)).First(&items[idx]).Error; err != nil {
				t.Fatal(err)
			}
			var err error
			bodies[idx], err = marshalBrokerNotification(items[idx])
			if err != nil {
				t.Fatal(err)
			}
			idx++
		}
	}

	// Simulate that the source outbox worker already published all notification.created rows to RabbitMQ
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", notificationCreatedTopic).Update("published_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}

	// Drain sustained backlog in chunks through consumer commit
	for i := 0; i < totalEvents; i++ {
		duplicate, err := s.acceptBrokerNotification(ctx, bodies[i], items[i].EventKey)
		if err != nil || duplicate {
			t.Fatalf("sustained item %d: duplicate=%v err=%v", i, duplicate, err)
		}
	}

	// Verify all delivery rows exist in SQL
	var deliveryCount int64
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", notificationDeliverTopic).Count(&deliveryCount).Error; err != nil || deliveryCount != totalEvents {
		t.Fatalf("expected %d delivery events, got %d", totalEvents, deliveryCount)
	}

	// Drain delivery outbox in batches of 32
	drained := 0
	for drained < totalEvents {
		n, err := s.ProcessOutboxOnce(ctx)
		if err != nil {
			t.Fatalf("drain outbox: %v", err)
		}
		if n == 0 {
			break
		}
		drained += n
	}
	if drained != totalEvents {
		t.Fatalf("expected to drain %d outbox items, got %d", totalEvents, drained)
	}

	// Verify all delivery rows have published_at IS NOT NULL
	var unpublishedDeliveryCount int64
	if err := db.Model(&OutboxEvent{}).Where("topic = ? AND published_at IS NULL", notificationDeliverTopic).Count(&unpublishedDeliveryCount).Error; err != nil || unpublishedDeliveryCount != 0 {
		t.Fatalf("expected 0 unpublished delivery events, got %d", unpublishedDeliveryCount)
	}

	// Redeliver entire sustained backlog: must all be classified as duplicates
	for i := 0; i < totalEvents; i++ {
		duplicate, err := s.acceptBrokerNotification(ctx, bodies[i], items[i].EventKey)
		if err != nil || !duplicate {
			t.Fatalf("sustained redelivery item %d: duplicate=%v err=%v", i, duplicate, err)
		}
	}

	// Delivery count must remain strictly unchanged
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", notificationDeliverTopic).Count(&deliveryCount).Error; err != nil || deliveryCount != totalEvents {
		t.Fatalf("expected %d delivery events after redelivery, got %d", totalEvents, deliveryCount)
	}
}

func TestBrokerPublisherConfirmFailureAndRecovery(t *testing.T) {
	s, _, db := testServer(t)
	ctx := context.Background()

	item := brokerEventFixture(t, db)

	// Attempt 1: Publisher experiences confirm failure (e.g. timeout or socket drop)
	confirmErr := errors.New("simulated publisher confirm timeout")
	var failPublish atomic.Bool
	failPublish.Store(true)

	mockDispatch := func(c context.Context, e OutboxEvent) error {
		if failPublish.Load() {
			return confirmErr
		}
		return nil
	}

	n, err := s.processOutboxOnce(ctx, mockDispatch)
	if err != nil || n != 1 {
		t.Fatalf("attempt 1: n=%d err=%v", n, err)
	}

	var row OutboxEvent
	if err := db.First(&row, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.PublishedAt != nil || row.Attempts != 1 || row.LastError != confirmErr.Error() {
		t.Fatalf("unconfirmed publish row state: %+v", row)
	}
	if !row.AvailableAt.After(time.Now().UTC()) {
		t.Fatalf("available_at not backed off: %v", row.AvailableAt)
	}

	// Recover publisher connection: fast forward available_at and allow success
	if err := db.Model(&row).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	failPublish.Store(false)

	n, err = s.processOutboxOnce(ctx, mockDispatch)
	if err != nil || n != 1 {
		t.Fatalf("attempt 2: n=%d err=%v", n, err)
	}

	if err := db.First(&row, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.PublishedAt == nil || row.Attempts != 2 || row.LastError != "" {
		t.Fatalf("recovered publish row state: %+v", row)
	}

	// Consumer receives message: commits delivery and verifies deduplication on retry
	body, err := marshalBrokerNotification(row)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := s.acceptBrokerNotification(ctx, body, row.EventKey)
	if err != nil || dup {
		t.Fatalf("first consumer accept: dup=%v err=%v", dup, err)
	}
	dup, err = s.acceptBrokerNotification(ctx, body, row.EventKey)
	if err != nil || !dup {
		t.Fatalf("retry consumer accept: dup=%v err=%v", dup, err)
	}

	var jobCount int64
	if err := db.Model(&OutboxEvent{}).Where("event_key = ?", "broker-delivery:"+row.EventKey).Count(&jobCount).Error; err != nil || jobCount != 1 {
		t.Fatalf("expected exactly 1 delivery job, got %d", jobCount)
	}
}

func TestBrokerConsumerPreACKCrashAndDeduplication(t *testing.T) {
	s, _, db := testServer(t)
	ctx := context.Background()

	item := brokerEventFixture(t, db)
	// Source event was published to RabbitMQ
	if err := db.Model(&item).Update("published_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	body, err := marshalBrokerNotification(item)
	if err != nil {
		t.Fatal(err)
	}

	// Consumer 1: processes delivery and commits to SQL
	dup1, err := s.acceptBrokerNotification(ctx, body, item.EventKey)
	if err != nil || dup1 {
		t.Fatalf("consumer 1 commit: dup=%v err=%v", dup1, err)
	}

	// Verify that the SQL delivery row exists
	var delivery OutboxEvent
	if err := db.Where("event_key = ?", "broker-delivery:"+item.EventKey).First(&delivery).Error; err != nil {
		t.Fatalf("delivery row not committed: %v", err)
	}

	// Crash window: Consumer 1 crashes before issuing basic.ack to broker.
	// Broker re-queues unacked delivery and redelivers to Consumer 2.
	dup2, err := s.acceptBrokerNotification(ctx, body, item.EventKey)
	if err != nil || !dup2 {
		t.Fatalf("consumer 2 redelivery: expected duplicate=true, got dup=%v err=%v", dup2, err)
	}

	// Consumer 2 issues basic.ack. Only 1 delivery outbox event must exist.
	var jobCount int64
	if err := db.Model(&OutboxEvent{}).Where("event_key = ?", "broker-delivery:"+item.EventKey).Count(&jobCount).Error; err != nil || jobCount != 1 {
		t.Fatalf("expected exactly 1 delivery row, got %d", jobCount)
	}

	// Dispatch delivery outbox row: single execution
	n, err := s.ProcessOutboxOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("dispatch delivery: n=%d err=%v", n, err)
	}
	if err := db.First(&delivery, delivery.ID).Error; err != nil || delivery.PublishedAt == nil {
		t.Fatalf("delivery row not published: %+v", delivery)
	}
}

func TestBrokerDLQReplayIdempotency(t *testing.T) {
	s, _, db := testServer(t)
	ctx := context.Background()

	item := brokerEventFixture(t, db)
	// Source event was published to RabbitMQ
	if err := db.Model(&item).Update("published_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	body, err := marshalBrokerNotification(item)
	if err != nil {
		t.Fatal(err)
	}

	// Existing completed delivery
	if dup, err := s.acceptBrokerNotification(ctx, body, item.EventKey); err != nil || dup {
		t.Fatalf("initial delivery: dup=%v err=%v", dup, err)
	}
	if n, err := s.ProcessOutboxOnce(ctx); err != nil || n != 1 {
		t.Fatalf("dispatch initial delivery: n=%d err=%v", n, err)
	}

	// Operator DLQ replay delivers message again (simulating repeated replay on interruption)
	for replay := 1; replay <= 3; replay++ {
		dup, err := s.acceptBrokerNotification(ctx, body, item.EventKey)
		if err != nil || !dup {
			t.Fatalf("replay %d: expected duplicate=true, got dup=%v err=%v", replay, dup, err)
		}
	}

	var jobCount int64
	if err := db.Model(&OutboxEvent{}).Where("event_key = ?", "broker-delivery:"+item.EventKey).Count(&jobCount).Error; err != nil || jobCount != 1 {
		t.Fatalf("expected exactly 1 delivery job after DLQ replays, got %d", jobCount)
	}
}

func TestBrokerNotificationBurstAndIdempotency(t *testing.T) {
	s, _, db := testServer(t)
	item := brokerEventFixture(t, db)
	body, err := marshalBrokerNotification(item)
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 10; attempt++ {
		duplicate, err := s.acceptBrokerNotification(context.Background(), body, item.EventKey)
		if err != nil {
			t.Fatalf("delivery attempt %d failed: %v", attempt, err)
		}
		if attempt == 0 && duplicate {
			t.Fatal("first delivery misclassified as duplicate")
		}
		if attempt > 0 && !duplicate {
			t.Fatalf("attempt %d should be duplicate", attempt)
		}
	}

	var jobCount int64
	if err := db.Model(&OutboxEvent{}).Where("event_key = ?", "broker-delivery:"+item.EventKey).Count(&jobCount).Error; err != nil || jobCount != 1 {
		t.Fatalf("expected exactly 1 delivery job, got %d", jobCount)
	}
}

func TestBrokerConsumerConcurrentDeliveriesAndPoisonMatrix(t *testing.T) {
	s, _, db := testServer(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	ctx := context.Background()

	const count = 16
	items := make([]OutboxEvent, count)
	bodies := make([][]byte, count)
	u := User{Username: "matrix-target", PasswordHash: "fixture", Nickname: "Target"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("matrix-event-%d", i)
		if err := db.Transaction(func(tx *gorm.DB) error {
			return enqueueNotification(tx, Notification{UserID: u.ID, ActorID: u.ID, Kind: "follow", EventKey: key, Text: "notice " + key})
		}); err != nil {
			t.Fatal(err)
		}
		var n Notification
		if err := db.Where("event_key = ?", key).First(&n).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Where("event_key = ?", fmt.Sprintf("notification:%d", n.ID)).First(&items[i]).Error; err != nil {
			t.Fatal(err)
		}
		var err error
		bodies[i], err = marshalBrokerNotification(items[i])
		if err != nil {
			t.Fatal(err)
		}
	}

	// Concurrent callers, with SQLite writes intentionally serialized on one
	// connection. Cross-connection SQL behavior belongs to the MySQL matrix.
	var wg sync.WaitGroup
	var errCount, created, duplicates atomic.Int64
	start := make(chan struct{})
	for i := range count {
		for range 2 {
			wg.Go(func() {
				<-start
				dup, err := s.acceptBrokerNotification(ctx, bodies[i], items[i].EventKey)
				if err != nil {
					errCount.Add(1)
				} else if dup {
					duplicates.Add(1)
				} else {
					created.Add(1)
				}
			})
		}
		wg.Go(func() {
			<-start
			_, err := s.acceptBrokerNotification(ctx, []byte(`{"version":99}`), fmt.Sprintf("matrix-poison-%d", i))
			if !errors.Is(err, errInvalidBrokerEvent) {
				errCount.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if errCount.Load() != 0 || created.Load() != count || duplicates.Load() != count {
		t.Fatalf("mixed results errors=%d created=%d duplicates=%d", errCount.Load(), created.Load(), duplicates.Load())
	}
	for _, source := range items {
		var rows int64
		if err := db.Model(&OutboxEvent{}).Where("event_key = ?", "broker-delivery:"+source.EventKey).Count(&rows).Error; err != nil || rows != 1 {
			t.Fatalf("event %s rows=%d err=%v", source.EventKey, rows, err)
		}
	}

	var jobs int64
	if err := db.Model(&OutboxEvent{}).Where("topic = ?", notificationDeliverTopic).Count(&jobs).Error; err != nil || jobs != count {
		t.Fatalf("expected exactly %d delivery jobs, got %d", count, jobs)
	}
}
