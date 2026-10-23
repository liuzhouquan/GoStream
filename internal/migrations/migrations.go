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
	for _, record := range records {
		switch record.Key {
		case baselineKey:
			baselineApplied = true
		case upgradeV002Key:
			upgradeApplied = true
		default:
			if strings.HasPrefix(record.Key, "schema_v") {
				return fmt.Errorf("unsupported schema version %q; start a compatible GoStream binary", record.Key)
			}
		}
	}
	if upgradeApplied && !baselineApplied {
		return errors.New("schema_v002 is recorded without schema_v001")
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
	return nil
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
	rows, err := db.Raw("PRAGMA index_info(`idx_outbox_dispatch`)").Rows()
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
