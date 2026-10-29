package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/liuzhouquan/GoStream/internal/platform"
	gormmysql "gorm.io/driver/mysql"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("gostream-events", flag.ContinueOnError)
	key := flags.String("outbox-key", "", "requeue one dead unpublished SQL outbox event")
	limit := flags.Int("dead-letters", 0, "replay up to 1-1000 RabbitMQ dead letters after fixing their cause")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*key == "") == (*limit == 0) {
		return errors.New("choose exactly one: -outbox-key EVENT_KEY or -dead-letters LIMIT")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	server := platform.NewServer(nil, "", "")
	defer server.CloseRabbitMQ()
	if *limit != 0 {
		queue := os.Getenv("RABBITMQ_QUEUE")
		if queue == "" {
			queue = "gostream.notifications.v1"
		}
		if err := server.UseRabbitMQ(os.Getenv("RABBITMQ_URL"), queue); err != nil {
			return err
		}
		count, err := server.ReplayRabbitDeadLetters(ctx, *limit)
		if err != nil {
			return fmt.Errorf("replayed %d dead letters before failure: %w", count, err)
		}
		fmt.Printf("replayed=%d\n", count)
		return nil
	}
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		return errors.New("DB_DSN is required; use an existing migrated database")
	}
	var dialector gorm.Dialector
	switch os.Getenv("DB_DRIVER") {
	case "sqlite":
		dialector = gormsqlite.Open(dsn)
	case "mysql":
		dialector = gormmysql.Open(dsn)
	default:
		return errors.New("DB_DRIVER must be sqlite or mysql")
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return errors.New("could not connect to the configured database")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	server.DB = db
	if err := server.RequeueDeadOutbox(ctx, *key); err != nil {
		return err
	}
	fmt.Println("requeued=1")
	return nil
}
