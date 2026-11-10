#!/usr/bin/env python3
"""Small-fixture SQL/media recovery component drill (not formal Gate A10).

SQLite uses temporary local files. Compose creates its own disposable two-API
AcceptanceStack and restores into a separate database and MinIO bucket. Checks
cover migration keys, fixture users/videos, final media digests, API reads and
resumable uploads. Timings cover backup and restore stages only. Source workers
remain live, so this does not prove a coordinated snapshot, RPO or full RTO.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

import bcrypt

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))
from launcher_lifecycle import LauncherInterrupted, cleanup_on_interrupt, finish_process_start
from acceptance_config import has_external_acceptance_target, open_api_request
from acceptance_stack import AcceptanceStack

SAMPLE_MP4 = bytes([0, 0, 0, 24]) + b"ftypisom" + bytes(4) + b"isommp42"


def find_default_ffmpeg() -> str:
    if "FFMPEG_BIN" in os.environ and shutil.which(os.environ["FFMPEG_BIN"]):
        return os.environ["FFMPEG_BIN"]
    system = shutil.which("ffmpeg")
    if system:
        return system
    candidates = [
        "/home/lzq/.vscode-server/extensions/kilocode.kilo-code-7.8.27-linux-x64/bin/ffmpeg",
        "/home/lzq/.vscode-server/extensions/kilocode.kilo-code-7.8.18-linux-x64/bin/ffmpeg",
        "/usr/bin/ffmpeg",
        "/usr/local/bin/ffmpeg",
    ]
    for c in candidates:
        if os.path.isfile(c) and os.access(c, os.X_OK):
            return c
    return ""


def generate_sample_video(ffmpeg_bin: str = "", title: str = "") -> bytes:
    if not ffmpeg_bin:
        ffmpeg_bin = find_default_ffmpeg()
    if ffmpeg_bin and shutil.which(ffmpeg_bin):
        with tempfile.NamedTemporaryFile(suffix=".mp4", delete=False) as tf:
            tmp_name = tf.name
        try:
            cmd = [
                ffmpeg_bin, "-hide_banner", "-loglevel", "error",
                "-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=25",
                "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
            ]
            if title:
                cmd.extend(["-metadata", f"title={title}"])
            cmd.extend(["-y", tmp_name])
            subprocess.run(cmd, check=True, timeout=15)
            return Path(tmp_name).read_bytes()
        except Exception:
            pass
        finally:
            if os.path.exists(tmp_name):
                os.remove(tmp_name)
    return SAMPLE_MP4 + f"//fallback-payload-{title}".encode()


def find_free_port() -> int:
    import socket
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def call_api(base: str, path: str, method: str = "GET", token: str = "", body: bytes | None = None, content_type: str = "application/json"):
    url = f"{base.rstrip('/')}{path}"
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if body is not None:
        headers["Content-Type"] = content_type
    req = Request(url, data=body, headers=headers, method=method)
    try:
        with open_api_request(req, timeout=30) as resp:
            data = resp.read()
            ct = resp.headers.get("Content-Type", "")
            if "application/json" in ct:
                return resp.status, json.loads(data.decode("utf-8")), resp.headers
            return resp.status, data, resp.headers
    except HTTPError as e:
        data = e.read()
        try:
            return e.code, json.loads(data.decode("utf-8")), e.headers
        except Exception:
            return e.code, data, e.headers
    except URLError as e:
        return 503, {"error": str(e.reason)}, {}


def json_call(base: str, path: str, method: str = "GET", token: str = "", body: dict | None = None, expected: int = 200):
    payload = json.dumps(body).encode("utf-8") if body is not None else None
    status, data, _ = call_api(base, path, method, token, payload, "application/json")
    if status != expected:
        details = data if isinstance(data, (dict, list, str)) else str(data)
        raise RuntimeError(f"{method} {path} expected {expected}, got {status}: {details}")
    return data


def wait_ready(url: str, timeout: float = 30.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            status, data, _ = call_api(url, "/ready")
            if status == 200 and isinstance(data, dict) and data.get("status") == "ready":
                return
        except Exception:
            pass
        time.sleep(0.1)
    raise TimeoutError(f"API at {url} did not become ready within {timeout}s")


def mysql_exec(container: str, sql: str, database: str = "") -> str:
    cmd = [
        "docker", "exec", "-i", container, "sh", "-c",
        'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names' + (' "$1"' if database else ""),
    ]
    if database:
        cmd += ["sh", database]
    res = subprocess.run(cmd, input=sql.encode("utf-8"), capture_output=True, check=True)
    return res.stdout.decode("utf-8").strip()


def find_compose_mysql_container(project_name: str) -> str:
    if not project_name:
        raise RuntimeError("an owned Compose project is required")
    result = subprocess.run([
        "docker", "ps", "-q", "--filter",
        f"label=com.docker.compose.project={project_name}",
        "--filter", "label=com.docker.compose.service=mysql",
    ], capture_output=True, text=True, check=True, timeout=15)
    containers = [line.strip() for line in result.stdout.splitlines() if line.strip()]
    if len(containers) != 1:
        raise RuntimeError("owned Compose project must contain exactly one running MySQL container")
    return containers[0]


def stop_process(process):
    """Reap owned API children even when graceful shutdown times out."""
    previous = {number: signal.signal(number, signal.SIG_IGN)
                for number in (signal.SIGINT, signal.SIGTERM)}
    try:
        process.terminate()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
    finally:
        for number, handler in previous.items():
            signal.signal(number, handler)



def find_api_port_range():
    import socket
    for _ in range(100):
        with socket.socket() as first, socket.socket() as second:
            first.bind(("127.0.0.1", 0))
            port = first.getsockname()[1]
            if port == 65535:
                continue
            try:
                second.bind(("127.0.0.1", port + 1))
            except OSError:
                continue
            return f"127.0.0.1:{port}-{port + 1}"
    raise RuntimeError("could not allocate two adjacent API ports")


class MigrationBackupRecoveryRunner:
    def __init__(
        self,
        mode: str,
        base_url: str,
        db_path: str = "",
        media_dir: str = "",
        project_name: str = "",
        mysql_container: str = "",
        mysql_addr: str = "",
        source_database: str = "gostream",
        minio_endpoint: str = "",
        minio_access_key: str = "gostreamminio",
        minio_secret_key: str = "gostream-local-minio-secret",
        source_bucket: str = "gostream-media",
        owned_stack: AcceptanceStack | None = None,
    ):
        self.owned_stack = owned_stack
        self.mode = mode
        self.base_url = base_url.rstrip("/")
        self.db_path = db_path
        self.media_dir = media_dir
        self.project_name = project_name
        self.mysql_container = mysql_container
        self.mysql_addr = mysql_addr
        self.source_database = source_database
        self.minio_endpoint = minio_endpoint
        self.minio_access_key = minio_access_key
        self.minio_secret_key = minio_secret_key
        self.source_bucket = source_bucket
        self.checkpoints = {}
        self.metrics = {}

    def run_all(self, server_bin: str, go_bin: str, env_template: dict, tmpdir: str, media_bin: str = "") -> dict:
        if self.mode == "compose":
            stack = self.owned_stack
            if (not isinstance(stack, AcceptanceStack) or not stack.started
                    or stack._interrupt_context is None
                    or self.project_name != stack.project_name
                    or self.base_url != stack.api_a
                    or self.mysql_addr != stack.mysql_addr
                    or self.minio_endpoint != stack.minio_endpoint
                    or self.source_database != "gostream"
                    or self.source_bucket != "gostream-media"
                    or self.minio_access_key != stack.env.get("MINIO_ROOT_USER", "gostreamminio")
                    or self.minio_secret_key != stack.env.get("MINIO_ROOT_PASSWORD", "gostream-local-minio-secret")):
                raise RuntimeError("Compose recovery requires a matching, active owned AcceptanceStack")
            if self.mysql_container != find_compose_mysql_container(stack.project_name):
                raise RuntimeError("MySQL container does not belong to the owned stack")
        print("============================================================")
        print("GoStream SQL/Media Recovery Component Drill (not Gate A10)")
        print(f"Mode: {self.mode}, Base URL: {self.base_url}")
        print("============================================================")

        pfx = secrets.token_hex(4)

        # ------------------------------------------------------------------
        # Step 1: Pre-populate comprehensive benchmark dataset
        # ------------------------------------------------------------------
        print("Step 1: Populating comprehensive benchmark dataset...")
        users = []
        user_tokens = []
        password_plain = "UserPassword123!"

        for i in range(5):
            u_resp = json_call(self.base_url, "/api/users", "POST", body={
                "username": f"backup_user_{pfx}_{i:02d}", "password": password_plain, "nickname": f"Backup User {i}"
            }, expected=201)
            users.append(u_resp["user"])
            user_tokens.append(u_resp["access_token"])

        u0_tok = user_tokens[0]
        u1_tok = user_tokens[1]
        u0_id = users[0]["id"]
        u1_id = users[1]["id"]

        # User 0 follows User 1 and User 2
        json_call(self.base_url, f"/api/users/me/following/{u1_id}", "PUT", token=u0_tok, expected=200)
        json_call(self.base_url, f"/api/users/me/following/{users[2]['id']}", "PUT", token=u0_tok, expected=200)

        # Publish 10 videos (5 by User 0, 5 by User 1) with real bytes
        videos = []
        for i in range(10):
            author_tok = u0_tok if i < 5 else u1_tok
            payload = generate_sample_video(title=f"video_{pfx}_{i}")

            # Upload session
            sess = json_call(self.base_url, "/api/uploads/sessions", "POST", token=author_tok, body={
                "filename": f"backup_video_{pfx}_{i}.mp4", "size": len(payload)
            }, expected=201)
            sess_id = sess["id"]

            c_stat, _, _ = call_api(self.base_url, f"/api/uploads/sessions/{sess_id}/chunks/0", "PUT",
                                    token=author_tok, body=payload, content_type="application/octet-stream")
            assert c_stat == 200, f"chunk upload failed: {c_stat}"

            comp = json_call(self.base_url, f"/api/uploads/sessions/{sess_id}/complete", "POST", token=author_tok, expected=200)
            initial_url = comp["url"]

            v_resp = json_call(self.base_url, "/api/videos", "POST", token=author_tok, body={
                "title": f"Backup Dataset Video {pfx} {i}", "media_url": initial_url
            }, expected=201)
            videos.append(v_resp)

        # Poll until all 10 videos are published (important for Compose where FFmpeg transcodes asynchronously)
        print("  Waiting for video publication...")
        media_assets = {}  # url -> sha256
        for v_item in videos:
            vid = v_item["id"]
            author_tok = u0_tok if v_item["author_id"] == u0_id else u1_tok
            deadline = time.monotonic() + 30
            published_url = None
            while time.monotonic() < deadline:
                v_cur = json_call(self.base_url, f"/api/videos/{vid}", "GET", token=author_tok)
                if v_cur.get("status") == "published":
                    published_url = v_cur["media_url"]
                    v_item["media_url"] = published_url
                    break
                if v_cur.get("status") == "failed":
                    raise RuntimeError(f"video {vid} processing failed: {v_cur}")
                time.sleep(0.3)
            if not published_url:
                raise TimeoutError(f"video {vid} did not publish in time")

            # Download asset to compute ground-truth digest for cross-system verification
            stat, asset_data, _ = call_api(self.base_url, published_url)
            assert stat == 200, f"could not download published video {published_url}"
            media_assets[published_url] = hashlib.sha256(asset_data).hexdigest()

        # Interactions: likes & comments on Video 0
        v0_id = videos[0]["id"]
        json_call(self.base_url, f"/api/videos/{v0_id}/like", "PUT", token=u1_tok, expected=200)
        json_call(self.base_url, f"/api/videos/{v0_id}/comments", "POST", token=u1_tok, body={
            "body": "First comment on video 0 for backup drill"
        }, expected=201)

        # Create 1 open upload session (chunk 0 is 4 MiB starting with valid MP4 bytes, chunk 1 is remainder)
        valid_hdr = generate_sample_video(title=f"open_hdr_{pfx}")
        chunk0_data = valid_hdr + bytes((4 << 20) - len(valid_hdr))
        chunk1_data = b"open-session-final-part-data-" + pfx.encode()
        total_open_size = len(chunk0_data) + len(chunk1_data)
        open_sess = json_call(self.base_url, "/api/uploads/sessions", "POST", token=u0_tok, body={
            "filename": f"open_session_{pfx}.mp4", "size": total_open_size
        }, expected=201)
        open_sess_id = open_sess["id"]
        c0_stat, _, _ = call_api(self.base_url, f"/api/uploads/sessions/{open_sess_id}/chunks/0", "PUT",
                                 token=u0_tok, body=chunk0_data, content_type="application/octet-stream")
        assert c0_stat == 200, f"open session chunk 0 upload failed: {c0_stat}"

        # Expected identity sets
        expected_user_ids = {u["id"] for u in users}
        expected_video_ids = {v["id"] for v in videos}

        print(f"  Pre-backup dataset: {len(users)} users, {len(videos)} videos, {len(media_assets)} media assets, 1 open session.")

        # ------------------------------------------------------------------
        # Step 2: Quiescent Snapshot Boundary (停写与快照)
        # ------------------------------------------------------------------
        print("Step 2: Taking component SQL/media backups (no coordinated stop-write boundary)...")
        backup_start = time.monotonic()

        backup_dir = os.path.join(tmpdir, "backup_snapshot")
        os.makedirs(backup_dir, exist_ok=True)
        sql_backup_path = os.path.join(backup_dir, "database.sql")
        media_backup_dir = os.path.join(backup_dir, "media_store")

        if self.mode == "sqlite":
            os.makedirs(media_backup_dir, exist_ok=True)
            # Force WAL checkpoint before copy to ensure quiescent consistency
            conn = sqlite3.connect(self.db_path)
            conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            conn.close()
            shutil.copy2(self.db_path, sql_backup_path)

            shutil.copytree(self.media_dir, os.path.join(media_backup_dir, "media"), dirs_exist_ok=True)
            chunks_source = f"{self.media_dir}.chunks"
            if os.path.exists(chunks_source):
                shutil.copytree(chunks_source, os.path.join(media_backup_dir, "media.chunks"), dirs_exist_ok=True)
            total_media_bytes = sum(f.stat().st_size for f in Path(media_backup_dir).glob("**/*") if f.is_file())
        else:
            # mysqldump single transaction from MySQL container
            cmd = [
                "docker", "exec", "-i", self.mysql_container, "sh", "-c",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --single-transaction --routines --triggers --no-tablespaces --set-gtid-purged=OFF "$1"',
                "sh", self.source_database,
            ]
            with open(sql_backup_path, "wb") as f:
                subprocess.run(cmd, stdout=f, check=True)

            # MinIO object backup using gostream-media backup tool
            media_tool_env = os.environ.copy()
            media_tool_env.update({
                "MINIO_ENDPOINT": self.minio_endpoint,
                "MINIO_ACCESS_KEY": self.minio_access_key,
                "MINIO_SECRET_KEY": self.minio_secret_key,
                "MINIO_BUCKET": self.source_bucket,
            })
            res = subprocess.run([media_bin, "backup", "--dir", media_backup_dir],
                                 env=media_tool_env, capture_output=True, text=True, check=True)
            media_report = json.loads(res.stdout)
            total_media_bytes = media_report.get("bytes", 0)

        sql_backup_bytes = os.path.getsize(sql_backup_path)
        sql_backup_sha256 = hashlib.sha256(Path(sql_backup_path).read_bytes()).hexdigest()
        backup_duration_ms = round((time.monotonic() - backup_start) * 1000, 2)
        print(f"  Backup complete in {backup_duration_ms}ms: SQL dump={sql_backup_bytes} bytes, Media={total_media_bytes} bytes.")

        # ------------------------------------------------------------------
        # Step 3: Disaster Restore into Clean Environment
        # ------------------------------------------------------------------
        print("Step 3: Restoring snapshot into clean destination...")
        restore_start = time.monotonic()

        restore_root = os.path.join(tmpdir, "restored_instance")
        os.makedirs(restore_root, exist_ok=True)
        restored_database = None
        restored_bucket = None

        if self.mode == "sqlite":
            restored_db_path = os.path.join(restore_root, "restored.db")
            restored_media_dir = os.path.join(restore_root, "restored_media")
            os.makedirs(restored_media_dir, exist_ok=True)

            shutil.copy2(sql_backup_path, restored_db_path)
            shutil.copytree(os.path.join(media_backup_dir, "media"), restored_media_dir, dirs_exist_ok=True)
            restored_chunks_dir = f"{restored_media_dir}.chunks"
            if os.path.exists(os.path.join(media_backup_dir, "media.chunks")):
                shutil.copytree(os.path.join(media_backup_dir, "media.chunks"), restored_chunks_dir, dirs_exist_ok=True)
        else:
            restored_database = f"gostream_restore_{pfx}"
            restored_bucket = f"gostream-restore-{pfx}"
            restored_media_dir = os.path.join(restore_root, "restored_object_media")
            os.makedirs(restored_media_dir, exist_ok=True)

            # Create clean MySQL database and grant privileges
            mysql_exec(self.mysql_container,
                       f"CREATE DATABASE `{restored_database}`;\n"
                       f"GRANT ALL PRIVILEGES ON `{restored_database}`.* TO 'gostream'@'%';\n")

            # Restore SQL dump into restored_database
            with open(sql_backup_path, "rb") as dump_f:
                subprocess.run([
                    "docker", "exec", "-i", self.mysql_container, "sh", "-c",
                    'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot "$1"',
                    "sh", restored_database,
                ], stdin=dump_f, check=True)

            # Restore MinIO objects into restored_bucket
            media_tool_env = os.environ.copy()
            media_tool_env.update({
                "MINIO_ENDPOINT": self.minio_endpoint,
                "MINIO_ACCESS_KEY": self.minio_access_key,
                "MINIO_SECRET_KEY": self.minio_secret_key,
                "MINIO_BUCKET": restored_bucket,
            })
            subprocess.run([media_bin, "restore", "--dir", media_backup_dir],
                           env=media_tool_env, capture_output=True, text=True, check=True)

        restored_port = find_free_port()
        restored_url = f"http://127.0.0.1:{restored_port}"
        restored_env = env_template.copy()
        if self.mode == "sqlite":
            restored_env.update({
                "ADDR": f"127.0.0.1:{restored_port}",
                "DB_DRIVER": "sqlite",
                "DB_DSN": f"{restored_db_path}?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1",
                "MEDIA_DIR": restored_media_dir,
                "MEDIA_STORE": "local",
            })
        else:
            restored_env.update({
                "ADDR": f"127.0.0.1:{restored_port}",
                "DB_DRIVER": "mysql",
                "DB_DSN": f"gostream:gostream-local@tcp({self.mysql_addr})/{restored_database}?charset=utf8mb4&parseTime=True&loc=UTC",
                "MEDIA_STORE": "minio",
                "MINIO_ENDPOINT": self.minio_endpoint,
                "MINIO_ACCESS_KEY": self.minio_access_key,
                "MINIO_SECRET_KEY": self.minio_secret_key,
                "MINIO_BUCKET": restored_bucket,
                "MEDIA_DIR": restored_media_dir,
                "JWT_SECRET": env_template.get("JWT_SECRET", "backup-recovery-jwt-secret-at-least-32-bytes!!"),
                "REDIS_ADDR": "",
                "RABBITMQ_URL": "",
                "RABBITMQ_URLS": "",
                "FFMPEG_BIN": "",
                "FFPROBE_BIN": "",
                "PPROF_ADDR": "",
            })

        p_restored = None
        try:
            with finish_process_start():
                p_restored = subprocess.Popen([server_bin], env=restored_env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            wait_ready(restored_url, timeout=30)
            restore_duration_ms = round((time.monotonic() - restore_start) * 1000, 2)
            print(f"  Restore complete in {restore_duration_ms}ms (restore stage through API /ready; not an end-to-end recovery objective).")

            # ------------------------------------------------------------------
            # Checkpoint 1: Schema Migration Keys
            # ------------------------------------------------------------------
            print("Testing Checkpoint 1: Schema Migration Keys...")
            if self.mode == "sqlite":
                conn_res = sqlite3.connect(restored_db_path)
                cur = conn_res.cursor()
                cur.execute("SELECT `key` FROM schema_migrations ORDER BY applied_at ASC")
                mig_keys = [r[0] for r in cur.fetchall()]
                conn_res.close()
            else:
                raw_keys = mysql_exec(self.mysql_container, "SELECT `key` FROM schema_migrations ORDER BY applied_at ASC;", database=restored_database)
                mig_keys = [k.strip() for k in raw_keys.splitlines() if k.strip()]

            expected_mig_keys = ["schema_v001", "schema_v002", "schema_v003", "schema_v004", "schema_v005"]
            for k in expected_mig_keys:
                assert k in mig_keys, f"expected migration key {k} missing from restored database: {mig_keys}"
            self.checkpoints["schema_migration_keys"] = True
            print(f"  schema_migration_keys: PASS (all 5 keys present: {expected_mig_keys})")

            # ------------------------------------------------------------------
            # Checkpoint 2: SQL Identity Sets Preserved
            # ------------------------------------------------------------------
            print("Testing Checkpoint 2: SQL Identity Sets Preservation...")
            if self.mode == "sqlite":
                conn_res = sqlite3.connect(restored_db_path)
                cur = conn_res.cursor()
                cur.execute("SELECT id FROM users")
                restored_user_ids = {r[0] for r in cur.fetchall()}
                cur.execute("SELECT id FROM videos")
                restored_video_ids = {r[0] for r in cur.fetchall()}
                cur.execute(f"SELECT like_count, comment_count FROM videos WHERE id = {v0_id}")
                v0_counts = cur.fetchone()
                cur.execute(f"SELECT status FROM upload_sessions WHERE id = '{open_sess_id}'")
                open_sess_status = cur.fetchone()[0]
                cur.execute(f"SELECT count(*) FROM upload_chunks WHERE session_id = '{open_sess_id}'")
                open_chunks_count = cur.fetchone()[0]
                conn_res.close()
            else:
                u_out = mysql_exec(self.mysql_container, "SELECT id FROM users;", database=restored_database)
                restored_user_ids = {int(x) for x in u_out.splitlines() if x.strip()}
                v_out = mysql_exec(self.mysql_container, "SELECT id FROM videos;", database=restored_database)
                restored_video_ids = {int(x) for x in v_out.splitlines() if x.strip()}
                c_out = mysql_exec(self.mysql_container, f"SELECT like_count, comment_count FROM videos WHERE id = {v0_id};", database=restored_database)
                parts = c_out.split()
                v0_counts = (int(parts[0]), int(parts[1]))
                open_sess_status = mysql_exec(self.mysql_container, f"SELECT status FROM upload_sessions WHERE id = '{open_sess_id}';", database=restored_database)
                open_chunks_count = int(mysql_exec(self.mysql_container, f"SELECT count(*) FROM upload_chunks WHERE session_id = '{open_sess_id}';", database=restored_database))

            assert expected_user_ids.issubset(restored_user_ids), f"user IDs mismatch: {expected_user_ids - restored_user_ids}"
            assert expected_video_ids.issubset(restored_video_ids), f"video IDs mismatch: {expected_video_ids - restored_video_ids}"
            assert v0_counts == (1, 1), f"video 0 interaction counts mismatch: {v0_counts}"
            assert open_sess_status == "open", f"open session status altered: {open_sess_status}"
            assert open_chunks_count == 1, f"open session chunk count mismatch: {open_chunks_count}"

            self.checkpoints["sql_identity_sets_preserved"] = True
            print(f"  sql_identity_sets_preserved: PASS ({len(users)} users, {len(videos)} videos, 1 open session)")

            # ------------------------------------------------------------------
            # Checkpoint 3: Media Objects Integrity
            # ------------------------------------------------------------------
            print("Testing Checkpoint 3: Media Objects Integrity (SHA-256)...")
            for url_path, expected_digest in media_assets.items():
                if self.mode == "sqlite":
                    rel_name = os.path.basename(url_path)
                    file_path = os.path.join(restored_media_dir, rel_name)
                    assert os.path.exists(file_path), f"media asset file missing from restored storage: {file_path}"
                    actual_digest = hashlib.sha256(Path(file_path).read_bytes()).hexdigest()
                else:
                    stat, data, _ = call_api(restored_url, url_path)
                    assert stat == 200, f"media asset {url_path} not readable from restored API: {stat}"
                    actual_digest = hashlib.sha256(data).hexdigest()
                assert actual_digest == expected_digest, f"SHA-256 digest mismatch for {url_path}: {actual_digest} != {expected_digest}"
            self.checkpoints["media_objects_integrity"] = True
            print(f"  media_objects_integrity: PASS ({len(media_assets)} assets verified with zero byte drift)")

            # ------------------------------------------------------------------
            # Checkpoint 4: Post-Restore Live API Health & Reading
            # ------------------------------------------------------------------
            print("Testing Checkpoint 4: Post-Restore Live API Health & Reads...")
            # Liveness & Readiness
            h_stat, h_body, _ = call_api(restored_url, "/health")
            assert h_stat == 200 and h_body.get("status") == "ok"
            r_stat, r_body, _ = call_api(restored_url, "/ready")
            assert r_stat == 200 and r_body.get("status") == "ready"

            # Login with pre-backup password
            login_resp = json_call(restored_url, "/api/sessions", "POST", body={
                "username": users[0]["username"], "password": password_plain
            }, expected=200)
            live_token = login_resp["access_token"]
            assert login_resp["user"]["id"] == u0_id

            # Read Feed Timeline
            feed_resp = json_call(restored_url, "/api/feed?scene=timeline&limit=20", "GET", expected=200)
            feed_items = feed_resp.get("items", [])
            assert len(feed_items) >= 10, f"expected at least 10 videos in restored feed, got {len(feed_items)}"

            # Read Video Details & Interactions
            v0_detail = json_call(restored_url, f"/api/videos/{v0_id}", "GET", expected=200)
            assert v0_detail["like_count"] == 1 and v0_detail["comment_count"] == 1

            # Read Notifications
            notifs = json_call(restored_url, "/api/notifications", "GET", token=live_token, expected=200)
            assert "items" in notifs

            self.checkpoints["post_restore_live_api_health"] = True
            print("  post_restore_live_api_health: PASS (login, feed timeline, video counts, notifications all intact)")

            # ------------------------------------------------------------------
            # Checkpoint 5: Resumable Upload Completion After Restore
            # ------------------------------------------------------------------
            print("Testing Checkpoint 5: Resuming Open Upload Session Post-Restore...")
            # Upload remaining chunk 1 on the restored API
            c1_stat, c1_res, _ = call_api(restored_url, f"/api/uploads/sessions/{open_sess_id}/chunks/1", "PUT",
                                          token=live_token, body=chunk1_data, content_type="application/octet-stream")
            assert c1_stat == 200, f"failed to upload chunk 1 post-restore: {c1_stat} {c1_res}"

            # Complete upload session
            comp_res = json_call(restored_url, f"/api/uploads/sessions/{open_sess_id}/complete", "POST", token=live_token, expected=200)
            assert "url" in comp_res, f"complete did not return url: {comp_res}"

            # Verify downloaded asset matches combined chunks
            asset_stat, asset_data, _ = call_api(restored_url, comp_res["url"])
            assert asset_stat == 200, f"failed to download completed asset: {asset_stat}"
            assert asset_data == (chunk0_data + chunk1_data), "restored resumable upload bytes do not match original"

            self.checkpoints["resumable_upload_completion_after_restore"] = True
            print("  resumable_upload_completion_after_restore: PASS (session completed post-restore with exact bytes)")

        finally:
            if p_restored is not None:
                stop_process(p_restored)
            if self.mode == "compose" and restored_database:
                try:
                    mysql_exec(self.mysql_container,
                               f"REVOKE ALL PRIVILEGES ON `{restored_database}`.* FROM 'gostream'@'%';\n"
                               f"DROP DATABASE IF EXISTS `{restored_database}`;\n")
                except Exception as e:
                    print(f"Warning: failed to drop restored DB {restored_database}: {e}", file=sys.stderr)

        # ------------------------------------------------------------------
        # Checkpoint 6: Idempotency & Migration Replay Safety
        # ------------------------------------------------------------------
        print("Testing Checkpoint 6: Migration Replay Safety on Restored Database...")
        if self.mode == "compose":
            # Recreate restored DB from backup to test startup Apply idempotency
            replay_db = f"gostream_replay_{pfx}"
            mysql_exec(self.mysql_container,
                       f"CREATE DATABASE `{replay_db}`;\n"
                       f"GRANT ALL PRIVILEGES ON `{replay_db}`.* TO 'gostream'@'%';\n")
            with open(sql_backup_path, "rb") as dump_f:
                subprocess.run([
                    "docker", "exec", "-i", self.mysql_container, "sh", "-c",
                    'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot "$1"',
                    "sh", replay_db,
                ], stdin=dump_f, check=True)

            replay_port = find_free_port()
            replay_url = f"http://127.0.0.1:{replay_port}"
            replay_env = restored_env.copy()
            replay_env.update({
                "ADDR": f"127.0.0.1:{replay_port}",
                "DB_DSN": f"gostream:gostream-local@tcp({self.mysql_addr})/{replay_db}?charset=utf8mb4&parseTime=True&loc=UTC",
            })
            p_replay = None
            try:
                with finish_process_start():
                    p_replay = subprocess.Popen([server_bin], env=replay_env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                wait_ready(replay_url, timeout=15)
            finally:
                if p_replay is not None:
                    stop_process(p_replay)

            count_after_replay = int(mysql_exec(self.mysql_container, "SELECT count(*) FROM schema_migrations;", database=replay_db))
            mysql_exec(self.mysql_container,
                       f"REVOKE ALL PRIVILEGES ON `{replay_db}`.* FROM 'gostream'@'%';\n"
                       f"DROP DATABASE IF EXISTS `{replay_db}`;\n")
        else:
            replay_port = find_free_port()
            replay_url = f"http://127.0.0.1:{replay_port}"
            replay_env = restored_env.copy()
            replay_env["ADDR"] = f"127.0.0.1:{replay_port}"

            p_replay = None
            try:
                with finish_process_start():
                    p_replay = subprocess.Popen([server_bin], env=replay_env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                wait_ready(replay_url, timeout=15)
            finally:
                if p_replay is not None:
                    stop_process(p_replay)

            conn_res = sqlite3.connect(restored_db_path)
            cur = conn_res.cursor()
            cur.execute("SELECT count(*) FROM schema_migrations")
            count_after_replay = cur.fetchone()[0]
            conn_res.close()

        assert count_after_replay == len(mig_keys), f"migration count altered after replay: {count_after_replay} != {len(mig_keys)}"
        self.checkpoints["idempotency_and_migration_replay_safety"] = True
        print("  idempotency_and_migration_replay_safety: PASS (Apply on restored DB performed 0 changes)")

        all_passed = all(self.checkpoints.values())
        print(f"\n{self.mode.upper()} recovery verification: {'PASS' if all_passed else 'FAIL'}; release gate: NOT_ACCEPTED")

        self.metrics = {
            "backup_duration_ms": backup_duration_ms,
            "restore_duration_ms": restore_duration_ms,
            "sql_backup_bytes": sql_backup_bytes,
            "media_objects_bytes": total_media_bytes,
            "media_assets_count": len(media_assets),
            "users_count": len(users),
            "videos_count": len(videos),
        }

        return {
            "status": "PASS" if all_passed else "FAIL",
            "mode": self.mode,
            "validation_scope": "compose_component_smoke" if self.mode == "compose" else "sqlite_component_smoke",
            "release_gate_passed": False,
            "checkpoints": self.checkpoints,
            "metrics": self.metrics,
            "rpo_rto_spec": {
                "measured_rpo": None,
                "measured_rto_ms": None,
                "file_restore_stage_ms": restore_duration_ms,
                "scope": "Small-fixture restore stage through API readiness only; source workers remain live; no coordinated snapshot or end-to-end RPO/RTO measured",
                "online_backup_limitation": "Online backup without stop-write/coordinated snapshot is unsupported; quiescent boundary is mandatory for cross-system consistency."
            }
        }


@cleanup_on_interrupt()
def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=["auto", "sqlite", "compose"], default="auto")
    parser.add_argument("--report", "--report-file", dest="report_file", type=Path,
                        default=ROOT / "data/diagnostics/migration_backup_recovery_smoke.json")
    parser.add_argument("--go-bin", default=os.environ.get("GOSTREAM_GO_BIN") or shutil.which("go") or "/usr/local/go/bin/go")
    parser.add_argument("--compose-e2e", action="store_true",
                        help="Run component recovery on an owned disposable Compose MySQL/MinIO stack")
    args = parser.parse_args()

    report_path = args.report_file.resolve()
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.unlink(missing_ok=True)

    if has_external_acceptance_target() or any(os.environ.get(key) for key in (
            "GOSTREAM_STACK_PROJECT", "GOSTREAM_MYSQL_ADDR", "GOSTREAM_MINIO_ENDPOINT")):
        raise ValueError("recovery drill refuses external acceptance targets; it creates its own disposable stack")
    mode = "compose" if args.mode == "compose" or args.compose_e2e else "sqlite"
    if mode == "compose" and not args.compose_e2e:
        raise ValueError("Compose component drill requires --compose-e2e")

    if mode == "sqlite":
        with tempfile.TemporaryDirectory(prefix="gostream_backup_drill_") as tmpdir:
            db_path = os.path.join(tmpdir, "primary.db")
            media_dir = os.path.join(tmpdir, "primary_media")
            os.makedirs(media_dir, exist_ok=True)

            server_bin = os.path.join(tmpdir, "gostream_server")
            print("Building server binary...")
            subprocess.run([args.go_bin, "build", "-o", server_bin, "./cmd/gostream"], cwd=ROOT, check=True)

            port = find_free_port()
            base_url = f"http://127.0.0.1:{port}"
            env = os.environ.copy()
            env.update({
                "ADDR": f"127.0.0.1:{port}",
                "DB_DRIVER": "sqlite",
                "DB_DSN": f"{db_path}?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1",
                "MEDIA_DIR": media_dir,
                "MEDIA_STORE": "local",
                "JWT_SECRET": "backup-recovery-jwt-secret-at-least-32-bytes!!",
                "REDIS_ADDR": "",
                "RABBITMQ_URL": "",
                "RABBITMQ_URLS": "",
                "FFMPEG_BIN": "",
                "FFPROBE_BIN": "",
                "PPROF_ADDR": "",
            })

            p = None
            try:
                with finish_process_start():
                    p = subprocess.Popen([server_bin], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                wait_ready(base_url, timeout=15)
                runner = MigrationBackupRecoveryRunner(mode="sqlite", base_url=base_url, db_path=db_path, media_dir=media_dir)
                report = runner.run_all(server_bin=server_bin, go_bin=args.go_bin, env_template=env, tmpdir=tmpdir)
                report_path.write_text(json.dumps(report, indent=2))
                print(f"Report written to: {report_path}")
            finally:
                if p is not None:
                    stop_process(p)
    else:
        with AcceptanceStack(
            api_ports=find_api_port_range(),
            mysql_port=f"127.0.0.1:{find_free_port()}",
            minio_api_port=f"127.0.0.1:{find_free_port()}",
            minio_console_port=f"127.0.0.1:{find_free_port()}",
            go_bin=args.go_bin,
        ) as stack, tempfile.TemporaryDirectory(prefix="gostream_backup_compose_") as tmpdir:
            mysql_container = find_compose_mysql_container(stack.project_name)
            server_bin = os.path.join(tmpdir, "gostream_server")
            media_bin = os.path.join(tmpdir, "gostream_media")
            print("Building server and media binaries for isolated Compose component recovery...")
            subprocess.run([args.go_bin, "build", "-o", server_bin, "./cmd/gostream"], cwd=ROOT, check=True)
            subprocess.run([args.go_bin, "build", "-o", media_bin, "./cmd/gostream-media"], cwd=ROOT, check=True)
            env = stack.env.copy()
            minio_access = env.get("MINIO_ROOT_USER", "gostreamminio")
            minio_secret = env.get("MINIO_ROOT_PASSWORD", "gostream-local-minio-secret")
            runner = MigrationBackupRecoveryRunner(
                mode="compose", base_url=stack.api_a,
                project_name=stack.project_name, mysql_container=mysql_container,
                mysql_addr=stack.mysql_addr, minio_endpoint=stack.minio_endpoint,
                minio_access_key=minio_access, minio_secret_key=minio_secret,
                owned_stack=stack,
            )
            report = runner.run_all(server_bin=server_bin, go_bin=args.go_bin, env_template=env, tmpdir=tmpdir, media_bin=media_bin)
        # The context verifies containers, volumes and networks are gone before success.
        report["owned_stack_cleanup_verified"] = True
        report_path.write_text(json.dumps(report, indent=2))
        print(f"Report written to: {report_path}")


if __name__ == "__main__":
    try:
        main()
    except LauncherInterrupted as e:
        print(f"Runner interrupted: {e}", file=sys.stderr)
        sys.exit(128 + int(e.args[0]))
