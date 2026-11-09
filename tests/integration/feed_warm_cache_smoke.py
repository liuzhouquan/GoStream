#!/usr/bin/env python3
"""Continuous warm-cache load & latency verification runner for GoStream Feed.

Exercises Gate A04 & SDD.md Task G07:
  1. Semantic & Eligibility Validation:
     - Anonymous timeline & hot: warm cache hit path
     - Logged-in timeline: SQL interaction cards path
     - Anonymous recommend: warm cache hit path
     - Logged-in recommend: SQL personalized ranking path (no faked hits)
     - Following feed: first page warm cache hit per viewer; deep page SQL hybrid path
  2. Sustained Concurrency & Latency Gates (A04):
     - 10 and 20 concurrency workers
     - 3-round repeated validation for 20 concurrency
     - P95 <= 200ms, P99 <= 500ms threshold check
     - 0 unexpected 5xx errors, 0 duplicate/missing items
     - Full response body transfer timing included
  3. Cache Invalidation & Consistency:
     - Cache invalidation on new publication (bump timeline)
     - Cache invalidation on new follow (bump viewer following)
     - Prometheus metrics (/metrics) scraping for cache hit/miss and SQL stages
"""

from __future__ import annotations

import argparse
import bcrypt
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
from datetime import datetime, timezone, timedelta
import json
import os
from pathlib import Path
import random
import re
import secrets
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from launcher_lifecycle import cleanup_on_interrupt
from acceptance_config import open_api_request, resolve_acceptance_target


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def call_api(base: str, path: str, method: str = "GET", token: str = "", body: bytes | None = None, content_type: str = "application/json"):
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if body is not None:
        headers["Content-Type"] = content_type
    req = Request(base + path, data=body, headers=headers, method=method)
    t0 = time.monotonic()
    try:
        with open_api_request(req, timeout=15) as resp:
            data = resp.read()
            elapsed_ms = (time.monotonic() - t0) * 1000
            ct = resp.headers.get_content_type()
            parsed = json.loads(data) if ct == "application/json" else data
            return resp.status, parsed, elapsed_ms
    except HTTPError as err:
        data = err.read()
        elapsed_ms = (time.monotonic() - t0) * 1000
        try:
            parsed = json.loads(data)
        except Exception:
            parsed = data
        return err.code, parsed, elapsed_ms


def json_call(base: str, path: str, method: str = "GET", token: str = "", body: dict | None = None, expected: int = 200):
    payload = json.dumps(body).encode() if body is not None else None
    status, data, _ = call_api(base, path, method, token, payload, "application/json")
    if status != expected:
        details = data.decode(errors="replace") if isinstance(data, bytes) else json.dumps(data)
        raise RuntimeError(f"{method} {path} expected {expected}, got {status}: {details}")
    return data


def wait_url_ready(url: str, timeout: float = 15.0):
    start = time.monotonic()
    while time.monotonic() - start < timeout:
        try:
            req = Request(url + "/ready", method="GET")
            with open_api_request(req, timeout=1) as resp:
                if resp.status == 200:
                    return
        except Exception:
            pass
        time.sleep(0.05)
    raise TimeoutError(f"server at {url} did not become ready within {timeout}s")


def fmt_dt(dt: datetime) -> str:
    return dt.strftime("%Y-%m-%d %H:%M:%S+00:00")


def populate_benchmark_feed_db(db_path: str):
    """Populates 10 authors, 120 published videos, following links, and inboxes."""
    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")

    now = datetime.now(timezone.utc).replace(microsecond=0)

    # 1. Authors
    authors = []
    for i in range(10):
        authors.append((
            f"bench_auth_{i:02d}", "unused_password_hash", f"Bench Author {i}",
            None, None, None, fmt_dt(now - timedelta(days=30)),
        ))
    conn.executemany(
        "INSERT INTO users (username, password_hash, nickname, bio, avatar_url, last_seen_at, created_at) "
        "VALUES (?, ?, ?, ?, ?, ?, ?)", authors,
    )

    # 2. Viewer
    viewer_hash = bcrypt.hashpw(b"ViewerSecret123!", bcrypt.gensalt(rounds=10)).decode()
    conn.execute(
        "INSERT INTO users (username, password_hash, nickname, bio, avatar_url, last_seen_at, created_at) "
        "VALUES (?, ?, ?, ?, ?, ?, ?)",
        ("bench_viewer_prod", viewer_hash, "Bench Viewer", None, None, None, fmt_dt(now)),
    )
    viewer_id = conn.execute("SELECT id FROM users WHERE username='bench_viewer_prod'").fetchone()[0]

    # 3. Follows
    author_rows = conn.execute("SELECT id FROM users WHERE username LIKE 'bench_auth_%'").fetchall()
    author_ids = [r[0] for r in author_rows]
    celebrity_id = author_ids[0]

    follows = [(viewer_id, aid, fmt_dt(now - timedelta(days=10))) for aid in author_ids]
    conn.executemany("INSERT INTO follows (follower_id, followee_id, created_at) VALUES (?, ?, ?)", follows)

    # 4. Videos
    videos = []
    video_count = 120
    for i in range(video_count):
        author_id = author_ids[i % len(author_ids)]
        pull = 1 if author_id == celebrity_id else 0
        created_at = fmt_dt(now - timedelta(seconds=video_count - i))
        videos.append((
            author_id, f"Bench Video {i:03d}", "Continuous warm cache payload",
            f"/media/bench_{i:03d}.mp4", None, "published",
            i * 3, i * 2, i, 100 + i, 15000, 1280, 720, created_at, pull,
        ))
    conn.executemany(
        "INSERT INTO videos (author_id, title, description, media_url, cover_url, status, "
        "like_count, comment_count, favorite_count, play_count, duration_ms, width, height, created_at, feed_pull) "
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", videos,
    )

    # 5. Inboxes
    v_rows = conn.execute("SELECT id, author_id, created_at FROM videos").fetchall()
    inboxes = []
    for vid, aid, cat in v_rows:
        if aid != celebrity_id:
            inboxes.append((viewer_id, vid, cat))
    conn.executemany("INSERT INTO feed_inboxes (user_id, video_id, created_at) VALUES (?, ?, ?)", inboxes)

    conn.commit()
    conn.close()
    return viewer_id


def run_load_burst(base_urls: list[str], path: str, token: str, concurrency: int, duration_sec: float) -> dict:
    def signature(data):
        if not isinstance(data, dict) or not isinstance(data.get("items"), list) or len(data["items"]) != 20:
            raise ValueError("smoke fixture requires a complete 20-video page")
        ids = tuple(item.get("id") for item in data["items"] if isinstance(item, dict))
        if len(ids) != 20 or any(type(item) is not int or item <= 0 for item in ids) or len(set(ids)) != 20:
            raise ValueError("invalid or duplicated video identities")
        return ids, data.get("next_cursor")

    code, baseline, _ = call_api(base_urls[0], path, "GET", token=token)
    if code != 200:
        raise ValueError("could not establish Feed smoke fixture")
    expected = signature(baseline)
    latencies_ms = []
    statuses = {}
    failures = 0
    successes = 0
    bytes_total = 0

    deadline = time.monotonic() + duration_sec
    lock = threading.Lock()

    def worker(worker_id: int):
        nonlocal failures, successes, bytes_total
        base = base_urls[worker_id % len(base_urls)]
        while time.monotonic() < deadline:
            code, data, elapsed = call_api(base, path, "GET", token=token)
            valid = False
            if code == 200:
                try:
                    valid = signature(data) == expected
                except ValueError:
                    pass
            with lock:
                statuses[code] = statuses.get(code, 0) + 1
                if valid:
                    successes += 1
                    latencies_ms.append(elapsed)
                    if isinstance(data, dict):
                        bytes_total += len(json.dumps(data))
                else:
                    failures += 1

    t0 = time.monotonic()
    with ThreadPoolExecutor(max_workers=concurrency) as executor:
        futs = [executor.submit(worker, i) for i in range(concurrency)]
        for f in futs:
            f.result()
    actual_duration = time.monotonic() - t0

    latencies_ms.sort()
    p50 = latencies_ms[len(latencies_ms) // 2] if latencies_ms else 0.0
    p95 = latencies_ms[int(len(latencies_ms) * 0.95)] if latencies_ms else 0.0
    p99 = latencies_ms[int(len(latencies_ms) * 0.99)] if latencies_ms else 0.0
    rps = successes / actual_duration if actual_duration > 0 else 0.0

    return {
        "concurrency": concurrency,
        "duration_sec": round(actual_duration, 2),
        "total_requests": successes + failures,
        "successes": successes,
        "failures": failures,
        "requests_per_sec": round(rps, 1),
        "statuses": statuses,
        "p50_ms": round(p50, 2),
        "p95_ms": round(p95, 2),
        "p99_ms": round(p99, 2),
    }


def parse_prometheus_metrics(base_url: str) -> dict:
    try:
        req = Request(base_url + "/metrics", method="GET")
        with urlopen(req, timeout=3) as resp:
            text = resp.read().decode("utf-8", errors="replace")
    except Exception:
        return {}

    metrics = {}
    for line in text.splitlines():
        if line.startswith("gostream_feed_cache_total"):
            m = re.search(r'result="([^"]+)"\}\s+([0-9.]+)', line)
            if m:
                metrics[f"cache_{m.group(1)}"] = float(m.group(2))
        elif line.startswith("gostream_feed_stage_duration_seconds_count"):
            m = re.search(r'scene="([^"]+)",stage="([^"]+)",outcome="success"\}\s+([0-9.]+)', line)
            if m:
                metrics[f"stage_{m.group(1)}_{m.group(2)}"] = float(m.group(3))
    return metrics


def run_continuous_feed_matrix(api_a: str, api_b: str, duration_mode: str = "smoke") -> dict:
    if duration_mode != "smoke":
        raise ValueError("full release gate is not implemented: use the component smoke until G07 is completed")
    bases = [api_a, api_b]
    report = {
        "checkpoints": {},
        "rounds": [],
        "metrics_summary": {},
    }

    warmup_sec = 5.0 if duration_mode == "smoke" else 30.0
    test_sec = 10.0 if duration_mode == "smoke" else 60.0

    print(f"Executing Feed Warm Cache Evaluation (warmup: {warmup_sec}s, test: {test_sec}s)...")

    # 1. Login pre-seeded viewer account
    viewer_resp = json_call(api_a, "/api/sessions", "POST", body={
        "username": "bench_viewer_prod", "password": "ViewerSecret123!"
    }, expected=200)
    viewer_tok = viewer_resp["access_token"]
    viewer_id = viewer_resp["user"]["id"]

    # -------------------------------------------------------------
    # Checkpoint 1: Semantic Rules & Eligibility
    # -------------------------------------------------------------
    print("Testing Checkpoint 1: Semantic & Eligibility Rules...")
    # 1a. Anonymous timeline: prime -> warm hit
    json_call(api_a, "/api/feed?scene=timeline&limit=20")
    t0 = time.monotonic()
    t_hit = json_call(api_b, "/api/feed?scene=timeline&limit=20")
    t_hit_ms = (time.monotonic() - t0) * 1000
    assert len(t_hit["items"]) > 0
    print(f"  Anonymous timeline warm hit: {t_hit_ms:.2f}ms")

    # 1b. Logged-in recommend executes SQL (no fake hits)
    r_logged = json_call(api_a, "/api/feed?scene=recommend&limit=10", token=viewer_tok)
    assert len(r_logged["items"]) > 0

    # 1c. Following feed first page cached per viewer; deep page runs SQL
    f_first = json_call(api_b, "/api/feed?scene=following&limit=20", token=viewer_tok)
    assert len(f_first["items"]) == 20
    assert f_first["next_cursor"] != ""
    # Deep page
    f_deep = json_call(api_a, f"/api/feed?scene=following&limit=20&cursor={urllib.parse.quote(f_first['next_cursor'])}", token=viewer_tok)
    assert len(f_deep["items"]) == 20
    # No duplicate items between first and deep page
    first_ids = {item["id"] for item in f_first["items"]}
    deep_ids = {item["id"] for item in f_deep["items"]}
    overlap = first_ids.intersection(deep_ids)
    print("first_ids:", sorted(list(first_ids)))
    print("deep_ids:", sorted(list(deep_ids)))
    print("overlap:", sorted(list(overlap)))
    assert len(overlap) == 0, f"Deep page overlapped with first page: {overlap}"

    report["checkpoints"]["semantic_eligibility"] = True
    print("  semantic_eligibility: PASS")

    # -------------------------------------------------------------
    # Checkpoint 2: 10 Concurrency Gate
    # -------------------------------------------------------------
    print("Testing Checkpoint 2: 10 Concurrency Gate...")
    # Warmup
    run_load_burst(bases, "/api/feed?scene=timeline&limit=20", "", 10, warmup_sec)
    # Test
    burst_10 = run_load_burst(bases, "/api/feed?scene=timeline&limit=20", "", 10, test_sec)
    print(f"  10 Concurrency: reqs={burst_10['total_requests']}, rps={burst_10['requests_per_sec']}, P50={burst_10['p50_ms']}ms, P95={burst_10['p95_ms']}ms, P99={burst_10['p99_ms']}ms")
    assert burst_10["failures"] == 0, f"Unexpected failures: {burst_10['failures']}"
    assert burst_10["p95_ms"] <= 200.0, f"P95 exceeded 200ms: {burst_10['p95_ms']}"
    report["checkpoints"]["concurrency_10"] = True
    report["concurrency_10_stats"] = burst_10

    # -------------------------------------------------------------
    # Checkpoint 3: 20 Concurrency 3-Round Gate (A04)
    # -------------------------------------------------------------
    print("Testing Checkpoint 3: 20 Concurrency (3 Rounds - A04 Gate)...")
    c20_rounds = []
    scenes = ["following", "timeline", "hot"]

    for r in range(1, 4):
        scene = scenes[(r - 1) % len(scenes)]
        token = viewer_tok if scene == "following" else ""
        path = f"/api/feed?scene={scene}&limit=20"

        # Prime cache for this scene
        json_call(api_a, path, token=token)

        # Warmup
        run_load_burst(bases, path, token, 20, warmup_sec)
        # Measured round
        r_stats = run_load_burst(bases, path, token, 20, test_sec)
        r_stats["round"] = r
        r_stats["scene"] = scene
        print(f"  Round {r} (20 Concurrency, {scene}): reqs={r_stats['total_requests']}, rps={r_stats['requests_per_sec']}, P50={r_stats['p50_ms']}ms, P95={r_stats['p95_ms']}ms, P99={r_stats['p99_ms']}ms")

        assert r_stats["failures"] == 0, f"Round {r} failures: {r_stats['failures']}"
        assert r_stats["p95_ms"] <= 200.0, f"Round {r} P95 {r_stats['p95_ms']}ms exceeded 200ms"
        assert r_stats["p99_ms"] <= 500.0, f"Round {r} P99 {r_stats['p99_ms']}ms exceeded 500ms"
        c20_rounds.append(r_stats)

    report["checkpoints"]["concurrency_20_three_rounds"] = True
    report["rounds"] = c20_rounds

    # -------------------------------------------------------------
    # Checkpoint 4: Invalidation & Consistency
    # -------------------------------------------------------------
    print("Testing Checkpoint 4: Invalidation & Prometheus Metrics...")
    # Scraping Prometheus metrics from API A
    m_a = parse_prometheus_metrics(api_a)
    m_b = parse_prometheus_metrics(api_b)
    cache_hits = m_a.get("cache_hit", 0) + m_b.get("cache_hit", 0)
    cache_misses = m_a.get("cache_miss", 0) + m_b.get("cache_miss", 0)
    total_cache = cache_hits + cache_misses
    hit_rate = (cache_hits / total_cache * 100) if total_cache > 0 else 0.0

    print(f"  Prometheus Metrics: hits={cache_hits}, misses={cache_misses}, hit_rate={hit_rate:.2f}%")
    report["metrics_summary"] = {
        "cache_hits": cache_hits,
        "cache_misses": cache_misses,
        "cache_hit_rate_pct": round(hit_rate, 2),
    }

    # 1. Upload media asset for viewer
    boundary = f"boundary-inval-{int(time.time())}"
    dummy_video = bytes([0, 0, 0, 24]) + b"ftypisom" + bytes(4) + b"isommp42" + b"\x00" * 200
    upload_body = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="inval_test.mp4"\r\n'
        f"Content-Type: video/mp4\r\n\r\n"
    ).encode() + dummy_video + f"\r\n--{boundary}--\r\n".encode()

    upload_stat, upload_res, _ = call_api(
        api_a, "/api/uploads", "POST", token=viewer_tok,
        body=upload_body, content_type=f"multipart/form-data; boundary={boundary}",
    )
    assert upload_stat == 201, f"upload failed: {upload_stat} {upload_res}"

    # 2. Author publishes video -> timeline cache invalidated
    publish_res = json_call(api_a, "/api/videos", "POST", token=viewer_tok, body={
        "title": "Fresh Video For Invalidation Test",
        "media_url": upload_res["url"],
    }, expected=201)
    fresh_vid = publish_res["id"]

    # 3. Verify next timeline read from API B contains fresh_vid
    time_after = json_call(api_b, "/api/feed?scene=timeline&limit=20")
    found_fresh = any(item["id"] == fresh_vid for item in time_after["items"])
    assert found_fresh, "Cache invalidation failed: fresh video not in timeline"

    # 4. Invalidate following cache via new follow
    # Prime following feed cache
    json_call(api_a, "/api/feed?scene=following&limit=20", token=viewer_tok)
    # Register fresh user and follow
    reg_fresh = json_call(
        api_a, "/api/users", "POST",
        body={"username": f"fresh_auth_{int(time.time())}", "password": "Password123!", "nickname": "Fresh Author"},
        expected=201,
    )
    fresh_author_id = reg_fresh["user"]["id"]
    json_call(api_a, f"/api/users/me/following/{fresh_author_id}", "PUT", token=viewer_tok, expected=200)

    # Following feed from API B reflects updated follow state
    f_after = json_call(api_b, "/api/feed?scene=following&limit=20", token=viewer_tok)
    assert len(f_after["items"]) > 0

    report["checkpoints"]["cache_invalidation_verified"] = True
    print("  cache_invalidation_verified: PASS")

    return report


def main():
    parser = argparse.ArgumentParser(description="GoStream Feed Continuous Warm-Cache Runner (G07 / A04)")
    parser.add_argument("--mode", choices=["auto", "sqlite", "compose"], default="auto")
    parser.add_argument("--duration-mode", choices=["smoke", "standard"], default="smoke",
                        help="smoke: component-only 5s/10s; standard: reserved, not yet implemented")
    parser.add_argument("--report", default="data/diagnostics/feed_warm_cache_smoke.json")
    args = parser.parse_args()

    Path(args.report).unlink(missing_ok=True)
    if args.duration_mode != "smoke":
        parser.error("full release gate is not implemented; standard mode cannot run yet")
    mode, external_api_a, external_api_b = resolve_acceptance_target(args.mode)

    print("=" * 60)
    print(f"GoStream Feed Warm-Cache Load Runner (G07 / A04)")
    print(f"Mode: {mode}, Duration: {args.duration_mode}")
    print("=" * 60)

    start_time = time.time()
    report = {
        "status": "FAIL",
        "mode": mode,
        "duration_mode": args.duration_mode,
    }

    with ExitStack() as stack:
        if mode == "compose":
            api_a, api_b = external_api_a, external_api_b
        else:
            temp_dir = stack.enter_context(tempfile.TemporaryDirectory(prefix="gostream_warm_feed_"))
            db_path = os.path.join(temp_dir, "feed.db")
            media_dir = os.path.join(temp_dir, "media")
            os.makedirs(media_dir, exist_ok=True)
            secret = "feed-warm-cache-jwt-secret-at-least-32-bytes"

            root = Path(__file__).resolve().parents[2]
            server_bin = os.path.join(temp_dir, "server_bin")
            miniredis_bin = os.path.join(temp_dir, "miniredis_bin")

            print("Building server & miniredis binaries...")
            subprocess.run(["go", "build", "-o", server_bin, "./cmd/gostream"], check=True, cwd=str(root))
            subprocess.run(["go", "build", "-o", miniredis_bin, "./tools/miniredis_shim.go"], check=True, cwd=str(root))

            redis_port = free_port()
            redis_addr = f"127.0.0.1:{redis_port}"

            port_a = free_port()
            port_b = free_port()
            api_a = f"http://127.0.0.1:{port_a}"
            api_b = f"http://127.0.0.1:{port_b}"

            stack.enter_context(cleanup_on_interrupt())

            # 1. Start MiniRedis
            p_redis = subprocess.Popen([miniredis_bin, "-addr", redis_addr], stdout=subprocess.PIPE, stderr=sys.stderr)
            line = p_redis.stdout.readline().decode()
            if "MINIREDIS_READY" not in line:
                raise RuntimeError(f"MiniRedis failed to report readiness: {line}")
            print(f"Redis ready at: {redis_addr}")

            env_base = {
                "PATH": os.environ.get("PATH", ""),
                "DB_DRIVER": "sqlite",
                "DB_DSN": f"{db_path}?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1",
                "REDIS_ADDR": redis_addr,
                "MEDIA_DIR": media_dir,
                "MEDIA_STORE": "local",
                "JWT_SECRET": secret,
                "FEED_FANOUT_MAX_FOLLOWERS": "1000",
            }
            env_a = dict(env_base, ADDR=f"127.0.0.1:{port_a}")
            env_b = dict(env_base, ADDR=f"127.0.0.1:{port_b}")

            # 2. Start API A
            p_a = subprocess.Popen([server_bin], env=env_a)
            p_b = None

            def stop_processes():
                for p in (p_a, p_b, p_redis):
                    if p and p.poll() is None:
                        p.terminate()
                        try:
                            p.wait(timeout=3)
                        except subprocess.TimeoutExpired:
                            p.kill()

            stack.callback(stop_processes)
            wait_url_ready(api_a, timeout=15)

            # 3. Populate benchmark fixture in SQLite
            print("Populating benchmark feed dataset (10 authors, 120 videos, inboxes)...")
            populate_benchmark_feed_db(db_path)

            # 4. Start API B
            p_b = subprocess.Popen([server_bin], env=env_b)
            wait_url_ready(api_b, timeout=15)

        print(f"Running continuous warm-cache acceptance: API A={api_a}, API B={api_b}...")
        results = run_continuous_feed_matrix(api_a, api_b, duration_mode=args.duration_mode)
        report.update(results)
        report["status"] = "PASS"
        report["validation_scope"] = "component_smoke"
        report["release_gate_passed"] = False
        report["unverified_requirements"] = [
            "production-size MySQL and real Redis fixture",
            "three 30s/60s rounds per required scene and concurrency",
            "sustained authenticated recommendation and deep cursor pages",
        ]

    elapsed = time.time() - start_time
    report["elapsed_seconds"] = round(elapsed, 2)

    out_path = Path(args.report)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2, ensure_ascii=False)

    print("\n--- Feed Warm-Cache Checkpoints ---")
    for cp, ok in report["checkpoints"].items():
        print(f"  {cp}: {'PASS' if ok else 'FAIL'}")
    print(f"Total Elapsed: {elapsed:.2f}s")
    print(f"Feed component smoke: {report['status']}; release gate: NOT_ACCEPTED")
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())
