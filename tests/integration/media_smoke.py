"""Exercise real upload, FFmpeg processing, cover generation and Feed publication.

Usage: python3 tests/integration/media_smoke.py --video-file /path/to/valid.mp4
The target API must have FFMPEG_BIN enabled. This creates one demo user/video.
"""

import argparse
import json
import secrets
import time
import uuid
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen


def call(base, path, method="GET", token="", body=None, content_type="application/json"):
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if body is not None:
        headers["Content-Type"] = content_type
    try:
        with urlopen(Request(base + path, data=body, headers=headers, method=method), timeout=15) as response:
            data = response.read()
            return json.loads(data) if response.headers.get_content_type() == "application/json" else data
    except HTTPError as error:
        raise RuntimeError(f"{method} {path}: {error.code} {error.read().decode()}") from error


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--video-file", required=True, type=Path)
    args = parser.parse_args()
    base = args.base_url.rstrip("/")
    video = args.video_file.read_bytes()
    if not video:
        raise RuntimeError("video file is empty")
    suffix = uuid.uuid4().hex[:12]
    account = call(base, "/api/users", "POST", body=json.dumps({
        "username": f"media_demo_{suffix}", "password": secrets.token_urlsafe(24), "nickname": "Media demo"
    }).encode())
    token = account["access_token"]
    boundary = f"gostream-{suffix}"
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"sample.mp4\"\r\n"
            f"Content-Type: video/mp4\r\n\r\n").encode() + video + f"\r\n--{boundary}--\r\n".encode()
    asset = call(base, "/api/uploads", "POST", token, body, f"multipart/form-data; boundary={boundary}")
    created = call(base, "/api/videos", "POST", token, json.dumps({
        "title": "GoStream media processing demo", "media_url": asset["url"]
    }).encode())
    if created["status"] != "processing":
        raise RuntimeError(f"expected processing status, got {created['status']}")
    video_id = created["id"]
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        item = call(base, f"/api/videos/{video_id}", token=token)
        if item["status"] == "published":
            break
        if item["status"] == "failed":
            raise RuntimeError("FFmpeg processing failed; inspect API worker logs")
        time.sleep(1)
    else:
        raise RuntimeError("video processing did not finish within 60 seconds")
    media_bytes = call(base, item["media_url"])
    cover_bytes = call(base, item["cover_url"])
    if len(media_bytes) < 12 or media_bytes[4:8] != b"ftyp":
        raise RuntimeError("processed media is not MP4")
    if not cover_bytes.startswith(b"\xff\xd8"):
        raise RuntimeError("generated cover is not JPEG")
    feed = call(base, "/api/feed?scene=timeline&limit=20")
    if not any(row["id"] == video_id for row in feed["items"]):
        raise RuntimeError("processed video is absent from public Feed")
    print(json.dumps({"video_id": video_id, "status": item["status"],
                      "media_bytes": len(media_bytes), "cover_bytes": len(cover_bytes),
                      "media_url": item["media_url"], "cover_url": item["cover_url"]}))


if __name__ == "__main__":
    main()
