package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const feedCacheTTL = 5 * time.Second

type redisFeedCache struct {
	client        *redis.Client
	mu            sync.Mutex
	disabledUntil time.Time
}

func (s *Server) UseRedisFeedCache(client *redis.Client) { s.cache = &redisFeedCache{client: client} }

func (r *redisFeedCache) allowed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !time.Now().Before(r.disabledUntil)
}

func (r *redisFeedCache) trip() {
	r.mu.Lock()
	r.disabledUntil = time.Now().Add(5 * time.Second)
	r.mu.Unlock()
}

func (r *redisFeedCache) key(version, scene string, limit int, cursor string) string {
	digest := sha256.Sum256([]byte(cursor))
	return fmt.Sprintf("gostream:feed:%s:%s:%d:%x", version, scene, limit, digest)
}

func (r *redisFeedCache) load(ctx context.Context, scene string, limit int, cursor string) ([]byte, string, error) {
	if !r.allowed() {
		return nil, "", errors.New("Redis cache circuit open")
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	version, err := r.client.Get(ctx, "gostream:feed:version").Result()
	if errors.Is(err, redis.Nil) {
		version, err = "0", nil
	}
	if err != nil {
		r.trip()
		return nil, "", err
	}
	value, err := r.client.Get(ctx, r.key(version, scene, limit, cursor)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, version, nil
	}
	if err != nil {
		r.trip()
		return nil, "", err
	}
	return value, version, nil
}

func (r *redisFeedCache) store(ctx context.Context, version, scene string, limit int, cursor string, value []byte) error {
	if !r.allowed() {
		return errors.New("Redis cache circuit open")
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := r.client.Set(ctx, r.key(version, scene, limit, cursor), value, feedCacheTTL).Err(); err != nil {
		r.trip()
		return err
	}
	return nil
}

func (r *redisFeedCache) bump(ctx context.Context) error {
	if !r.allowed() {
		return errors.New("Redis cache circuit open")
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := r.client.Incr(ctx, "gostream:feed:version").Err(); err != nil {
		r.trip()
		return err
	}
	return nil
}

func (s *Server) invalidateFeed(ctx context.Context) {
	if s.cache == nil {
		return
	}
	if err := s.cache.bump(ctx); err != nil {
		s.metrics.feedCache.WithLabelValues("invalidate_error").Inc()
	}
}
