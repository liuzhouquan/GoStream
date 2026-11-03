import unittest
from unittest import mock

import feed_a04_mixed


class FeedA04StatementDigestTests(unittest.TestCase):
    def test_digest_delta_reports_statement_latency_rows_and_interval_histogram(self):
        sql = "SELECT * FROM videos WHERE id = ?"
        digest_text_hex = sql.encode().hex().upper()
        follow_count_sql = "SELECT COUNT(*) FROM follows"
        before = feed_a04_mixed.parse_statement_digest_snapshot(
            [["D1", digest_text_hex, "2", "4000000000", "1000000000", "10", "100", "0", "0"]],
            [["D1", "1", "0", "1000000000", "2"], ["D1", "2", "1000000000", "2000000000", "0"]],
        )
        after = feed_a04_mixed.parse_statement_digest_snapshot(
            [
                ["D1", digest_text_hex, "5", "14000000000", "2000000000", "20", "400", "1", "0"],
                ["D2", follow_count_sql.encode().hex().upper(), "2", "10000000000", "0", "2", "80", "0", "0"],
            ],
            [
                ["D1", "1", "0", "1000000000", "2"],
                ["D1", "2", "1000000000", "2000000000", "3"],
                ["D2", "2", "1000000000", "2000000000", "1"],
                ["D2", "3", "2000000000", "4000000000", "1"],
            ],
        )

        report = feed_a04_mixed.statement_digest_delta(before, after)

        self.assertEqual(report["statement_count"], 5)
        self.assertEqual(report["sum_timer_wait_ms"], 20.0)
        digests = {item["digest"]: item for item in report["top_statements"]}
        self.assertEqual(digests["D1"]["statement"], sql)
        self.assertEqual(digests["D1"]["count"], 3)
        self.assertEqual(digests["D1"]["mean_timer_wait_ms"], 10 / 3)
        self.assertEqual(digests["D1"]["sum_rows_examined"], 300)
        self.assertEqual(digests["D1"]["p95_bucket_ms"], [1.0, 2.0])
        self.assertEqual(digests["D2"]["statement"], follow_count_sql)
        self.assertEqual(digests["D2"]["p95_bucket_ms"], [2.0, 4.0])

    def test_digest_delta_rejects_performance_schema_counter_reset(self):
        before = feed_a04_mixed.parse_statement_digest_snapshot(
            [["D1", "", "5", "1000000000", "0", "0", "0", "0", "0"]], []
        )
        after = feed_a04_mixed.parse_statement_digest_snapshot(
            [["D1", "", "2", "500000000", "0", "0", "0", "0", "0"]], []
        )

        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "counter reset"):
            feed_a04_mixed.statement_digest_delta(before, after)

    def test_digest_snapshot_rejects_invalid_rows(self):
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "unexpected statement digest row"):
            feed_a04_mixed.parse_statement_digest_snapshot([["D1", "abcd"]], [])

    def test_statement_delta_preserves_unavailable_diagnostic_reason(self):
        report = feed_a04_mixed.performance_schema_statement_delta(
            {"available": False, "reason": "statement_digest_consumer_disabled"},
            {"available": False, "reason": "snapshot_unavailable"},
        )
        self.assertEqual(
            report,
            {"available": False, "reason": "statement_digest_consumer_disabled"},
        )

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=0)
    def test_snapshot_reports_performance_schema_disabled_without_querying_it(self, scalar):
        with mock.patch.object(feed_a04_mixed, "mysql_query") as mysql_query:
            report = feed_a04_mixed.performance_schema_statement_snapshot(
                "mysql-a04", "gostream_feed_a04_mixed_123_1"
            )
        self.assertEqual(report, {"available": False, "reason": "performance_schema_disabled"})
        mysql_query.assert_not_called()
        scalar.assert_called_once_with("mysql-a04", "SELECT @@performance_schema;")

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=1)
    @mock.patch.object(feed_a04_mixed, "mysql_query", return_value=[])
    def test_snapshot_reports_digest_consumer_disabled(self, mysql_query, _scalar):
        report = feed_a04_mixed.performance_schema_statement_snapshot(
            "mysql-a04", "gostream_feed_a04_mixed_123_1"
        )
        self.assertEqual(report, {"available": False, "reason": "statement_digest_consumer_disabled"})
        mysql_query.assert_called_once_with(
            "mysql-a04",
            "SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME='statements_digest';",
        )

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=1)
    @mock.patch.object(feed_a04_mixed, "mysql_query")
    def test_snapshot_reads_only_the_isolated_schema_digest(self, mysql_query, _scalar):
        sql = "SELECT * FROM videos WHERE id = ?"
        mysql_query.side_effect = [
            [["YES"]],
            [["D1", sql.encode().hex().upper(), "3", "9000000000", "1000000000", "60", "300", "0", "0"]],
            [["D1", "1", "0", "1000000000", "2"], ["D1", "2", "1000000000", "2000000000", "1"]],
        ]

        result = feed_a04_mixed.performance_schema_statement_snapshot(
            "mysql-a04", "gostream_feed_a04_mixed_123_1"
        )

        self.assertTrue(result["available"])
        self.assertEqual(result["snapshot"]["statements"]["D1"]["statement"], sql)
        self.assertIn("WHERE SCHEMA_NAME='gostream_feed_a04_mixed_123_1'", mysql_query.call_args_list[1].args[1])
        self.assertIn("WHERE SCHEMA_NAME='gostream_feed_a04_mixed_123_1'", mysql_query.call_args_list[2].args[1])

    @mock.patch.object(feed_a04_mixed, "scalar", return_value=1)
    @mock.patch.object(feed_a04_mixed, "mysql_query")
    def test_snapshot_treats_performance_schema_query_failure_as_diagnostic_unavailable(self, mysql_query, _scalar):
        mysql_query.side_effect = [[['YES']], feed_a04_mixed.AcceptanceError("container stopped")]
        result = feed_a04_mixed.performance_schema_statement_snapshot(
            "mysql-a04", "gostream_feed_a04_mixed_123_1"
        )
        self.assertEqual(
            result,
            {"available": False, "reason": "performance_schema_query_failed"},
        )

    def test_snapshot_refuses_non_a04_database_prefix(self):
        with self.assertRaisesRegex(feed_a04_mixed.AcceptanceError, "outside the isolated A04 mixed prefix"):
            feed_a04_mixed.performance_schema_statement_snapshot("mysql-a04", "gostream")


if __name__ == "__main__":
    unittest.main()
