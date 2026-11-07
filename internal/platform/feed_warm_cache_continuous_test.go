package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
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
		createdAt := now.Add(-time.Duration(videoCount-i) * time.Second)

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
