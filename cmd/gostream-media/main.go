// gostream-media handles offline media copy and object backup/restore.
// Stop application writers and snapshot MySQL separately before a backup.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
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
	if len(args) == 0 {
		return errors.New("usage: gostream-media import-local|backup|restore --dir PATH")
	}
	command := args[0]
	if command != "import-local" && command != "backup" && command != "restore" {
		return fmt.Errorf("unsupported media command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	dir := flags.String("dir", "", "local media directory for import; new backup directory or existing backup for restore")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *dir == "" || flags.NArg() != 0 {
		return errors.New("exactly one --dir PATH is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	secure := os.Getenv("MINIO_SECURE") == "true"
	storeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	openStore := platform.NewMinIOMediaStore
	if command == "backup" {
		openStore = platform.OpenExistingMinIOMediaStore
	}
	store, err := openStore(storeCtx, os.Getenv("MINIO_ENDPOINT"), os.Getenv("MINIO_ACCESS_KEY"), os.Getenv("MINIO_SECRET_KEY"), os.Getenv("MINIO_BUCKET"), secure)
	cancel()
	if err != nil {
		return err
	}
	var report platform.MediaTransferReport
	switch command {
	case "import-local":
		var db *gorm.DB
		switch os.Getenv("DB_DRIVER") {
		case "mysql":
			if os.Getenv("DB_DSN") == "" {
				return errors.New("DB_DSN is required for import-local")
			}
			db, err = gorm.Open(gormmysql.Open(os.Getenv("DB_DSN")), &gorm.Config{})
		case "sqlite":
			if os.Getenv("DB_DSN") == "" {
				return errors.New("DB_DSN is required for import-local")
			}
			db, err = gorm.Open(gormsqlite.Open(os.Getenv("DB_DSN")), &gorm.Config{})
		default:
			return errors.New("DB_DRIVER must be mysql or sqlite for import-local")
		}
		if err != nil {
			return fmt.Errorf("connect database: %w", err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		defer sqlDB.Close()
		report, err = platform.ImportLocalMedia(ctx, db, *dir, store)
	case "backup":
		report, err = platform.BackupMediaObjects(ctx, store, *dir)
	case "restore":
		report, err = platform.RestoreMediaObjects(ctx, store, *dir)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
