package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type commentPageResponse struct {
	Items      []Comment `json:"items"`
	NextCursor string    `json:"next_cursor"`
	HasMore    bool      `json:"has_more"`
}

func TestCommentThreadsPaginationAndNotifications(t *testing.T) {
	_, h, db := testServer(t)
	authorID, authorToken := registerTestUser(t, h, "threadauthor")
	parentOwnerID, parentToken := registerTestUser(t, h, "threadparent")
	_, replierToken := registerTestUser(t, h, "threadreplier")
	video := Video{AuthorID: authorID, Title: "clip", MediaURL: "/media/clip.mp4", Status: "published"}
	other := Video{AuthorID: authorID, Title: "other", MediaURL: "/media/other.mp4", Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	create := func(videoID uint, token string, body string, parentID uint) Comment {
		t.Helper()
		payload := map[string]any{"body": body}
		if parentID != 0 {
			payload["parent_id"] = parentID
		}
		res := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", videoID), token, payload)
		if res.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", body, res.Code, res.Body.String())
		}
		var comment Comment
		if err := json.Unmarshal(res.Body.Bytes(), &comment); err != nil {
			t.Fatal(err)
		}
		return comment
	}
	root1 := create(video.ID, parentToken, "first root", 0)
	root2 := create(video.ID, parentToken, "second root", 0)
	reply1 := create(video.ID, replierToken, "first reply", root1.ID)
	reply2 := create(video.ID, replierToken, "second reply", root1.ID)
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := db.Model(&Comment{}).Where("video_id = ?", video.ID).UpdateColumn("created_at", stamp).Error; err != nil {
		t.Fatal(err)
	}
	readPage := func(path string) commentPageResponse {
		t.Helper()
		res := request(t, h, http.MethodGet, path, "", nil)
		if res.Code != http.StatusOK {
			t.Fatalf("list %s: %d %s", path, res.Code, res.Body.String())
		}
		var page commentPageResponse
		if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	base := fmt.Sprintf("/api/videos/%d/comments", video.ID)
	first := readPage(base + "?limit=1")
	if len(first.Items) != 1 || first.Items[0].ID != root2.ID || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first root page: %+v", first)
	}
	second := readPage(base + "?limit=1&cursor=" + url.QueryEscape(first.NextCursor))
	if len(second.Items) != 1 || second.Items[0].ID != root1.ID || second.HasMore || second.Items[0].ReplyCount != 2 {
		t.Fatalf("second root page: %+v", second)
	}
	replyBase := fmt.Sprintf("%s/%d/replies", base, root1.ID)
	replies1 := readPage(replyBase + "?limit=1")
	if len(replies1.Items) != 1 || replies1.Items[0].ID != reply2.ID || !replies1.HasMore {
		t.Fatalf("first reply page: %+v", replies1)
	}
	replies2 := readPage(replyBase + "?limit=1&cursor=" + url.QueryEscape(replies1.NextCursor))
	if len(replies2.Items) != 1 || replies2.Items[0].ID != reply1.ID || replies2.HasMore {
		t.Fatalf("second reply page: %+v", replies2)
	}
	for _, path := range []string{replyBase + "?cursor=" + url.QueryEscape(first.NextCursor), fmt.Sprintf("/api/videos/%d/comments?cursor=%s", other.ID, url.QueryEscape(first.NextCursor))} {
		if res := request(t, h, http.MethodGet, path, "", nil); res.Code != http.StatusBadRequest {
			t.Fatalf("cursor reused for another thread %s: %d", path, res.Code)
		}
	}
	for _, path := range []string{base + "?limit=0", base + "?cursor=garbage", replyBase + "?limit=51"} {
		if res := request(t, h, http.MethodGet, path, "", nil); res.Code != http.StatusBadRequest {
			t.Fatalf("expected bad page request %s: %d", path, res.Code)
		}
	}
	if res := request(t, h, http.MethodGet, fmt.Sprintf("/api/videos/%d/comments/%d/replies", other.ID, root1.ID), "", nil); res.Code != http.StatusNotFound {
		t.Fatalf("cross-video reply list: %d", res.Code)
	}
	if res := request(t, h, http.MethodPost, base, replierToken, map[string]any{"body": "nested", "parent_id": reply1.ID}); res.Code != http.StatusBadRequest {
		t.Fatalf("nested reply: %d", res.Code)
	}
	if res := request(t, h, http.MethodPost, fmt.Sprintf("/api/videos/%d/comments", other.ID), replierToken, map[string]any{"body": "cross", "parent_id": root1.ID}); res.Code != http.StatusNotFound {
		t.Fatalf("cross-video reply: %d", res.Code)
	}
	var count int64
	if err := db.Model(&Notification{}).Where("user_id = ? AND kind = ?", parentOwnerID, "reply").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("reply notifications: %d, want 2", count)
	}
	if err := db.Model(&Notification{}).Where("user_id = ? AND kind = ?", authorID, "comment").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("author comment notifications: %d, want 4", count)
	}
	self := create(video.ID, parentToken, "self reply", root1.ID)
	if self.ParentID == nil || *self.ParentID != root1.ID {
		t.Fatalf("self reply parent: %+v", self.ParentID)
	}
	if err := db.Model(&Notification{}).Where("user_id = ? AND kind = ?", parentOwnerID, "reply").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("self reply added notification: %d", count)
	}
	var stored Video
	if err := db.First(&stored, video.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CommentCount != 5 {
		t.Fatalf("comment count: %d, want 5", stored.CommentCount)
	}
	authorRoot := create(video.ID, authorToken, "author root", 0)
	create(video.ID, replierToken, "reply to author", authorRoot.ID)
	if err := db.Model(&Notification{}).Where("user_id = ? AND kind = ?", authorID, "reply").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("author received duplicate reply notice: %d", count)
	}
	if err := db.Model(&Notification{}).Where("user_id = ? AND kind = ?", authorID, "comment").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("author comment notices: %d, want 6", count)
	}
}
