package migrations

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
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

func TestFrozenV001SQLiteMatchesCurrentModels(t *testing.T) {
	frozen := migrationTestDB(t)
	if err := applyV001(frozen, sqliteV001); err != nil {
		t.Fatal(err)
	}
	models := migrationTestDB(t)
	if err := platform.MigrateSchema(models); err != nil {
		t.Fatal(err)
	}
	left := sqliteSchemaDDL(t, frozen)
	right := sqliteSchemaDDL(t, models)
	for _, name := range []string{"feed_fanout_jobs", "feed_inboxes", "idx_feed_fanout_jobs_video_id", "idx_feed_fanout_dispatch", "ux_feed_inbox", "idx_feed_inbox_user", "idx_video_feed_pull"} {
		delete(right, name)
	}
	right["videos"] = strings.Replace(right["videos"], "`feed_pull` numeric NOT NULL DEFAULT true,", "", 1)
	if !reflect.DeepEqual(left, right) {
		for name, ddl := range right {
			if left[name] != ddl {
				t.Errorf("v001 snapshot differs from current model at %s: frozen=%q model=%q", name, left[name], ddl)
			}
		}
		for name := range left {
			if _, ok := right[name]; !ok {
				t.Errorf("v001 snapshot has extra schema object %s", name)
			}
		}
	}
}

func sqliteSchemaDDL(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := raw.Query("SELECT name, sql FROM sqlite_master WHERE type IN ('table', 'index') AND sql IS NOT NULL AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	objects := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		objects[name] = strings.ReplaceAll(ddl, " IF NOT EXISTS", "")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
}

func TestApplyRejectsIncompleteLegacySchema(t *testing.T) {
	db := migrationTestDB(t)
	if err := db.Exec("CREATE TABLE users (id integer PRIMARY KEY AUTOINCREMENT,username text NOT NULL,password_hash text NOT NULL,nickname text NOT NULL,avatar_url text,last_seen_at datetime,created_at datetime)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil || !strings.Contains(err.Error(), "required column users.bio is missing") {
		t.Fatalf("expected frozen schema drift error, got %v", err)
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: baselineKey}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("incomplete schema marker: count=%d err=%v", count, err)
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
	if err := db.Model(&platform.SchemaMigration{}).Count(&count).Error; err != nil || count != 5 {
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
	if err := db.Create(&platform.SchemaMigration{Key: "schema_v005", AppliedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		t.Fatalf("expected future-schema rejection after v001, got %v", err)
	}
}

func TestApplyUpgradesV001AndRetriesInterruptedV002(t *testing.T) {
	db := migrationTestDB(t)
	if err := createMigrationRegistry(db, sqliteV001); err != nil {
		t.Fatal(err)
	}
	if err := applyV001(db, sqliteV001); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&platform.SchemaMigration{Key: baselineKey, AppliedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	event := platform.OutboxEvent{EventKey: "v002-upgrade", Topic: "notification.created", Payload: "{}", AvailableAt: time.Now().UTC()}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE idx_outbox_dispatch (id integer)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil {
		t.Fatal("expected the blocked v002 index to fail")
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: upgradeV002Key}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed v002 marker: count=%d err=%v", count, err)
	}
	if err := db.Exec("DROP TABLE idx_outbox_dispatch").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Apply(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasIndex(&platform.OutboxEvent{}, "idx_outbox_dispatch") {
		t.Fatal("v002 outbox index is missing")
	}
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: upgradeV002Key}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v002 marker after retry: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.OutboxEvent{}).Where("id = ?", event.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("outbox row after v002: count=%d err=%v", count, err)
	}
}

func TestApplyRejectsWrongV002Index(t *testing.T) {
	db := migrationTestDB(t)
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP INDEX idx_outbox_dispatch").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE INDEX idx_outbox_dispatch ON outbox_events(id)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` IN ?", []string{upgradeV003Key, upgradeV004Key}).Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` = ?", upgradeV002Key).Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil || !strings.Contains(err.Error(), "index columns differ") {
		t.Fatalf("expected index drift rejection, got %v", err)
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: upgradeV002Key}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("wrong index marked as v002: count=%d err=%v", count, err)
	}
}

func TestApplyRejectsV002WithoutV001(t *testing.T) {
	db := migrationTestDB(t)
	if err := createMigrationRegistry(db, sqliteV001); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&platform.SchemaMigration{Key: upgradeV002Key, AppliedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil || !strings.Contains(err.Error(), "without schema_v001") {
		t.Fatalf("expected invalid marker order rejection, got %v", err)
	}
	if db.Migrator().HasTable(&platform.User{}) {
		t.Fatal("invalid marker order changed the user schema")
	}
}

func TestApplyRetriesInterruptedV003WithoutLosingRows(t *testing.T) {
	db := migrationTestDB(t)
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	user := platform.User{Username: "before-v003-retry", Nickname: "Existing", PasswordHash: "test"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` IN ?", []string{upgradeV003Key, upgradeV004Key}).Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP INDEX idx_feed_inbox_user").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE idx_feed_inbox_user (id integer)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil {
		t.Fatal("expected blocked v003 index to fail")
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where("`key` = ?", upgradeV003Key).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed v003 was marked: count=%d err=%v", count, err)
	}
	if err := db.Exec("DROP TABLE idx_feed_inbox_user").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Apply(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&platform.User{}).Where("id = ?", user.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v003 retry lost user: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Where("`key` = ?", upgradeV003Key).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v003 marker: count=%d err=%v", count, err)
	}
}

func TestApplyV004BackfillsCompletedFanoutAndRetries(t *testing.T) {
	db := migrationTestDB(t)
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	user := platform.User{Username: "before-v004", Nickname: "Existing", PasswordHash: "test"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	video := platform.Video{AuthorID: user.ID, Title: "completed", MediaURL: "/media/v004.mp4", Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	done := time.Now().UTC()
	job := platform.FeedFanoutJob{VideoID: video.ID, AuthorID: user.ID, AvailableAt: done, DoneAt: &done}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` = ?", upgradeV004Key).Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropIndex(&platform.Video{}, "idx_video_feed_pull"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Apply(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.First(&video, video.ID).Error; err != nil || video.FeedPull {
		t.Fatalf("v004 backfill did not retire completed video: %+v err=%v", video, err)
	}
	if !db.Migrator().HasIndex(&platform.Video{}, "idx_video_feed_pull") {
		t.Fatal("v004 pull index was not restored")
	}
}

func TestApplyRejectsNonUniqueV003InboxIndex(t *testing.T) {
	db := migrationTestDB(t)
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` IN ?", []string{upgradeV003Key, upgradeV004Key}).Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP INDEX ux_feed_inbox").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE INDEX ux_feed_inbox ON feed_inboxes(user_id, video_id)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil || !strings.Contains(err.Error(), "must be unique") {
		t.Fatalf("expected unique index rejection, got %v", err)
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
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: upgradeV002Key}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v002 marker after concurrent starts: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: upgradeV003Key}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v003 marker after concurrent starts: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: upgradeV004Key}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v004 marker after concurrent starts: count=%d err=%v", count, err)
	}
	if !db.Migrator().HasIndex(&platform.OutboxEvent{}, "idx_outbox_dispatch") {
		t.Fatal("v002 did not create the outbox index")
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
	if err := db.Model(&platform.SchemaMigration{}).Count(&count).Error; err != nil || count != 5 {
		t.Fatalf("legacy MySQL markers: count=%d err=%v", count, err)
	}
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("repeat legacy migration: %v", err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Count(&count).Error; err != nil || count != 5 {
		t.Fatalf("legacy MySQL markers after repeat: count=%d err=%v", count, err)
	}
}

// Run after TestApplyMySQLLegacyIntegration on the same disposable database.
// A v001 marker with no v002 marker models the deployed previous release.
func TestMySQLV002UpgradeIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_LEGACY_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_LEGACY_DSN to the prepared disposable legacy database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: baselineKey}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("requires a prepared v001 database: count=%d err=%v", count, err)
	}
	if err := db.Migrator().DropIndex(&platform.OutboxEvent{}, "idx_outbox_dispatch"); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` IN ?", []string{upgradeV003Key, upgradeV004Key}).Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&platform.FeedInbox{}, &platform.FeedFanoutJob{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` = ?", upgradeV002Key).Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	event := platform.OutboxEvent{EventKey: "mysql-v002-upgrade", Topic: "notification.created", Payload: "{}", AvailableAt: time.Now().UTC()}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Apply(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasIndex(&platform.OutboxEvent{}, "idx_outbox_dispatch") {
		t.Fatal("v002 MySQL outbox index is missing")
	}
	if err := db.Model(&platform.SchemaMigration{}).Where(&platform.SchemaMigration{Key: upgradeV002Key}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v002 marker: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.OutboxEvent{}).Where("id = ?", event.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("outbox row after v002: count=%d err=%v", count, err)
	}
}

// The recovery script restores a real mysqldump, removes the v002 index and
// marker, and then invokes this test against only that disposable restore DB.
func TestMySQLRecoveryDrillIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_RECOVERY_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_RECOVERY_DSN through mysql_recovery.sh")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where("`key` = ?", baselineKey).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("restored v001 marker: count=%d err=%v", count, err)
	}
	if err := db.Model(&platform.SchemaMigration{}).Where("`key` = ?", upgradeV002Key).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("v002 marker before forward migration: count=%d err=%v", count, err)
	}
	if db.Migrator().HasIndex(&platform.OutboxEvent{}, "idx_outbox_dispatch") {
		t.Fatal("v002 index still exists before forward migration")
	}
	for range 2 {
		if err := Apply(context.Background(), db); err != nil {
			t.Fatalf("apply v002 after rollback: %v", err)
		}
	}
	if err := db.Model(&platform.SchemaMigration{}).Where("`key` = ?", upgradeV002Key).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v002 marker after repeated forward migration: count=%d err=%v", count, err)
	}
	columns, err := mysqlIndexColumns(db, "outbox_events", "idx_outbox_dispatch")
	if err != nil || !reflect.DeepEqual(columns, []string{"published_at", "dead_at", "available_at", "id"}) {
		t.Fatalf("restored v002 index columns: %v err=%v", columns, err)
	}
	for _, check := range []struct {
		query string
		args  []any
	}{
		{"SELECT count(*) FROM users WHERE username = ? AND password_hash = ?", []any{"recovery-user", "fixture-hash"}},
		{"SELECT count(*) FROM videos v JOIN users u ON u.id = v.author_id WHERE u.username = ? AND v.title = ?", []any{"recovery-user", "Recovery video"}},
		{"SELECT count(*) FROM outbox_events WHERE event_key = ? AND payload = ?", []any{"recovery-event", "{}"}},
	} {
		if err := db.Raw(check.query, check.args...).Scan(&count).Error; err != nil || count != 1 {
			t.Fatalf("restored data after forward migration: query=%q count=%d err=%v", check.query, count, err)
		}
	}
}

// Run after the fresh and legacy integration tests have prepared their two
// disposable databases. This compares the frozen DDL with the former GORM
// baseline at the database metadata level, independent of statement order.
func TestMySQLMigratedSchemaMatchesLegacyModels(t *testing.T) {
	freshDSN := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	legacyDSN := os.Getenv("GOSTREAM_TEST_MYSQL_LEGACY_DSN")
	if freshDSN == "" || legacyDSN == "" {
		t.Skip("set both disposable MySQL DSNs after applying the fresh and legacy baselines")
	}
	fresh, err := gorm.Open(mysql.Open(freshDSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := gorm.Open(mysql.Open(legacyDSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT table_name, column_name, column_type, is_nullable, COALESCE(column_default, '<NULL>'), extra FROM information_schema.columns WHERE table_schema = DATABASE() ORDER BY table_name, ordinal_position",
		"SELECT table_name, index_name, non_unique, seq_in_index, column_name, COALESCE(collation, '<NULL>'), COALESCE(sub_part, 0) FROM information_schema.statistics WHERE table_schema = DATABASE() ORDER BY table_name, index_name, seq_in_index",
		"SELECT table_name, constraint_name, column_name, referenced_table_name, referenced_column_name FROM information_schema.key_column_usage WHERE table_schema = DATABASE() AND referenced_table_name IS NOT NULL ORDER BY table_name, constraint_name, ordinal_position",
	} {
		got := mysqlMetadataRows(t, fresh, query)
		want := mysqlMetadataRows(t, legacy, query)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("frozen MySQL v001 metadata differs from current GORM baseline for %s\nfrozen=%v\nGORM=%v", query, got, want)
		}
	}
}

func mysqlMetadataRows(t *testing.T, db *gorm.DB, query string) [][]string {
	t.Helper()
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := raw.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]string
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		line := make([]string, len(columns))
		for i, value := range values {
			line[i] = value.String
		}
		result = append(result, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
