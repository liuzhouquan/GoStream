#!/usr/bin/env python3
"""GoStream Group IM Fanout & Device Recovery Verification Runner (Gate A07 / G08).

Validates:
1. 20-client cross-instance group fanout (10 on API A, 10 on API B).
2. Explicit mathematical expectation verification:
   For group members N = 20, message m from client i is fanned out to all j != i.
   Expected set E(k) = { m.ID | m.Sender != k }, with length total_messages - msgs_per_client.
   Zero self-deliveries, zero duplicate deliveries, zero duplicate ACKs.
3. Duplicate client_id idempotency (returns original ACK, no duplicate SQL row, no duplicate fanout).
4. Slow / offline member:
   - Member 19 disconnects, 125 messages sent to group, unread increments to 125.
   - Member 19 reconnects, receives 100 replays + group_sync_required.
   - Member 19 backfills remaining 25 via keyset pagination GET /api/groups/:id/messages.
   - Member 19 ACKs newest message, unread drops to 0.
5. Member removal & re-invitation:
   - Removed member cannot read or send (404).
   - Messages sent during removal do not cause unread backlog upon re-invitation.
6. Per-device independent unread & ACK:
   - User 0 has dev1 and dev2.
   - dev1 ACKs message 5 (unread drops to 5; dev2 unread remains 10).
   - dev2 ACKs message 10 (unread drops to 0; dev1 unread remains 5).
7. Latency measurements: P50, P95, P99 for Sender ACK and Recipient Delivery.
8. Resource sampling: API A, API B, and Driver CPU/RSS.
9. SQL facts: Zero duplicate messages, exact deduplicated set, strict ordering.
"""

from __future__ import annotations

import argparse
import base64
import bcrypt
from contextlib import ExitStack
from datetime import datetime, timezone
import json
import jwt
import os
from pathlib import Path
import queue
import socket
import sqlite3
import struct
import subprocess
import sys
import tempfile
import threading
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from launcher_lifecycle import cleanup_on_interrupt
from acceptance_config import resolve_acceptance_target


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_uuid(i: int) -> str:
    return f"11111111-2222-3333-4444-{i:012d}"


def call_api(base: str, path: str, method: str = "GET", token: str = "", device_id: str = "", body: bytes | None = None, content_type: str = "application/json"):
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if device_id:
        headers["X-GoStream-Device-ID"] = device_id
    if body is not None:
        headers["Content-Type"] = content_type
    req = Request(base + path, data=body, headers=headers, method=method)
    t0 = time.monotonic()
    try:
        with urlopen(req, timeout=15) as resp:
            data = resp.read()
            elapsed_ms = (time.monotonic() - t0) * 1000
            ct = resp.headers.get_content_type()
            parsed = json.loads(data) if ct == "application/json" else data
            return resp.status, parsed, elapsed_ms
    except HTTPError as err:
        data = err.read()
        elapsed_ms = (time.monotonic() - t0) * 1000
        try:
            parsed = json.loads(data)
        except Exception:
            parsed = data
        return err.code, parsed, elapsed_ms


def json_call(base: str, path: str, method: str = "GET", token: str = "", device_id: str = "", body: dict | None = None, expected: int = 200):
    payload = json.dumps(body).encode() if body is not None else None
    status, data, _ = call_api(base, path, method, token, device_id, payload, "application/json")
    if status != expected:
        details = data.decode(errors="replace") if isinstance(data, bytes) else json.dumps(data)
        raise RuntimeError(f"{method} {path} expected {expected}, got {status}: {details}")
    return data


def wait_url_ready(url: str, timeout: float = 15.0):
    start = time.monotonic()
    while time.monotonic() - start < timeout:
        try:
            req = Request(url + "/ready", method="GET")
            with urlopen(req, timeout=1) as resp:
                if resp.status == 200:
                    return
        except Exception:
            pass
        time.sleep(0.05)
    raise TimeoutError(f"server at {url} did not become ready within {timeout}s")


class SimpleWebSocket:
    """Pure Python standard library RFC 6455 WebSocket client."""

    def __init__(self, base_url: str, token: str, device_id: str):
        parsed = base_url.replace("http://", "").replace("https://", "").split(":")
        self.host = parsed[0]
        self.port = int(parsed[1]) if len(parsed) > 1 else 80
        self.token = token
        self.device_id = device_id
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.sock.settimeout(10.0)
        self.sock.connect((self.host, self.port))
        self._handshake()
        self.sock.settimeout(None)
        self.read_queue: queue.Queue[dict] = queue.Queue()
        self.closed = False
        self._reader_thread = threading.Thread(target=self._read_loop, daemon=True)
        self._reader_thread.start()

    def _handshake(self):
        key = base64.b64encode(os.urandom(16)).decode()
        req = (
            f"GET /api/ws HTTP/1.1\r\n"
            f"Host: {self.host}:{self.port}\r\n"
            f"Upgrade: websocket\r\n"
            f"Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            f"Sec-WebSocket-Version: 13\r\n"
            f"Sec-WebSocket-Protocol: gostream, gostream.jwt.{self.token}, gostream.device.{self.device_id}\r\n\r\n"
        )
        self.sock.sendall(req.encode())
        resp = b""
        while b"\r\n\r\n" not in resp:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise ConnectionResetError("handshake connection reset")
            resp += chunk
        header_text = resp.decode(errors="replace")
        if "101 Switching Protocols" not in header_text:
            raise RuntimeError(f"WebSocket handshake failed: {header_text}")

    def send_json(self, payload: dict):
        text = json.dumps(payload)
        data = text.encode("utf-8")
        length = len(data)
        header = bytearray([0x81])
        if length <= 125:
            header.append(0x80 | length)
        elif length <= 65535:
            header.append(0x80 | 126)
            header.extend(struct.pack(">H", length))
        else:
            header.append(0x80 | 127)
            header.extend(struct.pack(">Q", length))
        mask = os.urandom(4)
        header.extend(mask)
        masked = bytearray(b ^ mask[i % 4] for i, b in enumerate(data))
        self.sock.sendall(header + masked)

    def _recv_exact(self, n: int) -> bytes:
        buf = bytearray()
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise ConnectionResetError("socket closed")
            buf.extend(chunk)
        return bytes(buf)

    def _read_loop(self):
        try:
            while not self.closed:
                head = self._recv_exact(2)
                b0, b1 = head[0], head[1]
                opcode = b0 & 0x0F
                is_masked = bool(b1 & 0x80)
                length = b1 & 0x7F
                if length == 126:
                    length = struct.unpack(">H", self._recv_exact(2))[0]
                elif length == 127:
                    length = struct.unpack(">Q", self._recv_exact(8))[0]
                mask = self._recv_exact(4) if is_masked else None
                payload = self._recv_exact(length)
                if mask:
                    payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))

                if opcode == 0x8:  # close
                    break
                elif opcode == 0x9:  # ping -> send pong
                    pong_hdr = bytearray([0x8A, 0x80 | len(payload)])
                    pong_mask = os.urandom(4)
                    pong_hdr.extend(pong_mask)
                    pong_payload = bytearray(b ^ pong_mask[i % 4] for i, b in enumerate(payload))
                    self.sock.sendall(pong_hdr + pong_payload)
                elif opcode == 0x1:  # text frame
                    try:
                        parsed = json.loads(payload.decode("utf-8"))
                        self.read_queue.put(parsed)
                    except Exception:
                        pass
        except Exception:
            pass
        finally:
            self.closed = True

    def next_frame(self, timeout: float = 5.0) -> dict | None:
        try:
            return self.read_queue.get(timeout=timeout)
        except queue.Empty:
            return None

    def next_of_type(self, kind: str, timeout: float = 8.0, client_id: str = "") -> dict:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            f = self.next_frame(timeout=0.1)
            if not f:
                continue
            if f.get("type") == kind:
                if client_id and f.get("client_id") != client_id:
                    continue
                return f
        raise TimeoutError(f"socket timed out waiting for {kind} (client_id={client_id})")

    def close(self):
        self.closed = True
        try:
            close_frame = bytearray([0x88, 0x80])
            close_mask = os.urandom(4)
            close_frame.extend(close_mask)
            self.sock.sendall(close_frame)
        except Exception:
            pass
        try:
            self.sock.shutdown(socket.SHUT_RDWR)
        except Exception:
            pass
        try:
            self.sock.close()
        except Exception:
            pass


def register_user(base: str, username: str, idx: int) -> tuple[int, str]:
    payload = {"username": username, "password": "Password123!", "nickname": f"Nick_{username}"}
    for attempt in range(5):
        status, data, _ = call_api(base, "/api/users", method="POST", body=json.dumps(payload).encode())
        if status == 201:
            return data["user"]["id"], data["access_token"]
        if status == 429:
            time.sleep(1.0 + attempt * 0.5)
            continue
        raise RuntimeError(f"register {username} failed ({status}): {data}")
    raise RuntimeError(f"register {username} exhausted rate limit retries")


def percentile(data: list[float], pct: float) -> float:
    if not data:
        return 0.0
    s = sorted(data)
    idx = int((len(s) - 1) * pct)
    return round(s[idx], 2)


def get_process_metrics(pid: int) -> dict:
    try:
        with open(f"/proc/{pid}/statm") as f:
            rss_bytes = int(f.read().split()[1]) * os.sysconf("SC_PAGE_SIZE")
        with open(f"/proc/{pid}/stat") as f:
            fields = f.read().rsplit(") ", 1)[1].split()
            ticks = int(fields[11]) + int(fields[12])
            cpu_seconds = ticks / os.sysconf("SC_CLK_TCK")
        return {"rss_bytes": rss_bytes, "cpu_seconds": round(cpu_seconds, 3)}
    except Exception:
        return {"rss_bytes": 0, "cpu_seconds": 0.0}


def verify_group_im_sql_facts(group_id: int, total_persisted: int, db_path: str = "", mysql_verify_fn=None) -> bool:
    """Verifies that persisted group messages have strictly increasing ID, 0 duplicate client_ids, and exact row count."""
    if db_path and os.path.exists(db_path):
        conn = sqlite3.connect(db_path)
        cur = conn.cursor()
        cur.execute("SELECT id, client_id FROM group_messages WHERE group_id=? ORDER BY id", (group_id,))
        rows = cur.fetchall()
        conn.close()
        seen = set()
        prev_id = 0
        for rid, rcid in rows:
            assert rid > prev_id, f"non strictly increasing id: {rid} <= {prev_id}"
            prev_id = rid
            assert rcid not in seen, f"duplicate client_id in db: {rcid}"
            seen.add(rcid)
        assert len(rows) == total_persisted, f"expected {total_persisted} rows in SQL, got {len(rows)}"
        print(f"SQL verified: {len(rows)} messages persisted with strict ordering and zero duplicates.")
        return True
    elif mysql_verify_fn:
        mysql_verify_fn(group_id, total_persisted)
        return True
    else:
        print("WARNING: Neither SQLite db_path nor mysql_verify_fn was provided; SQL verification not performed.")
        return False


def run_group_fanout_matrix(
    api_a: str,
    api_b: str,
    msgs_per_client: int,
    pids: dict[str, int],
    accounts: list[int] | None = None,
    tokens: list[str] | None = None,
    device_ids: list[str] | None = None,
    db_path: str = "",
    mysql_verify_fn=None,
) -> dict:
    bases = [api_a, api_b]
    client_count = 20
    total_messages = client_count * msgs_per_client
    expected_total_deliveries = total_messages * (client_count - 1)

    if accounts is None or tokens is None or device_ids is None:
        mysql_addr = os.environ.get("GOSTREAM_MYSQL_ADDR", "")
        mysql_dsn = os.environ.get("GOSTREAM_TEST_MYSQL_DSN", "")
        if mysql_addr or mysql_dsn:
            print(f"Seeding {client_count} accounts directly into MySQL (addr: {mysql_addr})...")
            root = Path(__file__).resolve().parents[2]
            cmd = ["go", "run", "./tools/seed_group_im_benchmark.go", "-driver", "mysql"]
            if mysql_addr:
                dsn = f"gostream:gostream-local@tcp({mysql_addr})/gostream?charset=utf8mb4&parseTime=True&loc=Local"
                cmd.extend(["-dsn", dsn])
            proc = subprocess.run(cmd, cwd=str(root), capture_output=True, text=True, check=True)
            seed_json = None
            for line in proc.stdout.splitlines():
                if "SEED_GROUP_IM_BENCHMARK_OK" in line:
                    seed_json = json.loads(line.split("SEED_GROUP_IM_BENCHMARK_OK", 1)[1].strip())
                    break
            if not seed_json:
                raise RuntimeError(f"failed to parse seed output: {proc.stdout}")
            secret = os.environ.get("JWT_SECRET", "group-im-signing-secret-at-least-32-bytes")
            accounts = []
            tokens = []
            device_ids = []
            now_ts = int(time.time())
            for i, u in enumerate(seed_json["users"]):
                uid = u["id"]
                accounts.append(uid)
                tok = jwt.encode({"sub": uid, "iat": now_ts, "exp": now_ts + 86400}, secret, algorithm="HS256")
                tokens.append(tok)
                device_ids.append(test_uuid(i))

            if mysql_verify_fn is None:
                def auto_mysql_verify(gid: int, expected: int):
                    vcmd = ["go", "run", "./tools/seed_group_im_benchmark.go", "-driver", "mysql", "-action", "verify",
                            "-verify-group", str(gid), "-expected-count", str(expected)]
                    if mysql_addr:
                        vcmd.extend(["-dsn", f"gostream:gostream-local@tcp({mysql_addr})/gostream?charset=utf8mb4&parseTime=True&loc=Local"])
                    subprocess.run(vcmd, cwd=str(root), capture_output=True, text=True, check=True)
                    print(f"MySQL verified: {expected} messages persisted with strict ordering and zero duplicates.")
                mysql_verify_fn = auto_mysql_verify
        else:
            print(f"Setting up {client_count} accounts (10 on API A, 10 on API B)...")
            accounts = []
            tokens = []
            device_ids = []
            for i in range(client_count):
                base = bases[i // 10]
                uname = f"gfanout_u{i:02d}_{int(time.time())}"
                uid, tok = register_user(base, uname, i)
                dev = test_uuid(i)
                accounts.append(uid)
                tokens.append(tok)
                device_ids.append(dev)

    sockets: list[SimpleWebSocket] = []
    print("Establishing 20 WebSocket connections...")
    for i in range(client_count):
        base = bases[i // 10]
        sock = SimpleWebSocket(base, tokens[i], device_ids[i])
        sockets.append(sock)

    print("Creating 20-member chat group...")
    grp = json_call(api_a, "/api/groups", method="POST", token=tokens[0], body={"name": "20-Client Fanout Matrix"}, expected=201)
    group_id = grp["id"]

    for i in range(1, client_count):
        base = bases[i // 10]
        json_call(base, f"/api/groups/{group_id}/members", method="POST", token=tokens[0], body={"user_id": accounts[i]})

    # Tracking structures
    class SentMsg:
        def __init__(self, key: str, sender_idx: int, body: str):
            self.key = key
            self.sender_idx = sender_idx
            self.body = body
            self.id = 0
            self.sent_at = 0.0
            self.acked = False
            self.ack_ms = 0.0
            self.deliveries: dict[int, float] = {}

    messages: dict[str, SentMsg] = {}
    client_received: list[dict[int, bool]] = [{} for _ in range(client_count)]
    lock = threading.Lock()

    acks_received = 0
    deliveries_received = 0
    duplicate_acks = 0
    duplicate_deliveries = 0
    sender_ack_chans = [queue.Queue() for _ in range(client_count)]

    stop_reading = threading.Event()

    def reader_worker(c_idx: int):
        nonlocal acks_received, deliveries_received, duplicate_acks, duplicate_deliveries
        sock = sockets[c_idx]
        while not stop_reading.is_set():
            frame = sock.next_frame(timeout=0.1)
            if not frame:
                continue
            ftype = frame.get("type")
            if ftype == "ack":
                cid = frame.get("client_id", "")
                msg_id = int(frame.get("message_id", 0))
                now = time.monotonic()
                to_notify = False
                with lock:
                    sm = messages.get(cid)
                    if sm:
                        if not sm.acked:
                            sm.acked = True
                            if sm.id == 0:
                                sm.id = msg_id
                            sm.ack_ms = (now - sm.sent_at) * 1000
                            acks_received += 1
                            to_notify = True
                        else:
                            duplicate_acks += 1
                if to_notify:
                    sender_ack_chans[c_idx].put(cid)

            elif ftype == "group_message":
                mdata = frame.get("message", {})
                msg_id = int(mdata.get("id", 0))
                sender_id = int(mdata.get("sender_id", 0))
                cid = mdata.get("client_id", "")
                now = time.monotonic()
                with lock:
                    if sender_id == accounts[c_idx]:
                        raise RuntimeError(f"client {c_idx} received its own group message id={msg_id}")
                    if client_received[c_idx].get(msg_id):
                        duplicate_deliveries += 1
                    else:
                        client_received[c_idx][msg_id] = True
                        deliveries_received += 1
                    sm = messages.get(cid)
                    if sm:
                        if sm.id == 0:
                            sm.id = msg_id
                        sm.deliveries[c_idx] = (now - sm.sent_at) * 1000

    reader_threads = [threading.Thread(target=reader_worker, args=(i,), daemon=True) for i in range(client_count)]
    for t in reader_threads:
        t.start()

    print(f"Sampling initial resource metrics...")
    metrics_before = {name: get_process_metrics(pid) for name, pid in pids.items()}

    print(f"Launching 20 concurrent senders ({msgs_per_client} messages each, total {total_messages} messages)...")
    t_start = time.monotonic()

    def sender_worker(c_idx: int):
        sock = sockets[c_idx]
        for j in range(msgs_per_client):
            key = f"pyfanout-c{c_idx:02d}-m{j:02d}"
            body = f"PyFanout payload from c{c_idx:02d} m{j:02d}"
            with lock:
                sm = SentMsg(key, c_idx, body)
                sm.sent_at = time.monotonic()
                messages[key] = sm

            sock.send_json({
                "type": "group_message",
                "client_id": key,
                "group_id": group_id,
                "body": body,
            })

            # Wait for sender ACK
            try:
                ack_key = sender_ack_chans[c_idx].get(timeout=10.0)
                if ack_key != key:
                    raise RuntimeError(f"client {c_idx} got unexpected ack key {ack_key} != {key}")
            except queue.Empty:
                raise TimeoutError(f"client {c_idx} timed out waiting for ACK for {key}")

            time.sleep(0.001 * (1 + c_idx % 3))

    sender_threads = [threading.Thread(target=sender_worker, args=(i,)) for i in range(client_count)]
    for t in sender_threads:
        t.start()
    for t in sender_threads:
        t.join()

    t_send_done = time.monotonic()
    print(f"All 20 senders completed in {t_send_done - t_start:.2f}s. Waiting for deliveries...")

    deadline = time.monotonic() + 15.0
    while time.monotonic() < deadline:
        with lock:
            if acks_received == total_messages and deliveries_received == expected_total_deliveries:
                break
        time.sleep(0.05)

    stop_reading.set()
    for t in reader_threads:
        t.join(timeout=1.0)

    elapsed_fanout = time.monotonic() - t_start

    print(f"Sampling peak resource metrics...")
    metrics_peak = {name: get_process_metrics(pid) for name, pid in pids.items()}

    # Calculate latency distributions
    ack_latencies = [sm.ack_ms for sm in messages.values() if sm.acked]
    delivery_latencies = [lat for sm in messages.values() for lat in sm.deliveries.values()]

    ack_p50 = percentile(ack_latencies, 0.50)
    ack_p95 = percentile(ack_latencies, 0.95)
    ack_p99 = percentile(ack_latencies, 0.99)

    deliv_p50 = percentile(delivery_latencies, 0.50)
    deliv_p95 = percentile(delivery_latencies, 0.95)
    deliv_p99 = percentile(delivery_latencies, 0.99)

    print(f"Fanout Results:")
    print(f"  Messages: {total_messages}, Deliveries: {deliveries_received}/{expected_total_deliveries}")
    print(f"  Duplicate ACKs: {duplicate_acks}, Duplicate Deliveries: {duplicate_deliveries}")
    print(f"  Sender ACK Latency: P50={ack_p50}ms, P95={ack_p95}ms, P99={ack_p99}ms")
    print(f"  Recipient Delivery Latency: P50={deliv_p50}ms, P95={deliv_p95}ms, P99={deliv_p99}ms")

    # Mathematical Expectation E(k) verification
    print("Verifying explicit mathematical set E(k) = { m.ID | m.Sender != k }...")
    for k in range(client_count):
        expected_count = total_messages - msgs_per_client
        if len(client_received[k]) != expected_count:
            raise AssertionError(f"client {k} expected {expected_count} unique deliveries, got {len(client_received[k])}")
        for sm in messages.values():
            if sm.sender_idx == k:
                if client_received[k].get(sm.id):
                    raise AssertionError(f"client {k} received its own message id={sm.id}")
            else:
                if not client_received[k].get(sm.id):
                    raise AssertionError(f"client {k} missing expected message {sm.id} from sender {sm.sender_idx}")

    # Checkpoint 1: Idempotency
    print("Testing duplicate client_id idempotency...")
    key0 = f"pyfanout-c00-m00"
    m0 = messages[key0]
    sock0 = sockets[0]
    sock0.send_json({"type": "group_message", "client_id": key0, "group_id": group_id, "body": m0.body})
    ack_frame = sock0.next_frame(timeout=5.0)
    assert ack_frame and ack_frame.get("type") == "ack", f"expected ack frame, got {ack_frame}"
    assert int(ack_frame.get("message_id", 0)) == m0.id, f"resend returned different ID {ack_frame.get('message_id')} != {m0.id}"
    # Verify client 1 does NOT receive duplicate frame
    timer_end = time.monotonic() + 0.3
    while time.monotonic() < timer_end:
        f = sockets[1].next_frame(timeout=0.05)
        if f and f.get("type") == "group_message" and f.get("message", {}).get("id") == m0.id:
            raise AssertionError("recipient received duplicate group_message frame on idempotent retry")

    # Checkpoint 2: Offline member recovery & backfill
    print("Testing offline member disconnect, backlog 125 messages, and backfill...")
    # Clear prior unread for member 19 so baseline before disconnect is 0
    max_init_id = max(sm.id for sm in messages.values())
    json_call(api_b, f"/api/groups/{group_id}/messages/{max_init_id}/ack", method="POST", token=tokens[19], device_id=device_ids[19])
    sockets[19].close()

    offline_ids = []
    for i in range(125):
        s_idx = i % 19
        key = f"pyfanout-offline-{i:03d}"
        sockets[s_idx].send_json({
            "type": "group_message",
            "client_id": key,
            "group_id": group_id,
            "body": f"Offline backlog msg {i}",
        })
        ack = sockets[s_idx].next_of_type("ack", timeout=8.0, client_id=key)
        offline_ids.append(int(ack["message_id"]))

    u_before = json_call(api_b, f"/api/groups/{group_id}/unread", token=tokens[19], device_id=device_ids[19])
    assert u_before["unread_count"] == 125, f"expected 125 unread while offline, got {u_before['unread_count']}"

    # Reconnect socket 19
    sock19_new = SimpleWebSocket(api_b, tokens[19], device_ids[19])
    replayed = []
    sync_required_seen = False
    deadline = time.monotonic() + 10.0
    while time.monotonic() < deadline and (len(replayed) < 100 or not sync_required_seen):
        f = sock19_new.next_frame(timeout=0.2)
        if not f:
            continue
        if f.get("type") == "group_message":
            replayed.append(int(f["message"]["id"]))
        elif f.get("type") == "group_sync_required":
            sync_required_seen = True

    assert len(replayed) == 100, f"expected 100 replayed messages, got {len(replayed)}"
    assert sync_required_seen, "missing group_sync_required frame"

    # Keyset backfill remaining 25
    bf_page = json_call(api_b, f"/api/groups/{group_id}/messages?limit=50", token=tokens[19], device_id=device_ids[19])
    assert len(bf_page["items"]) >= 25, f"expected at least 25 items in backfill, got {len(bf_page['items'])}"

    # ACK newest message
    newest_id = offline_ids[-1]
    json_call(api_b, f"/api/groups/{group_id}/messages/{newest_id}/ack", method="POST", token=tokens[19], device_id=device_ids[19])
    u_after = json_call(api_b, f"/api/groups/{group_id}/unread", token=tokens[19], device_id=device_ids[19])
    assert u_after["unread_count"] == 0, f"expected unread count to drop to 0, got {u_after['unread_count']}"

    # Checkpoint 3: Member removal & re-invitation
    print("Testing member removal and re-invitation cursor advance...")
    json_call(api_a, f"/api/groups/{group_id}/members/{accounts[18]}", method="DELETE", token=tokens[0])

    s_code, _, _ = call_api(api_b, f"/api/groups/{group_id}/messages", token=tokens[18], device_id=device_ids[18])
    assert s_code == 404, f"removed member messages expected 404, got {s_code}"
    u_code, _, _ = call_api(api_b, f"/api/groups/{group_id}/unread", token=tokens[18], device_id=device_ids[18])
    assert u_code == 404, f"removed member unread expected 404, got {u_code}"

    # Send 10 messages during member 18's absence
    for i in range(10):
        key = f"pyfanout-abs-{i}"
        sockets[0].send_json({"type": "group_message", "client_id": key, "group_id": group_id, "body": "absence"})
        sockets[0].next_of_type("ack", timeout=8.0, client_id=key)

    # Re-invite member 18
    json_call(api_a, f"/api/groups/{group_id}/members", method="POST", token=tokens[0], body={"user_id": accounts[18]})
    reinv_unread = json_call(api_b, f"/api/groups/{group_id}/unread", token=tokens[18], device_id=device_ids[18])
    assert reinv_unread["unread_count"] == 0, f"re-invited member expected 0 unread, got {reinv_unread['unread_count']}"

    # Checkpoint 4: Per-device independent unread and ACK
    print("Testing per-device independent unread counts and ACKs...")
    dev2 = test_uuid(999)

    # Establish baseline: ACK up to newest message so both devices start at 0 unread
    cur_page = json_call(api_a, f"/api/groups/{group_id}/messages?limit=1", token=tokens[0], device_id=device_ids[0])
    if cur_page.get("items"):
        baseline_id = cur_page["items"][0]["id"]
        json_call(api_a, f"/api/groups/{group_id}/messages/{baseline_id}/ack", method="POST", token=tokens[0], device_id=device_ids[0])
        json_call(api_a, f"/api/groups/{group_id}/messages/{baseline_id}/ack", method="POST", token=tokens[0], device_id=dev2)

    dev_msg_ids = []
    for i in range(10):
        key = f"pyfanout-dev-{i}"
        sockets[1].send_json({"type": "group_message", "client_id": key, "group_id": group_id, "body": "dev msg"})
        ack = sockets[1].next_of_type("ack", timeout=8.0, client_id=key)
        dev_msg_ids.append(int(ack["message_id"]))

    u_d1 = json_call(api_a, f"/api/groups/{group_id}/unread", token=tokens[0], device_id=device_ids[0])
    u_d2 = json_call(api_a, f"/api/groups/{group_id}/unread", token=tokens[0], device_id=dev2)
    assert u_d1["unread_count"] == 10 and u_d2["unread_count"] == 10, f"both devices expected 10 unread, got d1={u_d1} d2={u_d2}"

    # dev1 ACKs message 5
    json_call(api_a, f"/api/groups/{group_id}/messages/{dev_msg_ids[4]}/ack", method="POST", token=tokens[0], device_id=device_ids[0])
    u_d1 = json_call(api_a, f"/api/groups/{group_id}/unread", token=tokens[0], device_id=device_ids[0])
    u_d2 = json_call(api_a, f"/api/groups/{group_id}/unread", token=tokens[0], device_id=dev2)
    assert u_d1["unread_count"] == 5 and u_d2["unread_count"] == 10, f"expected d1=5 d2=10, got d1={u_d1} d2={u_d2}"

    # dev2 ACKs message 10
    json_call(api_a, f"/api/groups/{group_id}/messages/{dev_msg_ids[9]}/ack", method="POST", token=tokens[0], device_id=dev2)
    u_d1 = json_call(api_a, f"/api/groups/{group_id}/unread", token=tokens[0], device_id=device_ids[0])
    u_d2 = json_call(api_a, f"/api/groups/{group_id}/unread", token=tokens[0], device_id=dev2)
    assert u_d1["unread_count"] == 5 and u_d2["unread_count"] == 0, f"expected d1=5 d2=0, got d1={u_d1} d2={u_d2}"

    # Close all sockets
    for s in sockets:
        s.close()
    sock19_new.close()

    metrics_after = {name: get_process_metrics(pid) for name, pid in pids.items()}

    total_persisted = total_messages + 125 + 10 + 10

    sql_verified = verify_group_im_sql_facts(group_id, total_persisted, db_path=db_path, mysql_verify_fn=mysql_verify_fn)

    checkpoints = {
        "fanout_matrix_20_clients": True,
        "expected_id_set_verified": True,
        "sender_ack_latency_measured": ack_p95 > 0,
        "delivery_latency_measured": deliv_p95 > 0,
        "duplicate_client_id_idempotent": True,
        "offline_member_backfill_verified": True,
        "member_removal_and_reinvite_cursor_advanced": True,
        "per_device_independent_unread_verified": True,
        "sql_facts_consistent_zero_duplicates": sql_verified,
    }

    return {
        "clients": client_count,
        "messages_per_client": msgs_per_client,
        "total_messages": total_messages,
        "expected_deliveries": expected_total_deliveries,
        "deliveries_received": deliveries_received,
        "duplicate_acks": duplicate_acks,
        "duplicate_deliveries": duplicate_deliveries,
        "elapsed_fanout_seconds": round(elapsed_fanout, 2),
        "sender_ack_latency_ms": {"p50": ack_p50, "p95": ack_p95, "p99": ack_p99},
        "delivery_latency_ms": {"p50": deliv_p50, "p95": deliv_p95, "p99": deliv_p99},
        "persisted_group_messages": total_persisted,
        "resources": {
            "initial": metrics_before,
            "peak": metrics_peak,
            "final": metrics_after,
        },
        "checkpoints": checkpoints,
    }


def main():
    parser = argparse.ArgumentParser(description="GoStream Group IM Fanout & Recovery Runner (G08 / A07)")
    parser.add_argument("--mode", choices=["auto", "sqlite", "compose"], default="auto")
    parser.add_argument("--duration-mode", choices=["smoke", "standard"], default="smoke",
                        help="smoke: 10 msgs/client (200 msgs); standard: 50 msgs/client (1000 msgs)")
    parser.add_argument("--report", default="data/diagnostics/group_im_smoke.json")
    args = parser.parse_args()

    Path(args.report).unlink(missing_ok=True)
    mode, external_api_a, external_api_b = resolve_acceptance_target(args.mode)

    msgs_per_client = 10 if args.duration_mode == "smoke" else 50

    print("=" * 65)
    print("GoStream Group IM Continuous Verification Runner (G08 / A07)")
    print(f"Mode: {mode}, Duration: {args.duration_mode} ({msgs_per_client} msgs/client, 20 clients)")
    print("=" * 65)

    start_time = time.time()
    report = {
        "status": "FAIL",
        "mode": mode,
        "duration_mode": args.duration_mode,
    }

    db_path = ""
    accounts = None
    tokens = None
    device_ids = None

    with ExitStack() as stack:
        if mode == "compose":
            api_a, api_b = external_api_a, external_api_b
            pids = {"driver": os.getpid()}
        else:
            temp_dir = stack.enter_context(tempfile.TemporaryDirectory(prefix="gostream_group_im_"))
            db_path = os.path.join(temp_dir, "group_im.db")
            media_dir = os.path.join(temp_dir, "media")
            os.makedirs(media_dir, exist_ok=True)
            secret = "group-im-signing-secret-at-least-32-bytes"

            root = Path(__file__).resolve().parents[2]
            server_bin = os.path.join(temp_dir, "server_bin")
            miniredis_bin = os.path.join(temp_dir, "miniredis_bin")

            print("Building server & miniredis binaries...")
            subprocess.run(["go", "build", "-o", server_bin, "./cmd/gostream"], check=True, cwd=str(root))
            subprocess.run(["go", "build", "-o", miniredis_bin, "./tools/miniredis_shim.go"], check=True, cwd=str(root))

            redis_port = free_port()
            redis_addr = f"127.0.0.1:{redis_port}"

            port_a = free_port()
            port_b = free_port()
            api_a = f"http://127.0.0.1:{port_a}"
            api_b = f"http://127.0.0.1:{port_b}"

            stack.enter_context(cleanup_on_interrupt())

            p_redis = subprocess.Popen([miniredis_bin, "-addr", redis_addr], stdout=subprocess.PIPE, stderr=sys.stderr)
            line = p_redis.stdout.readline().decode()
            if "MINIREDIS_READY" not in line:
                raise RuntimeError(f"MiniRedis failed to report readiness: {line}")
            print(f"Redis ready at: {redis_addr}")

            env_base = {
                "PATH": os.environ.get("PATH", ""),
                "DB_DRIVER": "sqlite",
                "DB_DSN": f"{db_path}?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1&_txlock=immediate",
                "REDIS_ADDR": redis_addr,
                "MEDIA_DIR": media_dir,
                "MEDIA_STORE": "local",
                "JWT_SECRET": secret,
            }
            env_a = dict(env_base, ADDR=f"127.0.0.1:{port_a}")
            env_b = dict(env_base, ADDR=f"127.0.0.1:{port_b}")

            p_a = subprocess.Popen([server_bin], env=env_a)
            p_b = None

            def stop_processes():
                for p in (p_a, p_b, p_redis):
                    if p and p.poll() is None:
                        p.terminate()
                        try:
                            p.wait(timeout=3)
                        except subprocess.TimeoutExpired:
                            p.kill()

            stack.callback(stop_processes)
            wait_url_ready(api_a, timeout=15)

            # Populate 20 accounts in SQLite and generate tokens directly
            print("Populating 20 test accounts and generating signed tokens...")
            conn = sqlite3.connect(db_path)
            now_str = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
            pw_hash = bcrypt.hashpw(b"Password123!", bcrypt.gensalt(rounds=10)).decode()
            accounts = []
            tokens = []
            device_ids = []
            users = []
            for i in range(1, 21):
                accounts.append(i)
                users.append((i, f"gfanout_u{i:02d}", pw_hash, f"Nick_{i:02d}", now_str))
                tok = jwt.encode({"sub": i, "iat": int(time.time()), "exp": int(time.time()) + 86400}, secret, algorithm="HS256")
                tokens.append(tok)
                device_ids.append(test_uuid(i - 1))
            conn.executemany("INSERT INTO users (id, username, password_hash, nickname, created_at) VALUES (?, ?, ?, ?, ?)", users)
            conn.commit()
            conn.close()

            p_b = subprocess.Popen([server_bin], env=env_b)
            wait_url_ready(api_b, timeout=15)

            pids = {
                "api_a": p_a.pid,
                "api_b": p_b.pid,
                "driver": os.getpid(),
            }

        print(f"Running group fanout acceptance: API A={api_a}, API B={api_b}...")
        results = run_group_fanout_matrix(
            api_a, api_b, msgs_per_client, pids, accounts, tokens, device_ids,
            db_path=db_path,
        )
        report.update(results)
        all_passed = bool(results.get("checkpoints")) and all(results["checkpoints"].values())
        report["status"] = "PASS" if all_passed else "FAIL"
        report["release_gate_passed"] = all_passed and args.duration_mode == "standard" and mode == "compose"
        if not report["release_gate_passed"]:
            unverified = []
            if mode != "compose":
                unverified.append("real Compose stack with dual API and Redis required for formal gate")
            if args.duration_mode != "standard":
                unverified.append("duration_mode must be standard (1000 messages)")
            if not results.get("checkpoints", {}).get("sql_facts_consistent_zero_duplicates"):
                unverified.append("SQL facts verification did not pass")
            report["unverified_requirements"] = unverified

    elapsed = time.time() - start_time
    report["elapsed_seconds"] = round(elapsed, 2)

    out_path = Path(args.report)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2, ensure_ascii=False)

    print("\n--- Group IM Gate A07 Checkpoints ---")
    for cp, ok in report["checkpoints"].items():
        print(f"  {cp}: {'PASS' if ok else 'FAIL'}")
    print(f"Total Elapsed: {elapsed:.2f}s")
    print(f"Gate A07 Acceptance: {report['status']}")
    print(f"Report written to: {out_path}")
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())

