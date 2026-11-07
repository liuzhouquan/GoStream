#!/usr/bin/env python3
"""FFprobe compatibility shim for environments where ffprobe is unavailable.

Delegates to system ffprobe when available; otherwise invokes ffmpeg -i to
extract container duration, video stream dimensions, and stream duration,
returning the JSON payload expected by GoStream's parseVideoProbe.
"""

import json
import os
import re
import shutil
import subprocess
import sys


def find_real_ffprobe():
    real = os.environ.get("REAL_FFPROBE_BIN")
    if real and shutil.which(real):
        return real
    system_ffprobe = shutil.which("ffprobe")
    if system_ffprobe:
        try:
            if not os.path.samefile(system_ffprobe, __file__):
                return system_ffprobe
        except OSError:
            pass
    for path in ["/usr/bin/ffprobe", "/usr/local/bin/ffprobe"]:
        if os.path.isfile(path) and os.access(path, os.X_OK):
            try:
                if not os.path.samefile(path, __file__):
                    return path
            except OSError:
                return path
    return None


def find_ffmpeg():
    if "FFMPEG_BIN" in os.environ and shutil.which(os.environ["FFMPEG_BIN"]):
        return os.environ["FFMPEG_BIN"]
    system_ffmpeg = shutil.which("ffmpeg")
    if system_ffmpeg:
        return system_ffmpeg
    candidates = [
        "/home/lzq/.vscode-server/extensions/kilocode.kilo-code-7.8.27-linux-x64/bin/ffmpeg",
        "/home/lzq/.vscode-server/extensions/kilocode.kilo-code-7.8.18-linux-x64/bin/ffmpeg",
        "/usr/bin/ffmpeg",
        "/usr/local/bin/ffmpeg",
    ]
    for c in candidates:
        if os.path.isfile(c) and os.access(c, os.X_OK):
            return c
    return "ffmpeg"


def probe_with_ffmpeg(ffmpeg_bin, target_path):
    if not os.path.exists(target_path):
        sys.stderr.write(f"ffprobe_shim: {target_path}: No such file or directory\n")
        return 1

    proc = subprocess.run([ffmpeg_bin, "-hide_banner", "-i", target_path],
                          capture_output=True, text=True)
    combined = proc.stdout + "\n" + proc.stderr

    # Check for fatal corruptions where ffmpeg cannot even parse container
    if "Invalid data found when processing input" in combined:
        sys.stderr.write(f"ffprobe_shim: invalid data found when processing {target_path}\n")
        return 1

    # Extract duration: Duration: 00:00:01.00, start: 0.000000, bitrate: 90 kb/s
    dur_match = re.search(r"Duration:\s*(\d+):(\d+):([\d\.]+)", combined)
    duration_str = "N/A"
    if dur_match:
        try:
            h = float(dur_match.group(1))
            m = float(dur_match.group(2))
            s = float(dur_match.group(3))
            duration_str = f"{(h * 3600.0 + m * 60.0 + s):.6f}"
        except ValueError:
            duration_str = "N/A"

    # Extract video streams
    # e.g.: Stream #0:0(und): Video: h264 (High) (avc1 / ...), yuv420p, 640x360 ...
    streams = []
    # Match lines with 'Video:'
    for line in combined.splitlines():
        if "Stream #" in line and "Video:" in line:
            # find dimensions: e.g. 640x360 or 1920x1080
            dim_match = re.search(r",\s*(\d{2,5})x(\d{2,5})\b", line)
            if dim_match:
                w = int(dim_match.group(1))
                h = int(dim_match.group(2))
                streams.append({
                    "width": w,
                    "height": h,
                    "duration": duration_str,
                })

    report = {
        "streams": streams,
        "format": {
            "duration": duration_str,
        },
    }
    sys.stdout.write(json.dumps(report) + "\n")
    return 0


def main():
    args = sys.argv[1:]
    real_ffprobe = find_real_ffprobe()
    if real_ffprobe:
        result = subprocess.run([real_ffprobe] + args)
        return result.returncode

    # If no real ffprobe, find target path from args
    target_path = None
    for arg in reversed(args):
        if not arg.startswith("-"):
            target_path = arg
            break

    if not target_path:
        sys.stderr.write("ffprobe_shim: missing target file argument\n")
        return 1

    ffmpeg_bin = find_ffmpeg()
    return probe_with_ffmpeg(ffmpeg_bin, target_path)


if __name__ == "__main__":
    sys.exit(main())
