package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	gormmysql "gorm.io/driver/mysql"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"github.com/liuzhouquan/GoStream/internal/platform"
)

func main() {
	addr := env("ADDR", ":8080")
	secret := env("JWT_SECRET", "local-development-secret-change-me-please")
	if len(secret) < 32 {
		log.Fatal("JWT_SECRET must contain at least 32 characters")
	}
	mediaDir := env("MEDIA_DIR", "data/media")
	driver := env("DB_DRIVER", "sqlite")
	dsn := os.Getenv("DB_DSN")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		log.Fatal(err)
	}
	if driver == "sqlite" {
		if dsn == "" {
			if err := os.MkdirAll("data", 0755); err != nil {
				log.Fatal(err)
			}
			dsn = "data/gostream.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
		}
	} else if dsn == "" {
		log.Fatal("DB_DSN is required for mysql")
	}
	var dialector gorm.Dialector
	switch driver {
	case "sqlite":
		dialector = gormsqlite.Open(dsn)
	case "mysql":
		dialector = gormmysql.Open(dsn)
	default:
		log.Fatalf("unsupported DB_DRIVER %q", driver)
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	if err = db.AutoMigrate(&platform.User{}, &platform.Video{}, &platform.Follow{}, &platform.VideoAction{}, &platform.Comment{}, &platform.Notification{}, &platform.ChatMessage{}, &platform.OutboxEvent{}); err != nil {
		log.Fatalf("migrate database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()
	server := platform.NewServer(db, secret, mediaDir)
	log.Printf("GoStream listening at %s (db=%s)", addr, driver)
	httpServer := &http.Server{Addr: addr, Handler: server.Router(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err = <-serveErr:
		log.Fatal(err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
