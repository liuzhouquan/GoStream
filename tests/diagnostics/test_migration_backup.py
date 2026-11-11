"""Diagnostic unit tests for Migration, Backup & Recovery Runner (G10 / Gate A10)."""

import json
import copy
from pathlib import Path
import sys
import unittest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

import migration_backup_recovery_smoke


class MigrationBackupDiagnosticUnitTest(unittest.TestCase):
    def test_free_port_allocation(self):
        port = migration_backup_recovery_smoke.find_free_port()
        self.assertIsInstance(port, int)
        self.assertGreater(port, 1024)
        self.assertLessEqual(port, 65535)

    def test_report_schema_and_serialization(self):
        report_file = ROOT / "data/diagnostics/migration_backup_recovery_smoke.json"
        if not report_file.exists():
            self.skipTest("migration_backup_recovery_smoke.json has not been generated yet")

        report = json.loads(report_file.read_text())
        self.assert_recovery_report(report)

    def assert_recovery_report(self, report):
        self.assertIn("status", report)
        self.assertIn("mode", report)
        self.assertIn("checkpoints", report)
        self.assertIn("metrics", report)
        self.assertIn("rpo_rto_spec", report)

        checkpoints = report["checkpoints"]
        required_checkpoints = [
            "schema_migration_keys",
            "sql_identity_sets_preserved",
            "media_objects_integrity",
            "post_restore_live_api_health",
            "resumable_upload_completion_after_restore",
            "idempotency_and_migration_replay_safety",
        ]
        for cp in required_checkpoints:
            self.assertIn(cp, checkpoints)
            self.assertTrue(checkpoints[cp], f"checkpoint {cp} was not true")

        metrics = report["metrics"]
        self.assertGreater(metrics["backup_duration_ms"], 0)
        self.assertGreater(metrics["restore_duration_ms"], 0)
        self.assertGreater(metrics["sql_backup_bytes"], 0)
        self.assertGreater(metrics["media_objects_bytes"], 0)
        # The formal fixture adds 100 shared objects; compare against the actual
        # frozen SQL proof instead of assuming the old ten-file local fixture.
        self.assertEqual(metrics["media_assets_count"],
                         report["sql_snapshot"]["media_assets"]["rows"])
        self.assertEqual(metrics["all_bucket_objects_verified"],
                         metrics["media_assets_count"] + 1)  # one open upload chunk
        self.assertEqual(metrics["sql_rows_verified"],
                         sum(table["rows"] for table in report["sql_snapshot"].values()))
        self.assertEqual(metrics["users_count"], 5)
        self.assertEqual(metrics["videos_count"], 10)

        rpo_rto = report["rpo_rto_spec"]
        self.assertIn("measured_rpo", rpo_rto)
        self.assertIn("measured_rto_ms", rpo_rto)
        self.assertIn("online_backup_limitation", rpo_rto)

    def test_formal_report_schema_and_corrupt_aggregate_rejection(self):
        report = json.loads((ROOT / "docs/benchmarks/pr64-recovery-verified-2026-11-11.json").read_text())
        self.assert_recovery_report(report)
        for field in ("media_assets_count", "all_bucket_objects_verified", "sql_rows_verified"):
            with self.subTest(field=field):
                corrupt = copy.deepcopy(report)
                corrupt["metrics"][field] += 1
                with self.assertRaises(AssertionError):
                    self.assert_recovery_report(corrupt)

    def test_json_call_failure_raises(self):
        with self.assertRaises(Exception):
            migration_backup_recovery_smoke.json_call("http://127.0.0.1:9", "/test", expected=200)


if __name__ == "__main__":
    unittest.main()
