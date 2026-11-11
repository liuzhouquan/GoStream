#!/usr/bin/env python3
"""Rewrite only Azure Ubuntu origins; preserve suites and signing directives."""
from pathlib import Path
import sys


def rewrite_sources(directory: Path) -> list[str]:
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
            updated = updated.replace(origin, "https://archive.ubuntu.com/ubuntu")
        if updated != original:
            path.write_text(updated)
            changed.append(str(path))
    return changed


if __name__ == "__main__":
    # Explicit path: this helper has no developer-machine default.
    if len(sys.argv) != 2:
        raise SystemExit("usage: rewrite_ci_apt_mirrors.py APT_DIRECTORY")
    for path in rewrite_sources(Path(sys.argv[1])):
        print(f"Updated Ubuntu origin: {path}")
