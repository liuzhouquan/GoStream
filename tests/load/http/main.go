package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/api/feed?scene=timeline&limit=20", "full Feed URL")
	requests := flag.Int("requests", 3000, "measured HTTP requests")
	concurrency := flag.Int("concurrency", 10, "concurrent workers")
	flag.Parse()
	if *requests < 1 || *concurrency < 1 || *concurrency > 1000 {
		panic("requests and concurrency must be positive; concurrency must be <= 1000")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConns: *concurrency, MaxIdleConnsPerHost: *concurrency}}
	defer client.CloseIdleConnections()
	// Warm the handler and optional cache without including startup in the result.
	for i := 0; i < 20; i++ {
		res, err := client.Get(*url)
		if err != nil {
			panic(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			panic(fmt.Sprintf("warmup returned %d", res.StatusCode))
		}
	}
	jobs := make(chan struct{}, *concurrency)
	latencies := make(chan time.Duration, *requests)
	var failures atomic.Int64
	var bytesRead atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for worker := 0; worker < *concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				began := time.Now()
				res, err := client.Get(*url)
				if err != nil {
					failures.Add(1)
					continue
				}
				n, readErr := io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				if readErr != nil || res.StatusCode != http.StatusOK {
					failures.Add(1)
					continue
				}
				bytesRead.Add(n)
				latencies <- time.Since(began)
			}
		}()
	}
	for i := 0; i < *requests; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	close(latencies)
	values := make([]time.Duration, 0, *requests)
	for value := range latencies {
		values = append(values, value)
	}
	if len(values) == 0 {
		panic("all measured requests failed")
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	percentile := func(p float64) time.Duration {
		index := int(float64(len(values)-1) * p)
		return values[index]
	}
	fmt.Printf("URL=%s requests=%d concurrency=%d success=%d failures=%d elapsed=%s bytes=%d\n", *url, *requests, *concurrency, len(values), failures.Load(), elapsed.Round(time.Millisecond), bytesRead.Load())
	fmt.Printf("throughput=%.1f requests/s p50=%s p95=%s p99=%s\n", float64(len(values))/elapsed.Seconds(), percentile(.50).Round(time.Microsecond), percentile(.95).Round(time.Microsecond), percentile(.99).Round(time.Microsecond))
}
