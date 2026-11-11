package im_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func sanitizedAPILog(data []byte, env []string) string {
	if len(data) > 65536 {
		data = data[len(data)-65536:]
	}
	text := string(data)
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		for _, field := range []string{"SECRET", "PASSWORD", "TOKEN", "DSN", "ACCESS_KEY"} {
			if value != "" && strings.Contains(strings.ToUpper(key), field) {
				text = strings.ReplaceAll(text, value, "<REDACTED>")
			}
		}
	}
	text = regexp.MustCompile(`(?i)Bearer\s+[^\s"']+`).ReplaceAllString(text, "Bearer <REDACTED>")
	text = regexp.MustCompile(`(?i)("(?:access_token|password|secret)"\s*:\s*)"[^"]*"`).ReplaceAllString(text, `${1}"<REDACTED>"`)
	text = regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(text, "")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "[rows:") { // SQL text can contain fixture identities and chat bodies.
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func (p *process) reportFailedLog(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, "api.log"))
	if err != nil {
		t.Logf("API %s failure log unavailable (%T)", p.addr, err)
		return
	}
	t.Logf("API %s sanitized failure log (last 64KiB; SQL rows omitted):\n%s", p.addr, sanitizedAPILog(data, p.env))
}

func TestAPIFailureLogRedaction(t *testing.T) {
	data := []byte("database is locked\nsecret-sentinel\nu:p@tcp(host)/db\nBearer jwt-sentinel\n[rows:1] INSERT INTO messages VALUES ('private-body')\n{\"access_token\":\"token-sentinel\"}")
	got := sanitizedAPILog(data, []string{"JWT_SECRET=secret-sentinel", "DB_DSN=u:p@tcp(host)/db"})
	if !strings.Contains(got, "database is locked") {
		t.Fatal("diagnostic lost SQL error")
	}
	for _, secret := range []string{"secret-sentinel", "u:p@tcp", "jwt-sentinel", "token-sentinel", "private-body"} {
		if strings.Contains(got, secret) {
			t.Fatalf("diagnostic exposed %s", secret)
		}
	}
}
