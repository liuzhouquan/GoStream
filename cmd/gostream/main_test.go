package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAPIServerBodyReadDeadline(t *testing.T) {
	readResult := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		readResult <- err
		if err != nil {
			http.Error(w, "body read failed", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	config := newAPIServer("127.0.0.1:0", handler)
	if config.ReadTimeout != 10*time.Minute {
		t.Fatalf("API body read timeout = %s, want 10m", config.ReadTimeout)
	}
	web := httptest.NewUnstartedServer(handler)
	web.Config.ReadTimeout = 150 * time.Millisecond
	web.Config.ReadHeaderTimeout = config.ReadHeaderTimeout
	web.Config.IdleTimeout = config.IdleTimeout
	web.Config.MaxHeaderBytes = config.MaxHeaderBytes
	web.Start()
	defer web.Close()
	conn, err := net.Dial("tcp", web.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "POST /upload HTTP/1.1\r\nHost: test\r\nContent-Length: 2\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readResult:
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("slow body read error = %v, want timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow body kept handler blocked past read deadline")
	}
	response, err := http.Post(web.URL+"/upload", "application/octet-stream", strings.NewReader("ok"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("later valid request status = %d", response.StatusCode)
	}
}

func TestJWTSecretStartupBoundary(t *testing.T) {
	if os.Getenv("GOSTREAM_STARTUP_PROBE") == "1" {
		main()
		return
	}
	tests := []struct {
		name      string
		addr      string
		secret    string
		wantError string
	}{
		{name: "default local start", addr: "", secret: "", wantError: "unsupported DB_DRIVER"},
		{name: "loopback development start", addr: "127.0.0.1:8080", secret: "", wantError: "unsupported DB_DRIVER"},
		{name: "wildcard development secret rejected", addr: ":8080", secret: "", wantError: "JWT_SECRET"},
		{name: "published compose secret rejected", addr: ":8080", secret: "local-development-secret-change-before-deploy", wantError: "JWT_SECRET"},
		{name: "explicit secret on wildcard", addr: ":8080", secret: "independent-startup-test-secret-123456789", wantError: "unsupported DB_DRIVER"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestJWTSecretStartupBoundary$")
			cmd.Env = append(os.Environ(),
				"GOSTREAM_STARTUP_PROBE=1", "ADDR="+test.addr, "JWT_SECRET="+test.secret,
				"DB_DRIVER=invalid-startup-probe", "DB_DSN=ignored", "MEDIA_DIR="+t.TempDir(),
			)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected startup to stop at the configured probe, output=%s", output)
			}
			if !strings.Contains(string(output), test.wantError) {
				t.Fatalf("startup output %q does not contain %q", output, test.wantError)
			}
		})
	}
}

func TestSQLiteDSNEnablesWALBusyTimeoutAndForeignKeys(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqliteDSN(filepath.Join(t.TempDir(), "profile.db"))), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var journal string
	if err := raw.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || !strings.EqualFold(journal, "wal") {
		t.Fatalf("journal_mode=%q err=%v", journal, err)
	}
	var timeout int
	if err := raw.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("busy_timeout=%d err=%v", timeout, err)
	}
	var foreignKeys int
	if err := raw.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d err=%v", foreignKeys, err)
	}
}
