package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/liuzhouquan/GoStream/internal/migrations"
	"github.com/liuzhouquan/GoStream/internal/platform"
	"github.com/redis/go-redis/v9"
	gormmysql "gorm.io/driver/mysql"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func main() {
	addr := env("ADDR", "127.0.0.1:8080")
	secret := env("JWT_SECRET", localDevelopmentJWTSecret)
	if err := validateJWTSecret(addr, secret); err != nil {
		log.Fatal(err)
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
			dsn = sqliteDSN("data/gostream.db")
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
	if err = migrations.Apply(context.Background(), db); err != nil {
		log.Fatalf("migrate database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()
	server := platform.NewServer(db, secret, mediaDir)
	if brokerURL := os.Getenv("RABBITMQ_URL"); brokerURL != "" {
		if err := server.UseRabbitMQ(brokerURL, env("RABBITMQ_QUEUE", "gostream.notifications.v1")); err != nil {
			log.Fatal(err)
		}
		defer server.CloseRabbitMQ()
	}
	fanoutMaxFollowers, err := strconv.Atoi(env("FEED_FANOUT_MAX_FOLLOWERS", "0"))
	if err != nil || fanoutMaxFollowers < 0 || fanoutMaxFollowers > 100000 {
		log.Fatal("FEED_FANOUT_MAX_FOLLOWERS must be an integer from 0 to 100000")
	}
	server.UseFeedFanout(fanoutMaxFollowers)
	switch mediaStore := env("MEDIA_STORE", "local"); mediaStore {
	case "local":
	case "minio":
		secure := env("MINIO_SECURE", "false") == "true"
		storeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		store, err := platform.NewMinIOMediaStore(storeCtx, os.Getenv("MINIO_ENDPOINT"), os.Getenv("MINIO_ACCESS_KEY"), os.Getenv("MINIO_SECRET_KEY"), os.Getenv("MINIO_BUCKET"), secure)
		cancel()
		if err != nil {
			log.Fatalf("initialize media storage: %v", err)
		}
		server.UseObjectMedia(store)
	default:
		log.Fatalf("unsupported MEDIA_STORE %q", mediaStore)
	}
	if err := server.BackfillMessageDevices(context.Background()); err != nil {
		log.Fatalf("backfill message devices: %v", err)
	}
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
		server.UseRedisRateLimiter(client)
		if err := server.UseRedisRealtime(client); err != nil {
			log.Fatalf("initialize realtime bridge: %v", err)
		}
		if leaseStr := os.Getenv("PRESENCE_LEASE"); leaseStr != "" {
			if d, err := time.ParseDuration(leaseStr); err == nil && d > 0 {
				renew := d / 2
				if renewStr := os.Getenv("PRESENCE_RENEW"); renewStr != "" {
					if r, err := time.ParseDuration(renewStr); err == nil && r > 0 {
						renew = r
					}
				}
				server.SetPresenceLease(d, renew)
			}
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
	httpServer := newAPIServer(addr, server.Router())
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var workers sync.WaitGroup
	for _, run := range []func(context.Context){server.RunOutbox, server.RunUploadCleanup, server.RunMessageDeviceCleanup, server.RunMediaReconciliation, server.RunVideoProcessing, server.RunFeedFanout, server.RunRealtimeBridge, server.RunRabbitMQ} {
		workers.Go(func() { run(ctx) })
	}
	defer func() { stop(); workers.Wait() }()
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

func newAPIServer(addr string, handler http.Handler) *http.Server {
	// Bound total time spent reading request headers and body, including uploads.
	// A 250 MiB direct upload gets ten minutes; resumable chunks are smaller.
	return &http.Server{
		Addr: addr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

const localDevelopmentJWTSecret = "local-development-secret-change-me-please"
const oldComposeJWTSecret = "local-development-secret-change-before-deploy"

func validateJWTSecret(addr, secret string) error {
	if len(secret) < 32 {
		return errors.New("JWT_SECRET must contain at least 32 characters")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid ADDR: %w", err)
	}
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if !loopback && (secret == localDevelopmentJWTSecret || secret == oldComposeJWTSecret) {
		return errors.New("JWT_SECRET must be a unique secret when ADDR is not loopback")
	}
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func sqliteDSN(path string) string {
	return path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
}
