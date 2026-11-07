package platform

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

const maxDeadlockRetries = 10

func isRetryableDBLock(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 1213 || mysqlErr.Number == 1205) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "busy")
}

// MySQL can abort one of two valid concurrent transactions with error 1213,
// and SQLite returns busy/locked under concurrent write transactions.
// Retry the whole transaction so the caller re-reads the current row state.
func transactionWithDeadlockRetry(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	for attempt := 0; ; attempt++ {
		err := db.WithContext(ctx).Transaction(fn)
		if err == nil || attempt == maxDeadlockRetries || !isRetryableDBLock(err) {
			return err
		}
		// Equal jitter spreads concurrent deadlock victims across the retry window.
		baseDelay := time.Duration(1<<attempt) * 5 * time.Millisecond
		halfDelay := baseDelay / 2
		delay := halfDelay + time.Duration(rand.Int63n(int64(halfDelay)+1))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
