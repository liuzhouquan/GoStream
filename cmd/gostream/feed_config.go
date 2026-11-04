package main

import (
	"errors"
	"strconv"
)

func feedFanoutMaxFollowersFromEnv(getenv func(string) string) (int, error) {
	raw := getenv("FEED_FANOUT_MAX_FOLLOWERS")
	if raw == "" {
		raw = "1000"
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > 100000 {
		return 0, errors.New("FEED_FANOUT_MAX_FOLLOWERS must be an integer from 0 to 100000")
	}
	return value, nil
}
