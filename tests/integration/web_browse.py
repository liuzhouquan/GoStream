#!/usr/bin/env python3
"""Browser interaction regression with real MP4 bytes and fixture API responses.

Not a MySQL/Redis/MinIO acceptance test: web_full_flow.py covers real business.
Exercises exclusive playback, pagination DOM identity, keyboard focus, stale
comments, layout persistence, mobile geometry and cleanup on navigation.
"""
import argparse
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
from urllib.parse import urlsplit, parse_qs

from web_full_flow import BrowserSession
from migration_backup_recovery_smoke import find_default_ffmpeg

ROOT = Path(__file__).resolve().parents[2]


class FixtureHandler(SimpleHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def json_response(self, data, status=200):
        payload = json.dumps(data).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        url = urlsplit(self.path)
        if url.path == "/api/feed":
            page = bool(parse_qs(url.query).get("cursor"))
            self.json_response({"items": self.server.videos[2:] if page else self.server.videos[:2],
                                "has_more": not page, "next_cursor": "fixture-page-2" if not page else ""})
        elif url.path.endswith("/comments"):
            if self.server.fail_comments:
                self.json_response({"error": "fixture unavailable"}, 503)
                return
            self.server.comments_started.set()
            self.server.release_comments.wait(timeout=10)
            self.json_response({"items": [{"id": 1, "body": "这段节奏很舒服", "user": {"nickname": "演示观众"}}], "has_more": False})
        else:
            super().do_GET()

    def do_POST(self):
        if self.path.endswith("/views"):
            self.server.views.append(self.path)
            self.json_response({"counted": True})
        else:
            self.send_error(404)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--screenshots-dir", type=Path)
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    if args.report:
        args.report.unlink(missing_ok=True)
    ffmpeg = find_default_ffmpeg()
    if not ffmpeg:
        raise RuntimeError("requires real FFmpeg, set FFMPEG_BIN")
    with tempfile.TemporaryDirectory(prefix="gostream-browse-") as directory:
        root = Path(directory)
        for name in ("index.html", "styles.css", "app.js"):
            shutil.copy2(ROOT / "web" / name, root / name)
        # A real moving clip, intentionally generated rather than pretending
        # these fixture titles/users are production content.
        subprocess.run([ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i",
                        "testsrc2=duration=4:size=360x640:rate=15", "-an", "-c:v", "libx264",
                        "-preset", "ultrafast", "-pix_fmt", "yuv420p", str(root / "clip.mp4")], check=True, timeout=30)
        server = ThreadingHTTPServer(("127.0.0.1", 0), partial(FixtureHandler, directory=str(root)))
        server.views = []
        server.fail_comments = False
        server.comments_started = threading.Event()
        server.release_comments = threading.Event()
        server.release_comments.set()
        server.videos = [{"id": i, "status": "published", "media_url": "/clip.mp4",
                          "title": title, "description": "浏览交互测试素材 · 上下滑动，发现下一份热爱",
                          "author": {"nickname": "GoStream 演示"}, "like_count": i,
                          "favorite_count": 8, "comment_count": 1}
                         for i, title in [(301, "把生活中的片刻，拍成自己的电影"), (302, "换个视角，发现不一样的日常"), (303, "下一段精彩，等你来记录")]]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        browser = BrowserSession(os.environ.get("AGENT_BROWSER_BIN", "agent-browser"), f"gostream-browse-{os.getpid()}")
        results = {"scope": "browser_interaction_fixture_with_real_mp4"}
        def check(name, expression):
            try:
                browser.wait_for(lambda: browser.eval_js(expression))
            except TimeoutError as error:
                raise AssertionError(f"{name}: browser condition did not become true") from error
            results[name] = True
        def shot(name):
            if args.screenshots_dir:
                args.screenshots_dir.mkdir(parents=True, exist_ok=True)
                browser.cli("screenshot", str((args.screenshots_dir / name).resolve()))
        try:
            browser.cli("set", "viewport", "1440", "900")
            browser.cli("open", f"http://127.0.0.1:{server.server_port}/")
            check("first_muted_autoplay", "(()=>{const v=document.querySelector('video');return v&&!v.paused&&v.muted&&v.currentTime>0.05})()")
            check("exclusive_playback", "[...document.querySelectorAll('video')].filter(v=>!v.paused).length===1")
            check("header_in_view", "document.querySelector('.topbar').getBoundingClientRect().top>=0 && document.documentElement.scrollHeight<=innerHeight+1")
            shot("desktop-feed.png")
            browser.eval_js("window.firstVideo=document.querySelector('video'); document.querySelector('.load-more').click()")
            check("append_preserves_video_node", "document.querySelectorAll('.video-card').length===3 && window.firstVideo===document.querySelector('video')")
            if server.views.count("/api/videos/301/views") != 1:
                raise AssertionError("pagination replayed the first video's view event")
            browser.cli("fill", "#global-search", "键盘焦点测试")
            before = browser.eval_js("document.querySelector('#content').scrollTop")
            browser.cli("press", "ArrowDown")
            if browser.eval_js("document.querySelector('#content').scrollTop") != before:
                raise AssertionError("typing in search moved the video")
            results["keyboard_ignores_input"] = True
            browser.eval_js("document.activeElement.blur()")
            browser.cli("press", "ArrowDown")
            check("keyboard_next_video", "(()=>{const a=[...document.querySelectorAll('video')];return a[0].paused&&!a[1].paused&&a[1].currentTime>0.05})()")
            browser.cli("click", '[data-video="302"] [data-action="comment"]')
            check("comment_drawer", "document.querySelector('#comments-302 .comment-list')?.textContent.includes('这段节奏很舒服')")
            shot("desktop-comments.png")
            browser.cli("press", "Escape")
            check("escape_closes_comments", "!document.querySelector('[data-video=\"302\"]').classList.contains('comments-open')")
            server.release_comments.clear()
            browser.eval_js("window.commentSettled=false;window.originalLoadComments=loadComments;loadComments=async(...args)=>{try{return await window.originalLoadComments(...args)}finally{window.commentSettled=true}}")
            server.comments_started.clear()
            browser.cli("click", '[data-video="302"] [data-action="comment"]')
            if not server.comments_started.wait(timeout=5):
                raise AssertionError("delayed comment request not reached")
            browser.cli("click", '[data-close-comments="302"]')
            server.release_comments.set()
            # Observe actual handler completion, not a guessed timeout.
            check("stale_comments_stay_closed", "window.commentSettled && !document.querySelector('[data-video=\"302\"]').classList.contains('comments-open') && !document.querySelector('#comments-302').textContent")
            server.fail_comments = True
            browser.cli("click", '[data-video="302"] [data-action="comment"]')
            check("comment_failure_retry_control", "Boolean(document.querySelector('#comments-302 [data-comment-retry]'))")
            server.fail_comments = False
            browser.cli("click", '[data-comment-retry="302"]')
            check("comment_retry_recovers", "document.querySelector('#comments-302 .comment-list')?.textContent.includes('这段节奏很舒服')")
            browser.cli("click", '[data-close-comments="302"]')
            browser.cli("click", '[data-layout-choice="grid"]')
            check("grid_layout", "!document.querySelector('#content').classList.contains('theater-feed') && [...document.querySelectorAll('video')].every(v=>v.paused)")
            browser.cli("reload")
            check("layout_persisted", "document.body.dataset.layout==='grid' && document.querySelectorAll('.video-card').length===2")
            browser.cli("click", '[data-layout-choice="theater"]')
            browser.cli("set", "viewport", "390", "844")
            check("mobile_no_overflow", "document.documentElement.scrollWidth<=innerWidth && document.documentElement.scrollHeight<=innerHeight+1")
            check("mobile_five_tabs", "[...document.querySelectorAll('.nav-item')].filter(e=>getComputedStyle(e).display!=='none').length===5")
            shot("mobile-feed.png")
            browser.cli("click", '[data-video="301"] [data-action="comment"]')
            check("mobile_comment_sheet", "(()=>{const r=document.querySelector('#comments-301').getBoundingClientRect();return r.width>250&&r.width<=innerWidth&&r.bottom<=innerHeight-60})()")
            shot("mobile-comments.png")
            browser.eval_js("window.oldVideo=document.querySelector('video')")
            browser.cli("press", "Escape")
            browser.cli("click", '[data-view="profile"]')
            check("navigation_stops_media", "window.oldVideo.paused&&!window.oldVideo.isConnected&&!document.querySelector('#content').classList.contains('theater-feed')")
            errors = browser.cli("errors")
            if errors.strip():
                raise AssertionError("browser reported JavaScript errors: " + errors)
            results["no_javascript_errors"] = True
            results["status"] = "PASS"
            if args.report:
                args.report.parent.mkdir(parents=True, exist_ok=True)
                args.report.write_text(json.dumps(results, indent=2) + "\n")
            print(json.dumps(results, ensure_ascii=False))
        finally:
            server.release_comments.set()
            try:
                browser.close()
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=5)


if __name__ == "__main__":
    main()
