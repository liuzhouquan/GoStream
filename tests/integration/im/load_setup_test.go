package im_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Respect the shared per-peer production registration budget. This is setup,
// outside the measured window; no rate limiter is disabled or forwarded IP used.
func registerLoadAccount(ctx context.Context, base, name string) (account, time.Duration, error) {
	payload, _ := json.Marshal(map[string]string{"username": name, "nickname": name, "password": "isolated-im-fixture-password"})
	var waited time.Duration
	for attempt := 0; attempt < 3; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		trace := &registrationTrace{}
		requestCtx = httptrace.WithClientTrace(requestCtx, trace.callbacks())
		request, err := http.NewRequestWithContext(requestCtx, "POST", base+"/api/users", bytes.NewReader(payload))
		if err != nil {
			cancel()
			return account{}, waited, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			return account{}, waited, trace.wrap(err)
		}
		if response.StatusCode == http.StatusCreated {
			var result account
			err := json.NewDecoder(response.Body).Decode(&result)
			_ = response.Body.Close()
			cancel()
			if err != nil || result.User.ID == 0 || result.Token == "" {
				return account{}, waited, fmt.Errorf("registration returned invalid identity")
			}
			return result, waited, nil
		}
		status, retry := response.StatusCode, response.Header.Get("Retry-After")
		_ = response.Body.Close()
		cancel()
		if status != http.StatusTooManyRequests {
			return account{}, waited, fmt.Errorf("load registration status=%d", status)
		}
		seconds, err := strconv.Atoi(retry)
		if err != nil || seconds < 1 || seconds > 61 {
			return account{}, waited, fmt.Errorf("invalid registration Retry-After")
		}
		begin := time.Now()
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		select {
		case <-timer.C:
			waited += time.Since(begin)
		case <-ctx.Done():
			timer.Stop()
			return account{}, waited, ctx.Err()
		}
	}
	return account{}, waited, fmt.Errorf("registration retry budget exhausted")
}

func TestIMLoadRegistrationEOFIsNotRetriedAndHasTrace(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close() // Server consumed POST but dropped the response: outcome is ambiguous.
	}))
	defer server.Close()
	_, waited, err := registerLoadAccount(context.Background(), server.URL, "fixture")
	if !errors.Is(err, io.EOF) || calls.Load() != 1 || waited != 0 {
		t.Fatalf("EOF must remain a failure without replay: calls=%d wait=%v err=%v", calls.Load(), waited, err)
	}
	if !strings.Contains(err.Error(), "got_conn=true") || !strings.Contains(err.Error(), "first_byte=false") {
		t.Fatalf("registration EOF lost transport evidence: %v", err)
	}
	if strings.Contains(err.Error(), "isolated-im-fixture-password") {
		t.Fatal("registration diagnostic exposed credentials")
	}
}

func TestIMLoadRegistrationAfterRateLimitAndIdleClose(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"user":{"id":7},"access_token":"test-only"}`))
	}))
	server.Config.IdleTimeout = 10 * time.Millisecond
	server.Start()
	defer server.Close()
	who, wait, err := registerLoadAccount(context.Background(), server.URL, "fixture")
	if err != nil || who.User.ID != 7 || calls.Load() != 2 || wait < time.Second {
		t.Fatalf("rate-limit wait and idle close must not invent EOF: calls=%d wait=%v err=%v", calls.Load(), wait, err)
	}
}

func TestIMLoadRegistrationRespectsRateLimit(t *testing.T) {
	t.Run("cancel_wait", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "60"); w.WriteHeader(429) }))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, _, err := registerLoadAccount(ctx, server.URL, "fixture"); err == nil {
			t.Fatal("registration ignored cancellation while rate limited")
		}
	})
	for _, header := range []string{"0", "62", "invalid"} {
		t.Run("invalid_retry_"+header, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", header)
				w.WriteHeader(429)
			}))
			defer server.Close()
			if _, _, err := registerLoadAccount(context.Background(), server.URL, "fixture"); err == nil {
				t.Fatal("invalid retry policy accepted")
			}
		})
	}
	t.Run("normal_identity", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"user":{"id":7},"access_token":"test-only"}`))
		}))
		defer server.Close()
		who, wait, err := registerLoadAccount(context.Background(), server.URL, "fixture")
		if err != nil || who.User.ID != 7 || who.Token != "test-only" || wait != 0 {
			t.Fatal("normal fixture registration failed")
		}
	})
}
