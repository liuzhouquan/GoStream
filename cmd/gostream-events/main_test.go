package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/liuzhouquan/GoStream/internal/migrations"
	"github.com/liuzhouquan/GoStream/internal/platform"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestOperatorRequeuesOnlySpecifiedDeadEvent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "events.db") + "?_journal_mode=WAL&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, key := range []string{"notification:1", "notification:2"} {
		if err := db.Create(&platform.OutboxEvent{EventKey: key, Topic: "notification.created", Payload: `{"notification_id":1}`, Attempts: 8, DeadAt: &now, AvailableAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DB_DRIVER", "sqlite")
	t.Setenv("DB_DSN", dsn)
	if err := run([]string{"-outbox-key", "notification:1"}); err != nil {
		t.Fatal(err)
	}
	var first, second platform.OutboxEvent
	if err := db.Where("event_key = ?", "notification:1").First(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("event_key = ?", "notification:2").First(&second).Error; err != nil {
		t.Fatal(err)
	}
	if first.DeadAt != nil || first.Attempts != 0 || second.DeadAt == nil || second.Attempts != 8 {
		t.Fatalf("operator changed wrong rows: first=%+v second=%+v", first, second)
	}
	if err := run([]string{"-outbox-key", "notification:1"}); err == nil {
		t.Fatal("active event was requeued")
	}
	if err := run([]string{"-outbox-key", "notification:2", "-dead-letters", "1"}); err == nil {
		t.Fatal("ambiguous operator command was accepted")
	}
}
