"""Diagnostic unit tests for Component Outage & Persistence Runner (G09 / A01-A10)."""

import json
from pathlib import Path
import re
import subprocess
import sys
import unittest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

import component_outage_smoke


class ComponentOutageDiagnosticUnitTest(unittest.TestCase):
    def test_free_port_allocation(self):
        port = component_outage_smoke.find_free_port()
        self.assertIsInstance(port, int)
        self.assertGreater(port, 1024)
        self.assertLessEqual(port, 65535)

    def test_test_uuid_format(self):
        uuid_pattern = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
        for i in [0, 1, 10, 99]:
            u = component_outage_smoke.test_uuid(i)
            self.assertEqual(len(u), 36)
            self.assertTrue(uuid_pattern.match(u), f"UUID {u} does not match 36-char format")

    def test_report_schema_and_serialization(self):
        report_file = ROOT / "data/diagnostics/component_outage_smoke.json"
        if not report_file.exists():
            self.skipTest("component_outage_smoke.json has not been generated yet")

        report = json.loads(report_file.read_text())
        self.assertIn("status", report)
        self.assertIn("mode", report)
        self.assertIn("endpoints", report)
        self.assertIn("checkpoints", report)
        self.assertIn("elapsed_seconds", report)

        checkpoints = report["checkpoints"]
        required_checkpoints = [
            "health_ready_semantics",
            "redis_outage_fallback_and_recovery",
            "rabbitmq_outage_outbox_retention_and_delivery",
            "minio_outage_isolation_and_recovery",
            "api_restart_and_lease_recovery",
        ]
        for cp in required_checkpoints:
            self.assertIn(cp, checkpoints)
            self.assertTrue(checkpoints[cp], f"checkpoint {cp} was not true")

    def test_websocket_masking_roundtrip(self):
        test_payload = b'{"type":"ping","timestamp":123456789}'
        mask = b"\x12\x34\x56\x78"
        masked = bytearray(b ^ mask[i % 4] for i, b in enumerate(test_payload))
        unmasked = bytearray(b ^ mask[i % 4] for i, b in enumerate(masked))
        self.assertEqual(bytes(unmasked), test_payload)

    def test_json_call_failure_raises(self):
        # json_call should raise when expected status doesn't match
        # Simulated using non-existent port
        with self.assertRaises(Exception):
            component_outage_smoke.json_call("http://127.0.0.1:9", "/test", expected=200)

    def test_seed_feed_benchmark_dsn_redaction(self):
        cmd = ["/usr/local/go/bin/go", "run", "./tools/seed_feed_benchmark.go", "-driver", "mysql", "-dsn", "fake_user:my_secret_token@tcp(127.0.0.1:99999)/gostream"]
        res = subprocess.run(cmd, cwd=str(ROOT), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.assertNotEqual(res.returncode, 0)
        output = res.stdout + res.stderr
        self.assertNotIn("my_secret_token", output)
        self.assertIn("fake_user:***@tcp(127.0.0.1:99999)/gostream", output)

    def test_seed_group_im_benchmark_dsn_redaction(self):
        cmd = ["/usr/local/go/bin/go", "run", "./tools/seed_group_im_benchmark.go", "-driver", "mysql", "-dsn", "fake_user:my_secret_token@tcp(127.0.0.1:99999)/gostream"]
        res = subprocess.run(cmd, cwd=str(ROOT), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.assertNotEqual(res.returncode, 0)
        output = res.stdout + res.stderr
        self.assertNotIn("my_secret_token", output)
        self.assertIn("fake_user:***@tcp(127.0.0.1:99999)/gostream", output)

    def test_source_contains_finally_blocks_for_all_outages(self):
        source = (ROOT / "tests/integration/component_outage_smoke.py").read_text()
        # Ensure restore calls are executed in finally blocks
        self.assertIn('finally:\n            # Clean up author WebSocket\n            try:\n                ws_author.close()\n            except Exception:\n                pass\n\n            # 2f. Restore Redis\n            if self.mode == "compose":\n                self.stack.run_compose("start", "redis")', source)
        self.assertIn('finally:\n            # 3e. Restore RabbitMQ in compose mode\n            if self.mode == "compose":\n                self.stack.run_compose("start", "rabbitmq")', source)
        self.assertIn('finally:\n            # 4c. Restore MinIO / write permissions\n            if self.mode == "compose":\n                self.stack.run_compose("start", "minio")', source)



if __name__ == "__main__":
    unittest.main()
