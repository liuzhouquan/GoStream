package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetFeedOptionalToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Authorization", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	for _, tc := range []struct {
		token string
		want  string
	}{{"", ""}, {"test-access-token", "Bearer test-access-token"}} {
		response, err := getFeed(server.Client(), server.URL, tc.token)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if got := response.Header.Get("X-Seen-Authorization"); got != tc.want {
			t.Fatalf("authorization header = %q, want %q", got, tc.want)
		}
	}
}
