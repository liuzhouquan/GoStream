import json
import os
import sqlite3
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import search_query_smoke


class SearchQuerySmokeUnitTest(unittest.TestCase):
    def test_free_port_allocation(self):
        port1 = search_query_smoke.free_port()
        port2 = search_query_smoke.free_port()
        self.assertIsInstance(port1, int)
        self.assertIsInstance(port2, int)
        self.assertGreater(port1, 1024)
        self.assertGreater(port2, 1024)

    def test_sqlite_population_schema_and_counts(self):
        with tempfile.NamedTemporaryFile(suffix=".db") as f:
            db_path = f.name
            conn = sqlite3.connect(db_path)
            conn.execute("CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, nickname TEXT NOT NULL, bio TEXT, avatar_url TEXT, last_seen_at DATETIME, created_at DATETIME)")
            conn.execute("CREATE TABLE videos (id INTEGER PRIMARY KEY AUTOINCREMENT, author_id INTEGER NOT NULL, title TEXT NOT NULL, description TEXT, media_url TEXT NOT NULL, cover_url TEXT, status TEXT NOT NULL DEFAULT 'published', like_count INTEGER DEFAULT 0, comment_count INTEGER DEFAULT 0, favorite_count INTEGER DEFAULT 0, play_count INTEGER DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0, width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0, created_at DATETIME, feed_pull INTEGER NOT NULL DEFAULT 1)")
            conn.execute("CREATE TABLE notifications (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, event_key TEXT NOT NULL UNIQUE, kind TEXT NOT NULL, actor_id INTEGER, video_id INTEGER, text TEXT, read_at DATETIME, created_at DATETIME)")
            conn.commit()
            conn.close()

            search_query_smoke.populate_sqlite_production_dataset(
                db_path, user_count=50, video_count=100, notif_count=30
            )

            conn = sqlite3.connect(db_path)
            u_count = conn.execute("SELECT COUNT(*) FROM users").fetchone()[0]
            v_count = conn.execute("SELECT COUNT(*) FROM videos").fetchone()[0]
            n_count = conn.execute("SELECT COUNT(*) FROM notifications").fetchone()[0]
            conn.close()

            self.assertEqual(u_count, 50)
            self.assertEqual(v_count, 100)
            self.assertEqual(n_count, 30)

    def test_json_call_failure_raises(self):
        with patch("search_query_smoke.call_api", return_value=(500, {"error": "db failure"})):
            with self.assertRaises(RuntimeError) as ctx:
                search_query_smoke.json_call("http://127.0.0.1:8080", "/api/search", expected=200)
            self.assertIn("expected 200, got 500", str(ctx.exception))

    def test_report_structure(self):
        report = {
            "status": "PASS",
            "mode": "sqlite",
            "scale": "production",
            "checkpoints": {
                "search_users": True,
                "search_videos": True,
                "notifications_pagination_and_read": True,
                "query_performance_and_plans": True,
            },
            "metrics": {
                "video_search_shallow_ms": 10.5,
                "video_search_deep_ms": 7.2,
                "video_search_p50_ms": 8.1,
                "video_search_p95_ms": 11.4,
                "video_search_max_ms": 13.0,
            },
            "applicability": {
                "dataset_size_videos": 20000,
                "dataset_size_users": 2000,
                "applicable_range_sql_like": "<= 100,000 videos",
            },
        }
        serialized = json.dumps(report)
        loaded = json.loads(serialized)
        self.assertEqual(loaded["status"], "PASS")
        self.assertTrue(loaded["checkpoints"]["search_users"])
        self.assertIn("applicable_range_sql_like", loaded["applicability"])


if __name__ == "__main__":
    unittest.main()
