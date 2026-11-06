package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type productionFixtureMeta struct {
	AdminID           uint
	AdminToken         string
	ViewerID          uint
	ViewerToken        string
	StrangerID        uint
	StrangerToken      string
	TotalUsers        int
	TotalVideos       int
	TotalNotifications int
	TargetUserIDs     []uint
	TargetVideoIDs    []uint
	SameTimeUserIDs   []uint
	SameTimeVideoIDs  []uint
	HiddenVideoIDs    []uint
}

func seedProductionSearchAndNotificationDataset(t *testing.T, s *Server, db *gorm.DB) productionFixtureMeta {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)

	// Admin (video author), Viewer (notifications recipient), Stranger
	adminID, adminTok := registerTestUser(t, s.Router(), "prod_admin")
	viewerID, viewerTok := registerTestUser(t, s.Router(), "prod_viewer")
	strangerID, strangerTok := registerTestUser(t, s.Router(), "prod_stranger")

	meta := productionFixtureMeta{
		AdminID:       adminID,
		AdminToken:    adminTok,
		ViewerID:      viewerID,
		ViewerToken:   viewerTok,
		StrangerID:    strangerID,
		StrangerToken: strangerTok,
	}

	// 1. Seed 2,000 Users
	const totalUserCount = 2000
	const userBatchSize = 400
	users := make([]User, totalUserCount)
	sameTime := now.Add(-10 * time.Hour)

	for i := range users {
		username := fmt.Sprintf("prod-u-%06d", i)
		nickname := fmt.Sprintf("Prod User %06d", i)
		createdAt := now.Add(-time.Duration(totalUserCount-i) * time.Minute)

		switch {
		case i == 0:
			username = "user_hit_alpha"
			nickname = "Alpha Target User"
		case i == 1:
			username = "user_hit_beta"
			nickname = "Beta Target User"
		case i == 2:
			username = "user_hit_gamma"
			nickname = "Gamma Target User"
		case i == 3:
			username = "literal%_user"
			nickname = "Literal Percent User"
		case i == 4:
			username = "literal__user"
			nickname = "Literal Underscore User"
		case i == 5:
			username = "literal!_user"
			nickname = "Literal Exclamation User"
		case i == 6:
			username = "unicode_zhangsan"
			nickname = "测试用户_张三"
		case i == 7:
			username = "unicode_lisi"
			nickname = "测试用户_李四"
		case i == 8:
			username = "emoji_rocket"
			nickname = "🚀火箭发射员"
		case i >= 10 && i < 30:
			// 20 users with identical timestamp
			username = fmt.Sprintf("sametime_u_%02d", i-10)
			nickname = fmt.Sprintf("SameTime User %02d", i-10)
			createdAt = sameTime
		}

		users[i] = User{
			Username:     username,
			Nickname:     nickname,
			PasswordHash: "unused_test_hash",
			CreatedAt:    createdAt,
		}
	}

	if err := db.CreateInBatches(&users, userBatchSize).Error; err != nil {
		t.Fatalf("failed to insert 2000 users: %v", err)
	}
	meta.TotalUsers = totalUserCount

	// Track target users
	for i := 0; i < 9; i++ {
		meta.TargetUserIDs = append(meta.TargetUserIDs, users[i].ID)
	}
	for i := 10; i < 30; i++ {
		meta.SameTimeUserIDs = append(meta.SameTimeUserIDs, users[i].ID)
	}

	// 2. Seed 20,000 Videos
	const totalVideoCount = 20000
	const videoBatchSize = 500
	videos := make([]Video, totalVideoCount)

	for i := range videos {
		status := "published"
		title := fmt.Sprintf("prod video %06d", i)
		desc := fmt.Sprintf("Description for production video %06d", i)
		authorID := adminID
		if i%10 == 0 {
			authorID = users[i%totalUserCount].ID
		}
		createdAt := now.Add(-time.Duration(totalVideoCount-i) * time.Second)

		switch {
		case i < 10:
			title = fmt.Sprintf("GoStream 架构剖析 第%02d集", i+1)
			desc = "GoStream 分布式高并发微服务与流媒体核心架构设计"
		case i == 10:
			title = "video_literal%_target"
			desc = "100%_clip! literal search fixture"
		case i == 11:
			title = "video_literal__target"
			desc = "underline_video_title test"
		case i == 12:
			title = "video_literal!_target"
			desc = "exclamation!_video test"
		case i == 13:
			title = "🚀航天探索纪录片"
			desc = "星际航行与深空探索"
		case i >= 20 && i < 40:
			// 20 videos with identical timestamp
			title = fmt.Sprintf("sametime_vid_%02d", i-20)
			desc = "Identical timestamp batch video"
			createdAt = sameTime
		case i >= 50 && i < 70:
			// 20 hidden videos with matching keywords
			status = "draft"
			title = fmt.Sprintf("hidden_draft_clip_%02d", i-50)
			desc = "Confidential draft not published"
		case i >= 70 && i < 90:
			status = "processing"
			title = fmt.Sprintf("hidden_proc_clip_%02d", i-70)
			desc = "Processing video"
		case i >= 90 && i < 110:
			status = "failed"
			title = fmt.Sprintf("hidden_failed_clip_%02d", i-90)
			desc = "Failed video"
		case i >= 110 && i < 130:
			status = "deleted"
			title = fmt.Sprintf("hidden_deleted_clip_%02d", i-110)
			desc = "Deleted video"
		}

		videos[i] = Video{
			AuthorID:    authorID,
			Title:       title,
			Description: desc,
			MediaURL:    fmt.Sprintf("/media/prod_%06d.mp4", i),
			Status:      status,
			CreatedAt:   createdAt,
		}
	}

	if err := db.CreateInBatches(&videos, videoBatchSize).Error; err != nil {
		t.Fatalf("failed to insert 20000 videos: %v", err)
	}
	meta.TotalVideos = totalVideoCount

	for i := 0; i < 14; i++ {
		meta.TargetVideoIDs = append(meta.TargetVideoIDs, videos[i].ID)
	}
	for i := 20; i < 40; i++ {
		meta.SameTimeVideoIDs = append(meta.SameTimeVideoIDs, videos[i].ID)
	}
	for i := 50; i < 130; i++ {
		meta.HiddenVideoIDs = append(meta.HiddenVideoIDs, videos[i].ID)
	}

	// 3. Seed 1,000 Notifications for Viewer
	const totalNotifCount = 1000
	const notifBatchSize = 250
	notifs := make([]Notification, totalNotifCount)

	for i := range notifs {
		actor := adminID
		if i%2 == 1 {
			actor = strangerID
		}
		var readAt *time.Time
		if i < 500 {
			// First 500 notifications are already read
			r := now.Add(-time.Duration(totalNotifCount-i) * time.Second)
			readAt = &r
		}
		stamp := now.Add(-time.Duration(totalNotifCount-i) * time.Second)
		if i >= 600 && i < 650 {
			// 50 notifications with exact same timestamp
			stamp = sameTime
		}

		notifs[i] = Notification{
			UserID:    viewerID,
			ActorID:   actor,
			Kind:      "like",
			EventKey:  fmt.Sprintf("notif_prod_%06d", i),
			Text:      fmt.Sprintf("Notification %06d for viewer", i),
			ReadAt:    readAt,
			CreatedAt: stamp,
		}
	}

	if err := db.CreateInBatches(&notifs, notifBatchSize).Error; err != nil {
		t.Fatalf("failed to insert 1000 notifications: %v", err)
	}
	meta.TotalNotifications = totalNotifCount

	return meta
}

func TestProductionScaleSearchAndNotificationQueries(t *testing.T) {
	s, h, db := testServer(t)
	meta := seedProductionSearchAndNotificationDataset(t, s, db)

	t.Run("UserSearch_HitsMissesUnicodeAndWildcards", func(t *testing.T) {
		// 1. Exact Hit: query 'user_hit' should match 3 users
		res := request(t, h, http.MethodGet, "/api/search?q=user_hit&type=users&limit=10", "", nil)
		if res.Code != http.StatusOK {
			t.Fatalf("search user_hit: status %d %s", res.Code, res.Body.String())
		}
		page := readSearchResponse(t, res.Body.Bytes())
		if len(page.Users) != 3 {
			t.Fatalf("expected 3 hit users, got %d", len(page.Users))
		}
		hitNames := []string{page.Users[0].Username, page.Users[1].Username, page.Users[2].Username}
		slices.Sort(hitNames)
		if !slices.Equal(hitNames, []string{"user_hit_alpha", "user_hit_beta", "user_hit_gamma"}) {
			t.Fatalf("unexpected hit users: %v", hitNames)
		}

		// 2. Miss: query 'nonexistent_user_99999' should return 0
		resMiss := request(t, h, http.MethodGet, "/api/search?q=nonexistent_user_99999&type=users", "", nil)
		if resMiss.Code != http.StatusOK {
			t.Fatalf("search miss: status %d", resMiss.Code)
		}
		pageMiss := readSearchResponse(t, resMiss.Body.Bytes())
		if len(pageMiss.Users) != 0 || pageMiss.HasMoreUsers {
			t.Fatalf("expected 0 users for miss, got %d, more=%v", len(pageMiss.Users), pageMiss.HasMoreUsers)
		}

		// 3. Literal Wildcard Escaping
		// Search "literal%" should match "literal%_user", NOT others
		resWildcard := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape("literal%")+"&type=users", "", nil)
		if resWildcard.Code != http.StatusOK {
			t.Fatalf("search literal%%: status %d", resWildcard.Code)
		}
		pageWildcard := readSearchResponse(t, resWildcard.Body.Bytes())
		if len(pageWildcard.Users) != 1 || pageWildcard.Users[0].Username != "literal%_user" {
			t.Fatalf("expected 1 user 'literal%%_user', got %+v", pageWildcard.Users)
		}

		// Search "literal_" should match "literal__user", NOT matching "literalX"
		resUnder := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape("literal_")+"&type=users", "", nil)
		if resUnder.Code != http.StatusOK {
			t.Fatalf("search literal_: status %d", resUnder.Code)
		}
		pageUnder := readSearchResponse(t, resUnder.Body.Bytes())
		if len(pageUnder.Users) != 1 || pageUnder.Users[0].Username != "literal__user" {
			t.Fatalf("expected 1 user 'literal__user', got %+v", pageUnder.Users)
		}

		// 4. Unicode Search
		resUni := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape("测试用户")+"&type=users", "", nil)
		if resUni.Code != http.StatusOK {
			t.Fatalf("search unicode: status %d", resUni.Code)
		}
		pageUni := readSearchResponse(t, resUni.Body.Bytes())
		if len(pageUni.Users) != 2 {
			t.Fatalf("expected 2 unicode users, got %d: %+v", len(pageUni.Users), pageUni.Users)
		}

		// 5. Emoji Search
		resEmoji := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape("🚀火箭")+"&type=users", "", nil)
		if resEmoji.Code != http.StatusOK {
			t.Fatalf("search emoji: status %d", resEmoji.Code)
		}
		pageEmoji := readSearchResponse(t, resEmoji.Body.Bytes())
		if len(pageEmoji.Users) != 1 || pageEmoji.Users[0].Username != "emoji_rocket" {
			t.Fatalf("expected 1 emoji user, got %+v", pageEmoji.Users)
		}
	})

	t.Run("UserSearch_KeysetPagination_IdenticalTimestampsAndDeepPages", func(t *testing.T) {
		// Search "sametime_u" matches 20 users sharing the exact same timestamp
		var seenIDs []uint
		cursor := uint(0)
		hasMore := true
		pageIndex := 0

		for hasMore {
			path := "/api/search?q=sametime_u&type=users&limit=6"
			if cursor > 0 {
				path = fmt.Sprintf("/api/search?q=sametime_u&type=users&limit=6&users_before_id=%d", cursor)
			}
			res := request(t, h, http.MethodGet, path, "", nil)
			if res.Code != http.StatusOK {
				t.Fatalf("page %d: status %d", pageIndex, res.Code)
			}
			page := readSearchResponse(t, res.Body.Bytes())
			for _, u := range page.Users {
				seenIDs = append(seenIDs, u.ID)
			}
			hasMore = page.HasMoreUsers
			cursor = page.NextUserCursor
			pageIndex++
			if pageIndex > 10 {
				t.Fatal("pagination loop exceeded safety limit")
			}
		}

		if len(seenIDs) != 20 {
			t.Fatalf("expected 20 users across pages, got %d", len(seenIDs))
		}
		// Verify zero duplicates
		unique := make(map[uint]bool)
		for _, id := range seenIDs {
			if unique[id] {
				t.Fatalf("duplicate user ID %d across pages", id)
			}
			unique[id] = true
		}
		// Verify strict monotonic descending
		for i := 1; i < len(seenIDs); i++ {
			if seenIDs[i] >= seenIDs[i-1] {
				t.Fatalf("user pagination not strictly descending: [%d]=%d >= [%d]=%d", i, seenIDs[i], i-1, seenIDs[i-1])
			}
		}
	})

	t.Run("VideoSearch_PublishedFilter_HiddenExclusionAndUnicode", func(t *testing.T) {
		// 1. Keyword search 'GoStream 架构剖析' should match 10 published videos
		res := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape("GoStream 架构剖析")+"&type=videos&limit=20", "", nil)
		if res.Code != http.StatusOK {
			t.Fatalf("search videos: status %d %s", res.Code, res.Body.String())
		}
		page := readSearchResponse(t, res.Body.Bytes())
		if len(page.Videos) != 10 {
			t.Fatalf("expected 10 published videos, got %d", len(page.Videos))
		}
		for _, v := range page.Videos {
			if v.Status != "published" {
				t.Fatalf("non-published video leaked into search: %+v", v)
			}
		}

		// 2. Hidden videos (draft, processing, failed, deleted) MUST NOT appear in search
		for _, hiddenKind := range []string{"hidden_draft", "hidden_proc", "hidden_failed", "hidden_deleted"} {
			resHidden := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape(hiddenKind)+"&type=videos", "", nil)
			if resHidden.Code != http.StatusOK {
				t.Fatalf("search %s: status %d", hiddenKind, resHidden.Code)
			}
			pageHidden := readSearchResponse(t, resHidden.Body.Bytes())
			if len(pageHidden.Videos) != 0 {
				t.Fatalf("hidden videos of kind %s leaked to public search! found: %+v", hiddenKind, pageHidden.Videos)
			}
		}

		// 3. Literal Wildcard on Video Titles
		resWild := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape("video_literal%")+"&type=videos", "", nil)
		if resWild.Code != http.StatusOK {
			t.Fatalf("search video wildcard: status %d", resWild.Code)
		}
		pageWild := readSearchResponse(t, resWild.Body.Bytes())
		if len(pageWild.Videos) != 1 || pageWild.Videos[0].Title != "video_literal%_target" {
			t.Fatalf("expected 1 video 'video_literal%%_target', got %+v", pageWild.Videos)
		}

		// 4. Emoji search on Video
		resEmoji := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape("🚀航天")+"&type=videos", "", nil)
		if resEmoji.Code != http.StatusOK {
			t.Fatalf("search video emoji: status %d", resEmoji.Code)
		}
		pageEmoji := readSearchResponse(t, resEmoji.Body.Bytes())
		if len(pageEmoji.Videos) != 1 || pageEmoji.Videos[0].Title != "🚀航天探索纪录片" {
			t.Fatalf("expected 1 video '🚀航天探索纪录片', got %+v", pageEmoji.Videos)
		}
	})

	t.Run("VideoSearch_DeepKeysetPagination_Scale20000", func(t *testing.T) {
		// Deep pagination across 19,800 matching videos
		// Page 1: limit 20
		t0 := time.Now()
		res1 := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape("prod video")+"&type=videos&limit=20", "", nil)
		shallowElapsed := time.Since(t0)
		if res1.Code != http.StatusOK {
			t.Fatalf("shallow page: %d", res1.Code)
		}
		page1 := readSearchResponse(t, res1.Body.Bytes())
		if len(page1.Videos) != 20 || !page1.HasMoreVideos || page1.NextVideoCursor == 0 {
			t.Fatalf("invalid shallow page: len=%d more=%v cursor=%d", len(page1.Videos), page1.HasMoreVideos, page1.NextVideoCursor)
		}

		// Deep page: query near middle of the dataset (e.g. cursor at ID = 10000)
		deepCursor := page1.Videos[0].ID / 2
		t1 := time.Now()
		pathDeep := fmt.Sprintf("/api/search?q=%s&type=videos&limit=20&videos_before_id=%d", url.QueryEscape("prod video"), deepCursor)
		resDeep := request(t, h, http.MethodGet, pathDeep, "", nil)
		deepElapsed := time.Since(t1)
		if resDeep.Code != http.StatusOK {
			t.Fatalf("deep page: %d", resDeep.Code)
		}
		pageDeep := readSearchResponse(t, resDeep.Body.Bytes())
		if len(pageDeep.Videos) != 20 || !pageDeep.HasMoreVideos {
			t.Fatalf("invalid deep page: len=%d more=%v", len(pageDeep.Videos), pageDeep.HasMoreVideos)
		}
		for _, v := range pageDeep.Videos {
			if v.ID >= deepCursor {
				t.Fatalf("deep page row %d >= cursor %d", v.ID, deepCursor)
			}
		}

		t.Logf("Video Search (20,000 videos): shallow_latency=%v deep_latency=%v", shallowElapsed, deepElapsed)
	})

	t.Run("Notifications_KeysetPagination_1000Items_NoGapsNoDups", func(t *testing.T) {
		var allIDs []uint
		cursor := uint(0)
		hasMore := true
		pageIndex := 0

		type notifPageResp struct {
			Items      []Notification `json:"items"`
			NextCursor uint           `json:"next_cursor"`
			HasMore    bool           `json:"has_more"`
		}

		for hasMore {
			path := "/api/notifications?limit=50"
			if cursor > 0 {
				path = fmt.Sprintf("/api/notifications?limit=50&before_id=%d", cursor)
			}
			res := request(t, h, http.MethodGet, path, meta.ViewerToken, nil)
			if res.Code != http.StatusOK {
				t.Fatalf("notif page %d: %d", pageIndex, res.Code)
			}
			var p notifPageResp
			if err := json.Unmarshal(res.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			for _, item := range p.Items {
				allIDs = append(allIDs, item.ID)
			}
			hasMore = p.HasMore
			cursor = p.NextCursor
			pageIndex++
			if pageIndex > 30 {
				t.Fatal("notification pagination safety limit exceeded")
			}
		}

		if len(allIDs) != meta.TotalNotifications {
			t.Fatalf("expected %d notifications paginated, got %d", meta.TotalNotifications, len(allIDs))
		}

		// Parity checks: no duplicates, strictly descending
		idMap := make(map[uint]bool)
		for _, id := range allIDs {
			if idMap[id] {
				t.Fatalf("duplicate notification ID %d", id)
			}
			idMap[id] = true
		}
		for i := 1; i < len(allIDs); i++ {
			if allIDs[i] >= allIDs[i-1] {
				t.Fatalf("notifications not strictly descending: [%d]=%d >= [%d]=%d", i, allIDs[i], i-1, allIDs[i-1])
			}
		}
	})

	t.Run("Notifications_BoundedMarkRead_Isolation", func(t *testing.T) {
		// Viewer starts with 500 unread notifications (IDs from 501 to 1000)
		resUnread := request(t, h, http.MethodGet, "/api/notifications/unread", meta.ViewerToken, nil)
		if resUnread.Code != http.StatusOK || !strings.Contains(resUnread.Body.String(), `"unread_count":500`) {
			t.Fatalf("initial unread count: %d %s", resUnread.Code, resUnread.Body.String())
		}

		// Query middle unread notification ID
		var middleNotif Notification
		if err := db.Where("user_id = ? AND read_at IS NULL", meta.ViewerID).Order("id ASC").Offset(249).First(&middleNotif).Error; err != nil {
			t.Fatal(err)
		}

		// Mark read up to middleNotif.ID
		markPath := fmt.Sprintf("/api/notifications/read?up_to_id=%d", middleNotif.ID)
		resMark := request(t, h, http.MethodPatch, markPath, meta.ViewerToken, nil)
		if resMark.Code != http.StatusOK || !strings.Contains(resMark.Body.String(), `"read_count":250`) {
			t.Fatalf("bounded mark read: %d %s", resMark.Code, resMark.Body.String())
		}

		// Idempotent repeat mark read up to same ID returns 0
		resRepeat := request(t, h, http.MethodPatch, markPath, meta.ViewerToken, nil)
		if resRepeat.Code != http.StatusOK || !strings.Contains(resRepeat.Body.String(), `"read_count":0`) {
			t.Fatalf("repeat bounded mark read: %d %s", resRepeat.Code, resRepeat.Body.String())
		}

		// Remaining unread count must be exactly 250
		resUnreadAfter := request(t, h, http.MethodGet, "/api/notifications/unread", meta.ViewerToken, nil)
		if resUnreadAfter.Code != http.StatusOK || !strings.Contains(resUnreadAfter.Body.String(), `"unread_count":250`) {
			t.Fatalf("unread after bounded mark: %s", resUnreadAfter.Body.String())
		}

		// Verify stranger account unread count is untouched (isolation)
		resStranger := request(t, h, http.MethodGet, "/api/notifications/unread", meta.StrangerToken, nil)
		if resStranger.Code != http.StatusOK || !strings.Contains(resStranger.Body.String(), `"unread_count":0`) {
			t.Fatalf("stranger unread affected: %s", resStranger.Body.String())
		}
	})

	t.Run("QueryPlans_ExplainAnalysis_AndScanCost", func(t *testing.T) {
		// 1. Notification keyset query
		notifQuery := "SELECT * FROM notifications WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 31"
		notifPlans := explainSQLite(t, db, notifQuery, meta.ViewerID, 1000)
		notifPlanStr := strings.Join(notifPlans, " | ")
		t.Logf("SQLite Plan (Notifications): %s", notifPlanStr)
		if !strings.Contains(notifPlanStr, "idx_notification_user_id") {
			t.Fatalf("notifications keyset did not use idx_notification_user_id: %s", notifPlanStr)
		}
		if strings.Contains(notifPlanStr, "USE TEMP B-TREE FOR ORDER BY") {
			t.Fatalf("notifications keyset used temp B-tree for ORDER BY: %s", notifPlanStr)
		}

		// 2. Notification unread count query
		unreadQuery := "SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL"
		unreadPlans := explainSQLite(t, db, unreadQuery, meta.ViewerID)
		unreadPlanStr := strings.Join(unreadPlans, " | ")
		t.Logf("SQLite Plan (Unread Count): %s", unreadPlanStr)
		if !strings.Contains(unreadPlanStr, "idx_notification_unread") {
			t.Fatalf("unread count did not use idx_notification_unread: %s", unreadPlanStr)
		}

		// 3. User search keyset query
		userQuery := "SELECT * FROM users WHERE (username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21"
		userPlans := explainSQLite(t, db, userQuery, "%test%", "%test%", 1000)
		userPlanStr := strings.Join(userPlans, " | ")
		t.Logf("SQLite Plan (User Search): %s", userPlanStr)
		if strings.Contains(userPlanStr, "USE TEMP B-TREE FOR ORDER BY") {
			t.Fatalf("user search used temp B-tree for ORDER BY: %s", userPlanStr)
		}

		// 4. Video search query
		videoQuery := "SELECT * FROM videos WHERE status = ? AND (title LIKE ? ESCAPE '!' OR description LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21"
		videoPlans := explainSQLite(t, db, videoQuery, "published", "%test%", "%test%", 1000)
		videoPlanStr := strings.Join(videoPlans, " | ")
		t.Logf("SQLite Plan (Video Search): %s", videoPlanStr)

		// 5. Measure query execution time over 50 iterations
		const iters = 50
		var durations []time.Duration
		for i := 0; i < iters; i++ {
			t0 := time.Now()
			var vRows []Video
			if err := db.Raw(videoQuery, "published", "%prod%", "%prod%", 15000).Scan(&vRows).Error; err != nil {
				t.Fatal(err)
			}
			durations = append(durations, time.Since(t0))
		}
		slices.Sort(durations)
		p50 := durations[len(durations)/2]
		p95 := durations[len(durations)*95/100]
		maxDur := durations[len(durations)-1]

		t.Logf("Production Search Latency (20,000 videos, 50 queries): P50=%v P95=%v Max=%v", p50, p95, maxDur)
	})
}

func TestMySQLProductionSearchAndNotificationQueries(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to run MySQL production query tests")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()

	s := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir())
	meta := seedProductionSearchAndNotificationDataset(t, s, tx)

	// Verify MySQL EXPLAIN for notifications keyset pagination
	notifQuery := "SELECT * FROM notifications WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 31"
	rows := explainMySQL(t, tx, notifQuery, meta.ViewerID, 1000)
	if len(rows) == 0 {
		t.Fatal("empty explain output for notifications keyset")
	}
	r := rows[0]
	t.Logf("MySQL Plan (Notifications): table=%s type=%s key=%s rows=%d extra=%s",
		r.Table, r.Type, r.Key.String, r.Rows, r.Extra)

	if strings.Contains(r.Extra, "Using filesort") {
		t.Fatalf("notifications keyset requires filesort: %s", r.Extra)
	}

	// Verify unread count uses covering index
	unreadQuery := "SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL"
	unreadRows := explainMySQL(t, tx, unreadQuery, meta.ViewerID)
	if len(unreadRows) > 0 {
		ur := unreadRows[0]
		t.Logf("MySQL Plan (Unread Count): table=%s type=%s key=%s rows=%d extra=%s",
			ur.Table, ur.Type, ur.Key.String, ur.Rows, ur.Extra)
	}

	// Verify EXPLAIN ANALYZE if supported (MySQL 8.0+)
	var analyzeResult []struct {
		Explain string `gorm:"column:EXPLAIN"`
	}
	if err := tx.Raw("EXPLAIN ANALYZE " + notifQuery, meta.ViewerID, 1000).Scan(&analyzeResult).Error; err == nil && len(analyzeResult) > 0 {
		t.Logf("MySQL EXPLAIN ANALYZE:\n%s", analyzeResult[0].Explain)
	}
}
