package im_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultGroupLoadClients = 20
const defaultGroupLoadPerClient = 50

type groupLoadMessage struct {
	Key        string
	Sender     int
	Body       string
	ID         uint
	Started    time.Time
	Acked      bool
	AckMS      float64
	Deliveries map[int]float64 // receiver client index -> delivery latency in ms
}

type groupLoadObserver struct {
	mu                sync.Mutex
	users             []account
	messages          map[string]*groupLoadMessage
	duplicateACK      int
	duplicateDelivery int
}

func (o *groupLoadObserver) observe(clientIdx int, e event, at time.Time) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	switch e.Type {
	case "ack":
		m, ok := o.messages[e.ClientID]
		if !ok {
			return false, nil
		}
		if m.Sender != clientIdx {
			return false, fmt.Errorf("client %d received ACK for sender %d key %s", clientIdx, m.Sender, e.ClientID)
		}
		if m.Acked {
			o.duplicateACK++
			return false, nil
		}
		m.Acked = true
		if m.ID == 0 {
			m.ID = e.MessageID
		}
		m.AckMS = at.Sub(m.Started).Seconds() * 1000
		return true, nil

	case "group_message":
		if e.Message.SenderID == o.users[clientIdx].User.ID {
			return false, fmt.Errorf("client %d received group_message sent by self (id=%d)", clientIdx, e.Message.ID)
		}
		m, ok := o.messages[e.Message.ClientID]
		if !ok {
			return false, nil
		}
		if m.ID == 0 {
			m.ID = e.Message.ID
		}
		if _, exists := m.Deliveries[clientIdx]; exists {
			o.duplicateDelivery++
			return false, nil
		}
		m.Deliveries[clientIdx] = at.Sub(m.Started).Seconds() * 1000
		return false, nil
	}
	return false, nil
}

func (o *groupLoadObserver) complete(expectedTotalDeliveries int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	totalDeliv := 0
	for _, m := range o.messages {
		if !m.Acked {
			return false
		}
		totalDeliv += len(m.Deliveries)
	}
	return totalDeliv == expectedTotalDeliveries
}

type groupLoadReport struct {
	RegistrationWaitMS float64              `json:"registration_rate_limit_wait_ms"`
	Version            int                  `json:"format_version"`
	GoVersion          string               `json:"go_version"`
	Revision           string               `json:"checkout_revision"`
	Race               bool                 `json:"race_instrumented"`
	MySQL              string               `json:"mysql_version"`
	Redis              string               `json:"redis_version"`
	CPUs               int                  `json:"visible_cpus"`
	Clients            int                  `json:"clients"`
	Messages           int                  `json:"total_messages"`
	ExpectedDeliveries int                  `json:"expected_deliveries"`
	ElapsedSeconds     float64              `json:"elapsed_seconds"`
	DuplicateACK       int                  `json:"duplicate_ack_frames"`
	DuplicateDelivery  int                  `json:"duplicate_delivery_frames"`
	ACK                latencySummary       `json:"sender_ack"`
	Delivery           latencySummary       `json:"recipient_delivery"`
	API                []loadResourceReport `json:"api_resources"`
	OfflineBacklog     int                  `json:"offline_messages_backfilled"`
	AbsenceChecked     bool                 `json:"absence_and_reinvite_checked"`
	MultiDeviceChecked bool                 `json:"multi_device_unread_checked"`
	IdempotencyChecked bool                 `json:"idempotency_checked"`
	Persisted          int                  `json:"persisted_group_messages"`
}

func runGroupLoadRound(
	t *testing.T,
	groupID uint,
	users []account,
	sockets []*socket,
	bases []string,
	msgsPerClient int,
) (groupLoadReport, map[string]*groupLoadMessage) {
	t.Helper()
	clientsCount := len(users)
	totalMessages := clientsCount * msgsPerClient
	expectedTotalDeliveries := totalMessages * (clientsCount - 1)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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

	observer := &groupLoadObserver{
		users:    users,
		messages: make(map[string]*groupLoadMessage, totalMessages),
	}
	for i := range clientsCount {
		for j := range msgsPerClient {
			key := fmt.Sprintf("gload-c%02d-m%02d", i, j)
			observer.messages[key] = &groupLoadMessage{
				Key:        key,
				Sender:     i,
				Body:       fmt.Sprintf("Group fanout payload c%02d m%02d", i, j),
				Deliveries: make(map[int]float64, clientsCount-1),
			}
		}
	}

	acked := make([]chan string, clientsCount)
	pongs := make(chan int, clientsCount)
	for i, s := range sockets {
		acked[i] = make(chan string, msgsPerClient)
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
			for j := range msgsPerClient {
				key := fmt.Sprintf("gload-c%02d-m%02d", i, j)
				observer.mu.Lock()
				m := observer.messages[key]
				m.Started = time.Now()
				observer.mu.Unlock()

				err := s.writeJSON(map[string]any{
					"type":      "group_message",
					"client_id": key,
					"group_id":  groupID,
					"body":      m.Body,
				})
				if err != nil {
					fail(err)
					return
				}

				select {
				case got := <-acked[i]:
					if got != key {
						fail(fmt.Errorf("sender %d received unexpected ACK key %s != %s", i, got, key))
						return
					}
				case <-ctx.Done():
					return
				}

				// Brief pacing to prevent lock starvation
				time.Sleep(time.Duration(1+i%3) * time.Millisecond)
			}
		})
	}

	workers.Go(func() {
		senders.Wait()
		close(sendDone)
	})
	close(ready)

	select {
	case <-sendDone:
	case <-ctx.Done():
		select {
		case err := <-errors:
			t.Fatal(err)
		default:
			t.Fatal("group load send timeout")
		}
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !observer.complete(expectedTotalDeliveries) {
		select {
		case err := <-errors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("incomplete recipient group delivery set")
		case <-ticker.C:
		}
	}

	// Pong barrier
	for _, s := range sockets {
		if err := s.writeJSON(map[string]string{"type": "ping"}); err != nil {
			t.Fatal(err)
		}
	}
	seenPong := map[int]bool{}
	for len(seenPong) < clientsCount {
		select {
		case i := <-pongs:
			if seenPong[i] {
				t.Fatal("duplicate barrier pong")
			}
			seenPong[i] = true
		case err := <-errors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("group load barrier timeout")
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

	// Verify mathematical expected set E(k) = { m.ID | m.Sender != k }
	for k := range clientsCount {
		wantDeliveries := totalMessages - msgsPerClient
		receivedCount := 0
		for _, m := range observer.messages {
			if m.Sender == k {
				if _, got := m.Deliveries[k]; got {
					t.Fatalf("client %d received delivery for its own message %s", k, m.Key)
				}
			} else {
				if _, got := m.Deliveries[k]; !got {
					t.Fatalf("client %d missing expected delivery for message %s from sender %d", k, m.Key, m.Sender)
				}
				receivedCount++
			}
		}
		if receivedCount != wantDeliveries {
			t.Fatalf("client %d expected %d deliveries, got %d", k, wantDeliveries, receivedCount)
		}
	}

	report := groupLoadReport{
		Clients:            clientsCount,
		Messages:           totalMessages,
		ExpectedDeliveries: expectedTotalDeliveries,
		ElapsedSeconds:     elapsed,
		DuplicateACK:       observer.duplicateACK,
		DuplicateDelivery:  observer.duplicateDelivery,
	}

	keys := make([]string, 0, len(observer.messages))
	for key := range observer.messages {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var ackMS, deliveryMS []float64
	for _, key := range keys {
		m := observer.messages[key]
		ackMS = append(ackMS, m.AckMS)
		for _, dms := range m.Deliveries {
			deliveryMS = append(deliveryMS, dms)
		}
	}
	report.ACK, report.Delivery = summarizeLatency(ackMS), summarizeLatency(deliveryMS)
	for _, r := range resources {
		report.API = append(report.API, summarizeResources(r))
	}

	return report, observer.messages
}

func TestGroupIMConcurrentFanout(t *testing.T) {
	if os.Getenv("GOSTREAM_TEST_IM_GROUP_LOAD") != "1" && os.Getenv("GOSTREAM_TEST_IM_LOAD") != "1" {
		t.Skip("use im_smoke.py --suite group-load with disposable dependencies")
	}

	binary := os.Getenv("GOSTREAM_TEST_IM_BINARY")
	dsn := os.Getenv("GOSTREAM_TEST_IM_MYSQL_DSN")
	redisAddr := os.Getenv("GOSTREAM_TEST_IM_REDIS_ADDR")
	if binary == "" || dsn == "" || redisAddr == "" || !strings.HasPrefix(os.Getenv("GOSTREAM_TEST_IM_REDIS_CONTAINER"), "gostream-im-test-") {
		t.Fatal("group load requires isolated API/MySQL/Redis configuration")
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
		t.Fatal("group load requires a newly created empty schema")
	}

	rc := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() {
		if err := rc.Close(); err != nil {
			t.Error(err)
		}
	})
	if n, err := rc.DBSize(ctx).Result(); err != nil || n != 0 {
		t.Fatal("group load requires empty dedicated Redis")
	}

	secret := randomID(t) + randomID(t)
	a, b := newProcess(t, binary, dsn, redisAddr, secret), newProcess(t, binary, dsn, redisAddr, secret)
	a.start(t)
	b.start(t)
	bases := []string{a.base, b.base}

	eventually(t, "two group load Redis subscribers", 10*time.Second, func() bool {
		values, err := rc.PubSubNumSub(ctx, "gostream:realtime:v1").Result()
		return err == nil && values["gostream:realtime:v1"] == 2
	})

	users := make([]account, defaultGroupLoadClients)
	devices := make([]string, defaultGroupLoadClients)
	sockets := make([]*socket, defaultGroupLoadClients)
	var registrationWait time.Duration

	for i := range defaultGroupLoadClients {
		who, wait, err := registerLoadAccount(ctx, bases[i/10], fmt.Sprintf("gload_u%02d", i))
		if err != nil {
			t.Fatal(err)
		}
		users[i] = who
		registrationWait += wait
		devices[i] = randomID(t)
	}

	for i := range defaultGroupLoadClients {
		sockets[i] = connect(t, bases[i/10], users[i], devices[i])
	}
	t.Logf("setup registered 20 group load accounts; waited %.3f seconds", registrationWait.Seconds())

	// Client 0 creates group
	var group struct {
		ID uint `json:"id"`
	}
	call(t, bases[0], "POST", "/api/groups", users[0], "", map[string]string{"name": "20-Client Cross-Instance Fanout Group"}, 201, &group)
	if group.ID == 0 {
		t.Fatal("failed to create group")
	}

	// Add remaining 19 members
	for i := 1; i < defaultGroupLoadClients; i++ {
		call(t, bases[i/10], "POST", fmt.Sprintf("/api/groups/%d/members", group.ID), users[0], "", map[string]uint{"user_id": users[i].User.ID}, 200, nil)
	}

	// Determine messages per client
	msgsPerClient := defaultGroupLoadPerClient
	if v := os.Getenv("GOSTREAM_TEST_GROUP_LOAD_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			msgsPerClient = n
		}
	}

	// Run concurrent fanout load
	roundReport, allMessages := runGroupLoadRound(t, group.ID, users, sockets, bases, msgsPerClient)

	report := roundReport
	report.RegistrationWaitMS = float64(registrationWait.Microseconds()) / 1000
	report.Version = 1
	report.GoVersion = runtime.Version()
	report.Revision = os.Getenv("GOSTREAM_TEST_IM_LOAD_REVISION")
	report.Race = os.Getenv("GOSTREAM_TEST_IM_LOAD_RACE") == "1"
	report.CPUs = runtime.NumCPU()

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

	t.Logf("group fanout load completed: messages=%d ack_p95_ms=%.3f delivery_p95_ms=%.3f seconds=%.3f",
		report.Messages, report.ACK.P95, report.Delivery.P95, report.ElapsedSeconds)

	// Checkpoint 1: Duplicate client_id idempotency
	t.Log("Verifying duplicate client_id idempotency...")
	key0 := "gload-c00-m00"
	m0 := allMessages[key0]
	resendID := send(t, sockets[0], "group_message", key0, m0.Body, group.ID)
	if resendID != m0.ID {
		t.Fatalf("idempotent resend returned different message ID: %d != %d", resendID, m0.ID)
	}
	var sqlCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM group_messages WHERE group_id=? AND sender_id=? AND client_id=?",
		group.ID, users[0].User.ID, key0).Scan(&sqlCount); err != nil || sqlCount != 1 {
		t.Fatalf("expected 1 row in SQL for %s, got %d", key0, sqlCount)
	}
	sockets[1].absent(t, "group_message", m0.ID)
	report.IdempotencyChecked = true

	// Checkpoint 2: Offline member backfill & unread convergence
	t.Log("Verifying offline member disconnect, backlog 125 messages, and backfill...")
	var maxInitID uint
	for _, m := range allMessages {
		if m.ID > maxInitID {
			maxInitID = m.ID
		}
	}
	call(t, bases[1], "POST", fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, maxInitID), users[19], devices[19], nil, 200, nil)
	_ = sockets[19].conn.Close()

	offlineIDs := make([]uint, 0, 125)
	for i := 0; i < 125; i++ {
		senderIdx := i % 19
		key := fmt.Sprintf("gload-offline-%03d", i)
		id := send(t, sockets[senderIdx], "group_message", key, fmt.Sprintf("Offline backlog message %d", i), group.ID)
		offlineIDs = append(offlineIDs, id)
	}

	var uBefore struct {
		Count int `json:"unread_count"`
	}
	call(t, bases[1], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[19], devices[19], nil, 200, &uBefore)
	if uBefore.Count != 125 {
		t.Fatalf("expected 125 unread messages while offline, got %d", uBefore.Count)
	}

	sock19New := connect(t, bases[1], users[19], devices[19])
	defer sock19New.conn.Close()

	replayed := make([]uint, 0, 100)
	for range 100 {
		e := sock19New.next(t, "group_message", nil)
		replayed = append(replayed, e.Message.ID)
	}
	if !slices.Equal(replayed, offlineIDs[:100]) {
		t.Fatal("first 100 replayed message IDs do not match expected first 100 offline messages")
	}
	sock19New.next(t, "group_sync_required", nil)

	// Backfill remaining 25 via Keyset pagination
	var backfillPage struct {
		Items []message `json:"items"`
	}
	call(t, bases[1], "GET", fmt.Sprintf("/api/groups/%d/messages?limit=50", group.ID), users[19], devices[19], nil, 200, &backfillPage)
	if len(backfillPage.Items) < 25 {
		t.Fatalf("keyset backfill returned %d items, want at least 25", len(backfillPage.Items))
	}

	// ACK newest message
	lastID := offlineIDs[len(offlineIDs)-1]
	call(t, bases[1], "POST", fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, lastID), users[19], devices[19], nil, 200, nil)

	var uAfter struct {
		Count int `json:"unread_count"`
	}
	call(t, bases[1], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[19], devices[19], nil, 200, &uAfter)
	if uAfter.Count != 0 {
		t.Fatalf("expected unread count to converge to 0 after ACK, got %d", uAfter.Count)
	}
	report.OfflineBacklog = 125

	// Checkpoint 3: Member removal & re-invitation cursor auto-advance
	t.Log("Verifying member removal and re-invitation...")
	call(t, bases[0], "DELETE", fmt.Sprintf("/api/groups/%d/members/%d", group.ID, users[18].User.ID), users[0], "", nil, 200, nil)

	call(t, bases[1], "GET", fmt.Sprintf("/api/groups/%d/messages", group.ID), users[18], devices[18], nil, 404, nil)
	call(t, bases[1], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[18], devices[18], nil, 404, nil)

	for i := 0; i < 10; i++ {
		send(t, sockets[0], "group_message", fmt.Sprintf("gload-absent-%02d", i), "Absence message", group.ID)
	}

	call(t, bases[0], "POST", fmt.Sprintf("/api/groups/%d/members", group.ID), users[0], "", map[string]uint{"user_id": users[18].User.ID}, 200, nil)

	var reinviteUnread struct {
		Count int `json:"unread_count"`
	}
	call(t, bases[1], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[18], devices[18], nil, 200, &reinviteUnread)
	if reinviteUnread.Count != 0 {
		t.Fatalf("expected 0 unread for re-invited member, got %d", reinviteUnread.Count)
	}
	report.AbsenceChecked = true

	// Checkpoint 4: Per-device independent unread and ACK
	t.Log("Verifying per-device independent unread counts and ACKs...")
	dev2 := randomID(t)

	// Establish baseline: ACK up to newest message so both devices start at 0 unread
	var curPage struct {
		Items []message `json:"items"`
	}
	call(t, bases[0], "GET", fmt.Sprintf("/api/groups/%d/messages?limit=1", group.ID), users[0], devices[0], nil, 200, &curPage)
	if len(curPage.Items) > 0 {
		baselineID := curPage.Items[0].ID
		call(t, bases[0], "POST", fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, baselineID), users[0], devices[0], nil, 200, nil)
		call(t, bases[0], "POST", fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, baselineID), users[0], dev2, nil, 200, nil)
	}

	devMsgIDs := make([]uint, 0, 10)
	for i := 0; i < 10; i++ {
		id := send(t, sockets[1], "group_message", fmt.Sprintf("gload-dev-%02d", i), "Dev test message", group.ID)
		devMsgIDs = append(devMsgIDs, id)
	}

	var dev1Unread, dev2Unread struct {
		Count int `json:"unread_count"`
	}
	call(t, bases[0], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[0], devices[0], nil, 200, &dev1Unread)
	call(t, bases[0], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[0], dev2, nil, 200, &dev2Unread)
	if dev1Unread.Count != 10 || dev2Unread.Count != 10 {
		t.Fatalf("expected both devices unread=10, got dev1=%d dev2=%d", dev1Unread.Count, dev2Unread.Count)
	}

	// Dev 1 ACKs message 5
	call(t, bases[0], "POST", fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, devMsgIDs[4]), users[0], devices[0], nil, 200, nil)
	call(t, bases[0], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[0], devices[0], nil, 200, &dev1Unread)
	call(t, bases[0], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[0], dev2, nil, 200, &dev2Unread)
	if dev1Unread.Count != 5 || dev2Unread.Count != 10 {
		t.Fatalf("after dev1 ACK: expected dev1=5 dev2=10, got dev1=%d dev2=%d", dev1Unread.Count, dev2Unread.Count)
	}

	// Dev 2 ACKs message 10
	call(t, bases[0], "POST", fmt.Sprintf("/api/groups/%d/messages/%d/ack", group.ID, devMsgIDs[9]), users[0], dev2, nil, 200, nil)
	call(t, bases[0], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[0], devices[0], nil, 200, &dev1Unread)
	call(t, bases[0], "GET", fmt.Sprintf("/api/groups/%d/unread", group.ID), users[0], dev2, nil, 200, &dev2Unread)
	if dev1Unread.Count != 5 || dev2Unread.Count != 0 {
		t.Fatalf("after dev2 ACK: expected dev1=5 dev2=0, got dev1=%d dev2=%d", dev1Unread.Count, dev2Unread.Count)
	}
	report.MultiDeviceChecked = true

	// Checkpoint 5: SQL persistence & uniqueness
	t.Log("Verifying SQL persistence & uniqueness...")
	rows, err := db.QueryContext(ctx, "SELECT id, group_id, sender_id, client_id, body FROM group_messages WHERE group_id=? ORDER BY id", group.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	persistedCount := 0
	seenClientIDs := make(map[string]bool)
	var prevID uint
	for rows.Next() {
		var id, grpID, senderID uint
		var cid, body string
		if err := rows.Scan(&id, &grpID, &senderID, &cid, &body); err != nil {
			t.Fatal(err)
		}
		if id <= prevID {
			t.Fatalf("SQL non-strictly increasing ID: %d <= %d", id, prevID)
		}
		prevID = id
		if seenClientIDs[cid] {
			t.Fatalf("duplicate client_id in SQL: %s", cid)
		}
		seenClientIDs[cid] = true
		persistedCount++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	expectedPersisted := report.Messages + 125 + 10 + 10 // main load + offline + absence + dev test
	if persistedCount != expectedPersisted {
		t.Fatalf("persisted messages count mismatch: got %d, expected %d", persistedCount, expectedPersisted)
	}
	report.Persisted = persistedCount

	// Output report JSON
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("GOSTREAM_TEST_IM_LOAD_REPORT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote raw group IM load report to %s: messages=%d deliveries=%d", path, report.Messages, report.ExpectedDeliveries)
	} else {
		t.Logf("GROUP_IM_LOAD_REPORT_JSON=%s", data)
	}
}

