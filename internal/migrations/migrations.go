package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liuzhouquan/GoStream/internal/platform"
	"gorm.io/gorm"
)

const baselineKey = "schema_v001"
const mysqlLockName = "gostream_schema_migrations"

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
	if err := db.AutoMigrate(&platform.SchemaMigration{}); err != nil {
		return fmt.Errorf("create schema migration registry: %w", err)
	}
	var records []platform.SchemaMigration
	if err := db.Find(&records).Error; err != nil {
		return fmt.Errorf("read schema versions: %w", err)
	}
	baselineApplied := false
	for _, record := range records {
		if strings.HasPrefix(record.Key, "schema_v") && record.Key != baselineKey {
			return fmt.Errorf("unsupported schema version %q; start a compatible GoStream binary", record.Key)
		}
		if record.Key == baselineKey {
			baselineApplied = true
		}
	}
	if baselineApplied {
		return nil
	}
	if err := platform.MigrateSchema(db); err != nil {
		return fmt.Errorf("apply %s: %w", baselineKey, err)
	}
	if err := db.Create(&platform.SchemaMigration{Key: baselineKey, AppliedAt: time.Now().UTC()}).Error; err != nil {
		return fmt.Errorf("record %s: %w", baselineKey, err)
	}
	return nil
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
