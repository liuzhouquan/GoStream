"""Exercise both direct and mirror+file Ubuntu apt origin configuration."""
from pathlib import Path
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tools"))
from rewrite_ci_apt_mirrors import rewrite_sources


class CIAptMirrorsTest(unittest.TestCase):
    def run_installer(self, directory, all_fail):
        root = Path(directory)
        apt = root / "apt"
        apt.mkdir()
        (apt / "apt-mirrors.txt").write_text("http://azure.archive.ubuntu.com/ubuntu\n")
        bin_dir = root / "bin"
        bin_dir.mkdir()
        scripts = {
            "curl": '#!/bin/sh\nprintf "%s\\n" "$*" >> "$PROBE_LOG"\ncase "$*" in *https://azure.archive.ubuntu.com/*) [ "$ALL_FAIL" = 0 ];; *) exit 28;; esac\n',
            "sudo": '#!/bin/sh\nprintf "%s\\n" "$*" >> "$SUDO_LOG"\n[ "$1" = python3 ] || exit 99\nexec python3 "$2" "$FIXTURE_APT_DIR" "$4"\n',
            "ffmpeg": "#!/bin/sh\nexit 0\n", "ffprobe": "#!/bin/sh\nexit 0\n",
        }
        for name, content in scripts.items():
            p = bin_dir / name
            p.write_text(content)
            p.chmod(0o700)
        env = {**os.environ, "GITHUB_ACTIONS": "true", "PATH": str(bin_dir) + os.pathsep + os.environ["PATH"],
               "PROBE_LOG": str(root / "probes"), "SUDO_LOG": str(root / "sudo"),
               "FIXTURE_APT_DIR": str(apt), "ALL_FAIL": "1" if all_fail else "0"}
        script = Path(__file__).resolve().parents[2] / "tools/install_ci_ffmpeg.sh"
        return subprocess.run(["bash", str(script)], env=env, capture_output=True, text=True, timeout=10)

    def test_installer_selects_reachable_official_origin(self):
        with tempfile.TemporaryDirectory() as directory:
            result = self.run_installer(directory, False)
            self.assertEqual(result.returncode, 0, result.stderr)
            root = Path(directory)
            probes = (root / "probes").read_text().splitlines()
            self.assertEqual(len(probes), 2)
            self.assertTrue(all("--ipv4" in p and "--max-time 8" in p for p in probes))
            self.assertEqual((root / "apt/apt-mirrors.txt").read_text(), "https://azure.archive.ubuntu.com/ubuntu\n")

    def test_installer_fails_before_mutation_if_all_probes_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            result = self.run_installer(directory, True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("No official Ubuntu origin", result.stderr)
            self.assertEqual(len((Path(directory) / "probes").read_text().splitlines()), 3)
            self.assertFalse((Path(directory) / "sudo").exists())

    def test_indirect_mirrorlist_is_rewritten_with_sources(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            sources = root / "sources.list.d" / "ubuntu.sources"
            sources.parent.mkdir()
            before = "Types: deb\nURIs: mirror+file:/etc/apt/apt-mirrors.txt\nSuites: noble noble-updates\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n"
            sources.write_text(before)
            mirrors = root / "apt-mirrors.txt"
            mirrors.write_text("http://azure.archive.ubuntu.com/ubuntu\thttps://azure.archive.ubuntu.com/ubuntu\nhttps://security.ubuntu.com/ubuntu\n")
            changed = rewrite_sources(root)
            self.assertEqual(changed, [str(mirrors)])
            self.assertEqual(sources.read_text(), before)
            self.assertEqual(mirrors.read_text(), "https://archive.ubuntu.com/ubuntu\thttps://archive.ubuntu.com/ubuntu\nhttps://security.ubuntu.com/ubuntu\n")
            self.assertEqual(rewrite_sources(root), [])

    def test_direct_sources_preserve_suite_signature_and_other_origins(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "sources.list.d").mkdir()
            line = "deb [signed-by=/keys/ubuntu.gpg] http://azure.archive.ubuntu.com/ubuntu noble main\n"
            for path in (root / "sources.list", root / "sources.list.d/ubuntu.list"):
                path.write_text(line)
            other = root / "sources.list.d/microsoft.sources"
            other.write_text("URIs: https://packages.microsoft.com/ubuntu/24.04/prod\nSigned-By: /keys/ms.gpg\n")
            rewrite_sources(root)
            self.assertEqual((root / "sources.list").read_text(), line.replace("http://azure.archive.ubuntu.com/ubuntu", "https://archive.ubuntu.com/ubuntu"))
            self.assertEqual(other.read_text(), "URIs: https://packages.microsoft.com/ubuntu/24.04/prod\nSigned-By: /keys/ms.gpg\n")
            with self.assertRaises(ValueError):
                rewrite_sources(root, "https://untrusted.test/ubuntu")


if __name__ == "__main__":
    unittest.main()
