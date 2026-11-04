import http.server
import json
import pathlib
import signal
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock

import feed_a04_mixed


def run(variant, round_number, following_p95_ns, publish_p95_ns=3_000_000, fanout_drain_seconds=0.5):
    return {
        "variant": variant,
        "round": round_number,
        "mixed_load": {
            "successes": 1000,
            "failures": 0,
            "server_following_latency": {
                "sample_count": 400,
                "mean_seconds": 0.045,
                "p95_bucket_lower_bound_seconds": 0.04,
                "p95_bucket_upper_bound_seconds": 0.05,
            },
            "server_feed_stages": [
                {
                    "scene": "following",
                    "stage": stage,
                    "outcome": "success",
                    "latency": {
                        "sample_count": 400,
                        "mean_seconds": 0.015,
                        "p95_bucket_lower_bound_seconds": 0.01,
                        "p95_bucket_upper_bound_seconds": 0.025,
                    },
                }
                for stage in (
                    "candidate_query",
                    "candidate_sql",
                    "candidate_query_start",
                    "candidate_row_next",
                    "candidate_row_scan",
                    "author_preload",
                    "interaction_state",
                )
            ],
            "operations": {
                "feed_following": {"successes": 400, "failures": 0, "p95_ns": following_p95_ns},
                "like": {"successes": 100, "failures": 0},
                "comment": {"successes": 50, "failures": 0},
            },
            "database_pool_wait": {
                "wait_count": round_number,
                "wait_duration_seconds": round_number / 1000,
            },
        },
        "database_statement_diagnostics": {
            "available": True,
            "reason": "",
            "window": "mixed driver lifetime: 30s read-only warmup plus 60s measured workload",
            "digest_count": 1,
            "statement_count": 10 + round_number,
            "sum_timer_wait_ms": 12.5 + round_number,
            "top_statements": [
                {
                    "statement": "SELECT * FROM videos WHERE id = ?",
                    "count": 10 + round_number,
                    "mean_timer_wait_ms": 1.25,
                    "p95_bucket_ms": [0.5, 2.0],
                }
            ],
        },
        "publish": {"successes": 20, "failures": 0, "p95_ns": publish_p95_ns},
        "fanout": {
            "within_deadline": True,
            "drain_seconds": fanout_drain_seconds,
            "jobs_completed": 20 if variant == "hybrid" else 0,
        },
        "verification": {"counter_mismatches": 0, "feed_visibility_failures": 0, "outbox_pending": 0, "outbox_dead": 0},
        "api_process": {"average_cpu_percent": 25.0, "peak_rss_kib": 50000},
    }


class FeedA04MixedHelpersTest(unittest.TestCase):
    def test_following_query_plan_capture_records_optional_plan_without_gating(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "following-plan-hybrid.json"

            def capture_plan(command, **kwargs):
                self.assertIn("^TestA04FollowingQueryPlanDiagnostic$", command)
                self.assertEqual(kwargs["timeout"], 90)
                pathlib.Path(kwargs["env"]["GOSTREAM_A04_PLAN_OUTPUT"]).write_text(
                    '{"mode":"hybrid","plan":{"query_block":{}}}'
                )
                return subprocess.CompletedProcess(command, 0, stdout="PASS\n", stderr="")

            with mock.patch.object(feed_a04_mixed.subprocess, "run", side_effect=capture_plan):
                result = feed_a04_mixed.capture_following_query_plan(
                    go_bin="/usr/local/go/bin/go",
                    dsn="gostream:secret@tcp(127.0.0.1:3306)/fixture",
                    viewer_id=500,
                    mode="hybrid",
                    round_number=1,
                    output_path=output,
                )

            self.assertTrue(result["available"])
            self.assertEqual(result["mode"], "hybrid")
            self.assertEqual(result["output_file"], output.name)
            self.assertEqual(result["viewer_id"], 500)

    def test_following_query_plan_capture_can_include_execution_analysis(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "following-plan-hybrid-diagnostic.json"

            def capture_plan(command, **kwargs):
                self.assertEqual(kwargs["env"]["GOSTREAM_A04_PLAN_ANALYZE"], "true")
                pathlib.Path(kwargs["env"]["GOSTREAM_A04_PLAN_OUTPUT"]).write_text(
                    json.dumps(
                        {
                            "mode": "hybrid",
                            "plan": {"query_block": {}},
                            "analyze_tree": "-> Limit: 21 row(s) (actual time=0.10..0.20 rows=20 loops=1)",
                        }
                    )
                )
                return subprocess.CompletedProcess(command, 0, stdout="PASS\\n", stderr="")

            with mock.patch.object(feed_a04_mixed.subprocess, "run", side_effect=capture_plan):
                result = feed_a04_mixed.capture_following_query_plan(
                    go_bin="/usr/local/go/bin/go",
                    dsn="gostream:secret@tcp(127.0.0.1:3306)/fixture",
                    viewer_id=500,
                    mode="hybrid",
                    round_number=4,
                    output_path=output,
                    analyze=True,
                )

            self.assertTrue(result["available"])
            self.assertTrue(result["analyze_available"])
            self.assertEqual(result["analyze_output_file"], output.name)

    def test_following_query_analysis_failure_does_not_hide_estimated_plan(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "following-plan-sql-only-diagnostic.json"

            def capture_plan(command, **kwargs):
                pathlib.Path(kwargs["env"]["GOSTREAM_A04_PLAN_OUTPUT"]).write_text(
                    json.dumps(
                        {
                            "mode": "sql-only",
                            "plan": {"query_block": {}},
                            "analyze_error": "EXPLAIN ANALYZE unavailable",
                        }
                    )
                )
                return subprocess.CompletedProcess(command, 0, stdout="PASS\\n", stderr="")

            with mock.patch.object(feed_a04_mixed.subprocess, "run", side_effect=capture_plan):
                result = feed_a04_mixed.capture_following_query_plan(
                    go_bin="go",
                    dsn="gostream:secret@tcp(127.0.0.1:3306)/fixture",
                    viewer_id=500,
                    mode="sql-only",
                    round_number=4,
                    output_path=output,
                    analyze=True,
                )

            self.assertTrue(result["available"])
            self.assertFalse(result["analyze_available"])
            self.assertEqual(result["analyze_reason"], "EXPLAIN ANALYZE unavailable")

    def test_following_query_plan_capture_failure_is_best_effort(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "following-plan-sql-only.json"
            completed = subprocess.CompletedProcess(args=[], returncode=1, stdout="", stderr="query plan failed")
            with mock.patch.object(feed_a04_mixed.subprocess, "run", return_value=completed):
                result = feed_a04_mixed.capture_following_query_plan(
                    go_bin="go",
                    dsn="gostream:secret@tcp(127.0.0.1:3306)/fixture",
                    viewer_id=500,
                    mode="sql-only",
                    round_number=1,
                    output_path=output,
                )

            self.assertFalse(result["available"])
            self.assertEqual(result["reason"], "diagnostic_test_failed")
            self.assertFalse(output.exists())

    def test_query_plan_diagnostics_do_not_change_acceptance_result(self):
        reports = []
        for round_number in range(1, 4):
            sql_report = run("sql-only", round_number, 100_000_000)
            hybrid_report = run("hybrid", round_number, 70_000_000)
            if round_number == 1:
                sql_report["following_query_plan_diagnostic"] = {
                    "available": True,
                    "mode": "sql-only",
                    "output_file": "following-plan-sql-only.json",
                }
                hybrid_report["following_query_plan_diagnostic"] = {
                    "available": True,
                    "mode": "hybrid",
                    "output_file": "following-plan-hybrid.json",
                }
            reports.extend((sql_report, hybrid_report))

        summary = feed_a04_mixed.summarize(reports, 0.20)
        rendered = feed_a04_mixed.format_summary(summary)

        self.assertTrue(summary["target_met"])
        self.assertTrue(summary["following_query_plan_diagnostics"]["hybrid"][0]["available"])
        self.assertIn("following-plan-hybrid.json", rendered)

    def test_query_analyze_diagnostics_are_reported_without_changing_acceptance_gate(self):
        reports = []
        for round_number in range(1, 4):
            reports.extend(
                (
                    run("sql-only", round_number, 100_000_000),
                    run("hybrid", round_number, 70_000_000),
                )
            )
        summary = feed_a04_mixed.summarize(reports, 0.20)
        summary["following_query_analyze_diagnostics"] = {
            "sql-only": [
                {
                    "round": 4,
                    "diagnostic": {
                        "available": True,
                        "analyze_available": True,
                        "output_file": "following-plan-sql-only-diagnostic.json",
                    },
                }
            ],
            "hybrid": [
                {
                    "round": 4,
                    "diagnostic": {
                        "available": True,
                        "analyze_available": False,
                        "analyze_reason": "explain analyze rejected",
                    },
                }
            ],
        }

        rendered = feed_a04_mixed.format_summary(summary)

        self.assertTrue(summary["target_met"])
        self.assertIn("SQL-only r4 following-plan-sql-only-diagnostic.json", rendered)
        self.assertIn("Hybrid r4 unavailable (explain analyze rejected)", rendered)

    def test_mysql_dsn_interpolation_is_opt_in(self):
        prefix = "gostream:gostream-ci-only@tcp(127.0.0.1:3306)/"
        self.assertEqual(
            feed_a04_mixed.schema_dsn(prefix, "a04_probe"),
            "gostream:gostream-ci-only@tcp(127.0.0.1:3306)/a04_probe?charset=utf8mb4&parseTime=True&loc=UTC",
        )
        self.assertEqual(
            feed_a04_mixed.schema_dsn(prefix, "a04_probe", interpolate_params=True),
            "gostream:gostream-ci-only@tcp(127.0.0.1:3306)/a04_probe?charset=utf8mb4&parseTime=True&loc=UTC&interpolateParams=true",
        )

    def test_acceptance_summary_reports_publish_count_and_hybrid_fanout_only(self):
        reports = []
        for variant, drain_seconds in (("sql-only", 0.9), ("hybrid", 0.5)):
            for round_number in range(1, 4):
                reports.append(run(variant, round_number, 1_000, fanout_drain_seconds=drain_seconds))
        summary = feed_a04_mixed.summarize(reports, 0.20)
        rendered = feed_a04_mixed.format_summary(summary)
        self.assertIn("20 real API video publishes per run", rendered)
        self.assertIn("maximum Hybrid fanout drain: 0.500s", rendered)

    def test_mixed_driver_uses_one_distinct_reader_per_worker(self):
        names = feed_a04_mixed.driver_reader_names()
        self.assertEqual(len(names), feed_a04_mixed.CONCURRENCY)
        self.assertEqual(len(set(names)), feed_a04_mixed.CONCURRENCY)
        self.assertEqual(names[0], "load-u-000500")
        self.assertEqual(names[-1], "load-u-000519")

    def test_publisher_logins_are_split_across_rate_limit_windows(self):
        publishers = [f"load-u-{index:06d}" for index in range(1, feed_a04_mixed.PUBLISHERS + 1)]
        before, after = feed_a04_mixed.publisher_login_batches(
            reader_count=feed_a04_mixed.CONCURRENCY,
            publisher_names=publishers,
            verifier_count=feed_a04_mixed.FOLLOWERS,
        )
        self.assertEqual(before, publishers[:10])
        self.assertEqual(after, publishers[10:])
        self.assertLessEqual(feed_a04_mixed.CONCURRENCY + len(before), feed_a04_mixed.LOGIN_RATE_BUDGET)
        self.assertLessEqual(len(after) + feed_a04_mixed.FOLLOWERS, feed_a04_mixed.LOGIN_RATE_BUDGET)

    def test_publisher_login_planning_rejects_a_post_load_budget_overflow(self):
        publishers = [f"load-u-{index:06d}" for index in range(1, feed_a04_mixed.PUBLISHERS + 2)]
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "post-load login budget"):
            feed_a04_mixed.publisher_login_batches(
                reader_count=feed_a04_mixed.CONCURRENCY,
                publisher_names=publishers,
                verifier_count=feed_a04_mixed.FOLLOWERS,
            )
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "cannot be negative"):
            feed_a04_mixed.publisher_login_batches(reader_count=-1, publisher_names=[], verifier_count=0)

    def test_request_json_checks_the_endpoint_status_code(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(201)
                self.send_header("Content-Length", "2")
                self.end_headers()
                self.wfile.write(b"{}")

            def log_message(self, format, *args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        url = f"http://127.0.0.1:{server.server_port}/test"
        try:
            with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "expected HTTP 200"):
                feed_a04_mixed.request_json(url)
            self.assertEqual(feed_a04_mixed.request_json(url, expected_status=201), {})
        finally:
            server.shutdown()
            thread.join(timeout=2)
            server.server_close()

    def test_capture_pprof_artifact_writes_successful_payload(self):
        payload = b"compressed profile fixture"

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, format, *args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                destination = pathlib.Path(directory) / "cpu.pb.gz"
                result = feed_a04_mixed.capture_pprof_artifact(
                    f"http://127.0.0.1:{server.server_port}/debug/pprof/profile?seconds=60",
                    destination,
                    timeout_seconds=2,
                )
                self.assertEqual(result, {"captured": True, "path": "cpu.pb.gz", "bytes": len(payload)})
                self.assertEqual(destination.read_bytes(), payload)
        finally:
            server.shutdown()
            thread.join(timeout=2)
            server.server_close()

    def test_capture_pprof_artifact_records_http_failure_without_raising(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_error(503, "profiler unavailable")

            def log_message(self, format, *args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                destination = pathlib.Path(directory) / "cpu.pb.gz"
                result = feed_a04_mixed.capture_pprof_artifact(
                    f"http://127.0.0.1:{server.server_port}/debug/pprof/profile?seconds=60",
                    destination,
                    timeout_seconds=2,
                )
                self.assertFalse(result["captured"])
                self.assertEqual(result["error"], "HTTP 503")
                self.assertNotIn("/debug/pprof", result["error"])
                self.assertFalse(destination.exists())
        finally:
            server.shutdown()
            thread.join(timeout=2)
            server.server_close()

    def test_a04_pprof_capture_runs_separately_from_acceptance_rounds(self):
        schedule = feed_a04_mixed.acceptance_schedule(feed_a04_mixed.DEFAULT_SCHEDULE_SEED)
        self.assertEqual(len(schedule), 6)
        self.assertEqual(feed_a04_mixed.PROFILE_SCHEDULE, (("sql-only", 4), ("hybrid", 4)))
        self.assertFalse(set(schedule) & set(feed_a04_mixed.PROFILE_SCHEDULE))
        self.assertTrue(feed_a04_mixed.should_capture_pprof("sql-only", True))
        self.assertTrue(feed_a04_mixed.should_capture_pprof("hybrid", True))
        self.assertFalse(feed_a04_mixed.should_capture_pprof("sql-only", False))
        self.assertFalse(feed_a04_mixed.should_capture_pprof("unknown", True))

    def test_acceptance_schedule_is_seeded_paired_and_balanced(self):
        schedule = feed_a04_mixed.acceptance_schedule(12345)
        self.assertEqual(schedule, feed_a04_mixed.acceptance_schedule(12345))
        self.assertEqual(len(schedule), 6)
        for round_number in (1, 2, 3):
            pair = schedule[(round_number - 1) * 2:round_number * 2]
            self.assertEqual({mode for mode, number in pair if number == round_number}, {"sql-only", "hybrid"})
            self.assertTrue(all(number == round_number for _, number in pair))
        sql_first = sum(1 for index in (0, 2, 4) if schedule[index][0] == "sql-only")
        self.assertIn(sql_first, (1, 2))
        majority_modes = set()
        for seed in range(32):
            candidate = feed_a04_mixed.acceptance_schedule(seed)
            first_counts = {mode: sum(candidate[index][0] == mode for index in (0, 2, 4)) for mode in ("sql-only", "hybrid")}
            majority_modes.add(max(first_counts, key=first_counts.get))
        self.assertEqual(majority_modes, {"sql-only", "hybrid"})

    def test_stop_api_accepts_intentional_sigterm_but_rejects_crash(self):
        with tempfile.TemporaryFile(mode="w+") as log:
            process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
            feed_a04_mixed.stop_api(process, log, pathlib.Path("api.log"))
            self.assertEqual(process.returncode, -signal.SIGTERM)

        with tempfile.TemporaryFile(mode="w+") as log:
            process = subprocess.Popen([sys.executable, "-c", "raise SystemExit(7)"])
            process.wait(timeout=5)
            with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "status 7"):
                feed_a04_mixed.stop_api(process, log, pathlib.Path("api.log"))

    def test_percentile_matches_existing_a04_index_rule(self):
        self.assertEqual(feed_a04_mixed.percentile([10, 20, 30], 95), 20)
        self.assertEqual(feed_a04_mixed.percentile([], 95), 0)

    def test_workload_ratios_reflect_seventy_twenty_ten_profile(self):
        report = {
            "operations": {
                "feed_following": {"successes": 40, "failures": 0},
                "feed_recommend": {"successes": 15, "failures": 0},
                "feed_hot": {"successes": 15, "failures": 0},
                "like": {"successes": 8, "failures": 0},
                "unlike": {"successes": 7, "failures": 0},
                "favorite": {"successes": 5, "failures": 0},
                "unfavorite": {"successes": 0, "failures": 0},
                "follow": {"successes": 5, "failures": 0},
                "comment": {"successes": 5, "failures": 0},
            }
        }
        self.assertEqual(
            feed_a04_mixed.workload_ratios(report),
            {"feed": 0.7, "interaction": 0.2, "social": 0.1},
        )

    def test_summary_compares_following_p95_medians_and_keeps_write_evidence(self):
        reports = []
        for variant, values in (("sql-only", (1000, 1100, 900)), ("hybrid", (750, 800, 700))):
            for round_number, value in enumerate(values, 1):
                reports.append(run(variant, round_number, value, 2_000_000 + round_number))
        summary = feed_a04_mixed.summarize(reports, 0.20)
        self.assertEqual(summary["sql_only_median_following_p95_ns"], 1000)
        self.assertEqual(summary["hybrid_median_following_p95_ns"], 750)
        self.assertEqual(summary["following_p95_improvement_percent"], 25.0)
        self.assertEqual(summary["runs_per_variant"], 3)
        self.assertEqual(summary["hybrid_publish_p95_ns"], [2_000_001, 2_000_002, 2_000_003])
        self.assertEqual(summary["fanout_drain_seconds"], [0.5] * 6)
        self.assertEqual(len(summary["server_following_latency"]["sql_only"]), 3)
        self.assertEqual(summary["server_following_latency"]["hybrid"][0]["sample_count"], 400)
        summary["schedule_seed"] = 42
        summary["schedule"] = [
            {"mode": "hybrid", "round": 1},
            {"mode": "sql-only", "round": 1},
        ]
        rendered = feed_a04_mixed.format_summary(summary)
        self.assertIn("Schedule seed: 42", rendered)
        self.assertIn("Acceptance run order: hybrid r1, sql-only r1", rendered)
        self.assertIn("API-side successful following P95 bucket", rendered)
        self.assertTrue(summary["target_met"])

    def test_summary_retains_following_api_stage_buckets_per_round(self):
        reports = []
        for variant in ("sql-only", "hybrid"):
            for round_number in range(1, 4):
                reports.append(run(variant, round_number, 1_000))
        summary = feed_a04_mixed.summarize(reports, 0.20)
        self.assertEqual(len(summary["server_feed_stages"]["sql_only"]), 3)
        self.assertEqual(
            summary["server_feed_stages"]["hybrid"][0][0]["stage"],
            "candidate_query",
        )
        rendered = feed_a04_mixed.format_summary(summary)
        self.assertIn("following candidate_query P95 bucket", rendered)
        self.assertIn("following candidate_sql P95 bucket", rendered)
        self.assertIn("following candidate_query_start P95 bucket", rendered)
        self.assertIn("following candidate_row_next P95 bucket", rendered)
        self.assertIn("following candidate_row_scan P95 bucket", rendered)
        self.assertIn("following author_preload P95 bucket", rendered)
        self.assertIn("following interaction_state P95 bucket", rendered)

    def test_summary_retains_database_diagnostics_without_changing_acceptance_gate(self):
        reports = []
        for variant, values in (("sql-only", (1000, 1100, 900)), ("hybrid", (750, 800, 700))):
            for round_number, value in enumerate(values, 1):
                reports.append(run(variant, round_number, value))
        reports[0]["database_statement_diagnostics"] = {
            "available": False,
            "reason": "performance_schema_query_failed",
            "failure_stage": "digest_histogram_query",
            "error": "AcceptanceError: access denied",
            "window": "mixed driver lifetime: 30s read-only warmup plus 60s measured workload",
        }

        summary = feed_a04_mixed.summarize(reports, 0.20)

        self.assertEqual(summary["database_pool_wait"]["sql_only"][0]["wait_count"], 1)
        self.assertEqual(
            summary["database_statement_diagnostics"]["sql_only"][0]["reason"],
            "performance_schema_query_failed",
        )
        self.assertEqual(
            summary["database_statement_diagnostics"]["sql_only"][0]["failure_stage"],
            "digest_histogram_query",
        )
        self.assertTrue(summary["target_met"], "diagnostic availability must not alter the P95 acceptance gate")
        rendered = feed_a04_mixed.format_summary(summary)
        self.assertIn("DB pool delta during 60s measured window", rendered)
        self.assertIn("Performance Schema statement digests", rendered)
        self.assertIn("performance_schema_query_failed; stage=digest_histogram_query", rendered)
        self.assertIn("SELECT * FROM videos WHERE id = ?", rendered)

    def test_summary_retains_warmup_and_measured_digest_windows(self):
        reports = []
        for variant, values in (("sql-only", (1000, 1100, 900)), ("hybrid", (750, 800, 700))):
            for round_number, value in enumerate(values, 1):
                report = run(variant, round_number, value)
                report["database_statement_diagnostics_warmup"] = {
                    "available": True,
                    "window": "30s following warmup",
                    "statement_count": 11,
                }
                report["database_statement_diagnostics_measured"] = {
                    "available": True,
                    "window": "60s mixed workload",
                    "statement_count": 7,
                }
                report["database_wait_diagnostics_measured"] = {
                    "available": True,
                    "window": "60s measured mixed workload",
                    "event_count_total": 9,
                    "all_event_count_total": 12,
                    "events": [
                        {
                            "event_name": "wait/io/file/innodb/innodb_data_file",
                            "count": 9,
                            "sum_timer_wait_ms": 12.5,
                            "mean_timer_wait_ms": 12.5 / 9,
                        }
                    ],
                    "idle_connection_event": {
                        "event_name": "idle",
                        "count": 3,
                        "sum_timer_wait_ms": 500.0,
                        "mean_timer_wait_ms": 500.0 / 3,
                    },
                }
                reports.append(report)

        summary = feed_a04_mixed.summarize(reports, 0.20)

        self.assertEqual(
            summary["database_statement_diagnostics_warmup"]["sql_only"][0]["statement_count"],
            11,
        )
        self.assertEqual(
            summary["database_statement_diagnostics_measured"]["hybrid"][2]["window"],
            "60s mixed workload",
        )
        self.assertEqual(
            summary["database_wait_diagnostics_measured"]["hybrid"][2]["events"][0]["count"],
            9,
        )
        rendered = feed_a04_mixed.format_summary(summary)
        self.assertIn("Performance Schema warmup-only statement digests", rendered)
        self.assertIn("Performance Schema measured-only statement digests", rendered)
        self.assertIn("Performance Schema account waits (measured-only)", rendered)
        self.assertIn("wait/io/file/innodb/innodb_data_file", rendered)
        self.assertIn("idle connection (separate; excluded from ranked waits)", rendered)
        self.assertTrue(summary["target_met"], "phase diagnostics must not alter the P95 acceptance gate")

    def test_warmup_driver_runs_without_measurement_report(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            log_path = root / "warmup.log"
            report_path = root / "warmup.json"
            completed = subprocess.CompletedProcess(
                args=[],
                returncode=0,
                stdout="read-only warmup completed requests=123 duration=30s\n",
                stderr="",
            )
            with mock.patch.object(feed_a04_mixed.subprocess, "run", return_value=completed) as run_process:
                report, _ = feed_a04_mixed.run_mixed_driver(
                    "mixed-driver",
                    "http://127.0.0.1:8080",
                    root / "tokens.txt",
                    [1],
                    [2],
                    report_path,
                    log_path,
                    0,
                    warmup_seconds=30,
                    duration_seconds=0,
                    warmup_only=True,
                )

            command = run_process.call_args.args[0]
            self.assertIn("-warmup-only", command)
            self.assertNotIn("-report-file", command)
            self.assertEqual(command[command.index("-duration") + 1], "0s")
            self.assertEqual(report, {"successes": 123, "duration_seconds": 30})
            self.assertFalse(report_path.exists())
            self.assertIn("requests=123", log_path.read_text())

    def test_prepared_statement_event_snapshot_and_delta_include_execute_command(self):
        before = feed_a04_mixed.parse_account_statement_event_snapshot([
            ["statement/com/Execute", "5", "1000000000", "0", "0", "0", "0", "0"],
            ["statement/com/Prepare", "5", "500000000", "0", "0", "0", "0", "0"],
        ])
        after = feed_a04_mixed.parse_account_statement_event_snapshot([
            ["statement/com/Execute", "8", "4000000000", "0", "2", "6", "0", "0"],
            ["statement/com/Prepare", "8", "900000000", "0", "0", "0", "0", "0"],
            ["statement/sql/select", "3", "600000000", "0", "3", "9", "0", "0"],
        ])

        report = feed_a04_mixed.prepared_statement_event_delta(before, after, "gostream")

        self.assertEqual(report["account_user"], "gostream")
        self.assertEqual(
            report["source"],
            "performance_schema.events_statements_summary_by_account_by_event_name",
        )
        self.assertEqual(report["prepared_execute_count"], 3)
        self.assertEqual(report["prepared_execute_sum_timer_wait_ms"], 3.0)
        self.assertEqual(report["prepared_execute_mean_timer_wait_ms"], 1.0)
        self.assertEqual(report["event_count_total"], 9)
        execute = next(event for event in report["events"] if event["event_name"] == "statement/com/Execute")
        self.assertEqual(execute["sum_rows_examined"], 6)

    def test_prepared_statement_event_snapshot_rejects_invalid_rows_and_counter_resets(self):
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "unexpected account statement event row"):
            feed_a04_mixed.parse_account_statement_event_snapshot([["statement/com/Execute", "1"]])
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "duplicate account statement event"):
            feed_a04_mixed.parse_account_statement_event_snapshot([
                ["statement/com/Execute", "1", "1", "0", "0", "0", "0", "0"],
                ["statement/com/Execute", "1", "1", "0", "0", "0", "0", "0"],
            ])

        before = {"statement/com/Execute": {"count_star": 5, "sum_timer_wait_ps": 5}}
        after = {"statement/com/Execute": {"count_star": 4, "sum_timer_wait_ps": 6}}
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "account statement event counter reset"):
            feed_a04_mixed.prepared_statement_event_delta(before, after, "gostream")

    def test_dsn_account_user_extracts_only_a_simple_mysql_username(self):
        self.assertEqual(
            feed_a04_mixed.dsn_account_user("gostream:gostream-ci-only@tcp(127.0.0.1:3306)/"),
            "gostream",
        )
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "simple account name"):
            feed_a04_mixed.dsn_account_user("bad'user:password@tcp(127.0.0.1:3306)/")

    def test_prepared_event_delta_remains_available_when_digest_consumer_is_disabled(self):
        before = {
            "available": False,
            "reason": "statement_digest_consumer_disabled",
            "account_statement_events": {
                "available": True,
                "account_user": "gostream",
                "snapshot": {"statement/com/Execute": {"count_star": 1, "sum_timer_wait_ps": 2_000_000_000}},
            },
        }
        after = {
            "available": False,
            "reason": "statement_digest_consumer_disabled",
            "account_statement_events": {
                "available": True,
                "account_user": "gostream",
                "snapshot": {"statement/com/Execute": {"count_star": 3, "sum_timer_wait_ps": 5_000_000_000}},
            },
        }

        report = feed_a04_mixed.performance_schema_statement_delta(
            before, after, "60s measured mixed workload"
        )

        self.assertFalse(report["available"])
        self.assertEqual(report["reason"], "statement_digest_consumer_disabled")
        self.assertTrue(report["prepared_statement_diagnostics"]["available"])
        self.assertEqual(report["prepared_statement_diagnostics"]["prepared_execute_count"], 2)
        self.assertEqual(report["window"], "60s measured mixed workload")

    def test_summary_retains_prepared_statement_diagnostics_without_changing_acceptance_gate(self):
        reports = []
        for variant, values in (("sql-only", (1000, 1100, 900)), ("hybrid", (750, 800, 700))):
            for round_number, value in enumerate(values, 1):
                report = run(variant, round_number, value)
                report["database_statement_diagnostics"]["prepared_statement_diagnostics"] = {
                    "available": True,
                    "account_user": "gostream",
                    "prepared_execute_count": round_number * 10,
                    "prepared_execute_sum_timer_wait_ms": 5.0,
                    "prepared_execute_mean_timer_wait_ms": 0.5,
                    "event_count_total": round_number * 30,
                    "events": [],
                }
                reports.append(report)

        summary = feed_a04_mixed.summarize(reports, 0.20)

        self.assertEqual(
            summary["prepared_statement_diagnostics"]["sql_only"][0]["prepared_execute_count"],
            10,
        )
        self.assertTrue(summary["target_met"], "diagnostic availability must not alter the P95 acceptance gate")
        self.assertIn("prepared COM_STMT_EXECUTE", feed_a04_mixed.format_summary(summary))

    def test_summary_retains_pprof_results_without_changing_acceptance_gate(self):
        reports = []
        for variant, following_p95_ns in (("sql-only", 100_000_000), ("hybrid", 40_000_000)):
            for round_number in range(1, 4):
                report = run(variant, round_number, following_p95_ns)
                if round_number == 1:
                    report["mixed_load"]["diagnostic_profiles"] = {
                        "capture_window": "measured workload",
                        "cpu": {"captured": True, "path": "cpu.pb.gz", "bytes": 12},
                        "goroutine": {"captured": False, "error": "test capture unavailable"},
                    }
                reports.append(report)

        summary = feed_a04_mixed.summarize(reports, 0.20)
        self.assertTrue(summary["target_met"], "diagnostic profile status must not change the P95 acceptance gate")
        self.assertEqual(len(summary["diagnostic_profiles"]), 2)
        rendered = feed_a04_mixed.format_summary(summary)
        self.assertIn("sql-only r1 CPU=captured, goroutine=unavailable", rendered)
        self.assertIn("hybrid r1 CPU=captured, goroutine=unavailable", rendered)

    def test_summary_records_mysql_interpolation_experiment_setting(self):
        reports = [
            run(variant, round_number, following_p95_ns=1_000)
            for variant in ("sql-only", "hybrid")
            for round_number in range(1, 4)
        ]
        summary = feed_a04_mixed.summarize(reports, 0.20, interpolate_params=True)
        self.assertTrue(summary["mysql_interpolate_params"])
        self.assertIn("MySQL interpolateParams: enabled", feed_a04_mixed.format_summary(summary))

    def test_summary_fails_below_threshold_or_on_inconsistent_state(self):
        reports = []
        for variant, values in (("sql-only", (1000, 1000, 1000)), ("hybrid", (810, 810, 810))):
            for round_number, value in enumerate(values, 1):
                reports.append(run(variant, round_number, value))
        self.assertFalse(feed_a04_mixed.summarize(reports, 0.20)["target_met"])
        reports[-1]["verification"]["counter_mismatches"] = 1
        summary = feed_a04_mixed.summarize(reports, 0.10)
        self.assertFalse(summary["all_runs_consistent"])
        self.assertFalse(summary["target_met"])

    def test_summary_requires_three_runs_per_mode(self):
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "three complete runs"):
            feed_a04_mixed.summarize([run("sql-only", 1, 1000)], 0.20)


if __name__ == "__main__":
    unittest.main()
