"""Diagnostic unit tests for feed warm cache load runner (G07 / A04)."""

import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

import feed_warm_cache_smoke


class FeedWarmCacheDiagnosticUnitTest(unittest.TestCase):
    def test_free_port_allocation(self):
        port = feed_warm_cache_smoke.free_port()
        self.assertIsInstance(port, int)
        self.assertGreater(port, 1024)
        self.assertLessEqual(port, 65535)

    def test_benchmark_feed_db_seeding(self):
        with tempfile.NamedTemporaryFile(suffix=".db") as f:
            db_path = f.name
            conn = sqlite3.connect(db_path)
            conn.execute(
                "CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, "
                "password_hash TEXT NOT NULL, nickname TEXT NOT NULL, bio TEXT, avatar_url TEXT, "
                "last_seen_at DATETIME, created_at DATETIME)"
            )
            conn.execute(
                "CREATE TABLE follows (follower_id INTEGER NOT NULL, followee_id INTEGER NOT NULL, "
                "created_at DATETIME, PRIMARY KEY (follower_id, followee_id))"
            )
            conn.execute(
                "CREATE TABLE videos (id INTEGER PRIMARY KEY AUTOINCREMENT, author_id INTEGER NOT NULL, "
                "title TEXT NOT NULL, description TEXT, media_url TEXT NOT NULL, cover_url TEXT, "
                "status TEXT NOT NULL DEFAULT 'published', like_count INTEGER DEFAULT 0, "
                "comment_count INTEGER DEFAULT 0, favorite_count INTEGER DEFAULT 0, play_count INTEGER DEFAULT 0, "
                "duration_ms INTEGER NOT NULL DEFAULT 0, width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0, "
                "created_at DATETIME, feed_pull INTEGER NOT NULL DEFAULT 0)"
            )
            conn.execute(
                "CREATE TABLE feed_inboxes (user_id INTEGER NOT NULL, video_id INTEGER NOT NULL, "
                "created_at DATETIME, PRIMARY KEY (user_id, video_id))"
            )
            conn.commit()
            conn.close()

            viewer_id = feed_warm_cache_smoke.populate_benchmark_feed_db(db_path)
            self.assertGreater(viewer_id, 0)

            conn = sqlite3.connect(db_path)
            user_count = conn.execute("SELECT COUNT(*) FROM users").fetchone()[0]
            follow_count = conn.execute("SELECT COUNT(*) FROM follows").fetchone()[0]
            video_count = conn.execute("SELECT COUNT(*) FROM videos").fetchone()[0]
            inbox_count = conn.execute("SELECT COUNT(*) FROM feed_inboxes").fetchone()[0]
            pull_count = conn.execute("SELECT COUNT(*) FROM videos WHERE feed_pull = 1").fetchone()[0]
            conn.close()

            self.assertEqual(user_count, 11)   # 10 authors + 1 viewer
            self.assertEqual(follow_count, 10) # viewer follows all 10 authors
            self.assertEqual(video_count, 120) # 120 published videos
            self.assertEqual(pull_count, 12)   # 12 celebrity pull videos
            self.assertEqual(inbox_count, 108) # 108 push videos in viewer inbox

    def test_prometheus_metrics_parsing(self):
        sample_metrics = (
            '# HELP gostream_feed_cache_total Total feed cache operations.\n'
            '# TYPE gostream_feed_cache_total counter\n'
            'gostream_feed_cache_total{result="hit"} 4820\n'
            'gostream_feed_cache_total{result="miss"} 12\n'
            'gostream_feed_cache_total{result="error"} 0\n'
        )
        mock_resp = MagicMock()
        mock_resp.read.return_value = sample_metrics.encode("utf-8")
        mock_resp.__enter__.return_value = mock_resp

        with patch("feed_warm_cache_smoke.urlopen", return_value=mock_resp):
            parsed = feed_warm_cache_smoke.parse_prometheus_metrics("http://127.0.0.1:8080")
            self.assertEqual(parsed.get("cache_hit"), 4820.0)
            self.assertEqual(parsed.get("cache_miss"), 12.0)
            self.assertEqual(parsed.get("cache_error"), 0.0)

    def test_report_schema_and_serialization(self):
        sample_report = {
            "status": "PASS",
            "mode": "sqlite",
            "checkpoints": {
                "semantic_eligibility": True,
                "concurrency_10": True,
                "concurrency_20_three_rounds": True,
                "cache_invalidation_verified": True,
            },
            "concurrency_10_stats": {
                "concurrency": 10,
                "p50_ms": 0.85,
                "p95_ms": 2.10,
                "p99_ms": 3.40,
                "requests_per_sec": 4200.5,
                "failures": 0,
            },
            "concurrency_20_rounds": [
                {"round": 1, "p50_ms": 0.90, "p95_ms": 2.25, "p99_ms": 3.60, "requests_per_sec": 5100.0, "failures": 0},
                {"round": 2, "p50_ms": 0.88, "p95_ms": 2.20, "p99_ms": 3.55, "requests_per_sec": 5150.0, "failures": 0},
                {"round": 3, "p50_ms": 0.89, "p95_ms": 2.22, "p99_ms": 3.58, "requests_per_sec": 5120.0, "failures": 0},
            ],
            "invalidation_metrics": {
                "timeline_invalidated": True,
                "following_invalidated": True,
            },
        }
        encoded = json.dumps(sample_report)
        decoded = json.loads(encoded)
        self.assertEqual(decoded["status"], "PASS")
        self.assertTrue(decoded["checkpoints"]["semantic_eligibility"])
        self.assertTrue(decoded["checkpoints"]["concurrency_10"])
        self.assertTrue(decoded["checkpoints"]["concurrency_20_three_rounds"])
        self.assertTrue(decoded["checkpoints"]["cache_invalidation_verified"])
        self.assertEqual(len(decoded["concurrency_20_rounds"]), 3)
        for r in decoded["concurrency_20_rounds"]:
            self.assertLessEqual(r["p95_ms"], 200.0)
            self.assertLessEqual(r["p99_ms"], 500.0)
            self.assertEqual(r["failures"], 0)

    def test_json_call_failure_raises(self):
        with patch("feed_warm_cache_smoke.call_api", return_value=(500, {"error": "internal error"}, 1.5)):
            with self.assertRaises(RuntimeError) as ctx:
                feed_warm_cache_smoke.json_call("http://127.0.0.1:8080", "/api/feed?scene=timeline", expected=200)
            self.assertIn("expected 200, got 500", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
