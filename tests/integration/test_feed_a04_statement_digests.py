import unittest
from unittest import mock

import feed_a04_mixed


class FeedA04StatementDigestTests(unittest.TestCase):
    def test_digest_delta_scopes_reused_digest_to_schema(self):
        digest_text_hex = b"SELECT * FROM videos WHERE id = ?".hex().upper()
        before = feed_a04_mixed.parse_statement_digest_snapshot(
            [
                ["a04_schema_a", "D1", digest_text_hex, "1", "1000000000", "0", "1", "10", "0", "0"],
                ["a04_schema_b", "D1", digest_text_hex, "1", "2000000000", "0", "1", "20", "0", "0"],
            ],
            [
                ["a04_schema_a", "D1", "1", "0", "1000000000", "1"],
                ["a04_schema_b", "D1", "1", "0", "1000000000", "1"],
            ],
        )
        after = feed_a04_mixed.parse_statement_digest_snapshot(
            [
                ["a04_schema_a", "D1", digest_text_hex, "2", "3000000000", "0", "2", "20", "0", "0"],
                ["a04_schema_b", "D1", digest_text_hex, "2", "5000000000", "0", "2", "40", "0", "0"],
            ],
            [
                ["a04_schema_a", "D1", "1", "0", "1000000000", "2"],
                ["a04_schema_b", "D1", "1", "0", "1000000000", "2"],
            ],
        )

        report = feed_a04_mixed.statement_digest_delta(before, after)

        self.assertEqual(report["statement_count"], 2)
        self.assertEqual(
            {(item["schema_name"], item["digest"], item["count"]) for item in report["top_statements"]},
            {("a04_schema_a", "D1", 1), ("a04_schema_b", "D1", 1)},
        )

    def test_digest_histogram_collapses_identical_duplicate_bucket_rows(self):
        snapshot = feed_a04_mixed.parse_statement_digest_snapshot(
            [],
            [
                ["a04_schema_a", "D1", "1", "0", "1000000000", "0"],
                ["a04_schema_a", "D1", "1", "0", "1000000000", "0"],
            ],
        )

        self.assertEqual(
            snapshot["histograms"],
            {("a04_schema_a", "D1"): {1: {"low_ps": 0, "high_ps": 1_000_000_000, "count": 0}}},
        )

    def test_digest_histogram_conflicting_duplicate_error_identifies_full_key(self):
        with self.assertRaisesRegex(
            feed_a04_mixed.AcceptanceError,
            r"conflicting duplicate bucket.*schema='a04_schema_a'.*digest=D1.*bucket=1",
        ):
            feed_a04_mixed.parse_statement_digest_snapshot(
                [],
                [
                    ["a04_schema_a", "D1", "1", "0", "1000000000", "1"],
                    ["a04_schema_a", "D1", "1", "0", "1000000000", "2"],
                ],
            )

    def test_digest_delta_reports_statement_latency_rows_and_interval_histogram(self):
        sql = "SELECT * FROM videos WHERE id = ?"
        digest_text_hex = sql.encode().hex().upper()
        follow_count_sql = "SELECT COUNT(*) FROM follows"
        schema_name = "gostream_feed_a04_mixed_123_1"
        before = feed_a04_mixed.parse_statement_digest_snapshot(
            [[schema_name, "D1", digest_text_hex, "2", "4000000000", "1000000000", "10", "100", "0", "0"]],
            [
                [schema_name, "D1", "1", "0", "1000000000", "2"],
                [schema_name, "D1", "2", "1000000000", "2000000000", "0"],
            ],
        )
        after = feed_a04_mixed.parse_statement_digest_snapshot(
            [
                [schema_name, "D1", digest_text_hex, "5", "14000000000", "2000000000", "20", "400", "1", "0"],
                [schema_name, "D2", follow_count_sql.encode().hex().upper(), "2", "10000000000", "0", "2", "80", "0", "0"],
            ],
            [
                [schema_name, "D1", "1", "0", "1000000000", "2"],
                [schema_name, "D1", "2", "1000000000", "2000000000", "3"],
                [schema_name, "D2", "2", "1000000000", "2000000000", "1"],
                [schema_name, "D2", "3", "2000000000", "4000000000", "1"],
            ],
        )

        report = feed_a04_mixed.statement_digest_delta(before, after)

        self.assertEqual(report["statement_count"], 5)
        self.assertEqual(report["sum_timer_wait_ms"], 20.0)
        digests = {item["digest"]: item for item in report["top_statements"]}
        self.assertEqual(digests["D1"]["statement"], sql)
        self.assertEqual(digests["D1"]["schema_name"], schema_name)
        self.assertEqual(digests["D1"]["count"], 3)
        self.assertEqual(digests["D1"]["mean_timer_wait_ms"], 10 / 3)
        self.assertEqual(digests["D1"]["sum_rows_examined"], 300)
        self.assertEqual(digests["D1"]["p95_bucket_ms"], [1.0, 2.0])
        self.assertEqual(digests["D2"]["statement"], follow_count_sql)
        self.assertEqual(digests["D2"]["p95_bucket_ms"], [2.0, 4.0])

    def test_digest_delta_rejects_performance_schema_counter_reset(self):
        schema_name = "gostream_feed_a04_mixed_123_1"
        before = feed_a04_mixed.parse_statement_digest_snapshot(
            [[schema_name, "D1", "", "5", "1000000000", "0", "0", "0", "0", "0"]], []
        )
        after = feed_a04_mixed.parse_statement_digest_snapshot(
            [[schema_name, "D1", "", "2", "500000000", "0", "0", "0", "0", "0"]], []
        )

        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "counter reset"):
            feed_a04_mixed.statement_digest_delta(before, after)

    def test_digest_snapshot_rejects_invalid_rows(self):
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "unexpected statement digest row"):
            feed_a04_mixed.parse_statement_digest_snapshot([["schema", "D1", "abcd"]], [])

    def test_statement_delta_preserves_unavailable_diagnostic_reason(self):
        report = feed_a04_mixed.performance_schema_statement_delta(
            {
                "available": False,
                "reason": "performance_schema_query_failed",
                "failure_stage": "digest_histogram_query",
                "error": "AcceptanceError: access denied",
            },
            {"available": False, "reason": "snapshot_unavailable"},
        )
        self.assertEqual(
            report,
            {
                "available": False,
                "reason": "performance_schema_query_failed",
                "failure_stage": "digest_histogram_query",
                "error": "AcceptanceError: access denied",
                "prepared_statement_diagnostics": {
                    "available": False,
                    "reason": "account_event_snapshot_unavailable",
                },
            },
        )

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=0)
    def test_snapshot_reports_performance_schema_disabled_without_querying_it(self, scalar):
        with mock.patch.object(feed_a04_mixed, "mysql_query") as mysql_query:
            report = feed_a04_mixed.performance_schema_statement_snapshot(
                "mysql-a04", "gostream_feed_a04_mixed_123_1", "gostream"
            )
        self.assertEqual(
            report,
            {
                "available": False,
                "reason": "performance_schema_disabled",
                "account_statement_events": {"available": False, "reason": "performance_schema_disabled"},
            },
        )
        mysql_query.assert_not_called()
        scalar.assert_called_once_with("mysql-a04", "SELECT @@performance_schema;")

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=1)
    @mock.patch.object(feed_a04_mixed, "mysql_query")
    def test_snapshot_reports_digest_consumer_disabled(self, mysql_query, _scalar):
        mysql_query.side_effect = [[], [["NO"]]]
        report = feed_a04_mixed.performance_schema_statement_snapshot(
            "mysql-a04", "gostream_feed_a04_mixed_123_1", "gostream"
        )
        self.assertEqual(report["available"], False)
        self.assertEqual(report["reason"], "statement_digest_consumer_disabled")
        self.assertTrue(report["account_statement_events"]["available"])
        self.assertEqual(report["account_statement_events"]["snapshot"], {})
        mysql_query.assert_has_calls([
            mock.call(
                "mysql-a04",
                "SELECT EVENT_NAME, SUM(COUNT_STAR), SUM(SUM_TIMER_WAIT), SUM(SUM_LOCK_TIME), "
                "SUM(SUM_ROWS_SENT), SUM(SUM_ROWS_EXAMINED), SUM(SUM_NO_INDEX_USED), SUM(SUM_NO_GOOD_INDEX_USED) "
                "FROM performance_schema.events_statements_summary_by_account_by_event_name "
                "WHERE USER='gostream' AND (EVENT_NAME LIKE 'statement/com/%' OR EVENT_NAME LIKE 'statement/sql/%') "
                "GROUP BY EVENT_NAME ORDER BY EVENT_NAME;",
            ),
            mock.call(
            "mysql-a04",
            "SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME='statements_digest';",
            ),
        ])

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=1)
    @mock.patch.object(feed_a04_mixed, "mysql_query")
    def test_snapshot_reads_only_the_isolated_schema_digest(self, mysql_query, _scalar):
        sql = "SELECT * FROM videos WHERE id = ?"
        schema_name = "gostream_feed_a04_mixed_123_1"
        mysql_query.side_effect = [
            [],
            [["YES"]],
            [[schema_name, "D1", sql.encode().hex().upper(), "3", "9000000000", "1000000000", "60", "300", "0", "0"]],
            [
                [schema_name, "D1", "1", "0", "1000000000", "2"],
                [schema_name, "D1", "2", "1000000000", "2000000000", "1"],
            ],
        ]

        result = feed_a04_mixed.performance_schema_statement_snapshot(
            "mysql-a04", "gostream_feed_a04_mixed_123_1", "gostream"
        )

        self.assertTrue(result["available"])
        self.assertEqual(result["snapshot"]["statements"][(schema_name, "D1")]["statement"], sql)
        self.assertIn("WHERE SCHEMA_NAME='gostream_feed_a04_mixed_123_1'", mysql_query.call_args_list[2].args[1])
        self.assertIn("WHERE SCHEMA_NAME='gostream_feed_a04_mixed_123_1'", mysql_query.call_args_list[3].args[1])
        self.assertIn("SELECT SCHEMA_NAME, DIGEST,", mysql_query.call_args_list[2].args[1])
        histogram_query = mysql_query.call_args_list[3].args[1]
        self.assertIn(
            "SELECT SCHEMA_NAME, DIGEST, BUCKET_NUMBER, BUCKET_TIMER_LOW, BUCKET_TIMER_HIGH, MAX(COUNT_BUCKET)",
            histogram_query,
        )
        self.assertIn(
            "GROUP BY SCHEMA_NAME, DIGEST, BUCKET_NUMBER, BUCKET_TIMER_LOW, BUCKET_TIMER_HIGH",
            histogram_query,
        )
        self.assertIn("DIGEST IS NOT NULL", mysql_query.call_args_list[2].args[1])
        self.assertIn("DIGEST IS NOT NULL", mysql_query.call_args_list[3].args[1])

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=1)
    @mock.patch.object(feed_a04_mixed, "mysql_query")
    def test_snapshot_treats_performance_schema_query_failure_as_diagnostic_unavailable(self, mysql_query, _scalar):
        def query(_container, sql):
            if "events_statements_summary_by_account_by_event_name" in sql:
                return []
            raise feed_a04_mixed.AcceptanceError("container stopped")

        mysql_query.side_effect = query
        result = feed_a04_mixed.performance_schema_statement_snapshot(
            "mysql-a04", "gostream_feed_a04_mixed_123_1", "gostream"
        )
        self.assertFalse(result["available"])
        self.assertEqual(result["reason"], "performance_schema_query_failed")
        self.assertTrue(result["account_statement_events"]["available"])

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=1)
    @mock.patch.object(feed_a04_mixed, "mysql_query")
    def test_snapshot_reports_digest_failure_stage_and_redacts_credentials(self, mysql_query, _scalar):
        def query(_container, sql):
            if "events_statements_summary_by_account_by_event_name" in sql:
                return []
            if "setup_consumers" in sql:
                return [["YES"]]
            raise feed_a04_mixed.AcceptanceError(
                "Lost connection; MYSQL_PWD=fixture-secret password:other-secret "
                "dsn=gostream:third-secret@tcp(localhost:3306)/a04 token=fixture-token"
            )

        mysql_query.side_effect = query
        result = feed_a04_mixed.performance_schema_statement_snapshot(
            "mysql-a04", "gostream_feed_a04_mixed_123_1", "gostream"
        )

        self.assertFalse(result["available"])
        self.assertEqual(result["failure_stage"], "digest_summary_query")
        self.assertIn("Lost connection", result["error"])
        self.assertNotIn("fixture-secret", result["error"])
        self.assertNotIn("other-secret", result["error"])
        self.assertNotIn("third-secret", result["error"])
        self.assertNotIn("fixture-token", result["error"])

    @mock.patch.object(feed_a04_mixed, "scalar", side_effect=ValueError("unexpected scalar result"))
    def test_snapshot_treats_unparseable_capability_result_as_unavailable(self, _scalar):
        result = feed_a04_mixed.performance_schema_statement_snapshot(
            "mysql-a04", "gostream_feed_a04_mixed_123_1", "gostream"
        )
        self.assertEqual(
            result,
            {
                "available": False,
                "reason": "performance_schema_query_failed",
                "failure_stage": "performance_schema_capability_check",
                "error": "ValueError: unexpected scalar result",
                "account_statement_events": {
                    "available": False,
                    "reason": "performance_schema_capability_check_failed",
                },
            },
        )

    def test_snapshot_refuses_non_a04_database_prefix(self):
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "outside the isolated A04 mixed prefix"):
            feed_a04_mixed.performance_schema_statement_snapshot("mysql-a04", "gostream", "gostream")


if __name__ == "__main__":
    unittest.main()
