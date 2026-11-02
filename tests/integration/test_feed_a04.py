import os
import unittest
from unittest.mock import patch

import feed_a04


class FeedA04HelpersTest(unittest.TestCase):
    def test_process_stat_and_metrics_report_cpu_and_peak_rss(self):
        cpu_ticks, rss_kib = feed_a04.process_stat(os.getpid())
        self.assertGreaterEqual(cpu_ticks, 0)
        self.assertGreater(rss_kib, 0)

        metrics = feed_a04.process_metrics([(0.0, 100, 1000), (2.0, 120, 4096)])
        self.assertEqual(metrics["cpu_seconds"], 0.2)
        self.assertEqual(metrics["average_cpu_percent"], 10.0)
        self.assertEqual(metrics["peak_rss_kib"], 4096)

    @patch("feed_a04.read_json")
    def test_following_page_ids_compares_two_full_unique_pages(self, read_json):
        read_json.side_effect = [
            {"items": [{"id": value} for value in range(1, 21)], "next_cursor": "cursor", "has_more": True},
            {"items": [{"id": value} for value in range(21, 41)], "has_more": True},
        ]
        self.assertEqual(feed_a04.following_page_ids("http://127.0.0.1", "token"), list(range(1, 41)))
        self.assertIn("cursor=cursor", read_json.call_args_list[1].args[0])

    @patch("feed_a04.read_json")
    def test_following_page_ids_rejects_duplicate_video_ids(self, read_json):
        read_json.side_effect = [
            {"items": [{"id": value} for value in range(1, 21)], "next_cursor": "cursor"},
            {"items": [{"id": value} for value in range(20, 40)]},
        ]
        with self.assertRaisesRegex(feed_a04.AcceptanceError, "duplicate video IDs"):
            feed_a04.following_page_ids("http://127.0.0.1", "token")

    def test_summary_uses_median_p95_and_enforces_twenty_percent_gate(self):
        reports = []
        for variant, values in (("sql-only", (100, 110, 90)), ("hybrid", (75, 80, 70))):
            for round_number, p95 in enumerate(values, 1):
                reports.append(
                    {
                        "variant": variant,
                        "round": round_number,
                        "p95_microseconds": p95,
                        "successes": 100,
                        "failures": 0,
                        "api_process": {"average_cpu_percent": 25.0, "peak_rss_kib": 50000},
                    }
                )
        result = feed_a04.summarize(reports, 0.20)
        self.assertEqual(result["sql_only_median_p95_microseconds"], 100)
        self.assertEqual(result["hybrid_median_p95_microseconds"], 75)
        self.assertEqual(result["hybrid_p95_improvement_percent"], 25.0)
        self.assertTrue(result["target_met"])

        below_gate = [
            {**report, "p95_microseconds": 85}
            if report["variant"] == "hybrid"
            else report
            for report in reports
        ]
        self.assertFalse(feed_a04.summarize(below_gate, 0.20)["target_met"])


if __name__ == "__main__":
    unittest.main()
