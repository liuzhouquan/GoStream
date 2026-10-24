"""Stop isolated MinIO, verify upload failure leaves no asset, then recover.

Requires the two-replica gostream-object Compose project from the object
storage documentation. The script restarts MinIO in a finally block.
"""

import json
import secrets
import subprocess
import time
import uuid
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

from media_smoke import call


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


def main():
    suffix = uuid.uuid4().hex[:12]
    account = call(API_A, "/api/users", "POST", body=json.dumps({
        "username": f"minio_outage_{suffix}",
        "password": secrets.token_urlsafe(24),
        "nickname": "Outage fixture",
    }).encode())
    token = account["access_token"]
    before = count_assets()
    stopped = False
    try:
        compose("stop", "minio")
        stopped = True
        status, error = upload(token)
        if status != 503 or error.get("error") != "media storage unavailable":
            raise RuntimeError(f"unexpected outage response: {status} {error}")
        if count_assets() != before:
            raise RuntimeError("failed upload created a media asset")
        if call(API_B, "/ready")["status"] != "ready":
            raise RuntimeError("database readiness changed during MinIO outage")
    finally:
        if stopped:
            compose("start", "minio")
            wait_for_minio()
    status, asset = upload(token)
    if status != 201 or call(API_B, asset["url"]) != SAMPLE:
        raise RuntimeError(f"upload did not recover: {status} {asset}")
    print(json.dumps({"outage_status": 503, "assets_before": before,
                      "assets_after_recovery": count_assets(), "recovered_url": asset["url"]}))


if __name__ == "__main__":
    main()
