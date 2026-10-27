package platform

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func publicRequest(t *testing.T, h http.Handler, method, path, remote, forwarded string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("X-Forwarded-For", forwarded)
	req.Header.Set("X-Real-IP", forwarded)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func TestPublicRouteBudgetsUsePeerAddress(t *testing.T) {
	s, h, db := testServer(t)
	s.rateLimiter.now = func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) }
	peer := "198.51.100.10:1234"
	for i := 0; i < 10; i++ {
		body := fmt.Sprintf(`{"username":"budget%d","password":"correct-horse-123"}`, i)
		res := publicRequest(t, h, http.MethodPost, "/api/users", peer, fmt.Sprintf("203.0.113.%d", i+1), body)
		if res.Code != http.StatusCreated {
			t.Fatalf("registration %d: %d %s", i, res.Code, res.Body.String())
		}
	}
	blocked := publicRequest(t, h, http.MethodPost, "/api/users", peer, "203.0.113.99", `{"username":"budgetoverflow","password":"correct-horse-123"}`)
	retrySeconds, parseErr := strconv.Atoi(blocked.Header().Get("Retry-After"))
	if blocked.Code != http.StatusTooManyRequests || parseErr != nil || retrySeconds < 1 || retrySeconds > 60 {
		t.Fatalf("registration budget: %d retry-after=%q", blocked.Code, blocked.Header().Get("Retry-After"))
	}
	if res := publicRequest(t, h, http.MethodPost, "/api/users", "198.51.100.11:1234", "", `{"username":"otherpeer","password":"correct-horse-123"}`); res.Code != http.StatusCreated {
		t.Fatalf("other peer registration: %d %s", res.Code, res.Body.String())
	}
	loginBody := `{"username":"budget0","password":"wrong-password"}`
	for i := 0; i < 30; i++ {
		res := publicRequest(t, h, http.MethodPost, "/api/sessions", peer, "", loginBody)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("login attempt %d: %d", i, res.Code)
		}
	}
	if res := publicRequest(t, h, http.MethodPost, "/api/sessions", peer, "", loginBody); res.Code != http.StatusTooManyRequests {
		t.Fatalf("login budget: %d", res.Code)
	}
	if res := publicRequest(t, h, http.MethodPost, "/api/sessions", "198.51.100.11:1234", "", `{"username":"budget0","password":"correct-horse-123"}`); res.Code != http.StatusOK {
		t.Fatalf("other peer login: %d %s", res.Code, res.Body.String())
	}
	var author User
	if err := db.Where("username = ?", "budget0").First(&author).Error; err != nil {
		t.Fatal(err)
	}
	video := Video{AuthorID: author.ID, Title: "Count accepted requests", MediaURL: "/media/budget.mp4", Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/videos/" + strconv.FormatUint(uint64(video.ID), 10) + "/views"
	for i := 0; i < 120; i++ {
		if res := publicRequest(t, h, http.MethodPost, path, peer, "", ""); res.Code != http.StatusNoContent {
			t.Fatalf("accepted view %d: %d", i, res.Code)
		}
	}
	if res := publicRequest(t, h, http.MethodPost, path, peer, "", ""); res.Code != http.StatusTooManyRequests {
		t.Fatalf("view budget: %d", res.Code)
	}
	if err := db.First(&video, video.ID).Error; err != nil || video.PlayCount != 120 {
		t.Fatalf("accepted view count = %d, err=%v", video.PlayCount, err)
	}
	if res := publicRequest(t, h, http.MethodPost, path, "198.51.100.11:1234", "", ""); res.Code != http.StatusNoContent {
		t.Fatalf("other peer view: %d", res.Code)
	}
	metrics := request(t, h, http.MethodGet, "/metrics", "", nil).Body.String()
	if !strings.Contains(metrics, `gostream_rate_limit_total{result="limited",route="register"}`) ||
		!strings.Contains(metrics, `gostream_rate_limit_total{result="limited",route="view"}`) {
		t.Fatal("rate-limit metrics missing bounded route results")
	}
}

func TestRateLimiterRedisAtomicAndLocalFallback(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer redisServer.Close()
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), MaxRetries: 0})
	defer client.Close()
	first := newRateLimiter(client)
	second := newRateLimiter(client)
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	fixed := func() time.Time { return clock }
	first.now, second.now = fixed, fixed
	var allowed atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			limiter := first
			if i%2 == 1 {
				limiter = second
			}
			ok, _ := limiter.allow(context.Background(), "login", "198.51.100.30", 7)
			if ok {
				allowed.Add(1)
			}
		}(i)
	}
	group.Wait()
	if allowed.Load() != 7 {
		t.Fatalf("Redis global budget allowed %d, want 7", allowed.Load())
	}
	key := first.key("login", "198.51.100.30", fixed())
	if stored, err := client.Get(context.Background(), key).Int(); err != nil || stored != 7 {
		t.Fatalf("Redis stored count after denied burst = %d, err=%v", stored, err)
	}
	firstServer, firstRouter, db := testServer(t)
	secondServer := NewServer(db, "test-signing-secret-at-least-32-bytes", t.TempDir())
	secondRouter := secondServer.Router()
	firstServer.UseRedisRateLimiter(client)
	secondServer.UseRedisRateLimiter(client)
	firstServer.rateLimiter.now, secondServer.rateLimiter.now = fixed, fixed
	for i := 0; i < 30; i++ {
		h := firstRouter
		if i%2 == 1 {
			h = secondRouter
		}
		res := publicRequest(t, h, http.MethodPost, "/api/sessions", "198.51.100.40:1234", "", `{"username":"missing","password":"wrong-password"}`)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("cross-instance login %d: %d", i, res.Code)
		}
	}
	if res := publicRequest(t, secondRouter, http.MethodPost, "/api/sessions", "198.51.100.40:1234", "", `{"username":"missing","password":"wrong-password"}`); res.Code != http.StatusTooManyRequests {
		t.Fatalf("cross-instance login budget: %d", res.Code)
	}
	redisServer.Close()
	if res := publicRequest(t, firstRouter, http.MethodPost, "/api/sessions", "198.51.100.41:1234", "", `{"username":"missing","password":"wrong-password"}`); res.Code != http.StatusUnauthorized {
		t.Fatalf("Redis outage route fallback: %d", res.Code)
	}
	metrics := request(t, firstRouter, http.MethodGet, "/metrics", "", nil).Body.String()
	if !strings.Contains(metrics, `gostream_rate_limit_total{result="fallback_allowed",route="login"}`) {
		t.Fatal("Redis outage fallback metric missing")
	}
	if ok, mode := first.allow(context.Background(), "register", "198.51.100.31", 2); !ok || mode != "fallback" {
		t.Fatalf("first local fallback: allowed=%v mode=%q", ok, mode)
	}
	if ok, mode := first.allow(context.Background(), "register", "198.51.100.31", 2); !ok || mode != "fallback" {
		t.Fatalf("second local fallback: allowed=%v mode=%q", ok, mode)
	}
	if ok, mode := first.allow(context.Background(), "register", "198.51.100.31", 2); ok || mode != "fallback" {
		t.Fatalf("fallback budget exceeded: allowed=%v mode=%q", ok, mode)
	}
	if err := redisServer.Restart(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(6 * time.Second)
	if ok, mode := first.allow(context.Background(), "register", "198.51.100.32", 2); !ok || mode != "redis" {
		t.Fatalf("Redis recovery: allowed=%v mode=%q", ok, mode)
	}
}

func TestRateLimiterLocalCapacityAndWindowRecovery(t *testing.T) {
	limiter := newRateLimiter(nil)
	limiter.maxLocalKeys = 2
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return clock }
	for _, peer := range []string{"198.51.100.1", "198.51.100.2"} {
		if ok, mode := limiter.allow(context.Background(), "login", peer, 1); !ok || mode != "local" {
			t.Fatalf("initial local budget for %s: allowed=%v mode=%q", peer, ok, mode)
		}
	}
	if ok, _ := limiter.allow(context.Background(), "login", "198.51.100.3", 1); ok {
		t.Fatal("local key cap admitted a third client")
	}
	if ok, _ := limiter.allow(context.Background(), "login", "198.51.100.1", 1); ok {
		t.Fatal("existing client exceeded its local budget")
	}
	clock = clock.Add(time.Minute)
	if ok, _ := limiter.allow(context.Background(), "login", "198.51.100.3", 1); !ok {
		t.Fatal("expired local keys were not reclaimed")
	}
}

func TestRateLimiterWindowRollover(t *testing.T) {
	limiter := newRateLimiter(nil)
	clock := time.Date(2026, 10, 27, 12, 0, 59, 0, time.UTC)
	limiter.now = func() time.Time { return clock }
	if ok, _ := limiter.allow(context.Background(), "register", "198.51.100.50", 1); !ok {
		t.Fatal("first request was denied")
	}
	if ok, _ := limiter.allow(context.Background(), "register", "198.51.100.50", 1); ok {
		t.Fatal("second request in same minute was admitted")
	}
	clock = clock.Add(time.Second)
	if ok, _ := limiter.allow(context.Background(), "register", "198.51.100.50", 1); !ok {
		t.Fatal("new minute did not reset the budget")
	}
}

func TestRateLimiterRealRedisIntegration(t *testing.T) {
	addr := os.Getenv("GOSTREAM_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set GOSTREAM_TEST_REDIS_ADDR to an isolated Redis instance")
	}
	client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: 0})
	defer client.Close()
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	first, second := newRateLimiter(client), newRateLimiter(client)
	fixed := func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) }
	first.now, second.now = fixed, fixed
	peer := fmt.Sprintf("isolated-redis-test-%d", time.Now().UnixNano())
	key := first.key("integration", peer, fixed())
	defer client.Del(context.Background(), key)
	var accepted atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			limiter := first
			if i%2 == 1 {
				limiter = second
			}
			ok, _ := limiter.allow(context.Background(), "integration", peer, 5)
			if ok {
				accepted.Add(1)
			}
		}(i)
	}
	group.Wait()
	if accepted.Load() != 5 {
		t.Fatalf("real Redis admitted %d requests, want 5", accepted.Load())
	}
	if stored, err := client.Get(context.Background(), key).Int(); err != nil || stored != 5 {
		t.Fatalf("real Redis stored count after denied burst = %d, err=%v", stored, err)
	}
}
