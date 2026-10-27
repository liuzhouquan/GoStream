"""Exercise video recovery across API restarts with real FFmpeg and isolated SQLite.

python3 tests/integration/video_recovery_restart.py --binary /path/to/gostream \
    --ffmpeg /usr/bin/ffmpeg --ffprobe /usr/bin/ffprobe

The script creates its own temporary database and media directory, starts three
API processes, and removes all fixtures when finished.
"""

import argparse
import json
import os
import secrets
import socket
import subprocess
import tempfile
import time
import uuid
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def call(base, path, method="GET", token="", body=None, content_type="application/json"):
    headers = {}
    if token:
        headers["Authorization"] = "Bearer " + token
    if body is not None:
        headers["Content-Type"] = content_type
    try:
        with urlopen(Request(base + path, data=body, headers=headers, method=method), timeout=10) as response:
            return response.status, response.read(), response.headers.get_content_type()
    except HTTPError as error:
        return error.code, error.read(), error.headers.get_content_type()


def json_request(base, path, expected, method="GET", token="", value=None):
    body = json.dumps(value).encode() if value is not None else None
    status, raw, content_type = call(base, path, method, token, body)
    if status != expected:
        raise RuntimeError(f"{method} {path}: expected {expected}, got {status}: {raw.decode(errors='replace')}")
    if content_type != "application/json":
        raise RuntimeError(f"{method} {path}: expected JSON, got {content_type}")
    return json.loads(raw)


def upload(base, token, payload):
    boundary = "gostream-" + uuid.uuid4().hex
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"clip.mp4\"\r\n"
            "Content-Type: video/mp4\r\n\r\n").encode() + payload + f"\r\n--{boundary}--\r\n".encode()
    status, raw, _ = call(base, "/api/uploads", "POST", token, body, f"multipart/form-data; boundary={boundary}")
    if status != 201:
        raise RuntimeError(f"upload: expected 201, got {status}: {raw.decode(errors='replace')}")
    return json.loads(raw)["url"]


def make_video(ffmpeg, path, size, rate, duration):
    result = subprocess.run([
        ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i",
        f"color=c=blue:s={size}:r={rate}", "-t", str(duration), "-an", "-c:v", "libx264",
        "-preset", "ultrafast", "-threads", "2", "-pix_fmt", "yuv420p", "-y", str(path),
    ], capture_output=True, text=True, timeout=60)
    if result.returncode:
        raise RuntimeError(f"fixture generation failed: {result.stderr}")
    return path.read_bytes()


def start_api(binary, root, secret, ffmpeg, ffprobe):
    port = free_port()
    env = os.environ.copy()
    env.update({
        "ADDR": f"127.0.0.1:{port}", "JWT_SECRET": secret, "DB_DRIVER": "sqlite",
        "DB_DSN": str(root / "recovery.db") + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1",
        "MEDIA_DIR": str(root / "media"), "MEDIA_STORE": "local", "REDIS_ADDR": "", "PPROF_ADDR": "",
        "FFMPEG_BIN": ffmpeg, "FFPROBE_BIN": ffprobe, "FEED_FANOUT_MAX_FOLLOWERS": "1000",
    })
    log = tempfile.TemporaryFile(mode="w+")
    process = subprocess.Popen([str(binary)], env=env, stdout=log, stderr=subprocess.STDOUT)
    base = f"http://127.0.0.1:{port}"
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if process.poll() is not None:
            log.seek(0)
            details = log.read()
            log.close()
            raise RuntimeError(f"API exited before readiness: {process.returncode}\n{details}")
        try:
            with urlopen(base + "/ready", timeout=2) as response:
                if response.status == 200:
                    return process, log, base
        except (URLError, TimeoutError):
            pass
        time.sleep(0.1)
    process.terminate()
    process.wait(timeout=10)
    log.seek(0)
    details = log.read()
    log.close()
    raise RuntimeError(f"API did not become ready\n{details}")


def stop_api(process, log):
    timed_out = False
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
            timed_out = True
    log.seek(0)
    details = log.read()
    log.close()
    if timed_out:
        raise RuntimeError(f"API did not stop gracefully\n{details}")
    if process.returncode != 0:
        raise RuntimeError(f"API exited with {process.returncode}\n{details}")


def wait_video(base, path, token, wanted):
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        video = json_request(base, path, 200, token=token)
        if video["status"] == wanted:
            return video
        if wanted == "published" and video["status"] == "failed":
            raise RuntimeError(f"retry failed: {video.get('processing_error')}")
        time.sleep(0.25)
    raise RuntimeError(f"video did not reach {wanted} within 45 seconds")


def contains_video(base, path, video_id, token=""):
    return any(row["id"] == video_id for row in json_request(base, path, 200, token=token)["items"])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--ffmpeg", required=True)
    parser.add_argument("--ffprobe", required=True)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    with tempfile.TemporaryDirectory(prefix="gostream-video-recovery-") as temp:
        root = Path(temp)
        valid = make_video(args.ffmpeg, root / "valid.mp4", "160x90", 10, 1)
        overlong = make_video(args.ffmpeg, root / "overlong.mp4", "16x16", 1, 601)
        secret = secrets.token_urlsafe(48)
        process = log = None
        try:
            process, log, base = start_api(binary, root, secret, args.ffmpeg, args.ffprobe)
            author = json_request(base, "/api/users", 201, "POST", value={"username": "recovery_author", "password": "correct-horse-123"})
            follower = json_request(base, "/api/users", 201, "POST", value={"username": "recovery_follower", "password": "correct-horse-123"})
            token, follower_token = author["access_token"], follower["access_token"]
            json_request(base, f"/api/users/me/following/{author['user']['id']}", 200, "PUT", follower_token)
            old_source = upload(base, token, overlong)
            failed = json_request(base, "/api/videos", 201, "POST", token, {"title": "failed original", "media_url": old_source})
            if failed["status"] != "processing":
                raise RuntimeError(f"expected initial processing, got {failed['status']}")
            path = f"/api/videos/{failed['id']}"
            failed = wait_video(base, path, token, "failed")
            if failed.get("processing_error") != "duration_limit":
                raise RuntimeError(f"unexpected failure code: {failed.get('processing_error')}")
            stop_api(process, log)
            process = log = None

            process, log, base = start_api(binary, root, secret, "", args.ffprobe)
            replacement = upload(base, token, valid)
            rejected = json_request(base, path, 409, "PATCH", token, {"title": "must roll back", "media_url": replacement})
            if "video processing is unavailable" not in rejected.get("error", ""):
                raise RuntimeError(f"409 response lacks recovery guidance: {rejected}")
            unchanged = json_request(base, path, 200, token=token)
            if (unchanged["status"], unchanged["title"], unchanged["media_url"]) != ("failed", "failed original", old_source):
                raise RuntimeError(f"rejected edit changed video: {unchanged}")
            if call(base, path)[0] != 404 or contains_video(base, "/api/feed?scene=following", failed["id"], follower_token):
                raise RuntimeError("failed video became visible after rejection")
            immediate = json_request(base, "/api/videos", 201, "POST", token, {"title": "no processor", "media_url": replacement})
            if immediate["status"] != "published":
                raise RuntimeError("no-processor publication did not remain available")
            stop_api(process, log)
            process = log = None

            process, log, base = start_api(binary, root, secret, args.ffmpeg, args.ffprobe)
            retry = json_request(base, path, 200, "PATCH", token, {"media_url": replacement})
            if retry["status"] != "processing":
                raise RuntimeError(f"retry did not enter processing: {retry['status']}")
            result = wait_video(base, path, token, "published")
            if not all(result.get(key, 0) > 0 for key in ("duration_ms", "width", "height")):
                raise RuntimeError("processed video has no duration or dimensions")
            media_status, media, _ = call(base, result["media_url"])
            cover_status, cover, _ = call(base, result["cover_url"])
            if media_status != 200 or media[4:8] != b"ftyp" or cover_status != 200 or not cover.startswith(b"\xff\xd8"):
                raise RuntimeError("processed video or thumbnail cannot be read")
            if not contains_video(base, "/api/feed?scene=following", result["id"], follower_token):
                raise RuntimeError("recovered video is missing from following feed")
            print(json.dumps({"stages": 3, "initial_error": "duration_limit", "disabled_response": 409,
                              "recovered_status": result["status"], "following_feed": True}))
        finally:
            if process is not None:
                stop_api(process, log)


if __name__ == "__main__":
    main()
