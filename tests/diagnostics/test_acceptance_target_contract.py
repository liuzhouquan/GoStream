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


class TargetCaptured(Exception):
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


if __name__ == "__main__":
    unittest.main()
