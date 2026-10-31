package im_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const loadClients = 20
const loadPerClient = 50
const loadRounds = 3

type loadSample struct {
	Key        string  `json:"client_id"`
	ID         uint    `json:"message_id"`
	ACKMS      float64 `json:"ack_ms"`
	DeliveryMS float64 `json:"delivery_ms"`
}

type loadRoundReport struct {
	Round             int                  `json:"round"`
	Clients           int                  `json:"clients"`
	Messages          int                  `json:"messages"`
	ElapsedSeconds    float64              `json:"elapsed_seconds"`
	DuplicateACK      int                  `json:"duplicate_ack_frames"`
	DuplicateDelivery int                  `json:"duplicate_delivery_frames"`
	ACK               latencySummary       `json:"sender_ack"`
	Delivery          latencySummary       `json:"recipient_delivery"`
	Samples           []loadSample         `json:"samples"`
	API               []loadResourceReport `json:"api_resources"`
}

type loadReport struct {
	Version     int               `json:"format_version"`
	GoVersion   string            `json:"go_version"`
	Revision    string            `json:"checkout_revision"`
	Race        bool              `json:"race_instrumented"`
	MySQL       string            `json:"mysql_version"`
	Redis       string            `json:"redis_version"`
	CPUs        int               `json:"visible_cpus"`
	BodyBytes   int               `json:"body_bytes"`
	Rounds      []loadRoundReport `json:"rounds"`
	Offline     int               `json:"offline_messages"`
	Persisted   int               `json:"persisted_messages"`
	RetryChecks int               `json:"retry_checks"`
}

// Read every frame during the measured window. Helpers that wait for one
// matching frame can discard errors, deliveries or ACKs and hide lost data.
func runLoadRound(t *testing.T, round int, users []account, sockets []*socket, bases []string) (loadRoundReport, map[string]*loadMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	errors := make(chan error, 1)
	fail := func(err error) {
		select {
		case errors <- err:
		default:
		}
		cancel()
	}
	observer := &loadObserver{users: users, messages: map[string]*loadMessage{}}
	for i := range loadClients {
		for j := range loadPerClient {
			observer.messages[fmt.Sprintf("load-r%d-c%02d-m%02d", round, i, j)] = &loadMessage{Sender: i, Recipient: (i + 10) % loadClients, Body: strings.Repeat("x", 256)}
		}
	}
	acked := make([]chan string, loadClients)
	pongs := make(chan int, loadClients)
	for i, s := range sockets {
		acked[i] = make(chan string, loadPerClient)
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case e, ok := <-s.read:
					if !ok {
						fail(fmt.Errorf("client %d disconnected", i))
						return
					}
					if e.Type == "pong" {
						select {
						case pongs <- i:
						case <-ctx.Done():
						}
						continue
					}
					firstACK, err := observer.observe(i, e, time.Now())
					if err != nil {
						fail(err)
						return
					}
					if firstACK {
						select {
						case acked[i] <- e.ClientID:
						case <-ctx.Done():
							return
						}
					}
				}
			}
		})
	}
	start := time.Now()
	resources := make([][]loadResource, len(bases))
	for i, base := range bases {
		sample, err := loadResources(ctx, base, start)
		if err != nil {
			t.Fatal(err)
		}
		if sample.Connections != 10 {
			t.Fatalf("API %d has %.0f connections, want 10", i, sample.Connections)
		}
		resources[i] = append(resources[i], sample)
	}
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	workers.Go(func() {
		defer close(monitorDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
				for i, base := range bases {
					sample, err := loadResources(monitorCtx, base, start)
					if err != nil {
						if monitorCtx.Err() == nil {
							fail(err)
						}
						return
					}
					if sample.Connections != 10 {
						fail(fmt.Errorf("API %d lost a measured socket", i))
						return
					}
					resources[i] = append(resources[i], sample)
				}
			}
		}
	})
	defer stopMonitor()
	ready := make(chan struct{})
	sendDone := make(chan struct{})
	var senders sync.WaitGroup
	for i, s := range sockets {
		senders.Go(func() {
			<-ready
			for j := range loadPerClient {
				key := fmt.Sprintf("load-r%d-c%02d-m%02d", round, i, j)
				observer.mu.Lock()
				m := observer.messages[key]
				m.Started = time.Now()
				observer.mu.Unlock()
				if err := s.writeJSON(map[string]any{"type": "message", "client_id": key, "body": m.Body, "recipient_id": users[m.Recipient].User.ID}); err != nil {
					fail(err)
					return
				}
				select {
				case got := <-acked[i]:
					if got != key {
						fail(fmt.Errorf("sender %d received unexpected ACK key", i))
						return
					}
				case <-ctx.Done():
					return
				}
			}
		})
	}
	workers.Go(func() { senders.Wait(); close(sendDone) })
	close(ready)
	select {
	case <-sendDone:
	case <-ctx.Done():
		select {
		case err := <-errors:
			t.Fatal(err)
		default:
			t.Fatal("load send timeout")
		}
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !observer.complete() {
		select {
		case err := <-errors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("incomplete recipient delivery set")
		case <-ticker.C:
		}
	}
	// Pongs delimit the observation window after all exact ACK/delivery pairs;
	// their frames must also be seen, with no helper discarding intervening data.
	for _, s := range sockets {
		if err := s.writeJSON(map[string]string{"type": "ping"}); err != nil {
			t.Fatal(err)
		}
	}
	seenPong := map[int]bool{}
	for len(seenPong) < loadClients {
		select {
		case i := <-pongs:
			if seenPong[i] {
				t.Fatal("duplicate barrier pong")
			}
			seenPong[i] = true
		case err := <-errors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("load barrier timeout")
		}
	}
	elapsed := time.Since(start).Seconds()
	stopMonitor()
	<-monitorDone
	for i, base := range bases {
		sample, err := loadResources(ctx, base, start)
		if err != nil {
			t.Fatal(err)
		}
		if sample.Connections != 10 {
			t.Fatal("socket closed before final resource sample")
		}
		resources[i] = append(resources[i], sample)
	}
	cancel()
	workers.Wait()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
	report := loadRoundReport{Round: round, Clients: loadClients, Messages: loadClients * loadPerClient, ElapsedSeconds: elapsed, DuplicateACK: observer.duplicateACK, DuplicateDelivery: observer.duplicateDelivery}
	keys := make([]string, 0, len(observer.messages))
	for key := range observer.messages {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var ackMS, deliveryMS []float64
	for _, key := range keys {
		m := observer.messages[key]
		report.Samples = append(report.Samples, loadSample{key, m.ID, m.AckMS, m.DeliveryMS})
		ackMS = append(ackMS, m.AckMS)
		deliveryMS = append(deliveryMS, m.DeliveryMS)
	}
	report.ACK, report.Delivery = summarizeLatency(ackMS), summarizeLatency(deliveryMS)
	for _, r := range resources {
		report.API = append(report.API, summarizeResources(r))
	}
	return report, observer.messages
}

func TestIMConcurrentLoad(t *testing.T) {
	if os.Getenv("GOSTREAM_TEST_IM_LOAD") != "1" {
		t.Skip("use im_smoke.py --suite load with disposable dependencies")
	}
	binary, dsn, redisAddr := os.Getenv("GOSTREAM_TEST_IM_BINARY"), os.Getenv("GOSTREAM_TEST_IM_MYSQL_DSN"), os.Getenv("GOSTREAM_TEST_IM_REDIS_ADDR")
	if binary == "" || dsn == "" || redisAddr == "" || !strings.HasPrefix(os.Getenv("GOSTREAM_TEST_IM_REDIS_CONTAINER"), "gostream-im-test-") {
		t.Fatal("load requires isolated API/MySQL/Redis configuration")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("invalid isolated SQL configuration")
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&count); err != nil || count != 0 {
		t.Fatal("load requires a newly created empty schema")
	}
	rc := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() {
		if err := rc.Close(); err != nil {
			t.Error(err)
		}
	})
	if n, err := rc.DBSize(ctx).Result(); err != nil || n != 0 {
		t.Fatal("load requires empty dedicated Redis")
	}
	secret := randomID(t) + randomID(t)
	a, b := newProcess(t, binary, dsn, redisAddr, secret), newProcess(t, binary, dsn, redisAddr, secret)
	a.start(t)
	b.start(t)
	bases := []string{a.base, b.base}
	eventually(t, "two load Redis subscribers", 10*time.Second, func() bool {
		values, err := rc.PubSubNumSub(ctx, "gostream:realtime:v1").Result()
		return err == nil && values["gostream:realtime:v1"] == 2
	})
	users := make([]account, loadClients)
	devices := make([]string, loadClients)
	sockets := make([]*socket, loadClients)
	for i := range loadClients {
		base := bases[i/10]
		users[i] = register(t, base, fmt.Sprintf("load_%02d", i))
		devices[i] = randomID(t)
		sockets[i] = connect(t, base, users[i], devices[i])
	}
	report := loadReport{Version: 1, GoVersion: runtime.Version(), Revision: os.Getenv("GOSTREAM_TEST_IM_LOAD_REVISION"), Race: os.Getenv("GOSTREAM_TEST_IM_LOAD_RACE") == "1", CPUs: runtime.NumCPU(), BodyBytes: 256}
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&report.MySQL); err != nil {
		t.Fatal(err)
	}
	info, err := rc.Info(ctx, "server").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(line, "redis_version:"); ok {
			report.Redis = strings.TrimSpace(v)
		}
	}
	if report.Redis == "" {
		t.Fatal("Redis version unavailable")
	}
	all := map[string]*loadMessage{}
	for round := 1; round <= loadRounds; round++ {
		r, messages := runLoadRound(t, round, users, sockets, bases)
		report.Rounds = append(report.Rounds, r)
		for key, m := range messages {
			all[key] = m
		}
		// Query and ACK each device through the other instance, checking full IDs.
		for i, user := range users {
			var want []uint
			for _, m := range messages {
				if m.Recipient == i {
					want = append(want, m.ID)
				}
			}
			slices.Sort(want)
			var p page
			base := bases[1-i/10]
			call(t, base, "GET", "/api/messages/unread?limit=100", user, devices[i], nil, 200, &p)
			got := make([]uint, len(p.Items))
			for j, m := range p.Items {
				got[j] = m.ID
			}
			if p.More || !slices.Equal(got, want) {
				t.Fatalf("round %d recipient %d unread mismatch", round, i)
			}
			var ack struct {
				Count int `json:"acked_count"`
			}
			call(t, base, "POST", "/api/messages/acks", user, devices[i], map[string]any{"ids": want}, 200, &ack)
			if ack.Count != loadPerClient {
				t.Fatalf("recipient %d new ACKs=%d", i, ack.Count)
			}
		}
		// Retry one command per active sender after the measured observation window.
		for i, s := range sockets {
			key := fmt.Sprintf("load-r%d-c%02d-m00", round, i)
			m := messages[key]
			if id := send(t, s, "message", key, m.Body, users[m.Recipient].User.ID); id != m.ID {
				t.Fatal("retry changed persisted identity")
			}
			report.RetryChecks++
		}
		t.Logf("load round=%d messages=%d ack_p95_ms=%.3f recipient_p95_ms=%.3f seconds=%.3f", round, r.Messages, r.ACK.P95, r.Delivery.P95, r.ElapsedSeconds)
	}
	verifyLoadOffline(t, ctx, db, a, b, users, devices, sockets, all, &report)
	rows, err := db.QueryContext(ctx, "SELECT id,sender_id,recipient_id,client_id,body FROM chat_messages ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	seenID := map[uint]bool{}
	for rows.Next() {
		var m message
		if err := rows.Scan(&m.ID, &m.SenderID, &m.RecipientID, &m.ClientID, &m.Body); err != nil {
			t.Fatal(err)
		}
		want, ok := all[m.ClientID]
		if !ok || seen[m.ClientID] || seenID[m.ID] || m.ID != want.ID || m.SenderID != users[want.Sender].User.ID || m.RecipientID != users[want.Recipient].User.ID || m.Body != want.Body {
			t.Fatal("SQL identity/content/uniqueness mismatch")
		}
		seen[m.ClientID], seenID[m.ID] = true, true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	report.Persisted = len(seen)
	if len(seen) != 3125 || len(seen) != len(all) {
		t.Fatalf("persisted=%d expected=%d", len(seen), len(all))
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("GOSTREAM_TEST_IM_LOAD_REPORT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote raw IM load report: rounds=%d persisted=%d retries=%d", len(report.Rounds), report.Persisted, report.RetryChecks)
	} else {
		t.Logf("IM_LOAD_REPORT_JSON=%s", data)
	}
}
