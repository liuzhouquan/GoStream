package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestFollowingFeedCacheIsolatedAndInvalidated(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), MaxRetries: 0})
	t.Cleanup(func() { _ = client.Close() })
	s, h, db := testServer(t)
	s.UseRedisFeedCache(client)
	authorA, _ := registerTestUser(t, h, "follow_cache_author_a")
	authorB, _ := registerTestUser(t, h, "follow_cache_author_b")
	viewerX, tokenX := registerTestUser(t, h, "follow_cache_viewer_x")
	_, tokenY := registerTestUser(t, h, "follow_cache_viewer_y")
	now := time.Now().UTC()
	videoA := Video{AuthorID: authorA, Title: "author A", MediaURL: "/media/a.mp4", Status: "published", CreatedAt: now.Add(-3 * time.Minute)}
	videoB1 := Video{AuthorID: authorB, Title: "author B recent", MediaURL: "/media/b1.mp4", Status: "published", CreatedAt: now.Add(-time.Minute)}
	videoB2 := Video{AuthorID: authorB, Title: "author B older", MediaURL: "/media/b2.mp4", Status: "published", CreatedAt: now.Add(-2 * time.Minute)}
	for _, video := range []*Video{&videoA, &videoB1, &videoB2} {
		if err := db.Create(video).Error; err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/feed?scene=following"
	if res := request(t, h, http.MethodGet, path, "", nil); res.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous following feed: %d", res.Code)
	}
	if res := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorA), tokenX, nil); res.Code != http.StatusOK {
		t.Fatalf("viewer X follow A: %d %s", res.Code, res.Body.String())
	}
	if res := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorB), tokenY, nil); res.Code != http.StatusOK {
		t.Fatalf("viewer Y follow B: %d %s", res.Code, res.Body.String())
	}
	if ttl := redisServer.TTL(followingVersionKey(viewerX)); ttl <= 0 || ttl > followingVersionTTL {
		t.Fatalf("viewer following cache version TTL: %s", ttl)
	}
	firstX := request(t, h, http.MethodGet, path, tokenX, nil)
	firstY := request(t, h, http.MethodGet, path, tokenY, nil)
	if !pageHasOnly(firstX.Body.Bytes(), videoA.ID) || !pageHasOnly(firstY.Body.Bytes(), videoB1.ID, videoB2.ID) {
		t.Fatalf("cross-viewer following results: X=%s Y=%s", firstX.Body.String(), firstY.Body.String())
	}
	if err := db.Model(&videoA).Update("title", "A changed directly in database").Error; err != nil {
		t.Fatal(err)
	}
	cachedX := request(t, h, http.MethodGet, path, tokenX, nil)
	if !bytes.Equal(cachedX.Body.Bytes(), firstX.Body.Bytes()) {
		t.Fatalf("expected viewer X cache hit: %s", cachedX.Body.String())
	}
	if res := request(t, h, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorB), tokenX, nil); res.Code != http.StatusOK {
		t.Fatalf("viewer X follow B: %d %s", res.Code, res.Body.String())
	}
	afterFollow := request(t, h, http.MethodGet, path, tokenX, nil)
	if !pageHasOnly(afterFollow.Body.Bytes(), videoA.ID, videoB1.ID, videoB2.ID) || !bytes.Contains(afterFollow.Body.Bytes(), []byte("A changed directly in database")) {
		t.Fatalf("viewer X following cache not invalidated: %s", afterFollow.Body.String())
	}
	if gotY := request(t, h, http.MethodGet, path, tokenY, nil); !bytes.Equal(gotY.Body.Bytes(), firstY.Body.Bytes()) {
		t.Fatalf("viewer Y cache changed after viewer X follow: %s", gotY.Body.String())
	}
	if res := request(t, h, http.MethodDelete, fmt.Sprintf("/api/users/me/following/%d", authorA), tokenX, nil); res.Code != http.StatusOK {
		t.Fatalf("viewer X unfollow A: %d %s", res.Code, res.Body.String())
	}
	afterUnfollow := request(t, h, http.MethodGet, path, tokenX, nil)
	if !pageHasOnly(afterUnfollow.Body.Bytes(), videoB1.ID, videoB2.ID) {
		t.Fatalf("unfollow did not invalidate page: %s", afterUnfollow.Body.String())
	}
	if res := request(t, h, http.MethodPut, fmt.Sprintf("/api/videos/%d/like", videoB1.ID), tokenX, nil); res.Code != http.StatusOK {
		t.Fatalf("like B1: %d %s", res.Code, res.Body.String())
	}
	likedX := request(t, h, http.MethodGet, path, tokenX, nil)
	likedY := request(t, h, http.MethodGet, path, tokenY, nil)
	if !bytes.Contains(likedX.Body.Bytes(), []byte(`"like_count":1`)) || !bytes.Contains(likedX.Body.Bytes(), []byte(`"liked":true`)) {
		t.Fatalf("viewer X cache did not reflect own like: %s", likedX.Body.String())
	}
	if !bytes.Contains(likedY.Body.Bytes(), []byte(`"like_count":1`)) || bytes.Contains(likedY.Body.Bytes(), []byte(`"liked":true`)) {
		t.Fatalf("viewer Y cache did not reflect count or leaked viewer state: %s", likedY.Body.String())
	}
	// Cursor pages read SQL directly. A direct database change is visible there.
	firstPage := request(t, h, http.MethodGet, path+"&limit=1", tokenX, nil)
	var page struct {
		Items      []Video `json:"items"`
		NextCursor string  `json:"next_cursor"`
	}
	if firstPage.Code != http.StatusOK || json.Unmarshal(firstPage.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("following first page: %d %s", firstPage.Code, firstPage.Body.String())
	}
	if err := db.Model(&videoB2).Update("title", "new cursor-page title").Error; err != nil {
		t.Fatal(err)
	}
	cursorPage := request(t, h, http.MethodGet, path+"&limit=1&cursor="+page.NextCursor, tokenX, nil)
	if cursorPage.Code != http.StatusOK || !bytes.Contains(cursorPage.Body.Bytes(), []byte("new cursor-page title")) {
		t.Fatalf("following cursor used stale cache: %d %s", cursorPage.Code, cursorPage.Body.String())
	}
	if err := db.Model(&videoB1).Update("title", "Redis fallback title").Error; err != nil {
		t.Fatal(err)
	}
	redisServer.Close()
	fallback := request(t, h, http.MethodGet, path, tokenX, nil)
	if fallback.Code != http.StatusOK || !bytes.Contains(fallback.Body.Bytes(), []byte("Redis fallback title")) {
		t.Fatalf("following database fallback: %d %s", fallback.Code, fallback.Body.String())
	}
}

func TestFollowingFeedCacheInvalidationAcrossInstances(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	clientA := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), MaxRetries: 0})
	clientB := redis.NewClient(&redis.Options{Addr: redisServer.Addr(), MaxRetries: 0})
	t.Cleanup(func() { _ = clientA.Close() })
	t.Cleanup(func() { _ = clientB.Close() })
	assertFollowingFeedCacheInvalidationAcrossInstances(t, clientA, clientB)
}

func TestFollowingFeedCacheCrossInstanceRealRedis(t *testing.T) {
	addr := os.Getenv("GOSTREAM_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set GOSTREAM_TEST_REDIS_ADDR to an isolated Redis 8 instance")
	}
	// The Redis integration job reserves logical database 15 for this test so
	// cache keys cannot collide with application data in the default database.
	clientA := redis.NewClient(&redis.Options{Addr: addr, DB: 15, MaxRetries: 0})
	clientB := redis.NewClient(&redis.Options{Addr: addr, DB: 15, MaxRetries: 0})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := clientA.Ping(ctx).Err(); err != nil {
		_ = clientA.Close()
		_ = clientB.Close()
		t.Fatal(err)
	}
	if keys, err := clientA.DBSize(ctx).Result(); err != nil || keys != 0 {
		_ = clientA.Close()
		_ = clientB.Close()
		t.Fatalf("requires empty dedicated Redis database 15: keys=%d err=%v", keys, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := clientA.Scan(ctx, cursor, "gostream:feed:*", 100).Result()
			if err != nil {
				t.Errorf("scan isolated Redis Feed keys: %v", err)
				break
			}
			if len(keys) > 0 {
				if err := clientA.Del(ctx, keys...).Err(); err != nil {
					t.Errorf("delete isolated Redis Feed keys: %v", err)
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		if err := clientA.Close(); err != nil {
			t.Errorf("close Redis client A: %v", err)
		}
		if err := clientB.Close(); err != nil {
			t.Errorf("close Redis client B: %v", err)
		}
	})
	assertFollowingFeedCacheInvalidationAcrossInstances(t, clientA, clientB)
}

func assertFollowingFeedCacheInvalidationAcrossInstances(t *testing.T, clientA, clientB *redis.Client) {
	t.Helper()
	instanceA, routerA, db := testServer(t)
	instanceB := NewServer(db, "test-signing-secret-at-least-32-bytes", t.TempDir())
	routerB := instanceB.Router()
	instanceA.UseRedisFeedCache(clientA)
	instanceB.UseRedisFeedCache(clientB)

	authorA, authorToken := registerTestUser(t, routerA, "feed_cache_cross_author_a")
	authorB, _ := registerTestUser(t, routerA, "feed_cache_cross_author_b")
	viewerID, viewerToken := registerTestUser(t, routerA, "feed_cache_cross_viewer")
	videoA := Video{AuthorID: authorA, Title: "author A video", MediaURL: "/media/cross-a.mp4", Status: "published"}
	videoB := Video{AuthorID: authorB, Title: "author B video", MediaURL: "/media/cross-b.mp4", Status: "published"}
	for _, video := range []*Video{&videoA, &videoB} {
		if err := db.Create(video).Error; err != nil {
			t.Fatal(err)
		}
	}
	if res := request(t, routerA, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorA), viewerToken, nil); res.Code != http.StatusOK {
		t.Fatalf("initial follow on instance A: %d %s", res.Code, res.Body.String())
	}
	path := "/api/feed?scene=following"
	first := request(t, routerA, http.MethodGet, path, viewerToken, nil)
	if first.Code != http.StatusOK || !pageHasOnly(first.Body.Bytes(), videoA.ID) {
		t.Fatalf("initial following feed: %d %s", first.Code, first.Body.String())
	}
	// A follow on instance B must advance the shared per-viewer version so A
	// cannot serve the page it cached before the relationship changed.
	if res := request(t, routerB, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", authorB), viewerToken, nil); res.Code != http.StatusOK {
		t.Fatalf("cross-instance follow: %d %s", res.Code, res.Body.String())
	}
	afterFollow := request(t, routerA, http.MethodGet, path, viewerToken, nil)
	if afterFollow.Code != http.StatusOK || !pageHasOnly(afterFollow.Body.Bytes(), videoA.ID, videoB.ID) {
		t.Fatalf("instance A served stale page after instance B follow (viewer %d): %d %s", viewerID, afterFollow.Code, afterFollow.Body.String())
	}
	// A global Feed mutation on B must also invalidate the following page held
	// by A, even though the cache entry is scoped to one viewer.
	updated := request(t, routerB, http.MethodPatch, fmt.Sprintf("/api/videos/%d", videoA.ID), authorToken, map[string]string{"title": "updated on instance B"})
	if updated.Code != http.StatusOK {
		t.Fatalf("cross-instance video edit: %d %s", updated.Code, updated.Body.String())
	}
	afterEdit := request(t, routerA, http.MethodGet, path, viewerToken, nil)
	if afterEdit.Code != http.StatusOK || !bytes.Contains(afterEdit.Body.Bytes(), []byte("updated on instance B")) {
		t.Fatalf("instance A served stale page after instance B video edit: %d %s", afterEdit.Code, afterEdit.Body.String())
	}
}

func pageHasOnly(raw []byte, ids ...uint) bool {
	var page struct {
		Items []Video `json:"items"`
	}
	if json.Unmarshal(raw, &page) != nil || len(page.Items) != len(ids) {
		return false
	}
	expected := make(map[uint]bool, len(ids))
	for _, id := range ids {
		expected[id] = true
	}
	for _, video := range page.Items {
		if !expected[video.ID] {
			return false
		}
		delete(expected, video.ID)
	}
	return len(expected) == 0
}
