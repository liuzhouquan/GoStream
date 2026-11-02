package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetFeedOptionalToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Authorization", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	for _, tc := range []struct {
		token string
		want  string
	}{{"", ""}, {"test-access-token", "Bearer test-access-token"}} {
		response, err := getFeed(server.Client(), server.URL, tc.token)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if got := response.Header.Get("X-Seen-Authorization"); got != tc.want {
			t.Fatalf("authorization header = %q, want %q", got, tc.want)
		}
	}
}

func TestRunForCollectsSuccessfulRequestsWithinDuration(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("feed"))
	}))
	defer server.Close()

	client := &http.Client{Timeout: time.Second}
	result := runFor(client, server.URL, "", 4, 100*time.Millisecond, true)
	if result.Successes == 0 || result.Failures != 0 {
		t.Fatalf("timed run result = successes:%d failures:%d", result.Successes, result.Failures)
	}
	if result.Elapsed != 100*time.Millisecond {
		t.Fatalf("measurement window = %s, want 100ms", result.Elapsed)
	}
	if result.Successes != requests.Load() || int64(len(result.Latencies)) != result.Successes {
		t.Fatalf("recorded requests=%d latencies=%d server requests=%d", result.Successes, len(result.Latencies), requests.Load())
	}
	if result.Bytes != int64(result.Successes)*4 {
		t.Fatalf("bytes read = %d, want %d", result.Bytes, result.Successes*4)
	}
}

func TestRunForRecordsUnexpectedHTTPStatuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	result := runFor(server.Client(), server.URL, "", 2, 50*time.Millisecond, true)
	if result.Failures == 0 || result.Statuses[http.StatusServiceUnavailable] != result.Failures {
		t.Fatalf("timed run did not account for HTTP 503 responses: %+v", result)
	}
}

func TestWriteReportRetainsLatencySamplesAndFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed-load.json")
	result := loadResult{
		Successes: 2, Failures: 1, Bytes: 8, Elapsed: time.Second,
		Statuses:  map[int]int64{http.StatusOK: 2, http.StatusServiceUnavailable: 1},
		Latencies: []time.Duration{5 * time.Millisecond, 3 * time.Millisecond},
	}
	if err := writeReport(path, "http://127.0.0.1/api/feed?scene=following", 20, result); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report loadReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Concurrency != 20 || report.Successes != 2 || report.Failures != 1 || report.Statuses[http.StatusServiceUnavailable] != 1 {
		t.Fatalf("report outcome = %+v", report)
	}
	if len(report.LatencySamplesMicro) != 2 || report.LatencySamplesMicro[0] != 3000 || report.LatencySamplesMicro[1] != 5000 {
		t.Fatalf("latency samples = %v", report.LatencySamplesMicro)
	}
}
