package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[token]++
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
	if err := run(server.URL, actors, 2, 0, 100*time.Millisecond, path); err != nil {
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
