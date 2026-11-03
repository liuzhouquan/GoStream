// Command mixed generates authenticated Feed and interaction traffic against
// an isolated GoStream deployment. It does not create fixtures or clean data.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type operation struct {
	name, method, path, body string
	want                     int
}

type actor struct {
	token      string
	videoID    uint
	followeeID uint
}

type operationReport struct {
	Successes           int         `json:"successes"`
	Failures            int         `json:"failures"`
	Statuses            map[int]int `json:"status_counts"`
	SuccessfulLatencyNS []int64     `json:"successful_latency_ns"`
	FailedLatencyNS     []int64     `json:"failed_latency_ns"`
	P50NS               int64       `json:"p50_ns"`
	P95NS               int64       `json:"p95_ns"`
	P99NS               int64       `json:"p99_ns"`
}

type feedHistogramSnapshot struct {
	Buckets    map[float64]uint64
	Count      uint64
	SumSeconds float64
}

type feedStageHistogramKey struct {
	Scene   string
	Stage   string
	Outcome string
}

type apiHistogramSnapshot struct {
	Following feedHistogramSnapshot
	Stages    map[feedStageHistogramKey]feedHistogramSnapshot
}

type serverLatencyReport struct {
	SampleCount                uint64   `json:"sample_count"`
	SumSeconds                 float64  `json:"sum_seconds"`
	MeanSeconds                float64  `json:"mean_seconds"`
	P95BucketLowerBoundSeconds float64  `json:"p95_bucket_lower_bound_seconds"`
	P95BucketUpperBoundSeconds *float64 `json:"p95_bucket_upper_bound_seconds,omitempty"`
}

type serverFeedStageReport struct {
	Scene   string              `json:"scene"`
	Stage   string              `json:"stage"`
	Outcome string              `json:"outcome"`
	Latency serverLatencyReport `json:"latency"`
}

type loadReport struct {
	Concurrency         int                        `json:"concurrency"`
	ActorCount          int                        `json:"actor_count"`
	WarmupNS            int64                      `json:"warmup_ns"`
	RequestedDurationNS int64                      `json:"requested_duration_ns"`
	ElapsedNS           int64                      `json:"elapsed_ns"`
	Successes           int                        `json:"successes"`
	Failures            int                        `json:"failures"`
	Operations          map[string]operationReport `json:"operations"`
	ServerFollowing     *serverLatencyReport       `json:"server_following_latency,omitempty"`
	ServerFeedStages    []serverFeedStageReport    `json:"server_feed_stages,omitempty"`
}

const maxMetricsResponseBytes = 8 << 20

func scrapeAPIHistograms(client *http.Client, baseURL string) (apiHistogramSnapshot, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/metrics", nil)
	if err != nil {
		return apiHistogramSnapshot{}, fmt.Errorf("build metrics scrape request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return apiHistogramSnapshot{}, fmt.Errorf("scrape API metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiHistogramSnapshot{}, fmt.Errorf("scrape API metrics: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetricsResponseBytes+1))
	if err != nil {
		return apiHistogramSnapshot{}, fmt.Errorf("read API metrics: %w", err)
	}
	if len(data) > maxMetricsResponseBytes {
		return apiHistogramSnapshot{}, fmt.Errorf("API metrics response exceeds %d bytes", maxMetricsResponseBytes)
	}
	exposition := string(data)
	following, err := parseFollowingHistogram(exposition)
	if err != nil {
		return apiHistogramSnapshot{}, err
	}
	stages, err := parseFeedStageHistograms(exposition)
	if err != nil {
		return apiHistogramSnapshot{}, err
	}
	return apiHistogramSnapshot{Following: following, Stages: stages}, nil
}

func scrapeFollowingHistogram(client *http.Client, baseURL string) (feedHistogramSnapshot, error) {
	snapshot, err := scrapeAPIHistograms(client, baseURL)
	if err != nil {
		return feedHistogramSnapshot{}, err
	}
	return snapshot.Following, nil
}

func parseFollowingHistogram(exposition string) (feedHistogramSnapshot, error) {
	snapshot := feedHistogramSnapshot{Buckets: make(map[float64]uint64)}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(exposition))
	if err != nil {
		return feedHistogramSnapshot{}, fmt.Errorf("parse Prometheus exposition: %w", err)
	}
	family := families["gostream_feed_request_duration_seconds"]
	if family == nil {
		return snapshot, nil
	}
	found := false
	for _, metric := range family.GetMetric() {
		labels := make(map[string]string, len(metric.GetLabel()))
		for _, label := range metric.GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}
		if labels["scene"] != "following" || labels["outcome"] != "success" {
			continue
		}
		if found {
			return feedHistogramSnapshot{}, fmt.Errorf("duplicate following success histogram series")
		}
		found = true
		parsed, err := parseMetricHistogram(metric, "following success")
		if err != nil {
			return feedHistogramSnapshot{}, err
		}
		snapshot = parsed
	}
	return snapshot, nil
}

func parseMetricHistogram(metric *dto.Metric, description string) (feedHistogramSnapshot, error) {
	snapshot := feedHistogramSnapshot{Buckets: make(map[float64]uint64)}
	histogram := metric.GetHistogram()
	if histogram == nil || histogram.SampleCount == nil || histogram.SampleSum == nil {
		return feedHistogramSnapshot{}, fmt.Errorf("incomplete %s histogram", description)
	}
	if histogram.GetSampleCountFloat() > 0 {
		return feedHistogramSnapshot{}, fmt.Errorf("floating-point %s histogram counts are unsupported", description)
	}
	snapshot.Count = histogram.GetSampleCount()
	snapshot.SumSeconds = histogram.GetSampleSum()
	for _, bucket := range histogram.GetBucket() {
		bound := bucket.GetUpperBound()
		if math.IsNaN(bound) || math.IsInf(bound, -1) {
			return feedHistogramSnapshot{}, fmt.Errorf("invalid %s histogram boundary %v", description, bound)
		}
		if _, duplicate := snapshot.Buckets[bound]; duplicate {
			return feedHistogramSnapshot{}, fmt.Errorf("duplicate %s histogram boundary %v", description, bound)
		}
		snapshot.Buckets[bound] = bucket.GetCumulativeCount()
	}
	if _, ok := snapshot.Buckets[math.Inf(1)]; !ok {
		snapshot.Buckets[math.Inf(1)] = snapshot.Count
	}
	if snapshot.SumSeconds < 0 {
		return feedHistogramSnapshot{}, fmt.Errorf("negative %s histogram sum", description)
	}
	if snapshot.Buckets[math.Inf(1)] != snapshot.Count {
		return feedHistogramSnapshot{}, fmt.Errorf("%s histogram +Inf bucket does not match count", description)
	}
	boundaries := make([]float64, 0, len(snapshot.Buckets))
	for boundary := range snapshot.Buckets {
		boundaries = append(boundaries, boundary)
	}
	sort.Float64s(boundaries)
	var previous uint64
	for _, boundary := range boundaries {
		count := snapshot.Buckets[boundary]
		if count < previous || count > snapshot.Count {
			return feedHistogramSnapshot{}, fmt.Errorf("%s histogram buckets are not cumulative", description)
		}
		previous = count
	}
	return snapshot, nil
}

func parseFeedStageHistograms(exposition string) (map[feedStageHistogramKey]feedHistogramSnapshot, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(exposition))
	if err != nil {
		return nil, fmt.Errorf("parse Prometheus exposition: %w", err)
	}
	stages := make(map[feedStageHistogramKey]feedHistogramSnapshot)
	family := families["gostream_feed_stage_duration_seconds"]
	if family == nil {
		return stages, nil
	}
	validScenes := map[string]bool{"timeline": true, "following": true, "recommend": true, "hot": true}
	validStages := map[string]bool{"candidate_query": true, "interaction_state": true, "recommendation_context": true, "recommendation_cursor": true}
	validOutcomes := map[string]bool{"success": true, "error": true}
	for _, metric := range family.GetMetric() {
		labels := make(map[string]string, len(metric.GetLabel()))
		for _, label := range metric.GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}
		key := feedStageHistogramKey{Scene: labels["scene"], Stage: labels["stage"], Outcome: labels["outcome"]}
		if len(labels) != 3 || !validScenes[key.Scene] || !validStages[key.Stage] || !validOutcomes[key.Outcome] {
			return nil, fmt.Errorf("unexpected Feed stage histogram labels %v", labels)
		}
		if _, duplicate := stages[key]; duplicate {
			return nil, fmt.Errorf("duplicate Feed stage histogram series %+v", key)
		}
		snapshot, err := parseMetricHistogram(metric, fmt.Sprintf("Feed stage %+v", key))
		if err != nil {
			return nil, err
		}
		stages[key] = snapshot
	}
	return stages, nil
}

func feedStageHistogramDeltaReports(before, after map[feedStageHistogramKey]feedHistogramSnapshot) ([]serverFeedStageReport, error) {
	keys := make([]feedStageHistogramKey, 0, len(after))
	for key := range after {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Scene != keys[j].Scene {
			return keys[i].Scene < keys[j].Scene
		}
		if keys[i].Stage != keys[j].Stage {
			return keys[i].Stage < keys[j].Stage
		}
		return keys[i].Outcome < keys[j].Outcome
	})
	for key := range before {
		if _, ok := after[key]; !ok {
			return nil, fmt.Errorf("Feed stage histogram series disappeared during measurement: %+v", key)
		}
	}
	reports := make([]serverFeedStageReport, 0, len(keys))
	for _, key := range keys {
		latency, err := histogramDeltaReport(before[key], after[key])
		if err != nil {
			return nil, fmt.Errorf("Feed stage histogram %+v: %w", key, err)
		}
		reports = append(reports, serverFeedStageReport{Scene: key.Scene, Stage: key.Stage, Outcome: key.Outcome, Latency: *latency})
	}
	return reports, nil
}

func hasFeedStageSamples(reports []serverFeedStageReport, scene, stage, outcome string) bool {
	for _, report := range reports {
		if report.Scene == scene && report.Stage == stage && report.Outcome == outcome && report.Latency.SampleCount > 0 {
			return true
		}
	}
	return false
}

func histogramDeltaReport(before, after feedHistogramSnapshot) (*serverLatencyReport, error) {
	if after.Count < before.Count || after.SumSeconds+1e-12 < before.SumSeconds {
		return nil, fmt.Errorf("Feed histogram reset during measurement")
	}
	bounds := make([]float64, 0, len(after.Buckets))
	deltaBuckets := make(map[float64]uint64, len(after.Buckets))
	for boundary, count := range after.Buckets {
		prior := before.Buckets[boundary]
		if count < prior {
			return nil, fmt.Errorf("Feed histogram bucket reset during measurement")
		}
		bounds = append(bounds, boundary)
		deltaBuckets[boundary] = count - prior
	}
	sort.Float64s(bounds)
	deltaCount := after.Count - before.Count
	if deltaBuckets[math.Inf(1)] != deltaCount {
		return nil, fmt.Errorf("Feed histogram bucket delta does not match count delta")
	}
	deltaSum := after.SumSeconds - before.SumSeconds
	if deltaSum < 0 {
		deltaSum = 0
	}
	report := &serverLatencyReport{SampleCount: deltaCount, SumSeconds: deltaSum}
	if deltaCount == 0 {
		return report, nil
	}
	report.MeanSeconds = deltaSum / float64(deltaCount)
	rank := deltaCount - deltaCount/20
	var lower float64
	for _, boundary := range bounds {
		if deltaBuckets[boundary] >= rank {
			report.P95BucketLowerBoundSeconds = lower
			if !math.IsInf(boundary, 1) {
				upper := boundary
				report.P95BucketUpperBoundSeconds = &upper
			}
			return report, nil
		}
		if !math.IsInf(boundary, 1) {
			lower = boundary
		}
	}
	return nil, fmt.Errorf("Feed histogram does not contain a bucket for p95")
}

func chooseOperation(sequence uint64, videoID, followeeID uint) operation {
	video := "/api/videos/" + strconv.FormatUint(uint64(videoID), 10)
	follow := "/api/users/me/following/" + strconv.FormatUint(uint64(followeeID), 10)
	switch sequence % 20 {
	case 14:
		return operation{"like", http.MethodPut, video + "/like", "", http.StatusOK}
	case 15:
		return operation{"unlike", http.MethodDelete, video + "/like", "", http.StatusOK}
	case 16:
		return operation{"favorite", http.MethodPut, video + "/favorite", "", http.StatusOK}
	case 17:
		return operation{"unfavorite", http.MethodDelete, video + "/favorite", "", http.StatusOK}
	case 18:
		method := http.MethodPut
		if sequence/20%2 == 1 {
			method = http.MethodDelete
		}
		return operation{"follow", method, follow, "", http.StatusOK}
	case 19:
		return operation{"comment", http.MethodPost, video + "/comments", fmt.Sprintf(`{"body":"mixed load %d"}`, sequence), http.StatusCreated}
	default:
		scene := "following"
		if sequence%20 >= 8 && sequence%20 < 11 {
			scene = "recommend"
		} else if sequence%20 >= 11 {
			scene = "hot"
		}
		return operation{"feed_" + scene, http.MethodGet, "/api/feed?scene=" + scene + "&limit=20", "", http.StatusOK}
	}
}

type sample struct {
	success       []time.Duration
	failedLatency []time.Duration
	failures      int
	statuses      map[int]int
}

func buildActors(tokens []string, videoIDs, followeeIDs []uint) ([]actor, error) {
	if len(tokens) == 0 || len(videoIDs) == 0 || len(followeeIDs) == 0 {
		return nil, fmt.Errorf("at least one token, video ID, and followee ID are required")
	}
	for i, token := range tokens {
		if strings.TrimSpace(token) == "" {
			return nil, fmt.Errorf("token %d is empty", i)
		}
	}
	for i, id := range videoIDs {
		if id == 0 {
			return nil, fmt.Errorf("video ID %d must be positive", i)
		}
	}
	for i, id := range followeeIDs {
		if id == 0 {
			return nil, fmt.Errorf("followee ID %d must be positive", i)
		}
	}
	actors := make([]actor, len(tokens))
	for i, token := range tokens {
		actors[i] = actor{token: strings.TrimSpace(token), videoID: videoIDs[i%len(videoIDs)], followeeID: followeeIDs[i%len(followeeIDs)]}
	}
	return actors, nil
}

func parseIDList(value string, fallback uint, label string) ([]uint, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		if fallback == 0 {
			return nil, fmt.Errorf("at least one %s is required", label)
		}
		return []uint{fallback}, nil
	}
	parts := strings.Split(value, ",")
	ids := make([]uint, 0, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		parsed, err := strconv.ParseUint(part, 10, 32)
		if err != nil || parsed == 0 {
			return nil, fmt.Errorf("invalid %s at position %d: %q", label, i+1, part)
		}
		ids = append(ids, uint(parsed))
	}
	return ids, nil
}

func writeReport(path string, report loadReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode load report: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write load report: %w", err)
	}
	return nil
}

func runRequest(client *http.Client, baseURL, token string, op operation) (time.Duration, int, error) {
	var body io.Reader
	if op.body != "" {
		body = bytes.NewBufferString(op.body)
	}
	req, err := http.NewRequest(op.method, baseURL+op.path, body)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if op.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return time.Since(start), 0, err
	}
	_, readErr := io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return time.Since(start), res.StatusCode, readErr
}

func percentile(sorted []time.Duration, numerator int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[(len(sorted)-1)*numerator/100]
}

func run(baseURL string, actors []actor, concurrency int, warmup, duration time.Duration, reportPath string) error {
	if len(actors) == 0 {
		return fmt.Errorf("at least one actor is required")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		MaxIdleConns: concurrency, MaxIdleConnsPerHost: concurrency,
	}}
	defer client.CloseIdleConnections()
	if warmup > 0 {
		deadline := time.Now().Add(warmup)
		var wg sync.WaitGroup
		var warmFailures atomic.Int64
		for worker := 0; worker < concurrency; worker++ {
			workerActor := actors[worker%len(actors)]
			wg.Add(1)
			go func(token string) {
				defer wg.Done()
				for time.Now().Before(deadline) {
					_, status, err := runRequest(client, baseURL, token, operation{"feed_following", http.MethodGet, "/api/feed?scene=following&limit=20", "", http.StatusOK})
					if err != nil || status != http.StatusOK {
						warmFailures.Add(1)
					}
				}
			}(workerActor.token)
		}
		wg.Wait()
		if warmFailures.Load() != 0 {
			return fmt.Errorf("warmup had %d failed requests", warmFailures.Load())
		}
	}
	serverHistogramBefore, err := scrapeAPIHistograms(client, baseURL)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(duration)
	start := time.Now()
	var sequence atomic.Uint64
	results := make(chan map[string]*sample, concurrency)
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workerActor := actors[worker%len(actors)]
		wg.Add(1)
		go func(currentActor actor) {
			defer wg.Done()
			local := make(map[string]*sample)
			for time.Now().Before(deadline) {
				op := chooseOperation(sequence.Add(1)-1, currentActor.videoID, currentActor.followeeID)
				value := local[op.name]
				if value == nil {
					value = &sample{statuses: make(map[int]int)}
					local[op.name] = value
				}
				latency, status, err := runRequest(client, baseURL, currentActor.token, op)
				value.statuses[status]++
				if err != nil || status != op.want {
					value.failures++
					value.failedLatency = append(value.failedLatency, latency)
				} else {
					value.success = append(value.success, latency)
				}
			}
			results <- local
		}(workerActor)
	}
	wg.Wait()
	elapsed := time.Since(start)
	serverHistogramAfter, err := scrapeAPIHistograms(client, baseURL)
	if err != nil {
		return err
	}
	serverFollowing, err := histogramDeltaReport(serverHistogramBefore.Following, serverHistogramAfter.Following)
	if err != nil {
		return err
	}
	if serverFollowing.SampleCount == 0 {
		return fmt.Errorf("measurement contained no successful following Feed samples")
	}
	serverFeedStages, err := feedStageHistogramDeltaReports(serverHistogramBefore.Stages, serverHistogramAfter.Stages)
	if err != nil {
		return err
	}
	if !hasFeedStageSamples(serverFeedStages, "following", "candidate_query", "success") ||
		!hasFeedStageSamples(serverFeedStages, "following", "interaction_state", "success") {
		return fmt.Errorf("measurement did not contain successful following Feed candidate and interaction stages")
	}
	fmt.Printf("server_following count=%d mean=%s p95_bucket=(%s,", serverFollowing.SampleCount,
		time.Duration(serverFollowing.MeanSeconds*float64(time.Second)).Round(time.Microsecond),
		time.Duration(serverFollowing.P95BucketLowerBoundSeconds*float64(time.Second)).Round(time.Microsecond))
	if serverFollowing.P95BucketUpperBoundSeconds == nil {
		fmt.Println("+Inf]")
	} else {
		fmt.Printf("%s]\n", time.Duration(*serverFollowing.P95BucketUpperBoundSeconds*float64(time.Second)).Round(time.Microsecond))
	}
	for _, stage := range serverFeedStages {
		fmt.Printf("server_feed_stage scene=%s stage=%s outcome=%s count=%d mean=%s p95_bucket=(%s,", stage.Scene, stage.Stage, stage.Outcome,
			stage.Latency.SampleCount, time.Duration(stage.Latency.MeanSeconds*float64(time.Second)).Round(time.Microsecond),
			time.Duration(stage.Latency.P95BucketLowerBoundSeconds*float64(time.Second)).Round(time.Microsecond))
		if stage.Latency.P95BucketUpperBoundSeconds == nil {
			fmt.Println("+Inf]")
		} else {
			fmt.Printf("%s]\n", time.Duration(*stage.Latency.P95BucketUpperBoundSeconds*float64(time.Second)).Round(time.Microsecond))
		}
	}
	close(results)
	merged := make(map[string]*sample)
	var all []time.Duration
	var failed int
	var successes int
	operationReports := make(map[string]operationReport)
	for local := range results {
		for name, result := range local {
			value := merged[name]
			if value == nil {
				value = &sample{statuses: make(map[int]int)}
				merged[name] = value
			}
			value.success = append(value.success, result.success...)
			value.failedLatency = append(value.failedLatency, result.failedLatency...)
			value.failures += result.failures
			for status, count := range result.statuses {
				value.statuses[status] += count
			}
		}
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := merged[name]
		sort.Slice(value.success, func(i, j int) bool { return value.success[i] < value.success[j] })
		sort.Slice(value.failedLatency, func(i, j int) bool { return value.failedLatency[i] < value.failedLatency[j] })
		all = append(all, value.success...)
		failed += value.failures
		successes += len(value.success)
		fmt.Printf("operation=%s success=%d failures=%d p50=%s p95=%s p99=%s statuses=%v\n", name, len(value.success), value.failures,
			percentile(value.success, 50).Round(time.Microsecond), percentile(value.success, 95).Round(time.Microsecond), percentile(value.success, 99).Round(time.Microsecond), value.statuses)
		operationReports[name] = operationReport{
			Successes: len(value.success), Failures: value.failures, Statuses: value.statuses,
			SuccessfulLatencyNS: durationValues(value.success), FailedLatencyNS: durationValues(value.failedLatency),
			P50NS: int64(percentile(value.success, 50)), P95NS: int64(percentile(value.success, 95)), P99NS: int64(percentile(value.success, 99)),
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	fmt.Printf("total success=%d failures=%d concurrency=%d elapsed=%s qps=%.1f p50=%s p95=%s p99=%s\n", len(all), failed, concurrency,
		elapsed.Round(time.Millisecond), float64(len(all))/elapsed.Seconds(), percentile(all, 50).Round(time.Microsecond),
		percentile(all, 95).Round(time.Microsecond), percentile(all, 99).Round(time.Microsecond))
	if reportPath != "" {
		report := loadReport{
			Concurrency: concurrency, ActorCount: len(actors), WarmupNS: int64(warmup), RequestedDurationNS: int64(duration),
			ElapsedNS: int64(elapsed), Successes: successes, Failures: failed, Operations: operationReports,
			ServerFollowing: serverFollowing, ServerFeedStages: serverFeedStages,
		}
		if err := writeReport(reportPath, report); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d measured requests failed", failed)
	}
	return nil
}

func main() {
	baseURL := flag.String("base-url", "", "isolated API origin")
	isolated := flag.Bool("isolated", false, "confirm this is a disposable fixture; the driver writes actions and comments")
	tokenFile := flag.String("token-file", "", "path to a JWT access token file")
	videoID := flag.Uint("video-id", 0, "published video ID for writes")
	followeeID := flag.Uint("followee-id", 0, "other user ID for follow/unfollow")
	videoIDsValue := flag.String("video-ids", "", "comma-separated published video IDs; distributed across token lines")
	followeeIDsValue := flag.String("followee-ids", "", "comma-separated followee user IDs; distributed across token lines")
	concurrency := flag.Int("concurrency", 20, "concurrent workers")
	warmup := flag.Duration("warmup", 30*time.Second, "read-only Feed warmup")
	duration := flag.Duration("duration", time.Minute, "measured mixed workload")
	reportPath := flag.String("report-file", "", "optional path for raw per-request JSON report (created with mode 0600)")
	flag.Parse()
	if !*isolated || *tokenFile == "" || *concurrency < 1 || *concurrency > 1000 || *warmup < 0 || *duration <= 0 ||
		!strings.HasPrefix(*baseURL, "http://") && !strings.HasPrefix(*baseURL, "https://") {
		fmt.Fprintln(os.Stderr, "-isolated, token-file, positive duration and concurrency (<=1000), and HTTP(S) base-url are required")
		os.Exit(2)
	}
	data, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	tokens := strings.Fields(string(data))
	videoIDs, err := parseIDList(*videoIDsValue, *videoID, "video ID")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	followeeIDs, err := parseIDList(*followeeIDsValue, *followeeID, "followee ID")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	actors, err := buildActors(tokens, videoIDs, followeeIDs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(strings.TrimRight(*baseURL, "/"), actors, *concurrency, *warmup, *duration, *reportPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func durationValues(values []time.Duration) []int64 {
	result := make([]int64, len(values))
	for i, value := range values {
		result[i] = int64(value)
	}
	return result
}
