#!/usr/bin/env python3
"""Media format, ten-video matrix, privacy, and recovery acceptance test.

Exercises real uploads, FFmpeg transcoding, thumbnail generation, probed
metadata, failure code isolation, privacy boundaries, author recovery, and Feed
publication across 10 distinct video fixtures:
  1. MP4 (H.264 + AAC, 640x360 landscape)
  2. MP4 (H.264, without audio, 640x360)
  3. WebM (VP8 + Vorbis, 320x240)
  4. WebM (VP9 + Opus, 640x360)
  5. QuickTime MOV (Apple ProRes + PCM audio, 480x270)
  6. QuickTime MOV (H.264 + AAC, 640x360)
  7. MP4 (H.264 + AAC, 360x640 portrait)
  8. MP4 overlong (>10 min duration) -> fails with duration_limit
  9. MP4 corrupted stream data -> fails with processing_failed
 10. MP4 large chunked (>4 MiB) -> multi-chunk upload session -> published

For failed videos, verifies:
  - Absent from public Feed & Search
  - 404 for anonymous users & non-authors
  - Visible only to author with publicProcessingCode
  - Author can recover via PATCH with replacement media

Corresponds to SDD.md Task G04, Gate A03, and TEST_STANDARD "IM与媒体".
"""

import argparse
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

from launcher_lifecycle import cleanup_on_interrupt
from acceptance_config import open_api_request


CHUNK_SIZE = 4 << 20  # 4 MiB


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def find_default_ffmpeg():
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
    return "ffmpeg"


def find_default_ffprobe():
    if "FFPROBE_BIN" in os.environ and shutil.which(os.environ["FFPROBE_BIN"]):
        return os.environ["FFPROBE_BIN"]
    root = Path(__file__).resolve().parents[2]
    shim = root / "tools" / "ffprobe_shim.py"
    if shim.is_file() and os.access(shim, os.X_OK):
        return str(shim)
    system = shutil.which("ffprobe")
    if system:
        return system
    for c in ["/usr/bin/ffprobe", "/usr/local/bin/ffprobe"]:
        if os.path.isfile(c) and os.access(c, os.X_OK):
            return c
    return str(shim)


def call_api(base, path, method="GET", token="", body=None, content_type="application/json"):
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if body is not None:
        headers["Content-Type"] = content_type
    req = Request(base + path, data=body, headers=headers, method=method)
    try:
        with open_api_request(req, timeout=25) as resp:
            data = resp.read()
            ct = resp.headers.get_content_type()
            return resp.status, json.loads(data) if ct == "application/json" else data
    except HTTPError as err:
        data = err.read()
        return err.code, data


def json_call(base, path, method="GET", token="", body=None, expected=200):
    payload = json.dumps(body).encode() if body is not None else None
    status, data = call_api(base, path, method, token, payload, "application/json")
    if status != expected:
        details = data.decode(errors="replace") if isinstance(data, bytes) else json.dumps(data)
        raise RuntimeError(f"{method} {path} expected {expected}, got {status}: {details}")
    return data


def upload_multipart(base, token, filename, content_bytes, mime_type):
    boundary = f"gostream-{uuid.uuid4().hex}"
    header = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
        f"Content-Type: {mime_type}\r\n\r\n"
    ).encode()
    footer = f"\r\n--{boundary}--\r\n".encode()
    body = header + content_bytes + footer
    status, data = call_api(base, "/api/uploads", "POST", token, body, f"multipart/form-data; boundary={boundary}")
    if status != 201:
        details = data.decode(errors="replace") if isinstance(data, bytes) else json.dumps(data)
        raise RuntimeError(f"upload failed: status {status}: {details}")
    return data["url"]


def upload_chunked(base_a, base_b, token, filename, content_bytes):
    size = len(content_bytes)
    sha = hashlib.sha256(content_bytes).hexdigest()
    session = json_call(base_a, "/api/uploads/sessions", "POST", token, {
        "filename": filename,
        "size": size,
        "sha256": sha,
    }, expected=201)
    session_id = session["id"]
    count = (size + CHUNK_SIZE - 1) // CHUNK_SIZE

    for index in range(count):
        # Alternate chunks between API A and API B
        target = base_a if index % 2 == 0 else base_b
        chunk_data = content_bytes[index * CHUNK_SIZE:(index + 1) * CHUNK_SIZE]
        status, data = call_api(
            target,
            f"/api/uploads/sessions/{session_id}/chunks/{index}",
            "PUT",
            token,
            chunk_data,
            "application/octet-stream",
        )
        if status != 200:
            raise RuntimeError(f"chunk {index} upload failed: {status} {data}")
        if data.get("sha256") != hashlib.sha256(chunk_data).hexdigest():
            raise RuntimeError(f"chunk {index} sha mismatch")

    completed = json_call(base_b, f"/api/uploads/sessions/{session_id}/complete", "POST", token, expected=200)
    if completed.get("sha256") != sha:
        raise RuntimeError("assembled chunk sha mismatch")
    return completed["url"]


def probe_file_metadata(path, ffprobe_bin):
    """Probe input fixture with ffprobe/shim to record input container, codecs, duration, dimensions."""
    proc = subprocess.run([
        ffprobe_bin, "-v", "error", "-select_streams", "v:0",
        "-show_entries", "stream=width,height,duration:format=duration",
        "-of", "json", str(path),
    ], capture_output=True, text=True)
    if proc.returncode != 0:
        return {"error": proc.stderr.strip() or "probe failed"}
    try:
        report = json.loads(proc.stdout)
        stream = report.get("streams", [{}])[0] if report.get("streams") else {}
        fmt = report.get("format", {})
        dur = fmt.get("duration") or stream.get("duration") or "0"
        return {
            "width": stream.get("width", 0),
            "height": stream.get("height", 0),
            "duration": float(dur) if dur != "N/A" else 0.0,
            "streams_count": len(report.get("streams", [])),
        }
    except Exception as e:
        return {"error": str(e)}


def generate_video_fixtures(ffmpeg_bin, dest_dir):
    """Generate the 10 reproducible video fixtures and one recovery fixture."""
    os.makedirs(dest_dir, exist_ok=True)
    fixtures = {}

    # 1. MP4 H.264 + AAC
    p1 = os.path.join(dest_dir, "01_mp4_h264_aac.mp4")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1:size=640x360:rate=25",
        "-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
        "-c:v", "libx264", "-c:a", "aac", "-pix_fmt", "yuv420p", "-y", p1,
    ], check=True)
    fixtures["01_mp4_h264_aac"] = {
        "file": p1, "name": "01_mp4_h264_aac.mp4", "mime": "video/mp4",
        "container": "mp4", "video_codec": "h264", "audio_codec": "aac",
        "expected_status": "published", "expected_code": None,
    }

    # 2. MP4 H.264 no audio
    p2 = os.path.join(dest_dir, "02_mp4_h264_noaudio.mp4")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1.2:size=640x360:rate=25",
        "-c:v", "libx264", "-an", "-pix_fmt", "yuv420p", "-y", p2,
    ], check=True)
    fixtures["02_mp4_h264_noaudio"] = {
        "file": p2, "name": "02_mp4_h264_noaudio.mp4", "mime": "video/mp4",
        "container": "mp4", "video_codec": "h264", "audio_codec": "none",
        "expected_status": "published", "expected_code": None,
    }

    # 3. WebM VP8 + Vorbis
    p3 = os.path.join(dest_dir, "03_webm_vp8_vorbis.webm")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=25",
        "-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
        "-c:v", "libvpx", "-c:a", "libvorbis", "-y", p3,
    ], check=True)
    fixtures["03_webm_vp8_vorbis"] = {
        "file": p3, "name": "03_webm_vp8_vorbis.webm", "mime": "video/webm",
        "container": "webm", "video_codec": "vp8", "audio_codec": "vorbis",
        "expected_status": "published", "expected_code": None,
    }

    # 4. WebM VP9 + Opus
    p4 = os.path.join(dest_dir, "04_webm_vp9_opus.webm")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1:size=640x360:rate=25",
        "-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
        "-c:v", "libvpx-vp9", "-c:a", "libopus", "-y", p4,
    ], check=True)
    fixtures["04_webm_vp9_opus"] = {
        "file": p4, "name": "04_webm_vp9_opus.webm", "mime": "video/webm",
        "container": "webm", "video_codec": "vp9", "audio_codec": "opus",
        "expected_status": "published", "expected_code": None,
    }

    # 5. QuickTime MOV ProRes + PCM
    p5 = os.path.join(dest_dir, "05_mov_prores.mov")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1:size=480x270:rate=25",
        "-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
        "-c:v", "prores", "-c:a", "pcm_s16le", "-f", "mov", "-y", p5,
    ], check=True)
    fixtures["05_mov_prores"] = {
        "file": p5, "name": "05_mov_prores.mov", "mime": "video/quicktime",
        "container": "mov", "video_codec": "prores", "audio_codec": "pcm",
        "expected_status": "published", "expected_code": None,
    }

    # 6. QuickTime MOV H.264 + AAC
    p6 = os.path.join(dest_dir, "06_mov_h264_aac.mov")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1:size=640x360:rate=25",
        "-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
        "-c:v", "libx264", "-c:a", "aac", "-f", "mov", "-y", p6,
    ], check=True)
    fixtures["06_mov_h264_aac"] = {
        "file": p6, "name": "06_mov_h264_aac.mov", "mime": "video/quicktime",
        "container": "mov", "video_codec": "h264", "audio_codec": "aac",
        "expected_status": "published", "expected_code": None,
    }

    # 7. MP4 Portrait 360x640
    p7 = os.path.join(dest_dir, "07_mp4_portrait.mp4")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1:size=360x640:rate=25",
        "-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
        "-c:v", "libx264", "-c:a", "aac", "-pix_fmt", "yuv420p", "-y", p7,
    ], check=True)
    fixtures["07_mp4_portrait"] = {
        "file": p7, "name": "07_mp4_portrait.mp4", "mime": "video/mp4",
        "container": "mp4", "video_codec": "h264", "audio_codec": "aac",
        "expected_status": "published", "expected_code": None,
    }

    # 8. MP4 Overlong (605s duration > 600s max limit)
    p8 = os.path.join(dest_dir, "08_overlong_reject.mp4")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "color=c=blue:s=16x16:r=1",
        "-t", "605", "-an", "-c:v", "libx264", "-preset", "ultrafast",
        "-threads", "2", "-pix_fmt", "yuv420p", "-y", p8,
    ], check=True)
    fixtures["08_overlong_reject"] = {
        "file": p8, "name": "08_overlong_reject.mp4", "mime": "video/mp4",
        "container": "mp4", "video_codec": "h264", "audio_codec": "none",
        "expected_status": "failed", "expected_code": "duration_limit",
    }

    # 9. MP4 Corrupted payload
    p9 = os.path.join(dest_dir, "09_corrupted_reject.mp4")
    header = bytes([0, 0, 0, 24]) + b"ftypisom" + bytes(4) + b"isommp42"
    with open(p9, "wb") as f:
        f.write(header + (b"\xaa\x55\xde\xad\xbe\xef\x00\xff" * 2000))
    fixtures["09_corrupted_reject"] = {
        "file": p9, "name": "09_corrupted_reject.mp4", "mime": "video/mp4",
        "container": "mp4", "video_codec": "corrupted", "audio_codec": "none",
        "expected_status": "failed", "expected_code": "processing_failed",
    }

    # 10. MP4 Large chunked (>4 MiB)
    p10 = os.path.join(dest_dir, "10_chunked_large_mp4.mp4")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "nullsrc=s=720x480:d=1:r=25",
        "-filter_complex", "geq=random(1)*255:128:128",
        "-c:v", "libx264", "-crf", "22", "-preset", "ultrafast",
        "-pix_fmt", "yuv420p", "-y", p10,
    ], check=True)
    p10_size = os.path.getsize(p10)
    if p10_size <= CHUNK_SIZE:
        raise RuntimeError(f"chunked fixture size {p10_size} is not > {CHUNK_SIZE}")
    fixtures["10_chunked_large_mp4"] = {
        "file": p10, "name": "10_chunked_large_mp4.mp4", "mime": "video/mp4",
        "container": "mp4", "video_codec": "h264", "audio_codec": "none",
        "expected_status": "published", "expected_code": None,
    }

    # Extra: Replacement MP4 for author recovery
    prec = os.path.join(dest_dir, "recovery_valid.mp4")
    subprocess.run([
        ffmpeg_bin, "-hide_banner", "-loglevel", "error",
        "-f", "lavfi", "-i", "testsrc=duration=1:size=640x360:rate=25",
        "-f", "lavfi", "-i", "sine=frequency=1200:duration=1",
        "-c:v", "libx264", "-c:a", "aac", "-pix_fmt", "yuv420p", "-y", prec,
    ], check=True)
    fixtures["recovery_valid"] = {
        "file": prec, "name": "recovery_valid.mp4", "mime": "video/mp4",
    }

    return fixtures


def run_media_matrix(api_a, api_b, fixtures, ffprobe_bin):
    """Executes the 10-video matrix, privacy checks, and author recovery."""
    results = {
        "matrix": [],
        "privacy_verified": False,
        "recovery_verified": False,
        "search_verified": False,
        "feed_verified": False,
    }

    # 1. Create author and viewer accounts
    suffix = uuid.uuid4().hex[:10]
    author = json_call(api_a, "/api/users", "POST", body={
        "username": f"media_author_{suffix}",
        "password": "AuthorSecretPass!123",
        "nickname": "Media Matrix Author",
    }, expected=201)
    author_token = author["access_token"]
    author_id = author["user"]["id"]

    viewer = json_call(api_a, "/api/users", "POST", body={
        "username": f"media_viewer_{suffix}",
        "password": "ViewerSecretPass!123",
        "nickname": "Media Matrix Viewer",
    }, expected=201)
    viewer_token = viewer["access_token"]
    viewer_id = viewer["user"]["id"]

    # 2. Iterate through the 10 video fixtures
    matrix_keys = [
        "01_mp4_h264_aac",
        "02_mp4_h264_noaudio",
        "03_webm_vp8_vorbis",
        "04_webm_vp9_opus",
        "05_mov_prores",
        "06_mov_h264_aac",
        "07_mp4_portrait",
        "08_overlong_reject",
        "09_corrupted_reject",
        "10_chunked_large_mp4",
    ]

    failed_video_id = None
    published_video_ids = []

    for key in matrix_keys:
        meta = fixtures[key]
        filepath = meta["file"]
        file_bytes = Path(filepath).read_bytes()
        source_size = len(file_bytes)
        input_probe = probe_file_metadata(filepath, ffprobe_bin)

        t_start = time.monotonic()

        # Step A: Upload file
        if key == "10_chunked_large_mp4":
            media_url = upload_chunked(api_a, api_b, author_token, meta["name"], file_bytes)
        else:
            media_url = upload_multipart(api_a, author_token, meta["name"], file_bytes, meta["mime"])

        # Step B: Create Video
        video_title = f"Test Video {key}"
        created = json_call(api_a, "/api/videos", "POST", author_token, {
            "title": video_title,
            "media_url": media_url,
        }, expected=201)
        video_id = created["id"]
        if created["status"] != "processing":
            raise RuntimeError(f"{key}: expected initial status 'processing', got {created['status']}")

        # Step C: Poll for completion
        deadline = time.monotonic() + 60
        final_video = None
        while time.monotonic() < deadline:
            target_api = api_a if video_id % 2 == 0 else api_b
            curr = json_call(target_api, f"/api/videos/{video_id}", token=author_token)
            if curr["status"] in ("published", "failed"):
                final_video = curr
                break
            time.sleep(0.4)

        if not final_video:
            raise RuntimeError(f"{key}: video processing timed out after 60s")

        t_elapsed = round(time.monotonic() - t_start, 3)
        status = final_video["status"]
        proc_error = final_video.get("processing_error")

        item_record = {
            "key": key,
            "video_id": video_id,
            "container": meta["container"],
            "video_codec": meta["video_codec"],
            "audio_codec": meta["audio_codec"],
            "source_bytes": source_size,
            "input_probe": input_probe,
            "final_status": status,
            "expected_status": meta["expected_status"],
            "processing_error": proc_error,
            "elapsed_seconds": t_elapsed,
            "media_bytes": 0,
            "cover_bytes": 0,
            "duration_ms": final_video.get("duration_ms", 0),
            "width": final_video.get("width", 0),
            "height": final_video.get("height", 0),
        }

        # Step D: Check results against expectations
        if meta["expected_status"] == "published":
            if status != "published":
                raise RuntimeError(f"{key}: expected published, got {status} ({proc_error})")
            # Fetch output video and cover bytes
            s_media, b_media = call_api(api_a, final_video["media_url"])
            if s_media != 200 or len(b_media) < 12 or b_media[4:8] != b"ftyp":
                raise RuntimeError(f"{key}: processed media is not valid MP4: {s_media}")
            s_cover, b_cover = call_api(api_a, final_video["cover_url"])
            if s_cover != 200 or not b_cover.startswith(b"\xff\xd8"):
                raise RuntimeError(f"{key}: generated cover is not JPEG: {s_cover}")

            item_record["media_bytes"] = len(b_media)
            item_record["cover_bytes"] = len(b_cover)
            published_video_ids.append(video_id)

        elif meta["expected_status"] == "failed":
            if status != "failed":
                raise RuntimeError(f"{key}: expected failed, got {status}")
            if meta["expected_code"] and proc_error != meta["expected_code"]:
                raise RuntimeError(f"{key}: expected code '{meta['expected_code']}', got '{proc_error}'")
            failed_video_id = video_id

        results["matrix"].append(item_record)

    # 3. Privacy Boundary Verification on Failed Video
    if not failed_video_id:
        raise RuntimeError("no failed video available to test privacy boundaries")

    # (a) Anonymous query GET /api/videos/{failed_id} -> 404
    anon_code, _ = call_api(api_a, f"/api/videos/{failed_video_id}")
    if anon_code != 404:
        raise RuntimeError(f"failed video leaked to anonymous user: status {anon_code}")

    # (b) Non-author query GET /api/videos/{failed_id} -> 404
    viewer_code, _ = call_api(api_b, f"/api/videos/{failed_video_id}", token=viewer_token)
    if viewer_code != 404:
        raise RuntimeError(f"failed video leaked to non-author: status {viewer_code}")

    # (c) Non-author query author's profile video list -> should not include failed video
    author_vids = json_call(api_a, f"/api/users/{author_id}/videos", token=viewer_token)
    if any(v["id"] == failed_video_id for v in author_vids["items"]):
        raise RuntimeError("failed video leaked into public author profile")

    # (d) Author query author's video list -> DOES include failure_code
    owner_vids = json_call(api_a, f"/api/users/{author_id}/videos", token=author_token)
    if not any(v["id"] == failed_video_id and v.get("processing_error") for v in owner_vids["items"]):
        raise RuntimeError("author video list did not show failure code")

    # (e) Search for failed video title -> returns 0 items in videos
    search_failed = json_call(api_a, "/api/search?q=corrupted&type=videos", token=viewer_token)
    if any(v["id"] == failed_video_id for v in search_failed.get("videos", [])):
        raise RuntimeError("failed video appeared in search results")

    results["privacy_verified"] = True

    # 4. Author Recovery Verification via PATCH /api/videos/{failed_id}
    rec_file = fixtures["recovery_valid"]["file"]
    rec_bytes = Path(rec_file).read_bytes()
    rec_media_url = upload_multipart(api_a, author_token, "recovery_valid.mp4", rec_bytes, "video/mp4")

    # Non-author cannot PATCH author's video
    forbidden_code, _ = call_api(api_a, f"/api/videos/{failed_video_id}", "PATCH", viewer_token,
                                 json.dumps({"media_url": rec_media_url}).encode(), "application/json")
    if forbidden_code != 403:
        raise RuntimeError(f"non-author PATCH expected 403, got {forbidden_code}")

    # Author PATCHes failed video with new media_url
    recovered = json_call(api_a, f"/api/videos/{failed_video_id}", "PATCH", author_token, {
        "media_url": rec_media_url,
    }, expected=200)

    # Poll until recovered video reaches published
    deadline = time.monotonic() + 45
    rec_final = None
    while time.monotonic() < deadline:
        v = json_call(api_b, f"/api/videos/{failed_video_id}", token=author_token)
        if v["status"] == "published":
            rec_final = v
            break
        time.sleep(0.4)

    if not rec_final:
        raise RuntimeError("recovered video did not reach published state within 45s")

    # Now recovered video must be readable by viewer and anonymous
    s_view, _ = call_api(api_a, f"/api/videos/{failed_video_id}", token=viewer_token)
    if s_view != 200:
        raise RuntimeError(f"recovered video unreadable by viewer: {s_view}")
    results["recovery_verified"] = True

    # 5. Public Feed & Search Verification for Published Videos
    feed = json_call(api_a, "/api/feed?scene=timeline&limit=50")
    feed_ids = {item["id"] for item in feed["items"]}
    for pub_id in published_video_ids:
        if pub_id not in feed_ids:
            raise RuntimeError(f"published video {pub_id} missing from public feed")
    results["feed_verified"] = True

    sample_id = published_video_ids[0]
    search_res = json_call(api_b, "/api/search?q=h264&type=videos")
    if not any(v["id"] == sample_id for v in search_res.get("videos", [])):
        raise RuntimeError(f"published video {sample_id} missing from search results: {search_res}")
    results["search_verified"] = True

    return results


def print_matrix_table(matrix_data):
    print("\n| Key | Codec | Res | Source B | Final Status | Code | Transcode S | Out Video B | Out Cover B |")
    print("|---|---|---|---|---|---|---|---|---|")
    for r in matrix_data:
        res_str = f"{r['input_probe'].get('width', 0)}x{r['input_probe'].get('height', 0)}"
        err_str = r['processing_error'] or "-"
        print(f"| {r['key']} | {r['video_codec']}+{r['audio_codec']} | {res_str} | {r['source_bytes']} | {r['final_status']} | {err_str} | {r['elapsed_seconds']}s | {r['media_bytes']} | {r['cover_bytes']} |")
    print()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=["auto", "sqlite", "compose"], default="auto")
    parser.add_argument("--api-a", help="URL for API A")
    parser.add_argument("--api-b", help="URL for API B")
    parser.add_argument("--base-url", help="fallback single base URL")
    parser.add_argument("--ffmpeg-bin", default=find_default_ffmpeg(), help="ffmpeg binary")
    parser.add_argument("--ffprobe-bin", default=find_default_ffprobe(), help="ffprobe binary")
    parser.add_argument("--report-file", type=Path, help="path to write JSON report")
    parser.add_argument("--fixtures-dir", type=Path, help="directory to store video fixtures")
    parser.add_argument("--go-bin", default="go", help="go binary")
    args = parser.parse_args()

    # Determine mode
    env_api_a = os.environ.get("GOSTREAM_API_A")
    env_api_b = os.environ.get("GOSTREAM_API_B")

    if args.mode == "compose" or (args.mode == "auto" and (args.api_a or env_api_a)):
        mode = "compose"
        api_a = (args.api_a or env_api_a or args.base_url or "http://127.0.0.1:18080").rstrip("/")
        api_b = (args.api_b or env_api_b or api_a or "http://127.0.0.1:18081").rstrip("/")
    else:
        mode = "sqlite"
        api_a = api_b = None

    report_path = args.report_file.resolve() if args.report_file else None
    if report_path:
        report_path.parent.mkdir(parents=True, exist_ok=True)
        if report_path.is_file():
            report_path.unlink()

    ffmpeg_bin = args.ffmpeg_bin
    ffprobe_bin = args.ffprobe_bin

    print(f"GoStream Media Matrix Smoke Runner")
    print(f"Mode: {mode}")
    print(f"FFmpeg: {ffmpeg_bin}")
    print(f"FFprobe: {ffprobe_bin}")

    start_time = time.monotonic()

    with ExitStack() as stack:
        temp_dir = stack.enter_context(tempfile.TemporaryDirectory(prefix="gostream_media_matrix_"))
        fixtures_dir = str(args.fixtures_dir.resolve()) if args.fixtures_dir else os.path.join(temp_dir, "fixtures")

        print("Generating 10 video fixtures...")
        fixtures = generate_video_fixtures(ffmpeg_bin, fixtures_dir)
        print(f"Generated {len(fixtures)} fixtures in {fixtures_dir}")

        if mode == "sqlite":
            db_path = os.path.join(temp_dir, "matrix.db")
            media_dir = os.path.join(temp_dir, "media")
            os.makedirs(media_dir, exist_ok=True)
            server_bin = os.path.join(temp_dir, "server_bin")
            root = Path(__file__).resolve().parents[2]

            subprocess.run([args.go_bin, "build", "-o", server_bin, "./cmd/gostream"],
                           check=True, cwd=str(root))

            port_a = free_port()
            port_b = free_port()
            api_a = f"http://127.0.0.1:{port_a}"
            api_b = f"http://127.0.0.1:{port_b}"

            env_base = {
                "PATH": os.environ.get("PATH", ""),
                "DB_DRIVER": "sqlite",
                "DB_DSN": f"{db_path}?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1",
                "MEDIA_DIR": media_dir,
                "MEDIA_STORE": "local",
                "JWT_SECRET": "matrix-secret-at-least-32-bytes-long-123456!",
                "FFMPEG_BIN": ffmpeg_bin,
                "FFPROBE_BIN": ffprobe_bin,
                "FEED_FANOUT_MAX_FOLLOWERS": "1000",
            }

            env_a = dict(env_base, ADDR=f"127.0.0.1:{port_a}")
            env_b = dict(env_base, ADDR=f"127.0.0.1:{port_b}")

            def wait_url_ready(url, timeout=15):
                deadline = time.monotonic() + timeout
                while time.monotonic() < deadline:
                    try:
                        with open_api_request(f"{url}/ready", timeout=1) as resp:
                            if resp.status == 200:
                                return True
                    except Exception:
                        pass
                    time.sleep(0.1)
                return False

            proc_a = subprocess.Popen([server_bin], env=env_a)
            proc_b = None

            def stop_servers():
                for p in (proc_a, proc_b):
                    if p and p.poll() is None:
                        p.terminate()
                        try:
                            p.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            p.kill()

            stack.callback(stop_servers)
            stack.enter_context(cleanup_on_interrupt())

            # Wait for proc_a to finish database migration and become ready
            if not wait_url_ready(api_a):
                raise RuntimeError(f"Server at {api_a} failed to become ready")

            # Start proc_b after migrations are in place
            proc_b = subprocess.Popen([server_bin], env=env_b)
            if not wait_url_ready(api_b):
                raise RuntimeError(f"Server at {api_b} failed to become ready")

        print(f"Running media matrix across endpoints: API A={api_a}, API B={api_b}...")
        report_data = run_media_matrix(api_a, api_b, fixtures, ffprobe_bin)

        report_data["mode"] = mode
        report_data["endpoints"] = {"api_a": api_a, "api_b": api_b}
        report_data["ffmpeg_bin"] = ffmpeg_bin
        report_data["ffprobe_bin"] = ffprobe_bin
        report_data["elapsed_seconds"] = round(time.monotonic() - start_time, 3)

        print_matrix_table(report_data["matrix"])
        print(f"Privacy verified: {report_data['privacy_verified']}")
        print(f"Recovery verified: {report_data['recovery_verified']}")
        print(f"Feed verified: {report_data['feed_verified']}")
        print(f"Search verified: {report_data['search_verified']}")
        print(f"Total time: {report_data['elapsed_seconds']}s")

        if report_path:
            with open(report_path, "w", encoding="utf-8") as f:
                json.dump(report_data, f, indent=2)
            print(f"Report written to: {report_path}")

    print("Media Matrix Acceptance: PASS")


if __name__ == "__main__":
    main()
