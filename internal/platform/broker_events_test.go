package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
