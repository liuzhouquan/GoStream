"""Recovery drills must own their target and reap local child processes."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tests/integration'))
import migration_backup_recovery_smoke as recovery


class RecoveryIsolationTest(unittest.TestCase):
    def test_missing_project_never_falls_back_to_other_mysql(self):
        with patch.object(recovery.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, '', '')) as run:
            with self.assertRaisesRegex(RuntimeError, 'exactly one'):
                recovery.find_compose_mysql_container('owned-project')
            self.assertEqual(run.call_count, 1)

    def test_ambiguous_project_is_rejected(self):
        with patch.object(recovery.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, 'first\nsecond\n', '')):
            with self.assertRaisesRegex(RuntimeError, 'exactly one'):
                recovery.find_compose_mysql_container('owned-project')

    def test_compose_runner_refuses_unowned_target_before_writes(self):
        runner = recovery.MigrationBackupRecoveryRunner('compose', 'http://127.0.0.1:18080')
        with patch.object(recovery, 'json_call') as api, tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(RuntimeError, 'owned'):
                runner.run_all('server', 'go', {}, directory)
            api.assert_not_called()

    def test_main_rejects_external_targets_before_starting_or_writing(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / 'report.json'
            report.write_text('previous evidence')
            argv = ['recovery', '--mode', 'compose', '--compose-e2e', '--report', str(report)]
            with patch.object(sys, 'argv', argv), patch.dict(os.environ, {'GOSTREAM_API_A': 'http://external.test'}, clear=True), patch.object(recovery, 'json_call') as api, patch.object(recovery.subprocess, 'run') as run:
                with self.assertRaisesRegex(ValueError, 'external'):
                    recovery.main()
                api.assert_not_called()
                run.assert_not_called()
                self.assertFalse(report.exists())

    def test_runner_rejects_mismatched_owned_endpoints_before_writes(self):
        stack = recovery.AcceptanceStack(project_name="owned-test")
        stack.started = True
        stack._interrupt_context = object()
        runner = recovery.MigrationBackupRecoveryRunner(
            'compose', 'http://wrong.test', project_name=stack.project_name,
            mysql_addr=stack.mysql_addr, minio_endpoint=stack.minio_endpoint,
            owned_stack=stack,
        )
        with patch.object(recovery, 'json_call') as api, patch.object(recovery.subprocess, 'run') as run:
            with self.assertRaisesRegex(RuntimeError, 'owned'):
                runner.run_all('server', 'go', {}, '/unused')
            api.assert_not_called()
            run.assert_not_called()

    def test_owned_stack_cleanup_runs_on_drill_failure_and_cancellation(self):
        for failure in (RuntimeError("restore failed"), recovery.LauncherInterrupted(15)):
            with self.subTest(failure=type(failure).__name__), tempfile.TemporaryDirectory() as directory:
                report = Path(directory) / 'report.json'
                argv = ['recovery', '--mode', 'compose', '--compose-e2e', '--report', str(report)]
                with patch.object(sys, 'argv', argv), patch.dict(os.environ, {}, clear=True), \
                     patch.object(recovery.AcceptanceStack, 'up'), \
                     patch.object(recovery.AcceptanceStack, 'down') as cleanup, \
                     patch.object(recovery, 'find_compose_mysql_container', return_value='owned-mysql'), \
                     patch.object(recovery.subprocess, 'run'), \
                     patch.object(recovery.MigrationBackupRecoveryRunner, 'run_all', side_effect=failure):
                    with self.assertRaises(type(failure)):
                        recovery.main()
                    cleanup.assert_called_once_with()
                    self.assertFalse(report.exists())

    def test_shutdown_kills_and_waits_after_timeout(self):
        child = Mock()
        child.wait.side_effect = [subprocess.TimeoutExpired('server', 3), 0]
        recovery.stop_process(child)
        child.terminate.assert_called_once_with()
        child.kill.assert_called_once_with()
        self.assertEqual(child.wait.call_count, 2)


if __name__ == '__main__':
    unittest.main()
