"""Cancellation regressions: real OS signals/children, simulated Docker/SQL."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


FIXTURE = r'''
import json,os,re,signal,subprocess,sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
root,work=map(Path,sys.argv[1:3])
stage=sys.argv[3]
sys.path.insert(0,str(root/'tests/integration'))
import rabbitmq_smoke as smoke
real_popen=subprocess.Popen
state={'schema':False,'user':False,'network':False,'containers':set()}
def record(event):
    with (work/'cleanup.txt').open('a') as stream: stream.write(event+'\n')
def cancel(): os.kill(os.getpid(),signal.SIGTERM)
def run(command,**kwargs):
    output=''
    sql=kwargs.get('input','')
    if sql.startswith('CREATE DATABASE'):
        state['schema']=True
        if stage=='schema': cancel()
    if sql.startswith('CREATE USER'):
        state['user']=True
        if stage=='user': cancel()
    if command[:3]==['docker','network','create']:
        state['network']=True
        if stage=='network': cancel()
    if command[:2]==['docker','run']:
        state['containers'].add(command[command.index('--name')+1])
        if stage in ('container','cluster-container','second-signal'): cancel()
    if command[:2]==['docker','ps']:
        output='\n'.join(state['containers'])
    if command[:3]==['docker','network','ls']:
        output='network-id' if state['network'] else ''
    if command[:2]==['docker','inspect']: output='false'
    if command[:3]==['docker','rm','-f']:
        state['containers'].discard(command[-1]);record('remove_container')
    if command[:3]==['docker','network','rm']:
        state['network']=False;record('remove_network')
    if sql.startswith('DROP DATABASE'):
        if stage=='second-signal': cancel()
        state['schema']=False;record('drop_schema')
    if sql.startswith('DROP USER'): state['user']=False;record('drop_user')
    if 'information_schema.schemata' in sql: output=str(int(state['schema']))
    if 'mysql.user' in sql: output=str(int(state['user']))
    if 'eval' in command: output='{ok,1000}'
    return subprocess.CompletedProcess(command,0,stdout=output+'\n',stderr='')
def popen(command,**kwargs):
    child=real_popen([sys.executable,'-c',
                     'import sys,time;line=sys.stdin.readline();time.sleep(60) if line.strip()!="ROLLBACK;" else None'
                     if stage=='sql-lock' else 'import time;time.sleep(60)'],
                     stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,
                     text=True,start_new_session=True)
    (work/'child.pid').write_text(str(child.pid))
    cancel()
    return child
class Ready:
    status=200
    def __enter__(self): return self
    def __exit__(self,*args): pass
def urlopen(*args,**kwargs):
    if stage=='wait': cancel()
    return Ready()
version=re.search(r'^go (\S+)$',(root/'go.mod').read_text(),re.M).group(1)
sys.argv=['rabbitmq_smoke.py','--mysql-container','fixture-mysql','--go-bin','fixture-go']
if stage in ('network','cluster-container'): sys.argv.append('--rabbitmq-cluster')
with patch.object(smoke.subprocess,'run',side_effect=run), \
     patch.object(smoke.subprocess,'check_output',return_value='go'+version+'\n'), \
     patch.object(smoke.subprocess,'Popen',side_effect=popen), \
     patch.object(smoke.urllib.request,'urlopen',side_effect=urlopen):
    try:
        if stage=='sql-lock':
            def interrupt(number,frame): raise smoke.LauncherInterrupted(number)
            signal.signal(signal.SIGTERM,interrupt)
            smoke.start_outbox_row_lock('fixture-mysql','fixture_schema',12)
        else: smoke.main()
    except smoke.LauncherInterrupted:
        (work/'state.json').write_text(json.dumps(dict(state,containers=list(state['containers']))))
        raise SystemExit(143)
'''


class RabbitMQCleanupTest(unittest.TestCase):
    def invoke(self, directory, stage):
        return subprocess.run([sys.executable, "-c", FIXTURE, str(ROOT), directory, stage],
                              capture_output=True, text=True, timeout=8)

    def test_interrupt_after_resource_creation_cleans_all_owned_resources(self):
        for stage, expected in (
            ("schema", ["drop_schema"]),
            ("user", ["drop_schema", "drop_user"]),
            ("network", ["drop_schema", "drop_user", "remove_network"]),
            ("container", ["drop_schema", "drop_user", "remove_container"]),
            ("cluster-container", ["drop_schema", "drop_user", "remove_container", "remove_network"]),
            ("second-signal", ["drop_schema", "drop_user", "remove_container"]),
            ("wait", ["drop_schema", "drop_user", "remove_container"]),
        ):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as directory:
                result = self.invoke(directory, stage)
                self.assertEqual(result.returncode, 143, result.stdout + result.stderr)
                state = json.loads((Path(directory) / "state.json").read_text())
                self.assertEqual(state, dict(schema=False, user=False, network=False, containers=[]))
                self.assertCountEqual((Path(directory) / "cleanup.txt").read_text().splitlines(), expected)

    def test_interrupt_during_api_spawn_preserves_child_ownership(self):
        self.check_child_cleanup("spawn")

    def test_interrupt_during_sql_lock_spawn_preserves_child_ownership(self):
        self.check_child_cleanup("sql-lock")

    def check_child_cleanup(self, stage):
        with tempfile.TemporaryDirectory() as directory:
            child_path = Path(directory) / "child.pid"
            try:
                result = self.invoke(directory, stage)
                self.assertEqual(result.returncode, 143, result.stdout + result.stderr)
                self.assertTrue(child_path.exists(), "a real child must have started")
                stat = Path(f"/proc/{child_path.read_text()}/stat")
                if stat.exists():
                    self.assertIn(stat.read_text().rsplit(") ", 1)[1].split()[0], ("Z", "X"),
                                  "cancelled launcher leaked its child")
                if stage == "spawn":
                    state = json.loads((Path(directory) / "state.json").read_text())
                    self.assertEqual(state, dict(schema=False, user=False, network=False, containers=[]))
            finally:
                if child_path.exists():
                    try:
                        os.kill(int(child_path.read_text()), signal.SIGKILL)
                    except ProcessLookupError:
                        pass


if __name__ == "__main__":
    unittest.main()
