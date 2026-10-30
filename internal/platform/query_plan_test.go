package platform

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type sqliteQueryPlanRow struct {
	ID     int    `gorm:"column:id"`
	Parent int    `gorm:"column:parent"`
	NotUse int    `gorm:"column:notused"`
	Detail string `gorm:"column:detail"`
}

type mysqlExplainRow struct {
	ID           int            `gorm:"column:id"`
	SelectType   string         `gorm:"column:select_type"`
	Table        string         `gorm:"column:table"`
	Partitions   sql.NullString `gorm:"column:partitions"`
	Type         string         `gorm:"column:type"`
	PossibleKeys sql.NullString `gorm:"column:possible_keys"`
	Key          sql.NullString `gorm:"column:key"`
	KeyLen       sql.NullString `gorm:"column:key_len"`
	Ref          sql.NullString `gorm:"column:ref"`
	Rows         int64          `gorm:"column:rows"`
	Filtered     float64        `gorm:"column:filtered"`
	Extra        string         `gorm:"column:Extra"`
}

func explainSQLite(t *testing.T, db *gorm.DB, query string, args ...any) []string {
	t.Helper()
	var rows []sqliteQueryPlanRow
	if err := db.Raw("EXPLAIN QUERY PLAN "+query, args...).Scan(&rows).Error; err != nil {
		t.Fatalf("sqlite EXPLAIN failed for query %q: %v", query, err)
	}
	details := make([]string, len(rows))
	for i, r := range rows {
		details[i] = r.Detail
	}
	return details
}

func explainMySQL(t *testing.T, db *gorm.DB, query string, args ...any) []mysqlExplainRow {
	t.Helper()
	var rows []mysqlExplainRow
	if err := db.Raw("EXPLAIN "+query, args...).Scan(&rows).Error; err != nil {
		t.Fatalf("mysql EXPLAIN failed for query %q: %v", query, err)
	}
	return rows
}

func TestQueryPlansSQLite(t *testing.T) {
	_, _, db := testServer(t)

	// Populate sample data so the query planner has stats
	now := time.Now().UTC()
	user := User{Username: "qp_user", Nickname: "QP User", PasswordHash: "unused"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	other := User{Username: "qp_other", Nickname: "QP Other", PasswordHash: "unused"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 20; i++ {
		_ = db.Create(&Notification{
			UserID:    user.ID,
			ActorID:   other.ID,
			Kind:      "follow",
			EventKey:  fmt.Sprintf("notif-%d", i),
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		})
		_ = db.Create(&ChatMessage{
			SenderID:    user.ID,
			RecipientID: other.ID,
			ClientID:    fmt.Sprintf("chat-%d", i),
			Body:        "test message",
			CreatedAt:   now.Add(time.Duration(i) * time.Minute),
		})
		_ = db.Create(&Video{
			AuthorID:  user.ID,
			Title:     fmt.Sprintf("Video %d", i),
			MediaURL:  fmt.Sprintf("/media/v%d.mp4", i),
			Status:    "published",
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		})
		_ = db.Create(&VideoAction{
			UserID:    user.ID,
			VideoID:   uint(i),
			Kind:      "like",
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		})
		_ = db.Create(&Comment{
			VideoID:   1,
			UserID:    user.ID,
			Body:      "comment",
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		})
		_ = db.Create(&FeedInbox{
			UserID:    user.ID,
			VideoID:   uint(i),
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		})
	}

	queries := []struct {
		name           string
		query          string
		args           []any
		wantIndex      string
		allowTempBTree bool
	}{
		{
			name:      "notification_keyset_page",
			query:     "SELECT * FROM notifications WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 31",
			args:      []any{user.ID, 100},
			wantIndex: "idx_notification_user_id",
		},
		{
			name:      "notification_unread_count",
			query:     "SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL",
			args:      []any{user.ID},
			wantIndex: "idx_notification_unread",
		},
		{
			name:      "direct_message_history_page",
			query:     "SELECT * FROM chat_messages WHERE sender_id = ? AND recipient_id = ? AND id < ? ORDER BY id DESC LIMIT 31",
			args:      []any{user.ID, other.ID, 100},
			wantIndex: "idx_message_thread_id",
		},
		{
			name:      "direct_message_unacked",
			query:     "SELECT * FROM chat_messages WHERE recipient_id = ? AND acked_at IS NULL ORDER BY id ASC LIMIT 101",
			args:      []any{other.ID},
			wantIndex: "idx_message_unacked",
		},
		{
			name:      "direct_message_device_antijoin",
			query:     "SELECT chat_messages.* FROM chat_messages LEFT JOIN message_device_acks ON message_device_acks.message_id = chat_messages.id AND message_device_acks.user_id = ? AND message_device_acks.device_id = ? WHERE chat_messages.recipient_id = ? AND chat_messages.acked_at IS NULL AND message_device_acks.id IS NULL ORDER BY chat_messages.id ASC LIMIT 101",
			args:      []any{other.ID, "test-device", other.ID},
			wantIndex: "idx_message_unacked",
		},
		{
			name:           "group_message_history_page",
			query:          "SELECT * FROM group_messages WHERE group_id = ? AND id < ? ORDER BY id DESC LIMIT 31",
			args:           []any{1, 100},
			wantIndex:      "idx_group_message",
			allowTempBTree: true, // v001 index is (group_id, created_at)
		},
		{
			name:      "video_feed_timeline",
			query:     "SELECT * FROM videos WHERE status = ? AND created_at < ? ORDER BY created_at DESC, id DESC LIMIT 21",
			args:      []any{"published", now},
			wantIndex: "idx_video_feed",
		},
		{
			name:      "video_feed_author",
			query:     "SELECT * FROM videos WHERE author_id = ? AND status = ? ORDER BY created_at DESC LIMIT 50",
			args:      []any{user.ID, "published"},
			wantIndex: "idx_video_author",
		},
		{
			name:      "video_action_recent",
			query:     "SELECT * FROM video_actions WHERE user_id = ? ORDER BY id DESC LIMIT 200",
			args:      []any{user.ID},
			wantIndex: "idx_action_recent",
		},
		{
			name:      "comment_thread_roots",
			query:     "SELECT * FROM comments WHERE video_id = ? AND parent_id IS NULL AND created_at < ? ORDER BY created_at DESC, id DESC LIMIT 31",
			args:      []any{1, now},
			wantIndex: "idx_comment_thread",
		},
		{
			name:      "feed_inbox_page",
			query:     "SELECT * FROM feed_inboxes WHERE user_id = ? AND created_at < ? ORDER BY created_at DESC, video_id DESC LIMIT 21",
			args:      []any{user.ID, now},
			wantIndex: "idx_feed_inbox_user",
		},
		{
			name:      "user_search_keyset",
			query:     "SELECT * FROM users WHERE (username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21",
			args:      []any{"%test%", "%test%", 100},
			wantIndex: "PRIMARY", // SQLite uses INTEGER PRIMARY KEY (rowid < ?)
		},
		{
			name:           "video_search_keyset",
			query:          "SELECT * FROM videos WHERE status = ? AND (title LIKE ? ESCAPE '!' OR description LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21",
			args:           []any{"published", "%test%", "%test%", 100},
			wantIndex:      "",   // Substring LIKE requires filtered scan across published videos
			allowTempBTree: true, // Status index scan + temp B-tree sort
		},
	}

	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			details := explainSQLite(t, db, tc.query, tc.args...)
			fullPlan := strings.Join(details, " | ")
			t.Logf("Plan for %s: %s", tc.name, fullPlan)
			if !strings.Contains(fullPlan, tc.wantIndex) {
				t.Fatalf("query plan for %s did not use expected index %q: %s", tc.name, tc.wantIndex, fullPlan)
			}
			// Verify plan does NOT require a temporary B-tree for sorting unless explicitly allowed
			if strings.Contains(fullPlan, "USE TEMP B-TREE FOR ORDER BY") && !tc.allowTempBTree {
				t.Fatalf("query plan for %s used temp B-tree for ORDER BY: %s", tc.name, fullPlan)
			}
		})
	}
}

func TestRedundantIndexesAnalysis(t *testing.T) {
	_, _, db := testServer(t)

	// 1. Notifications keyset pagination uses idx_notification_user_id, leaving idx_notification_page unused in runtime
	plans := explainSQLite(t, db, "SELECT * FROM notifications WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 31", 1, 100)
	planStr := strings.Join(plans, " | ")
	if !strings.Contains(planStr, "idx_notification_user_id") {
		t.Fatalf("expected idx_notification_user_id for notifications keyset, got %s", planStr)
	}
	if strings.Contains(planStr, "idx_notification_page") {
		t.Fatalf("expected idx_notification_page NOT to be used for keyset pagination, got %s", planStr)
	}

	// 2. Chat messages keyset pagination uses idx_message_thread_id, leaving idx_message_thread unused in runtime
	plans = explainSQLite(t, db, "SELECT * FROM chat_messages WHERE sender_id = ? AND recipient_id = ? AND id < ? ORDER BY id DESC LIMIT 31", 1, 2, 100)
	planStr = strings.Join(plans, " | ")
	if !strings.Contains(planStr, "idx_message_thread_id") {
		t.Fatalf("expected idx_message_thread_id for chat keyset, got %s", planStr)
	}
	if strings.Contains(planStr, "idx_message_thread ") || strings.HasSuffix(planStr, "idx_message_thread") {
		t.Fatalf("expected idx_message_thread NOT to be used for keyset pagination, got %s", planStr)
	}
}

func TestMySQLQueryPlans(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run the MySQL query plans test")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	db = tx
	now := time.Now().UTC()
	user := User{Username: fmt.Sprintf("qp_my_%d", now.UnixNano()), Nickname: "QP User", PasswordHash: "unused"}
	peer := User{Username: fmt.Sprintf("qp_peer_%d", now.UnixNano()), Nickname: "Peer", PasswordHash: "unused"}
	for _, u := range []*User{&user, &peer} {
		if err := db.Create(u).Error; err != nil {
			t.Fatal(err)
		}
	}
	const count = 500
	videos := make([]Video, count)
	for i := range videos {
		author := user.ID
		if i%2 == 1 {
			author = peer.ID
		}
		videos[i] = Video{AuthorID: author, Title: "Plan fixture", MediaURL: "/media/plan.mp4", Status: "published", CreatedAt: now.Add(-time.Duration(i+1) * time.Second)}
	}
	if err := db.CreateInBatches(&videos, 100).Error; err != nil {
		t.Fatal(err)
	}
	notices := make([]Notification, count)
	messages := make([]ChatMessage, count)
	actions := make([]VideoAction, count)
	comments := make([]Comment, count)
	inboxes := make([]FeedInbox, count)
	for i := range count {
		viewer, sender := user.ID, peer.ID
		if i%2 == 1 {
			viewer, sender = peer.ID, user.ID
		}
		stamp := now.Add(-time.Duration(i+1) * time.Second)
		notices[i] = Notification{UserID: viewer, ActorID: sender, Kind: "follow", EventKey: fmt.Sprintf("qp_%d", i), CreatedAt: stamp}
		messages[i] = ChatMessage{SenderID: sender, RecipientID: viewer, ClientID: fmt.Sprintf("qp_%d", i), Body: "plan fixture", CreatedAt: stamp}
		actions[i] = VideoAction{UserID: viewer, VideoID: videos[i].ID, Kind: "like", CreatedAt: stamp}
		comments[i] = Comment{UserID: viewer, VideoID: videos[i%2].ID, Body: "plan fixture", CreatedAt: stamp}
		inboxes[i] = FeedInbox{UserID: viewer, VideoID: videos[i].ID, CreatedAt: stamp}
	}
	for _, rows := range []any{&notices, &messages, &actions, &comments, &inboxes} {
		if err := db.CreateInBatches(rows, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Log("EXPLAIN fixture: two users, 500 rows per business table; rollback after test; simplified probes, not production workload")

	queries := []struct {
		name      string
		query     string
		args      []any
		wantIndex string
		allowScan bool
	}{
		{
			name:      "notification_keyset_page",
			query:     "SELECT * FROM notifications WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 31",
			args:      []any{user.ID, notices[len(notices)-1].ID + 1},
			wantIndex: "idx_notification_user_id",
		},
		{
			name:      "notification_unread_count",
			query:     "SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL",
			args:      []any{user.ID},
			wantIndex: "idx_notification_unread",
		},
		{
			name:      "direct_message_history_page",
			query:     "SELECT * FROM chat_messages WHERE sender_id = ? AND recipient_id = ? AND id < ? ORDER BY id DESC LIMIT 31",
			args:      []any{user.ID, peer.ID, messages[len(messages)-1].ID + 1},
			wantIndex: "idx_message_thread_id",
		},
		{
			name:      "direct_message_unacked",
			query:     "SELECT * FROM chat_messages WHERE recipient_id = ? AND acked_at IS NULL ORDER BY id ASC LIMIT 101",
			args:      []any{user.ID},
			wantIndex: "idx_message_unacked",
		},
		{
			name:      "video_feed_timeline",
			query:     "SELECT * FROM videos WHERE status = ? AND created_at < ? ORDER BY created_at DESC, id DESC LIMIT 21",
			args:      []any{"published", now},
			wantIndex: "idx_video_feed",
		},
		{
			name:      "video_feed_author",
			query:     "SELECT * FROM videos WHERE author_id = ? AND status = ? ORDER BY created_at DESC LIMIT 50",
			args:      []any{user.ID, "published"},
			wantIndex: "idx_video_author",
		},
		{
			name:      "video_action_recent",
			query:     "SELECT * FROM video_actions WHERE user_id = ? ORDER BY id DESC LIMIT 200",
			args:      []any{user.ID},
			wantIndex: "idx_action_recent",
		},
		{
			name:      "comment_thread_roots",
			query:     "SELECT * FROM comments WHERE video_id = ? AND parent_id IS NULL AND created_at < ? ORDER BY created_at DESC, id DESC LIMIT 31",
			args:      []any{videos[0].ID, now},
			wantIndex: "idx_comment_thread",
		},
		{
			name:      "feed_inbox_page",
			query:     "SELECT * FROM feed_inboxes WHERE user_id = ? AND created_at < ? ORDER BY created_at DESC, video_id DESC LIMIT 21",
			args:      []any{user.ID, now},
			wantIndex: "idx_feed_inbox_user",
		},
		{
			name:      "user_search_keyset",
			query:     "SELECT * FROM users WHERE (username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21",
			args:      []any{"%QP%", "%QP%", peer.ID + 1},
			wantIndex: "PRIMARY",
		},
	}

	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			rows := explainMySQL(t, db, tc.query, tc.args...)
			if len(rows) == 0 {
				t.Fatalf("no explain output for %s", tc.name)
			}
			r := rows[0]
			keyUsed := ""
			if r.Key.Valid {
				keyUsed = r.Key.String
			}
			t.Logf("MySQL Plan for %s: table=%s type=%s key=%s rows=%d extra=%s",
				tc.name, r.Table, r.Type, keyUsed, r.Rows, r.Extra)

			if !strings.EqualFold(keyUsed, tc.wantIndex) {
				t.Fatalf("MySQL query plan for %s used key %q, want %q", tc.name, keyUsed, tc.wantIndex)
			}
			if r.Type == "ALL" && !tc.allowScan {
				t.Fatalf("MySQL query plan for %s did full table scan (type=ALL)", tc.name)
			}
			if strings.Contains(r.Extra, "Using filesort") {
				t.Fatalf("MySQL query plan for %s used filesort: %s", tc.name, r.Extra)
			}
		})
	}
}
