// Command mixed generates authenticated Feed and interaction traffic against
// an isolated GoStream deployment. It does not create fixtures or clean data.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type operation struct {
	name, method, path, body string
	want                     int
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
	success  []time.Duration
	failures int
	statuses map[int]int
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

func run(baseURL, token string, videoID, followeeID uint, concurrency int, warmup, duration time.Duration) error {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		MaxIdleConns: concurrency, MaxIdleConnsPerHost: concurrency,
	}}
	defer client.CloseIdleConnections()
	if warmup > 0 {
		deadline := time.Now().Add(warmup)
		var wg sync.WaitGroup
		var warmFailures atomic.Int64
		for worker := 0; worker < concurrency; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					_, status, err := runRequest(client, baseURL, token, operation{"feed_following", http.MethodGet, "/api/feed?scene=following&limit=20", "", http.StatusOK})
					if err != nil || status != http.StatusOK {
						warmFailures.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		if warmFailures.Load() != 0 {
			return fmt.Errorf("warmup had %d failed requests", warmFailures.Load())
		}
	}
	deadline := time.Now().Add(duration)
	start := time.Now()
	var sequence atomic.Uint64
	results := make(chan map[string]*sample, concurrency)
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make(map[string]*sample)
			for time.Now().Before(deadline) {
				op := chooseOperation(sequence.Add(1)-1, videoID, followeeID)
				value := local[op.name]
				if value == nil {
					value = &sample{statuses: make(map[int]int)}
					local[op.name] = value
				}
				latency, status, err := runRequest(client, baseURL, token, op)
				if err != nil || status != op.want {
					value.failures++
					value.statuses[status]++
				} else {
					value.success = append(value.success, latency)
				}
			}
			results <- local
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(results)
	merged := make(map[string]*sample)
	var all []time.Duration
	var failed int
	for local := range results {
		for name, result := range local {
			value := merged[name]
			if value == nil {
				value = &sample{statuses: make(map[int]int)}
				merged[name] = value
			}
			value.success = append(value.success, result.success...)
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
		all = append(all, value.success...)
		failed += value.failures
		fmt.Printf("operation=%s success=%d failures=%d p50=%s p95=%s p99=%s statuses=%v\n", name, len(value.success), value.failures,
			percentile(value.success, 50).Round(time.Microsecond), percentile(value.success, 95).Round(time.Microsecond), percentile(value.success, 99).Round(time.Microsecond), value.statuses)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	fmt.Printf("total success=%d failures=%d concurrency=%d elapsed=%s qps=%.1f p50=%s p95=%s p99=%s\n", len(all), failed, concurrency,
		elapsed.Round(time.Millisecond), float64(len(all))/elapsed.Seconds(), percentile(all, 50).Round(time.Microsecond),
		percentile(all, 95).Round(time.Microsecond), percentile(all, 99).Round(time.Microsecond))
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
	concurrency := flag.Int("concurrency", 20, "concurrent workers")
	warmup := flag.Duration("warmup", 30*time.Second, "read-only Feed warmup")
	duration := flag.Duration("duration", time.Minute, "measured mixed workload")
	flag.Parse()
	if !*isolated || *tokenFile == "" || *videoID == 0 || *followeeID == 0 || *concurrency < 1 || *concurrency > 1000 || *warmup < 0 || *duration <= 0 ||
		!strings.HasPrefix(*baseURL, "http://") && !strings.HasPrefix(*baseURL, "https://") {
		fmt.Fprintln(os.Stderr, "-isolated, token-file, video-id, followee-id, positive duration and concurrency (<=1000), and HTTP(S) base-url are required")
		os.Exit(2)
	}
	data, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		fmt.Fprintln(os.Stderr, "token file is empty")
		os.Exit(2)
	}
	if err := run(strings.TrimRight(*baseURL, "/"), token, *videoID, *followeeID, *concurrency, *warmup, *duration); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
