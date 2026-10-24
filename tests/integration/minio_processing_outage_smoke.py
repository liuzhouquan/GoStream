"""Verify a durable FFmpeg job survives MinIO downtime before it reads source.

Run only against the isolated gostream-object Compose project with two API
replicas. The script restarts MinIO in a finally block and leaves fixture rows
in the isolated database for inspection.
"""

import argparse
import json
import secrets
import subprocess
import time
import uuid
from pathlib import Path

from media_smoke import call
from minio_outage_smoke import API_A, API_B, compose, wait_for_minio


def job_state(video_id):
    result = subprocess.run([
        "docker", "exec", "gostream-object-mysql-1", "sh", "-c",
        'MYSQL_PWD="$MYSQL_PASSWORD" mysql -ugostream -D gostream --batch --skip-column-names '
        f'-e "SELECT attempts, done_at IS NOT NULL, dead_at IS NOT NULL, LENGTH(last_error) > 0 FROM video_process_jobs WHERE video_id = {video_id}"',
    ], check=True, capture_output=True, text=True)
    fields = result.stdout.strip().split("\t")
    if len(fields) != 4:
        raise RuntimeError(f"missing video job for {video_id}: {result.stdout!r}")
    return tuple(map(int, fields))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--video-file", required=True, type=Path)
    args = parser.parse_args()
    source = args.video_file.read_bytes()
    if len(source) < 12 or source[4:8] != b"ftyp":
        raise RuntimeError("fixture must be a valid MP4")
    suffix = uuid.uuid4().hex[:12]
    account = call(API_A, "/api/users", "POST", body=json.dumps({
        "username": f"minio_job_{suffix}", "password": secrets.token_urlsafe(24),
        "nickname": "MinIO job outage fixture",
    }).encode())
    token = account["access_token"]
    boundary = "gostream-" + suffix
    payload = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; "
               f"filename=\"source.mp4\"\r\nContent-Type: video/mp4\r\n\r\n").encode()
    payload += source + f"\r\n--{boundary}--\r\n".encode()
    asset = call(API_A, "/api/uploads", "POST", token, payload,
                 f"multipart/form-data; boundary={boundary}")

    stopped = False
    video_id = None
    failed_attempts = 0
    try:
        compose("stop", "minio")
        stopped = True
        created = call(API_B, "/api/videos", "POST", token, json.dumps({
            "title": "MinIO retry after source outage", "media_url": asset["url"],
        }).encode())
        video_id = created["id"]
        if created["status"] != "processing":
            raise RuntimeError(f"unexpected initial state: {created['status']}")
        deadline = time.monotonic() + 25
        while time.monotonic() < deadline:
            failed_attempts, done, dead, has_error = job_state(video_id)
            if failed_attempts >= 1 and has_error:
                if done or dead:
                    raise RuntimeError("job reached a final state while storage was down")
                break
            time.sleep(1)
        else:
            raise RuntimeError("worker did not attempt processing while MinIO was down")
        if call(API_A, f"/api/videos/{video_id}", token=token)["status"] != "processing":
            raise RuntimeError("video became visible before source was available")
        feed = call(API_A, "/api/feed?scene=timeline&limit=20")
        if any(item["id"] == video_id for item in feed["items"]):
            raise RuntimeError("unprocessed video appeared in public Feed")
    finally:
        if stopped:
            compose("start", "minio")
            wait_for_minio()

    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        item = call(API_B, f"/api/videos/{video_id}", token=token)
        if item["status"] == "published":
            break
        if item["status"] == "failed":
            raise RuntimeError("job exhausted retries after MinIO recovered")
        time.sleep(1)
    else:
        raise RuntimeError("job did not publish after MinIO recovered")
    if len(call(API_A, item["media_url"])) < 12 or not call(API_B, item["cover_url"]).startswith(b"\xff\xd8"):
        raise RuntimeError("published output objects are missing")
    attempts, done, dead, _ = job_state(video_id)
    if attempts < 2 or not done or dead:
        raise RuntimeError(f"unexpected final job state: {(attempts, done, dead)}")
    print(json.dumps({"video_id": video_id, "failed_attempts_before_recovery": failed_attempts,
                      "final_attempts": attempts, "status": item["status"]}))


if __name__ == "__main__":
    main()
