package main

import "testing"

func TestFeedFanoutMaxFollowersFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want int
	}{
		{name: "default enables hybrid fanout", want: 1000},
		{name: "zero selects SQL-only", env: map[string]string{"FEED_FANOUT_MAX_FOLLOWERS": "0"}, want: 0},
		{name: "custom threshold", env: map[string]string{"FEED_FANOUT_MAX_FOLLOWERS": "250"}, want: 250},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := feedFanoutMaxFollowersFromEnv(func(key string) string { return tt.env[key] })
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("fanout threshold = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFeedFanoutMaxFollowersFromEnvRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{"not-a-number", "-1", "100001"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := feedFanoutMaxFollowersFromEnv(func(string) string { return raw }); err == nil {
				t.Fatalf("expected threshold %q to be rejected", raw)
			}
		})
	}
}
