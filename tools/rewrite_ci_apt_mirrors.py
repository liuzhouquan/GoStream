#!/usr/bin/env python3
"""Rewrite only Azure Ubuntu origins; preserve suites and signing directives."""
from pathlib import Path
import sys


OFFICIAL_MIRRORS = ("https://archive.ubuntu.com/ubuntu", "https://azure.archive.ubuntu.com/ubuntu",
                    "https://us.archive.ubuntu.com/ubuntu")


def rewrite_sources(directory: Path, mirror: str = OFFICIAL_MIRRORS[0]) -> list[str]:
    if mirror not in OFFICIAL_MIRRORS:
        raise ValueError("only the configured official Ubuntu mirrors are permitted")
    candidates = [directory / "sources.list", *sorted((directory / "sources.list.d").glob("*.sources")),
                  *sorted((directory / "sources.list.d").glob("*.list")),
                  *sorted(directory.glob("apt-mirrors*.txt"))]
    changed = []
    for path in candidates:
        if not path.is_file():
            continue
        original = path.read_text()
        updated = original
        for origin in ("http://azure.archive.ubuntu.com/ubuntu", "https://azure.archive.ubuntu.com/ubuntu"):
            updated = updated.replace(origin, mirror)
        if updated != original:
            path.write_text(updated)
            changed.append(str(path))
    return changed


if __name__ == "__main__":
    # Explicit path: this helper has no developer-machine default.
    if len(sys.argv) not in (2, 3):
        raise SystemExit("usage: rewrite_ci_apt_mirrors.py APT_DIRECTORY [OFFICIAL_MIRROR]")
    for path in rewrite_sources(Path(sys.argv[1]), sys.argv[2] if len(sys.argv) == 3 else OFFICIAL_MIRRORS[0]):
        print(f"Updated Ubuntu origin: {path}")
