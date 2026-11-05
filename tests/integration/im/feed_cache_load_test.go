package im_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

const (
	warmFeedWorkers           = 12
	warmFeedRequestsPerWorker = 20
)

type warmFeedResult struct {
	latencies []time.Duration
	elapsed   time.Duration
	errors    []error
}

// TestFeedCacheWarmBurst measures short, bounded cache-hit bursts against two
// production API processes sharing the isolated MySQL and Redis instances.
func TestFeedCacheWarmBurst(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("GOSTREAM_TEST_IM_BINARY"))
	dsn := strings.TrimSpace(os.Getenv("GOSTREAM_TEST_IM_MYSQL_DSN"))
	redisAddr := strings.TrimSpace(os.Getenv("GOSTREAM_TEST_IM_REDIS_ADDR"))
	container := strings.TrimSpace(os.Getenv("GOSTREAM_TEST_IM_REDIS_CONTAINER"))
	if binary == "" || dsn == "" || redisAddr == "" || container == "" {
		t.Skip("requires isolated API binary, empty MySQL schema and disposable Redis; use tests/integration/im_smoke.py --suite feed-cache-load")
	}
	if !strings.HasPrefix(container, "gostream-im-test-") {
		t.Fatal("refusing to use Redis outside the disposable gostream-im-test- namespace")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("invalid isolated MySQL configuration")
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("requires a newly created empty schema: table_count=%d err=%v", tables, err)
	}

	rc := redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: 0})
	t.Cleanup(func() { _ = rc.Close() })
	if keys, err := rc.DBSize(ctx).Result(); err != nil || keys != 0 {
		t.Fatalf("requires an empty dedicated Redis server: keys=%d err=%v", keys, err)
	}
	secret := randomID(t) + randomID(t)
	servers := []*process{
		newProcess(t, binary, dsn, redisAddr, secret),
		newProcess(t, binary, dsn, redisAddr, secret),
	}
	for _, server := range servers {
		server.start(t)
	}
	author := register(t, servers[0].base, "feed_cache_author_"+randomID(t)[:8])
	viewer := register(t, servers[1].base, "feed_cache_viewer_"+randomID(t)[:8])
	call(t, servers[0].base, http.MethodPut, fmt.Sprintf("/api/users/me/following/%d", author.User.ID), viewer, "", nil, http.StatusOK, nil)

	const videoCount = 24
	videoIDs := make(map[uint]struct{}, videoCount)
	for i := range videoCount {
		createdAt := time.Now().UTC().Add(-time.Duration(videoCount-i) * time.Second)
		result, err := db.ExecContext(ctx,
			"INSERT INTO videos (author_id,title,media_url,status,created_at) VALUES (?,?,?,'published',?)",
			author.User.ID, fmt.Sprintf("warm-cache-%02d", i), "/media/"+randomID(t)+".mp4", createdAt)
		if err != nil {
			t.Fatalf("seed isolated Feed video %d: %v", i, err)
		}
		id, err := result.LastInsertId()
		if err != nil || id <= 0 {
			t.Fatalf("read isolated Feed video ID: id=%d err=%v", id, err)
		}
		videoIDs[uint(id)] = struct{}{}
	}

	clients := []*http.Client{
		{Timeout: 4 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: warmFeedWorkers, MaxConnsPerHost: warmFeedWorkers}},
		{Timeout: 4 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: warmFeedWorkers, MaxConnsPerHost: warmFeedWorkers}},
	}
	t.Cleanup(func() {
		for _, client := range clients {
			client.CloseIdleConnections()
		}
	})

	for _, scene := range []string{"following", "recommend"} {
		path := "/api/feed?scene=" + scene + "&limit=24"
		beforePrimeHits := feedCacheOutcomeTotal(t, servers, "hit")
		beforePrimeMisses := feedCacheOutcomeTotal(t, servers, "miss")
		for _, server := range servers {
			if _, err := requestWarmFeed(clients[0], server.base, path, sceneToken(scene, viewer), videoIDs); err != nil {
				t.Fatalf("prime %s cache on %s: %v", scene, server.addr, err)
			}
		}
		primeHits := feedCacheOutcomeTotal(t, servers, "hit") - beforePrimeHits
		primeMisses := feedCacheOutcomeTotal(t, servers, "miss") - beforePrimeMisses
		if primeHits != 1 || primeMisses != 1 {
			t.Fatalf("%s cache priming outcomes: hits=%.0f misses=%.0f, want one cold miss followed by one shared-cache hit", scene, primeHits, primeMisses)
		}
		viewerID := uint(0)
		if scene == "following" {
			viewerID = viewer.User.ID
		}
		pattern := fmt.Sprintf("gostream:feed:*:%s:%d:24:*", scene, viewerID)
		keys, err := rc.Keys(ctx, pattern).Result()
		if err != nil || len(keys) != 1 {
			t.Fatalf("%s Redis cache key count=%d err=%v; pattern=%q", scene, len(keys), err, pattern)
		}
		if ttl, err := rc.TTL(ctx, keys[0]).Result(); err != nil || ttl <= 0 {
			t.Fatalf("%s Redis cache key has no live TTL: ttl=%s err=%v", scene, ttl, err)
		}
		beforeHits := feedCacheOutcomeTotal(t, servers, "hit")
		beforeMisses := feedCacheOutcomeTotal(t, servers, "miss")
		beforeQueries := feedStageTotal(t, servers, scene, "candidate_sql")
		if beforeQueries < 1 {
			t.Fatalf("%s cache priming did not expose a successful candidate_sql metric sample", scene)
		}

		result := runWarmFeedBurst(t, servers, clients, path, sceneToken(scene, viewer), videoIDs)
		afterHits := feedCacheOutcomeTotal(t, servers, "hit")
		afterMisses := feedCacheOutcomeTotal(t, servers, "miss")
		afterQueries := feedStageTotal(t, servers, scene, "candidate_sql")
		hits := afterHits - beforeHits
		misses := afterMisses - beforeMisses
		queries := afterQueries - beforeQueries
		total := warmFeedWorkers * warmFeedRequestsPerWorker
		hitRate := float64(hits) / float64(total)
		p50, p95 := durationPercentiles(result.latencies)
		t.Logf("warm_feed_cache scene=%s requests=%d concurrency=%d successes=%d failures=%d hits=%.0f misses=%.0f hit_rate=%.4f candidate_sql=%.0f elapsed=%s rps=%.1f p50=%s p95=%s",
			scene, total, warmFeedWorkers, len(result.latencies), len(result.errors), hits, misses,
			hitRate, queries, result.elapsed, float64(total)/result.elapsed.Seconds(), p50, p95)
		if len(result.errors) != 0 {
			t.Fatalf("%s warm-cache burst had %d failed requests; first: %v", scene, len(result.errors), result.errors[0])
		}
		if hits+misses != float64(total) {
			t.Fatalf("%s cache outcome delta=%g want %d requests", scene, hits+misses, total)
		}
		if hitRate < 0.95 {
			t.Fatalf("%s warm-cache hit ratio %.2f%% is below 95%%", scene, hitRate*100)
		}
		if queries != 0 {
			t.Fatalf("%s warmed burst issued %.0f candidate_sql queries, want zero", scene, queries)
		}
		if p50 == 0 || p95 == 0 {
			t.Fatalf("%s burst did not produce latency samples", scene)
		}
	}
}

func sceneToken(scene string, viewer account) string {
	if scene == "following" {
		return viewer.Token
	}
	return ""
}

func requestWarmFeed(client *http.Client, base, path, token string, seeded map[uint]struct{}) (time.Duration, error) {
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	started := time.Now()
	response, err := client.Do(request)
	elapsed := time.Since(started)
	if err != nil {
		return elapsed, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	elapsed = time.Since(started) // Include transfer of the complete response body.
	if err != nil {
		return elapsed, err
	}
	if response.StatusCode != http.StatusOK {
		return elapsed, fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var page followingFeedPage
	if err := json.Unmarshal(body, &page); err != nil {
		return elapsed, fmt.Errorf("decode Feed response: %w", err)
	}
	if len(page.Items) != len(seeded) {
		return elapsed, fmt.Errorf("Feed response contained %d videos, want all %d seeded videos", len(page.Items), len(seeded))
	}
	seen := make(map[uint]struct{}, len(page.Items))
	for _, item := range page.Items {
		if _, ok := seeded[item.ID]; !ok {
			return elapsed, fmt.Errorf("Feed returned unseeded video %d", item.ID)
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return elapsed, fmt.Errorf("Feed returned duplicate video %d", item.ID)
		}
		seen[item.ID] = struct{}{}
	}
	return elapsed, nil
}

func TestWarmFeedResponseRejectsMissingVideos(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"id":1}]}`)
	}))
	defer server.Close()
	_, err := requestWarmFeed(server.Client(), server.URL, "/", "", map[uint]struct{}{1: {}, 2: {}})
	if err == nil {
		t.Fatal("warm-cache validation accepted a response missing one fixture video")
	}
}

func TestWarmFeedLatencyIncludesResponseBody(t *testing.T) {
	const bodyDelay = 100 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(bodyDelay)
		_, _ = io.WriteString(w, `{"items":[{"id":1}]}`)
	}))
	defer server.Close()
	elapsed, err := requestWarmFeed(server.Client(), server.URL, "/", "", map[uint]struct{}{1: {}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed < bodyDelay {
		t.Fatalf("latency %s excluded delayed response body (%s)", elapsed, bodyDelay)
	}
}

func runWarmFeedBurst(t *testing.T, servers []*process, clients []*http.Client, path, token string, seeded map[uint]struct{}) warmFeedResult {
	t.Helper()
	const total = warmFeedWorkers * warmFeedRequestsPerWorker
	latencies := make(chan time.Duration, total)
	errorsFound := make(chan error, total)
	started := time.Now()
	var workers sync.WaitGroup
	workers.Add(warmFeedWorkers)
	for worker := range warmFeedWorkers {
		go func() {
			defer workers.Done()
			serverIndex := worker % len(servers)
			for range warmFeedRequestsPerWorker {
				elapsed, err := requestWarmFeed(clients[serverIndex], servers[serverIndex].base, path, token, seeded)
				if err != nil {
					errorsFound <- err
					continue
				}
				latencies <- elapsed
			}
		}()
	}
	workers.Wait()
	result := warmFeedResult{elapsed: time.Since(started)}
	close(latencies)
	close(errorsFound)
	for sample := range latencies {
		result.latencies = append(result.latencies, sample)
	}
	for err := range errorsFound {
		result.errors = append(result.errors, err)
	}
	return result
}

func durationPercentiles(samples []time.Duration) (time.Duration, time.Duration) {
	if len(samples) == 0 {
		return 0, 0
	}
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	nearestRank := func(percentile int) time.Duration {
		index := (percentile*len(ordered)+99)/100 - 1
		if index < 0 {
			index = 0
		}
		return ordered[index]
	}
	return nearestRank(50), nearestRank(95)
}

func feedCacheOutcomeTotal(t *testing.T, servers []*process, outcome string) float64 {
	t.Helper()
	var total float64
	for _, server := range servers {
		total += metricValue(t, server.base, `gostream_feed_cache_total{result="`+outcome+`"}`)
	}
	return total
}

func feedStageTotal(t *testing.T, servers []*process, scene, stage string) float64 {
	t.Helper()
	var total float64
	for _, server := range servers {
		total += metricValue(t, server.base, `gostream_feed_stage_duration_seconds_count{scene="`+scene+`",stage="`+stage+`",outcome="success"}`)
	}
	return total
}

func metricValue(t *testing.T, base, metric string) float64 {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("get API metrics: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("read API metrics: status=%d err=%v", response.StatusCode, err)
	}
	name, labels, hasLabels := strings.Cut(metric, "{")
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, name) {
			continue
		}
		metricLine, sample, hasSample := strings.Cut(line, " ")
		if !hasSample {
			continue
		}
		if hasLabels {
			actualLabels, _, hasActualLabels := strings.Cut(strings.TrimPrefix(metricLine, name), "}")
			if !hasActualLabels {
				continue
			}
			actualLabels = strings.TrimPrefix(actualLabels, "{")
			wanted := strings.TrimSuffix(labels, "}")
			allFound := true
			for _, label := range strings.Split(wanted, ",") {
				if !strings.Contains(","+actualLabels+",", ","+label+",") {
					allFound = false
					break
				}
			}
			if !allFound {
				continue
			}
		} else if metricLine != name {
			continue
		}
		var value float64
		if _, err := fmt.Sscanf(sample, "%f", &value); err != nil {
			t.Fatalf("parse metric %q: %v", line, err)
		}
		return value
	}
	return 0
}
