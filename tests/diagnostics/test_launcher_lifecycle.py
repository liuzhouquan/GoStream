"""Real captured subprocess I/O must not swallow launcher cancellation."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class LauncherLifecycleTest(unittest.TestCase):
    def test_sigterm_during_captured_subprocess_unwinds_each_launcher(self):
        code = r'''
import importlib,signal,subprocess,sys
from pathlib import Path
sys.path.insert(0,sys.argv[1])
launcher=importlib.import_module(sys.argv[2])
interrupted=getattr(launcher,'LauncherInterrupted',InterruptedError)
child="import os,signal,time;time.sleep(.05);os.kill(os.getppid(),signal.SIGTERM);time.sleep(.2);print('command finished')"
try:
    with launcher.cleanup_on_interrupt():
        try:
            subprocess.run([sys.executable,'-c',child],capture_output=True,text=True,check=True,timeout=2)
        finally:
            Path(sys.argv[3]).write_text('cleanup ran')
except interrupted as error:
    raise SystemExit(128+int(error.args[0]))
'''
        for name in ("im_smoke", "rabbitmq_smoke"):
            with self.subTest(launcher=name), tempfile.TemporaryDirectory() as directory:
                marker = Path(directory) / "cleaned.txt"
                result = subprocess.run(
                    [sys.executable, "-c", code, str(ROOT / "tests/integration"), name, str(marker)],
                    capture_output=True, text=True, timeout=4,
                )
                self.assertEqual(result.returncode, 143, result.stdout + result.stderr)
                self.assertEqual(marker.read_text(), "cleanup ran")


if __name__ == "__main__":
    unittest.main()
