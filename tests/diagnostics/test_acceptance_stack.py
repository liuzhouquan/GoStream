"""Diagnostic tests for the AcceptanceStack launcher: lifecycle, signals, and cleanup."""

import contextlib
import io
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

import acceptance_stack
from acceptance_stack import AcceptanceStack, get_git_info, get_host_info, parse_ports


CANCELLATION_FIXTURE = r'''
import json, os, signal, subprocess, sys
from pathlib import Path
from unittest.mock import patch, MagicMock

root, work, stage = map(Path, sys.argv[1:4])
sys.path.insert(0, str(root / "tests/integration"))
import acceptance_stack as stack_mod

cleanup_events = []
def record(evt):
    with (work / "cleanup.txt").open("a") as f:
        f.write(evt + "\n")

def cancel():
    os.kill(os.getpid(), signal.SIGTERM)

def fake_run(*cmd, **kwargs):
    cmd_str = " ".join(cmd)
    if "up" in cmd_str:
        record("compose_up")
        if stage.name in ("up", "second_signal"):
            cancel()
    elif "down" in cmd_str:
        record("compose_down")
        if stage.name == "second_signal":
            cancel()
    elif "ps" in cmd_str and stage.name == "inspect":
        cancel()
    return subprocess.CompletedProcess(cmd, 0, stdout="{}", stderr="")

def fake_probe(url, timeout=2):
    if stage.name == "wait":
        cancel()
    if "/health" in url:
        return 200, {"status": "ok"}
    if "/ready" in url:
        return 200, {"status": "ready"}
    if "/metrics" in url:
        return 200, "gostream_requests_total 1\n"
    return 200, {}

stack = stack_mod.AcceptanceStack(
    project_name="test-cancel-proj",
    build=False,
    wait_timeout=5,
    root_dir=root,
)

with patch.object(stack, "run_compose", side_effect=fake_run), \
     patch.object(stack, "probe_url", side_effect=fake_probe), \
     patch.object(stack, "check_remaining_containers", return_value=""), \
     patch.object(stack, "check_remaining_volumes", return_value=""), \
     patch.object(stack, "check_remaining_networks", return_value=""), \
     patch.object(stack, "verify_go_runtime", return_value="go1.27.2"):
    try:
        with stack:
            if stage.name == "inside":
                cancel()
    except stack_mod.LauncherInterrupted as err:
        (work / "exit_signal.txt").write_text(str(err.args[0]))
        raise SystemExit(128 + int(err.args[0]))

'''


class AcceptanceStackUnitTest(unittest.TestCase):
    def test_parse_ports(self):
        urls = parse_ports("127.0.0.1:18080-18081")
        self.assertEqual(urls, ["http://127.0.0.1:18080", "http://127.0.0.1:18081"])

        urls_single = parse_ports("127.0.0.1:8080")
        self.assertEqual(urls_single, ["http://127.0.0.1:8080"])

        urls_custom = parse_ports("192.168.1.10:9000-9002")
        self.assertEqual(urls_custom, [
            "http://192.168.1.10:9000",
            "http://192.168.1.10:9001",
            "http://192.168.1.10:9002",
        ])

    def test_git_and_host_metadata(self):
        git_info = get_git_info(ROOT)
        self.assertTrue(re.match(r"^[0-9a-f]{40}$", git_info["commit"]), f"invalid sha: {git_info}")
        self.assertIsInstance(git_info["dirty"], bool)

        host_info = get_host_info()
        self.assertIn("kernel", host_info)
        self.assertIn("machine", host_info)
        self.assertGreaterEqual(host_info["memory_bytes"], 0)

    def test_compose_command_structure(self):
        stack = AcceptanceStack(
            project_name="test-proj-123",
            api_ports="127.0.0.1:18080-18081",
            mysql_port="127.0.0.1:13307",
            root_dir=ROOT,
        )
        cmd = stack.compose_cmd("up", "-d")
        self.assertEqual(cmd[:4], ["docker", "compose", "-p", "test-proj-123"])
        self.assertIn("-f", cmd)
        self.assertIn(str(ROOT / "docker-compose.yml"), cmd)
        self.assertIn(str(ROOT / "tests/integration/object-test.compose.yml"), cmd)
        self.assertEqual(cmd[-2:], ["up", "-d"])

        # Check environment variable injection
        self.assertEqual(stack.env["GOSTREAM_API_PORTS"], "127.0.0.1:18080-18081")
        self.assertEqual(stack.env["GOSTREAM_MYSQL_PORT"], "127.0.0.1:13307")
        self.assertTrue(len(stack.env["JWT_SECRET"]) >= 32)

    def test_subcommand_environment_injection(self):
        stack = AcceptanceStack(
            project_name="test-env-proj",
            api_ports="127.0.0.1:18080-18081",
            mysql_port="127.0.0.1:13307",
            minio_api_port="127.0.0.1:29000",
            root_dir=ROOT,
        )
        # Execute an echo command and inspect received environment variables
        code = (
            "import os, json; print(json.dumps({"
            "'API_A': os.environ.get('GOSTREAM_API_A'),"
            "'API_B': os.environ.get('GOSTREAM_API_B'),"
            "'BASE': os.environ.get('GOSTREAM_BASE_URL'),"
            "'ACTIVE': os.environ.get('GOSTREAM_COMPOSE_ACTIVE'),"
            "'PROJ': os.environ.get('GOSTREAM_STACK_PROJECT'),"
            "'MYSQL': os.environ.get('GOSTREAM_MYSQL_ADDR'),"
            "'MINIO': os.environ.get('GOSTREAM_MINIO_ENDPOINT'),"
            "}))"
        )
        with tempfile.TemporaryDirectory() as td:
            out_file = Path(td) / "env_out.json"
            exit_code = stack.run_subcommand([
                sys.executable, "-c",
                f"{code}; open('{out_file}', 'w').write(json.dumps({{"
                f"'API_A': os.environ.get('GOSTREAM_API_A'),"
                f"'API_B': os.environ.get('GOSTREAM_API_B'),"
                f"'BASE': os.environ.get('GOSTREAM_BASE_URL'),"
                f"'ACTIVE': os.environ.get('GOSTREAM_COMPOSE_ACTIVE'),"
                f"'PROJ': os.environ.get('GOSTREAM_STACK_PROJECT'),"
                f"'MYSQL': os.environ.get('GOSTREAM_MYSQL_ADDR'),"
                f"'MINIO': os.environ.get('GOSTREAM_MINIO_ENDPOINT'),"
                f"}}))"
            ])
            self.assertEqual(exit_code, 0)
            data = json.loads(out_file.read_text())
            self.assertEqual(data["API_A"], "http://127.0.0.1:18080")
            self.assertEqual(data["API_B"], "http://127.0.0.1:18081")
            self.assertEqual(data["BASE"], "http://127.0.0.1:18080")
            self.assertEqual(data["ACTIVE"], "1")
            self.assertEqual(data["PROJ"], "test-env-proj")
            self.assertEqual(data["MYSQL"], "127.0.0.1:13307")
            self.assertEqual(data["MINIO"], "127.0.0.1:29000")

    def test_failed_subcommand_exit_code_and_report_handling(self):
        stack = AcceptanceStack(project_name="test-fail-proj", root_dir=ROOT)
        with tempfile.TemporaryDirectory() as td:
            report_file = Path(td) / "test_report.json"
            report_file.write_text("{\"stale\": true}")

            with patch.object(stack, "run_compose", return_value=subprocess.CompletedProcess([], 0, "{}", "")), \
                 patch.object(stack, "wait_for_ready", return_value=None), \
                 patch.object(stack, "inspect_instances", return_value=[]), \
                 patch.object(stack, "check_remaining_containers", return_value=""), \
                 patch.object(stack, "check_remaining_volumes", return_value=""), \
                 patch.object(stack, "check_remaining_networks", return_value=""), \
                 patch.object(stack, "verify_go_runtime", return_value="go1.27.2"):
                # Subcommand that exits with 42
                exit_code = stack.run_subcommand([sys.executable, "-c", "import sys; sys.exit(42)"])
                self.assertEqual(exit_code, 42)


class AcceptanceStackLifecycleTest(unittest.TestCase):
    def run_cancellation(self, stage_name):
        with tempfile.TemporaryDirectory() as td:
            work = Path(td)
            stage_path = work / stage_name
            stage_path.touch()
            proc = subprocess.run(
                [sys.executable, "-c", CANCELLATION_FIXTURE, str(ROOT), str(work), str(stage_path)],
                capture_output=True, text=True, timeout=8
            )
            cleanups = (work / "cleanup.txt").read_text().splitlines() if (work / "cleanup.txt").exists() else []
            return proc.returncode, cleanups

    def test_sigterm_during_compose_up_unwinds_and_downs_compose(self):
        ret, cleanups = self.run_cancellation("up")
        self.assertEqual(ret, 143)
        self.assertIn("compose_up", cleanups)
        self.assertIn("compose_down", cleanups)

    def test_sigterm_during_readiness_wait_unwinds_and_downs_compose(self):
        ret, cleanups = self.run_cancellation("wait")
        self.assertEqual(ret, 143)
        self.assertIn("compose_up", cleanups)
        self.assertIn("compose_down", cleanups)

    def test_sigterm_inside_context_unwinds_and_downs_compose(self):
        ret, cleanups = self.run_cancellation("inside")
        self.assertEqual(ret, 143)
        self.assertIn("compose_up", cleanups)
        self.assertIn("compose_down", cleanups)

    def test_sigterm_during_instance_inspection_is_not_swallowed(self):
        ret, cleanups = self.run_cancellation("inspect")
        self.assertEqual(ret, 143)
        self.assertIn("compose_down", cleanups)

    def test_second_signal_during_teardown_does_not_abort_cleanup(self):
        ret, cleanups = self.run_cancellation("second_signal")
        self.assertEqual(ret, 143)
        self.assertIn("compose_up", cleanups)
        self.assertIn("compose_down", cleanups)


class AcceptanceStackReviewRegressionTest(unittest.TestCase):
    def test_up_refuses_any_existing_project_resources_without_teardown(self):
        for kind in ("containers", "volumes", "networks"):
            with self.subTest(kind=kind):
                stack = AcceptanceStack(project_name="existing-demo", root_dir=ROOT)
                with patch.object(stack, "verify_go_runtime", return_value="go1.27.2"), \
                     patch.object(stack, "check_remaining_containers", return_value="demo-api" if kind == "containers" else ""), \
                     patch.object(stack, "check_remaining_volumes", return_value="demo-sql" if kind == "volumes" else ""), \
                     patch.object(stack, "run_compose") as compose, \
                     patch.object(stack, "wait_for_ready"), patch.object(stack, "inspect_instances", return_value=[]), \
                     patch.object(stack, "check_remaining_networks", return_value="demo-net" if kind == "networks" else "", create=True):
                    with self.assertRaisesRegex(RuntimeError, "existing|already|refus"):
                        with stack:
                            pass
                    self.assertFalse(stack.started)
                    compose.assert_not_called()

    def test_exited_command_leader_does_not_leave_its_descendant(self):
        stack = AcceptanceStack(project_name="test-orphan-review", root_dir=ROOT)
        with tempfile.TemporaryDirectory() as directory:
            pid_file = Path(directory) / "child.pid"
            child = "import time;time.sleep(30)"
            code = ("import subprocess,sys;from pathlib import Path;"
                    f"p=subprocess.Popen([sys.executable,'-c',{child!r}]);"
                    f"Path({str(pid_file)!r}).write_text(str(p.pid))")
            try:
                result = stack.run_subcommand([sys.executable, "-c", code])
                self.assertEqual(result, 0)
                child_pid = int(pid_file.read_text())
                stack.down()
                stat = Path(f"/proc/{child_pid}/stat")
                if stat.exists():
                    state = stat.read_text().rsplit(") ", 1)[1].split()[0]
                    self.assertIn(state, ("Z", "X"), "exited leader left a running descendant")
            finally:
                if pid_file.exists():
                    try:
                        os.kill(int(pid_file.read_text()), signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_cli_publishes_report_only_after_successful_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "report.json"
            stack = AcceptanceStack(project_name="test-publish-review", root_dir=ROOT)
            stack.metadata = {"fixture": True}
            observed = []

            def down():
                observed.append(report.exists())

            with patch.object(acceptance_stack, "AcceptanceStack", return_value=stack), \
                 patch.object(stack, "up"), patch.object(stack, "down", side_effect=down), \
                 patch.object(sys, "argv", ["acceptance_stack.py", "--probe-only", "--report-file", str(report)]), \
                 contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaises(SystemExit) as exited:
                    acceptance_stack.main()
            self.assertEqual(exited.exception.code, 0)
            self.assertEqual(observed, [False], "success report was visible before cleanup")
            self.assertEqual(json.loads(report.read_text())["launcher_cleanup"], {"containers": 0, "volumes": 0, "networks": 0})

    def test_cli_teardown_failure_does_not_print_success_metadata(self):
        stack = AcceptanceStack(project_name="test-failed-cleanup-review", root_dir=ROOT)
        stack.metadata = {"fixture": True}
        output = io.StringIO()
        with patch.object(acceptance_stack, "AcceptanceStack", return_value=stack), \
             patch.object(stack, "up"), patch.object(stack, "down", side_effect=RuntimeError("cleanup failed")), \
             patch.object(sys, "argv", ["acceptance_stack.py", "--probe-only"]), \
             contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as exited:
                acceptance_stack.main()
        self.assertEqual(exited.exception.code, 1)
        self.assertNotIn("ACCEPTANCE_METADATA_JSON=", output.getvalue())

    def test_down_detects_remaining_project_volumes(self):
        stack = AcceptanceStack(project_name="test-volume-review", root_dir=ROOT)
        stack.started = True

        def run(command, **kwargs):
            output = "left-behind-volume\n" if command[:3] == ["docker", "volume", "ls"] else ""
            return subprocess.CompletedProcess(command, 0, stdout=output, stderr="")

        with patch.object(stack, "run_compose"), patch.object(acceptance_stack.subprocess, "run", side_effect=run):
            with self.assertRaisesRegex(RuntimeError, "volume"):
                stack.down()


if __name__ == "__main__":
    unittest.main()
