"""Exercise offline local-media import, object backup/restore, and cutover.

Run only with a disposable MinIO server. This script creates temporary SQLite
and media files, builds the exact local Go project, and leaves two random test
buckets in the disposable server for inspection until its volume is removed.
"""

import argparse
import hashlib
import json
import os
import re
import secrets
import socket
import subprocess
import tempfile
import time
import uuid
from pathlib import Path
from urllib.error import URLError
from urllib.request import urlopen

from media_smoke import call


SAMPLE = bytes([0, 0, 0, 24]) + b"ftypisom" + bytes(4) + b"isommp42"
RESUMABLE_SAMPLE = SAMPLE + bytes(4 << 20)


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def wait_ready(base, process):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"API exited before readiness: {process.returncode}")
        try:
            with urlopen(base + "/ready", timeout=2) as response:
                if response.status == 200:
                    return
        except (URLError, TimeoutError):
            pass
        time.sleep(0.25)
    raise RuntimeError("API did not become ready")


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def mysql_root(container, sql, database=None):
    command = ["docker", "exec", "-i", container, "sh", "-c",
               'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names']
    if database:
        command[-1] += ' "$1"'
        command += ["sh", database]
    subprocess.run(command, input=sql.encode(), check=True)


def mysql_query(container, database, sql):
    result = subprocess.run([
        "docker", "exec", "-i", container, "sh", "-c",
        'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names "$1"',
        "sh", database,
    ], input=sql, capture_output=True, text=True, check=True)
    return result.stdout.strip()


def require_empty_mysql_database(args):
    match = re.fullmatch(r"(.*/)([A-Za-z0-9_]+)(\?.*)", args.mysql_dsn)
    if not match:
        raise RuntimeError("--mysql-dsn must include a database name and query options")
    database = match.group(2)
    result = subprocess.run([
        "docker", "exec", "-i", args.mysql_container, "sh", "-c",
        'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names',
    ], input=f"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '{database}';\n",
       capture_output=True, text=True, check=True)
    if result.stdout.strip() != "0":
        raise RuntimeError(f"refusing nonempty MySQL database {database}")


def restore_mysql_snapshot(args, root):
    match = re.fullmatch(r"(.*/)([A-Za-z0-9_]+)(\?.*)", args.mysql_dsn)
    if not match:
        raise RuntimeError("--mysql-dsn must include a database name and query options")
    source_database = match.group(2)
    restored_database = "gostream_transfer_restore_" + uuid.uuid4().hex[:8]
    if not re.fullmatch(r"[A-Za-z0-9_]+", args.mysql_user):
        raise RuntimeError("unsafe MySQL user name")
    created = False
    try:
        mysql_root(args.mysql_container,
                   f"CREATE DATABASE `{restored_database}`;\n"
                   f"GRANT ALL PRIVILEGES ON `{restored_database}`.* TO '{args.mysql_user}'@'%';\n")
        created = True
        dump_path = root / "mysql-backup.sql"
        with dump_path.open("wb") as dump:
            subprocess.run([
                "docker", "exec", "-i", args.mysql_container, "sh", "-c",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --single-transaction '
                '--routines --triggers --no-tablespaces --set-gtid-purged=OFF "$1"',
                "sh", source_database,
            ], stdout=dump, check=True)
        if dump_path.stat().st_size == 0:
            raise RuntimeError("MySQL backup is empty")
        with dump_path.open("rb") as dump:
            subprocess.run([
                "docker", "exec", "-i", args.mysql_container, "sh", "-c",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot "$1"',
                "sh", restored_database,
            ], stdin=dump, check=True)
        markers = mysql_query(args.mysql_container, restored_database,
                              "SELECT COUNT(*) FROM schema_migrations WHERE `key` IN ('schema_v001', 'schema_v002', 'schema_v003', 'schema_v004', 'schema_v005');\n")
        if markers != "5":
            raise RuntimeError(f"restored migration markers mismatch: {markers}")
        dump_bytes = dump_path.read_bytes()
        return (match.group(1) + restored_database + match.group(3), restored_database,
                len(dump_bytes), hashlib.sha256(dump_bytes).hexdigest())
    except Exception:
        if created:
            remove_mysql_restore(args, restored_database)
        raise


def remove_mysql_restore(args, database):
    mysql_root(args.mysql_container,
               f"REVOKE ALL PRIVILEGES ON `{database}`.* FROM '{args.mysql_user}'@'%';\n"
               f"DROP DATABASE IF EXISTS `{database}`;\n")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--go-bin", default="go")
    parser.add_argument("--minio-endpoint", default="127.0.0.1:29000")
    parser.add_argument("--access-key", default="gostreamminio")
    parser.add_argument("--secret-key", default="gostream-local-minio-secret")
    parser.add_argument("--mysql-dsn", help="use an isolated MySQL database instead of temporary SQLite")
    parser.add_argument("--mysql-container", default="gostream-transfer-mysql-1")
    parser.add_argument("--mysql-user", default="gostream")
    args = parser.parse_args()
    if args.mysql_dsn:
        require_empty_mysql_database(args)
    with tempfile.TemporaryDirectory(prefix="gostream-transfer-") as temporary:
        root = Path(temporary)
        api_bin, media_bin = root / "gostream", root / "gostream-media"
        subprocess.run([args.go_bin, "build", "-o", str(api_bin), "./cmd/gostream"], check=True)
        subprocess.run([args.go_bin, "build", "-o", str(media_bin), "./cmd/gostream-media"], check=True)
        dsn = args.mysql_dsn or (str(root / "gostream.db") + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1")
        driver = "mysql" if args.mysql_dsn else "sqlite"
        base_env = os.environ.copy()
        base_env.update({
            "DB_DRIVER": driver, "DB_DSN": dsn,
            "JWT_SECRET": "gostream-transfer-disposable-secret-123456",
            "MEDIA_DIR": str(root / "media"),
            "MINIO_ENDPOINT": args.minio_endpoint,
            "MINIO_ACCESS_KEY": args.access_key,
            "MINIO_SECRET_KEY": args.secret_key,
        })
        base_env.pop("FFMPEG_BIN", None)
        suffix = uuid.uuid4().hex[:12]
        local_bucket = "gostream-transfer-smoke-" + suffix + "-source"
        restored_bucket = "gostream-transfer-smoke-" + suffix + "-restored"
        local_port = free_port()
        local_base = f"http://127.0.0.1:{local_port}"
        local_env = dict(base_env, ADDR=f"127.0.0.1:{local_port}", MEDIA_STORE="local")
        with (root / "api.log").open("w") as log:
            process = subprocess.Popen([str(api_bin)], env=local_env, stdout=log, stderr=subprocess.STDOUT)
            try:
                wait_ready(local_base, process)
                account = call(local_base, "/api/users", "POST", body=json.dumps({
                    "username": "transfer_" + suffix,
                    "password": secrets.token_urlsafe(24),
                    "nickname": "Media transfer fixture",
                }).encode())
                token = account["access_token"]
                boundary = "gostream-" + suffix
                payload = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; "
                           f"filename=\"source.mp4\"\r\nContent-Type: video/mp4\r\n\r\n").encode()
                payload += SAMPLE + f"\r\n--{boundary}--\r\n".encode()
                asset = call(local_base, "/api/uploads", "POST", token, payload,
                             f"multipart/form-data; boundary={boundary}")
                video = call(local_base, "/api/videos", "POST", token, json.dumps({
                    "title": "Imported local video", "media_url": asset["url"],
                }).encode())
                if video["status"] != "published" or call(local_base, asset["url"]) != SAMPLE:
                    raise RuntimeError("local media fixture was not published")
                session = call(local_base, "/api/uploads/sessions", "POST", token, json.dumps({
                    "filename": "resumed.mp4", "size": len(RESUMABLE_SAMPLE),
                    "sha256": hashlib.sha256(RESUMABLE_SAMPLE).hexdigest(),
                }).encode())
                first_chunk = RESUMABLE_SAMPLE[:4 << 20]
                chunk = call(local_base, f"/api/uploads/sessions/{session['id']}/chunks/0",
                             "PUT", token, first_chunk, "application/octet-stream")
                if chunk["sha256"] != hashlib.sha256(first_chunk).hexdigest():
                    raise RuntimeError("local resumable chunk hash mismatch")
            finally:
                stop(process)
        tool_env = dict(base_env, MINIO_BUCKET=local_bucket)

        def tool(command, directory, bucket):
            result = subprocess.run([str(media_bin), command, "--dir", str(directory)],
                                    env=dict(tool_env, MINIO_BUCKET=bucket),
                                    capture_output=True, text=True, check=True)
            return json.loads(result.stdout)

        first = tool("import-local", root / "media", local_bucket)
        again = tool("import-local", root / "media", local_bucket)
        backup = tool("backup", root / "backup", local_bucket)
        restored = tool("restore", root / "backup", restored_bucket)
        if (first["copied"], again["existing"], backup["objects"], restored["copied"]) != (2, 2, 2, 2):
            raise RuntimeError(f"unexpected transfer reports: {first}, {again}, {backup}, {restored}")
        restored_dsn, restored_database, sql_bytes, sql_sha256 = dsn, None, 0, None
        if args.mysql_dsn:
            restored_dsn, restored_database, sql_bytes, sql_sha256 = restore_mysql_snapshot(args, root)
        try:
            object_port = free_port()
            object_base = f"http://127.0.0.1:{object_port}"
            object_env = dict(base_env, ADDR=f"127.0.0.1:{object_port}", MEDIA_STORE="minio",
                              DB_DSN=restored_dsn, MEDIA_DIR=str(root / "object-media"), MINIO_BUCKET=restored_bucket)
            with (root / "object-api.log").open("w") as log:
                process = subprocess.Popen([str(api_bin)], env=object_env, stdout=log, stderr=subprocess.STDOUT)
                try:
                    wait_ready(object_base, process)
                    if call(object_base, asset["url"]) != SAMPLE:
                        raise RuntimeError("restored object was not readable through API")
                    feed = call(object_base, "/api/feed?scene=timeline&limit=20")
                    if not any(item["id"] == video["id"] for item in feed["items"]):
                        raise RuntimeError("imported video disappeared from Feed")
                    status = call(object_base, f"/api/uploads/sessions/{session['id']}", token=token)
                    if status["uploaded_chunks"] != [0] or status["status"] != "open":
                        raise RuntimeError(f"resumable session was not restored: {status}")
                    final_chunk = RESUMABLE_SAMPLE[4 << 20:]
                    uploaded = call(object_base, f"/api/uploads/sessions/{session['id']}/chunks/1",
                                    "PUT", token, final_chunk, "application/octet-stream")
                    if uploaded["sha256"] != hashlib.sha256(final_chunk).hexdigest():
                        raise RuntimeError("post-cutover chunk hash mismatch")
                    completed = call(object_base, f"/api/uploads/sessions/{session['id']}/complete",
                                     "POST", token)
                    if call(object_base, completed["url"]) != RESUMABLE_SAMPLE:
                        raise RuntimeError("resumed upload did not produce original bytes")
                finally:
                    stop(process)
        finally:
            if restored_database:
                remove_mysql_restore(args, restored_database)
        print(json.dumps({"video_id": video["id"], "media_url": asset["url"],
                          "resumed_url": completed["url"],
                          "database": driver,
                          "sql_backup_bytes": sql_bytes, "sql_backup_sha256": sql_sha256,
                          "import": first, "import_repeat": again,
                          "backup": backup, "restore": restored}))


if __name__ == "__main__":
    main()
