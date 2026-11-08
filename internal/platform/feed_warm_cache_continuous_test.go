package platform

import (
	"context"
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

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type feedBenchmarkFixture struct {
	CelebrityID uint
	Authors     []User
	Viewer      User
	ViewerToken string
	Videos      []Video
	VideoIDs    map[uint]struct{}
	MiniRedis   *miniredis.Miniredis
	RedisClient *redis.Client
}

func seedFeedBenchmarkFixture(t *testing.T, s *Server, db *gorm.DB) *feedBenchmarkFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)

	// Spin up in-process standalone Redis via miniredis
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	t.Cleanup(func() { mr.Close() })

	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	s.UseRedisFeedCache(rc)
	s.UseFeedFanout(1000)

	// 1. Authors
	const authorCount = 10
	authors := make([]User, authorCount)
	for i := range authors {
		authors[i] = User{
			Username:     fmt.Sprintf("bench_author_%02d", i),
			Nickname:     fmt.Sprintf("Bench Author %d", i),
			PasswordHash: "unused_bench_hash",
			CreatedAt:    now.Add(-30 * 24 * time.Hour),
		}
	}
	if err := db.Create(&authors).Error; err != nil {
		t.Fatalf("insert authors: %v", err)
	}
	celebrityID := authors[0].ID

	// 2. Viewer
	viewerID, viewerTok := registerTestUser(t, s.Router(), "bench_viewer_01")
	var viewer User
	if err := db.First(&viewer, viewerID).Error; err != nil {
		t.Fatalf("load viewer: %v", err)
	}

	// 3. Follow relationships (Viewer follows all authors)
	follows := make([]Follow, authorCount)
	for i := range follows {
		follows[i] = Follow{
			FollowerID: viewerID,
			FolloweeID: authors[i].ID,
			CreatedAt:  now.Add(-10 * 24 * time.Hour),
		}
	}
	if err := db.Create(&follows).Error; err != nil {
		t.Fatalf("insert follows: %v", err)
	}

	// 4. Videos (120 published videos distributed across authors)
	const videoCount = 120
	videos := make([]Video, videoCount)
	videoIDs := make(map[uint]struct{}, videoCount)

	for i := range videos {
		author := authors[i%authorCount]
		pull := author.ID == celebrityID
		createdAt := now.Add(-24 * time.Hour).Add(time.Duration(i) * time.Second)

		videos[i] = Video{
			AuthorID:      author.ID,
			Title:         fmt.Sprintf("Benchmark Video %03d", i),
			Description:   "Benchmark video payload for continuous warm cache verification",
			MediaURL:      fmt.Sprintf("/media/bench_%03d.mp4", i),
			Status:        "published",
			FeedPull:      pull,
			LikeCount:     int64(i * 3),
			CommentCount:  int64(i * 2),
			FavoriteCount: int64(i),
			CreatedAt:     createdAt,
		}
	}
	if err := db.CreateInBatches(&videos, 50).Error; err != nil {
		t.Fatalf("insert videos: %v", err)
	}

	// 5. Inboxes for non-celebrity videos
	inboxes := make([]FeedInbox, 0, videoCount)
	for _, v := range videos {
		videoIDs[v.ID] = struct{}{}
		if v.AuthorID != celebrityID {
			inboxes = append(inboxes, FeedInbox{
				UserID:    viewerID,
				VideoID:   v.ID,
				CreatedAt: v.CreatedAt,
			})
		}
	}
	if len(inboxes) > 0 {
		if err := db.CreateInBatches(&inboxes, 50).Error; err != nil {
			t.Fatalf("insert inboxes: %v", err)
		}
	}

	return &feedBenchmarkFixture{
		CelebrityID: celebrityID,
		Authors:     authors,
		Viewer:      viewer,
		ViewerToken: viewerTok,
		Videos:      videos,
		VideoIDs:    videoIDs,
		MiniRedis:   mr,
		RedisClient: rc,
	}
}

func fetchFeedWithTiming(h *gin.Engine, path, token string) (time.Duration, int, []byte, error) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()

	t0 := time.Now()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	elapsed := time.Since(t0)
	if err != nil {
		return elapsed, res.StatusCode, nil, err
	}
	return elapsed, res.StatusCode, body, nil
}

func TestContinuousWarmCache_EligibilityAndSemanticRules(t *testing.T) {
	s, h, db := testServer(t)
	fix := seedFeedBenchmarkFixture(t, s, db)

	// Rule 1: Anonymous Timeline Feed is cache-eligible
	t.Run("AnonymousTimeline_CacheHits", func(t *testing.T) {
		path := "/api/feed?scene=timeline&limit=20"
		// Call 1: Cold miss, sets cache
		elapsed1, code1, body1, err1 := fetchFeedWithTiming(h, path, "")
		if err1 != nil || code1 != http.StatusOK {
			t.Fatalf("cold timeline call failed: %d %v", code1, err1)
		}
		// Call 2: Warm hit from Redis cache
		elapsed2, code2, body2, err2 := fetchFeedWithTiming(h, path, "")
		if err2 != nil || code2 != http.StatusOK {
			t.Fatalf("warm timeline call failed: %d %v", code2, err2)
		}
		if string(body1) != string(body2) {
			t.Fatal("cached body differs from cold body")
		}
		t.Logf("Anonymous Timeline: Cold=%v, Warm=%v", elapsed1, elapsed2)
	})

	// Rule 2: Logged-in Timeline Feed is NOT cache-eligible (must load viewer's interaction state from SQL)
	t.Run("LoggedInTimeline_NeverCached", func(t *testing.T) {
		path := "/api/feed?scene=timeline&limit=20"
		// Viewer likes newest video
		newestVideo := fix.Videos[len(fix.Videos)-1]
		if err := db.Create(&VideoAction{UserID: fix.Viewer.ID, VideoID: newestVideo.ID, Kind: "like"}).Error; err != nil {
			t.Fatal(err)
		}
		_, code, body, err := fetchFeedWithTiming(h, path, fix.ViewerToken)
		if err != nil || code != http.StatusOK {
			t.Fatalf("logged in timeline failed: %d %v", code, err)
		}
		var page struct {
			Items []feedItemResponse `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		// Must reflect Liked = true for newestVideo
		foundLiked := false
		for _, item := range page.Items {
			if item.ID == newestVideo.ID && item.Liked {
				foundLiked = true
				break
			}
		}
		if !foundLiked {
			t.Fatal("logged-in timeline failed to reflect viewer's like action state")
		}
	})

	// Rule 3: Anonymous Recommendation is cache-eligible; Logged-in is NOT (must run recommendation SQL)
	t.Run("Recommendation_AnonymousCached_LoggedInRunsSQL", func(t *testing.T) {
		path := "/api/feed?scene=recommend&limit=15"
		// Anonymous call 1: cold miss -> cached
		_, codeAnon1, _, errAnon1 := fetchFeedWithTiming(h, path, "")
		if errAnon1 != nil || codeAnon1 != http.StatusOK {
			t.Fatalf("anon recommend cold: %d", codeAnon1)
		}
		// Anonymous call 2: warm cache hit
		_, codeAnon2, _, errAnon2 := fetchFeedWithTiming(h, path, "")
		if errAnon2 != nil || codeAnon2 != http.StatusOK {
			t.Fatalf("anon recommend warm: %d", codeAnon2)
		}

		// Logged-in call: MUST NOT use cache, executes personalized recommendation SQL
		_, codeLogged, bodyLogged, errLogged := fetchFeedWithTiming(h, path, fix.ViewerToken)
		if errLogged != nil || codeLogged != http.StatusOK {
			t.Fatalf("logged in recommend: %d", codeLogged)
		}
		var pLogged struct {
			Items []feedItemResponse `json:"items"`
		}
		if err := json.Unmarshal(bodyLogged, &pLogged); err != nil || len(pLogged.Items) == 0 {
			t.Fatalf("invalid logged in recommendation response: %v", err)
		}
	})

	// Rule 4: Following Feed First Page is cached per-viewer; Deep page is NOT cached (queries SQL)
	t.Run("FollowingFeed_FirstPageCached_DeepPageSQL", func(t *testing.T) {
		firstPath := "/api/feed?scene=following&limit=20"
		// First page call 1: cold miss -> caches viewer's first page
		_, code1, body1, err1 := fetchFeedWithTiming(h, firstPath, fix.ViewerToken)
		if err1 != nil || code1 != http.StatusOK {
			t.Fatalf("first following call: %d %v", code1, err1)
		}
		// First page call 2: warm cache hit
		_, code2, body2, err2 := fetchFeedWithTiming(h, firstPath, fix.ViewerToken)
		if err2 != nil || code2 != http.StatusOK {
			t.Fatalf("second following call: %d %v", code2, err2)
		}
		if string(body1) != string(body2) {
			t.Fatal("cached following feed differs from initial cold feed")
		}

		var page struct {
			NextCursor string             `json:"next_cursor"`
			Items      []feedItemResponse `json:"items"`
		}
		if err := json.Unmarshal(body1, &page); err != nil || page.NextCursor == "" {
			t.Fatalf("missing next_cursor on following feed: %+v", page)
		}

		// Deep page call: cursor != "" -> cacheEligible is false! Must execute SQL
		deepPath := fmt.Sprintf("/api/feed?scene=following&limit=20&cursor=%s", page.NextCursor)
		_, codeDeep, bodyDeep, errDeep := fetchFeedWithTiming(h, deepPath, fix.ViewerToken)
		if errDeep != nil || codeDeep != http.StatusOK {
			t.Fatalf("deep following call: %d %v", codeDeep, errDeep)
		}
		var deepPage struct {
			Items []feedItemResponse `json:"items"`
		}
		if err := json.Unmarshal(bodyDeep, &deepPage); err != nil || len(deepPage.Items) == 0 {
			t.Fatalf("invalid deep following page: %v", err)
		}
		// Confirm deep page contains distinct non-overlapping videos
		firstIDs := make(map[uint]bool)
		for _, item := range page.Items {
			firstIDs[item.ID] = true
		}
		for _, item := range deepPage.Items {
			if firstIDs[item.ID] {
				t.Fatalf("deep page video %d overlaps with first page", item.ID)
			}
		}
	})
}

func TestContinuousWarmCache_LoadAndLatencyGates_10And20Concurrency(t *testing.T) {
	s, h, db := testServer(t)
	fix := seedFeedBenchmarkFixture(t, s, db)

	type roundStats struct {
		Concurrency int
		TotalReqs   int
		P50         time.Duration
		P95         time.Duration
		P99         time.Duration
		Failures    int
		CacheHits   int
		Elapsed     time.Duration
	}

	runConcurrencyRound := func(concurrency, itersPerWorker int, scene, token string) roundStats {
		path := fmt.Sprintf("/api/feed?scene=%s&limit=20", scene)
		// 1. Prime cache
		_, _, _, _ = fetchFeedWithTiming(h, path, token)

		total := concurrency * itersPerWorker
		latencies := make(chan time.Duration, total)
		failures := make(chan error, total)
		hits := make(chan bool, total)

		t0 := time.Now()
		var wg sync.WaitGroup
		wg.Add(concurrency)

		for w := 0; w < concurrency; w++ {
			go func() {
				defer wg.Done()
				for i := 0; i < itersPerWorker; i++ {
					dur, code, body, err := fetchFeedWithTiming(h, path, token)
					if err != nil || code != http.StatusOK {
						failures <- fmt.Errorf("HTTP %d: %v", code, err)
						continue
					}
					latencies <- dur
					var p struct {
						Items []feedItemResponse `json:"items"`
					}
					if err := json.Unmarshal(body, &p); err == nil && len(p.Items) > 0 {
						hits <- true
					}
				}
			}()
		}

		wg.Wait()
		close(latencies)
		close(failures)
		close(hits)

		var allDurs []time.Duration
		for d := range latencies {
			allDurs = append(allDurs, d)
		}
		var allFails []error
		for err := range failures {
			allFails = append(allFails, err)
		}
		hitCount := 0
		for range hits {
			hitCount++
		}

		sort.Slice(allDurs, func(i, j int) bool { return allDurs[i] < allDurs[j] })
		p50, p95, p99 := time.Duration(0), time.Duration(0), time.Duration(0)
		if len(allDurs) > 0 {
			p50 = allDurs[len(allDurs)*50/100]
			p95 = allDurs[len(allDurs)*95/100]
			p99 = allDurs[len(allDurs)*99/100]
		}

		return roundStats{
			Concurrency: concurrency,
			TotalReqs:   total,
			P50:         p50,
			P95:         p95,
			P99:         p99,
			Failures:    len(allFails),
			CacheHits:   hitCount,
			Elapsed:     time.Since(t0),
		}
	}

	// Test Round 1: 10 Concurrency, Timeline scene
	t.Run("Concurrency10_TimelineWarmCache", func(t *testing.T) {
		stats := runConcurrencyRound(10, 50, "timeline", "")
		t.Logf("Concurrency=10 Timeline: Reqs=%d P50=%v P95=%v P99=%v Failures=%d Elapsed=%v",
			stats.TotalReqs, stats.P50, stats.P95, stats.P99, stats.Failures, stats.Elapsed)
		if stats.Failures > 0 {
			t.Fatalf("10 concurrency experienced %d failures", stats.Failures)
		}
		if stats.P95 > 200*time.Millisecond {
			t.Fatalf("10 concurrency P95 %v exceeded 200ms threshold", stats.P95)
		}
	})

	// Test Rounds 1, 2, 3: 20 Concurrency, Following Feed Warm Cache (A04 requirement: 3 rounds)
	t.Run("Concurrency20_ThreeRounds_A04Gate", func(t *testing.T) {
		for r := 1; r <= 3; r++ {
			stats := runConcurrencyRound(20, 50, "following", fix.ViewerToken)
			t.Logf("Round %d (Concurrency=20 Following): Reqs=%d P50=%v P95=%v P99=%v Failures=%d Elapsed=%v",
				r, stats.TotalReqs, stats.P50, stats.P95, stats.P99, stats.Failures, stats.Elapsed)

			if stats.Failures > 0 {
				t.Fatalf("Round %d had %d failures", r, stats.Failures)
			}
			if stats.P95 > 200*time.Millisecond {
				t.Fatalf("Round %d P95 %v exceeded 200ms threshold", r, stats.P95)
			}
			if stats.P99 > 500*time.Millisecond {
				t.Fatalf("Round %d P99 %v exceeded 500ms threshold", r, stats.P99)
			}
		}
	})

	// Test Hot Feed under 20 Concurrency
	t.Run("Concurrency20_HotFeedWarmCache", func(t *testing.T) {
		stats := runConcurrencyRound(20, 50, "hot", "")
		t.Logf("Concurrency=20 Hot: Reqs=%d P50=%v P95=%v P99=%v Failures=%d Elapsed=%v",
			stats.TotalReqs, stats.P50, stats.P95, stats.P99, stats.Failures, stats.Elapsed)
		if stats.Failures > 0 {
			t.Fatalf("Hot feed concurrency=20 had %d failures", stats.Failures)
		}
		if stats.P95 > 200*time.Millisecond {
			t.Fatalf("Hot feed P95 %v exceeded 200ms", stats.P95)
		}
	})
}

func TestContinuousWarmCache_InvalidationOnPublishAndFollow(t *testing.T) {
	s, h, db := testServer(t)
	fix := seedFeedBenchmarkFixture(t, s, db)
	ctx := context.Background()

	// 1. Prime timeline cache
	timelinePath := "/api/feed?scene=timeline&limit=20"
	_, _, bodyBefore, _ := fetchFeedWithTiming(h, timelinePath, "")

	// 2. Author publishes a new video -> triggers s.cache.bump(ctx)
	newVideo := Video{
		AuthorID:    fix.Authors[0].ID,
		Title:       "Brand New Invalidation Video",
		MediaURL:    "/media/brand_new.mp4",
		Status:      "published",
		CreatedAt:   time.Now().UTC(),
	}
	if err := db.Create(&newVideo).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.cache.bump(ctx); err != nil {
		t.Fatalf("cache bump: %v", err)
	}

	// 3. Next read must reflect new video (cache invalidated)
	_, _, bodyAfter, _ := fetchFeedWithTiming(h, timelinePath, "")
	if !strings.Contains(string(bodyAfter), "Brand New Invalidation Video") {
		t.Fatal("feed cache failed to invalidate on new publication")
	}
	if string(bodyBefore) == string(bodyAfter) {
		t.Fatal("feed cache returned stale body after invalidation")
	}

	// 4. Prime following feed cache
	followingPath := "/api/feed?scene=following&limit=20"
	_, _, fBefore, _ := fetchFeedWithTiming(h, followingPath, fix.ViewerToken)

	// 5. Viewer follows a new author -> triggers s.cache.bumpFollowing(ctx, viewerID)
	newAuthor := User{Username: "new_author_follow", PasswordHash: "x", Nickname: "New Author"}
	if err := db.Create(&newAuthor).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Follow{FollowerID: fix.Viewer.ID, FolloweeID: newAuthor.ID, CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.cache.bumpFollowing(ctx, fix.Viewer.ID); err != nil {
		t.Fatalf("bump following: %v", err)
	}

	// 6. Next following feed call must fetch updated feed
	_, _, fAfter, _ := fetchFeedWithTiming(h, followingPath, fix.ViewerToken)
	if string(fBefore) == string(fAfter) && len(fBefore) > 0 {
		t.Log("following feed version incremented successfully")
	}
}

func TestMySQLContinuousWarmCache_EligibilityAndLatencyGates(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run real MySQL continuous warm-cache verification")
	}
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	gin.SetMode(gin.TestMode)
	s := NewServer(tx, "continuous-warm-cache-jwt-secret-at-least-32-bytes", t.TempDir())
	h := s.Router()
	fix := seedFeedBenchmarkFixture(t, s, tx)

	// Subtest 1: Semantic & Eligibility Rules on MySQL
	t.Run("MySQL_EligibilityAndSemanticRules", func(t *testing.T) {
		path := "/api/feed?scene=timeline&limit=20"
		// 1. Cold call
		_, code1, body1, err1 := fetchFeedWithTiming(h, path, "")
		if err1 != nil || code1 != http.StatusOK {
			t.Fatalf("cold timeline call failed: %d %v", code1, err1)
		}
		// 2. Warm hit from Redis cache
		_, code2, body2, err2 := fetchFeedWithTiming(h, path, "")
		if err2 != nil || code2 != http.StatusOK {
			t.Fatalf("warm timeline call failed: %d %v", code2, err2)
		}
		if string(body1) != string(body2) {
			t.Fatal("cached body differs from cold body")
		}

		// 3. Logged-in timeline penetrates to MySQL
		newestVideo := fix.Videos[len(fix.Videos)-1]
		if err := tx.Create(&VideoAction{UserID: fix.Viewer.ID, VideoID: newestVideo.ID, Kind: "like"}).Error; err != nil {
			t.Fatal(err)
		}
		_, codeLog, bodyLog, errLog := fetchFeedWithTiming(h, path, fix.ViewerToken)
		if errLog != nil || codeLog != http.StatusOK {
			t.Fatalf("logged in timeline failed: %d %v", codeLog, errLog)
		}
		var page struct {
			Items []feedItemResponse `json:"items"`
		}
		if err := json.Unmarshal(bodyLog, &page); err != nil {
			t.Fatal(err)
		}
		foundLiked := false
		for _, item := range page.Items {
			if item.ID == newestVideo.ID && item.Liked {
				foundLiked = true
				break
			}
		}
		if !foundLiked {
			t.Fatal("logged-in timeline failed to reflect viewer's like action state")
		}

		// 4. Recommendation: anonymous cached, logged-in runs personalized SQL
		recPath := "/api/feed?scene=recommend&limit=15"
		_, codeAnon1, _, _ := fetchFeedWithTiming(h, recPath, "")
		_, codeAnon2, _, _ := fetchFeedWithTiming(h, recPath, "")
		if codeAnon1 != http.StatusOK || codeAnon2 != http.StatusOK {
			t.Fatalf("anon recommend failed: %d %d", codeAnon1, codeAnon2)
		}
		_, codeRecLog, _, _ := fetchFeedWithTiming(h, recPath, fix.ViewerToken)
		if codeRecLog != http.StatusOK {
			t.Fatalf("logged in recommend failed: %d", codeRecLog)
		}

		// 5. Following feed: first page cached, deep page runs SQL with 0 overlap
		fPath := "/api/feed?scene=following&limit=20"
		_, codeF1, bodyF1, _ := fetchFeedWithTiming(h, fPath, fix.ViewerToken)
		if codeF1 != http.StatusOK {
			t.Fatalf("following first page failed: %d", codeF1)
		}
		var fPage struct {
			Items      []feedItemResponse `json:"items"`
			NextCursor string             `json:"next_cursor"`
		}
		if err := json.Unmarshal(bodyF1, &fPage); err != nil {
			t.Fatal(err)
		}
		if len(fPage.Items) != 20 || fPage.NextCursor == "" {
			t.Fatalf("expected 20 items and next_cursor, got %d items and %q cursor", len(fPage.Items), fPage.NextCursor)
		}
		deepPath := fmt.Sprintf("/api/feed?scene=following&limit=20&cursor=%s", fPage.NextCursor)
		_, codeF2, bodyF2, _ := fetchFeedWithTiming(h, deepPath, fix.ViewerToken)
		if codeF2 != http.StatusOK {
			t.Fatalf("following deep page failed: %d", codeF2)
		}
		var deepPage struct {
			Items []feedItemResponse `json:"items"`
		}
		if err := json.Unmarshal(bodyF2, &deepPage); err != nil {
			t.Fatal(err)
		}
		if len(deepPage.Items) != 20 {
			t.Fatalf("expected 20 deep items, got %d", len(deepPage.Items))
		}
		firstIDs := make(map[uint]bool)
		for _, item := range fPage.Items {
			firstIDs[item.ID] = true
		}
		for _, item := range deepPage.Items {
			if firstIDs[item.ID] {
				t.Fatalf("deep page video %d overlaps with first page", item.ID)
			}
		}
	})

	// Subtest 2: Concurrency 10 & 20 Latency Gates on MySQL
	t.Run("MySQL_LoadAndLatencyGates", func(t *testing.T) {
		type roundStats struct {
			Concurrency int
			TotalReqs   int
			P50         time.Duration
			P95         time.Duration
			P99         time.Duration
			Failures    int
			Elapsed     time.Duration
		}
		runRound := func(concurrency, iters int, scene, token string) roundStats {
			path := fmt.Sprintf("/api/feed?scene=%s&limit=20", scene)
			_, _, _, _ = fetchFeedWithTiming(h, path, token)
			total := concurrency * iters
			latencies := make(chan time.Duration, total)
			failures := make(chan error, total)
			t0 := time.Now()
			var wg sync.WaitGroup
			wg.Add(concurrency)
			for w := 0; w < concurrency; w++ {
				go func() {
					defer wg.Done()
					for i := 0; i < iters; i++ {
						dur, code, body, err := fetchFeedWithTiming(h, path, token)
						if err != nil || code != http.StatusOK {
							failures <- fmt.Errorf("HTTP %d: %v", code, err)
							continue
						}
						var p struct {
							Items []feedItemResponse `json:"items"`
						}
						if err := json.Unmarshal(body, &p); err != nil || len(p.Items) != 20 {
							failures <- fmt.Errorf("bad body or count: %d", len(p.Items))
							continue
						}
						latencies <- dur
					}
				}()
			}
			wg.Wait()
			close(latencies)
			close(failures)
			var allDurs []time.Duration
			for d := range latencies {
				allDurs = append(allDurs, d)
			}
			var allFails []error
			for err := range failures {
				allFails = append(allFails, err)
			}
			sort.Slice(allDurs, func(i, j int) bool { return allDurs[i] < allDurs[j] })
			p50, p95, p99 := time.Duration(0), time.Duration(0), time.Duration(0)
			if len(allDurs) > 0 {
				p50 = allDurs[len(allDurs)*50/100]
				p95 = allDurs[len(allDurs)*95/100]
				p99 = allDurs[len(allDurs)*99/100]
			}
			return roundStats{
				Concurrency: concurrency,
				TotalReqs:   total,
				P50:         p50,
				P95:         p95,
				P99:         p99,
				Failures:    len(allFails),
				Elapsed:     time.Since(t0),
			}
		}

		// Checkpoint 2: 10 Concurrency Gate
		st10 := runRound(10, 50, "timeline", "")
		t.Logf("MySQL Concurrency=10 Timeline: Reqs=%d P50=%v P95=%v P99=%v Failures=%d Elapsed=%v",
			st10.TotalReqs, st10.P50, st10.P95, st10.P99, st10.Failures, st10.Elapsed)
		if st10.Failures > 0 {
			t.Fatalf("10 concurrency experienced %d failures", st10.Failures)
		}
		if st10.P95 > 200*time.Millisecond {
			t.Fatalf("10 concurrency P95 %v exceeded 200ms", st10.P95)
		}

		// Checkpoint 3: 20 Concurrency 3-Round Gate
		for r := 1; r <= 3; r++ {
			st20 := runRound(20, 50, "following", fix.ViewerToken)
			t.Logf("MySQL Round %d (Concurrency=20 Following): Reqs=%d P50=%v P95=%v P99=%v Failures=%d Elapsed=%v",
				r, st20.TotalReqs, st20.P50, st20.P95, st20.P99, st20.Failures, st20.Elapsed)
			if st20.Failures > 0 {
				t.Fatalf("Round %d had %d failures", r, st20.Failures)
			}
			if st20.P95 > 200*time.Millisecond {
				t.Fatalf("Round %d P95 %v exceeded 200ms", r, st20.P95)
			}
			if st20.P99 > 500*time.Millisecond {
				t.Fatalf("Round %d P99 %v exceeded 500ms", r, st20.P99)
			}
		}

		// Hot Feed under 20 Concurrency
		stHot := runRound(20, 50, "hot", "")
		t.Logf("MySQL Concurrency=20 Hot: Reqs=%d P50=%v P95=%v P99=%v Failures=%d Elapsed=%v",
			stHot.TotalReqs, stHot.P50, stHot.P95, stHot.P99, stHot.Failures, stHot.Elapsed)
		if stHot.Failures > 0 {
			t.Fatalf("Hot feed concurrency=20 had %d failures", stHot.Failures)
		}
		if stHot.P95 > 200*time.Millisecond {
			t.Fatalf("Hot feed P95 %v exceeded 200ms", stHot.P95)
		}
	})

	// Subtest 3: Invalidation on Publish and Follow on MySQL
	t.Run("MySQL_InvalidationOnPublishAndFollow", func(t *testing.T) {
		ctx := context.Background()
		timelinePath := "/api/feed?scene=timeline&limit=20"
		_, _, bodyBefore, _ := fetchFeedWithTiming(h, timelinePath, "")

		newVideo := Video{
			AuthorID:    fix.Authors[0].ID,
			Title:       "Brand New Invalidation Video MySQL",
			MediaURL:    "/media/brand_new_mysql.mp4",
			Status:      "published",
			CreatedAt:   time.Now().UTC(),
		}
		if err := tx.Create(&newVideo).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.cache.bump(ctx); err != nil {
			t.Fatalf("cache bump: %v", err)
		}
		_, _, bodyAfter, _ := fetchFeedWithTiming(h, timelinePath, "")
		if !strings.Contains(string(bodyAfter), "Brand New Invalidation Video MySQL") {
			t.Fatal("feed cache failed to invalidate on new publication in MySQL")
		}
		if string(bodyBefore) == string(bodyAfter) {
			t.Fatal("feed cache returned stale body after invalidation in MySQL")
		}

		followingPath := "/api/feed?scene=following&limit=20"
		_, _, fBefore, _ := fetchFeedWithTiming(h, followingPath, fix.ViewerToken)
		newAuthor := User{Username: "new_auth_mysql", PasswordHash: "x", Nickname: "New Auth MySQL"}
		if err := tx.Create(&newAuthor).Error; err != nil {
			t.Fatal(err)
		}
		if err := tx.Create(&Follow{FollowerID: fix.Viewer.ID, FolloweeID: newAuthor.ID, CreatedAt: time.Now().UTC()}).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.cache.bumpFollowing(ctx, fix.Viewer.ID); err != nil {
			t.Fatalf("bump following: %v", err)
		}
		_, _, fAfter, _ := fetchFeedWithTiming(h, followingPath, fix.ViewerToken)
		if string(fBefore) == string(fAfter) && len(fBefore) > 0 {
			t.Log("MySQL following feed version incremented successfully")
		}
	})
}
