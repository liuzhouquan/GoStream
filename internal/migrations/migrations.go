package migrations

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/liuzhouquan/GoStream/internal/platform"
	"gorm.io/gorm"
)

const baselineKey = "schema_v001"
const upgradeV002Key = "schema_v002"
const upgradeV003Key = "schema_v003"
const upgradeV004Key = "schema_v004"
const upgradeV005Key = "schema_v005"
const mysqlLockName = "gostream_schema_migrations"

// These are immutable snapshots of the schema before the first explicit
// upgrade. Changes to platform models belong in a new numbered migration.
//
//go:embed sql/v001_sqlite.sql
var sqliteV001 string

//go:embed sql/v001_mysql.sql
var mysqlV001 string

//go:embed sql/v001_mysql_indexes.sql
var mysqlV001Indexes string

//go:embed sql/v001_columns.json
var v001Columns []byte

//go:embed sql/v002_sqlite.sql
var sqliteV002 string

//go:embed sql/v002_mysql.sql
var mysqlV002 string

//go:embed sql/v003_sqlite.sql
var sqliteV003 string

//go:embed sql/v003_mysql.sql
var mysqlV003 string

//go:embed sql/v004_sqlite.sql
var sqliteV004 string

//go:embed sql/v004_mysql.sql
var mysqlV004 string

//go:embed sql/v005_sqlite.sql
var sqliteV005 string

//go:embed sql/v005_mysql.sql
var mysqlV005 string

var mysqlIndexStatement = regexp.MustCompile("^CREATE (?:UNIQUE )?INDEX `([^`]+)` ON `([^`]+)` ")
var sqliteIndexStatement = regexp.MustCompile("^CREATE (?:UNIQUE )?INDEX IF NOT EXISTS `([^`]+)` ON `([^`]+)`\\(")

// Apply installs the current schema on an empty database or adopts an
// existing AutoMigrate database. The baseline records only after every
// additive schema operation succeeds, so an interrupted run can retry.
func Apply(ctx context.Context, db *gorm.DB) error {
	if db.Dialector.Name() != "mysql" {
		return apply(ctx, db)
	}
	return withMySQLLock(ctx, db, func() error { return apply(ctx, db) })
}

func apply(ctx context.Context, db *gorm.DB) error {
	db = db.WithContext(ctx)
	script, err := baselineSQL(db.Dialector.Name())
	if err != nil {
		return err
	}
	if err := createMigrationRegistry(db, script); err != nil {
		return fmt.Errorf("create schema migration registry: %w", err)
	}
	var records []platform.SchemaMigration
	if err := db.Find(&records).Error; err != nil {
		return fmt.Errorf("read schema versions: %w", err)
	}
	baselineApplied := false
	upgradeApplied := false
	v003Applied := false
	v004Applied := false
	v005Applied := false
	for _, record := range records {
		switch record.Key {
		case baselineKey:
			baselineApplied = true
		case upgradeV002Key:
			upgradeApplied = true
		case upgradeV003Key:
			v003Applied = true
		case upgradeV004Key:
			v004Applied = true
		case upgradeV005Key:
			v005Applied = true
		default:
			if strings.HasPrefix(record.Key, "schema_v") {
				return fmt.Errorf("unsupported schema version %q; start a compatible GoStream binary", record.Key)
			}
		}
	}
	if upgradeApplied && !baselineApplied {
		return errors.New("schema_v002 is recorded without schema_v001")
	}
	if v003Applied && !upgradeApplied {
		return errors.New("schema_v003 is recorded without schema_v002")
	}
	if v004Applied && !v003Applied {
		return errors.New("schema_v004 is recorded without schema_v003")
	}
	if v005Applied && !v004Applied {
		return errors.New("schema_v005 is recorded without schema_v004")
	}
	if !baselineApplied {
		if err := applyV001(db, script); err != nil {
			return fmt.Errorf("apply %s: %w", baselineKey, err)
		}
		if err := db.Create(&platform.SchemaMigration{Key: baselineKey, AppliedAt: time.Now().UTC()}).Error; err != nil {
			return fmt.Errorf("record %s: %w", baselineKey, err)
		}
	}
	if !upgradeApplied {
		if err := applyV002(db); err != nil {
			return fmt.Errorf("apply %s: %w", upgradeV002Key, err)
		}
		if err := db.Create(&platform.SchemaMigration{Key: upgradeV002Key, AppliedAt: time.Now().UTC()}).Error; err != nil {
			return fmt.Errorf("record %s: %w", upgradeV002Key, err)
		}
	}
	if !v003Applied {
		if err := applyV003(db); err != nil {
			return fmt.Errorf("apply %s: %w", upgradeV003Key, err)
		}
		if err := db.Create(&platform.SchemaMigration{Key: upgradeV003Key, AppliedAt: time.Now().UTC()}).Error; err != nil {
			return fmt.Errorf("record %s: %w", upgradeV003Key, err)
		}
	}
	if !v004Applied {
		if err := applyV004(db); err != nil {
			return fmt.Errorf("apply %s: %w", upgradeV004Key, err)
		}
		if err := db.Create(&platform.SchemaMigration{Key: upgradeV004Key, AppliedAt: time.Now().UTC()}).Error; err != nil {
			return fmt.Errorf("record %s: %w", upgradeV004Key, err)
		}
	}
	if !v005Applied {
		if err := applyV005(db); err != nil {
			return fmt.Errorf("apply %s: %w", upgradeV005Key, err)
		}
		if err := db.Create(&platform.SchemaMigration{Key: upgradeV005Key, AppliedAt: time.Now().UTC()}).Error; err != nil {
			return fmt.Errorf("record %s: %w", upgradeV005Key, err)
		}
	}
	return nil
}

func applyV005(db *gorm.DB) error {
	script := sqliteV005
	if db.Dialector.Name() == "mysql" {
		script = mysqlV005
	}
	for _, statement := range splitSQL(script) {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	for _, column := range []string{"id", "user_id", "device_id", "first_message_id", "last_message_id"} {
		if !db.Migrator().HasColumn("message_device_ack_ranges", column) {
			return fmt.Errorf("v005 required column message_device_ack_ranges.%s is missing", column)
		}
	}
	var columns []string
	var err error
	if db.Dialector.Name() == "mysql" {
		columns, err = mysqlIndexColumns(db, "message_device_ack_ranges", "ux_device_ack_range")
		if err == nil && len(columns) == 0 {
			if err = db.Exec("CREATE UNIQUE INDEX ux_device_ack_range ON message_device_ack_ranges(user_id,device_id,first_message_id)").Error; err == nil {
				columns, err = mysqlIndexColumns(db, "message_device_ack_ranges", "ux_device_ack_range")
			}
		}
	} else {
		columns, err = sqliteIndexColumns(db, "ux_device_ack_range")
	}
	if err != nil {
		return err
	}
	if !slices.Equal(columns, []string{"user_id", "device_id", "first_message_id"}) {
		return fmt.Errorf("v005 index ux_device_ack_range columns differ: %v", columns)
	}
	unique, err := indexUnique(db, "message_device_ack_ranges", "ux_device_ack_range")
	if err != nil {
		return err
	}
	if !unique {
		return errors.New("v005 index ux_device_ack_range must be unique")
	}
	return nil
}

// Completed jobs already have durable inbox rows. This idempotent backfill is
// safe to retry after an interrupted DDL migration.
func applyV004(db *gorm.DB) error {
	script := sqliteV004
	if db.Dialector.Name() == "mysql" {
		script = mysqlV004
	}
	statements := splitSQL(script)
	if len(statements) != 2 {
		return errors.New("invalid v004 SQL snapshot")
	}
	if !db.Migrator().HasColumn(&platform.Video{}, "FeedPull") {
		if err := db.Exec(statements[0]).Error; err != nil {
			return err
		}
	}
	if err := db.Exec("UPDATE videos SET feed_pull = ? WHERE id IN (SELECT video_id FROM feed_fanout_jobs WHERE done_at IS NOT NULL)", false).Error; err != nil {
		return err
	}
	var columns []string
	var err error
	if db.Dialector.Name() == "mysql" {
		columns, err = mysqlIndexColumns(db, "videos", "idx_video_feed_pull")
	} else {
		columns, err = sqliteIndexColumns(db, "idx_video_feed_pull")
	}
	if err != nil {
		return err
	}
	if len(columns) == 0 {
		if err := db.Exec(statements[1]).Error; err != nil {
			return err
		}
		if db.Dialector.Name() == "mysql" {
			columns, err = mysqlIndexColumns(db, "videos", "idx_video_feed_pull")
		} else {
			columns, err = sqliteIndexColumns(db, "idx_video_feed_pull")
		}
		if err != nil {
			return err
		}
	}
	if !slices.Equal(columns, []string{"status", "feed_pull", "created_at", "id"}) {
		return fmt.Errorf("v004 index idx_video_feed_pull columns differ: %v", columns)
	}
	return nil
}

func applyV003(db *gorm.DB) error {
	indexes := map[string]struct {
		table   string
		columns []string
		mysql   string
	}{
		"idx_feed_fanout_jobs_video_id": {"feed_fanout_jobs", []string{"video_id"}, "CREATE UNIQUE INDEX `idx_feed_fanout_jobs_video_id` ON `feed_fanout_jobs` (`video_id`)"},
		"idx_feed_fanout_dispatch":      {"feed_fanout_jobs", []string{"done_at", "available_at", "lease_until", "id"}, "CREATE INDEX `idx_feed_fanout_dispatch` ON `feed_fanout_jobs` (`done_at`,`available_at`,`lease_until`,`id`)"},
		"ux_feed_inbox":                 {"feed_inboxes", []string{"user_id", "video_id"}, "CREATE UNIQUE INDEX `ux_feed_inbox` ON `feed_inboxes` (`user_id`,`video_id`)"},
		"idx_feed_inbox_user":           {"feed_inboxes", []string{"user_id", "created_at", "video_id"}, "CREATE INDEX `idx_feed_inbox_user` ON `feed_inboxes` (`user_id`,`created_at` DESC,`video_id` DESC)"},
	}
	script := sqliteV003
	if db.Dialector.Name() == "mysql" {
		script = mysqlV003
	}
	for _, statement := range splitSQL(script) {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("%s: %w", shortStatement(statement), err)
		}
	}
	for table, columns := range map[string][]string{
		"feed_fanout_jobs": {"id", "video_id", "author_id", "last_follower_id", "attempts", "available_at", "lease_until", "done_at", "last_error", "created_at"},
		"feed_inboxes":     {"id", "user_id", "video_id", "created_at"},
	} {
		for _, column := range columns {
			if !db.Migrator().HasColumn(table, column) {
				return fmt.Errorf("v003 required column %s.%s is missing", table, column)
			}
		}
	}
	for name, spec := range indexes {
		var columns []string
		var err error
		if db.Dialector.Name() == "mysql" {
			columns, err = mysqlIndexColumns(db, spec.table, name)
			if err == nil && len(columns) == 0 {
				if err = db.Exec(spec.mysql).Error; err == nil {
					columns, err = mysqlIndexColumns(db, spec.table, name)
				}
			}
		} else {
			columns, err = sqliteIndexColumns(db, name)
		}
		if err != nil {
			return err
		}
		if !slices.Equal(columns, spec.columns) {
			return fmt.Errorf("v003 index %s columns differ: %v", name, columns)
		}
		if name == "idx_feed_fanout_jobs_video_id" || name == "ux_feed_inbox" {
			unique, err := indexUnique(db, spec.table, name)
			if err != nil {
				return err
			}
			if !unique {
				return fmt.Errorf("v003 index %s must be unique", name)
			}
		}
	}
	return nil
}

func indexUnique(db *gorm.DB, table, name string) (bool, error) {
	if db.Dialector.Name() == "mysql" {
		var nonUnique int
		err := db.Raw("SELECT MIN(non_unique) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?", table, name).Scan(&nonUnique).Error
		return nonUnique == 0, err
	}
	rows, err := db.Raw("PRAGMA index_list(`" + table + "`)").Rows()
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq, unique, partial int
		var indexName, origin string
		if err := rows.Scan(&seq, &indexName, &unique, &origin, &partial); err != nil {
			return false, err
		}
		if indexName == name {
			return unique == 1, nil
		}
	}
	return false, rows.Err()
}

func applyV002(db *gorm.DB) error {
	script := sqliteV002
	if db.Dialector.Name() == "mysql" {
		script = mysqlV002
	}
	statements := splitSQL(script)
	if len(statements) != 1 {
		return errors.New("invalid v002 SQL snapshot")
	}
	statement := statements[0]
	if db.Dialector.Name() == "mysql" {
		count, err := mysqlIndexCount(db, "outbox_events", "idx_outbox_dispatch")
		if err != nil {
			return err
		}
		if count == 0 {
			if err := db.Exec(statement).Error; err != nil {
				return err
			}
		}
	} else {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	if db.Dialector.Name() == "mysql" {
		columns, err := mysqlIndexColumns(db, "outbox_events", "idx_outbox_dispatch")
		if err != nil {
			return err
		}
		if !slices.Equal(columns, []string{"published_at", "dead_at", "available_at", "id"}) {
			return fmt.Errorf("v002 MySQL outbox index columns differ: %v", columns)
		}
	} else {
		columns, err := sqliteOutboxIndexColumns(db)
		if err != nil {
			return err
		}
		if !slices.Equal(columns, []string{"published_at", "dead_at", "available_at", "id"}) {
			return fmt.Errorf("v002 SQLite outbox index columns differ: %v", columns)
		}
	}
	return nil
}

func sqliteOutboxIndexColumns(db *gorm.DB) ([]string, error) {
	return sqliteIndexColumns(db, "idx_outbox_dispatch")
}

func sqliteIndexColumns(db *gorm.DB, name string) ([]string, error) {
	rows, err := db.Raw("PRAGMA index_info(`" + name + "`)").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var seq, cid int
		var column string
		if err := rows.Scan(&seq, &cid, &column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func mysqlIndexColumns(db *gorm.DB, table, index string) ([]string, error) {
	rows, err := db.Raw("SELECT column_name FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ? ORDER BY seq_in_index", table, index).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func baselineSQL(driver string) (string, error) {
	switch driver {
	case "sqlite":
		return sqliteV001, nil
	case "mysql":
		return mysqlV001, nil
	default:
		return "", fmt.Errorf("unsupported migration driver %q", driver)
	}
}

func createMigrationRegistry(db *gorm.DB, script string) error {
	for _, statement := range splitSQL(script) {
		if strings.HasPrefix(statement, "CREATE TABLE IF NOT EXISTS `schema_migrations` ") {
			return db.Exec(statement).Error
		}
	}
	return errors.New("v001 snapshot has no migration registry")
}

// The v001 files contain only generated DDL without semicolons in literals.
// Remove their comment lines before splitting individual statements.
func splitSQL(script string) []string {
	var body strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	var statements []string
	for _, statement := range strings.Split(body.String(), ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}

func applyV001(db *gorm.DB, script string) error {
	for _, statement := range splitSQL(script) {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("%s: %w", shortStatement(statement), err)
		}
	}
	if db.Dialector.Name() == "mysql" {
		for _, statement := range splitSQL(mysqlV001Indexes) {
			match := mysqlIndexStatement.FindStringSubmatch(statement)
			if match == nil {
				return fmt.Errorf("invalid v001 MySQL index statement: %s", shortStatement(statement))
			}
			count, err := mysqlIndexCount(db, match[2], match[1])
			if err != nil {
				return err
			}
			if count == 0 {
				if err := db.Exec(statement).Error; err != nil {
					return fmt.Errorf("restore v001 index %s: %w", match[1], err)
				}
			}
		}
	}
	if err := validateV001Columns(db); err != nil {
		return err
	}
	if db.Dialector.Name() == "sqlite" {
		for _, statement := range splitSQL(script) {
			match := sqliteIndexStatement.FindStringSubmatch(statement)
			if match == nil {
				continue
			}
			var count int64
			if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ? AND tbl_name = ?", match[1], match[2]).Scan(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return fmt.Errorf("v001 SQLite index %s is missing from %s", match[1], match[2])
			}
		}
	}
	return nil
}

func mysqlIndexCount(db *gorm.DB, table, index string) (int64, error) {
	var count int64
	err := db.Raw("SELECT count(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?", table, index).Scan(&count).Error
	return count, err
}

func validateV001Columns(db *gorm.DB) error {
	var required map[string][]string
	if err := json.Unmarshal(v001Columns, &required); err != nil {
		return fmt.Errorf("decode frozen v001 column manifest: %w", err)
	}
	for table, names := range required {
		columns, err := db.Migrator().ColumnTypes(table)
		if err != nil {
			return fmt.Errorf("read columns of %s: %w", table, err)
		}
		present := make(map[string]bool, len(columns))
		for _, column := range columns {
			present[column.Name()] = true
		}
		for _, name := range names {
			if !present[name] {
				return fmt.Errorf("v001 schema drift: required column %s.%s is missing", table, name)
			}
		}
	}
	return nil
}

func shortStatement(statement string) string {
	line, _, _ := strings.Cut(statement, "\n")
	if len(line) > 90 {
		return line[:90] + "..."
	}
	return line
}

// MySQL DDL is not transactional. Serialize API starts and keep the marker
// absent until the idempotent baseline has finished, allowing a retry after
// an interrupted migration.
func withMySQLLock(ctx context.Context, db *gorm.DB, run func() error) error {
	raw, err := db.DB()
	if err != nil {
		return err
	}
	if raw.Stats().MaxOpenConnections == 1 {
		return errors.New("schema migration needs at least two MySQL connections")
	}
	conn, err := raw.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 30)", mysqlLockName).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire MySQL migration lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return errors.New("timed out waiting for MySQL migration lock")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", mysqlLockName)
	}()
	return run()
}
