package migrations

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhouquan/GoStream/internal/platform"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newContinuousTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "continuous_migrations.db") + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	db, err := gorm.Open(gormsqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestContinuousV005_FreshInstallAllFiveVersions verifies that a fresh installation
// from scratch applies all 5 migrations in order and sets up the complete v005 schema.
func TestContinuousV005_FreshInstallAllFiveVersions(t *testing.T) {
	db := newContinuousTestDB(t)

	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("fresh install Apply failed: %v", err)
	}

	var records []platform.SchemaMigration
	if err := db.Order("applied_at ASC").Find(&records).Error; err != nil {
		t.Fatal(err)
	}

	expectedKeys := []string{baselineKey, upgradeV002Key, upgradeV003Key, upgradeV004Key, upgradeV005Key}
	if len(records) != len(expectedKeys) {
		t.Fatalf("expected %d migration records, got %d: %+v", len(expectedKeys), len(records), records)
	}

	for i, expected := range expectedKeys {
		if records[i].Key != expected {
			t.Errorf("migration[%d]: expected key %q, got %q", i, expected, records[i].Key)
		}
	}

	// Verify required v005 tables and indexes exist
	var rangeTableCount int64
	if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'message_device_ack_ranges'").Scan(&rangeTableCount).Error; err != nil || rangeTableCount != 1 {
		t.Fatalf("table message_device_ack_ranges missing: %v", err)
	}

	var rangeIndexCount int64
	if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'ux_device_ack_range'").Scan(&rangeIndexCount).Error; err != nil || rangeIndexCount != 1 {
		t.Fatalf("index ux_device_ack_range missing: %v", err)
	}
}

// TestContinuousV005_IdempotencyAndRepeatedRuns verifies that repeatedly executing
// Apply on an already-migrated database performs zero changes and returns nil.
func TestContinuousV005_IdempotencyAndRepeatedRuns(t *testing.T) {
	db := newContinuousTestDB(t)

	// Pass 1: Fresh install
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("pass 1 failed: %v", err)
	}

	// Insert arbitrary production records
	user := platform.User{Username: "idempotency_user", PasswordHash: "secret_hash", Nickname: "Idempotent"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}

	// Pass 2: Repeat apply
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("pass 2 failed: %v", err)
	}

	// Pass 3: Repeat apply
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("pass 3 failed: %v", err)
	}

	// Invariant: exactly 5 migration records, user still exists
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Count(&count).Error; err != nil || count != 5 {
		t.Fatalf("expected exactly 5 migration records, got %d", count)
	}

	var foundUser platform.User
	if err := db.First(&foundUser, "username = ?", "idempotency_user").Error; err != nil || foundUser.ID != user.ID {
		t.Fatalf("user was lost or corrupted during repeated migrations: %+v", foundUser)
	}
}

// TestContinuousV005_IncrementalUpgradeAndDataPreservation verifies upgrading an existing
// v001-v004 database to v005 without data loss.
func TestContinuousV005_IncrementalUpgradeAndDataPreservation(t *testing.T) {
	db := newContinuousTestDB(t)

	// Step 1: Install baseline v001 through v004 manually
	if err := db.Exec(sqliteV001).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(sqliteV002).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(sqliteV003).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(sqliteV004).Error; err != nil {
		t.Fatal(err)
	}

	// Record versions v001 through v004
	now := time.Now().UTC()
	for _, key := range []string{baselineKey, upgradeV002Key, upgradeV003Key, upgradeV004Key} {
		if err := db.Create(&platform.SchemaMigration{Key: key, AppliedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Seed business data: user, video, direct message, device receipts
	user := platform.User{Username: "upgrade_user", PasswordHash: "secret_hash", Nickname: "Upgrade User"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	msg := platform.ChatMessage{SenderID: user.ID, RecipientID: 999, ClientID: "upgrade_cli_1", Body: "pre-upgrade message"}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatal(err)
	}
	receipt := platform.MessageDeviceAck{UserID: 999, DeviceID: "11111111-2222-3333-4444-000000000001", MessageID: msg.ID, CreatedAt: now}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}

	// Step 2: Run Apply -> must detect missing v005, apply v005, and record marker
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("incremental upgrade to v005 failed: %v", err)
	}

	var v005Record platform.SchemaMigration
	if err := db.First(&v005Record, "`key` = ?", upgradeV005Key).Error; err != nil {
		t.Fatalf("v005 migration record missing: %v", err)
	}

	// Invariant: data preserved
	var storedMsg platform.ChatMessage
	if err := db.First(&storedMsg, msg.ID).Error; err != nil || storedMsg.Body != msg.Body {
		t.Fatalf("stored message differed after upgrade: %+v", storedMsg)
	}

	var storedReceipt platform.MessageDeviceAck
	if err := db.First(&storedReceipt, "user_id = ? AND device_id = ?", 999, receipt.DeviceID).Error; err != nil {
		t.Fatalf("device receipt differed after upgrade: %+v", storedReceipt)
	}
}

// TestContinuousV005_InterruptedStepResume verifies that when an upgrade DDL statement
// succeeds partially (e.g. table created but index missing or marker not recorded),
// Apply can safely resume, restore missing pieces, and finalize the migration.
func TestContinuousV005_InterruptedStepResume(t *testing.T) {
	db := newContinuousTestDB(t)

	// Fresh install through v004
	if err := db.Exec(sqliteV001).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(sqliteV002).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(sqliteV003).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(sqliteV004).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, key := range []string{baselineKey, upgradeV002Key, upgradeV003Key, upgradeV004Key} {
		if err := db.Create(&platform.SchemaMigration{Key: key, AppliedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Simulate interrupted v005: create table, but DO NOT create index and DO NOT record marker
	if err := db.Exec("CREATE TABLE IF NOT EXISTS message_device_ack_ranges(id integer primary key autoincrement, user_id integer not null, device_id text not null, first_message_id integer not null, last_message_id integer not null, created_at datetime not null)").Error; err != nil {
		t.Fatal(err)
	}

	// Resume: Apply must detect table exists, build the missing unique index, and record v005
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("interrupted v005 resume failed: %v", err)
	}

	var v005Record platform.SchemaMigration
	if err := db.First(&v005Record, "`key` = ?", upgradeV005Key).Error; err != nil {
		t.Fatalf("v005 record missing after resume: %v", err)
	}

	var indexCount int64
	if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'ux_device_ack_range'").Scan(&indexCount).Error; err != nil || indexCount != 1 {
		t.Fatalf("index ux_device_ack_range missing after resume: %d", indexCount)
	}
}

// TestContinuousV005_RejectIncompatibleOrNewerSchema verifies that starting against
// a database marked with an unknown future schema version is rejected with an actionable error.
func TestContinuousV005_RejectIncompatibleOrNewerSchema(t *testing.T) {
	db := newContinuousTestDB(t)

	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	// Inject unknown future version
	futureKey := "schema_v006"
	if err := db.Create(&platform.SchemaMigration{Key: futureKey, AppliedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}

	err := Apply(context.Background(), db)
	if err == nil {
		t.Fatal("expected error when running against newer schema version, got nil")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("unsupported schema version %q", futureKey)) {
		t.Fatalf("unexpected error message: %v", err)
	}
}
