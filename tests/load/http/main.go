package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/api/feed?scene=timeline&limit=20", "full Feed URL")
	tokenFile := flag.String("token-file", "", "optional path to a JWT access token file for authenticated Feed requests")
	requests := flag.Int("requests", 3000, "measured HTTP requests")
	concurrency := flag.Int("concurrency", 10, "concurrent workers")
	warmup := flag.Duration("warmup", 0, "concurrent warmup duration; fixed-request mode defaults to 20 sequential warmups")
	duration := flag.Duration("duration", 0, "measured load duration; when set, requests are sent continuously until this window ends")
	reportFile := flag.String("report-file", "", "optional JSON report path; includes successful request latency samples")
	flag.Parse()
	if *concurrency < 1 || *concurrency > 1000 || *duration < 0 || *warmup < 0 || (*duration == 0 && *requests < 1) {
		log.Fatal("concurrency must be 1-1000; duration and warmup must be non-negative; requests must be positive in fixed-request mode")
	}
	token := ""
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatal(err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			log.Fatal("token file is empty")
		}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConns: *concurrency, MaxIdleConnsPerHost: *concurrency}}
	defer client.CloseIdleConnections()

	var warmResult loadResult
	warmupRan := false
	if *warmup > 0 {
		warmResult = runFor(client, *url, token, *concurrency, *warmup, false)
		warmupRan = true
	} else if *duration == 0 {
		warmResult = runCount(client, *url, token, 1, 20, false)
		warmupRan = true
	}
	if warmupRan && (warmResult.Failures > 0 || warmResult.Successes == 0) {
		log.Fatalf("warmup failed: successes=%d failures=%d statuses=%v", warmResult.Successes, warmResult.Failures, warmResult.Statuses)
	}

	var result loadResult
	if *duration > 0 {
		result = runFor(client, *url, token, *concurrency, *duration, true)
	} else {
		result = runCount(client, *url, token, *concurrency, *requests, true)
	}
	printResult(*url, *concurrency, result)
	if *reportFile != "" {
		if err := writeReport(*reportFile, *url, *concurrency, result); err != nil {
			log.Fatal(err)
		}
	}
	if result.Failures > 0 || result.Successes == 0 {
		log.Fatalf("load run failed: successes=%d failures=%d statuses=%v", result.Successes, result.Failures, result.Statuses)
	}
}

func getFeed(client *http.Client, url, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req)
}

type loadResult struct {
	Successes int64
	Failures  int64
	Bytes     int64
	Statuses  map[int]int64
	Latencies []time.Duration
	Elapsed   time.Duration
	Drain     time.Duration
}

func runFor(client *http.Client, url, token string, concurrency int, duration time.Duration, record bool) loadResult {
	started := time.Now()
	deadline := started.Add(duration)
	results := make(chan loadResult, concurrency)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			local := loadResult{Statuses: make(map[int]int64)}
			for time.Now().Before(deadline) {
				latency, bytes, status, err := requestFeed(client, url, token)
				local.Statuses[status]++
				local.Bytes += bytes
				if err != nil {
					local.Failures++
					continue
				}
				local.Successes++
				if record {
					local.Latencies = append(local.Latencies, latency)
				}
			}
			results <- local
		}()
	}
	workers.Wait()
	close(results)
	result := combineResults(results)
	result.Elapsed = duration
	result.Drain = time.Since(deadline)
	if result.Drain < 0 {
		result.Drain = 0
	}
	return result
}

func runCount(client *http.Client, url, token string, concurrency, requests int, record bool) loadResult {
	started := time.Now()
	jobs := make(chan struct{}, concurrency)
	results := make(chan loadResult, concurrency)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			local := loadResult{Statuses: make(map[int]int64)}
			for range jobs {
				latency, bytes, status, err := requestFeed(client, url, token)
				local.Statuses[status]++
				local.Bytes += bytes
				if err != nil {
					local.Failures++
					continue
				}
				local.Successes++
				if record {
					local.Latencies = append(local.Latencies, latency)
				}
			}
			results <- local
		}()
	}
	for i := 0; i < requests; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	workers.Wait()
	close(results)
	result := combineResults(results)
	result.Elapsed = time.Since(started)
	return result
}

func requestFeed(client *http.Client, url, token string) (time.Duration, int64, int, error) {
	started := time.Now()
	response, err := getFeed(client, url, token)
	if err != nil {
		return time.Since(started), 0, 0, err
	}
	bytes, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return time.Since(started), bytes, response.StatusCode, readErr
	}
	if closeErr != nil {
		return time.Since(started), bytes, response.StatusCode, closeErr
	}
	if response.StatusCode != http.StatusOK {
		return time.Since(started), bytes, response.StatusCode, fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	return time.Since(started), bytes, response.StatusCode, nil
}

func combineResults(results <-chan loadResult) loadResult {
	combined := loadResult{Statuses: make(map[int]int64)}
	for result := range results {
		combined.Successes += result.Successes
		combined.Failures += result.Failures
		combined.Bytes += result.Bytes
		for status, count := range result.Statuses {
			combined.Statuses[status] += count
		}
		combined.Latencies = append(combined.Latencies, result.Latencies...)
	}
	return combined
}

func printResult(url string, concurrency int, result loadResult) {
	sort.Slice(result.Latencies, func(i, j int) bool { return result.Latencies[i] < result.Latencies[j] })
	requestsPerSecond := float64(result.Successes) / result.Elapsed.Seconds()
	fmt.Printf("URL=%s concurrency=%d success=%d failures=%d statuses=%v elapsed=%s drain=%s bytes=%d\n", url, concurrency, result.Successes, result.Failures, result.Statuses, result.Elapsed.Round(time.Millisecond), result.Drain.Round(time.Millisecond), result.Bytes)
	fmt.Printf("throughput=%.1f successful_requests/s p50=%s p95=%s p99=%s\n", requestsPerSecond, percentile(result.Latencies, .50).Round(time.Microsecond), percentile(result.Latencies, .95).Round(time.Microsecond), percentile(result.Latencies, .99).Round(time.Microsecond))
}

type loadReport struct {
	URL                 string        `json:"url"`
	Concurrency         int           `json:"concurrency"`
	WindowMS            int64         `json:"window_ms"`
	DrainMS             int64         `json:"drain_ms"`
	Successes           int64         `json:"successes"`
	Failures            int64         `json:"failures"`
	Statuses            map[int]int64 `json:"statuses"`
	Bytes               int64         `json:"bytes"`
	RequestsPerSecond   float64       `json:"successful_requests_per_second"`
	P50Microseconds     int64         `json:"p50_microseconds"`
	P95Microseconds     int64         `json:"p95_microseconds"`
	P99Microseconds     int64         `json:"p99_microseconds"`
	LatencySamplesMicro []int64       `json:"latency_samples_microseconds"`
}

func writeReport(path, url string, concurrency int, result loadResult) error {
	samples := append([]time.Duration(nil), result.Latencies...)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	serialized := make([]int64, len(samples))
	for i, sample := range samples {
		serialized[i] = sample.Microseconds()
	}
	windowSeconds := result.Elapsed.Seconds()
	if windowSeconds <= 0 {
		return fmt.Errorf("report window must be positive")
	}
	toMicros := func(p float64) int64 { return percentile(samples, p).Microseconds() }
	report := loadReport{
		URL: url, Concurrency: concurrency, WindowMS: result.Elapsed.Milliseconds(), DrainMS: result.Drain.Milliseconds(),
		Successes: result.Successes, Failures: result.Failures, Statuses: result.Statuses, Bytes: result.Bytes,
		RequestsPerSecond: float64(result.Successes) / windowSeconds, P50Microseconds: toMicros(.50),
		P95Microseconds: toMicros(.95), P99Microseconds: toMicros(.99), LatencySamplesMicro: serialized,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode load report: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write load report: %w", err)
	}
	return nil
}

func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	index := int(float64(len(samples)-1) * p)
	return samples[index]
}
