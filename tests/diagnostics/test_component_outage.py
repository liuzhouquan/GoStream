"""Diagnostic unit tests for Component Outage & Persistence Runner (G09 / A01-A10)."""

import json
import os
import shutil
from pathlib import Path
import re
import subprocess
import sys
import unittest
from unittest.mock import patch, MagicMock

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

import component_outage_smoke


def declared_go_binary():
    """Use the configured toolchain on both developer machines and setup-go runners."""
    binary = os.environ.get("GOSTREAM_GO_BIN") or shutil.which("go") or "/usr/local/go/bin/go"
    required = "go" + re.search(r"^go (\S+)", (ROOT / "go.mod").read_text(), re.MULTILINE).group(1)
    # Do not silently select or download an older/newer toolchain for this check.
    env = dict(os.environ, GOTOOLCHAIN="local")
    actual = subprocess.check_output([binary, "env", "GOVERSION"], env=env, text=True, timeout=10).strip()
    if actual != required:
        raise RuntimeError(f"Go version mismatch: required {required}, found {actual} at {binary}")
    return binary


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
            if report.get("mode") == "sqlite" and cp == "rabbitmq_outage_outbox_retention_and_delivery":
                continue
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
        # Test standard password and complex password with :// special characters
        for secret in ["my_secret_token", "sentinel_secret://tail", "pw@with!chars"]:
            cmd = [declared_go_binary(), "run", "./tools/seed_feed_benchmark.go", "-driver", "mysql", "-dsn", f"fake_user:{secret}@tcp(127.0.0.1:99999)/gostream"]
            res = subprocess.run(cmd, cwd=str(ROOT), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            self.assertNotEqual(res.returncode, 0)
            output = res.stdout + res.stderr
            self.assertNotIn(secret, output)
            self.assertIn("fake_user:***@tcp(127.0.0.1:99999)/gostream", output)

    def test_seed_group_im_benchmark_dsn_redaction(self):
        for secret in ["my_secret_token", "sentinel_secret://tail", "pw@with!chars"]:
            cmd = [declared_go_binary(), "run", "./tools/seed_group_im_benchmark.go", "-driver", "mysql", "-dsn", f"fake_user:{secret}@tcp(127.0.0.1:99999)/gostream"]
            res = subprocess.run(cmd, cwd=str(ROOT), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            self.assertNotEqual(res.returncode, 0)
            output = res.stdout + res.stderr
            self.assertNotIn(secret, output)
            self.assertIn("fake_user:***@tcp(127.0.0.1:99999)/gostream", output)

    def test_outage_guarantees_cleanup_on_injected_failure(self):
        # Behavioral test: verify that if an assertion or error occurs inside an outage window,
        # runner.managed_stopped_services tracks the stopped service and start is executed in finally.
        class MockStack:
            def __init__(self):
                self.actions = []

            def run_compose(self, action, service):
                self.actions.append((action, service))
                return ""

        stack = MockStack()
        runner = component_outage_smoke.ComponentOutageRunner(
            mode="compose", api_a="http://fake-a", api_b="http://fake-b", stack=stack
        )

        def fake_call_api(url, path, method="GET", **kwargs):
            if path in ("/health", "/ready") and ("stop", "redis") not in stack.actions:
                return 200, {"status": "ok" if path == "/health" else "ready"}, 1.0
            if ("stop", "redis") in stack.actions:
                raise RuntimeError("simulated failure inside redis outage window")
            return 200, {}, 1.0

        def fake_json_call(url, path, method="GET", **kwargs):
            if path == "/api/users":
                return {"access_token": "fake-jwt", "user": {"id": 1}}
            return {}

        with patch.object(component_outage_smoke, "call_api", side_effect=fake_call_api), \
             patch.object(component_outage_smoke, "json_call", side_effect=fake_json_call), \
             patch.object(component_outage_smoke, "SimpleWebSocket") as mock_ws:
            ws_inst = MagicMock()
            ws_inst.next_of_type.return_value = {"type": "ready"}
            mock_ws.return_value = ws_inst

            with self.assertRaises(RuntimeError):
                runner.run_all()

        # Verify that start redis was executed despite the error in outage, and tracking is clean
        self.assertIn(("stop", "redis"), stack.actions)
        self.assertIn(("start", "redis"), stack.actions)
        self.assertEqual(len(runner.managed_stopped_services), 0)

    def test_outage_cleanup_when_stop_raises_exception(self):
        # T-S1: If stop fails/times out after taking partial effect, service must have been registered
        # beforehand so finally still attempts recovery.
        class MockStack:
            def __init__(self):
                self.actions = []

            def run_compose(self, action, service):
                self.actions.append((action, service))
                if action == "stop" and service == "redis":
                    raise RuntimeError("simulated failure during stop command")
                return ""

        stack = MockStack()
        runner = component_outage_smoke.ComponentOutageRunner(
            mode="compose", api_a="http://fake-a", api_b="http://fake-b", stack=stack
        )

        def fake_call_api(url, path, method="GET", **kwargs):
            return 200, {"status": "ok" if path == "/health" else "ready"}, 1.0

        def fake_json_call(url, path, method="GET", **kwargs):
            if path == "/api/users":
                return {"access_token": "fake-jwt", "user": {"id": 1}}
            return {}

        with patch.object(component_outage_smoke, "call_api", side_effect=fake_call_api), \
             patch.object(component_outage_smoke, "json_call", side_effect=fake_json_call), \
             patch.object(component_outage_smoke, "SimpleWebSocket") as mock_ws:
            ws_inst = MagicMock()
            ws_inst.next_of_type.return_value = {"type": "ready"}
            mock_ws.return_value = ws_inst

            with self.assertRaises(RuntimeError) as ctx:
                runner.run_all()
            self.assertIn("simulated failure during stop command", str(ctx.exception))

        # Verify stop was attempted, and start was also attempted in finally!
        self.assertIn(("stop", "redis"), stack.actions)
        self.assertIn(("start", "redis"), stack.actions)
        # Tracking is clean because start succeeded
        self.assertEqual(len(runner.managed_stopped_services), 0)

    def test_outage_cleanup_retried_by_outer_finally_when_inner_start_fails(self):
        # T-S1: If start fails in inner finally, service must NOT be discarded from
        # managed_stopped_services so outer fallback in main() can retry.
        class MockStack:
            def __init__(self):
                self.actions = []
                self.start_attempts = 0

            def run_compose(self, action, service):
                self.actions.append((action, service))
                if action == "start" and service == "redis":
                    self.start_attempts += 1
                    if self.start_attempts == 1:
                        raise RuntimeError("simulated transient failure during first start")
                return ""

        stack = MockStack()
        runner = component_outage_smoke.ComponentOutageRunner(
            mode="compose", api_a="http://fake-a", api_b="http://fake-b", stack=stack
        )

        def fake_call_api(url, path, method="GET", **kwargs):
            if path in ("/health", "/ready") and ("stop", "redis") not in stack.actions:
                return 200, {"status": "ok" if path == "/health" else "ready"}, 1.0
            if ("stop", "redis") in stack.actions:
                raise RuntimeError("simulated error inside outage window")
            return 200, {}, 1.0

        def fake_json_call(url, path, method="GET", **kwargs):
            if path == "/api/users":
                return {"access_token": "fake-jwt", "user": {"id": 1}}
            return {}

        with patch.object(component_outage_smoke, "call_api", side_effect=fake_call_api), \
             patch.object(component_outage_smoke, "json_call", side_effect=fake_json_call), \
             patch.object(component_outage_smoke, "SimpleWebSocket") as mock_ws:
            ws_inst = MagicMock()
            ws_inst.next_of_type.return_value = {"type": "ready"}
            mock_ws.return_value = ws_inst

            with self.assertRaises(RuntimeError):
                runner.run_all()

        # At this point, inner start failed, so "redis" MUST still be in runner.managed_stopped_services
        self.assertIn("redis", runner.managed_stopped_services)
        self.assertEqual(stack.start_attempts, 1)

        # Simulate outer fallback in main():
        for svc in list(runner.managed_stopped_services):
            stack.run_compose("start", svc)
            runner.managed_stopped_services.discard(svc)

        # Now outer retry succeeded and tracking is clean
        self.assertEqual(stack.start_attempts, 2)
        self.assertEqual(len(runner.managed_stopped_services), 0)

    def test_initial_running_services_guard(self):
        # T-S1: If initial running services is tracked and a service is not in it,
        # runner must refuse to stop or alter it.
        class MockStack:
            def __init__(self):
                self.actions = []

            def get_running_services(self):
                return {"mysql", "minio"}  # redis is not running

            def run_compose(self, action, service):
                self.actions.append((action, service))
                return ""

        stack = MockStack()
        runner = component_outage_smoke.ComponentOutageRunner(
            mode="compose", api_a="http://fake-a", api_b="http://fake-b", stack=stack
        )

        def fake_call_api(url, path, method="GET", **kwargs):
            return 200, {"status": "ok" if path == "/health" else "ready"}, 1.0

        def fake_json_call(url, path, method="GET", **kwargs):
            if path == "/api/users":
                return {"access_token": "fake-jwt", "user": {"id": 1}}
            return {}

        with patch.object(component_outage_smoke, "call_api", side_effect=fake_call_api), \
             patch.object(component_outage_smoke, "json_call", side_effect=fake_json_call), \
             patch.object(component_outage_smoke, "SimpleWebSocket") as mock_ws:
            ws_inst = MagicMock()
            ws_inst.next_of_type.return_value = {"type": "ready"}
            mock_ws.return_value = ws_inst

            with self.assertRaises(RuntimeError) as ctx:
                runner.run_all()
            self.assertIn("not running initially", str(ctx.exception))

        # Stop redis should NOT have been executed
        self.assertNotIn(("stop", "redis"), stack.actions)
        self.assertEqual(len(runner.managed_stopped_services), 0)




if __name__ == "__main__":
    unittest.main()
