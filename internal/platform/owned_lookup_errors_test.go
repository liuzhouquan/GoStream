package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestOwnedLookupStorageErrors(t *testing.T) {
	_, router, db := testServer(t)
	_, ownerToken := registerTestUser(t, router, "lookupowner")
	_, otherToken := registerTestUser(t, router, "lookupother")

	groupResponse := request(t, router, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "lookup group"})
	if groupResponse.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", groupResponse.Code, groupResponse.Body.String())
	}
	var group ChatGroup
	if err := json.Unmarshal(groupResponse.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	groupPath := fmt.Sprintf("/api/groups/%d/messages", group.ID)
	uploadResponse := request(t, router, http.MethodPost, "/api/uploads/sessions", ownerToken, map[string]any{"filename": "clip.mp4", "size": 1})
	if uploadResponse.Code != http.StatusCreated {
		t.Fatalf("create upload session: %d %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	var upload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(uploadResponse.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	uploadPath := "/api/uploads/sessions/" + upload.ID

	for _, path := range []string{groupPath, uploadPath} {
		if res := request(t, router, http.MethodGet, path, ownerToken, nil); res.Code != http.StatusOK {
			t.Fatalf("owner read %s: %d %s", path, res.Code, res.Body.String())
		}
		if res := request(t, router, http.MethodGet, path, otherToken, nil); res.Code != http.StatusNotFound {
			t.Fatalf("other user read %s: %d %s", path, res.Code, res.Body.String())
		}
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{groupPath, uploadPath} {
		if res := request(t, router, http.MethodGet, path, ownerToken, nil); res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), "database is closed") {
			t.Fatalf("storage error on %s: %d %s", path, res.Code, res.Body.String())
		}
	}
}

func TestGroupMembershipLookupStorageError(t *testing.T) {
	_, router, db := testServer(t)
	_, token := registerTestUser(t, router, "membershiplookup")
	res := request(t, router, http.MethodPost, "/api/groups", token, map[string]string{"name": "lookup group"})
	if res.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", res.Code, res.Body.String())
	}
	var group ChatGroup
	if err := json.Unmarshal(res.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&GroupMember{}); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/groups/%d/messages", group.ID)
	if res := request(t, router, http.MethodGet, path, token, nil); res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), "no such table") {
		t.Fatalf("membership query error: %d %s", res.Code, res.Body.String())
	}
}

func TestMySQLOwnedLookupStorageErrors(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSTREAM_TEST_MYSQL_DSN to a migrated disposable MySQL database")
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
	router := NewServer(tx, "test-signing-secret-at-least-32-bytes", t.TempDir()).Router()
	username := fmt.Sprintf("lookup%x", time.Now().UnixNano()&0xffffffff)
	_, ownerToken := registerTestUser(t, router, username)
	_, otherToken := registerTestUser(t, router, "other"+username)

	groupResponse := request(t, router, http.MethodPost, "/api/groups", ownerToken, map[string]string{"name": "lookup group"})
	if groupResponse.Code != http.StatusCreated {
		t.Fatalf("MySQL create group: %d %s", groupResponse.Code, groupResponse.Body.String())
	}
	var group ChatGroup
	if err := json.Unmarshal(groupResponse.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	uploadResponse := request(t, router, http.MethodPost, "/api/uploads/sessions", ownerToken, map[string]any{"filename": "clip.mp4", "size": 1})
	if uploadResponse.Code != http.StatusCreated {
		t.Fatalf("MySQL create upload: %d %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	var upload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(uploadResponse.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fmt.Sprintf("/api/groups/%d/messages", group.ID), "/api/uploads/sessions/" + upload.ID} {
		if res := request(t, router, http.MethodGet, path, ownerToken, nil); res.Code != http.StatusOK {
			t.Fatalf("MySQL owner read %s: %d %s", path, res.Code, res.Body.String())
		}
		if res := request(t, router, http.MethodGet, path, otherToken, nil); res.Code != http.StatusNotFound {
			t.Fatalf("MySQL other user read %s: %d %s", path, res.Code, res.Body.String())
		}
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fmt.Sprintf("/api/groups/%d/messages", group.ID), "/api/uploads/sessions/" + upload.ID} {
		if res := request(t, router, http.MethodGet, path, ownerToken, nil); res.Code != http.StatusInternalServerError {
			t.Fatalf("MySQL storage error on %s: %d %s", path, res.Code, res.Body.String())
		}
	}
}
