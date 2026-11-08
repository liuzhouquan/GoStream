"""A 200 response cannot hide an incomplete or inconsistent Feed payload."""
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tests/integration"))
import feed_warm_cache_smoke as smoke
import feed_warm_cache_continuous as continuous


class FeedPayloadValidationTest(unittest.TestCase):
    def test_http_200_with_bad_body_is_counted_as_failure(self):
        complete = {"items": [{"id": i} for i in range(1, 21)], "next_cursor": "cursor"}
        invalid = [
            {"items": []},
            {"items": [{"id": 1}] * 20, "next_cursor": "cursor"},
            {"items": [{"id": i} for i in range(2, 22)], "next_cursor": "cursor"},
            {"items": complete["items"], "next_cursor": "different"},
            {"error": "not a feed"},
        ]
        for payload in invalid:
            with self.subTest(payload=payload):
                calls = 0
                def response(*args, **kwargs):
                    nonlocal calls
                    calls += 1
                    return 200, complete if calls == 1 else payload, 1.0
                with patch.object(smoke, "call_api", side_effect=response):
                    result = smoke.run_load_burst(["http://fixture"], "/api/feed?limit=20", "", 1, .01)
                self.assertGreater(result["total_requests"], 0)
                self.assertEqual(result["successes"], 0)
                self.assertEqual(result["failures"], result["total_requests"])

    def test_standard_mode_cannot_claim_unimplemented_release_gate(self):
        with self.assertRaisesRegex(ValueError, "release.*not implemented"):
            smoke.run_continuous_feed_matrix("http://a", "http://b", duration_mode="standard")

    def test_continuous_feed_rejects_zero_or_negative_parameters(self):
        with self.assertRaisesRegex(ValueError, "Invalid duration or rounds parameters"):
            continuous.run_continuous_feed_matrix("http://a", "http://b", warmup_sec=0, test_sec=0, rounds=0)
        with self.assertRaisesRegex(ValueError, "Invalid duration or rounds parameters"):
            continuous.run_continuous_feed_matrix("http://a", "http://b", warmup_sec=-1, test_sec=10, rounds=3)
        with self.assertRaisesRegex(ValueError, "Invalid duration or rounds parameters"):
            continuous.run_continuous_feed_matrix("http://a", "http://b", warmup_sec=5, test_sec=-5, rounds=3)
        with self.assertRaisesRegex(ValueError, "Invalid duration or rounds parameters"):
            continuous.run_continuous_feed_matrix("http://a", "http://b", warmup_sec=5, test_sec=10, rounds=-1)

    def test_continuous_feed_empty_samples_returns_none_latencies(self):
        valid_items = [{"id": i + 1} for i in range(20)]
        call_count = 0

        def response(base_url, path, method="GET", token="", **kwargs):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                # Baseline call succeeds
                return 200, {"items": valid_items, "next_cursor": None}, 5.0
            # Subsequent measurement calls return 500 (no successful samples in window)
            return 500, {"error": "internal error"}, 10.0

        with patch.object(continuous, "call_api", side_effect=response):
            res = continuous.run_load_burst(["http://fixture"], "/api/feed", "", 1, 0.05)
            self.assertEqual(res["successes"], 0)
            self.assertGreater(res["failures"], 0)
            self.assertIsNone(res["p50_ms"])
            self.assertIsNone(res["p95_ms"])
            self.assertIsNone(res["p99_ms"])
