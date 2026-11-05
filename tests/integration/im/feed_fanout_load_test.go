package im_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

const (
	fanoutLoadFollowers = 320
	fanoutLoadWaves     = 30
	fanoutLoadWaveSize  = 4
	fanoutLoadWavePause = time.Second
)

type fanoutPublishResult struct {
	videoID uint
	err     error
	latency time.Duration
}

// TestFeedFanoutSustainedWriteDrain publishes in paced concurrent waves while
// two production workers share a durable MySQL fanout queue.
func TestFeedFanoutSustainedWriteDrain(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("GOSTREAM_TEST_IM_BINARY"))
	dsn := strings.TrimSpace(os.Getenv("GOSTREAM_TEST_IM_MYSQL_DSN"))
	redisAddr := strings.TrimSpace(os.Getenv("GOSTREAM_TEST_IM_REDIS_ADDR"))
	container := strings.TrimSpace(os.Getenv("GOSTREAM_TEST_IM_REDIS_CONTAINER"))
	if binary == "" || dsn == "" || redisAddr == "" || container == "" {
		t.Skip("requires isolated API binary, empty MySQL schema and disposable Redis; use tests/integration/im_smoke.py --suite fanout-drain")
	}
	if !strings.HasPrefix(container, "gostream-im-test-") {
		t.Fatal("refusing to use Redis outside the disposable gostream-im-test- namespace")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("invalid isolated MySQL configuration")
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var tableCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tableCount); err != nil || tableCount != 0 {
		t.Fatalf("requires a newly created empty schema: table_count=%d err=%v", tableCount, err)
	}
	rc := redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: 0})
	t.Cleanup(func() { _ = rc.Close() })
	if keys, err := rc.DBSize(ctx).Result(); err != nil || keys != 0 {
		t.Fatalf("requires an empty dedicated Redis server: keys=%d err=%v", keys, err)
	}

	secret := randomID(t) + randomID(t)
	servers := []*process{
		newProcessWithFanout(t, binary, dsn, redisAddr, secret, fanoutLoadFollowers),
		newProcessWithFanout(t, binary, dsn, redisAddr, secret, fanoutLoadFollowers),
	}
	for _, server := range servers {
		server.start(t)
	}
	author := register(t, servers[0].base, "fanout_author_"+randomID(t)[:8])
	reader := register(t, servers[1].base, "fanout_reader_"+randomID(t)[:8])
	seedFanoutFollowers(t, ctx, db, author.User.ID, reader.User.ID)
	mediaURLs := seedFanoutMedia(t, ctx, db, author.User.ID, fanoutLoadWaves*fanoutLoadWaveSize)

	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{
		MaxIdleConnsPerHost: fanoutLoadWaveSize,
		MaxConnsPerHost:     fanoutLoadWaveSize,
	}}
	t.Cleanup(client.CloseIdleConnections)
	videoIDs := make([]uint, 0, fanoutLoadWaves*fanoutLoadWaveSize)
	var sampledPeakPending int
	writeStarted := time.Now()
	for wave := range fanoutLoadWaves {
		first := wave * fanoutLoadWaveSize
		results := publishFanoutWave(client, servers, author, first, mediaURLs[first:first+fanoutLoadWaveSize])
		for _, result := range results {
			if result.err != nil {
				t.Fatalf("publish fanout wave %d failed after %s: %v", wave+1, result.latency, result.err)
			}
			videoIDs = append(videoIDs, result.videoID)
		}
		pending, err := pendingFanoutJobs(ctx, db, author.User.ID)
		if err != nil {
			t.Fatalf("sample fanout queue after wave %d: %v", wave+1, err)
		}
		if pending > sampledPeakPending {
			sampledPeakPending = pending
		}
		if wave+1 < fanoutLoadWaves {
			time.Sleep(fanoutLoadWavePause)
		}
	}
	writeDuration := time.Since(writeStarted)
	if len(videoIDs) != fanoutLoadWaves*fanoutLoadWaveSize {
		t.Fatalf("published video count=%d want=%d", len(videoIDs), fanoutLoadWaves*fanoutLoadWaveSize)
	}

	drainStarted := time.Now()
	deadline := drainStarted.Add(60 * time.Second)
	// Setup and publications must not consume the post-write drain allowance.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelDrain()
	var pending int
	for {
		pending, err = pendingFanoutJobs(drainCtx, db, author.User.ID)
		if err != nil {
			t.Fatalf("read fanout queue while draining: %v", err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fanout queue did not drain within 60s after writes stopped: pending=%d", pending)
		}
		time.Sleep(250 * time.Millisecond)
	}
	drainDuration := time.Since(drainStarted)
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelVerify()

	ids := make([]string, 0, len(videoIDs))
	for _, id := range videoIDs {
		ids = append(ids, strconv.FormatUint(uint64(id), 10))
	}
	videoList := strings.Join(ids, ",")
	var jobCount, completedCount, errorCount, attemptCount, inboxCount, incompleteVideos, pullVideos int
	if err := db.QueryRowContext(verifyCtx,
		"SELECT COUNT(*), SUM(done_at IS NOT NULL), SUM(last_error <> ''), SUM(attempts) FROM feed_fanout_jobs WHERE video_id IN ("+videoList+")",
	).Scan(&jobCount, &completedCount, &errorCount, &attemptCount); err != nil {
		t.Fatalf("verify completed fanout jobs: %v", err)
	}
	if err := db.QueryRowContext(verifyCtx,
		"SELECT COUNT(*) FROM feed_inboxes WHERE video_id IN ("+videoList+")",
	).Scan(&inboxCount); err != nil {
		t.Fatalf("count fanout inbox entries: %v", err)
	}
	if err := db.QueryRowContext(verifyCtx,
		"SELECT COUNT(*) FROM (SELECT video_id, COUNT(*) AS n FROM feed_inboxes WHERE video_id IN ("+videoList+") GROUP BY video_id HAVING n <> ?) AS incomplete",
		fanoutLoadFollowers,
	).Scan(&incompleteVideos); err != nil {
		t.Fatalf("verify per-video fanout coverage: %v", err)
	}
	if err := db.QueryRowContext(verifyCtx,
		"SELECT COUNT(*) FROM videos WHERE id IN ("+videoList+") AND feed_pull = TRUE",
	).Scan(&pullVideos); err != nil {
		t.Fatalf("verify completed videos left pull mode: %v", err)
	}
	wantVideos := fanoutLoadWaves * fanoutLoadWaveSize
	wantInboxRows := wantVideos * fanoutLoadFollowers
	wantAttempts := wantVideos * 4 // three 128-row batches and the terminal empty batch
	if jobCount != wantVideos || completedCount != wantVideos || errorCount != 0 || attemptCount != wantAttempts ||
		inboxCount != wantInboxRows || incompleteVideos != 0 || pullVideos != 0 {
		t.Fatalf("fanout consistency mismatch: jobs=%d/%d completed=%d errors=%d attempts=%d/%d inbox=%d/%d incomplete=%d pull_videos=%d",
			jobCount, wantVideos, completedCount, errorCount, attemptCount, wantAttempts,
			inboxCount, wantInboxRows, incompleteVideos, pullVideos)
	}
	publishedSet := make(map[uint]struct{}, len(videoIDs))
	for _, id := range videoIDs {
		publishedSet[id] = struct{}{}
	}
	feedSet := make(map[uint]struct{}, len(videoIDs))
	cursor := ""
	pageCount := 0
	for {
		query := url.Values{"scene": {"following"}, "limit": {"50"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var feedPage struct {
			Items      []feedCard `json:"items"`
			NextCursor string     `json:"next_cursor"`
			HasMore    bool       `json:"has_more"`
		}
		call(t, servers[0].base, http.MethodGet, "/api/feed?"+query.Encode(), reader, "", nil, http.StatusOK, &feedPage)
		pageCount++
		if pageCount == 1 && (len(feedPage.Items) != 50 || !feedPage.HasMore || feedPage.NextCursor == "") {
			t.Fatalf("Following Feed first page has %d rows, has_more=%t cursor=%q", len(feedPage.Items), feedPage.HasMore, feedPage.NextCursor)
		}
		if len(feedPage.Items) == 0 {
			t.Fatalf("Following Feed returned an empty page %d before completion", pageCount)
		}
		for _, item := range feedPage.Items {
			if _, exists := publishedSet[item.ID]; !exists {
				t.Fatalf("Following Feed returned video %d outside the fanout fixture", item.ID)
			}
			if _, duplicate := feedSet[item.ID]; duplicate {
				t.Fatalf("Following Feed duplicated fanout video %d across cursor pages", item.ID)
			}
			feedSet[item.ID] = struct{}{}
		}
		if !feedPage.HasMore {
			break
		}
		if feedPage.NextCursor == "" || feedPage.NextCursor == cursor {
			t.Fatalf("Following Feed cursor did not advance on page %d: %q", pageCount, feedPage.NextCursor)
		}
		cursor = feedPage.NextCursor
	}
	if len(feedSet) != len(videoIDs) {
		t.Fatalf("Following Feed returned %d of %d published videos across %d pages", len(feedSet), len(videoIDs), pageCount)
	}
	t.Logf("fanout_sustained_drain videos=%d followers=%d inbox_rows=%d waves=%d wave_size=%d publish_window=%s sampled_peak_pending_jobs=%d drain_after_stop=%s jobs_completed=%d errors=%d worker_attempts=%d following_pages=%d following_videos=%d",
		wantVideos, fanoutLoadFollowers, inboxCount, fanoutLoadWaves, fanoutLoadWaveSize,
		writeDuration, sampledPeakPending, drainDuration, completedCount, errorCount, attemptCount, pageCount, len(feedSet))
}

func seedFanoutFollowers(t *testing.T, ctx context.Context, db *sql.DB, authorID, readerID uint) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin fanout follower seed transaction")
	}
	defer func() { _ = tx.Rollback() }()
	const followerInsert = "INSERT INTO users (username,password_hash,nickname,created_at) VALUES (?,?,?,?)"
	const followInsert = "INSERT INTO follows (follower_id,followee_id,created_at) VALUES (?,?,?)"
	createdAt := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, followInsert, readerID, authorID, createdAt); err != nil {
		t.Fatalf("seed authenticated reader follow: %v", err)
	}
	seedCount := fanoutLoadFollowers - 1
	seedTag := randomID(t)[:8]
	for i := range seedCount {
		result, err := tx.ExecContext(ctx, followerInsert,
			fmt.Sprintf("f%s_%03d", seedTag, i), "unused-fanout-test-credential", "Fanout load follower", createdAt)
		if err != nil {
			t.Fatalf("seed fanout follower %d: %v", i, err)
		}
		followerID, err := result.LastInsertId()
		if err != nil || followerID <= 0 {
			t.Fatalf("read seeded follower ID: id=%d err=%v", followerID, err)
		}
		if _, err := tx.ExecContext(ctx, followInsert, followerID, authorID, createdAt); err != nil {
			t.Fatalf("seed fanout relationship %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fanout follower fixture: %v", err)
	}
	var got int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM follows WHERE followee_id=?", authorID).Scan(&got); err != nil || got != fanoutLoadFollowers {
		t.Fatalf("fanout author followers=%d want=%d err=%v", got, fanoutLoadFollowers, err)
	}
}

func seedFanoutMedia(t *testing.T, ctx context.Context, db *sql.DB, ownerID uint, count int) []string {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin fanout media seed transaction")
	}
	defer func() { _ = tx.Rollback() }()
	createdAt := time.Now().UTC()
	urls := make([]string, 0, count)
	for i := range count {
		url := fmt.Sprintf("/media/fanout-load-%s-%03d.mp4", randomID(t)[:8], i)
		if _, err := tx.ExecContext(ctx, "INSERT INTO media_assets (owner_id,url,kind,size,created_at) VALUES (?,?,?,?,?)", ownerID, url, "video", 1024, createdAt); err != nil {
			t.Fatalf("seed fanout media asset %d: %v", i, err)
		}
		urls = append(urls, url)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fanout media fixture: %v", err)
	}
	return urls
}

func publishFanoutWave(client *http.Client, servers []*process, author account, first int, mediaURLs []string) []fanoutPublishResult {
	count := len(mediaURLs)
	results := make(chan fanoutPublishResult, count)
	var workers sync.WaitGroup
	workers.Add(count)
	for offset := range count {
		index := first + offset
		go func() {
			defer workers.Done()
			id, latency, err := fanoutRequest(client, servers[index%len(servers)], author,
				fmt.Sprintf("fanout load video %03d", index), mediaURLs[offset])
			results <- fanoutPublishResult{videoID: id, latency: latency, err: err}
		}()
	}
	workers.Wait()
	close(results)
	collected := make([]fanoutPublishResult, 0, count)
	for result := range results {
		collected = append(collected, result)
	}
	return collected
}

func pendingFanoutJobs(ctx context.Context, db *sql.DB, authorID uint) (int, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var pending int
	err := db.QueryRowContext(queryCtx, "SELECT COUNT(*) FROM feed_fanout_jobs WHERE author_id=? AND done_at IS NULL", authorID).Scan(&pending)
	return pending, err
}

func fanoutRequest(client *http.Client, server *process, author account, title, mediaURL string) (uint, time.Duration, error) {
	body, err := json.Marshal(map[string]string{"title": title, "media_url": mediaURL})
	if err != nil {
		return 0, 0, err
	}
	request, err := http.NewRequest(http.MethodPost, server.base+"/api/videos", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+author.Token)
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	response, err := client.Do(request)
	latency := time.Since(started)
	if err != nil {
		return 0, latency, err
	}
	defer response.Body.Close()
	var result struct {
		ID     uint   `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return 0, latency, fmt.Errorf("decode publish response (HTTP %d): %w", response.StatusCode, err)
	}
	if response.StatusCode != http.StatusCreated || result.ID == 0 || result.Status != "published" {
		return 0, latency, fmt.Errorf("unexpected publish response: HTTP %d video=%d status=%q", response.StatusCode, result.ID, result.Status)
	}
	return result.ID, latency, nil
}
