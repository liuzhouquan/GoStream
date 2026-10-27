package platform

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	publicRateWindow  = time.Minute
	localRateMaxKeys  = 20000
	rateRedisTimeout  = 75 * time.Millisecond
	rateRedisCooldown = 5 * time.Second
)

var rateIncrement = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
if current >= tonumber(ARGV[1]) then
  return current + 1
end
local count = redis.call('INCR', KEYS[1])
if count == 1 or redis.call('TTL', KEYS[1]) < 0 then
  redis.call('EXPIRE', KEYS[1], 120)
end
return count
`)

type localRateCount struct {
	count     int
	expiresAt time.Time
}

type rateLimiter struct {
	mu            sync.Mutex
	client        *redis.Client
	disabledUntil time.Time
	local         map[string]localRateCount
	maxLocalKeys  int
	now           func() time.Time
}

func newRateLimiter(client *redis.Client) *rateLimiter {
	return &rateLimiter{client: client, local: make(map[string]localRateCount), maxLocalKeys: localRateMaxKeys, now: time.Now}
}

func (s *Server) UseRedisRateLimiter(client *redis.Client) {
	s.rateLimiter.mu.Lock()
	s.rateLimiter.client = client
	s.rateLimiter.mu.Unlock()
}

func peerIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	return ip.Unmap().String()
}

func (r *rateLimiter) key(route, peer string, now time.Time) string {
	digest := sha256.Sum256([]byte(peer))
	return fmt.Sprintf("gostream:rate:%s:%x:%d", route, digest, now.Unix()/int64(publicRateWindow.Seconds()))
}

func (r *rateLimiter) localAllow(key string, limit int, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, exists := r.local[key]
	if !exists {
		if len(r.local) >= r.maxLocalKeys {
			for stored, count := range r.local {
				if !now.Before(count.expiresAt) {
					delete(r.local, stored)
				}
			}
		}
		if len(r.local) >= r.maxLocalKeys {
			return false
		}
		entry.expiresAt = time.Unix((now.Unix()/int64(publicRateWindow.Seconds())+1)*int64(publicRateWindow.Seconds()), 0)
	}
	if entry.count >= limit {
		return false
	}
	entry.count++
	r.local[key] = entry
	return true
}

// allow uses a shared Redis window when available, with a bounded per-process
// counter as a fallback. The local counter shadows accepted Redis requests so
// a short Redis outage does not reset this process's already-used budget.
func (r *rateLimiter) allow(ctx context.Context, route, peer string, limit int) (bool, string) {
	now := r.now()
	key := r.key(route, peer, now)
	r.mu.Lock()
	client, disabled := r.client, now.Before(r.disabledUntil)
	r.mu.Unlock()
	if client != nil && !disabled {
		redisCtx, cancel := context.WithTimeout(ctx, rateRedisTimeout)
		count, err := rateIncrement.Run(redisCtx, client, []string{key}, limit).Int64()
		cancel()
		if err == nil {
			if count <= int64(limit) {
				_ = r.localAllow(key, limit, now)
				return true, "redis"
			}
			return false, "redis"
		}
		r.mu.Lock()
		r.disabledUntil = now.Add(rateRedisCooldown)
		r.mu.Unlock()
	}
	mode := "local"
	if client != nil {
		mode = "fallback"
	}
	return r.localAllow(key, limit, now), mode
}

func (s *Server) limitPublicRoute(route string, budget int) gin.HandlerFunc {
	return func(c *gin.Context) {
		allowed, mode := s.rateLimiter.allow(c.Request.Context(), route, peerIP(c.Request.RemoteAddr), budget)
		result := "allowed"
		if !allowed {
			result = "limited"
		}
		if mode == "fallback" {
			result = "fallback_" + result
		}
		s.metrics.rateLimit.WithLabelValues(route, result).Inc()
		if !allowed {
			remaining := int64(publicRateWindow.Seconds()) - s.rateLimiter.now().Unix()%int64(publicRateWindow.Seconds())
			c.Header("Retry-After", strconv.FormatInt(remaining, 10))
			respondError(c, 429, "rate limit exceeded")
			c.Abort()
			return
		}
		c.Next()
	}
}
