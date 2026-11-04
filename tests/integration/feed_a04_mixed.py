#!/usr/bin/env python3
"""Run six isolated full-size MySQL A04 mixed-load comparisons."""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import math
import os
import pathlib
import random
import re
import secrets
import signal
import shutil
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

from feed_a04 import AcceptanceError, host_evidence, process_metrics, process_stat


CONCURRENCY = 20
WARMUP_SECONDS = 30
DURATION_SECONDS = 60
PUBLISHERS = 20
DRIVER_ACTORS = CONCURRENCY
FOLLOWERS = 20
# Keep each batch within POST /api/sessions' per-IP budget in internal/platform/server.go.
LOGIN_RATE_BUDGET = 30
FANOUT_DEADLINE_SECONDS = 60
REQUIRED_IMPROVEMENT = 0.20
DEFAULT_SCHEDULE_SEED = 0
PROFILE_SCHEDULE = (("sql-only", 4), ("hybrid", 4))


def acceptance_schedule(seed: int) -> tuple[tuple[str, int], ...]:
    """Return three adjacent mode pairs with deterministic, counterbalanced order."""
    rng = random.Random(seed)
    first_mode = rng.choice(("sql-only", "hybrid"))
    second_mode = "hybrid" if first_mode == "sql-only" else "sql-only"
    first_modes = [first_mode, first_mode, second_mode]
    rng.shuffle(first_modes)
    return tuple(
        (mode, round_number)
        for round_number, first in enumerate(first_modes, 1)
        for mode in (first, second_mode if first == first_mode else first_mode)
    )


def percentile(values: list[int], numerator: int) -> int:
    ordered = sorted(values)
    if not ordered:
        return 0
    return ordered[(len(ordered) - 1) * numerator // 100]


def driver_reader_names() -> list[str]:
    return [f"load-u-{500 + index:06d}" for index in range(DRIVER_ACTORS)]


def publisher_login_batches(
    reader_count: int, publisher_names: list[str], verifier_count: int
) -> tuple[list[str], list[str]]:
    if reader_count < 0 or verifier_count < 0:
        raise AcceptanceError("login counts cannot be negative")
    preload_count = max(0, LOGIN_RATE_BUDGET - reader_count)
    before_load = publisher_names[:preload_count]
    after_load = publisher_names[preload_count:]
    if reader_count + len(before_load) > LOGIN_RATE_BUDGET:
        raise AcceptanceError("reader and publisher logins exceed the pre-load login budget")
    if len(after_load) + verifier_count > LOGIN_RATE_BUDGET:
        raise AcceptanceError("deferred publisher and verifier logins exceed the post-load login budget")
    return before_load, after_load


def summarize(reports: list[dict], required_improvement: float, interpolate_params: bool = False) -> dict:
    sql = [report for report in reports if report["variant"] == "sql-only"]
    hybrid = [report for report in reports if report["variant"] == "hybrid"]
    if len(sql) != 3 or len(hybrid) != 3:
        raise AcceptanceError("A04 mixed acceptance requires three complete runs per mode")
    sql_p95 = statistics.median(
        report["mixed_load"]["operations"]["feed_following"]["p95_ns"] for report in sql
    )
    hybrid_p95 = statistics.median(
        report["mixed_load"]["operations"]["feed_following"]["p95_ns"] for report in hybrid
    )
    server_following_latency = {
        "sql_only": [report["mixed_load"].get("server_following_latency") for report in sql],
        "hybrid": [report["mixed_load"].get("server_following_latency") for report in hybrid],
    }
    server_feed_stages = {
        "sql_only": [report["mixed_load"].get("server_feed_stages", []) for report in sql],
        "hybrid": [report["mixed_load"].get("server_feed_stages", []) for report in hybrid],
    }
    database_pool_wait = {
        "sql_only": [report["mixed_load"].get("database_pool_wait") for report in sql],
        "hybrid": [report["mixed_load"].get("database_pool_wait") for report in hybrid],
    }
    database_statement_diagnostics = {
        "sql_only": [report.get("database_statement_diagnostics") for report in sql],
        "hybrid": [report.get("database_statement_diagnostics") for report in hybrid],
    }
    database_statement_diagnostics_warmup = {
        "sql_only": [report.get("database_statement_diagnostics_warmup") for report in sql],
        "hybrid": [report.get("database_statement_diagnostics_warmup") for report in hybrid],
    }
    database_statement_diagnostics_measured = {
        "sql_only": [report.get("database_statement_diagnostics_measured") for report in sql],
        "hybrid": [report.get("database_statement_diagnostics_measured") for report in hybrid],
    }
    database_wait_diagnostics = {
        "sql_only": [report.get("database_wait_diagnostics") for report in sql],
        "hybrid": [report.get("database_wait_diagnostics") for report in hybrid],
    }
    database_wait_diagnostics_warmup = {
        "sql_only": [report.get("database_wait_diagnostics_warmup") for report in sql],
        "hybrid": [report.get("database_wait_diagnostics_warmup") for report in hybrid],
    }
    database_wait_diagnostics_measured = {
        "sql_only": [report.get("database_wait_diagnostics_measured") for report in sql],
        "hybrid": [report.get("database_wait_diagnostics_measured") for report in hybrid],
    }
    prepared_statement_diagnostics = {
        "sql_only": [
            report.get("database_statement_diagnostics", {}).get("prepared_statement_diagnostics")
            for report in sql
        ],
        "hybrid": [
            report.get("database_statement_diagnostics", {}).get("prepared_statement_diagnostics")
            for report in hybrid
        ],
    }
    diagnostic_profiles = [
        {
            "variant": report["variant"],
            "round": report["round"],
            **report["mixed_load"]["diagnostic_profiles"],
        }
        for report in reports
        if report["mixed_load"].get("diagnostic_profiles")
    ]
    following_query_plan_diagnostics = {
        "sql_only": [report.get("following_query_plan_diagnostic") for report in sql],
        "hybrid": [report.get("following_query_plan_diagnostic") for report in hybrid],
    }
    improvement = (sql_p95 - hybrid_p95) / sql_p95 if sql_p95 > 0 else 0.0
    all_consistent = all(
        report["verification"]["counter_mismatches"] == 0
        and report["verification"]["feed_visibility_failures"] == 0
        and report["verification"]["outbox_pending"] == 0
        and report["verification"]["outbox_dead"] == 0
        and report["fanout"]["within_deadline"]
        and report["fanout"]["jobs_completed"] == (PUBLISHERS if report["variant"] == "hybrid" else 0)
        and report["mixed_load"]["failures"] == 0
        and report["publish"]["failures"] == 0
        and report["publish"]["successes"] == PUBLISHERS
        for report in reports
    )
    return {
        "runs_per_variant": 3,
        "concurrency": CONCURRENCY,
        "warmup_seconds": WARMUP_SECONDS,
        "duration_seconds": DURATION_SECONDS,
        "mysql_interpolate_params": interpolate_params,
        "sql_only_median_following_p95_ns": sql_p95,
        "hybrid_median_following_p95_ns": hybrid_p95,
        "server_following_latency": server_following_latency,
        "server_feed_stages": server_feed_stages,
        "database_pool_wait": database_pool_wait,
        "database_statement_diagnostics": database_statement_diagnostics,
        "database_statement_diagnostics_warmup": database_statement_diagnostics_warmup,
        "database_statement_diagnostics_measured": database_statement_diagnostics_measured,
        "database_wait_diagnostics": database_wait_diagnostics,
        "database_wait_diagnostics_warmup": database_wait_diagnostics_warmup,
        "database_wait_diagnostics_measured": database_wait_diagnostics_measured,
        "prepared_statement_diagnostics": prepared_statement_diagnostics,
        "following_query_plan_diagnostics": following_query_plan_diagnostics,
        "diagnostic_profiles": diagnostic_profiles,
        "following_p95_improvement_percent": round(improvement * 100, 2),
        "required_improvement_percent": required_improvement * 100,
        "publishes_per_run": PUBLISHERS,
        "sql_only_publish_p95_ns": [report["publish"]["p95_ns"] for report in sql],
        "hybrid_publish_p95_ns": [report["publish"]["p95_ns"] for report in hybrid],
        "fanout_drain_seconds": [report["fanout"]["drain_seconds"] for report in reports],
        "hybrid_fanout_drain_seconds": [report["fanout"]["drain_seconds"] for report in hybrid],
        "fanout_jobs_completed": [report["fanout"]["jobs_completed"] for report in reports],
        "load_successes": [report["mixed_load"]["successes"] for report in reports],
        "load_failures": [report["mixed_load"]["failures"] for report in reports],
        "publish_failures": [report["publish"]["failures"] for report in reports],
        "api_cpu_percent_avg": [report["api_process"]["average_cpu_percent"] for report in reports],
        "api_peak_rss_kib": [report["api_process"]["peak_rss_kib"] for report in reports],
        "all_runs_consistent": all_consistent,
        "target_met": improvement >= required_improvement and all_consistent,
    }


def format_summary(summary: dict) -> str:
    def format_server_buckets(variant: str) -> str:
        buckets = summary["server_following_latency"][variant]
        if not buckets or any(bucket is None for bucket in buckets):
            return "unavailable"
        values = []
        for round_number, bucket in enumerate(buckets, 1):
            lower = bucket["p95_bucket_lower_bound_seconds"] * 1000
            upper_value = bucket.get("p95_bucket_upper_bound_seconds")
            upper = f"{upper_value * 1000:.1f}" if upper_value is not None else "+Inf"
            values.append(f"r{round_number} ({lower:.1f}, {upper}] ms (n={bucket['sample_count']})")
        return ", ".join(values)

    def format_stage_buckets(variant: str, stage: str) -> str:
        runs = summary["server_feed_stages"][variant]
        values = []
        for round_number, reports in enumerate(runs, 1):
            report = next(
                (
                    entry
                    for entry in reports
                    if entry.get("scene") == "following"
                    and entry.get("stage") == stage
                    and entry.get("outcome") == "success"
                ),
                None,
            )
            if report is None:
                return "unavailable"
            latency = report["latency"]
            lower = latency["p95_bucket_lower_bound_seconds"] * 1000
            upper_value = latency.get("p95_bucket_upper_bound_seconds")
            upper = f"{upper_value * 1000:.1f}" if upper_value is not None else "+Inf"
            values.append(f"r{round_number} ({lower:.1f}, {upper}] ms (n={latency['sample_count']})")
        return ", ".join(values) if values else "unavailable"

    def format_database_pool_wait(variant: str) -> str:
        runs = summary.get("database_pool_wait", {}).get(variant, [])
        values = []
        for round_number, report in enumerate(runs, 1):
            if report is None:
                values.append(f"r{round_number} unavailable")
                continue
            values.append(
                f"r{round_number} {report['wait_count']} / {report['wait_duration_seconds']:.3f}s / "
                f"{report.get('max_idle_closed', 0)} max-idle closures"
            )
        return ", ".join(values) if values else "unavailable"

    def format_statement_diagnostics(variant: str, field: str = "database_statement_diagnostics") -> str:
        runs = summary.get(field, {}).get(variant, [])
        values = []
        for round_number, report in enumerate(runs, 1):
            if not report or not report.get("available"):
                reason = report.get("reason", "snapshot_unavailable") if report else "snapshot_unavailable"
                failure_stage = report.get("failure_stage") if report else None
                detail = f"; stage={failure_stage}" if failure_stage else ""
                values.append(f"r{round_number} unavailable ({reason}{detail})")
                continue
            top = report.get("top_statements", [])[:3]
            if not top:
                values.append(f"r{round_number} no statements captured")
                continue
            statements = []
            for statement in top:
                normalized = " ".join(statement.get("statement", "").split()).replace("|", "\\|")
                if len(normalized) > 100:
                    normalized = normalized[:97] + "..."
                statements.append(
                    f"{normalized} (n={statement['count']}, mean={statement['mean_timer_wait_ms']:.3f}ms)"
                )
            values.append(f"r{round_number} " + "; ".join(statements))
        return ", ".join(values) if values else "unavailable"

    def format_wait_event_diagnostics(variant: str, field: str = "database_wait_diagnostics") -> str:
        runs = summary.get(field, {}).get(variant, [])
        values = []
        for round_number, report in enumerate(runs, 1):
            if not report or not report.get("available"):
                reason = report.get("reason", "snapshot_unavailable") if report else "snapshot_unavailable"
                failure_stage = report.get("failure_stage") if report else None
                detail = f"; stage={failure_stage}" if failure_stage else ""
                values.append(f"r{round_number} unavailable ({reason}{detail})")
                continue
            rendered = []
            for event in report.get("events", [])[:3]:
                name = event["event_name"].replace("|", "\\|")
                if len(name) > 84:
                    name = name[:81] + "..."
                rendered.append(f"{name} (n={event['count']}, wait={event['sum_timer_wait_ms']:.3f}ms)")
            if not rendered:
                rendered.append("no instrumented wait events captured")
            idle_event = report.get("idle_connection_event")
            if idle_event:
                rendered.append(
                    "idle connection (separate; excluded from ranked waits) "
                    f"(n={idle_event['count']}, wait={idle_event['sum_timer_wait_ms']:.3f}ms)"
                )
            values.append(f"r{round_number} " + "; ".join(rendered))
        return ", ".join(values) if values else "unavailable"

    def format_prepared_statement_diagnostics(variant: str) -> str:
        runs = summary.get("prepared_statement_diagnostics", {}).get(variant, [])
        values = []
        for round_number, report in enumerate(runs, 1):
            if not report or not report.get("available"):
                reason = report.get("reason", "snapshot_unavailable") if report else "snapshot_unavailable"
                failure_stage = report.get("failure_stage") if report else None
                detail = f"; stage={failure_stage}" if failure_stage else ""
                values.append(f"r{round_number} unavailable ({reason}{detail})")
                continue
            count = report["prepared_execute_count"]
            total = report["prepared_execute_sum_timer_wait_ms"]
            mean = report["prepared_execute_mean_timer_wait_ms"]
            mean_text = f"{mean:.3f}ms" if mean is not None else "n/a"
            values.append(
                f"r{round_number} Execute n={count}, wait={total:.3f}ms, mean={mean_text}"
            )
        return ", ".join(values) if values else "unavailable"

    def format_diagnostic_profiles() -> str:
        profiles = summary.get("diagnostic_profiles", [])
        if not profiles:
            return "not captured"
        values = []
        for profile in profiles:
            if profile.get("run_error"):
                values.append(f"{profile['variant']} r{profile['round']} run unavailable ({profile['run_error']})")
                continue
            cpu = "captured" if profile.get("cpu", {}).get("captured") else "unavailable"
            goroutine = "captured" if profile.get("goroutine", {}).get("captured") else "unavailable"
            values.append(f"{profile['variant']} r{profile['round']} CPU={cpu}, goroutine={goroutine}")
        return "; ".join(values)

    def format_query_plan_diagnostics(variant: str) -> str:
        reports = summary.get("following_query_plan_diagnostics", {}).get(variant, [])
        values = []
        for round_number, report in enumerate(reports, 1):
            if not report or not report.get("available"):
                reason = report.get("reason", "not_scheduled") if report else "not_scheduled"
                values.append(f"r{round_number} unavailable ({reason})")
            else:
                values.append(f"r{round_number} {report['output_file']}")
        return ", ".join(values) if values else "not captured"

    statement_window = next(
        (
            report.get("window")
            for variant in ("sql_only", "hybrid")
            for report in summary.get("database_statement_diagnostics", {}).get(variant, [])
            if report and report.get("window")
        ),
        "driver lifetime; diagnostic only",
    )

    schedule_text = ", ".join(
        "{} r{}".format(entry["mode"], entry["round"])
        for entry in summary.get("schedule", [])
    )
    lines = [
        "# A04 mixed Feed acceptance",
        "",
        f"- Schedule seed: {summary.get('schedule_seed', 'not recorded')}",
        f"- Acceptance run order: {schedule_text or 'not recorded'}",
        f"- SQL-only median following Feed P95: {summary['sql_only_median_following_p95_ns'] / 1_000_000:.3f} ms",
        f"- Hybrid median following Feed P95: {summary['hybrid_median_following_p95_ns'] / 1_000_000:.3f} ms",
        f"- Hybrid P95 improvement under mixed writes: {summary['following_p95_improvement_percent']:.2f}%",
        f"- Required improvement: {summary['required_improvement_percent']:.0f}%",
        f"- Load profile: {CONCURRENCY} workers, {WARMUP_SECONDS}s warmup, {DURATION_SECONDS}s measured per run; three fresh fixtures per mode",
        f"- MySQL interpolateParams: {'enabled' if summary.get('mysql_interpolate_params') else 'disabled'}",
        f"- {summary['publishes_per_run']} real API video publishes per run; maximum Hybrid fanout drain: {max(summary['hybrid_fanout_drain_seconds']):.3f}s",
        f"- Counter, follower Feed visibility, outbox and fanout consistency: {summary['all_runs_consistent']}",
        f"- API-side successful following P95 bucket (approximate): SQL-only {format_server_buckets('sql_only')}; Hybrid {format_server_buckets('hybrid')}",
        f"- API-side following candidate_query P95 bucket (approximate): SQL-only {format_stage_buckets('sql_only', 'candidate_query')}; Hybrid {format_stage_buckets('hybrid', 'candidate_query')}",
        f"- API-side following candidate_sql P95 bucket (approximate): SQL-only {format_stage_buckets('sql_only', 'candidate_sql')}; Hybrid {format_stage_buckets('hybrid', 'candidate_sql')}",
        f"- API-side following candidate_query_start P95 bucket (approximate): SQL-only {format_stage_buckets('sql_only', 'candidate_query_start')}; Hybrid {format_stage_buckets('hybrid', 'candidate_query_start')}",
        f"- API-side following candidate_row_next P95 bucket (approximate): SQL-only {format_stage_buckets('sql_only', 'candidate_row_next')}; Hybrid {format_stage_buckets('hybrid', 'candidate_row_next')}",
        f"- API-side following candidate_row_scan P95 bucket (approximate): SQL-only {format_stage_buckets('sql_only', 'candidate_row_scan')}; Hybrid {format_stage_buckets('hybrid', 'candidate_row_scan')}",
        f"- API-side following author_preload P95 bucket (approximate): SQL-only {format_stage_buckets('sql_only', 'author_preload')}; Hybrid {format_stage_buckets('hybrid', 'author_preload')}",
        f"- API-side following interaction_state P95 bucket (approximate): SQL-only {format_stage_buckets('sql_only', 'interaction_state')}; Hybrid {format_stage_buckets('hybrid', 'interaction_state')}",
        f"- DB pool delta during 60s measured window (waits / wait seconds / connections closed by idle cap): SQL-only {format_database_pool_wait('sql_only')}; Hybrid {format_database_pool_wait('hybrid')}",
        f"- Performance Schema statement digests ({statement_window}; available SQL digests only): SQL-only {format_statement_diagnostics('sql_only')}; Hybrid {format_statement_diagnostics('hybrid')}",
        f"- Performance Schema warmup-only statement digests: SQL-only {format_statement_diagnostics('sql_only', 'database_statement_diagnostics_warmup')}; Hybrid {format_statement_diagnostics('hybrid', 'database_statement_diagnostics_warmup')}",
        f"- Performance Schema measured-only statement digests: SQL-only {format_statement_diagnostics('sql_only', 'database_statement_diagnostics_measured')}; Hybrid {format_statement_diagnostics('hybrid', 'database_statement_diagnostics_measured')}",
        f"- Performance Schema account waits (total window): SQL-only {format_wait_event_diagnostics('sql_only')}; Hybrid {format_wait_event_diagnostics('hybrid')}",
        f"- Performance Schema account waits (warmup-only): SQL-only {format_wait_event_diagnostics('sql_only', 'database_wait_diagnostics_warmup')}; Hybrid {format_wait_event_diagnostics('hybrid', 'database_wait_diagnostics_warmup')}",
        f"- Performance Schema account waits (measured-only): SQL-only {format_wait_event_diagnostics('sql_only', 'database_wait_diagnostics_measured')}; Hybrid {format_wait_event_diagnostics('hybrid', 'database_wait_diagnostics_measured')}",
        f"- Performance Schema prepared COM_STMT_EXECUTE aggregates (account scoped; command-level, not per-SQL; diagnostic only): SQL-only {format_prepared_statement_diagnostics('sql_only')}; Hybrid {format_prepared_statement_diagnostics('hybrid')}",
        f"- Production Following EXPLAIN FORMAT=JSON plans (captured after Performance Schema snapshots; diagnostic only): SQL-only {format_query_plan_diagnostics('sql_only')}; Hybrid {format_query_plan_diagnostics('hybrid')}",
        f"- Go pprof diagnostics (separate from the 6-run acceptance gate): {format_diagnostic_profiles()}",
        f"- Target met: {summary['target_met']}",
        "- Client-side P95 remains the acceptance gate. Server histogram buckets are diagnostic intervals; raw request latencies, samples, queue state, MySQL status, and API CPU/RSS are in adjacent JSON reports.",
        "",
    ]
    return "\n".join(lines)


def request_json(
    url: str,
    method: str = "GET",
    token: str | None = None,
    payload: dict | None = None,
    expected_status: int = 200,
) -> dict:
    headers = {"Accept": "application/json"}
    data = None
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            if response.status != expected_status:
                raise AcceptanceError(
                    f"{method} {urllib.parse.urlsplit(url).path} expected HTTP {expected_status}, "
                    f"got {response.status}"
                )
            raw = response.read()
            return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as error:
        body = error.read(512).decode(errors="replace")
        raise AcceptanceError(
            f"{method} {urllib.parse.urlsplit(url).path} returned HTTP {error.code}: {body}"
        ) from error
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as error:
        raise AcceptanceError(f"{method} {urllib.parse.urlsplit(url).path} failed: {error}") from error


def mysql_query(container: str, sql: str) -> list[list[str]]:
    command = [
        "docker",
        "exec",
        "-i",
        container,
        "sh",
        "-c",
        'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --raw --skip-column-names',
    ]
    result = subprocess.run(command, input=sql, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        raise AcceptanceError(f"isolated MySQL query failed: {result.stderr.strip()}")
    return [line.split("\t") for line in result.stdout.splitlines() if line]


def scalar(container: str, sql: str) -> int:
    rows = mysql_query(container, sql)
    if len(rows) != 1 or len(rows[0]) != 1:
        raise AcceptanceError(f"expected one scalar from MySQL; got {rows!r}")
    return int(rows[0][0])


def status_snapshot(container: str) -> dict[str, int]:
    rows = mysql_query(
        container,
        "SHOW GLOBAL STATUS WHERE Variable_name IN "
        "('Threads_connected','Threads_running','Questions','Innodb_row_lock_time','Innodb_row_lock_waits');",
    )
    return {row[0]: int(row[1]) for row in rows}


STATEMENT_DIGEST_COUNTERS = (
    "count_star",
    "sum_timer_wait_ps",
    "sum_lock_time_ps",
    "sum_rows_sent",
    "sum_rows_examined",
    "sum_no_index_used",
    "sum_no_good_index_used",
)

ACCOUNT_STATEMENT_EVENT_COUNTERS = (
    "count_star",
    "sum_timer_wait_ps",
    "sum_lock_time_ps",
    "sum_rows_sent",
    "sum_rows_examined",
    "sum_no_index_used",
    "sum_no_good_index_used",
)


def parse_statement_digest_snapshot(summary_rows: list[list[str]], histogram_rows: list[list[str]]) -> dict:
    statements = {}
    for row in summary_rows:
        if len(row) != 10:
            raise AcceptanceError(f"unexpected statement digest row with {len(row)} columns")
        schema_name, digest = row[:2]
        identity = (schema_name, digest)
        if not schema_name or not digest or identity in statements:
            raise AcceptanceError(
                "statement digest snapshot contains a blank or duplicate identity: "
                f"schema={schema_name!r} digest={digest}"
            )
        try:
            statement = bytes.fromhex(row[2]).decode("utf-8") if row[2] else ""
            counters = [int(value) for value in row[3:]]
        except (UnicodeDecodeError, ValueError) as error:
            raise AcceptanceError(f"invalid statement digest counters or text: {error}") from error
        if any(value < 0 for value in counters):
            raise AcceptanceError("statement digest counters must be non-negative")
        statements[identity] = {
            "schema_name": schema_name,
            "digest": digest,
            "statement": statement,
            **dict(zip(STATEMENT_DIGEST_COUNTERS, counters)),
        }

    histograms: dict[tuple[str, str], dict[int, dict[str, int]]] = {}
    for row in histogram_rows:
        if len(row) != 6:
            raise AcceptanceError(f"unexpected statement histogram row with {len(row)} columns")
        schema_name, digest = row[:2]
        try:
            bucket_number, low_ps, high_ps, count = map(int, row[2:])
        except ValueError as error:
            raise AcceptanceError(f"invalid statement histogram counter: {error}") from error
        if (
            not schema_name
            or not digest
            or bucket_number < 0
            or low_ps < 0
            or high_ps <= low_ps
            or count < 0
        ):
            raise AcceptanceError("statement histogram bucket values are invalid")
        identity = (schema_name, digest)
        buckets = histograms.setdefault(identity, {})
        if bucket_number in buckets:
            previous = buckets[bucket_number]
            current = {"low_ps": low_ps, "high_ps": high_ps, "count": count}
            if previous == current:
                # A live A04 run returned identical rows for the same bucket. Exact repeats
                # carry no additional count; conflicting observations remain an error.
                continue
            raise AcceptanceError(
                "statement histogram snapshot contains a conflicting duplicate bucket: "
                f"schema={schema_name!r} digest={digest} bucket={bucket_number} "
                f"existing=({previous['low_ps']},{previous['high_ps']},{previous['count']}) "
                f"current=({low_ps},{high_ps},{count})"
            )
        buckets[bucket_number] = {"low_ps": low_ps, "high_ps": high_ps, "count": count}
    return {"statements": statements, "histograms": histograms}


def redact_diagnostic_error(error: Exception) -> str:
    """Keep useful Performance Schema failure context without persisting credentials."""
    message = " ".join(str(error).split())
    message = re.sub(r"(?i)\bMYSQL_PWD\s*=\s*[^\s,;]+", "MYSQL_PWD=<REDACTED>", message)
    message = re.sub(r"(?i)\b(password|passwd|pwd)\s*[:=]\s*[^\s,;]+", r"\1=<REDACTED>", message)
    message = re.sub(r"(?i)\b(token|secret|api[_-]?key)\s*[:=]\s*[^\s,;]+", r"\1=<REDACTED>", message)
    message = re.sub(r"(?i)\bdsn\s*=\s*[^\s,;]+", "dsn=<REDACTED>", message)
    message = re.sub(r"(?i)\bBearer\s+[^\s,;]+", "Bearer <REDACTED>", message)
    message = "".join(character for character in message if character.isprintable())
    if len(message) > 500:
        message = message[:497] + "..."
    return f"{type(error).__name__}: {message}"


def diagnostic_failure(reason: str, stage: str, error: Exception) -> dict:
    return {
        "available": False,
        "reason": reason,
        "failure_stage": stage,
        "error": redact_diagnostic_error(error),
    }


def parse_account_statement_event_snapshot(rows: list[list[str]]) -> dict[str, dict[str, int]]:
    events = {}
    for row in rows:
        if len(row) != 8:
            raise AcceptanceError(f"unexpected account statement event row with {len(row)} columns")
        event_name = row[0]
        if not event_name or event_name in events:
            raise AcceptanceError("duplicate account statement event name")
        if not (event_name.startswith("statement/com/") or event_name.startswith("statement/sql/")):
            raise AcceptanceError("unexpected account statement event class")
        try:
            counters = [int(value) for value in row[1:]]
        except ValueError as error:
            raise AcceptanceError(f"invalid account statement event counter: {error}") from error
        if any(value < 0 for value in counters):
            raise AcceptanceError("account statement event counters must be non-negative")
        events[event_name] = dict(zip(ACCOUNT_STATEMENT_EVENT_COUNTERS, counters))
    return events


def parse_account_wait_event_snapshot(rows: list[list[str]]) -> dict[str, dict[str, int]]:
    events = {}
    for row in rows:
        if len(row) != 3:
            raise AcceptanceError(f"unexpected account wait event row with {len(row)} columns")
        event_name = row[0]
        if not event_name or event_name in events:
            raise AcceptanceError("duplicate account wait event name")
        if event_name != "idle" and not event_name.startswith("wait/"):
            raise AcceptanceError("unexpected account wait event class")
        try:
            count_star, sum_timer_wait_ps = map(int, row[1:])
        except ValueError as error:
            raise AcceptanceError(f"invalid account wait event counter: {error}") from error
        if count_star < 0 or sum_timer_wait_ps < 0:
            raise AcceptanceError("account wait event counters must be non-negative")
        events[event_name] = {
            "count_star": count_star,
            "sum_timer_wait_ps": sum_timer_wait_ps,
        }
    return events


def account_wait_event_delta(before: dict, after: dict, account_user: str) -> dict:
    for event_name, old in before.items():
        if event_name not in after and any(old.get(field, 0) for field in ("count_star", "sum_timer_wait_ps")):
            raise AcceptanceError("Performance Schema account wait event counter reset")

    deltas = []
    for event_name, current in after.items():
        old = before.get(event_name, {})
        count = current.get("count_star", 0) - old.get("count_star", 0)
        timer_wait_ps = current.get("sum_timer_wait_ps", 0) - old.get("sum_timer_wait_ps", 0)
        if count < 0 or timer_wait_ps < 0:
            raise AcceptanceError("Performance Schema account wait event counter reset")
        if count == 0 and timer_wait_ps == 0:
            continue
        deltas.append({
            "event_name": event_name,
            "count": count,
            "sum_timer_wait_ms": timer_wait_ps / 1_000_000_000,
            "mean_timer_wait_ms": timer_wait_ps / count / 1_000_000_000 if count else None,
        })
    deltas.sort(key=lambda item: (-item["sum_timer_wait_ms"], -item["count"], item["event_name"]))
    idle_connection_event = next((item for item in deltas if item["event_name"] == "idle"), None)
    wait_events = [item for item in deltas if item["event_name"] != "idle"]
    return {
        "available": True,
        "source": "performance_schema.events_waits_summary_by_account_by_event_name",
        "scope": "account user aggregated across all hosts; idle connection time is separate from ranked waits",
        "account_user": account_user,
        "event_count_total": sum(item["count"] for item in wait_events),
        "sum_timer_wait_ms": sum(item["sum_timer_wait_ms"] for item in wait_events),
        "all_event_count_total": sum(item["count"] for item in deltas),
        "all_sum_timer_wait_ms": sum(item["sum_timer_wait_ms"] for item in deltas),
        "idle_connection_event": idle_connection_event,
        "events": wait_events[:50],
    }


def performance_schema_wait_event_snapshot(container: str, account_user: str) -> dict:
    if not re.fullmatch(r"[A-Za-z0-9_]+", account_user):
        raise AcceptanceError("refusing Performance Schema wait query with an invalid username")
    try:
        if scalar(container, "SELECT @@performance_schema;") != 1:
            return {"available": False, "reason": "performance_schema_disabled"}
        rows = mysql_query(
            container,
            "SELECT EVENT_NAME, COALESCE(SUM(COUNT_STAR), 0), COALESCE(SUM(SUM_TIMER_WAIT), 0) "
            "FROM performance_schema.events_waits_summary_by_account_by_event_name "
            f"WHERE USER='{account_user}' GROUP BY EVENT_NAME ORDER BY EVENT_NAME;",
        )
        return {
            "available": True,
            "account_user": account_user,
            "snapshot": parse_account_wait_event_snapshot(rows),
        }
    except Exception as error:
        return diagnostic_failure("performance_schema_wait_query_failed", "wait_event_query", error)


def performance_schema_wait_event_delta(before: dict, after: dict, window: str) -> dict:
    result = {
        "available": False,
        "reason": before.get("reason") or after.get("reason") or "snapshot_unavailable",
    }
    failed_snapshot = before if not before.get("available") else after if not after.get("available") else None
    if failed_snapshot:
        for field in ("failure_stage", "error"):
            if failed_snapshot.get(field):
                result[field] = failed_snapshot[field]
    else:
        try:
            if before.get("account_user") != after.get("account_user"):
                raise AcceptanceError("Performance Schema wait account user changed between snapshots")
            result = account_wait_event_delta(before["snapshot"], after["snapshot"], after["account_user"])
        except (AcceptanceError, KeyError) as error:
            result = diagnostic_failure(
                "performance_schema_wait_counter_reset_or_invalid_snapshot", "wait_event_delta", error
            )
    result["window"] = window
    return result


def prepared_statement_event_delta(before: dict, after: dict, account_user: str) -> dict:
    deltas = []
    for event_name, old in before.items():
        if event_name not in after and any(old.get(field, 0) for field in ACCOUNT_STATEMENT_EVENT_COUNTERS):
            raise AcceptanceError("Performance Schema account statement event counter reset")

    for event_name, current in after.items():
        old = before.get(event_name, {})
        difference = {}
        for field in ACCOUNT_STATEMENT_EVENT_COUNTERS:
            old_value = old.get(field, 0)
            current_value = current.get(field, 0)
            if current_value < old_value:
                raise AcceptanceError("Performance Schema account statement event counter reset")
            difference[field] = current_value - old_value
        if difference["count_star"] == 0:
            continue
        deltas.append({
            "event_name": event_name,
            "count": difference["count_star"],
            "sum_timer_wait_ms": difference["sum_timer_wait_ps"] / 1_000_000_000,
            "mean_timer_wait_ms": difference["sum_timer_wait_ps"] / difference["count_star"] / 1_000_000_000,
            "sum_lock_time_ms": difference["sum_lock_time_ps"] / 1_000_000_000,
            "sum_rows_sent": difference["sum_rows_sent"],
            "sum_rows_examined": difference["sum_rows_examined"],
            "sum_no_index_used": difference["sum_no_index_used"],
            "sum_no_good_index_used": difference["sum_no_good_index_used"],
        })
    deltas.sort(key=lambda item: (-item["sum_timer_wait_ms"], -item["count"], item["event_name"]))
    execute = next((item for item in deltas if item["event_name"] == "statement/com/Execute"), None)
    execute_count = execute["count"] if execute else 0
    execute_sum_wait_ms = execute["sum_timer_wait_ms"] if execute else 0.0
    return {
        "available": True,
        "source": "performance_schema.events_statements_summary_by_account_by_event_name",
        "scope": "account user aggregated by statement event class across the MySQL server",
        "account_user": account_user,
        "event_count_total": sum(item["count"] for item in deltas),
        "prepared_execute_count": execute_count,
        "prepared_execute_sum_timer_wait_ms": execute_sum_wait_ms,
        "prepared_execute_mean_timer_wait_ms": execute_sum_wait_ms / execute_count if execute_count else None,
        "events": deltas[:30],
    }


def _statement_percentile_bucket(buckets: dict[int, dict[str, int]], count: int, quantile: float):
    if count <= 0 or sum(bucket["count"] for bucket in buckets.values()) != count:
        return None
    target = math.ceil(count * quantile)
    cumulative = 0
    for bucket_number in sorted(buckets):
        bucket = buckets[bucket_number]
        cumulative += bucket["count"]
        if cumulative >= target:
            return [round(bucket["low_ps"] / 1_000_000_000, 6), round(bucket["high_ps"] / 1_000_000_000, 6)]
    return None


def statement_digest_delta(before: dict, after: dict) -> dict:
    before_statements = before.get("statements", {})
    after_statements = after.get("statements", {})
    before_histograms = before.get("histograms", {})
    after_histograms = after.get("histograms", {})
    deltas = []

    for identity, old in before_statements.items():
        if identity not in after_statements and any(old[field] for field in STATEMENT_DIGEST_COUNTERS):
            raise AcceptanceError(
                "Performance Schema statement digest counter reset: "
                f"schema={identity[0]!r} digest={identity[1]}"
            )

    for identity, current in after_statements.items():
        schema_name, digest = identity
        old = before_statements.get(identity, {field: 0 for field in STATEMENT_DIGEST_COUNTERS})
        difference = {}
        for field in STATEMENT_DIGEST_COUNTERS:
            if current[field] < old[field]:
                raise AcceptanceError(
                    "Performance Schema statement digest counter reset: "
                    f"schema={schema_name!r} digest={digest}"
                )
            difference[field] = current[field] - old[field]

        old_buckets = before_histograms.get(identity, {})
        current_buckets = after_histograms.get(identity, {})
        if any(bucket["count"] and number not in current_buckets for number, bucket in old_buckets.items()):
            raise AcceptanceError("Performance Schema statement histogram counter reset")
        histogram_delta = {}
        for number, bucket in current_buckets.items():
            old_bucket = old_buckets.get(number, {"count": 0, "low_ps": bucket["low_ps"], "high_ps": bucket["high_ps"]})
            if bucket["count"] < old_bucket["count"] or (bucket["low_ps"], bucket["high_ps"]) != (old_bucket["low_ps"], old_bucket["high_ps"]):
                raise AcceptanceError("Performance Schema statement histogram counter reset")
            histogram_delta[number] = {
                "low_ps": bucket["low_ps"],
                "high_ps": bucket["high_ps"],
                "count": bucket["count"] - old_bucket["count"],
            }

        if difference["count_star"] == 0:
            continue
        histogram_count = sum(bucket["count"] for bucket in histogram_delta.values())
        deltas.append({
            "schema_name": schema_name,
            "digest": digest,
            "statement": current["statement"] or "<statement text unavailable>",
            "count": difference["count_star"],
            "sum_timer_wait_ms": difference["sum_timer_wait_ps"] / 1_000_000_000,
            "mean_timer_wait_ms": difference["sum_timer_wait_ps"] / difference["count_star"] / 1_000_000_000,
            "sum_lock_time_ms": difference["sum_lock_time_ps"] / 1_000_000_000,
            "sum_rows_sent": difference["sum_rows_sent"],
            "sum_rows_examined": difference["sum_rows_examined"],
            "sum_no_index_used": difference["sum_no_index_used"],
            "sum_no_good_index_used": difference["sum_no_good_index_used"],
            "histogram_count": histogram_count,
            "histogram_count_difference": histogram_count - difference["count_star"],
            "p50_bucket_ms": _statement_percentile_bucket(histogram_delta, difference["count_star"], 0.50),
            "p95_bucket_ms": _statement_percentile_bucket(histogram_delta, difference["count_star"], 0.95),
            "p99_bucket_ms": _statement_percentile_bucket(histogram_delta, difference["count_star"], 0.99),
            # Diagnostic only: this uses the observed histogram sample count and
            # does not imply that the histogram covers every statement event.
            "observed_histogram_p95_bucket_ms": _statement_percentile_bucket(histogram_delta, histogram_count, 0.95),
        })

    deltas.sort(
        key=lambda item: (
            -item["sum_timer_wait_ms"],
            -item["count"],
            item["schema_name"],
            item["digest"],
        )
    )
    return {
        "digest_count": len(deltas),
        "statement_count": sum(item["count"] for item in deltas),
        "sum_timer_wait_ms": sum(item["sum_timer_wait_ms"] for item in deltas),
        "sum_lock_time_ms": sum(item["sum_lock_time_ms"] for item in deltas),
        "top_statements": deltas[:30],
    }


def performance_schema_statement_snapshot(container: str, database: str, account_user: str) -> dict:
    if not re.fullmatch(r"gostream_feed_a04_mixed_[A-Za-z0-9_]+", database):
        raise AcceptanceError("refusing Performance Schema query outside the isolated A04 mixed prefix")
    if not re.fullmatch(r"[A-Za-z0-9_]+", account_user):
        raise AcceptanceError("refusing Performance Schema account query with an invalid username")
    unavailable_account_events = {"available": False, "reason": "performance_schema_capability_check_failed"}
    try:
        if scalar(container, "SELECT @@performance_schema;") != 1:
            return {
                "available": False,
                "reason": "performance_schema_disabled",
                "account_statement_events": {"available": False, "reason": "performance_schema_disabled"},
            }
        account_statement_events = unavailable_account_events
        try:
            account_event_rows = mysql_query(
                container,
                "SELECT EVENT_NAME, SUM(COUNT_STAR), SUM(SUM_TIMER_WAIT), SUM(SUM_LOCK_TIME), "
                "SUM(SUM_ROWS_SENT), SUM(SUM_ROWS_EXAMINED), SUM(SUM_NO_INDEX_USED), SUM(SUM_NO_GOOD_INDEX_USED) "
                "FROM performance_schema.events_statements_summary_by_account_by_event_name "
                f"WHERE USER='{account_user}' AND (EVENT_NAME LIKE 'statement/com/%' OR EVENT_NAME LIKE 'statement/sql/%') "
                "GROUP BY EVENT_NAME ORDER BY EVENT_NAME;",
            )
            account_statement_events = {
                "available": True,
                "account_user": account_user,
                "snapshot": parse_account_statement_event_snapshot(account_event_rows),
            }
        except Exception as error:
            account_statement_events = diagnostic_failure(
                "account_event_query_failed", "account_event_query", error
            )

        digest_diagnostics = {"available": False, "reason": "performance_schema_query_failed"}
        digest_stage = "digest_consumer_check"
        try:
            consumers = mysql_query(
                container,
                "SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME='statements_digest';",
            )
            if consumers != [["YES"]]:
                digest_diagnostics = {"available": False, "reason": "statement_digest_consumer_disabled"}
            else:
                digest_stage = "digest_summary_query"
                # Only retain attributable statement digests. NULL digests have
                # no per-SQL identity and their histogram rows can share bucket numbers.
                summary_rows = mysql_query(
                    container,
                    "SELECT SCHEMA_NAME, DIGEST, HEX(COALESCE(DIGEST_TEXT,'')), COUNT_STAR, SUM_TIMER_WAIT, "
                    "SUM_LOCK_TIME, SUM_ROWS_SENT, SUM_ROWS_EXAMINED, SUM_NO_INDEX_USED, SUM_NO_GOOD_INDEX_USED "
                    "FROM performance_schema.events_statements_summary_by_digest "
                    f"WHERE SCHEMA_NAME='{database}' AND DIGEST IS NOT NULL ORDER BY DIGEST;",
                )
                digest_stage = "digest_histogram_query"
                histogram_rows = mysql_query(
                    container,
                    # Bucket counters update while workers are active. Aggregate by the documented
                    # bucket identity and retain the largest observed monotonic counter value.
                    "SELECT SCHEMA_NAME, DIGEST, BUCKET_NUMBER, BUCKET_TIMER_LOW, BUCKET_TIMER_HIGH, MAX(COUNT_BUCKET) "
                    "FROM performance_schema.events_statements_histogram_by_digest "
                    f"WHERE SCHEMA_NAME='{database}' AND DIGEST IS NOT NULL "
                    "GROUP BY SCHEMA_NAME, DIGEST, BUCKET_NUMBER, BUCKET_TIMER_LOW, BUCKET_TIMER_HIGH "
                    "ORDER BY DIGEST, BUCKET_NUMBER;",
                )
                digest_stage = "digest_snapshot_parse"
                digest_diagnostics = {
                    "available": True,
                    "snapshot": parse_statement_digest_snapshot(summary_rows, histogram_rows),
                }
        except Exception as error:
            digest_diagnostics = diagnostic_failure(
                "performance_schema_query_failed", digest_stage, error
            )
        return {**digest_diagnostics, "account_statement_events": account_statement_events}
    except Exception as error:
        return {
            **diagnostic_failure("performance_schema_query_failed", "performance_schema_capability_check", error),
            "account_statement_events": unavailable_account_events,
        }


def performance_schema_statement_delta(
    before: dict,
    after: dict,
    window: str = "mixed driver lifetime: 30s read-only warmup plus 60s measured workload",
) -> dict:
    result = {"available": False, "reason": before.get("reason") or after.get("reason") or "snapshot_unavailable"}
    failed_snapshot = None
    if not before.get("available"):
        failed_snapshot = before
    elif not after.get("available"):
        failed_snapshot = after
    if failed_snapshot:
        for field in ("failure_stage", "error"):
            if failed_snapshot.get(field):
                result[field] = failed_snapshot[field]
    if before.get("available") and after.get("available"):
        try:
            result = {"available": True, **statement_digest_delta(before["snapshot"], after["snapshot"])}
        except (AcceptanceError, KeyError) as error:
            result = diagnostic_failure(
                "performance_schema_counter_reset_or_invalid_snapshot", "statement_digest_delta", error
            )

    before_events = before.get("account_statement_events", {})
    after_events = after.get("account_statement_events", {})
    prepared_diagnostics = {
        "available": False,
        "reason": before_events.get("reason") or after_events.get("reason") or "account_event_snapshot_unavailable",
    }
    failed_events = None
    if not before_events.get("available"):
        failed_events = before_events
    elif not after_events.get("available"):
        failed_events = after_events
    if failed_events:
        for field in ("failure_stage", "error"):
            if failed_events.get(field):
                prepared_diagnostics[field] = failed_events[field]
    if before_events.get("available") and after_events.get("available"):
        try:
            if before_events.get("account_user") != after_events.get("account_user"):
                raise AcceptanceError("Performance Schema account user changed between snapshots")
            prepared_diagnostics = prepared_statement_event_delta(
                before_events["snapshot"], after_events["snapshot"], after_events["account_user"]
            )
        except (AcceptanceError, KeyError):
            prepared_diagnostics = {
                "available": False,
                "reason": "performance_schema_account_event_reset_or_invalid_snapshot",
            }
    result["prepared_statement_diagnostics"] = prepared_diagnostics
    result["window"] = window
    return result


def schema_sql(container: str, sql: str) -> None:
    mysql_query(container, sql)


def schema_dsn(prefix: str, schema: str, interpolate_params: bool = False) -> str:
    dsn = prefix + schema + "?charset=utf8mb4&parseTime=True&loc=UTC"
    if interpolate_params:
        dsn += "&interpolateParams=true"
    return dsn


def dsn_account_user(dsn_prefix: str) -> str:
    match = re.match(r"^([A-Za-z0-9_]+)(?::[^@]*)?@tcp\(", dsn_prefix)
    if not match:
        raise AcceptanceError("A04 MySQL DSN must use a simple account name and the tcp protocol")
    return match.group(1)


def prepare_schema(container: str, schema: str) -> None:
    if not re.fullmatch(r"gostream_feed_a04_mixed_[A-Za-z0-9_]+", schema):
        raise AcceptanceError("refusing schema outside the isolated A04 mixed prefix")
    schema_sql(
        container,
        f"DROP DATABASE IF EXISTS `{schema}`;\n"
        f"CREATE DATABASE `{schema}` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n"
        f"GRANT ALL PRIVILEGES ON `{schema}`.* TO 'gostream'@'%';\n",
    )


def drop_schema(container: str, schema: str) -> None:
    if not re.fullmatch(r"gostream_feed_a04_mixed_[A-Za-z0-9_]+", schema):
        raise AcceptanceError("refusing cleanup outside the isolated A04 mixed prefix")
    try:
        schema_sql(
            container,
            f"REVOKE ALL PRIVILEGES ON `{schema}`.* FROM 'gostream'@'%';\n"
            f"DROP DATABASE IF EXISTS `{schema}`;\n",
        )
    except AcceptanceError:
        # The disposable MySQL service is dropped at job teardown as a final cleanup boundary.
        schema_sql(container, f"DROP DATABASE IF EXISTS `{schema}`;\n")


def seed_schema(go_bin: str, dsn: str, report_dir: pathlib.Path, mode: str, round_number: int) -> None:
    command = [
        go_bin,
        "run",
        "./tests/load/seed",
        "-driver",
        "mysql",
        "-dsn",
        dsn,
        "-scenario",
        "hybrid",
        "-users",
        "2000",
        "-authors",
        "100",
        "-videos",
        "20000",
        "-follows-per-user",
        "5",
        "-celebrity-followers",
        "1500",
        "-fanout-max-followers",
        "1000",
        "-likes",
        "50000",
    ]
    result = subprocess.run(command, capture_output=True, text=True, check=False, timeout=900)
    log = (result.stdout + result.stderr).replace("password=load-fixture-password", "password=[redacted]")
    (report_dir / f"seed-{mode}-round-{round_number}.log").write_text(log)
    if result.returncode != 0:
        raise AcceptanceError(f"full-size fixture seed failed for {mode} round {round_number}")


def start_api(
    binary: str,
    database: str,
    prefix: str,
    jwt_secret: str,
    report_dir: pathlib.Path,
    mode: str,
    round_number: int,
    args,
    capture_profiles: bool = False,
    interpolate_params: bool = False,
):
    port = args.port
    address = f"127.0.0.1:{port}"
    log_path = report_dir / f"api-{mode}-round-{round_number}.log"
    log_handle = log_path.open("w")
    media_dir = tempfile.mkdtemp(prefix=f"gostream-feed-a04-{mode}-{round_number}-")
    environment = os.environ.copy()
    environment.update(
        {
            "ADDR": address,
            "DB_DRIVER": "mysql",
            "DB_DSN": schema_dsn(prefix, database, interpolate_params),
            "MEDIA_DIR": media_dir,
            "JWT_SECRET": jwt_secret,
            "REDIS_ADDR": "",
            "RABBITMQ_URL": "",
            "FFMPEG_BIN": "",
            "FEED_FANOUT_MAX_FOLLOWERS": "0" if mode == "sql-only" else "1000",
        }
    )
    if should_capture_pprof(mode, capture_profiles):
        environment["PPROF_ADDR"] = f"127.0.0.1:{args.pprof_port}"
    pathlib.Path(environment["MEDIA_DIR"]).mkdir(parents=True, exist_ok=True)
    process = subprocess.Popen([binary], stdout=log_handle, stderr=subprocess.STDOUT, env=environment)
    base_url = f"http://{address}"
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        if process.poll() is not None:
            log_handle.close()
            shutil.rmtree(media_dir, ignore_errors=True)
            raise AcceptanceError(f"{mode} API exited during startup; see {log_path}")
        try:
            request_json(f"{base_url}/health")
            request_json(f"{base_url}/ready")
            return process, log_handle, log_path, base_url, media_dir
        except AcceptanceError:
            time.sleep(0.25)
    terminate_api(process, log_handle)
    shutil.rmtree(media_dir, ignore_errors=True)
    raise AcceptanceError(f"{mode} API did not become ready within 45 seconds; see {log_path}")


def terminate_api(process: subprocess.Popen, log_handle) -> None:
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
    if not log_handle.closed:
        log_handle.close()


def stop_api(process: subprocess.Popen, log_handle, log_path: pathlib.Path) -> None:
    requested_shutdown = process.poll() is None
    terminate_api(process, log_handle)
    expected_signal_exit = requested_shutdown and process.returncode == -signal.SIGTERM
    if process.returncode != 0 and not expected_signal_exit:
        raise AcceptanceError(f"API process exited with status {process.returncode}; see {log_path}")


def login(base_url: str, username: str) -> str:
    result = request_json(
        f"{base_url}/api/sessions",
        method="POST",
        payload={"username": username, "password": "load-fixture-password"},
    )
    token = result.get("access_token")
    if not isinstance(token, str) or not token:
        raise AcceptanceError(f"fixture login returned no token for {username}")
    return token


def id_for(container: str, database: str, username: str) -> int:
    rows = mysql_query(container, f"SELECT id FROM `{database}`.users WHERE username = '{username}';")
    if len(rows) != 1:
        raise AcceptanceError(f"fixture user missing or duplicated: {username}")
    return int(rows[0][0])


def initial_workload_targets(container: str, database: str, reader_names: list[str]) -> tuple[list[int], list[int]]:
    rows = mysql_query(
        container,
        f"SELECT v.id FROM `{database}`.videos v "
        "WHERE v.status='published' AND NOT EXISTS "
        f"(SELECT 1 FROM `{database}`.video_actions a WHERE a.video_id=v.id) "
        f"ORDER BY v.id LIMIT {DRIVER_ACTORS};",
    )
    video_ids = [int(row[0]) for row in rows]
    if len(video_ids) != DRIVER_ACTORS:
        raise AcceptanceError(f"fixture has {len(video_ids)} untouched videos; expected {DRIVER_ACTORS}")
    followee_ids = []
    for reader in reader_names:
        rows = mysql_query(
            container,
            f"SELECT f.followee_id FROM `{database}`.follows f "
            f"JOIN `{database}`.users u ON u.id=f.follower_id WHERE u.username='{reader}' "
            "ORDER BY f.followee_id LIMIT 1;",
        )
        if len(rows) != 1:
            raise AcceptanceError(f"fixture reader has no followed author: {reader}")
        followee_ids.append(int(rows[0][0]))
    return video_ids, followee_ids


def collect_api_samples(pid: int, stop: threading.Event, samples: list[tuple[float, int, int]]) -> None:
    while not stop.is_set():
        try:
            cpu, rss = process_stat(pid)
        except (FileNotFoundError, ProcessLookupError):
            return
        samples.append((time.monotonic(), cpu, rss))
        stop.wait(0.5)


def should_capture_pprof(variant: str, diagnostic_only: bool) -> bool:
    return diagnostic_only and variant in {"sql-only", "hybrid"}


def capture_pprof_artifact(url: str, output_path: pathlib.Path, timeout_seconds: float) -> dict:
    try:
        with urllib.request.urlopen(url, timeout=timeout_seconds) as response:
            payload = response.read()
    except urllib.error.HTTPError as error:
        return {"captured": False, "error": f"HTTP {error.code}"}
    except Exception as error:
        return {"captured": False, "error": type(error).__name__}
    if not payload:
        return {"captured": False, "error": "profiler returned an empty response"}
    try:
        output_path.write_bytes(payload)
    except OSError as error:
        return {"captured": False, "error": type(error).__name__}
    return {"captured": True, "path": output_path.name, "bytes": len(payload)}


def capture_pprof_after_delay(
    stop: threading.Event,
    delay_seconds: float,
    url: str,
    output_path: pathlib.Path,
    timeout_seconds: float,
) -> dict:
    if stop.wait(delay_seconds):
        return {"captured": False, "error": "load finished before capture started"}
    return capture_pprof_artifact(url, output_path, timeout_seconds)


def capture_following_query_plan(
    go_bin: str,
    dsn: str,
    viewer_id: int,
    mode: str,
    round_number: int,
    output_path: pathlib.Path,
) -> dict:
    """Capture EXPLAIN JSON from the production GORM query builder outside load windows."""
    if mode not in {"sql-only", "hybrid"} or viewer_id <= 0 or round_number <= 0:
        return {"available": False, "reason": "invalid_diagnostic_arguments"}
    output_path = pathlib.Path(output_path)
    env = os.environ.copy()
    env.update(
        {
            "GOSTREAM_TEST_MYSQL_DSN": dsn,
            "GOSTREAM_A04_PLAN_MODE": mode,
            "GOSTREAM_A04_PLAN_VIEWER_ID": str(viewer_id),
            "GOSTREAM_A04_PLAN_OUTPUT": str(output_path),
        }
    )
    command = [go_bin, "test", "./internal/platform", "-run", "^TestA04FollowingQueryPlanDiagnostic$", "-count=1"]
    try:
        completed = subprocess.run(
            command,
            cwd=pathlib.Path(__file__).resolve().parents[2],
            env=env,
            capture_output=True,
            text=True,
            timeout=90,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        return {
            "available": False,
            "reason": "diagnostic_runner_error",
            "error_type": type(error).__name__,
        }
    if completed.returncode != 0:
        return {
            "available": False,
            "reason": "diagnostic_test_failed",
            "exit_code": completed.returncode,
        }
    if not output_path.is_file():
        return {"available": False, "reason": "diagnostic_output_missing"}
    try:
        payload = json.loads(output_path.read_text())
    except (OSError, json.JSONDecodeError):
        return {"available": False, "reason": "diagnostic_output_invalid"}
    if payload.get("mode") != mode or not isinstance(payload.get("plan"), dict):
        return {"available": False, "reason": "diagnostic_output_unexpected"}
    return {
        "available": True,
        "source": "production followingCandidateQuery via opt-in MySQL integration diagnostic",
        "mode": mode,
        "viewer_id": viewer_id,
        "output_file": output_path.name,
    }


def run_mixed_driver(
    driver: str,
    base_url: str,
    token_path: pathlib.Path,
    video_ids: list[int],
    followee_ids: list[int],
    report_path: pathlib.Path,
    log_path: pathlib.Path,
    api_pid: int,
    pprof_base_url: str = "",
    warmup_seconds: int = WARMUP_SECONDS,
    duration_seconds: int = DURATION_SECONDS,
    warmup_only: bool = False,
) -> tuple[dict, dict]:
    command = [
        driver,
        "-isolated",
        "-base-url",
        base_url,
        "-token-file",
        str(token_path),
        "-video-ids",
        ",".join(map(str, video_ids)),
        "-followee-ids",
        ",".join(map(str, followee_ids)),
        "-concurrency",
        str(CONCURRENCY),
        "-warmup",
        f"{warmup_seconds}s",
        "-duration",
        f"{duration_seconds}s",
    ]
    if warmup_only:
        command.append("-warmup-only")
    else:
        command.extend(("-report-file", str(report_path)))
    samples: list[tuple[float, int, int]] = []
    stop = threading.Event()
    sampler = None
    if not warmup_only:
        sampler = threading.Thread(target=collect_api_samples, args=(api_pid, stop, samples), daemon=True)
        sampler.start()
    profile_results: dict[str, dict] = {}
    profile_threads = []
    if pprof_base_url:
        cpu_path = report_path.with_name(report_path.stem + "-cpu.pb.gz")
        goroutine_path = report_path.with_name(report_path.stem + "-goroutines.txt")
        profile_targets = (
            (
                "cpu",
                warmup_seconds + 0.25,
                f"{pprof_base_url}/debug/pprof/profile?seconds={duration_seconds}",
                cpu_path,
                duration_seconds + 20,
            ),
            (
                "goroutine",
                warmup_seconds + duration_seconds / 2,
                f"{pprof_base_url}/debug/pprof/goroutine?debug=2",
                goroutine_path,
                10,
            ),
        )

        def capture_profile(name, delay, url, path, timeout):
            profile_results[name] = capture_pprof_after_delay(stop, delay, url, path, timeout)

        for name, delay, url, path, timeout in profile_targets:
            thread = threading.Thread(target=capture_profile, args=(name, delay, url, path, timeout), daemon=True)
            thread.start()
            profile_threads.append(thread)
    try:
        result = subprocess.run(
            command,
            capture_output=True,
            text=True,
            timeout=warmup_seconds + (0 if warmup_only else duration_seconds) + 60,
            check=False,
        )
    except subprocess.TimeoutExpired as error:
        log_path.write_text(f"mixed load driver timed out: {error}\n")
        raise AcceptanceError("mixed load driver exceeded warmup + measurement timeout") from error
    finally:
        stop.set()
        if sampler is not None:
            sampler.join(timeout=5)
        for thread in profile_threads:
            thread.join(timeout=duration_seconds + 25)
    log_path.write_text(result.stdout + result.stderr)
    if warmup_only:
        if result.returncode != 0:
            raise AcceptanceError(f"read-only Following warmup failed; see {log_path}")
        match = re.search(r"read-only warmup completed requests=(\d+)", result.stdout)
        if not match or int(match.group(1)) <= 0:
            raise AcceptanceError(f"read-only Following warmup reported no successful requests; see {log_path}")
        return {"successes": int(match.group(1)), "duration_seconds": warmup_seconds}, {
            "stdout": result.stdout,
            "stderr": result.stderr,
        }
    if not report_path.exists():
        raise AcceptanceError(f"mixed load driver wrote no report; see {log_path}")
    report = json.loads(report_path.read_text())
    report["failures"] = int(report.get("failures", 0))
    if result.returncode != 0:
        raise AcceptanceError(f"mixed load had failed requests; see {log_path}")
    if report["successes"] == 0 or report["failures"] != 0:
        raise AcceptanceError(f"mixed load returned no successes or had failures; see {log_path}")
    report["api_process"] = process_metrics(samples)
    report["api_process"]["window"] = f"{duration_seconds}s measured mixed workload"
    if pprof_base_url:
        report["diagnostic_profiles"] = {
            "capture_window": "Separate diagnostic-only fixture: CPU profile starts 250ms into the measured mixed workload for 60s; goroutine dump is scheduled halfway through that window.",
            "cpu": profile_results.get("cpu", {"captured": False, "error": "capture thread did not finish"}),
            "goroutine": profile_results.get("goroutine", {"captured": False, "error": "capture thread did not finish"}),
        }
    return report, {"stdout": result.stdout, "stderr": result.stderr}


def audit_counters(container: str, database: str) -> dict[str, int]:
    rows = mysql_query(
        container,
        "SELECT "
        "(SELECT COUNT(*) FROM `" + database + "`.videos v LEFT JOIN "
        "(SELECT video_id, SUM(kind='like') likes, SUM(kind='favorite') favorites "
        "FROM `" + database + "`.video_actions GROUP BY video_id) a ON a.video_id=v.id "
        "WHERE v.like_count<>COALESCE(a.likes,0) OR v.favorite_count<>COALESCE(a.favorites,0)), "
        "(SELECT COUNT(*) FROM `" + database + "`.videos v LEFT JOIN "
        "(SELECT video_id, COUNT(*) comments FROM `" + database + "`.comments GROUP BY video_id) c ON c.video_id=v.id "
        "WHERE v.comment_count<>COALESCE(c.comments,0));",
    )
    if len(rows) != 1 or len(rows[0]) != 2:
        raise AcceptanceError(f"counter audit returned unexpected rows: {rows!r}")
    return {"action_mismatches": int(rows[0][0]), "comment_mismatches": int(rows[0][1])}


def queue_counts(container: str, database: str) -> dict[str, int]:
    rows = mysql_query(
        container,
        f"SELECT (SELECT COUNT(*) FROM `{database}`.feed_fanout_jobs WHERE done_at IS NULL), "
        f"(SELECT COUNT(*) FROM `{database}`.outbox_events WHERE published_at IS NULL AND dead_at IS NULL), "
        f"(SELECT COUNT(*) FROM `{database}`.outbox_events WHERE dead_at IS NOT NULL);",
    )
    if len(rows) != 1 or len(rows[0]) != 3:
        raise AcceptanceError(f"queue query returned unexpected rows: {rows!r}")
    return {"fanout_pending": int(rows[0][0]), "outbox_pending": int(rows[0][1]), "outbox_dead": int(rows[0][2])}


def wait_for_queues(container: str, database: str, timeout_seconds: int) -> tuple[dict, float]:
    start = time.monotonic()
    deadline = start + timeout_seconds
    latest = queue_counts(container, database)
    while any(latest.values()) and time.monotonic() < deadline:
        time.sleep(1)
        latest = queue_counts(container, database)
    return latest, time.monotonic() - start


def publish_videos(
    base_url: str,
    publisher_tokens: list[str],
    publisher_usernames: list[str],
    media_urls: list[str],
    mode: str,
    round_number: int,
) -> dict:
    def publish_one(index: int) -> dict:
        payload = {
            "title": f"A04 mixed {mode} round {round_number} author {index}",
            "media_url": media_urls[index],
        }
        start = time.monotonic_ns()
        response = request_json(
            f"{base_url}/api/videos",
            method="POST",
            token=publisher_tokens[index],
            payload=payload,
            expected_status=201,
        )
        elapsed = time.monotonic_ns() - start
        video_id = response.get("id")
        if not isinstance(video_id, int) or video_id <= 0:
            raise AcceptanceError(f"publisher {publisher_usernames[index]} returned no video ID")
        return {
            "username": publisher_usernames[index],
            "video_id": video_id,
            "elapsed_ns": elapsed,
            "status": 201,
        }

    started = time.monotonic()
    results: list[dict] = []
    failures: list[str] = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=PUBLISHERS) as pool:
        futures = {pool.submit(publish_one, index): index for index in range(PUBLISHERS)}
        for future in concurrent.futures.as_completed(futures):
            try:
                results.append(future.result())
            except Exception as error:  # retain partial publish evidence before failing the acceptance
                failures.append(f"publisher {publisher_usernames[futures[future]]}: {error}")
    results.sort(key=lambda result: result["username"])
    latencies = [result["elapsed_ns"] for result in results]
    return {
        "successes": len(results),
        "failures": len(failures),
        "errors": failures,
        "elapsed_seconds": time.monotonic() - started,
        "videos": results,
        "latency_ns": latencies,
        "p50_ns": percentile(latencies, 50),
        "p95_ns": percentile(latencies, 95),
        "p99_ns": percentile(latencies, 99),
    }


def published_video_rows(container: str, database: str, video_ids: list[int]) -> list[dict]:
    if not video_ids:
        raise AcceptanceError("no published video IDs to verify")
    ids = ",".join(map(str, video_ids))
    rows = mysql_query(
        container,
        f"SELECT v.id, v.author_id, v.feed_pull, COALESCE(j.id,0), COALESCE(j.done_at,''), COALESCE(j.attempts,0), "
        f"(SELECT COUNT(*) FROM `{database}`.feed_inboxes i WHERE i.video_id=v.id), "
        f"(SELECT COUNT(*) FROM `{database}`.follows f WHERE f.followee_id=v.author_id) "
        f"FROM `{database}`.videos v LEFT JOIN `{database}`.feed_fanout_jobs j ON j.video_id=v.id "
        f"WHERE v.id IN ({ids}) ORDER BY v.id;",
    )
    return [
        {
            "video_id": int(row[0]),
            "author_id": int(row[1]),
            "feed_pull": bool(int(row[2])),
            "job_id": int(row[3]),
            "job_done_at": row[4],
            "job_attempts": int(row[5]),
            "inbox_rows": int(row[6]),
            "followers": int(row[7]),
        }
        for row in rows
    ]


def run_case(args, mode: str, round_number: int, diagnostic_only: bool = False) -> dict:
    database = f"{args.schema_prefix}_{mode.replace('-', '')}_r{round_number}"
    prepare_schema(args.mysql_container, database)
    api = None
    api_log_path = None
    log_handle = None
    media_dir = None
    token_path = None
    try:
        dsn = schema_dsn(args.dsn_prefix, database, args.interpolate_params)
        seed_schema(args.go_bin, dsn, pathlib.Path(args.report_dir), mode, round_number)
        seed_count = mysql_query(
            args.mysql_container,
            f"SELECT (SELECT COUNT(*) FROM `{database}`.users), "
            f"(SELECT COUNT(*) FROM `{database}`.videos), "
            f"(SELECT COUNT(*) FROM `{database}`.follows), "
            f"(SELECT COUNT(*) FROM `{database}`.video_actions), "
            f"(SELECT COUNT(*) FROM `{database}`.feed_inboxes), "
            f"(SELECT COUNT(*) FROM `{database}`.media_assets);",
        )
        if seed_count != [["2000", "20000", "10999", "50000", "1900000", "100"]]:
            raise AcceptanceError(f"full-size fixture counts differ: {seed_count!r}")
        if not (pathlib.Path(args.report_dir) / "environment.txt").exists():
            evidence = host_evidence(args.go_bin, args.mysql_container, database)
            (pathlib.Path(args.report_dir) / "environment.txt").write_text(evidence)

        jwt_secret = secrets.token_urlsafe(48)
        api, log_handle, api_log_path, base_url, media_dir = start_api(
            args.api_binary, database, args.dsn_prefix, jwt_secret, pathlib.Path(args.report_dir), mode, round_number, args,
            capture_profiles=diagnostic_only,
            interpolate_params=args.interpolate_params,
        )
        reader_names = driver_reader_names()
        publisher_names = [f"load-u-{i:06d}" for i in range(1, PUBLISHERS + 1)]
        publishers_before_load, publishers_after_load = publisher_login_batches(
            len(reader_names), publisher_names, FOLLOWERS
        )
        reader_tokens = [login(base_url, username) for username in reader_names]
        publisher_tokens = [login(base_url, username) for username in publishers_before_load]
        video_ids, followee_ids = initial_workload_targets(args.mysql_container, database, reader_names)
        token_fd, token_name = tempfile.mkstemp(prefix="gostream-feed-a04-mixed-", suffix=".tokens")
        token_path = pathlib.Path(token_name)
        with os.fdopen(token_fd, "w") as token_file:
            token_file.write("\n".join(reader_tokens) + "\n")
        report_path = pathlib.Path(args.report_dir) / f"mixed-{mode}-round-{round_number}.json"
        driver_log = pathlib.Path(args.report_dir) / f"mixed-{mode}-round-{round_number}.log"
        warmup_log = pathlib.Path(args.report_dir) / f"warmup-{mode}-round-{round_number}.log"
        account_user = dsn_account_user(args.dsn_prefix)
        mysql_digest_before = performance_schema_statement_snapshot(args.mysql_container, database, account_user)
        mysql_wait_before = performance_schema_wait_event_snapshot(args.mysql_container, account_user)
        db_before = status_snapshot(args.mysql_container)
        warmup_report, _ = run_mixed_driver(
            args.mixed_driver,
            base_url,
            token_path,
            video_ids,
            followee_ids,
            report_path.with_name(f"warmup-{mode}-round-{round_number}.json"),
            warmup_log,
            api.pid,
            warmup_seconds=WARMUP_SECONDS,
            duration_seconds=0,
            warmup_only=True,
        )
        mysql_digest_after_warmup = performance_schema_statement_snapshot(
            args.mysql_container, database, account_user
        )
        mysql_wait_after_warmup = performance_schema_wait_event_snapshot(args.mysql_container, account_user)
        db_after_warmup = status_snapshot(args.mysql_container)
        mixed_report, _ = run_mixed_driver(
            args.mixed_driver,
            base_url,
            token_path,
            video_ids,
            followee_ids,
            report_path,
            driver_log,
            api.pid,
            pprof_base_url=(
                f"http://127.0.0.1:{args.pprof_port}"
                if should_capture_pprof(mode, diagnostic_only)
                else ""
            ),
            warmup_seconds=0,
            duration_seconds=DURATION_SECONDS,
        )
        mixed_report["warmup_ns"] = int(WARMUP_SECONDS * 1_000_000_000)
        mixed_report["warmup_phase"] = {
            "mode": "read-only following",
            "duration_seconds": WARMUP_SECONDS,
            "successful_requests": warmup_report["successes"],
            "report_path": warmup_log.name,
        }
        db_after_mix = status_snapshot(args.mysql_container)
        mysql_digest_after = performance_schema_statement_snapshot(args.mysql_container, database, account_user)
        mysql_wait_after_mix = performance_schema_wait_event_snapshot(args.mysql_container, account_user)
        mysql_statement_diagnostics = performance_schema_statement_delta(
            mysql_digest_before,
            mysql_digest_after,
            f"{WARMUP_SECONDS}s read-only warmup plus {DURATION_SECONDS}s measured mixed workload",
        )
        mysql_statement_diagnostics_warmup = performance_schema_statement_delta(
            mysql_digest_before,
            mysql_digest_after_warmup,
            f"{WARMUP_SECONDS}s read-only Following warmup",
        )
        mysql_statement_diagnostics_measured = performance_schema_statement_delta(
            mysql_digest_after_warmup,
            mysql_digest_after,
            f"{DURATION_SECONDS}s measured mixed workload",
        )
        mysql_wait_diagnostics = performance_schema_wait_event_delta(
            mysql_wait_before,
            mysql_wait_after_mix,
            f"{WARMUP_SECONDS}s read-only warmup plus {DURATION_SECONDS}s measured mixed workload",
        )
        mysql_wait_diagnostics_warmup = performance_schema_wait_event_delta(
            mysql_wait_before,
            mysql_wait_after_warmup,
            f"{WARMUP_SECONDS}s read-only Following warmup",
        )
        mysql_wait_diagnostics_measured = performance_schema_wait_event_delta(
            mysql_wait_after_warmup,
            mysql_wait_after_mix,
            f"{DURATION_SECONDS}s measured mixed workload",
        )
        following_query_plan_diagnostic = {"available": False, "reason": "not_scheduled"}
        if not diagnostic_only and round_number == 1:
            try:
                plan_viewer_id = id_for(args.mysql_container, database, reader_names[0])
                following_query_plan_diagnostic = capture_following_query_plan(
                    args.go_bin,
                    dsn,
                    plan_viewer_id,
                    mode,
                    round_number,
                    pathlib.Path(args.report_dir) / f"following-plan-{mode}.json",
                )
            except Exception as error:
                following_query_plan_diagnostic = {
                    "available": False,
                    "reason": "diagnostic_setup_failed",
                    "error_type": type(error).__name__,
                }
        report_path.write_text(json.dumps(mixed_report, indent=2) + "\n")
        ratios = workload_ratios(mixed_report)
        if not (0.68 <= ratios["feed"] <= 0.72 and 0.18 <= ratios["interaction"] <= 0.22 and 0.08 <= ratios["social"] <= 0.12):
            raise AcceptanceError(f"mixed driver operation ratio outside 70/20/10 tolerances: {ratios}")
        if any(audit_counters(args.mysql_container, database).values()):
            raise AcceptanceError(f"counter mismatch after mixed workload in {database}")
        drained, outbox_drain_seconds = wait_for_queues(args.mysql_container, database, FANOUT_DEADLINE_SECONDS)
        if drained["fanout_pending"] or drained["outbox_pending"] or drained["outbox_dead"]:
            raise AcceptanceError(f"workload outbox did not drain cleanly: {drained}")

        # Split 30 logins into each rate-limit window: readers plus the first publisher batch
        # run before the 90-second workload; deferred publishers share the next window with verifiers.
        publisher_tokens.extend(login(base_url, username) for username in publishers_after_load)
        verifier_names = [f"load-u-{99 + i:06d}" for i in range(1, FOLLOWERS + 1)]
        verifier_tokens = [login(base_url, username) for username in verifier_names]
        publisher_ids = [id_for(args.mysql_container, database, username) for username in publisher_names]
        verifier_ids = [id_for(args.mysql_container, database, username) for username in verifier_names]
        media_urls = []
        for author_id, reader_id, publisher_index in zip(publisher_ids, verifier_ids, range(1, PUBLISHERS + 1)):
            following = scalar(
                args.mysql_container,
                f"SELECT COUNT(*) FROM `{database}`.follows WHERE follower_id={reader_id} AND followee_id={author_id};",
            )
            if following != 1:
                raise AcceptanceError(f"verification reader {reader_id} does not follow publisher {author_id}")
            media_urls.append(f"/media/load-fixture-{publisher_index:06d}.mp4")
        db_before_publish = status_snapshot(args.mysql_container)
        publish_report = publish_videos(base_url, publisher_tokens, publisher_names, media_urls, mode, round_number)
        publish_ids = [video["video_id"] for video in publish_report["videos"]]
        if publish_report["failures"] or len(publish_ids) != PUBLISHERS:
            raise AcceptanceError(f"one or more synthetic video publish requests failed: {publish_report['errors']}")

        job_rows = published_video_rows(args.mysql_container, database, publish_ids)
        if len(job_rows) != PUBLISHERS:
            raise AcceptanceError(f"expected {PUBLISHERS} published rows, found {len(job_rows)}")
        if mode == "hybrid":
            if any(row["job_id"] == 0 for row in job_rows):
                raise AcceptanceError("hybrid publishes did not create a fanout job for every ordinary author")
        elif any(row["job_id"] or row["job_done_at"] or row["job_attempts"] or row["inbox_rows"] for row in job_rows):
            raise AcceptanceError("SQL-only publishes unexpectedly created fanout inbox state")

        queue_state, fanout_drain_seconds = wait_for_queues(
            args.mysql_container, database, FANOUT_DEADLINE_SECONDS
        )
        if queue_state["fanout_pending"] or queue_state["outbox_pending"] or queue_state["outbox_dead"]:
            raise AcceptanceError(f"publish/fanout queues did not drain within 60 seconds: {queue_state}")
        job_rows = published_video_rows(args.mysql_container, database, publish_ids)
        if mode == "hybrid":
            for row in job_rows:
                if row["feed_pull"] or row["inbox_rows"] != row["followers"] or not row["job_done_at"]:
                    raise AcceptanceError(
                        f"fanout result mismatch for video {row['video_id']}: "
                        f"feed_pull={row['feed_pull']} inbox={row['inbox_rows']} followers={row['followers']}"
                    )

        visible_ids: dict[str, list[int]] = {}
        visibility_failures = []
        verifier_tokens_by_name = dict(zip(verifier_names, verifier_tokens))
        for published in publish_report["videos"]:
            author_number = int(published["username"].rsplit("-", 1)[1])
            username = f"load-u-{99 + author_number:06d}"
            token = verifier_tokens_by_name[username]
            expected_video = published["video_id"]
            feed = request_json(
                f"{base_url}/api/feed?scene=following&limit=100", token=token
            )
            ids = [int(item["id"]) for item in feed.get("items", []) if isinstance(item, dict) and "id" in item]
            visible_ids[username] = ids
            if expected_video not in ids:
                visibility_failures.append({"username": username, "video_id": expected_video})
        if visibility_failures:
            raise AcceptanceError(f"published videos missing from follower Feed: {visibility_failures}")

        counters = audit_counters(args.mysql_container, database)
        queue_state = queue_counts(args.mysql_container, database)
        db_after = status_snapshot(args.mysql_container)
        if any(counters.values()) or queue_state["outbox_pending"] or queue_state["outbox_dead"]:
            raise AcceptanceError(f"post-publish consistency failure: counters={counters}, queues={queue_state}")
        driver_report_path = pathlib.Path(args.report_dir) / f"mixed-{mode}-round-{round_number}.json"
        full_report = {
            "variant": mode,
            "round": round_number,
            "diagnostic_only": diagnostic_only,
            "mysql_interpolate_params": args.interpolate_params,
            "database": database,
            "fixture_rows": {"users": 2000, "authors": 100, "videos": 20000, "follows": 10999, "likes": 50000, "feed_inboxes": 1900000},
            "mixed_load": mixed_report,
            "workload_ratios": ratios,
            "database_status_before": db_before,
            "database_status_after_warmup": db_after_warmup,
            "database_status_after_mixed": db_after_mix,
            "database_statement_diagnostics": {
                "source": "performance_schema.events_statements_summary_by_digest",
                **mysql_statement_diagnostics,
            },
            "database_statement_diagnostics_warmup": {
                "source": "performance_schema.events_statements_summary_by_digest",
                **mysql_statement_diagnostics_warmup,
            },
            "database_statement_diagnostics_measured": {
                "source": "performance_schema.events_statements_summary_by_digest",
                **mysql_statement_diagnostics_measured,
            },
            "database_wait_diagnostics": mysql_wait_diagnostics,
            "database_wait_diagnostics_warmup": mysql_wait_diagnostics_warmup,
            "database_wait_diagnostics_measured": mysql_wait_diagnostics_measured,
            "following_query_plan_diagnostic": following_query_plan_diagnostic,
            "database_status_before_publish": db_before_publish,
            "database_status_after": db_after,
            "outbox_drain_seconds": outbox_drain_seconds,
            "publish": publish_report,
            "fanout": {
                "expected_jobs": PUBLISHERS if mode == "hybrid" else 0,
                "jobs_completed": sum(1 for row in job_rows if row["job_done_at"]),
                "drain_seconds": fanout_drain_seconds,
                "within_deadline": fanout_drain_seconds <= FANOUT_DEADLINE_SECONDS,
                "videos": job_rows,
            },
            "verification": {
                "counter_mismatches": sum(counters.values()),
                "counter_details": counters,
                "feed_visibility_failures": len(visibility_failures),
                "feed_video_ids": visible_ids,
                "outbox_pending": queue_state["outbox_pending"],
                "outbox_dead": queue_state["outbox_dead"],
                "fanout_pending": queue_state["fanout_pending"],
            },
            "api_process": mixed_report["api_process"],
        }
        driver_report_path.write_text(json.dumps(full_report, indent=2) + "\n")
        return full_report
    finally:
        if token_path is not None:
            token_path.unlink(missing_ok=True)
        try:
            if api is not None:
                stop_api(api, log_handle, api_log_path)
        except AcceptanceError:
            terminate_api(api, log_handle)
            raise
        finally:
            if media_dir is not None:
                shutil.rmtree(media_dir, ignore_errors=True)
            drop_schema(args.mysql_container, database)


def workload_ratios(report: dict) -> dict[str, float]:
    operations = report.get("operations", {})
    counts = {name: value.get("successes", 0) + value.get("failures", 0) for name, value in operations.items()}
    total = sum(counts.values())
    if total == 0:
        raise AcceptanceError("mixed driver reported zero requests")
    feed = sum(value for name, value in counts.items() if name.startswith("feed_"))
    interaction = sum(counts.get(name, 0) for name in ("like", "unlike", "favorite", "unfavorite"))
    social = sum(counts.get(name, 0) for name in ("follow", "comment"))
    return {"feed": feed / total, "interaction": interaction / total, "social": social / total}


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-binary", required=True)
    parser.add_argument("--mixed-driver", required=True)
    parser.add_argument("--report-dir", required=True)
    parser.add_argument("--mysql-container", required=True)
    parser.add_argument("--schema-prefix", required=True)
    parser.add_argument("--port", type=int, default=18087)
    parser.add_argument("--pprof-port", type=int, default=18088)
    parser.add_argument("--go-bin", default="go")
    parser.add_argument(
        "--schedule-seed",
        type=int,
        default=DEFAULT_SCHEDULE_SEED,
        help="seed for deterministic, counterbalanced SQL-only/Hybrid round order",
    )
    parser.add_argument(
        "--interpolate-params",
        action="store_true",
        help="enable go-sql-driver/mysql parameter interpolation for this isolated A04 experiment",
    )
    args = parser.parse_args()
    if not (1 <= args.pprof_port <= 65535) or args.pprof_port == args.port:
        parser.error("pprof port must be in 1..65535 and differ from the API port")
    if not re.fullmatch(r"gostream_feed_a04_mixed_[A-Za-z0-9_]+", args.schema_prefix):
        parser.error("schema prefix must use gostream_feed_a04_mixed_<run>_<attempt>")
    args.dsn_prefix = os.environ.get("GOSTREAM_FEED_A04_DSN_PREFIX", "")
    if not args.dsn_prefix or not args.dsn_prefix.endswith("/"):
        parser.error("GOSTREAM_FEED_A04_DSN_PREFIX must end with the isolated MySQL database slash")
    return args


def main() -> int:
    args = parse_args()
    report_dir = pathlib.Path(args.report_dir)
    report_dir.mkdir(parents=True, exist_ok=True)
    if any(report_dir.iterdir()):
        raise AcceptanceError(f"report directory must be empty: {report_dir}")
    for path in (args.api_binary, args.mixed_driver):
        if not pathlib.Path(path).is_file():
            raise AcceptanceError(f"required executable does not exist: {path}")
    version = subprocess.run([args.go_bin, "version"], capture_output=True, text=True, check=False)
    if version.returncode != 0 or "go1.27.2" not in version.stdout:
        raise AcceptanceError(f"A04 mixed acceptance requires Go 1.27.2; got {version.stdout.strip()}")
    schedule = acceptance_schedule(args.schedule_seed)
    reports = []
    try:
        for mode, round_number in schedule:
            report = run_case(args, mode, round_number)
            reports.append(report)
            (report_dir / "partial-summary.json").write_text(json.dumps(reports, indent=2) + "\n")
        profile_reports = []
        for mode, round_number in PROFILE_SCHEDULE:
            try:
                report = run_case(args, mode, round_number, diagnostic_only=True)
            except Exception as error:
                report = {
                    "variant": mode,
                    "round": round_number,
                    "diagnostic_only": True,
                    "run_error": f"{type(error).__name__}: diagnostic-only fixture failed",
                }
            profile_reports.append(report)
            (report_dir / "profile-partial-summary.json").write_text(json.dumps(profile_reports, indent=2) + "\n")
        summary = summarize(reports, REQUIRED_IMPROVEMENT, interpolate_params=args.interpolate_params)
        summary["schedule_seed"] = args.schedule_seed
        summary["schedule"] = [{"mode": mode, "round": number} for mode, number in schedule]
        summary["diagnostic_profiles"] = [
            {
                "variant": report["variant"],
                "round": report["round"],
                **(
                    report["mixed_load"].get("diagnostic_profiles", {})
                    if "mixed_load" in report
                    else {"run_error": report.get("run_error", "diagnostic run produced no report")}
                ),
            }
            for report in profile_reports
        ]
        summary["diagnostic_profile_schedule"] = [
            {"mode": mode, "round": number, "excluded_from_acceptance_gate": True}
            for mode, number in PROFILE_SCHEDULE
        ]
        (report_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        (report_dir / "summary.md").write_text(format_summary(summary))
        print((report_dir / "summary.md").read_text())
        return 0 if summary["target_met"] else 1
    except Exception:
        if reports:
            (report_dir / "partial-summary.json").write_text(json.dumps(reports, indent=2) + "\n")
        raise


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AcceptanceError as error:
        print(f"A04 mixed acceptance failed: {error}", file=sys.stderr)
        raise SystemExit(1)
