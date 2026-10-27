package platform

import (
	"context"
	"errors"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func TestTransactionWithDeadlockRetry(t *testing.T) {
	_, _, db := testServer(t)
	attempts := 0
	err := transactionWithDeadlockRetry(context.Background(), db, func(_ *gorm.DB) error {
		attempts++
		if attempts < 3 {
			return &mysqlDriver.MySQLError{Number: 1213, Message: "deadlock"}
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("deadlock retry: attempts=%d err=%v", attempts, err)
	}
	attempts = 0
	want := errors.New("permanent error")
	err = transactionWithDeadlockRetry(context.Background(), db, func(_ *gorm.DB) error {
		attempts++
		return want
	})
	if !errors.Is(err, want) || attempts != 1 {
		t.Fatalf("non-retryable error: attempts=%d err=%v", attempts, err)
	}
}
