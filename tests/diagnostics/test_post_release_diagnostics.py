"""Non-happy-path evidence must survive cleanup without exposing credentials."""
import json
import os
import signal
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "integration"))
import migration_backup_recovery_smoke as recovery
import feed_warm_cache_continuous as feed
import deployment_provenance as provenance


class PostReleaseDiagnosticsTest(unittest.TestCase):
    def test_clock_jump_does_not_inflate_rejected_run(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "feed.json"
            with patch.object(sys, "argv", ["feed", "--mode", "sqlite", "--duration-seconds", "0", "--report", str(report)]), \
                 patch.object(feed.time, "time", side_effect=[100, 26000]), \
                 patch.object(feed.time, "monotonic", side_effect=[10, 12]):
                self.assertEqual(feed.main(), 1)
            data = json.loads(report.read_text())
            self.assertEqual(data["elapsed_seconds"], 2)
            self.assertEqual(data["elapsed_clock"], "monotonic")
            self.assertEqual(data["wall_elapsed_seconds"], 25900)

    def test_failed_child_log_survives_temp_cleanup_and_redacts_secrets(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "drill.json"
            failure = report.with_suffix(".failure.json")
            with self.assertRaisesRegex(RuntimeError, "publish failed"):
                with tempfile.TemporaryDirectory() as work, recovery.RecoveryDiagnostics(report, work):
                    env = {**os.environ, "JWT_SECRET": "secret-jwt-sentinel"}
                    child = recovery.start_logged_api(
                        [sys.executable, "-c", "print('database is locked'); print('secret-jwt-sentinel'); print('Bearer abc.secret.token')"],
                        env, work, "source")
                    child.wait(timeout=5)
                    raise RuntimeError("publish failed")
            data = failure.read_text()
            self.assertIn("database is locked", data)
            self.assertNotIn("secret-jwt-sentinel", data)
            self.assertNotIn("abc.secret.token", data)
            self.assertEqual(failure.stat().st_mode & 0o777, 0o600)

    def test_success_clears_stale_failure_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "drill.json"
            failure = report.with_suffix(".failure.json")
            failure.write_text("old failure")
            with recovery.RecoveryDiagnostics(report, directory):
                pass
            self.assertFalse(failure.exists())

    def test_deployment_digest_comes_from_each_measured_container(self):
        def output(command, **kwargs):
            if command[1] == "ps":
                return "container-a\ncontainer-b"
            if command[1] == "inspect":
                cid = command[-1]
                port = "18080" if cid == "container-a" else "18081"
                self.assertNotIn(".Config", command[3])
                return json.dumps({"id": cid, "image": "sha256:" + "a" * 64,
                                   "ports": {"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": port}]}})
            self.assertEqual(command[-2:], ["sha256sum", "/proc/1/exe"])
            return "b" * 64 + "  /proc/1/exe"
        with patch.object(provenance.subprocess, "check_output", side_effect=output):
            result = provenance.inspect_api_deployment("owned", ["http://127.0.0.1:18080", "http://127.0.0.1:18081"])
        self.assertTrue(result["verified"])
        self.assertEqual([i["container_id"] for i in result["instances"]], ["container-a", "container-b"])
        self.assertEqual(result["instances"][0]["binary_sha256"], "b" * 64)

    def test_missing_docker_or_unmatched_endpoint_is_not_verified(self):
        with patch.object(provenance.subprocess, "check_output", side_effect=FileNotFoundError("secret")):
            result = provenance.inspect_api_deployment("owned", ["http://127.0.0.1:18080"])
        self.assertFalse(result["verified"])
        self.assertNotIn("secret", json.dumps(result))
        self.assertFalse(provenance.inspect_api_deployment("", ["http://127.0.0.1:18080"])["verified"])
        with patch.object(provenance.subprocess, "check_output", side_effect=[
            "container-a", json.dumps({"id": "container-a", "image": "sha256:" + "a" * 64,
                                       "ports": {"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": "18081"}]}}),
        ]):
            self.assertFalse(provenance.inspect_api_deployment("owned", ["http://127.0.0.1:18080"])["verified"])

    def test_failure_log_omits_sql_row_payloads(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "drill.json"
            with self.assertRaises(RuntimeError), recovery.RecoveryDiagnostics(report, directory):
                (Path(directory) / "source.api.log").write_text("database is locked\n[rows:1] INSERT 'private-body'\n")
                raise RuntimeError("failed")
            text = report.with_suffix(".failure.json").read_text()
            self.assertIn("database is locked", text)
            self.assertNotIn("private-body", text)

    def test_signal_during_logged_start_keeps_child_owned(self):
        original = subprocess.Popen
        children = []
        def interrupt_after_spawn(*args, **kwargs):
            child = original(*args, **kwargs)
            children.append(child)
            os.kill(os.getpid(), signal.SIGTERM)
            return child
        owned = None
        with tempfile.TemporaryDirectory() as directory:
            try:
                with self.assertRaises(recovery.LauncherInterrupted), recovery.cleanup_on_interrupt(), \
                     patch.object(recovery.subprocess, "Popen", side_effect=interrupt_after_spawn):
                    with recovery.finish_process_start():
                        owned = recovery.start_logged_api([sys.executable, "-c", "import time; time.sleep(30)"], os.environ, directory, "source")
                self.assertIsNotNone(owned, "cancellation escaped before child ownership assignment")
            finally:
                for child in children:
                    recovery.stop_process(child)

    def test_unavailable_diagnostic_disk_preserves_original_error(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(RuntimeError, "original"), \
                 patch.object(recovery.os, "open", side_effect=OSError("disk full")), \
                 recovery.RecoveryDiagnostics(Path(directory) / "report.json", directory):
                raise RuntimeError("original")


if __name__ == "__main__":
    unittest.main()
