"""Exercise real cancellation signals with simulated MinIO operations."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class MinIOOutageCleanupTest(unittest.TestCase):
    def test_interrupt_during_stop_restores_minio_despite_second_signal(self):
        code = r'''
import os,signal,sys
from pathlib import Path
from unittest.mock import patch
root,report=map(Path,sys.argv[1:])
sys.path.insert(0,str(root/'tests/integration'))
import minio_outage_smoke as smoke
def compose(*args):
    if args[0]=='start':
        report.write_text('start\n')
    os.kill(os.getpid(),signal.SIGTERM)
def recovered():
    with report.open('a') as stream: stream.write('ready\n')
with patch.object(smoke,'call',side_effect=[{'access_token':'fixture'}, {'id':'a'*32}]), \
     patch.object(smoke,'count_assets',return_value=0), \
     patch.object(smoke,'compose',side_effect=compose), \
     patch.object(smoke,'wait_for_minio',side_effect=recovered):
    try: smoke.main()
    except smoke.LauncherInterrupted: raise SystemExit(143)
'''
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "recovery.txt"
            result = subprocess.run([sys.executable, "-c", code, str(ROOT), str(report)],
                                    capture_output=True, text=True, timeout=8)
            self.assertEqual(result.returncode, 143, result.stdout + result.stderr)
            self.assertEqual(report.read_text().splitlines(), ["start", "ready"])


if __name__ == "__main__":
    unittest.main()
