#!/usr/bin/env python3
"""Run the full-size, cache-free A04 following Feed comparison on an isolated MySQL service."""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import platform
import re
import secrets
import statistics
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


class AcceptanceError(RuntimeError):
    pass


A04_CONCURRENCY = 20
A04_WARMUP = "30s"
A04_WARMUP_SECONDS = 30
A04_DURATION = "60s"
A04_DURATION_SECONDS = 60
A04_REQUIRED_IMPROVEMENT = 0.20


def process_stat(pid: int) -> tuple[int, int]:
    """Return process CPU ticks and RSS KiB from Linux /proc."""
    stat = pathlib.Path(f"/proc/{pid}/stat").read_text()
    fields = stat[stat.rfind(")") + 2 :].split()
    cpu_ticks = int(fields[11]) + int(fields[12])
    resident_pages = int(pathlib.Path(f"/proc/{pid}/statm").read_text().split()[1])
    rss_kib = resident_pages * os.sysconf("SC_PAGE_SIZE") // 1024
    return cpu_ticks, rss_kib


def process_metrics(samples: list[tuple[float, int, int]]) -> dict[str, float | int]:
    if len(samples) < 2:
        raise AcceptanceError("API process resource sampler collected fewer than two samples")
    elapsed = samples[-1][0] - samples[0][0]
    if elapsed <= 0:
        raise AcceptanceError("API process resource sample interval is not positive")
    ticks_per_second = os.sysconf("SC_CLK_TCK")
    cpu_seconds = (samples[-1][1] - samples[0][1]) / ticks_per_second
    peak_cpu = 0.0
    for previous, current in zip(samples, samples[1:]):
        interval = current[0] - previous[0]
        if interval > 0:
            peak_cpu = max(peak_cpu, (current[1] - previous[1]) / ticks_per_second / interval * 100)
    return {
        "sample_count": len(samples),
        "sample_interval_seconds": round(elapsed, 3),
        "cpu_seconds": round(cpu_seconds, 3),
        "average_cpu_percent": round(cpu_seconds / elapsed * 100, 2),
        "peak_sampled_cpu_percent": round(peak_cpu, 2),
        "peak_rss_kib": max(sample[2] for sample in samples),
    }


def read_json(url: str, token: str | None = None, payload: dict | None = None) -> dict:
    headers = {"Accept": "application/json"}
    data = None
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(url, data=data, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as error:
        raise AcceptanceError(f"HTTP request failed for {urllib.parse.urlsplit(url).path}: {error}") from error


def following_page_ids(base_url: str, token: str) -> list[int]:
    query = {"scene": "following", "limit": "20"}
    first = read_json(f"{base_url}/api/feed?{urllib.parse.urlencode(query)}", token)
    first_items = first.get("items")
    cursor = first.get("next_cursor")
    if not isinstance(first_items, list) or len(first_items) != 20 or not cursor:
        raise AcceptanceError("fixture must return a full first following page and a next cursor")
    second = read_json(
        f"{base_url}/api/feed?{urllib.parse.urlencode({**query, 'cursor': cursor})}", token
    )
    second_items = second.get("items")
    if not isinstance(second_items, list) or len(second_items) != 20:
        raise AcceptanceError("fixture must return a full second following page")
    ids = [int(item["id"]) for item in first_items + second_items]
    if len(set(ids)) != 40:
        raise AcceptanceError("the first two following Feed pages contain duplicate video IDs")
    return ids


def host_evidence(go_bin: str, mysql_container: str, database: str) -> str:
    sections = [f"captured_at_utc={time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}"]
    sections.append(f"platform={platform.platform()}")
    sections.append(f"cpu_count={os.cpu_count()}")
    for command in (("uname", "-a"), ("lscpu",), ("free", "-h"), (go_bin, "version")):
        result = subprocess.run(command, capture_output=True, text=True, check=False)
        sections.append(f"$ {' '.join(command)}\n{result.stdout}{result.stderr}")

    if not re.fullmatch(r"gostream_feed_a04_[A-Za-z0-9_]+", database):
        raise AcceptanceError("database name must use the isolated gostream_feed_a04_ prefix")
    sql = "\n".join(
        [
            "SELECT CONCAT('mysql_version=', VERSION());",
            "SHOW GLOBAL VARIABLES WHERE Variable_name IN ('innodb_buffer_pool_size','innodb_flush_log_at_trx_commit','max_connections','transaction_isolation');",
            *[
                f"SELECT '{table}_rows=', COUNT(*) FROM `{database}`.`{table}`;"
                for table in ("users", "follows", "videos", "feed_fanout_jobs", "feed_inboxes", "video_actions")
            ],
            f"SELECT 'fanout_jobs_pending=', COUNT(*) FROM `{database}`.`feed_fanout_jobs` WHERE done_at IS NULL;",
            f"SELECT 'outbox_pending=', COUNT(*) FROM `{database}`.`outbox_events` WHERE published_at IS NULL AND dead_at IS NULL;",
        ]
    )
    command = [
        "docker",
        "exec",
        "-i",
        mysql_container,
        "sh",
        "-c",
        'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names',
    ]
    result = subprocess.run(command, input=sql, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        raise AcceptanceError(f"could not collect isolated MySQL evidence: {result.stderr.strip()}")
    sections.append("mysql_and_fixture:\n" + result.stdout)
    return "\n\n".join(sections)


def start_api(binary: str, report_dir: pathlib.Path, work_dir: pathlib.Path, mode: str, round_number: int, args):
    address = f"127.0.0.1:{args.port}"
    log_path = report_dir / f"api-{mode}-round-{round_number}.log"
    log_handle = log_path.open("w")
    environment = os.environ.copy()
    environment.update(
        {
            "ADDR": address,
            "DB_DRIVER": "mysql",
            "MEDIA_DIR": str(work_dir / f"media-{mode}-{round_number}"),
            "JWT_SECRET": args.jwt_secret,
            "REDIS_ADDR": "",
            "RABBITMQ_URL": "",
            "FFMPEG_BIN": "",
            "FEED_FANOUT_MAX_FOLLOWERS": "0" if mode == "sql-only" else str(args.fanout_max_followers),
        }
    )
    if not environment.get("DB_DSN"):
        log_handle.close()
        raise AcceptanceError("DB_DSN must point at the isolated MySQL fixture")
    pathlib.Path(environment["MEDIA_DIR"]).mkdir(parents=True, exist_ok=True)
    process = subprocess.Popen([binary], stdout=log_handle, stderr=subprocess.STDOUT, env=environment)
    base_url = f"http://{address}"
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        if process.poll() is not None:
            log_handle.close()
            raise AcceptanceError(f"{mode} API process exited during startup; see {log_path}")
        try:
            read_json(f"{base_url}/health")
            read_json(f"{base_url}/ready")
            return process, log_handle, log_path, base_url
        except AcceptanceError:
            time.sleep(0.25)
    process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
    log_handle.close()
    raise AcceptanceError(f"{mode} API did not become healthy within 45 seconds; see {log_path}")


def stop_api(process, log_handle) -> None:
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
            log_handle.close()
            raise AcceptanceError("API process did not stop after SIGTERM")
    log_handle.close()
    if process.returncode != 0:
        raise AcceptanceError(f"API process exited with status {process.returncode}")


def login(base_url: str) -> str:
    result = read_json(
        f"{base_url}/api/sessions",
        payload={"username": "load-u-000100", "password": "load-fixture-password"},
    )
    token = result.get("access_token")
    if not isinstance(token, str) or not token:
        raise AcceptanceError("fixture login returned no access token")
    return token


def collect_api_samples(pid: int, stop: threading.Event, samples: list[tuple[float, int, int]]) -> None:
    while not stop.is_set():
        try:
            cpu, rss = process_stat(pid)
        except (FileNotFoundError, ProcessLookupError):
            return
        samples.append((time.monotonic(), cpu, rss))
        stop.wait(0.5)


def run_load(driver: str, base_url: str, token_file: pathlib.Path, report_path: pathlib.Path, mode: str, round_number: int, args, api_pid: int) -> dict:
    command = [
        driver,
        "-url",
        f"{base_url}/api/feed?scene=following&limit=20",
        "-token-file",
        str(token_file),
        "-concurrency",
        str(A04_CONCURRENCY),
        "-warmup",
        A04_WARMUP,
        "-duration",
        A04_DURATION,
        "-report-file",
        str(report_path),
    ]
    samples: list[tuple[float, int, int]] = []
    stop = threading.Event()
    sampler = threading.Thread(target=collect_api_samples, args=(api_pid, stop, samples), daemon=True)
    sampler.start()
    timeout = A04_WARMUP_SECONDS + A04_DURATION_SECONDS + 45
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=timeout, check=False)
    except subprocess.TimeoutExpired as error:
        (report_path.parent / f"{report_path.stem}.log").write_text(f"load driver timed out: {error}\n")
        raise AcceptanceError(f"{mode} round {round_number} load driver timed out") from error
    finally:
        stop.set()
        sampler.join(timeout=5)
    log_path = report_path.parent / f"{report_path.stem}.log"
    log_path.write_text(result.stdout + result.stderr)
    if result.returncode != 0:
        raise AcceptanceError(f"{mode} round {round_number} had failed HTTP requests; see {log_path}")
    report = json.loads(report_path.read_text())
    report["variant"] = mode
    report["round"] = round_number
    report["warmup_seconds"] = A04_WARMUP_SECONDS
    report["fanout_max_followers"] = 0 if mode == "sql-only" else args.fanout_max_followers
    report["api_process"] = process_metrics(samples)
    report_path.write_text(json.dumps(report, indent=2) + "\n")
    return report


def summarize(reports: list[dict], required_improvement: float) -> dict:
    sql = [report for report in reports if report["variant"] == "sql-only"]
    hybrid = [report for report in reports if report["variant"] == "hybrid"]
    if len(sql) != 3 or len(hybrid) != 3:
        raise AcceptanceError("A04 requires exactly three complete runs for each Feed mode")
    sql_p95 = statistics.median(report["p95_microseconds"] for report in sql)
    hybrid_p95 = statistics.median(report["p95_microseconds"] for report in hybrid)
    improvement = (sql_p95 - hybrid_p95) / sql_p95 if sql_p95 > 0 else 0.0
    return {
        "runs_per_variant": 3,
        "concurrency": A04_CONCURRENCY,
        "warmup_seconds": A04_WARMUP_SECONDS,
        "duration_seconds": A04_DURATION_SECONDS,
        "sql_only_median_p95_microseconds": sql_p95,
        "hybrid_median_p95_microseconds": hybrid_p95,
        "hybrid_p95_improvement_percent": round(improvement * 100, 2),
        "required_improvement_percent": required_improvement * 100,
        "target_met": improvement >= required_improvement,
        "sql_only_successes": [report["successes"] for report in sql],
        "hybrid_successes": [report["successes"] for report in hybrid],
        "sql_only_failures": [report["failures"] for report in sql],
        "hybrid_failures": [report["failures"] for report in hybrid],
        "sql_only_api_cpu_percent_avg": [report["api_process"]["average_cpu_percent"] for report in sql],
        "hybrid_api_cpu_percent_avg": [report["api_process"]["average_cpu_percent"] for report in hybrid],
        "sql_only_api_peak_rss_kib": [report["api_process"]["peak_rss_kib"] for report in sql],
        "hybrid_api_peak_rss_kib": [report["api_process"]["peak_rss_kib"] for report in hybrid],
    }


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-binary", required=True)
    parser.add_argument("--http-driver", required=True)
    parser.add_argument("--report-dir", required=True)
    parser.add_argument("--mysql-container", required=True)
    parser.add_argument("--database", required=True)
    parser.add_argument("--go-bin", default="go")
    parser.add_argument("--port", type=int, default=18086)
    parser.add_argument("--fanout-max-followers", type=int, default=1000)
    args = parser.parse_args()
    if args.fanout_max_followers < 1:
        parser.error("fanout threshold must be positive")
    return args


def main() -> int:
    args = parse_args()
    report_dir = pathlib.Path(args.report_dir)
    report_dir.mkdir(parents=True, exist_ok=True)
    if any(report_dir.iterdir()):
        raise AcceptanceError(f"report directory must be empty: {report_dir}")
    if not pathlib.Path(args.api_binary).is_file() or not pathlib.Path(args.http_driver).is_file():
        raise AcceptanceError("build the API and HTTP load driver before starting A04 acceptance")
    if os.environ.get("DB_DRIVER") not in (None, "mysql"):
        raise AcceptanceError("DB_DRIVER must be mysql")
    if not os.environ.get("DB_DSN"):
        raise AcceptanceError("DB_DSN must point at the isolated MySQL fixture")
    if os.environ.get("GOSTREAM_FEED_A04_JWT_SECRET"):
        jwt_secret = os.environ["GOSTREAM_FEED_A04_JWT_SECRET"]
    else:
        jwt_secret = secrets.token_urlsafe(48)
    args.jwt_secret = jwt_secret

    version = subprocess.run([args.go_bin, "version"], capture_output=True, text=True, check=False)
    if version.returncode != 0 or "go1.27.2" not in version.stdout:
        raise AcceptanceError(f"A04 acceptance requires Go 1.27.2; got {version.stdout.strip()}")
    (report_dir / "environment.txt").write_text(host_evidence(args.go_bin, args.mysql_container, args.database))

    reports: list[dict] = []
    expected_ids: list[int] | None = None
    schedule = (("sql-only", 1), ("hybrid", 1), ("hybrid", 2), ("sql-only", 2), ("sql-only", 3), ("hybrid", 3))
    with tempfile.TemporaryDirectory(prefix="gostream-feed-a04-") as temporary:
        work_dir = pathlib.Path(temporary)
        token_path = work_dir / "feed-token"
        try:
            for mode, round_number in schedule:
                api, api_log, api_log_path, base_url = start_api(
                    args.api_binary, report_dir, work_dir, mode, round_number, args
                )
                try:
                    token = login(base_url)
                    if not token_path.exists():
                        token_path.write_text(token)
                        token_path.chmod(0o600)
                    page_ids = following_page_ids(base_url, token)
                    if expected_ids is None:
                        expected_ids = page_ids
                    elif expected_ids != page_ids:
                        raise AcceptanceError(f"{mode} round {round_number} returned different first-two-page IDs")
                    report_path = report_dir / f"{mode}-round-{round_number}.json"
                    reports.append(
                        run_load(args.http_driver, base_url, token_path, report_path, mode, round_number, args, api.pid)
                    )
                except Exception:
                    if api.poll() is None:
                        api.terminate()
                        try:
                            api.wait(timeout=15)
                        except subprocess.TimeoutExpired:
                            api.kill()
                            api.wait(timeout=5)
                    api_log.close()
                    raise
                else:
                    stop_api(api, api_log)

            summary = summarize(reports, A04_REQUIRED_IMPROVEMENT)
            summary["compared_page_ids"] = len(expected_ids or [])
            summary["all_load_runs_succeeded"] = all(
                report["failures"] == 0 and report["successes"] > 0 for report in reports
            )
            (report_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
            (report_dir / "summary.md").write_text(
                "# A04 Feed acceptance\n\n"
                f"- SQL-only median P95: {summary['sql_only_median_p95_microseconds'] / 1000:.3f} ms\n"
                f"- Hybrid median P95: {summary['hybrid_median_p95_microseconds'] / 1000:.3f} ms\n"
                f"- Hybrid P95 improvement: {summary['hybrid_p95_improvement_percent']:.2f}%\n"
                f"- Required improvement: {summary['required_improvement_percent']:.0f}%\n"
                f"- Load profile: {summary['concurrency']} workers, {summary['warmup_seconds']}s warmup, "
                f"{summary['duration_seconds']}s measured per run\n"
                f"- Target met: {summary['target_met']}\n"
                f"- First two pages matched: {summary['compared_page_ids']} unique IDs\n"
                "- Raw per-run latency samples and process CPU/RSS are in the adjacent JSON files.\n"
            )
            print((report_dir / "summary.md").read_text())
            return 0 if summary["target_met"] and summary["all_load_runs_succeeded"] else 1
        except Exception:
            if reports:
                partial = {"completed_runs": len(reports), "reports": reports}
                (report_dir / "partial-summary.json").write_text(json.dumps(partial, indent=2) + "\n")
            raise


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AcceptanceError as error:
        print(f"A04 acceptance failed: {error}", file=sys.stderr)
        raise SystemExit(1)
