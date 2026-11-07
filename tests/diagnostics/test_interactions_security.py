"""Diagnostic unit tests for interactions & security closure runner."""

import json
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

import interactions_security_smoke


class InteractionsSecuritySmokeUnitTest(unittest.TestCase):
    def test_free_port_allocation(self):
        port = interactions_security_smoke.free_port()
        self.assertIsInstance(port, int)
        self.assertGreater(port, 1024)
        self.assertLessEqual(port, 65535)

    def test_report_schema_and_serialization(self):
        sample_report = {
            "checkpoints": {
                "interactions_idempotency": "PASS",
                "comment_hierarchy": "PASS",
                "play_rate_limiting": "PASS",
                "security_negative_boundaries": "PASS",
            },
            "interactions_idempotent": True,
            "comments_hierarchy_verified": True,
            "play_rate_limit_verified": True,
            "security_boundaries_verified": True,
            "mode": "sqlite",
            "elapsed_seconds": 2.5,
        }
        encoded = json.dumps(sample_report)
        decoded = json.loads(encoded)
        self.assertEqual(len(decoded["checkpoints"]), 4)
        self.assertTrue(decoded["interactions_idempotent"])
        self.assertTrue(decoded["comments_hierarchy_verified"])
        self.assertTrue(decoded["play_rate_limit_verified"])
        self.assertTrue(decoded["security_boundaries_verified"])

    def test_json_call_failure_raises(self):
        with patch("interactions_security_smoke.call_api", return_value=(403, {"error": "forbidden"}, None)):
            with self.assertRaises(RuntimeError) as ctx:
                interactions_security_smoke.json_call("http://127.0.0.1:8080", "/api/protected", expected=200)
            self.assertIn("expected 200, got 403", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
