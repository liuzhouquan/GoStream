package main

import (
	"net/http"
	"testing"
)

func TestChooseOperationWeightsAndMethods(t *testing.T) {
	counts := make(map[string]int)
	for i := uint64(0); i < 40; i++ {
		op := chooseOperation(i, 12, 34)
		counts[op.name]++
		if op.path == "" || op.want == 0 {
			t.Fatalf("incomplete operation %d: %+v", i, op)
		}
		if i%20 == 14 && (op.method != http.MethodPut || op.path != "/api/videos/12/like") {
			t.Fatalf("like operation %d: %+v", i, op)
		}
		if i%20 == 15 && op.method != http.MethodDelete {
			t.Fatalf("unlike operation %d: %+v", i, op)
		}
		if i == 18 && op.method != http.MethodPut || i == 38 && op.method != http.MethodDelete {
			t.Fatalf("follow toggle %d: %+v", i, op)
		}
	}
	feed, interaction, social := 0, 0, 0
	for name, count := range counts {
		switch name {
		case "feed_following", "feed_recommend", "feed_hot":
			feed += count
		case "like", "unlike", "favorite", "unfavorite":
			interaction += count
		case "follow", "comment":
			social += count
		default:
			t.Fatalf("unexpected operation %s", name)
		}
	}
	if feed != 28 || interaction != 8 || social != 4 {
		t.Fatalf("weights: feed=%d interaction=%d social=%d", feed, interaction, social)
	}
}
