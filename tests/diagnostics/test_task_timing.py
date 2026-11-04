import importlib.util
import contextlib
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "tools" / "task_timing.py"
spec = importlib.util.spec_from_file_location("task_timing", SCRIPT)
timing = importlib.util.module_from_spec(spec)
spec.loader.exec_module(timing)


class CommandTimingTest(unittest.TestCase):
    def invoke(self, directory, command, timeout=3):
        report = Path(directory) / "result.json"
        completed = subprocess.run(
            [sys.executable, str(SCRIPT), "run", "--label", "test-command", "--report", str(report),
             "--heartbeat-seconds", "0.05", "--timeout-seconds", str(timeout),
             "--grace-seconds", "0.1", "--quiet", "--", *command],
            capture_output=True, text=True, timeout=8,
        )
        self.assertTrue(report.exists(), completed.stderr)
        return completed, json.loads(report.read_text())

    def test_heartbeat_reports_active_test_and_preserves_success(self):
        code = """import json,time
print(json.dumps({'Action':'run','Package':'example/p','Test':'TestWork'}),flush=True)
time.sleep(0.3)
print(json.dumps({'Action':'pass','Package':'example/p','Test':'TestWork','Elapsed':0.3}),flush=True)
print(json.dumps({'Action':'pass','Package':'example/p','Elapsed':0.3}),flush=True)
"""
        with tempfile.TemporaryDirectory() as directory:
            completed, report = self.invoke(directory, [sys.executable, "-c", code, "private-argument"])
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertFalse(report["timed_out"])
            self.assertGreaterEqual(report["elapsed_seconds"], 0.3)
            self.assertEqual(report["slowest_tests"][0]["test"], "TestWork")
            self.assertEqual(report["packages"][0]["action"], "pass")
            self.assertEqual(report["unfinished_tests"], [])
            events = [json.loads(line) for line in (Path(directory) / "result.events.jsonl").read_text().splitlines()]
            self.assertTrue(any("TestWork" in e.get("active_go_tests", []) for e in events))
            self.assertNotIn("private-argument", json.dumps(report))

    def test_failure_exit_code_and_stderr_are_retained(self):
        with tempfile.TemporaryDirectory() as directory:
            code = "import sys; print('{\"Action\":{},\"Package\":\"not-go\",\"Test\":[]}'); print('failed operation',file=sys.stderr);sys.exit(7)"
            completed, report = self.invoke(directory, [sys.executable, "-c", code])
            self.assertEqual(completed.returncode, 7)
            self.assertEqual(report["exit_code"], 7)
            self.assertIn("failed operation", (Path(directory) / "result.log").read_text())

    def test_timeout_kills_descendant_that_ignores_sigterm(self):
        with tempfile.TemporaryDirectory() as directory:
            pid_path = Path(directory) / "child.pid"
            code = """import signal,subprocess,sys,time
from pathlib import Path
child=subprocess.Popen([sys.executable,'-c','import signal,time;signal.signal(signal.SIGTERM,signal.SIG_IGN);time.sleep(30)'])
Path(sys.argv[1]).write_text(str(child.pid))
signal.signal(signal.SIGTERM,signal.SIG_IGN)
time.sleep(30)
"""
            try:
                completed, report = self.invoke(directory, [sys.executable, "-c", code, str(pid_path)], timeout=0.6)
                self.assertEqual(completed.returncode, 124, completed.stderr)
                self.assertTrue(report["timed_out"])
                self.assertTrue(pid_path.exists(), "child must have started before the deadline")
                child_stat = Path(f"/proc/{pid_path.read_text()}/stat")
                deadline = time.monotonic() + 1
                while child_stat.exists():
                    try:
                        state = child_stat.read_text().rsplit(") ", 1)[1].split()[0]
                    except FileNotFoundError:
                        break
                    if state == "Z":
                        break
                    if time.monotonic() >= deadline:
                        self.fail("timeout left the descendant running")
                    time.sleep(0.01)
            finally:
                if pid_path.exists():
                    try:
                        os.kill(int(pid_path.read_text()), signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_launch_error_is_reported_without_command_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            completed, report = self.invoke(directory, [str(Path(directory) / "missing-command"), "private-value"])
            self.assertEqual(completed.returncode, 127)
            self.assertEqual(report["error_kind"], "FileNotFoundError")
            self.assertNotIn("private-value", json.dumps(report))


class ActionsTimingTest(unittest.TestCase):
    def test_collector_paginates_without_requiring_new_gh_flags(self):
        payloads = [
            {"id": 123, "status": "queued"},
            {"total_count": 2, "jobs": [{"name": "first", "started_at": None}]},
            {"total_count": 2, "jobs": [{"name": "second", "started_at": None}]},
        ]
        responses = [subprocess.CompletedProcess([], 0, json.dumps(p).encode()) for p in payloads]
        with tempfile.TemporaryDirectory() as directory, patch.object(timing.subprocess, "run", side_effect=responses) as run:
            with contextlib.redirect_stdout(io.StringIO()):
                result = timing.ci_report(SimpleNamespace(repo="owner/repo", run_id=123, report=str(Path(directory) / "ci.json")))
            self.assertEqual(result, 0)
            self.assertEqual(run.call_count, 3)
            self.assertIn("page=2", run.call_args.args[0][-1])
            self.assertTrue(all("--slurp" not in call.args[0] for call in run.call_args_list))

    def test_parallel_job_span_is_not_the_sum_of_jobs(self):
        run = {"id": 123, "status": "completed", "conclusion": "success"}
        jobs = [
            {"name": "first", "started_at": "2026-11-04T10:00:00Z", "completed_at": "2026-11-04T10:00:10Z", "steps": [
                {"name": "slow step", "started_at": "2026-11-04T10:00:01Z", "completed_at": "2026-11-04T10:00:09Z"}]},
            {"name": "second", "started_at": "2026-11-04T10:00:02Z", "completed_at": "2026-11-04T10:00:12Z", "steps": []},
        ]
        report = timing.summarize_ci(run, jobs)
        self.assertEqual(report["job_span_seconds"], 12)
        self.assertEqual(sum(job["seconds"] for job in report["jobs"]), 20)
        self.assertEqual(report["slowest_steps"][0]["seconds"], 8)

    def test_queued_jobs_and_steps_do_not_invent_durations(self):
        report = timing.summarize_ci({"id": 1, "status": "queued"}, [{"name": "queued", "started_at": None}])
        self.assertIsNone(report["job_span_seconds"])
        self.assertEqual(report["slowest_steps"], [])


if __name__ == "__main__":
    unittest.main()
