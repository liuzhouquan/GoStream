package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/liuzhouquan/GoStream/internal/platform"
	"github.com/redis/go-redis/v9"
	gormmysql "gorm.io/driver/mysql"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
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
	if err = db.AutoMigrate(&platform.User{}, &platform.MediaAsset{}, &platform.UploadSession{}, &platform.UploadChunk{}, &platform.Video{}, &platform.VideoProcessJob{}, &platform.Follow{}, &platform.VideoAction{}, &platform.Comment{}, &platform.Notification{}, &platform.ChatMessage{}, &platform.ChatGroup{}, &platform.GroupMember{}, &platform.GroupMessage{}, &platform.GroupRead{}, &platform.OutboxEvent{}); err != nil {
		log.Fatalf("migrate database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()
	server := platform.NewServer(db, secret, mediaDir)
	if ffmpegBin := os.Getenv("FFMPEG_BIN"); ffmpegBin != "" {
		if _, err := exec.LookPath(ffmpegBin); err != nil {
			log.Fatalf("FFMPEG_BIN is unavailable: %v", err)
		}
		ffprobeBin := env("FFPROBE_BIN", "ffprobe")
		if _, err := exec.LookPath(ffprobeBin); err != nil {
			log.Fatalf("FFPROBE_BIN is unavailable: %v", err)
		}
		server.UseFFmpeg(ffmpegBin)
		server.UseFFprobe(ffprobeBin)
	}
	if redisAddr := os.Getenv("REDIS_ADDR"); redisAddr != "" {
		client := redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 500 * time.Millisecond, ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond, MaxRetries: 0})
		defer client.Close()
		server.UseRedisFeedCache(client)
		if err := server.UseRedisRealtime(client); err != nil {
			log.Fatalf("initialize realtime bridge: %v", err)
		}
		pingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := client.Ping(pingCtx).Err(); err != nil {
			log.Printf("Redis unavailable at startup; Feed will use database reads: %v", err)
		}
		cancel()
	}
	if pprofAddr := os.Getenv("PPROF_ADDR"); pprofAddr != "" {
		go func() {
			log.Printf("pprof listening at %s", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				log.Printf("pprof server stopped: %v", err)
			}
		}()
	}
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
	go server.RunOutbox(ctx)
	go server.RunUploadCleanup(ctx)
	go server.RunVideoProcessing(ctx)
	go server.RunRealtimeBridge(ctx)
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
