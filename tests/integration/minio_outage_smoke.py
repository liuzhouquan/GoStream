"""Stop isolated MinIO, verify upload failure leaves no asset, then recover.

Requires the two-replica gostream-object Compose project from the object
storage documentation. The script restarts MinIO in a finally block.
"""

import hashlib
import json
import secrets
import signal
import subprocess
import sys
import time
import uuid
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

from media_smoke import call
from launcher_lifecycle import LauncherInterrupted, cleanup_on_interrupt


COMPOSE = [
    "docker", "compose", "-p", "gostream-object",
    "-f", "docker-compose.yml", "-f", "docker-compose.object.yml",
    "-f", "tests/integration/object-test.compose.yml",
]
API_A = "http://127.0.0.1:18080"
API_B = "http://127.0.0.1:18081"
SAMPLE = bytes([0, 0, 0, 24]) + b"ftypisom" + bytes(4) + b"isommp42"


def compose(*args):
    subprocess.run(COMPOSE + list(args), check=True, stdout=subprocess.DEVNULL)


def count_assets():
    result = subprocess.run([
        "docker", "exec", "gostream-object-mysql-1", "sh", "-c",
        'MYSQL_PWD="$MYSQL_PASSWORD" mysql -ugostream -D gostream --batch --skip-column-names '
        '-e "SELECT count(*) FROM media_assets"',
    ], check=True, capture_output=True, text=True)
    return int(result.stdout.strip())


def count_chunks(session_id):
    result = subprocess.run([
        "docker", "exec", "gostream-object-mysql-1", "sh", "-c",
        'MYSQL_PWD="$MYSQL_PASSWORD" mysql -ugostream -D gostream --batch --skip-column-names '
        f'-e "SELECT count(*) FROM upload_chunks WHERE session_id = \'{session_id}\'"',
    ], check=True, capture_output=True, text=True)
    return int(result.stdout.strip())


def request(base, path, method, token, body, content_type):
    headers = {"Authorization": "Bearer " + token, "Content-Type": content_type}
    req = Request(base + path, data=body, method=method, headers=headers)
    try:
        with urlopen(req, timeout=45) as response:
            data = response.read()
            result = json.loads(data) if response.headers.get_content_type() == "application/json" else data
            return response.status, result
    except HTTPError as error:
        data = error.read()
        return error.code, json.loads(data) if data else {}


def upload(token):
    boundary = "gostream-" + uuid.uuid4().hex
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; "
            f"filename=\"outage.mp4\"\r\nContent-Type: video/mp4\r\n\r\n").encode()
    body += SAMPLE + f"\r\n--{boundary}--\r\n".encode()
    request = Request(API_A + "/api/uploads", data=body, method="POST", headers={
        "Authorization": "Bearer " + token,
        "Content-Type": f"multipart/form-data; boundary={boundary}",
    })
    try:
        with urlopen(request, timeout=45) as response:
            return response.status, json.load(response)
    except HTTPError as error:
        return error.code, json.load(error)


def wait_for_minio():
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        try:
            with urlopen("http://127.0.0.1:29000/minio/health/live", timeout=2) as response:
                if response.status == 200:
                    return
        except (URLError, TimeoutError):
            pass
        time.sleep(1)
    raise RuntimeError("MinIO did not recover within 45 seconds")


@cleanup_on_interrupt()
def main():
    suffix = uuid.uuid4().hex[:12]
    account = call(API_A, "/api/users", "POST", body=json.dumps({
        "username": f"minio_outage_{suffix}",
        "password": secrets.token_urlsafe(24),
        "nickname": "Outage fixture",
    }).encode())
    token = account["access_token"]
    before = count_assets()
    chunk_data = SAMPLE + b"outage-resumable-chunk"
    session = call(API_A, "/api/uploads/sessions", "POST", token, json.dumps({
        "filename": "resumable-outage.mp4",
        "size": len(chunk_data),
        "sha256": hashlib.sha256(chunk_data).hexdigest(),
    }).encode())
    session_path = f"/api/uploads/sessions/{session['id']}"
    stopped = False
    try:
        stopped = True
        compose("stop", "minio")
        status, error = upload(token)
        if status != 503 or error.get("error") != "media storage unavailable":
            raise RuntimeError(f"unexpected outage response: {status} {error}")
        status, error = request(API_B, session_path + "/chunks/0", "PUT", token,
                                chunk_data, "application/octet-stream")
        if status != 503 or error.get("error") != "media storage unavailable":
            raise RuntimeError(f"unexpected resumable chunk outage response: {status} {error}")
        if count_chunks(session["id"]) != 0:
            raise RuntimeError("failed resumable chunk was persisted in MySQL")
        session_state = call(API_B, session_path, token=token)
        if session_state["status"] != "open" or session_state["uploaded_chunks"]:
            raise RuntimeError(f"failed resumable chunk changed session state: {session_state}")
        if count_assets() != before:
            raise RuntimeError("failed upload created a media asset")
        if call(API_B, "/ready")["status"] != "ready":
            raise RuntimeError("database readiness changed during MinIO outage")
    finally:
        for number in (signal.SIGINT, signal.SIGTERM):
            signal.signal(number, signal.SIG_IGN)
        if stopped:
            compose("start", "minio")
            wait_for_minio()
    status, chunk = request(API_B, session_path + "/chunks/0", "PUT", token,
                            chunk_data, "application/octet-stream")
    if status != 200 or chunk.get("sha256") != hashlib.sha256(chunk_data).hexdigest():
        raise RuntimeError(f"resumable chunk did not recover: {status} {chunk}")
    completed = call(API_B, session_path + "/complete", "POST", token)
    if completed.get("sha256") != hashlib.sha256(chunk_data).hexdigest():
        raise RuntimeError(f"resumable completion hash mismatch: {completed}")
    repeated = call(API_A, session_path + "/complete", "POST", token)
    if repeated.get("url") != completed.get("url"):
        raise RuntimeError("cross-instance completion retry changed the media URL")
    if call(API_B, completed["url"]) != chunk_data:
        raise RuntimeError("resumed object bytes differ from the uploaded chunk")
    if count_assets() != before + 1:
        raise RuntimeError("resumed completion did not create exactly one media asset")
    status, asset = upload(token)
    if status != 201 or call(API_B, asset["url"]) != SAMPLE:
        raise RuntimeError(f"upload did not recover: {status} {asset}")
    print(json.dumps({"outage_status": 503, "assets_before": before,
                      "failed_chunk_rows": 0, "resumable_status_after_failure": "open",
                      "completion_cross_instance": True, "assets_after_recovery": count_assets(),
                      "resumed_url": completed["url"], "recovered_url": asset["url"]}))


if __name__ == "__main__":
    try:
        main()
    except LauncherInterrupted as error:
        print("MinIO outage check interrupted; storage restart completed", file=sys.stderr)
        raise SystemExit(128 + int(error.args[0]))
