"""Exercise two real APIs sharing MySQL and MinIO, with FFmpeg enabled.

Run against an isolated Compose project. The video must be a valid MP4 larger
than 4 MiB, so the two API processes receive different upload chunks.
"""

import argparse
import hashlib
import json
import secrets
import time
import uuid
from pathlib import Path
from urllib.request import Request, urlopen

from media_smoke import call


CHUNK_SIZE = 4 << 20


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--api-a", default="http://127.0.0.1:18080")
    parser.add_argument("--api-b", default="http://127.0.0.1:18081")
    parser.add_argument("--video-file", required=True, type=Path)
    args = parser.parse_args()
    api_a, api_b = args.api_a.rstrip("/"), args.api_b.rstrip("/")
    source = args.video_file.read_bytes()
    if len(source) <= CHUNK_SIZE or source[4:8] != b"ftyp":
        raise RuntimeError("fixture must be a valid MP4 larger than 4 MiB")

    suffix = uuid.uuid4().hex[:12]
    account = call(api_a, "/api/users", "POST", body=json.dumps({
        "username": f"minio_demo_{suffix}",
        "password": secrets.token_urlsafe(24),
        "nickname": "MinIO media demo",
    }).encode())
    token = account["access_token"]
    session = call(api_a, "/api/uploads/sessions", "POST", token, json.dumps({
        "filename": "source.mp4", "size": len(source), "sha256": hashlib.sha256(source).hexdigest(),
    }).encode())
    session_id = session["id"]
    count = (len(source) + CHUNK_SIZE - 1) // CHUNK_SIZE
    for index in range(count):
        base = api_a if index % 2 == 0 else api_b
        chunk = source[index * CHUNK_SIZE:(index + 1) * CHUNK_SIZE]
        reply = call(base, f"/api/uploads/sessions/{session_id}/chunks/{index}",
                     "PUT", token, chunk, "application/octet-stream")
        if reply["sha256"] != hashlib.sha256(chunk).hexdigest():
            raise RuntimeError(f"chunk {index} hash mismatch")
    status = call(api_b, f"/api/uploads/sessions/{session_id}", token=token)
    if status["uploaded_chunks"] != list(range(count)):
        raise RuntimeError(f"cross-instance chunk status mismatch: {status}")
    completed = call(api_b, f"/api/uploads/sessions/{session_id}/complete", "POST", token)
    if completed["sha256"] != hashlib.sha256(source).hexdigest():
        raise RuntimeError("assembled object hash mismatch")
    if call(api_a, f"/api/uploads/sessions/{session_id}/complete", "POST", token)["url"] != completed["url"]:
        raise RuntimeError("completion retry changed the object URL")

    created = call(api_a, "/api/videos", "POST", token, json.dumps({
        "title": "MinIO two-instance processing demo", "media_url": completed["url"],
    }).encode())
    if created["status"] != "processing":
        raise RuntimeError(f"expected processing, got {created['status']}")
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        video = call(api_b, f"/api/videos/{created['id']}", token=token)
        if video["status"] == "published":
            break
        if video["status"] == "failed":
            raise RuntimeError(f"FFmpeg job failed: {video.get('processing_error')}")
        time.sleep(1)
    else:
        raise RuntimeError("FFmpeg job did not publish within 120 seconds")

    output = call(api_b, video["media_url"])
    cover = call(api_a, video["cover_url"])
    if len(output) < 12 or output[4:8] != b"ftyp" or not cover.startswith(b"\xff\xd8"):
        raise RuntimeError("published video or cover object is invalid")
    if min(video["duration_ms"], video["width"], video["height"]) <= 0:
        raise RuntimeError("published video is missing FFprobe metadata")
    with urlopen(Request(api_a + video["media_url"], headers={"Range": "bytes=0-15"}), timeout=15) as ranged:
        if ranged.status != 206 or ranged.read() != output[:16]:
            raise RuntimeError("cross-instance media Range response is incorrect")
    feed = call(api_b, "/api/feed?scene=timeline&limit=20")
    if not any(item["id"] == created["id"] for item in feed["items"]):
        raise RuntimeError("published object-backed video is absent from Feed")
    print(json.dumps({
        "video_id": created["id"], "chunks": count, "source_bytes": len(source),
        "output_bytes": len(output), "cover_bytes": len(cover),
        "duration_ms": video["duration_ms"], "width": video["width"], "height": video["height"],
        "media_url": video["media_url"], "cover_url": video["cover_url"],
    }))


if __name__ == "__main__":
    main()
