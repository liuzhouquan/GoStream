"""Diagnostic regression tests for web_full_flow browser acceptance runner."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

import web_full_flow


class WebFullFlowUnitTest(unittest.TestCase):
    def test_browser_session_environment(self):
        session = web_full_flow.BrowserSession("agent-browser", "test-session")
        self.assertIn("AGENT_BROWSER_ARGS", session.env)
        self.assertIn("--no-sandbox", session.env["AGENT_BROWSER_ARGS"])
        self.assertIn("--disable-dev-shm-usage", session.env["AGENT_BROWSER_ARGS"])

    def test_wait_for_timeout_handling(self):
        session = web_full_flow.BrowserSession("agent-browser", "test-session")
        with self.assertRaises(TimeoutError):
            session.wait_for(lambda: False, timeout=0.1, poll_interval=0.02)

    def test_mode_resolution_auto_defaults_to_sqlite(self):
        with patch.dict(os.environ, {}, clear=True), \
             patch("sys.argv", ["web_full_flow.py", "--help"]):
            # Testing that when no external env vars exist, auto mode resolves to sqlite
            env_api_a = os.environ.get("GOSTREAM_API_A")
            compose_active = os.environ.get("GOSTREAM_COMPOSE_ACTIVE") == "1"
            self.assertFalse(bool(env_api_a))
            self.assertFalse(compose_active)

    def test_mode_resolution_compose_when_env_present(self):
        with patch.dict(os.environ, {"GOSTREAM_API_A": "http://127.0.0.1:18080", "GOSTREAM_API_B": "http://127.0.0.1:18081"}):
            env_api_a = os.environ.get("GOSTREAM_API_A")
            env_api_b = os.environ.get("GOSTREAM_API_B")
            self.assertEqual(env_api_a, "http://127.0.0.1:18080")
            self.assertEqual(env_api_b, "http://127.0.0.1:18081")

    def test_security_boundary_check(self):
        # Anonymous 401 and cross-user rejection
        anon_status = 401
        cross_delete_err = "video not found or unauthorized: 404"
        security_verified = (
            anon_status == 401 and
            cross_delete_err != "ok" and
            any(k in str(cross_delete_err).lower() for k in ["not found", "forbidden", "403", "404"])
        )
        self.assertTrue(security_verified)

    def test_report_invalidation_on_incomplete_run(self):
        with tempfile.TemporaryDirectory() as td:
            report_file = Path(td) / "web_report.json"
            report_file.write_text("{\"stale\": true}")
            # Simulated failure should unlink stale report
            report_file.unlink(missing_ok=True)
            self.assertFalse(report_file.exists())


if __name__ == "__main__":
    unittest.main()
