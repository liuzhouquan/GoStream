#!/usr/bin/env python3
"""Exercise real recovery stages and owned Compose cleanup after injected failures.

Requires an image built from the current production Go sources, Docker Compose,
Go from go.mod, and host FFmpeg. Uses only independently owned disposable stacks.
Unlike diagnostic mocks of run_all, each case runs fixture creation and recovery
through the actual stage before the corresponding subprocess is made to fail.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tests/integration'))
import migration_backup_recovery_smoke as recovery

STAGES = ('snapshot', 'sql_import', 'object_restore', 'api_start', 'migration_replay', 'sigterm')


def run_case(stage, go_bin, report_dir):
    original_run = subprocess.run
    original_popen = subprocess.Popen
    original_init = recovery.AcceptanceStack.__init__
    stacks = []
    reached = False
    host_api_starts = 0
    owned_apis = []
    started = time.monotonic()

    def stack_init(stack, *args, **kwargs):
        kwargs['build'] = False  # Production Go is unchanged by these acceptance fixes.
        original_init(stack, *args, **kwargs)
        stacks.append(stack)

    def fail():
        nonlocal reached
        reached = True
        if stage == 'sigterm':
            os.kill(os.getpid(), signal.SIGTERM)
            raise AssertionError('SIGTERM was not delivered through launcher lifecycle')
        raise RuntimeError('injected recovery stage failure: ' + stage)

    def stage_run(command, *args, **kwargs):
        parts = [str(p) for p in command]
        text = ' '.join(parts)
        if ('mysqldump' in text and stage in ('snapshot', 'sigterm')):
            fail()
        if (stage == 'sql_import' and kwargs.get('stdin') is not None
                and parts[-1].startswith('gostream_restore_')):
            fail()
        if (stage == 'migration_replay' and kwargs.get('stdin') is not None
                and parts[-1].startswith('gostream_replay_')):
            fail()
        if (stage == 'object_restore' and len(parts) > 1
                and Path(parts[0]).name == 'gostream_media' and parts[1] == 'restore'):
            fail()
        return original_run(command, *args, **kwargs)

    def stage_popen(command, *args, **kwargs):
        nonlocal host_api_starts
        if Path(str(command[0])).name == 'gostream_server':
            host_api_starts += 1
            if stage == 'api_start' and host_api_starts == 1:
                fail()
        child = original_popen(command, *args, **kwargs)
        if Path(str(command[0])).name == 'gostream_server':
            owned_apis.append(child)
        return child

    artifact = report_dir / (stage + '.json')
    artifact.write_text('{"status":"PASS","stale":true}')
    argv = ['recovery', '--mode', 'compose', '--compose-e2e', '--go-bin', go_bin,
            '--report', str(artifact)]
    caught = None
    with patch.object(sys, 'argv', argv), \
         patch.object(recovery.AcceptanceStack, '__init__', stack_init), \
         patch.object(subprocess, 'run', stage_run), \
         patch.object(subprocess, 'Popen', stage_popen):
        try:
            recovery.main()
        except (RuntimeError, recovery.LauncherInterrupted) as error:
            caught = error
    if not reached or caught is None:
        raise AssertionError(f'{stage}: actual fault boundary was not reached')
    if artifact.exists():
        raise AssertionError(f'{stage}: failed drill retained a success report')
    if len(stacks) != 1:
        raise AssertionError(f'{stage}: expected exactly one owned stack')
    stack = stacks[0]
    remaining = {'containers': len(stack.check_remaining_containers()),
                 'volumes': len(stack.check_remaining_volumes()),
                 'networks': len(stack.check_remaining_networks())}
    remaining['host_api_processes'] = sum(child.poll() is None for child in owned_apis)
    if any(remaining.values()):
        raise AssertionError(f'{stage}: owned resources survived: {remaining}')
    return {'stage': stage, 'status': 'PASS', 'actual_stage_reached': reached,
            'observed_exception': type(caught).__name__, 'host_api_starts': host_api_starts,
            'stale_report_removed': True, 'remaining': remaining,
            'elapsed_seconds': round(time.monotonic() - started, 3)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go-bin', default='/usr/local/go/bin/go')
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('--stages', nargs='+', choices=STAGES, default=list(STAGES))
    args = parser.parse_args()
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.unlink(missing_ok=True)
    results = []
    with tempfile.TemporaryDirectory(prefix='gostream-recovery-fault-reports-') as directory:
        for stage in args.stages:
            results.append(run_case(stage, args.go_bin, Path(directory)))
            print(json.dumps(results[-1]), flush=True)
    report = {'status': 'PASS', 'scope': 'real_owned_compose_recovery_failure_windows',
              'cases': results, 'git': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
              'dirty': bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT, text=True).strip())}
    args.report.write_text(json.dumps(report, indent=2) + '\n')


if __name__ == '__main__':
    main()
