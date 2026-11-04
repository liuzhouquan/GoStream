package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseFollowingHistogramAndDeltaReport(t *testing.T) {
	beforeText := `# TYPE gostream_feed_request_duration_seconds histogram
gostream_feed_request_duration_seconds_bucket{scene="following",outcome="success",le="0.01"} 2
gostream_feed_request_duration_seconds_bucket{scene="following",outcome="success",le="0.05"} 4
gostream_feed_request_duration_seconds_bucket{scene="following",outcome="success",le="+Inf"} 4
gostream_feed_request_duration_seconds_sum{scene="following",outcome="success"} 0.12
gostream_feed_request_duration_seconds_count{scene="following",outcome="success"} 4
gostream_feed_request_duration_seconds_count{scene="recommend",outcome="success"} 500
`
	afterText := `# TYPE gostream_feed_request_duration_seconds histogram
gostream_feed_request_duration_seconds_bucket{scene="following",outcome="success",le="0.01"} 4
gostream_feed_request_duration_seconds_bucket{scene="following",outcome="success",le="0.05"} 10
gostream_feed_request_duration_seconds_bucket{scene="following",outcome="success",le="+Inf"} 10
gostream_feed_request_duration_seconds_sum{scene="following",outcome="success"} 0.47
gostream_feed_request_duration_seconds_count{scene="following",outcome="success"} 10
`
	before, err := parseFollowingHistogram(beforeText)
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseFollowingHistogram(afterText)
	if err != nil {
		t.Fatal(err)
	}
	got, err := histogramDeltaReport(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if got.SampleCount != 6 || got.SumSeconds < 0.349999 || got.SumSeconds > 0.350001 {
		t.Fatalf("unexpected histogram delta: %+v", got)
	}
	if got.MeanSeconds < 0.058333 || got.MeanSeconds > 0.058334 {
		t.Fatalf("mean = %.9f, want about 0.058333 seconds", got.MeanSeconds)
	}
	if got.P95BucketLowerBoundSeconds != 0.01 || got.P95BucketUpperBoundSeconds == nil || *got.P95BucketUpperBoundSeconds != 0.05 {
		t.Fatalf("p95 bucket = [%v,%v], want [0.01,0.05]", got.P95BucketLowerBoundSeconds, got.P95BucketUpperBoundSeconds)
	}
}

func TestParseFollowingHistogramRejectsInconsistentSamples(t *testing.T) {
	for name, exposition := range map[string]string{
		"count without buckets": `# TYPE gostream_feed_request_duration_seconds histogram
gostream_feed_request_duration_seconds_count{scene="following",outcome="success"} 3`,
		"fractional count": `# TYPE gostream_feed_request_duration_seconds histogram
gostream_feed_request_duration_seconds_count{scene="following",outcome="success"} 1.5`,
		"duplicate bucket": `# TYPE gostream_feed_request_duration_seconds histogram
gostream_feed_request_duration_seconds_bucket{scene="following",outcome="success",le="0.01"} 1
gostream_feed_request_duration_seconds_bucket{scene="following",outcome="success",le="0.01"} 2`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFollowingHistogram(exposition); err == nil {
				t.Fatal("expected malformed histogram to fail")
			}
		})
	}
}

func TestHistogramDeltaReportRejectsReset(t *testing.T) {
	before := feedHistogramSnapshot{Count: 8, SumSeconds: 0.8, Buckets: map[float64]uint64{0.05: 8, math.Inf(1): 8}}
	after := feedHistogramSnapshot{Count: 3, SumSeconds: 0.3, Buckets: map[float64]uint64{0.05: 3, math.Inf(1): 3}}
	if _, err := histogramDeltaReport(before, after); err == nil {
		t.Fatal("expected process reset to be reported")
	}
}

func TestDatabasePoolWaitMetricsReportMeasurementDelta(t *testing.T) {
	beforeText := `# TYPE gostream_database_pool_wait_count_total counter
gostream_database_pool_wait_count_total 5
# TYPE gostream_database_pool_wait_duration_seconds_total counter
gostream_database_pool_wait_duration_seconds_total 0.25
# TYPE gostream_database_pool_max_idle_closed_total counter
gostream_database_pool_max_idle_closed_total 11
`
	afterText := `# TYPE gostream_database_pool_wait_count_total counter
gostream_database_pool_wait_count_total 12
# TYPE gostream_database_pool_wait_duration_seconds_total counter
gostream_database_pool_wait_duration_seconds_total 0.75
# TYPE gostream_database_pool_max_idle_closed_total counter
gostream_database_pool_max_idle_closed_total 18
`
	before, err := parseDatabasePoolWaitMetrics(beforeText)
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseDatabasePoolWaitMetrics(afterText)
	if err != nil {
		t.Fatal(err)
	}
	got, err := databasePoolWaitDeltaReport(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if got.WaitCount != 7 || math.Abs(got.WaitDurationSeconds-0.5) > 1e-9 || got.MaxIdleClosed != 7 {
		t.Fatalf("database pool delta = %+v, want 7 waits, 0.5 seconds and 7 max-idle closures", got)
	}
}

func TestDatabasePoolWaitMetricsRejectReset(t *testing.T) {
	before := databasePoolWaitSnapshot{WaitCount: 10, WaitDurationSeconds: 1.5, MaxIdleClosed: 8}
	after := databasePoolWaitSnapshot{WaitCount: 2, WaitDurationSeconds: 0.5, MaxIdleClosed: 7}
	if _, err := databasePoolWaitDeltaReport(before, after); err == nil {
		t.Fatal("expected database pool counter reset to be reported")
	}
}

func TestDatabasePoolWaitMetricsRejectIdleCloseCounterReset(t *testing.T) {
	before := databasePoolWaitSnapshot{WaitCount: 1, WaitDurationSeconds: 0.1, MaxIdleClosed: 10}
	after := databasePoolWaitSnapshot{WaitCount: 2, WaitDurationSeconds: 0.2, MaxIdleClosed: 3}
	if _, err := databasePoolWaitDeltaReport(before, after); err == nil {
		t.Fatal("expected max idle closed counter reset to be reported")
	}
}

func TestDatabasePoolWaitMetricsRejectCounterOutsideUint64Range(t *testing.T) {
	text := `# TYPE gostream_database_pool_wait_count_total counter
gostream_database_pool_wait_count_total 18446744073709551616
# TYPE gostream_database_pool_wait_duration_seconds_total counter
gostream_database_pool_wait_duration_seconds_total 0
# TYPE gostream_database_pool_max_idle_closed_total counter
gostream_database_pool_max_idle_closed_total 0
`
	if _, err := parseDatabasePoolWaitMetrics(text); err == nil {
		t.Fatal("expected a wait count outside uint64 range to be rejected")
	}
}

func TestFeedStageHistogramDeltasAreReportedByBoundedKey(t *testing.T) {
	beforeText := `# TYPE gostream_feed_stage_duration_seconds histogram
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="candidate_query",le="0.01"} 2
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="candidate_query",le="0.05"} 4
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="candidate_query",le="+Inf"} 4
gostream_feed_stage_duration_seconds_sum{outcome="success",scene="following",stage="candidate_query"} 0.12
gostream_feed_stage_duration_seconds_count{outcome="success",scene="following",stage="candidate_query"} 4
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="interaction_state",le="0.01"} 1
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="interaction_state",le="0.05"} 1
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="interaction_state",le="+Inf"} 1
gostream_feed_stage_duration_seconds_sum{outcome="success",scene="following",stage="interaction_state"} 0.02
gostream_feed_stage_duration_seconds_count{outcome="success",scene="following",stage="interaction_state"} 1
`
	afterText := `# TYPE gostream_feed_stage_duration_seconds histogram
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="candidate_query",le="0.01"} 4
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="candidate_query",le="0.05"} 10
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="candidate_query",le="+Inf"} 10
gostream_feed_stage_duration_seconds_sum{outcome="success",scene="following",stage="candidate_query"} 0.47
gostream_feed_stage_duration_seconds_count{outcome="success",scene="following",stage="candidate_query"} 10
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="interaction_state",le="0.01"} 2
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="interaction_state",le="0.05"} 5
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="interaction_state",le="+Inf"} 5
gostream_feed_stage_duration_seconds_sum{outcome="success",scene="following",stage="interaction_state"} 0.14
gostream_feed_stage_duration_seconds_count{outcome="success",scene="following",stage="interaction_state"} 5
`
	before, err := parseFeedStageHistograms(beforeText)
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseFeedStageHistograms(afterText)
	if err != nil {
		t.Fatal(err)
	}
	got, err := feedStageHistogramDeltaReports(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Stage != "candidate_query" || got[0].Latency.SampleCount != 6 ||
		got[0].Latency.P95BucketLowerBoundSeconds != 0.01 || got[0].Latency.P95BucketUpperBoundSeconds == nil || *got[0].Latency.P95BucketUpperBoundSeconds != 0.05 {
		t.Fatalf("candidate-query stage delta = %+v", got)
	}
	if got[1].Stage != "interaction_state" || got[1].Latency.SampleCount != 4 || got[1].Outcome != "success" {
		t.Fatalf("interaction-state stage delta = %+v", got[1])
	}
}

func TestFeedStageHistogramAcceptsCandidateSubstageLabels(t *testing.T) {
	exposition := `# TYPE gostream_feed_stage_duration_seconds histogram
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="candidate_sql",le="0.025"} 1
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="candidate_sql",le="+Inf"} 1
gostream_feed_stage_duration_seconds_sum{outcome="success",scene="following",stage="candidate_sql"} 0.02
gostream_feed_stage_duration_seconds_count{outcome="success",scene="following",stage="candidate_sql"} 1
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="author_preload",le="0.01"} 2
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="author_preload",le="+Inf"} 2
gostream_feed_stage_duration_seconds_sum{outcome="success",scene="following",stage="author_preload"} 0.015
gostream_feed_stage_duration_seconds_count{outcome="success",scene="following",stage="author_preload"} 2
`
	stages, err := parseFeedStageHistograms(exposition)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 {
		t.Fatalf("parsed substages = %+v, want candidate_sql and author_preload", stages)
	}
	if _, ok := stages[feedStageHistogramKey{Scene: "following", Stage: "candidate_sql", Outcome: "success"}]; !ok {
		t.Fatal("candidate_sql histogram missing")
	}
	if _, ok := stages[feedStageHistogramKey{Scene: "following", Stage: "author_preload", Outcome: "success"}]; !ok {
		t.Fatal("author_preload histogram missing")
	}
}

func TestFeedStageHistogramRejectsUnknownLabels(t *testing.T) {
	text := `# TYPE gostream_feed_stage_duration_seconds histogram
gostream_feed_stage_duration_seconds_bucket{outcome="success",scene="following",stage="viewer-123",le="+Inf"} 1
gostream_feed_stage_duration_seconds_sum{outcome="success",scene="following",stage="viewer-123"} 0.01
gostream_feed_stage_duration_seconds_count{outcome="success",scene="following",stage="viewer-123"} 1
`
	if _, err := parseFeedStageHistograms(text); err == nil {
		t.Fatal("expected an unknown stage label to be rejected")
	}
}

func TestChooseOperationWeightsAndMethods(t *testing.T) {
	counts := make(map[string]int)
	for i := uint64(0); i < 40; i++ {
		op := chooseOperation(i, 12, 34)
		counts[op.name]++
		if op.path == "" || op.want == 0 {
			t.Fatalf("incomplete operation %d: %+v", i, op)
		}
		if i%20 == 14 && (op.method != http.MethodPut || op.path != "/api/videos/12/like") {
			t.Fatalf("like operation %d: %+v", i, op)
		}
		if i%20 == 15 && op.method != http.MethodDelete {
			t.Fatalf("unlike operation %d: %+v", i, op)
		}
		if i == 18 && op.method != http.MethodPut || i == 38 && op.method != http.MethodDelete {
			t.Fatalf("follow toggle %d: %+v", i, op)
		}
	}
	feed, interaction, social := 0, 0, 0
	for name, count := range counts {
		switch name {
		case "feed_following", "feed_recommend", "feed_hot":
			feed += count
		case "like", "unlike", "favorite", "unfavorite":
			interaction += count
		case "follow", "comment":
			social += count
		default:
			t.Fatalf("unexpected operation %s", name)
		}
	}
	if feed != 28 || interaction != 8 || social != 4 {
		t.Fatalf("weights: feed=%d interaction=%d social=%d", feed, interaction, social)
	}
}

func TestBuildActorsRotatesTokensAndTargets(t *testing.T) {
	actors, err := buildActors([]string{"token-a", "token-b", "token-c"}, []uint{11, 22}, []uint{31, 32, 33})
	if err != nil {
		t.Fatal(err)
	}
	want := []actor{
		{token: "token-a", videoID: 11, followeeID: 31},
		{token: "token-b", videoID: 22, followeeID: 32},
		{token: "token-c", videoID: 11, followeeID: 33},
	}
	if len(actors) != len(want) {
		t.Fatalf("got %d actors, want %d", len(actors), len(want))
	}
	for i := range want {
		if actors[i] != want[i] {
			t.Errorf("actor %d = %+v, want %+v", i, actors[i], want[i])
		}
	}
}

func TestParseIDListAndFallback(t *testing.T) {
	got, err := parseIDList(" 12, 34,56 ", 0, "video ID")
	if err != nil || len(got) != 3 || got[0] != 12 || got[1] != 34 || got[2] != 56 {
		t.Fatalf("parsed IDs=%v err=%v", got, err)
	}
	got, err = parseIDList("", 78, "video ID")
	if err != nil || len(got) != 1 || got[0] != 78 {
		t.Fatalf("fallback IDs=%v err=%v", got, err)
	}
	for _, input := range []string{"0", "1,", "-1", "x"} {
		if _, err := parseIDList(input, 0, "video ID"); err == nil {
			t.Errorf("parseIDList(%q) unexpectedly succeeded", input)
		}
	}
}

func TestBuildActorsRejectsMissingOrInvalidInputs(t *testing.T) {
	for name, input := range map[string]struct {
		tokens    []string
		videos    []uint
		followees []uint
	}{
		"no tokens":     {videos: []uint{1}, followees: []uint{2}},
		"blank token":   {tokens: []string{"  "}, videos: []uint{1}, followees: []uint{2}},
		"no videos":     {tokens: []string{"token"}, followees: []uint{2}},
		"zero video":    {tokens: []string{"token"}, videos: []uint{0}, followees: []uint{2}},
		"no followees":  {tokens: []string{"token"}, videos: []uint{1}},
		"zero followee": {tokens: []string{"token"}, videos: []uint{1}, followees: []uint{0}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildActors(input.tokens, input.videos, input.followees); err == nil {
				t.Fatal("expected invalid actor input to fail")
			}
		})
	}
}

func TestWriteReportPreservesRawSuccessfulAndFailedSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.json")
	report := loadReport{
		Concurrency: 2,
		ActorCount:  2,
		Operations: map[string]operationReport{
			"like": {Successes: 1, Failures: 1, Statuses: map[int]int{0: 1, 200: 1}, SuccessfulLatencyNS: []int64{1234}, FailedLatencyNS: []int64{5678}},
		},
	}
	if err := writeReport(path, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got loadReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	operation := got.Operations["like"]
	if operation.Successes != 1 || operation.Failures != 1 || operation.Statuses[0] != 1 || operation.Statuses[200] != 1 {
		t.Fatalf("unexpected operation counts: %+v", operation)
	}
	if len(operation.SuccessfulLatencyNS) != 1 || operation.SuccessfulLatencyNS[0] != 1234 || len(operation.FailedLatencyNS) != 1 || operation.FailedLatencyNS[0] != 5678 {
		t.Fatalf("raw request latencies were not preserved: %+v", operation)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("report permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadReportPercentilesUseSuccessfulSamples(t *testing.T) {
	got := percentile([]time.Duration{time.Millisecond, 2 * time.Millisecond, 10 * time.Millisecond}, 95)
	if got != 2*time.Millisecond {
		t.Fatalf("p95 = %s, want existing nearest-rank behavior of 2ms", got)
	}
}

func TestRunUsesActorPoolAndWritesRequestReport(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]int)
	var followingCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			mu.Lock()
			count := followingCount
			mu.Unlock()
			fmt.Fprintln(w, "# TYPE gostream_feed_request_duration_seconds histogram")
			fmt.Fprintf(w, "gostream_feed_request_duration_seconds_bucket{scene=\"following\",outcome=\"success\",le=\"0.05\"} %d\n", count)
			fmt.Fprintf(w, "gostream_feed_request_duration_seconds_bucket{scene=\"following\",outcome=\"success\",le=\"+Inf\"} %d\n", count)
			fmt.Fprintf(w, "gostream_feed_request_duration_seconds_sum{scene=\"following\",outcome=\"success\"} %.3f\n", float64(count)*0.01)
			fmt.Fprintf(w, "gostream_feed_request_duration_seconds_count{scene=\"following\",outcome=\"success\"} %d\n", count)
			fmt.Fprintln(w, "# TYPE gostream_database_pool_wait_count_total counter")
			fmt.Fprintf(w, "gostream_database_pool_wait_count_total %d\n", count)
			fmt.Fprintln(w, "# TYPE gostream_database_pool_wait_duration_seconds_total counter")
			fmt.Fprintf(w, "gostream_database_pool_wait_duration_seconds_total %.3f\n", float64(count)*0.002)
			fmt.Fprintln(w, "# TYPE gostream_database_pool_max_idle_closed_total counter")
			fmt.Fprintf(w, "gostream_database_pool_max_idle_closed_total %d\n", count/2)
			fmt.Fprintln(w, "# TYPE gostream_feed_stage_duration_seconds histogram")
			for _, stage := range []string{
				"candidate_query", "candidate_sql", "candidate_query_start", "candidate_row_next", "candidate_row_scan",
				"author_preload", "interaction_state",
			} {
				fmt.Fprintf(w, "gostream_feed_stage_duration_seconds_bucket{scene=\"following\",stage=\"%s\",outcome=\"success\",le=\"0.05\"} %d\n", stage, count)
				fmt.Fprintf(w, "gostream_feed_stage_duration_seconds_bucket{scene=\"following\",stage=\"%s\",outcome=\"success\",le=\"+Inf\"} %d\n", stage, count)
				fmt.Fprintf(w, "gostream_feed_stage_duration_seconds_sum{scene=\"following\",stage=\"%s\",outcome=\"success\"} %.3f\n", stage, float64(count)*0.01)
				fmt.Fprintf(w, "gostream_feed_stage_duration_seconds_count{scene=\"following\",stage=\"%s\",outcome=\"success\"} %d\n", stage, count)
			}
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[token]++
		if r.URL.Path == "/api/feed" && r.URL.Query().Get("scene") == "following" {
			followingCount++
		}
		mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	actors, err := buildActors([]string{"token-a", "token-b"}, []uint{1, 2}, []uint{3, 4})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "load.json")
	if err := run(server.URL, actors, 2, 0, 100*time.Millisecond, path, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["token-a"] == 0 || seen["token-b"] == 0 {
		t.Fatalf("workers did not use the full actor pool: %v", seen)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report loadReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.ActorCount != 2 || report.Concurrency != 2 || report.Successes == 0 || report.Failures != 0 {
		t.Fatalf("unexpected mixed workload report: %+v", report)
	}
	if report.ServerFollowing == nil || report.ServerFollowing.SampleCount == 0 || report.ServerFollowing.P95BucketUpperBoundSeconds == nil {
		t.Fatalf("following server histogram missing from report: %+v", report.ServerFollowing)
	}
	if len(report.ServerFeedStages) != 7 || !hasFeedStageSamples(report.ServerFeedStages, "following", "candidate_query", "success") ||
		!hasFeedStageSamples(report.ServerFeedStages, "following", "candidate_sql", "success") ||
		!hasFeedStageSamples(report.ServerFeedStages, "following", "candidate_query_start", "success") ||
		!hasFeedStageSamples(report.ServerFeedStages, "following", "candidate_row_next", "success") ||
		!hasFeedStageSamples(report.ServerFeedStages, "following", "candidate_row_scan", "success") ||
		!hasFeedStageSamples(report.ServerFeedStages, "following", "author_preload", "success") ||
		!hasFeedStageSamples(report.ServerFeedStages, "following", "interaction_state", "success") {
		t.Fatalf("following Feed stage histograms missing from report: %+v", report.ServerFeedStages)
	}
	if report.DatabasePoolWait == nil || report.DatabasePoolWait.WaitCount == 0 || report.DatabasePoolWait.WaitDurationSeconds <= 0 {
		t.Fatalf("database/sql pool wait delta missing from report: %+v", report.DatabasePoolWait)
	}
	for name, operation := range report.Operations {
		if operation.Successes == 0 || len(operation.SuccessfulLatencyNS) != operation.Successes {
			t.Errorf("operation %q has incomplete samples: %+v", name, operation)
		}
		wantStatus := http.StatusOK
		if name == "comment" {
			wantStatus = http.StatusCreated
		}
		if operation.Statuses[wantStatus] != operation.Successes {
			t.Errorf("operation %q successful statuses = %v, want %d responses with HTTP %d", name, operation.Statuses, operation.Successes, wantStatus)
		}
	}
}

func TestRunWarmupOnlySkipsMeasuredWorkloadAndMetricsScrape(t *testing.T) {
	feedCalls := 0
	metricsCalls := 0
	writeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/feed":
			feedCalls++
			if r.URL.Query().Get("scene") != "following" {
				t.Errorf("warmup requested unexpected scene: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[]}`))
		case "/metrics":
			metricsCalls++
		case "/api/videos/1/like", "/api/videos/1/comments", "/api/users/me/following/3":
			writeCalls++
		default:
			t.Errorf("unexpected warmup request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	actors, err := buildActors([]string{"reader-token"}, []uint{1}, []uint{3})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(server.URL, actors, 1, 40*time.Millisecond, time.Millisecond, "", true); err != nil {
		t.Fatal(err)
	}
	if feedCalls == 0 || metricsCalls != 0 || writeCalls != 0 {
		t.Fatalf("warmup-only did not stop at the phase boundary: feed=%d metrics=%d writes=%d", feedCalls, metricsCalls, writeCalls)
	}
}
