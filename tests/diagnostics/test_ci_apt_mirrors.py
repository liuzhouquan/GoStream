"""Exercise both direct and mirror+file Ubuntu apt origin configuration."""
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tools"))
from rewrite_ci_apt_mirrors import rewrite_sources


class CIAptMirrorsTest(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
