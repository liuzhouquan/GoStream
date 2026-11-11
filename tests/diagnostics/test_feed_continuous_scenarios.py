"""The Feed acceptance driver must execute every required load scenario."""
from pathlib import Path
import sys
import base64
import json
import threading
import time
from types import SimpleNamespace
import unittest
from urllib.parse import parse_qs, urlsplit
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tests/integration"))
import feed_warm_cache_continuous as feed


def _deterministic_time():
    """Return a module-local time shim whose monotonic clock advances only on explicit ticks."""
    state = {"now": 100.0}
    lock = threading.Lock()

    def monotonic():
        with lock:
            return state["now"]

    def advance(delta=0.005):
        with lock:
            state["now"] += delta

    return SimpleNamespace(monotonic=monotonic, sleep=time.sleep, time=time.time), advance


class FeedContinuousScenariosTest(unittest.TestCase):
    def test_all_scenarios_run_three_times_at_both_concurrencies(self):
        seen = set()
        cursor = "opaque+/cursor=="
        clock, advance = _deterministic_time()

        def api(base, path, method="GET", token="", **kwargs):
            query = parse_qs(urlsplit(path).query)
            scene = query["scene"][0]
            if scene == "following":
                self.assertEqual(token, "viewer-token")
                if "cursor" in query:
                    self.assertEqual(query["cursor"], [cursor])
                    name, start = "following_deep", 21
                else:
                    name, start = "following", 1
            elif scene == "recommend":
                self.assertEqual(token, "viewer-token")
                name, start = "recommend_logged_in", 41
            else:
                self.assertEqual(scene, "hot")
                self.assertEqual(token, "")
                name, start = "hot", 61
            time.sleep(0.0001)  # Yield like real network I/O so both API instances participate.
            seen.add((base, name))
            advance(0.00025)
            next_cursor = "next"
            if scene == "recommend":
                next_cursor = base64.urlsafe_b64encode(json.dumps({
                    "c": 100, "i": start + 19, "s": 12, "a": 1000, "v": "recommend",
                }).encode()).decode().rstrip("=")
            return 200, {"items": [{"id": i} for i in range(start, start + 20)],
                         "next_cursor": next_cursor}, 1.0

        with patch.object(feed, "call_api", side_effect=api), \
             patch.object(feed, "time", clock):
            records = feed.run_feed_load_scenarios(
                ["http://a", "http://b"], "viewer-token", cursor,
                warmup_sec=0, test_sec=0.01, rounds=3,
            )
        self.assertEqual(len(records), 24)
        for concurrency in (10, 20):
            for scene in ("following", "following_deep", "recommend_logged_in", "hot"):
                group = [r for r in records if r["concurrency"] == concurrency and r["scene"] == scene]
                self.assertEqual({r["round"] for r in group}, {1, 2, 3})
                for record in group:
                    self.assertGreater(record["successes"], 0)
                    self.assertEqual(record["failures"], 0)
        for base in ("http://a", "http://b"):
            for scene in ("following", "following_deep", "recommend_logged_in", "hot"):
                self.assertIn((base, scene), seen)

    def test_formal_coverage_rejects_missing_duplicate_empty_and_short_rounds(self):
        from copy import deepcopy
        valid = [
            {"scene": scene, "concurrency": concurrency, "round": number,
             "warmup_duration_sec": 30.0, "duration_sec": 60.0,
             "total_requests": 100, "successes": 100, "failures": 0,
             "p95_ms": 10.0, "p99_ms": 20.0}
            for concurrency in (10, 20)
            for scene in ("following", "following_deep", "recommend_logged_in", "hot")
            for number in (1, 2, 3)
        ]
        self.assertEqual(feed.feed_load_coverage_issues(valid), [])
        invalid = [valid[1:], [r for r in valid if r["concurrency"] != 10]]
        for field, value in (("round", 2), ("successes", 0), ("failures", 1),
                             ("warmup_duration_sec", 0), ("duration_sec", 0),
                             ("p95_ms", None), ("p99_ms", 501)):
            changed = deepcopy(valid)
            changed[0][field] = value
            invalid.append(changed)
        for records in invalid:
            with self.subTest(records=records[:1]):
                self.assertTrue(feed.feed_load_coverage_issues(records))

    def test_recommendation_cursor_anchor_can_change_but_page_cannot(self):
        import base64
        import json
        counter = 0
        clock, advance = _deterministic_time()
        def response(*args, **kwargs):
            nonlocal counter
            counter += 1
            if counter > 1:
                advance(0.006)
            cursor = {"c": 100, "i": 20, "s": 12, "a": counter, "v": "recommend"}
            return 200, {"items": [{"id": i} for i in range(1, 21)],
                         "next_cursor": base64.urlsafe_b64encode(json.dumps(cursor).encode()).decode().rstrip("=")}, 1.0
        with patch.object(feed, "call_api", side_effect=response), \
             patch.object(feed, "time", clock):
            result = feed.run_load_burst(["http://a"], "/api/feed?scene=recommend&limit=20", "viewer-token", 1, 0.01)
        self.assertGreater(result["successes"], 0)
        self.assertEqual(result["failures"], 0)

    def test_recommendation_changes_other_than_anchor_remain_failures(self):
        for defect in ("wrong_scene", "wrong_last_id", "wrong_page", "duplicate", "malformed_cursor"):
            with self.subTest(defect=defect):
                calls = 0
                clock, advance = _deterministic_time()
                def response(*args, **kwargs):
                    nonlocal calls
                    calls += 1
                    if calls > 1:
                        advance(0.006)
                    cursor = {"c": 100, "i": 20, "s": 12, "a": calls, "v": "recommend"}
                    items = [{"id": i} for i in range(1, 21)]
                    if calls > 1:
                        if defect == "wrong_scene":
                            cursor["v"] = "following"
                        elif defect == "wrong_last_id":
                            cursor["i"] = 99
                        elif defect == "wrong_page":
                            items[0] = {"id": 99}
                        elif defect == "duplicate":
                            items[0] = items[1]
                    encoded = base64.urlsafe_b64encode(json.dumps(cursor).encode()).decode().rstrip("=")
                    if calls > 1 and defect == "malformed_cursor":
                        encoded = "invalid!"
                    return 200, {"items": items, "next_cursor": encoded}, 1.0
                with patch.object(feed, "call_api", side_effect=response), \
                     patch.object(feed, "time", clock):
                    result = feed.run_load_burst(["http://a"], "/api/feed?scene=recommend&limit=20", "viewer-token", 1, 0.01)
                self.assertEqual(result["successes"], 0)
                self.assertGreater(result["failures"], 0)

    def test_load_rejects_missing_login_or_deep_cursor_before_any_request(self):
        for token, cursor in (("", "cursor"), ("token", ""), ("token", None)):
            with self.subTest(token=token, cursor=cursor), patch.object(feed, "call_api") as api:
                with self.assertRaises(ValueError):
                    feed.run_feed_load_scenarios(["http://a"], token, cursor, 0, 0.01, 3)
                api.assert_not_called()
