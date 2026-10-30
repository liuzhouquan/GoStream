package im_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type account struct {
	User  struct{ ID uint } `json:"user"`
	Token string            `json:"access_token"`
}

type message struct {
	ID          uint   `json:"id"`
	ClientID    string `json:"client_id"`
	SenderID    uint   `json:"sender_id"`
	RecipientID uint   `json:"recipient_id"`
	GroupID     uint   `json:"group_id"`
	Body        string `json:"body"`
}

type event struct {
	Type      string  `json:"type"`
	ClientID  string  `json:"client_id"`
	MessageID uint    `json:"message_id"`
	Message   message `json:"message"`
}

func (e *event) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type      string          `json:"type"`
		ClientID  string          `json:"client_id"`
		MessageID uint            `json:"message_id"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	e.Type, e.ClientID, e.MessageID = wire.Type, wire.ClientID, wire.MessageID
	if wire.Type == "message" || wire.Type == "group_message" {
		return json.Unmarshal(wire.Message, &e.Message)
	}
	return nil
}

type page struct {
	Items []message `json:"items"`
	Next  uint      `json:"next_cursor"`
	More  bool      `json:"has_more"`
}

func eventually(t *testing.T, label string, limit time.Duration, check func() bool) {
	t.Helper()
	until := time.Now().Add(limit)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

func randomID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	value := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", value[:8], value[8:12], value[12:16], value[16:20], value[20:])
}

// process launches the production executable. It never calls in-process routes
// or workers; SIGKILL therefore exercises a real connection/process loss.
type process struct {
	base, addr, binary, dir string
	env                     []string
	cmd                     *exec.Cmd
	done                    chan error
	log                     *os.File
}

func newProcess(t *testing.T, binary, dsn, redisAddr, secret string) *process {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	p := &process{addr: addr, base: "http://" + addr, binary: binary, dir: t.TempDir()}
	values := map[string]string{
		"ADDR": addr, "DB_DRIVER": "mysql", "DB_DSN": dsn, "JWT_SECRET": secret,
		"REDIS_ADDR": redisAddr, "RABBITMQ_URL": "", "MEDIA_STORE": "local",
		"MEDIA_DIR": filepath.Join(p.dir, "media"), "FFMPEG_BIN": "", "PPROF_ADDR": "",
		"FEED_FANOUT_MAX_FOLLOWERS": "0", "PRESENCE_LEASE": "", "PRESENCE_RENEW": "",
	}
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := values[key]; !replaced {
			p.env = append(p.env, item)
		}
	}
	for k, v := range values {
		p.env = append(p.env, k+"="+v)
	}
	t.Cleanup(func() {
		if p.cmd != nil {
			p.stop(t, false)
		}
	})
	return p
}

func (p *process) start(t *testing.T) {
	t.Helper()
	if p.cmd != nil {
		t.Fatal("process already started")
	}
	var err error
	p.log, err = os.OpenFile(filepath.Join(p.dir, "api.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	p.cmd = exec.Command(p.binary)
	p.cmd.Env, p.cmd.Dir, p.cmd.Stdout, p.cmd.Stderr = p.env, p.dir, p.log, p.log
	if err := p.cmd.Start(); err != nil {
		_ = p.log.Close()
		p.cmd = nil
		t.Fatal(err)
	}
	p.done = make(chan error, 1)
	cmd := p.cmd
	go func() { p.done <- cmd.Wait() }()
	t.Logf("started production API pid=%d", cmd.Process.Pid)
	eventually(t, "API readiness", 30*time.Second, func() bool {
		select {
		case err := <-p.done:
			p.cmd = nil
			_ = p.log.Close()
			t.Fatalf("API exited before ready: %v (log: %s)", err, p.log.Name())
		default:
		}
		client := &http.Client{Timeout: time.Second}
		response, err := client.Get(p.base + "/ready")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
}

func (p *process) stop(t *testing.T, hard bool) {
	t.Helper()
	if hard {
		if err := p.cmd.Process.Kill(); err != nil {
			t.Errorf("could not SIGKILL API: %v", err)
		}
	} else {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case err := <-p.done:
		if hard {
			var exited *exec.ExitError
			if !errors.As(err, &exited) {
				t.Errorf("expected SIGKILL exit, got %v", err)
			} else if state, ok := exited.Sys().(syscall.WaitStatus); !ok || state.Signal() != syscall.SIGKILL {
				t.Errorf("expected SIGKILL, got %v", err)
			} else {
				t.Logf("confirmed SIGKILL pid=%d", p.cmd.Process.Pid)
			}
		} else if err != nil {
			t.Errorf("API graceful exit: %v", err)
		}
	case <-time.After(12 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Error("API did not exit within shutdown limit")
	}
	p.cmd = nil
	_ = p.log.Close()
	data, _ := os.ReadFile(p.log.Name())
	if bytes.Contains(data, []byte("WARNING: DATA RACE")) {
		t.Error("race detector reported a race in API child process")
	}
}

func call(t *testing.T, base, method, path string, who account, device string, body any, want int, out any) {
	t.Helper()
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, base+path, data)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if who.Token != "" {
		request.Header.Set("Authorization", "Bearer "+who.Token)
	}
	if device != "" {
		request.Header.Set("X-GoStream-Device-ID", device)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d", method, path, response.StatusCode, want)
	}
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

func register(t *testing.T, base, name string) account {
	t.Helper()
	var result account
	call(t, base, "POST", "/api/users", account{}, "", map[string]string{"username": name, "nickname": name, "password": "isolated-im-fixture-password"}, 201, &result)
	if result.User.ID == 0 || result.Token == "" {
		t.Fatal("registration returned no identity")
	}
	return result
}

type socket struct {
	conn *websocket.Conn
	mu   sync.Mutex
	read chan event
}

func connect(t *testing.T, base string, who account, device string) *socket {
	t.Helper()
	protocols := []string{"gostream", "gostream.jwt." + who.Token, "gostream.device." + device}
	conn, _, err := (&websocket.Dialer{Subprotocols: protocols, HandshakeTimeout: 5 * time.Second}).Dial("ws"+strings.TrimPrefix(base, "http")+"/api/ws", nil)
	if err != nil {
		t.Fatal("WebSocket handshake failed") // Do not print token-bearing request headers.
	}
	s := &socket{conn: conn, read: make(chan event, 2048)}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		defer close(s.read)
		for {
			var e event
			if err := conn.ReadJSON(&e); err != nil {
				return
			}
			select {
			case s.read <- e:
			default:
				_ = conn.Close()
				return
			}
		}
	}()
	s.next(t, "ready", nil)
	return s
}

func (s *socket) write(t *testing.T, value any) {
	t.Helper()
	if err := s.writeJSON(value); err != nil {
		t.Fatal(err)
	}
}

func (s *socket) writeJSON(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteJSON(value)
}

func concurrentWrites(t *testing.T, a, b *socket, value any) {
	t.Helper()
	start, results := make(chan struct{}), make(chan error, 2)
	for _, s := range []*socket{a, b} {
		go func() { <-start; results <- s.writeJSON(value) }()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func (s *socket) next(t *testing.T, kind string, match func(event) bool) event {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-s.read:
			if !ok {
				t.Fatalf("socket closed while waiting for %s", kind)
			}
			if e.Type == "error" && kind != "error" {
				t.Fatalf("server returned error while waiting for %s", kind)
			}
			if e.Type == kind && (match == nil || match(e)) {
				return e
			}
		case <-timer.C:
			t.Fatalf("socket timed out waiting for %s", kind)
		}
	}
}

func (s *socket) absent(t *testing.T, kind string, id uint) {
	t.Helper()
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-s.read:
			if !ok {
				t.Fatal("socket closed during absence check")
			}
			if e.Type == kind && e.Message.ID == id {
				t.Fatalf("unexpected %s delivery %d", kind, id)
			}
		case <-timer.C:
			return
		}
	}
}

func send(t *testing.T, s *socket, kind, key, body string, target uint) uint {
	t.Helper()
	e := map[string]any{"type": kind, "client_id": key, "body": body}
	if kind == "message" {
		e["recipient_id"] = target
	} else {
		e["group_id"] = target
	}
	s.write(t, e)
	ack := s.next(t, "ack", func(e event) bool { return e.ClientID == key })
	if ack.MessageID == 0 {
		t.Fatal("empty message ACK")
	}
	return ack.MessageID
}
