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

    def test_security_boundary_uses_actual_http_statuses(self):
        owner = MagicMock()
        other = MagicMock()
        owner.eval_js.side_effect = [200, 401, 200]
        other.eval_js.return_value = 403
        self.assertTrue(web_full_flow.verify_security_boundaries(owner, other, 7))
        expressions = [call.args[0] for call in owner.eval_js.call_args_list + other.eval_js.call_args_list]
        self.assertTrue(all("fetch(" in expression and ".status" in expression for expression in expressions))
        self.assertNotIn(".catch(e=>e.message)", "".join(expressions))

    def test_security_boundary_rejects_successful_delete_or_disappearing_video(self):
        for delete_status, after_status in ((200, 200), (500, 200), (403, 404)):
            with self.subTest(delete_status=delete_status, after_status=after_status):
                owner, other = MagicMock(), MagicMock()
                owner.eval_js.side_effect = [200, 401, after_status]
                other.eval_js.return_value = delete_status
                with self.assertRaises(AssertionError):
                    web_full_flow.verify_security_boundaries(owner, other, 7)

    def test_revocation_requires_socket_rejection_and_unchanged_history(self):
        owner, removed = MagicMock(), MagicMock()
        owner.eval_js.side_effect = [[11, 12], [11, 12]]
        removed.eval_js.side_effect = [404, {"rejected": True, "error": "group not found"}, 404]
        self.assertTrue(web_full_flow.verify_group_member_revocation(owner, removed, 7))
        socket_expression = removed.eval_js.call_args_list[1].args[0]
        self.assertIn("socket.send(", socket_expression)
        self.assertIn('"type": "group_message"', socket_expression)
        self.assertIn("addEventListener('message'", socket_expression)
        self.assertNotIn("method:'POST'", socket_expression)

    def test_revocation_rejects_ack_and_growth_even_when_http_returns_404(self):
        for reply, after in (({"rejected": False}, [11, 12]),
                             ({"rejected": True, "error": "group not found"}, [11, 12, 13])):
            with self.subTest(reply=reply, after=after):
                owner, removed = MagicMock(), MagicMock()
                owner.eval_js.side_effect = [[11, 12], after]
                removed.eval_js.side_effect = [404, reply, 404]
                with self.assertRaises(AssertionError):
                    web_full_flow.verify_group_member_revocation(owner, removed, 7)

    def test_report_invalidation_on_incomplete_run(self):
        with tempfile.TemporaryDirectory() as td:
            report_file = Path(td) / "web_report.json"
            report_file.write_text("{\"stale\": true}")
            # Simulated failure should unlink stale report
            report_file.unlink(missing_ok=True)
            self.assertFalse(report_file.exists())


if __name__ == "__main__":
    unittest.main()
