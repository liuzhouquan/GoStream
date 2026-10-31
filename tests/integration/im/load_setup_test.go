package im_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		request, err := http.NewRequestWithContext(requestCtx, "POST", base+"/api/users", bytes.NewReader(payload))
		if err != nil {
			cancel()
			return account{}, waited, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			return account{}, waited, err
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
