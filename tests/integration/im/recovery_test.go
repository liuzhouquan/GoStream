package im_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

type feedCard struct {
	ID    uint   `json:"id"`
	Title string `json:"title"`
}

type followingFeedPage struct {
	Items []feedCard `json:"items"`
}

func followingFeed(t *testing.T, base string, who account) []feedCard {
	t.Helper()
	var page followingFeedPage
	call(t, base, http.MethodGet, "/api/feed?scene=following", who, "", nil, http.StatusOK, &page)
	return page.Items
}

func feedCacheHitCount(t *testing.T, base string) float64 {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("get Feed cache metrics: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("read Feed cache metrics: status=%d err=%v", response.StatusCode, err)
	}
	const metric = `gostream_feed_cache_total{result="hit"} `
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, metric) {
			var hits float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, metric), "%f", &hits); err != nil {
				t.Fatalf("parse Feed cache hit metric %q: %v", line, err)
			}
			return hits
		}
	}
	return 0
}

// All dependencies must be disposable. The Python entry point creates them;
// no credentials, Redis keys or rows from a demonstration instance are used.
func TestIMProcessRecovery(t *testing.T) {
	binary, dsn, redisAddr, container := os.Getenv("GOSTREAM_TEST_IM_BINARY"), os.Getenv("GOSTREAM_TEST_IM_MYSQL_DSN"), os.Getenv("GOSTREAM_TEST_IM_REDIS_ADDR"), os.Getenv("GOSTREAM_TEST_IM_REDIS_CONTAINER")
	if binary == "" || dsn == "" || redisAddr == "" || container == "" {
		t.Skip("requires isolated API binary, empty MySQL schema and disposable Redis; use tests/integration/im_smoke.py")
	}
	if !strings.HasPrefix(container, "gostream-im-test-") {
		t.Fatal("refusing to stop a Redis container outside the disposable gostream-im-test- namespace")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("invalid isolated MySQL configuration")
	}
	t.Cleanup(func() { _ = db.Close() })
	count := func(query string, args ...any) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var n int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("fixture SQL query failed: %v", err)
		}
		return n
	}
	if count("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()") != 0 {
		t.Fatal("requires a newly created empty schema")
	}
	rc := redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: 0})
	t.Cleanup(func() { _ = rc.Close() })
	ctx := context.Background()
	if n, err := rc.DBSize(ctx).Result(); err != nil || n != 0 {
		t.Fatal("requires an empty dedicated Redis server")
	}
	secret := randomID(t) + randomID(t)
	a, b := newProcess(t, binary, dsn, redisAddr, secret), newProcess(t, binary, dsn, redisAddr, secret)
	a.start(t)
	if os.Getenv("GOSTREAM_TEST_IM_WAIT_FOR_TIMEOUT") == "1" {
		t.Log("intentional API cleanup timeout: production API is ready")
		// Only the test process pauses. The production API has no fault hook.
		// go test's timeout skips t.Cleanup; the outer launcher must stop it.
		<-make(chan struct{})
	}
	b.start(t)
	subscribed := func() {
		t.Helper()
		eventually(t, "two real Redis subscribers", 20*time.Second, func() bool {
			values, err := rc.PubSubNumSub(ctx, "gostream:realtime:v1").Result()
			return err == nil && values["gostream:realtime:v1"] == 2
		})
	}
	subscribed()
	owner, member, outsider := register(t, a.base, "im_owner"), register(t, b.base, "im_member"), register(t, a.base, "im_outsider")
	ownerDevice, memberOne, memberTwo := randomID(t), randomID(t), randomID(t)
	open := func(base string, who account, device string) *socket { return connect(t, base, who, device) }
	ownerA := open(a.base, owner, ownerDevice)
	ownerB := open(b.base, owner, randomID(t))
	memberB := open(b.base, member, memberOne)
	other := open(b.base, outsider, randomID(t))
	var group struct{ ID uint }
	call(t, a.base, "POST", "/api/groups", owner, "", map[string]string{"name": "isolated IM recovery"}, 201, &group)
	call(t, b.base, "POST", fmt.Sprintf("/api/groups/%d/members", group.ID), owner, "", map[string]uint{"user_id": member.User.ID}, 200, nil)
	groupPath := fmt.Sprintf("/api/groups/%d", group.ID)
	ackDirect := func(t *testing.T, device string, ids []uint, expected int) {
		t.Helper()
		var result struct {
			Count int `json:"acked_count"`
		}
		call(t, b.base, "POST", "/api/messages/acks", member, device, map[string]any{"ids": ids}, 200, &result)
		if result.Count != expected {
			t.Fatalf("new receipts=%d want=%d", result.Count, expected)
		}
	}
	unread := func(t *testing.T, device string) page {
		t.Helper()
		var p page
		call(t, b.base, "GET", "/api/messages/unread", member, device, nil, 200, &p)
		return p
	}
	check := func(name string, run func(*testing.T)) {
		t.Helper()
		if !t.Run(name, run) {
			t.Fatal("stopping dependent recovery scenarios after failure")
		}
	}
	presence := func(t *testing.T, base string, want bool) {
		t.Helper()
		var result struct {
			Online bool `json:"online"`
		}
		call(t, base, "GET", fmt.Sprintf("/api/users/%d/presence", member.User.ID), account{}, "", nil, 200, &result)
		if result.Online != want {
			t.Fatalf("presence online=%t want=%t", result.Online, want)
		}
	}
	var direct, grouped uint
	check("cross_instance_idempotency_permissions_and_duplicate_hints", func(t *testing.T) {
		// Both API processes race to persist the same sender/client identity.
		wire := map[string]any{"type": "message", "client_id": "concurrent-direct", "recipient_id": member.User.ID, "body": "remote direct"}
		concurrentWrites(t, ownerA, ownerB, wire)
		ackA := ownerA.next(t, "ack", func(e event) bool { return e.ClientID == "concurrent-direct" })
		ackB := ownerB.next(t, "ack", func(e event) bool { return e.ClientID == "concurrent-direct" })
		direct = ackA.MessageID
		if direct == 0 || ackB.MessageID != direct || count("SELECT COUNT(*) FROM chat_messages WHERE sender_id=? AND client_id=?", owner.User.ID, "concurrent-direct") != 1 {
			t.Fatal("concurrent retries did not resolve to one direct message")
		}
		memberB.next(t, "message", func(e event) bool { return e.Message.ID == direct && e.Message.Body == "remote direct" })
		ownerA.write(t, map[string]any{"type": "message", "client_id": "concurrent-direct", "recipient_id": member.User.ID, "body": "conflicting body"})
		ownerA.next(t, "error", nil)
		grouped = send(t, ownerA, "group_message", "cross-group", "remote group", group.ID)
		memberB.next(t, "group_message", func(e event) bool { return e.Message.ID == grouped && e.Message.GroupID == group.ID })
		if send(t, ownerB, "group_message", "cross-group", "remote group", group.ID) != grouped || count("SELECT COUNT(*) FROM group_messages WHERE sender_id=? AND client_id=?", owner.User.ID, "cross-group") != 1 {
			t.Fatal("cross-instance group retry duplicated a message")
		}
		other.absent(t, "group_message", grouped)
		call(t, a.base, "GET", groupPath+"/messages", outsider, "", nil, 404, nil)
		call(t, b.base, "POST", fmt.Sprintf("/api/messages/%d/ack", direct), outsider, randomID(t), nil, 404, nil)
		var private page
		call(t, a.base, "GET", fmt.Sprintf("/api/messages?user_id=%d", owner.User.ID), outsider, "", nil, 200, &private)
		if len(private.Items) != 0 {
			t.Fatal("third account could read a private conversation")
		}
		call(t, b.base, "POST", "/api/messages/acks", member, memberOne, map[string]any{"ids": []uint{direct, direct + 99999}}, 404, nil)
		if count("SELECT COUNT(*) FROM message_device_acks WHERE user_id=?", member.User.ID) != 0 {
			t.Fatal("mixed invalid ACK batch partially acknowledged a message")
		}
		other.write(t, map[string]any{"type": "group_message", "client_id": "forbidden", "group_id": group.ID, "body": "not a member"})
		other.next(t, "error", nil)
		if count("SELECT COUNT(*) FROM group_messages WHERE client_id='forbidden'") != 0 {
			t.Fatal("nonmember persisted a group message")
		}
		// Redis hints are intentionally repeatable; SQL remains a single row.
		for _, hint := range []map[string]any{
			{"node_id": "external-replay", "kind": "direct", "user_id": member.User.ID, "sender_id": owner.User.ID, "message_id": direct},
			{"node_id": "external-replay", "kind": "group", "group_id": group.ID, "sender_id": owner.User.ID, "message_id": grouped},
		} {
			body, _ := json.Marshal(hint)
			for i := 0; i < 2; i++ {
				if err := rc.Publish(ctx, "gostream:realtime:v1", body).Err(); err != nil {
					t.Fatal(err)
				}
			}
			kind, id := "message", direct
			if hint["kind"] == "group" {
				kind, id = "group_message", grouped
			}
			for i := 0; i < 2; i++ {
				memberB.next(t, kind, func(e event) bool { return e.Message.ID == id })
			}
		}
		if count("SELECT COUNT(*) FROM chat_messages") != 1 || count("SELECT COUNT(*) FROM group_messages") != 1 {
			t.Fatal("duplicate hints changed persistent history")
		}
		ackDirect(t, memberOne, []uint{direct}, 1)
		ackDirect(t, memberOne, []uint{direct}, 0)
		if len(unread(t, memberOne).Items) != 0 || len(unread(t, memberTwo).Items) != 1 {
			t.Fatal("one device ACK cleared another device's pending message")
		}
	})
	check("redis_outage_sql_recovery_and_resubscription", func(t *testing.T) {
		presence(t, a.base, true)
		// The name guard above and the launcher guarantee this is not demo Redis.
		runDocker := func(action string) {
			t.Helper()
			commandCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if err := exec.CommandContext(commandCtx, "docker", action, container).Run(); err != nil {
				t.Fatalf("disposable Redis %s failed: %v", action, err)
			}
		}
		runDocker("stop")
		defer runDocker("start")
		presence(t, a.base, false)
		presence(t, b.base, true)
		missed := send(t, ownerA, "message", "redis-down-direct", "durable while Redis is down", member.User.ID)
		missedGroup := send(t, ownerA, "group_message", "redis-down-group", "durable group while Redis is down", group.ID)
		memberB.absent(t, "message", missed)
		memberB.absent(t, "group_message", missedGroup)
		pending := unread(t, memberOne)
		if len(pending.Items) != 1 || pending.Items[0].ID != missed {
			t.Fatal("SQL unread did not recover direct hint lost during Redis outage")
		}
		var history page
		call(t, b.base, "GET", groupPath+"/messages", member, memberOne, nil, 200, &history)
		if len(history.Items) != 2 || history.Items[0].ID != missedGroup {
			t.Fatal("SQL history did not recover group hint lost during Redis outage")
		}
		ownerA.write(t, map[string]any{"type": "ping"})
		ownerA.next(t, "pong", nil)
		runDocker("start")
		subscribed()
		recovered := send(t, ownerA, "message", "redis-return-direct", "remote after resubscription", member.User.ID)
		memberB.next(t, "message", func(e event) bool { return e.Message.ID == recovered })
		ownerB.next(t, "message", func(e event) bool { return e.Message.ID == recovered })
		back := send(t, ownerA, "group_message", "redis-return-group", "group after resubscription", group.ID)
		memberB.next(t, "group_message", func(e event) bool { return e.Message.ID == back })
		ownerB.next(t, "group_message", func(e event) bool { return e.Message.ID == back })
		secondDevice := open(b.base, member, memberTwo)
		presence(t, a.base, true)
		_ = secondDevice.conn.Close()
	})
	check("recipient_sigkill_reconnect_and_sender_ack_retry", func(t *testing.T) {
		// A fresh real connection renews this node's production-default lease.
		freshPresence := open(b.base, member, randomID(t))
		_ = freshPresence
		scores, err := rc.ZRangeWithScores(ctx, fmt.Sprintf("gostream:presence:%d", member.User.ID), 0, -1).Result()
		if err != nil || len(scores) != 1 {
			t.Fatalf("presence scores: %v %v", scores, err)
		}
		remaining := time.Until(time.UnixMilli(int64(scores[0].Score)))
		if remaining < 70*time.Second || remaining > 76*time.Second {
			t.Fatalf("expected 75-second production lease, remaining=%v", remaining)
		}
		started := time.Now()
		b.stop(t, true)
		// Process b was SIGKILL'd, so clearPresence was never called.
		// Immediately after crash, member should still be reported online via Redis.
		presence(t, a.base, true)
		// After the production 75-second presence lease expires, member should transition to offline.
		lastHeartbeat := time.Now()
		eventually(t, "crashed node presence expiry", 85*time.Second, func() bool {
			if time.Since(lastHeartbeat) > 10*time.Second {
				ownerA.write(t, map[string]any{"type": "ping"})
				ownerA.next(t, "pong", nil)
				lastHeartbeat = time.Now()
			}
			var result struct {
				Online bool `json:"online"`
			}
			call(t, a.base, "GET", fmt.Sprintf("/api/users/%d/presence", member.User.ID), account{}, "", nil, 200, &result)
			return !result.Online
		})
		t.Logf("observed production presence expiry after %v", time.Since(started))
		offline := send(t, ownerA, "message", "recipient-killed", "recipient process is down", member.User.ID)
		offlineGroup := send(t, ownerA, "group_message", "recipient-killed-group", "recipient group process is down", group.ID)
		b.start(t)
		subscribed()
		memberB = open(b.base, member, memberOne)
		presence(t, a.base, true)
		memberB.next(t, "message", func(e event) bool { return e.Message.ID == offline })
		memberB.next(t, "group_message", func(e event) bool { return e.Message.ID == offlineGroup })
		// Persist before killing the sender, then retry the same key elsewhere.
		ownerA.write(t, map[string]any{"type": "message", "client_id": "sender-killed", "recipient_id": member.User.ID, "body": "retry after sender restart"})
		eventually(t, "committed message before sender kill", 3*time.Second, func() bool {
			return count("SELECT COUNT(*) FROM chat_messages WHERE client_id='sender-killed'") == 1
		})
		committed := uint(count("SELECT id FROM chat_messages WHERE client_id='sender-killed'"))
		a.stop(t, true)
		ownerB = open(b.base, owner, ownerDevice)
		if retry := send(t, ownerB, "message", "sender-killed", "retry after sender restart", member.User.ID); retry != committed || count("SELECT COUNT(*) FROM chat_messages WHERE client_id='sender-killed'") != 1 {
			t.Fatal("retry after real sender process loss changed message identity")
		}
		a.start(t)
		subscribed()
		ownerA = open(a.base, owner, ownerDevice)
	})
	check("over_100_replay_pagination_and_independent_device_acks", func(t *testing.T) {
		_ = memberB.conn.Close()
		// Clear earlier fixtures through the public ACK API, on both devices.
		for _, device := range []string{memberOne, memberTwo} {
			pending := unread(t, device)
			ids := make([]uint, 0, len(pending.Items))
			for _, msg := range pending.Items {
				ids = append(ids, msg.ID)
			}
			if len(ids) > 0 {
				ackDirect(t, device, ids, len(ids))
			}
			last := count("SELECT MAX(id) FROM group_messages WHERE group_id=?", group.ID)
			call(t, b.base, "POST", fmt.Sprintf("%s/messages/%d/ack", groupPath, last), member, device, nil, 200, nil)
		}
		directIDs, groupIDs := make([]uint, 0, 105), make([]uint, 0, 105)
		for i := 0; i < 105; i++ {
			directIDs = append(directIDs, send(t, ownerA, "message", fmt.Sprintf("backlog-d-%d", i), "offline direct page", member.User.ID))
			groupIDs = append(groupIDs, send(t, ownerA, "group_message", fmt.Sprintf("backlog-g-%d", i), "offline group page", group.ID))
		}
		memberB = open(b.base, member, memberOne)
		for _, expected := range directIDs[:100] {
			got := memberB.next(t, "message", nil)
			if got.Message.ID != expected {
				t.Fatalf("direct replay id=%d want=%d", got.Message.ID, expected)
			}
		}
		memberB.next(t, "sync_required", nil)
		for _, expected := range groupIDs[:100] {
			got := memberB.next(t, "group_message", nil)
			if got.Message.ID != expected {
				t.Fatalf("group replay id=%d want=%d", got.Message.ID, expected)
			}
		}
		memberB.next(t, "group_sync_required", nil)
		var collected []uint
		var cursor uint
		for pages := 0; pages < 5; pages++ {
			var p page
			call(t, b.base, "GET", fmt.Sprintf("/api/messages/unread?after_id=%d&limit=40", cursor), member, memberOne, nil, 200, &p)
			for _, msg := range p.Items {
				collected = append(collected, msg.ID)
			}
			if !p.More {
				break
			}
			if p.Next <= cursor {
				t.Fatal("unread cursor did not advance")
			}
			cursor = p.Next
		}
		if !slices.Equal(collected, directIDs) {
			t.Fatal("direct unread pagination skipped or repeated a message")
		}
		for _, scenario := range []struct {
			path string
			ids  []uint
		}{{fmt.Sprintf("/api/messages?user_id=%d&", owner.User.ID), directIDs}, {groupPath + "/messages?", groupIDs}} {
			var all []uint
			var before uint
			for pages := 0; pages < 5; pages++ {
				var history page
				call(t, a.base, "GET", fmt.Sprintf("%sbefore_id=%d&limit=40", scenario.path, before), member, memberOne, nil, 200, &history)
				for _, msg := range history.Items {
					if len(all) > 0 && all[len(all)-1] <= msg.ID {
						t.Fatal("history pages are not strictly descending by server ID")
					}
					all = append(all, msg.ID)
				}
				if !history.More {
					break
				}
				if history.Next == 0 || (before != 0 && history.Next >= before) {
					t.Fatal("history cursor did not advance")
				}
				before = history.Next
			}
			expected := slices.Clone(scenario.ids)
			slices.Reverse(expected)
			if len(all) < len(expected) || !slices.Equal(all[:len(expected)], expected) {
				t.Fatal("history pagination lost or repeated part of the 105-message backlog")
			}
		}
		for start := 0; start < len(directIDs); start += 50 {
			end := min(start+50, len(directIDs))
			ackDirect(t, memberOne, directIDs[start:end], end-start)
		}
		ackDirect(t, memberOne, directIDs[:5], 0)
		if len(unread(t, memberOne).Items) != 0 {
			t.Fatal("acknowledged device still has unread direct messages")
		}
		otherPending := unread(t, memberTwo)
		if len(otherPending.Items) != 100 || !otherPending.More {
			t.Fatal("device ACK changed the other device's direct backlog")
		}
		call(t, a.base, "POST", fmt.Sprintf("%s/messages/%d/ack", groupPath, groupIDs[104]), member, memberOne, nil, 200, nil)
		for _, scenario := range []struct {
			device string
			want   int
		}{{memberOne, 0}, {memberTwo, 105}} {
			var result struct {
				Count int `json:"unread_count"`
			}
			call(t, b.base, "GET", groupPath+"/unread", member, scenario.device, nil, 200, &result)
			if result.Count != scenario.want {
				t.Fatalf("group device unread=%d want=%d", result.Count, scenario.want)
			}
		}
	})
	check("membership_revocation_blocks_live_push_and_history", func(t *testing.T) {
		call(t, a.base, "DELETE", fmt.Sprintf("%s/members/%d", groupPath, member.User.ID), owner, "", nil, 200, nil)
		afterRemoval := send(t, ownerA, "group_message", "after-removal", "removed members cannot read this", group.ID)
		ownerB.next(t, "group_message", func(e event) bool { return e.Message.ID == afterRemoval })
		memberB.absent(t, "group_message", afterRemoval)
		call(t, b.base, "GET", groupPath+"/messages", member, memberOne, nil, 404, nil)
		call(t, a.base, "GET", groupPath+"/unread", member, memberOne, nil, 404, nil)
		memberB.write(t, map[string]any{"type": "group_message", "client_id": "removed-sender", "group_id": group.ID, "body": "cannot send"})
		memberB.next(t, "error", nil)
		if count("SELECT COUNT(*) FROM group_messages WHERE client_id='removed-sender'") != 0 {
			t.Fatal("removed member sent a message")
		}
	})
	check("compacted_device_receipts_survive_process_loss", func(t *testing.T) {
		if count("SELECT COUNT(*) FROM message_device_ack_ranges WHERE user_id=? AND device_id=?", member.User.ID, memberOne) == 0 {
			t.Fatal("fixture did not actually compact receipts")
		}
		b.stop(t, true)
		b.start(t)
		subscribed()
		if p := unread(t, memberOne); len(p.Items) != 0 {
			t.Fatal("compacted ACKs were lost on process restart")
		}
		if p := unread(t, memberTwo); len(p.Items) != 100 || !p.More {
			t.Fatal("compaction suppressed the other device's backlog")
		}
		memberB = open(b.base, member, memberOne)
		memberB.write(t, map[string]any{"type": "ping"})
		if err := waitReplayBarrier(memberB.read, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	})
}

// TestFeedCacheProcessRedisRecovery exercises shared version invalidation and
// cache recovery across two production API processes, not in-process routers.
func TestFeedCacheProcessRedisRecovery(t *testing.T) {
	binary := os.Getenv("GOSTREAM_TEST_IM_BINARY")
	dsn := os.Getenv("GOSTREAM_TEST_IM_MYSQL_DSN")
	redisAddr := os.Getenv("GOSTREAM_TEST_IM_REDIS_ADDR")
	container := os.Getenv("GOSTREAM_TEST_IM_REDIS_CONTAINER")
	if binary == "" || dsn == "" || redisAddr == "" || container == "" {
		t.Skip("requires isolated API binary, empty MySQL schema and disposable Redis; use tests/integration/im_smoke.py --suite feed-cache")
	}
	if !strings.HasPrefix(container, "gostream-im-test-") {
		t.Fatal("refusing to stop a Redis container outside the disposable gostream-im-test- namespace")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("invalid isolated MySQL configuration")
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("requires a newly created empty schema: table_count=%d err=%v", tables, err)
	}
	rc := redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: 0})
	t.Cleanup(func() { _ = rc.Close() })
	if keys, err := rc.DBSize(ctx).Result(); err != nil || keys != 0 {
		t.Fatalf("requires an empty dedicated Redis server: keys=%d err=%v", keys, err)
	}
	secret := randomID(t) + randomID(t)
	a, b := newProcess(t, binary, dsn, redisAddr, secret), newProcess(t, binary, dsn, redisAddr, secret)
	a.start(t)
	b.start(t)
	owner := register(t, a.base, "feed_owner_"+randomID(t)[:8])
	secondAuthor := register(t, a.base, "feed_author_"+randomID(t)[:8])
	viewer := register(t, a.base, "feed_viewer_"+randomID(t)[:8])
	insertVideo := func(authorID uint, title string, createdAt time.Time) uint {
		t.Helper()
		result, err := db.ExecContext(ctx, "INSERT INTO videos (author_id,title,media_url,status,created_at) VALUES (?,?,?,'published',?)", authorID, title, "/media/"+randomID(t)+".mp4", createdAt.UTC())
		if err != nil {
			t.Fatalf("insert isolated Feed video: %v", err)
		}
		id, err := result.LastInsertId()
		if err != nil || id <= 0 {
			t.Fatalf("read isolated Feed video ID: id=%d err=%v", id, err)
		}
		return uint(id)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	ownerVideo := insertVideo(owner.User.ID, "feed owner", now.Add(-time.Second))
	authorVideo := insertVideo(secondAuthor.User.ID, "second author", now)
	call(t, a.base, "PUT", fmt.Sprintf("/api/users/me/following/%d", owner.User.ID), viewer, "", nil, http.StatusOK, nil)
	first := followingFeed(t, a.base, viewer)
	if len(first) != 1 || first[0].ID != ownerVideo {
		t.Fatalf("initial process Following Feed: %+v", first)
	}
	// The mutation is sent to process B. A must observe it through the shared
	// per-viewer Redis version and invalidate the page it cached above.
	call(t, b.base, "PUT", fmt.Sprintf("/api/users/me/following/%d", secondAuthor.User.ID), viewer, "", nil, http.StatusOK, nil)
	updated := followingFeed(t, a.base, viewer)
	if len(updated) != 2 || updated[0].ID != authorVideo || updated[1].ID != ownerVideo {
		t.Fatalf("process A served a stale page after process B follow: %+v", updated)
	}
	_ = followingFeed(t, a.base, viewer)
	initialHits := feedCacheHitCount(t, a.base)
	if initialHits < 1 {
		t.Fatal("process A did not serve the warmed Following page from Redis")
	}

	stopRedis := func(action string) {
		t.Helper()
		commandCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := exec.CommandContext(commandCtx, "docker", action, container).Run(); err != nil {
			t.Fatalf("disposable Redis %s failed: %v", action, err)
		}
	}
	stopRedis("stop")
	redisStopped := true
	t.Cleanup(func() {
		if redisStopped {
			stopRedis("start")
		}
	})
	// MySQL remains authoritative while Redis is down. The successful SQL
	// unfollow cannot bump the cache version, so process A must bypass Redis.
	call(t, b.base, "DELETE", fmt.Sprintf("/api/users/me/following/%d", owner.User.ID), viewer, "", nil, http.StatusOK, nil)
	duringOutage := followingFeed(t, a.base, viewer)
	if len(duringOutage) != 1 || duringOutage[0].ID != authorVideo {
		t.Fatalf("Following Feed did not reflect SQL state during Redis outage: %+v", duringOutage)
	}
	stopRedis("start")
	redisStopped = false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		items := followingFeed(t, a.base, viewer)
		if len(items) != 1 || items[0].ID != authorVideo {
			t.Fatalf("stale Following page after Redis recovery: %+v", items)
		}
		if feedCacheHitCount(t, a.base) > initialHits {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("process A did not rebuild and hit its Following cache after Redis recovery")
}

// waitReplayBarrier verifies no direct/group backlog precedes the post-replay pong.
// The production handler replays both backlogs before reading ping. Notifications
// may arrive concurrently through realtime delivery and do not imply IM replay.
func waitReplayBarrier(frames <-chan event, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		// A notification flood must not postpone the fixed replay deadline.
		if !time.Now().Before(deadline) {
			return fmt.Errorf("replay barrier timed out")
		}
		select {
		case frame, ok := <-frames:
			if !ok {
				return fmt.Errorf("socket closed before replay barrier")
			}
			switch frame.Type {
			case "pong":
				return nil
			case "notification":
				continue
			default:
				return fmt.Errorf("unexpected replay frame %q", frame.Type)
			}
		case <-timer.C:
			return fmt.Errorf("replay barrier timed out")
		}
	}
}
