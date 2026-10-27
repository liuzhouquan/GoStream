package platform

import (
	"context"
	"errors"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// MySQL can abort one of two valid concurrent transactions with error 1213.
// Retry the whole transaction so the caller re-reads the current row state.
func transactionWithDeadlockRetry(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	for attempt := 0; ; attempt++ {
		err := db.WithContext(ctx).Transaction(fn)
		var mysqlErr *mysqlDriver.MySQLError
		if err == nil || attempt == 4 || !errors.As(err, &mysqlErr) || mysqlErr.Number != 1213 {
			return err
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * 5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
