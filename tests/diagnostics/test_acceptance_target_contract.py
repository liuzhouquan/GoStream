"""Exercise runner entrypoints with the actual final-stack environment contract."""

from contextlib import redirect_stdout
import importlib
import io
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

RUNNERS = {
    "feed_warm_cache_smoke": "run_continuous_feed_matrix",
    "search_query_smoke": "run_search_query_matrix",
    "group_im_smoke": "run_group_fanout_matrix",
}
STACK_ENV = {
    "GOSTREAM_API_A": "http://127.0.0.1:54321",
    "GOSTREAM_API_B": "http://127.0.0.1:54322",
    "GOSTREAM_BASE_URL": "http://127.0.0.1:54321",
    "GOSTREAM_COMPOSE_ACTIVE": "1",
}


class TargetCaptured(BaseException):
    pass


class AcceptanceTargetContractTest(unittest.TestCase):
    def test_launcher_environment_reaches_each_runner_without_local_build(self):
        for name, matrix_name in RUNNERS.items():
            with self.subTest(runner=name), tempfile.TemporaryDirectory() as directory:
                module = importlib.import_module(name)
                calls = []

                def capture(api_a, api_b, *args, **kwargs):
                    calls.append((api_a, api_b))
                    raise TargetCaptured()

                with patch.dict(os.environ, STACK_ENV, clear=True), \
                     patch.object(sys, "argv", [name, "--report", str(Path(directory) / "report.json")]), \
                     patch.object(module, matrix_name, side_effect=capture), \
                     patch.object(module.subprocess, "run", side_effect=AssertionError("must not build/start SQLite")), \
                     redirect_stdout(io.StringIO()):
                    with self.assertRaises(TargetCaptured):
                        module.main()
                self.assertEqual(calls, [(STACK_ENV["GOSTREAM_API_A"], STACK_ENV["GOSTREAM_API_B"])])

    def test_explicit_compose_requires_two_distinct_endpoints(self):
        for name, matrix_name in RUNNERS.items():
            for env in ({}, {"API_A": "http://127.0.0.1:54321"},
                        {"API_A": "http://127.0.0.1:54321", "API_B": "http://127.0.0.1:54321/"}):
                with self.subTest(runner=name, env=env), tempfile.TemporaryDirectory() as directory:
                    module = importlib.import_module(name)
                    report = Path(directory) / "report.json"
                    report.write_text('{"status":"PASS"}')
                    with patch.dict(os.environ, env, clear=True), \
                         patch.object(sys, "argv", [name, "--mode", "compose", "--report", str(report)]), \
                         patch.object(module, matrix_name, side_effect=AssertionError("must reject before traffic")), \
                         redirect_stdout(io.StringIO()):
                        with self.assertRaises(ValueError):
                            module.main()
                    self.assertFalse(report.exists(), "a rejected target must not leave an old PASS report")

    def test_legacy_environment_is_supported_and_new_names_take_precedence(self):
        from acceptance_config import resolve_acceptance_target
        legacy = {"API_A": "http://127.0.0.1:54321/", "API_B": "http://127.0.0.1:54322/"}
        self.assertEqual(resolve_acceptance_target("auto", legacy),
                         ("compose", "http://127.0.0.1:54321", "http://127.0.0.1:54322"))
        mixed = dict(legacy, **STACK_ENV)
        mixed["API_A"] = "http://127.0.0.1:9991"
        self.assertEqual(resolve_acceptance_target("auto", mixed)[1], STACK_ENV["GOSTREAM_API_A"])

    def test_unimplemented_compose_backup_never_starts_sqlite(self):
        module = importlib.import_module("migration_backup_recovery_smoke")
        for args, env in ((["--mode", "compose"], {}), ([], STACK_ENV),
                          (["--mode", "sqlite"], STACK_ENV)):
            with self.subTest(args=args), tempfile.TemporaryDirectory() as directory:
                report = Path(directory) / "report.json"
                report.write_text('{"status":"PASS"}')
                with patch.dict(os.environ, env, clear=True), \
                     patch.object(sys, "argv", [module.__name__, *args, "--report-file", str(report)]), \
                     patch.object(module.subprocess, "run", side_effect=AssertionError("must not build/start SQLite")), \
                     redirect_stdout(io.StringIO()):
                    with self.assertRaisesRegex(NotImplementedError, "MySQL/MinIO"):
                        module.main()
                self.assertFalse(report.exists())

    def test_is_loopback_host_classification(self):
        from acceptance_config import is_loopback_host
        self.assertTrue(is_loopback_host("127.0.0.1"))
        self.assertTrue(is_loopback_host("127.0.0.2"))
        self.assertTrue(is_loopback_host("localhost"))
        self.assertTrue(is_loopback_host("::1"))
        self.assertTrue(is_loopback_host("[::1]"))

        self.assertFalse(is_loopback_host(""))
        self.assertFalse(is_loopback_host("example.com"))
        self.assertFalse(is_loopback_host("192.168.1.100"))
        self.assertFalse(is_loopback_host("10.0.0.1"))

    def test_open_api_request_routes_loopback_to_direct_opener(self):
        from acceptance_config import open_api_request, _DIRECT_API_OPENER
        from unittest.mock import MagicMock

        with patch.object(_DIRECT_API_OPENER, "open", return_value=MagicMock()) as mock_direct, \
             patch("acceptance_config.urlopen", return_value=MagicMock()) as mock_urlopen:
            open_api_request("http://127.0.0.1:18080/ready", timeout=5)
            mock_direct.assert_called_once()
            mock_urlopen.assert_not_called()

        with patch.object(_DIRECT_API_OPENER, "open", return_value=MagicMock()) as mock_direct, \
             patch("acceptance_config.urlopen", return_value=MagicMock()) as mock_urlopen:
            open_api_request("http://example.com/api", timeout=5)
            mock_direct.assert_not_called()
            mock_urlopen.assert_called_once()

    def test_feed_continuous_execution_failure_emits_structured_report(self):
        import json
        module = importlib.import_module("feed_warm_cache_continuous")
        with tempfile.TemporaryDirectory() as directory:
            report_path = Path(directory) / "feed_fail.json"
            with patch.dict(os.environ, STACK_ENV, clear=True), \
                 patch.object(sys, "argv", ["feed_warm_cache_continuous.py", "--report", str(report_path)]), \
                 patch.object(module, "seed_mysql_feed_db"), \
                 patch.object(module, "run_continuous_feed_matrix", side_effect=TimeoutError("simulated connection timeout")), \
                 redirect_stdout(io.StringIO()):
                code = module.main()
            self.assertEqual(code, 1)
            self.assertTrue(report_path.exists())
            rep = json.loads(report_path.read_text())
            self.assertEqual(rep["status"], "FAIL")
            self.assertFalse(rep["release_gate_passed"])
            self.assertEqual(rep["validation_scope"], "execution_failure")
            self.assertEqual(rep["execution_error"]["type"], "TimeoutError")

    def test_feed_runtime_failure_also_emits_structured_report(self):
        import json
        module = importlib.import_module("feed_warm_cache_continuous")
        with tempfile.TemporaryDirectory() as directory:
            report_path = Path(directory) / "feed_fail.json"
            with patch.dict(os.environ, STACK_ENV, clear=True), \
                 patch.object(sys, "argv", ["feed_warm_cache_continuous.py", "--report", str(report_path)]), \
                 patch.object(module, "seed_mysql_feed_db"), \
                 patch.object(module, "run_continuous_feed_matrix", side_effect=RuntimeError("video processing failed")), \
                 redirect_stdout(io.StringIO()):
                code = module.main()
            self.assertEqual(code, 1)
            self.assertTrue(report_path.exists())
            rep = json.loads(report_path.read_text())
            self.assertEqual(rep["status"], "FAIL")
            self.assertFalse(rep["release_gate_passed"])
            self.assertEqual(rep["validation_scope"], "execution_failure")
            self.assertEqual(rep["execution_error"]["type"], "RuntimeError")

    def test_group_im_execution_failure_emits_structured_report(self):
        import json
        module = importlib.import_module("group_im_smoke")
        with tempfile.TemporaryDirectory() as directory:
            report_path = Path(directory) / "group_fail.json"
            with patch.dict(os.environ, STACK_ENV, clear=True), \
                 patch.object(sys, "argv", ["group_im_smoke.py", "--report", str(report_path)]), \
                 patch.object(module, "run_group_fanout_matrix", side_effect=ConnectionRefusedError("simulated connection refused")), \
                 redirect_stdout(io.StringIO()):
                code = module.main()
            self.assertEqual(code, 1)
            self.assertTrue(report_path.exists())
            rep = json.loads(report_path.read_text())
            self.assertEqual(rep["status"], "FAIL")
            self.assertFalse(rep["release_gate_passed"])
            self.assertEqual(rep["validation_scope"], "execution_failure")
            self.assertEqual(rep["execution_error"]["type"], "ConnectionRefusedError")

    def test_component_outage_execution_failure_emits_structured_report(self):
        import json
        module = importlib.import_module("component_outage_smoke")
        with tempfile.TemporaryDirectory() as directory:
            report_path = Path(directory) / "outage_fail.json"
            with patch.dict(os.environ, STACK_ENV, clear=True), \
                 patch.object(sys, "argv", ["component_outage_smoke.py", "--mode", "compose", "--report", str(report_path)]), \
                 patch.object(module.ComponentOutageRunner, "run_all", side_effect=AssertionError("simulated outage assert fail")), \
                 redirect_stdout(io.StringIO()):
                with self.assertRaises(AssertionError):
                    module.main()
            self.assertTrue(report_path.exists())
            rep = json.loads(report_path.read_text())
            self.assertEqual(rep["status"], "FAIL")
            self.assertFalse(rep["release_gate_passed"])
            self.assertEqual(rep["validation_scope"], "execution_failure")
            self.assertEqual(rep["execution_error"]["type"], "AssertionError")


if __name__ == "__main__":
    unittest.main()
