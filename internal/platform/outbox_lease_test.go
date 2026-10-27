package platform

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

func checkOutboxLeaseHandoff(t *testing.T, dbA, dbB *gorm.DB) {
	t.Helper()
	for _, tc := range []struct {
		name            string
		initialAttempts int
		firstErr        error
		secondErr       error
		wantPublished   bool
		wantDead        bool
		wantLastError   string
	}{
		{name: "stale failure after new success", firstErr: errors.New("old worker failed"), wantPublished: true},
		{name: "stale success after new dead letter", initialAttempts: outboxMaxAttempts - 1, secondErr: errors.New("new worker failed"), wantDead: true, wantLastError: "new worker failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := OutboxEvent{
				EventKey: fmt.Sprintf("lease-test-%d", time.Now().UnixNano()),
				Topic:    "test", Payload: "{}", Attempts: tc.initialAttempts,
				AvailableAt: time.Now().UTC().Add(-time.Second),
			}
			if err := dbA.Create(&event).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := dbA.Delete(&OutboxEvent{}, event.ID).Error; err != nil {
					t.Errorf("clean outbox fixture: %v", err)
				}
			})
			first := NewServer(dbA, "test-signing-secret-at-least-32-bytes", t.TempDir())
			second := NewServer(dbB, "test-signing-secret-at-least-32-bytes", t.TempDir())
			claimed := make(chan struct{})
			release := make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				_, err := first.processOutboxOnce(context.Background(), func(_ context.Context, item OutboxEvent) error {
					if item.ID != event.ID {
						return fmt.Errorf("first worker claimed unexpected event %d", item.ID)
					}
					close(claimed)
					<-release
					return tc.firstErr
				})
				firstDone <- err
			}()
			select {
			case <-claimed:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("first worker did not claim the event")
			}
			// Model passage of the 30-second lease without a slow test.
			if err := dbB.Model(&OutboxEvent{}).Where("id = ?", event.ID).
				Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
				close(release)
				t.Fatal(err)
			}
			processed, secondErr := second.processOutboxOnce(context.Background(), func(_ context.Context, item OutboxEvent) error {
				if item.ID != event.ID {
					return fmt.Errorf("second worker claimed unexpected event %d", item.ID)
				}
				return tc.secondErr
			})
			close(release)
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
			if secondErr != nil || processed != 1 {
				t.Fatalf("second worker: processed=%d err=%v", processed, secondErr)
			}
			var stored OutboxEvent
			if err := dbA.First(&stored, event.ID).Error; err != nil {
				t.Fatal(err)
			}
			if (stored.PublishedAt != nil) != tc.wantPublished || (stored.DeadAt != nil) != tc.wantDead ||
				stored.LastError != tc.wantLastError || stored.Attempts != tc.initialAttempts+2 {
				t.Fatalf("stale worker changed newer state: published=%v dead=%v last_error=%q attempts=%d", stored.PublishedAt != nil, stored.DeadAt != nil, stored.LastError, stored.Attempts)
			}
		})
	}
}

func TestOutboxLeaseHandoff(t *testing.T) {
	_, _, db := testServer(t)
	checkOutboxLeaseHandoff(t, db, db)
}

func TestOutboxExpiredLeaseWithoutTakeover(t *testing.T) {
	s, _, db := testServer(t)
	event := OutboxEvent{EventKey: "expired-without-takeover", Topic: "test", Payload: "{}", AvailableAt: time.Now().UTC().Add(-time.Second)}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	claimed := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.processOutboxOnce(context.Background(), func(_ context.Context, _ OutboxEvent) error {
			close(claimed)
			<-release
			return nil
		})
		done <- err
	}()
	select {
	case <-claimed:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("worker did not claim event")
	}
	if err := db.Model(&OutboxEvent{}).Where("id = ?", event.ID).
		Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var stored OutboxEvent
	if err := db.First(&stored, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.PublishedAt != nil || stored.DeadAt != nil || stored.Attempts != 1 || stored.AvailableAt.After(time.Now()) {
		t.Fatalf("expired lease was finalized or hidden from retry: %+v", stored)
	}
}
