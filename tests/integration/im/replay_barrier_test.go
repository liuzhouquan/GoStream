package im_test

import (
	"strings"
	"testing"
	"time"
)

func TestReplayBarrier(t *testing.T) {
	for _, test := range []struct {
		name   string
		kinds  []string
		closed bool
		want   string
	}{
		{name: "pong", kinds: []string{"pong"}},
		{name: "unrelated_notifications", kinds: []string{"notification", "notification", "pong"}},
		{name: "direct_replay", kinds: []string{"notification", "message", "pong"}, want: "unexpected replay frame"},
		{name: "group_replay", kinds: []string{"notification", "group_message", "pong"}, want: "unexpected replay frame"},
		{name: "server_error", kinds: []string{"error", "pong"}, want: "unexpected replay frame"},
		{name: "unknown_frame", kinds: []string{"unknown", "pong"}, want: "unexpected replay frame"},
		{name: "closed", closed: true, want: "socket closed"},
		{name: "closed_after_notification", kinds: []string{"notification"}, closed: true, want: "socket closed"},
		{name: "missing_pong", want: "timed out"},
	} {
		t.Run(test.name, func(t *testing.T) {
			frames := make(chan event, len(test.kinds))
			for _, kind := range test.kinds {
				frames <- event{Type: kind}
			}
			if test.closed {
				close(frames)
			}
			err := waitReplayBarrier(frames, 20*time.Millisecond)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}

func TestReplayBarrierNotificationFloodKeepsFixedDeadline(t *testing.T) {
	frames := make(chan event, 16)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case frames <- event{Type: "notification"}:
			case <-stop:
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()
	start := time.Now()
	err := waitReplayBarrier(frames, 50*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("notification flood must time out without pong: %v", err)
	}
	if elapsed < 40*time.Millisecond || elapsed > time.Second {
		t.Fatalf("fixed 50ms deadline not honored: %s", elapsed)
	}
}
