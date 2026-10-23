package migrations

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhouquan/GoStream/internal/platform"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func migrationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "schema.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestApplyCreatesSchemaAndIsIdempotent(t *testing.T) {
	db := migrationTestDB(t)
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{&platform.User{}, &platform.Video{}, &platform.ChatMessage{}, &platform.OutboxEvent{}, &platform.SchemaMigration{}} {
		if !db.Migrator().HasTable(model) {
			t.Fatalf("missing table for %T", model)
		}
	}
	user := platform.User{Username: "after-first-migration", Nickname: "User", PasswordHash: "test"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&platform.User{}).Where("id = ?", user.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("user after repeated migration: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: baselineKey}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("baseline marker: count=%d err=%v", count, err)
	}
}

func TestApplyAdoptsExistingAutoMigrateDatabase(t *testing.T) {
	db := migrationTestDB(t)
	if err := platform.MigrateSchema(db); err != nil {
		t.Fatal(err)
	}
	user := platform.User{Username: "before-baseline", Nickname: "Existing", PasswordHash: "test"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&platform.SchemaMigration{Key: "message_devices_v1", AppliedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&platform.User{}).Where("id = ?", user.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("existing user: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("migration markers: count=%d err=%v", count, err)
	}
}

func TestApplyRejectsNewerSchema(t *testing.T) {
	db := migrationTestDB(t)
	if err := db.AutoMigrate(&platform.SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&platform.SchemaMigration{Key: "schema_v999", AppliedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		t.Fatalf("expected future-schema rejection, got %v", err)
	}
	if db.Migrator().HasTable(&platform.User{}) {
		t.Fatal("unsupported migration changed the user schema")
	}
}

func TestApplyRejectsNewerSchemaAfterBaseline(t *testing.T) {
	db := migrationTestDB(t)
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&platform.SchemaMigration{Key: "schema_v002", AppliedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		t.Fatalf("expected future-schema rejection after v001, got %v", err)
	}
}

func TestApplyRetriesAfterInterruptedBaseline(t *testing.T) {
	db := migrationTestDB(t)
	// SQLite tables and indexes share a name namespace. This blocks the Feed
	// index after earlier tables have been created, without writing v001.
	if err := db.Exec("CREATE TABLE idx_video_feed (id integer)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil {
		t.Fatal("expected the first baseline attempt to fail")
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: baselineKey}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed baseline marker: count=%d err=%v", count, err)
	}
	user := platform.User{Username: "survives-interrupted-baseline", Nickname: "Existing", PasswordHash: "test"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP TABLE idx_video_feed").Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("retry baseline: %v", err)
	}
	if !db.Migrator().HasIndex(&platform.Video{}, "idx_video_feed") {
		t.Fatal("retry did not create the Feed index")
	}
	if err := db.Model(&platform.User{}).Where("id = ?", user.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("existing user after retry: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: baselineKey}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("baseline marker after retry: count=%d err=%v", count, err)
	}
}

// Set GOSTREAM_TEST_MYSQL_DSN to a dedicated empty database. This test creates
// the schema and is intentionally skipped by the default local suite.
func TestApplyMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to an empty MySQL test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable(&platform.SchemaMigration{}) {
		t.Fatal("migration integration test requires an empty database")
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errors <- Apply(context.Background(), db)
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
	if !db.Migrator().HasIndex(&platform.Video{}, "idx_video_feed") {
		t.Fatal("baseline did not create the feed index")
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: baselineKey}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("baseline marker after concurrent starts: count=%d err=%v", count, err)
	}
}

// Set GOSTREAM_TEST_MYSQL_LEGACY_DSN to a second dedicated empty database.
func TestApplyMySQLLegacyIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_LEGACY_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_LEGACY_DSN to an empty MySQL test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable(&platform.SchemaMigration{}) {
		t.Fatal("legacy migration integration test requires an empty database")
	}
	if err := platform.MigrateSchema(db); err != nil {
		t.Fatal(err)
	}
	user := platform.User{Username: "existing-mysql-user", Nickname: "Existing", PasswordHash: "test"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&platform.SchemaMigration{Key: "message_devices_v1", AppliedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	// Approximate an interrupted additive migration: earlier schema and data
	// exist, but one index and the v001 marker are absent.
	if err := db.Migrator().DropIndex(&platform.Video{}, "idx_video_feed"); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasIndex(&platform.Video{}, "idx_video_feed") {
		t.Fatal("legacy MySQL Feed index was not restored")
	}
	var count int64
	if err := db.Model(&platform.User{}).Where("id = ?", user.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("legacy MySQL user after migration: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("legacy MySQL markers: count=%d err=%v", count, err)
	}
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("repeat legacy migration: %v", err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("legacy MySQL markers after repeat: count=%d err=%v", count, err)
	}
}
