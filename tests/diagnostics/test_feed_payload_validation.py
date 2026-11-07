"""A 200 response cannot hide an incomplete or inconsistent Feed payload."""
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tests/integration"))
import feed_warm_cache_smoke as smoke


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
