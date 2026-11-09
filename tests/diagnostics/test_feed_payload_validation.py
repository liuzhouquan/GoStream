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
        with self.assertRaisesRegex(ValueError, "Invalid duration or rounds parameters"):
            continuous.run_continuous_feed_matrix("http://a", "http://b", warmup_sec=5, test_sec=10, rounds=1)
        with self.assertRaisesRegex(ValueError, "Invalid duration or rounds parameters"):
            continuous.run_continuous_feed_matrix("http://a", "http://b", warmup_sec=5, test_sec=10, rounds=2)

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

    def test_dataset_scale_check_and_formal_gate_rejection(self):
        import sqlite3
        import tempfile
        with tempfile.NamedTemporaryFile(suffix=".db") as tmp:
            db_path = tmp.name
            conn = sqlite3.connect(db_path)
            conn.execute("CREATE TABLE users (id INTEGER PRIMARY KEY)")
            conn.execute("CREATE TABLE videos (id INTEGER PRIMARY KEY, status TEXT)")
            conn.execute("CREATE TABLE follows (id INTEGER PRIMARY KEY, follower_id INTEGER, followee_id INTEGER)")
            conn.execute("CREATE TABLE video_actions (id INTEGER PRIMARY KEY, kind TEXT)")
            conn.execute("CREATE TABLE comments (id INTEGER PRIMARY KEY)")
            for i in range(10):
                conn.execute("INSERT INTO users VALUES (?)", (i + 1,))
            for i in range(120):
                conn.execute("INSERT INTO videos VALUES (?, ?)", (i + 1, "published"))
            conn.commit()
            conn.close()

            scale = continuous.check_dataset_scale("sqlite", db_path=db_path)
            self.assertEqual(scale["users"], 10)
            self.assertEqual(scale["published_videos"], 120)
            self.assertEqual(scale["videos"], 120)
            self.assertFalse(scale["meets_formal_scale"])

    def test_compose_dataset_scale_check_command_and_parsing(self):
        from unittest.mock import MagicMock
        with patch.object(continuous.subprocess, "run") as mock_run:
            # Case 1: Meets formal scale
            mock_res = MagicMock()
            mock_res.returncode = 0
            mock_res.stdout = "2500 21000 22000 12000 55000 3 15\n"
            mock_run.return_value = mock_res

            scale = continuous.check_dataset_scale("compose")
            self.assertEqual(scale["users"], 2500)
            self.assertEqual(scale["published_videos"], 21000)
            self.assertEqual(scale["videos"], 22000)
            self.assertEqual(scale["follows"], 12000)
            self.assertEqual(scale["interactions"], 55000)
            self.assertTrue(scale["has_high_author"])
            self.assertTrue(scale["has_low_author"])
            self.assertTrue(scale["meets_formal_scale"])

            # Verify command includes Compose files and ROOT
            cmd = mock_run.call_args[0][0]
            self.assertIn("docker", cmd)
            self.assertIn("compose", cmd)
            self.assertIn(str(continuous.ROOT / "docker-compose.yml"), cmd)

            # Case 2: Insufficient published videos (19,999)
            mock_res.stdout = "2500 19999 22000 12000 55000 3 15\n"
            scale = continuous.check_dataset_scale("compose")
            self.assertFalse(scale["meets_formal_scale"])

            # Case 3: Insufficient interactions (49,999)
            mock_res.stdout = "2500 21000 22000 12000 49999 3 15\n"
            scale = continuous.check_dataset_scale("compose")
            self.assertFalse(scale["meets_formal_scale"])

            # Case 4: No high author
            mock_res.stdout = "2500 21000 22000 12000 55000 0 15\n"
            scale = continuous.check_dataset_scale("compose")
            self.assertFalse(scale["meets_formal_scale"])

    def test_main_aborts_upfront_on_insufficient_formal_dataset(self):
        import tempfile
        import json
        with tempfile.NamedTemporaryFile(suffix=".json") as tmp:
            report_file = tmp.name
            with patch.object(continuous, "resolve_acceptance_target", return_value=("sqlite", "http://fake-a", "http://fake-b")), \
                 patch.object(continuous, "populate_benchmark_feed_db"), \
                 patch.object(continuous, "wait_url_ready"), \
                 patch.object(continuous, "check_dataset_scale", return_value={
                     "users": 10, "published_videos": 120, "videos": 120, "follows": 10,
                     "interactions": 50, "has_high_author": False, "has_low_author": True,
                     "meets_formal_scale": False
                 }), \
                 patch.object(continuous, "run_continuous_feed_matrix") as mock_matrix, \
                 patch.object(sys, "argv", ["feed_warm_cache_continuous.py", "--mode", "sqlite", "--formal-dataset", "--report", report_file]):
                code = continuous.main()
                self.assertEqual(code, 1)
                # Verify load matrix was NOT executed
                mock_matrix.assert_not_called()
                # Verify report content
                rep = json.loads(Path(report_file).read_text())
                self.assertEqual(rep["status"], "FAIL")
                self.assertEqual(rep["validation_scope"], "rejected_insufficient_scale")
                self.assertFalse(rep["release_gate_passed"])

    def test_main_rejects_fewer_than_three_rounds(self):
        import tempfile
        import json
        with tempfile.NamedTemporaryFile(suffix=".json") as tmp:
            report_file = tmp.name
            with patch.object(continuous, "resolve_acceptance_target", return_value=("sqlite", "http://fake-a", "http://fake-b")), \
                 patch.object(continuous, "run_continuous_feed_matrix") as mock_matrix, \
                 patch.object(sys, "argv", ["feed_warm_cache_continuous.py", "--mode", "sqlite", "--rounds", "2", "--report", report_file]):
                code = continuous.main()
                self.assertEqual(code, 1)
                mock_matrix.assert_not_called()
                rep = json.loads(Path(report_file).read_text())
                self.assertEqual(rep["status"], "FAIL")
                self.assertEqual(rep["validation_scope"], "rejected_invalid_parameters")
                self.assertFalse(rep["release_gate_passed"])

    def test_main_aborts_upfront_on_compose_formal_dataset_without_seeding(self):
        import tempfile
        import json
        with tempfile.NamedTemporaryFile(suffix=".json") as tmp:
            report_file = tmp.name
            with patch.object(continuous, "resolve_acceptance_target", return_value=("compose", "http://fake-a", "http://fake-b")), \
                 patch.object(continuous, "seed_mysql_feed_db") as mock_seed, \
                 patch.object(continuous, "check_dataset_scale", return_value={
                     "users": 10, "published_videos": 120, "videos": 120, "follows": 10,
                     "interactions": 50, "has_high_author": False, "has_low_author": True,
                     "meets_formal_scale": False
                 }), \
                 patch.object(continuous, "run_continuous_feed_matrix") as mock_matrix, \
                 patch.object(sys, "argv", ["feed_warm_cache_continuous.py", "--mode", "compose", "--formal-dataset", "--report", report_file]):
                code = continuous.main()
                self.assertEqual(code, 1)
                mock_seed.assert_not_called()
                mock_matrix.assert_not_called()
                rep = json.loads(Path(report_file).read_text())
                self.assertEqual(rep["status"], "FAIL")
                self.assertEqual(rep["validation_scope"], "rejected_insufficient_scale")
                self.assertFalse(rep["release_gate_passed"])
