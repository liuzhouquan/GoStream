package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type searchResponse struct {
	Users           []User             `json:"users"`
	Videos          []feedItemResponse `json:"videos"`
	NextUserCursor  uint               `json:"next_user_cursor"`
	NextVideoCursor uint               `json:"next_video_cursor"`
	HasMoreUsers    bool               `json:"has_more_users"`
	HasMoreVideos   bool               `json:"has_more_videos"`
}

func readSearchResponse(t *testing.T, body []byte) searchResponse {
	t.Helper()
	var page searchResponse
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestSearchKeysetPagesAndIndependentTypes(t *testing.T) {
	_, h, db := testServer(t)
	ownerID, _ := registerTestUser(t, h, "search_page_owner")
	for i := 0; i < 25; i++ {
		user := User{Username: fmt.Sprintf("finder_%02d", i), Nickname: "Finder", PasswordHash: "unused"}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		video := Video{AuthorID: ownerID, Title: fmt.Sprintf("Finder clip %02d", i), MediaURL: "/media/unused.mp4", Status: "published"}
		if err := db.Create(&video).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&Video{AuthorID: ownerID, Title: "Finder hidden", MediaURL: "/media/unused.mp4", Status: "processing"}).Error; err != nil {
		t.Fatal(err)
	}
	first := request(t, h, http.MethodGet, "/api/search?q=finder&limit=10", "", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first page: %d %s", first.Code, first.Body.String())
	}
	page := readSearchResponse(t, first.Body.Bytes())
	if len(page.Users) != 10 || len(page.Videos) != 10 || !page.HasMoreUsers || !page.HasMoreVideos {
		t.Fatalf("first page size: %+v", page)
	}
	if page.NextUserCursor != page.Users[9].ID || page.NextVideoCursor != page.Videos[9].ID {
		t.Fatalf("cursor does not identify final row: %+v", page)
	}
	firstUser, firstVideo := page.NextUserCursor, page.NextVideoCursor
	if err := db.Create(&User{Username: "finder_new", Nickname: "Finder", PasswordHash: "unused"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Video{AuthorID: ownerID, Title: "Finder newest", MediaURL: "/media/unused.mp4", Status: "published"}).Error; err != nil {
		t.Fatal(err)
	}
	seenUsers, seenVideos := make(map[uint]bool), make(map[uint]bool)
	for _, user := range page.Users {
		seenUsers[user.ID] = true
	}
	for _, video := range page.Videos {
		seenVideos[video.ID] = true
	}
	for page.HasMoreUsers || page.HasMoreVideos {
		path := fmt.Sprintf("/api/search?q=finder&limit=10&users_before_id=%d&videos_before_id=%d", page.NextUserCursor, page.NextVideoCursor)
		response := request(t, h, http.MethodGet, path, "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("next page: %d %s", response.Code, response.Body.String())
		}
		page = readSearchResponse(t, response.Body.Bytes())
		for _, user := range page.Users {
			if seenUsers[user.ID] || user.ID >= firstUser {
				t.Fatalf("duplicate/new user on later page: %d", user.ID)
			}
			seenUsers[user.ID] = true
		}
		for _, video := range page.Videos {
			if seenVideos[video.ID] || video.ID >= firstVideo {
				t.Fatalf("duplicate/new video on later page: %d", video.ID)
			}
			seenVideos[video.ID] = true
		}
	}
	if len(seenUsers) != 25 || len(seenVideos) != 25 {
		t.Fatalf("paged totals: users=%d videos=%d", len(seenUsers), len(seenVideos))
	}
	for _, kind := range []string{"users", "videos"} {
		response := request(t, h, http.MethodGet, "/api/search?q=finder&type="+kind+"&limit=3", "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("type %s: %d", kind, response.Code)
		}
		result := readSearchResponse(t, response.Body.Bytes())
		if kind == "users" && (len(result.Users) != 3 || len(result.Videos) != 0 || !result.HasMoreUsers) {
			t.Fatalf("users-only page: %+v", result)
		}
		if kind == "videos" && (len(result.Users) != 0 || len(result.Videos) != 3 || !result.HasMoreVideos) {
			t.Fatalf("videos-only page: %+v", result)
		}
	}
}

func TestSearchLiteralWildcardsUnicodeAndInteractionState(t *testing.T) {
	_, h, db := testServer(t)
	ownerID, token := registerTestUser(t, h, "srchowner")
	for _, name := range []string{"literal_hit", "literalxhit"} {
		if err := db.Create(&User{Username: name, Nickname: name, PasswordHash: "unused"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	videos := []Video{
		{AuthorID: ownerID, Title: "literal% clip", MediaURL: "/media/unused.mp4", Status: "published"},
		{AuthorID: ownerID, Title: "literalX clip", MediaURL: "/media/unused.mp4", Status: "published"},
		{AuthorID: ownerID, Title: "亮点视频!", MediaURL: "/media/unused.mp4", Status: "published"},
	}
	for i := range videos {
		if err := db.Create(&videos[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&VideoAction{UserID: ownerID, VideoID: videos[0].ID, Kind: "like"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query  string
		users  int
		videos int
	}{
		{"literal_", 1, 0},
		{"literal%", 0, 1},
		{"亮点", 0, 1},
		{"视频!", 0, 1},
	} {
		response := request(t, h, http.MethodGet, "/api/search?q="+url.QueryEscape(test.query), token, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("search %q: %d %s", test.query, response.Code, response.Body.String())
		}
		result := readSearchResponse(t, response.Body.Bytes())
		if len(result.Users) != test.users || len(result.Videos) != test.videos {
			t.Fatalf("literal %q: users=%d videos=%d", test.query, len(result.Users), len(result.Videos))
		}
		if test.query == "literal%" && (!result.Videos[0].Liked || result.Videos[0].Favorited || result.Videos[0].Author.ID != ownerID) {
			t.Fatalf("search card state: %+v", result.Videos[0])
		}
	}
}

func TestSearchRejectsInvalidParameters(t *testing.T) {
	_, h, _ := testServer(t)
	for _, path := range []string{
		"/api/search?q=a", "/api/search?q=" + strings.Repeat("界", 81),
		"/api/search?q=test&type=posts", "/api/search?q=test&limit=0", "/api/search?q=test&limit=51",
		"/api/search?q=test&users_before_id=abc", "/api/search?q=test&videos_before_id=0",
	} {
		response := request(t, h, http.MethodGet, path, "", nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d %s", path, response.Code, response.Body.String())
		}
	}
}
