package im_test

import (
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"
	"time"
)

type loadMessage struct {
	Sender, Recipient int
	Body              string
	Started           time.Time
	ID                uint
	Acked, Delivered  bool
	AckMS, DeliveryMS float64
}

type loadObserver struct {
	mu                                                sync.Mutex
	users                                             []account
	messages                                          map[string]*loadMessage
	acked, delivered, duplicateACK, duplicateDelivery int
}

// ACK and recipient arrival may occur in either order. Both must name the
// same persisted identity and correspond to the exact requested payload.
func (o *loadObserver) observe(client int, e event, at time.Time) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if e.Type == "notification" {
		return false, nil
	}
	if e.Type != "ack" && e.Type != "message" {
		return false, fmt.Errorf("unexpected load frame %q", e.Type)
	}
	key, id := e.ClientID, e.MessageID
	if e.Type == "message" {
		key, id = e.Message.ClientID, e.Message.ID
	}
	m, ok := o.messages[key]
	if !ok || m.Started.IsZero() || id == 0 {
		return false, fmt.Errorf("unknown/unissued message or empty ID for %q", key)
	}
	if m.ID != 0 && m.ID != id {
		return false, fmt.Errorf("message %s changed server ID", key)
	}
	if at.Before(m.Started) {
		return false, fmt.Errorf("negative elapsed time for %s", key)
	}
	elapsed := float64(at.Sub(m.Started).Microseconds()) / 1000
	if e.Type == "ack" {
		if client != m.Sender {
			return false, fmt.Errorf("ACK %s reached wrong sender", key)
		}
		if m.Acked {
			o.duplicateACK++
			return false, nil
		}
		m.ID, m.Acked, m.AckMS = id, true, elapsed
		o.acked++
		return true, nil
	}
	if client != m.Recipient || e.Message.SenderID != o.users[m.Sender].User.ID || e.Message.RecipientID != o.users[m.Recipient].User.ID || e.Message.Body != m.Body {
		return false, fmt.Errorf("message %s identity/payload mismatch", key)
	}
	if m.Delivered {
		o.duplicateDelivery++
		return false, nil
	}
	m.ID, m.Delivered, m.DeliveryMS = id, true, elapsed
	o.delivered++
	return false, nil
}

func (o *loadObserver) complete() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.acked == len(o.messages) && o.delivered == len(o.messages)
}

type latencySummary struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Max   float64 `json:"max_ms"`
}

func summarizeLatency(values []float64) latencySummary {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	if len(ordered) == 0 {
		return latencySummary{}
	}
	quantile := func(p float64) float64 { return ordered[int(math.Ceil(float64(len(ordered))*p))-1] }
	return latencySummary{Count: len(ordered), P50: quantile(.50), P95: quantile(.95), P99: quantile(.99), Max: ordered[len(ordered)-1]}
}

func TestIMLoadObserverRejectsFalseSuccess(t *testing.T) {
	start := time.Now()
	users := make([]account, 2)
	users[0].User.ID = 11
	users[1].User.ID = 22
	makeObserver := func() *loadObserver {
		return &loadObserver{users: users, messages: map[string]*loadMessage{"key": {Sender: 0, Recipient: 1, Body: "expected", Started: start}}}
	}
	delivery := event{Type: "message", Message: message{ID: 9, ClientID: "key", SenderID: 11, RecipientID: 22, Body: "expected"}}
	ack := event{Type: "ack", ClientID: "key", MessageID: 9}
	for _, tc := range []struct {
		name   string
		client int
		frame  event
	}{
		{"unknown", 0, event{Type: "ack", ClientID: "other", MessageID: 9}},
		{"wrong_ack_recipient", 1, ack},
		{"wrong_delivery_socket", 0, delivery},
		{"server_error", 0, event{Type: "error"}},
		{"empty_id", 0, event{Type: "ack", ClientID: "key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := makeObserver().observe(tc.client, tc.frame, start.Add(time.Millisecond)); err == nil {
				t.Fatal("accepted invalid protocol observation")
			}
		})
	}
	for _, recipientFirst := range []bool{false, true} {
		o := makeObserver()
		frames := []struct {
			client int
			frame  event
		}{{0, ack}, {1, delivery}}
		if recipientFirst {
			slices.Reverse(frames)
		}
		for i, v := range frames {
			if _, err := o.observe(v.client, v.frame, start.Add(time.Duration(i+1)*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		if !o.complete() {
			t.Fatal("missing complete ACK/delivery set")
		}
		if _, err := o.observe(1, delivery, start.Add(3*time.Millisecond)); err != nil || o.duplicateDelivery != 1 {
			t.Fatal("duplicate delivery identity not retained")
		}
		changed := ack
		changed.MessageID = 10
		if _, err := o.observe(0, changed, start.Add(4*time.Millisecond)); err == nil {
			t.Fatal("same client key accepted a different server ID")
		}
	}
	o := makeObserver()
	bad := delivery
	bad.Message.Body = "changed"
	if _, err := o.observe(1, bad, start.Add(time.Millisecond)); err == nil {
		t.Fatal("accepted wrong body")
	}
	if _, err := o.observe(0, ack, start.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if o.complete() {
		t.Fatal("ACK-only observations must not count as recipient delivery")
	}
}

func TestIMLoadLatencyRanks(t *testing.T) {
	values := []float64{100, 1, 4, 2, 3}
	got := summarizeLatency(values)
	if got != (latencySummary{Count: 5, P50: 3, P95: 100, P99: 100, Max: 100}) {
		t.Fatalf("nearest-rank quantiles: %+v", got)
	}
	if values[0] != 100 {
		t.Fatal("summary mutated per-message samples")
	}
}
