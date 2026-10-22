"""Verify an overlong MP4 fails with an author-only reason.

Usage: python3 tests/integration/media_failure_smoke.py --video-file /path/to/overlong.mp4
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

from media_smoke import call


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--video-file", required=True, type=Path)
    args = parser.parse_args()
    base = args.base_url.rstrip("/")
    payload = args.video_file.read_bytes()
    if len(payload) < 12 or payload[4:8] != b"ftyp":
        raise RuntimeError("fixture must be an MP4 file")
    suffix = uuid.uuid4().hex[:12]
    account = call(base, "/api/users", "POST", body=json.dumps({
        "username": f"media_failure_{suffix}", "password": secrets.token_urlsafe(24), "nickname": "Media failure demo"
    }).encode())
    token = account["access_token"]
    boundary = f"gostream-{suffix}"
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"overlong.mp4\"\r\n"
            f"Content-Type: video/mp4\r\n\r\n").encode() + payload + f"\r\n--{boundary}--\r\n".encode()
    asset = call(base, "/api/uploads", "POST", token, body, f"multipart/form-data; boundary={boundary}")
    video = call(base, "/api/videos", "POST", token, json.dumps({
        "title": "Overlong media rejection demo", "media_url": asset["url"]
    }).encode())
    if video["status"] != "processing":
        raise RuntimeError(f"expected processing, got {video['status']}")
    video_id = video["id"]
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        item = call(base, f"/api/videos/{video_id}", token=token)
        if item["status"] == "failed":
            break
        time.sleep(1)
    else:
        raise RuntimeError("video did not fail within 45 seconds")
    if item.get("processing_error") != "duration_limit":
        raise RuntimeError(f"unexpected failure code: {item.get('processing_error')}")
    owner_videos = call(base, f"/api/users/{account['user']['id']}/videos", token=token)["items"]
    if not any(v["id"] == video_id and v.get("processing_error") == "duration_limit" for v in owner_videos):
        raise RuntimeError("author list did not include the failure code")
    for path in [f"/api/videos/{video_id}", f"/api/users/{account['user']['id']}/videos"]:
        try:
            with urlopen(Request(base + path), timeout=15) as response:
                if path.endswith("/videos"):
                    rows = json.load(response)["items"]
                    if any(v["id"] == video_id for v in rows):
                        raise RuntimeError("failed video leaked into public profile")
                    continue
                raise RuntimeError("failed video is publicly readable")
        except HTTPError as error:
            if error.code != 404 or path.endswith("/videos"):
                raise
    print(json.dumps({"video_id": video_id, "user_id": account["user"]["id"],
                      "source_url": asset["url"], "status": item["status"],
                      "processing_error": item["processing_error"]}))


if __name__ == "__main__":
    main()
