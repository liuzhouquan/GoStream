package platform

import (
	"context"
	"errors"
	"math/rand"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

const maxDeadlockRetries = 10

// MySQL can abort one of two valid concurrent transactions with error 1213.
// Retry the whole transaction so the caller re-reads the current row state.
func transactionWithDeadlockRetry(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	for attempt := 0; ; attempt++ {
		err := db.WithContext(ctx).Transaction(fn)
		var mysqlErr *mysqlDriver.MySQLError
		if err == nil || attempt == maxDeadlockRetries || !errors.As(err, &mysqlErr) || mysqlErr.Number != 1213 {
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
