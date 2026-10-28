package platform

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestAccountStorageErrorResponses(t *testing.T) {
	_, router, db := testServer(t)
	userID, token := registerTestUser(t, router, "storageaccount")

	registerBody := map[string]string{"username": "storageaccount", "password": "correct-horse-123"}
	if res := request(t, router, http.MethodPost, "/api/users", "", registerBody); res.Code != http.StatusConflict {
		t.Fatalf("duplicate username: %d %s", res.Code, res.Body.String())
	}
	loginBody := map[string]string{"username": "storageaccount", "password": "correct-horse-123"}
	if res := request(t, router, http.MethodPost, "/api/sessions", "", loginBody); res.Code != http.StatusOK {
		t.Fatalf("valid login: %d %s", res.Code, res.Body.String())
	}
	loginBody["password"] = "incorrect-password"
	if res := request(t, router, http.MethodPost, "/api/sessions", "", loginBody); res.Code != http.StatusUnauthorized {
		t.Fatalf("incorrect password: %d %s", res.Code, res.Body.String())
	}
	loginBody["username"] = "missingaccount"
	if res := request(t, router, http.MethodPost, "/api/sessions", "", loginBody); res.Code != http.StatusUnauthorized {
		t.Fatalf("missing account: %d %s", res.Code, res.Body.String())
	}
	if err := db.Model(&User{}).Where("username = ?", "storageaccount").Update("password_hash", "not-a-bcrypt-hash").Error; err != nil {
		t.Fatal(err)
	}
	loginBody["username"] = "storageaccount"
	if res := request(t, router, http.MethodPost, "/api/sessions", "", loginBody); res.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt password hash: %d %s", res.Code, res.Body.String())
	}
	for _, path := range []string{"/api/users/999999", "/api/users/999999/presence"} {
		if res := request(t, router, http.MethodGet, path, "", nil); res.Code != http.StatusNotFound {
			t.Fatalf("missing user at %s: %d %s", path, res.Code, res.Body.String())
		}
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	registerBody["username"] = "newaccount"
	if res := request(t, router, http.MethodPost, "/api/users", "", registerBody); res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), "sql: database is closed") {
		t.Fatalf("register database failure: %d %s", res.Code, res.Body.String())
	}
	if res := request(t, router, http.MethodPost, "/api/sessions", "", loginBody); res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), "sql: database is closed") {
		t.Fatalf("login database failure: %d %s", res.Code, res.Body.String())
	}
	profilePath := "/api/users/" + strconv.FormatUint(uint64(userID), 10)
	for _, route := range []struct {
		path  string
		token string
	}{
		{path: "/api/users/me", token: token},
		{path: profilePath},
		{path: profilePath + "/presence"},
	} {
		res := request(t, router, http.MethodGet, route.path, route.token, nil)
		if res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), "sql: database is closed") {
			t.Fatalf("%s database failure: %d %s", route.path, res.Code, res.Body.String())
		}
	}
}

func TestMySQLAccountStorageErrorResponses(t *testing.T) {
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
	username := fmt.Sprintf("acct%x", time.Now().UnixNano()&0xffffffff)
	_, _ = registerTestUser(t, router, username)
	registerBody := map[string]string{"username": username, "password": "correct-horse-123"}
	if res := request(t, router, http.MethodPost, "/api/users", "", registerBody); res.Code != http.StatusConflict {
		t.Fatalf("MySQL duplicate username: %d %s", res.Code, res.Body.String())
	}
	loginBody := map[string]string{"username": username, "password": "correct-horse-123"}
	if res := request(t, router, http.MethodPost, "/api/sessions", "", loginBody); res.Code != http.StatusOK {
		t.Fatalf("MySQL valid login: %d %s", res.Code, res.Body.String())
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	registerBody["username"] = "new" + username
	if res := request(t, router, http.MethodPost, "/api/users", "", registerBody); res.Code != http.StatusInternalServerError {
		t.Fatalf("MySQL register storage failure: %d %s", res.Code, res.Body.String())
	}
	if res := request(t, router, http.MethodPost, "/api/sessions", "", loginBody); res.Code != http.StatusInternalServerError {
		t.Fatalf("MySQL login storage failure: %d %s", res.Code, res.Body.String())
	}
}
