#!/usr/bin/env python3
"""Exercise like/favorite display counts in an isolated Chromium session.

Requires the agent-browser CLI. The HTTP server supplies fixture API responses;
no GoStream database or running demo instance is touched.
"""

import json
import os
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[2]
WEB_ROOT = Path(os.environ.get("GOSTREAM_WEB_ROOT", ROOT / "web")).resolve()
COUNTS = ((901, 999), (902, 1000), (903, 10000))


class FixtureHandler(SimpleHTTPRequestHandler):
    def log_message(self, _format, *_args):
        pass

    def json_response(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if urlsplit(self.path).path == "/api/feed":
            items = [
                {
                    "id": video_id,
                    "status": "published",
                    "media_url": "/media/fixture.jpg",
                    "title": f"Count {count}",
                    "author": {"nickname": "Fixture"},
                    "like_count": count,
                    "favorite_count": count,
                    "comment_count": 0,
                    "play_count": 0,
                }
                for video_id, count in COUNTS
            ]
            self.json_response(200, {"scene": "recommend", "items": items, "has_more": False})
            return
        super().do_GET()

    def action(self, active):
        path = urlsplit(self.path).path
        if not (path.startswith("/api/videos/") and path.endswith(("/like", "/favorite"))):
            self.json_response(404, {"error": "not found"})
            return
        if path.endswith("/like") and self.server.fail_next_like:
            self.server.fail_next_like = False
            self.json_response(503, {"error": "fixture failure"})
            return
        self.json_response(200, {"active": active})

    def do_PUT(self):
        self.action(True)

    def do_DELETE(self):
        self.action(False)


def main():
    browser = os.environ.get("AGENT_BROWSER_BIN", "agent-browser")
    session = f"gostream-count-{os.getpid()}"
    server = ThreadingHTTPServer(
        ("127.0.0.1", 0), partial(FixtureHandler, directory=str(WEB_ROOT))
    )
    server.fail_next_like = False
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    def cli(*args):
        result = subprocess.run(
            [browser, "--session", session, *args],
            capture_output=True,
            text=True,
            timeout=45,
            check=False,
        )
        if result.returncode:
            raise RuntimeError(f"agent-browser {' '.join(args)}: {result.stderr or result.stdout}")
        return result.stdout.strip()

    def button(video_id, action):
        selector = f'[data-video="{video_id}"] [data-action="{action}"]'
        expression = (
            "(()=>{const button=document.querySelector(" + json.dumps(selector) + ");"
            "return {text:button.textContent,count:button.dataset.count,"
            "active:button.classList.contains('on'),disabled:button.disabled}})()"
        )
        return selector, json.loads(cli("eval", expression))

    def expect(video_id, action, text, count, active):
        deadline = time.monotonic() + 3
        while True:
            _, current = button(video_id, action)
            if current == {"text": text, "count": str(count), "active": active, "disabled": False}:
                return
            if time.monotonic() >= deadline:
                toast = cli("eval", "document.querySelector('#toast')?.textContent")
                requests = cli("network", "requests", "--filter", "api/videos")
                raise AssertionError(
                    f"{video_id} {action}: expected {text}/{count}/{active}, "
                    f"got {current}; toast={toast}; requests={requests}"
                )
            time.sleep(0.05)

    def click(video_id, action):
        selector, _ = button(video_id, action)
        cli("scrollintoview", selector)
        cli("click", selector)

    try:
        cli("set", "viewport", "1280", "577")
        cli("open", f"http://127.0.0.1:{server.server_port}/")
        cli(
            "eval",
            "localStorage.setItem('gostream.token','fixture-token');"
            "localStorage.setItem('gostream.user',JSON.stringify({id:42,nickname:'Fixture'}))",
        )
        cli("reload")
        cli("wait", '[data-video="901"] [data-action="like"]')
        if json.loads(cli("eval", "state.token")) != "fixture-token":
            raise AssertionError("fixture login was not loaded")
        for video_id, count, display in (
            (902, 1000, "1.0k"),
            (901, 999, "1.0k"),
            (903, 10000, "1.0万"),
        ):
            click(video_id, "like")
            expect(video_id, "like", f"♥ {display}", count + 1, True)
            click(video_id, "like")
            expect(video_id, "like", f"♡ {('999' if count == 999 else display)}", count, False)
        click(902, "favorite")
        expect(902, "favorite", "★ 1.0k", 1001, True)
        click(902, "favorite")
        expect(902, "favorite", "☆ 1.0k", 1000, False)
        server.fail_next_like = True
        click(902, "like")
        deadline = time.monotonic() + 3
        while server.fail_next_like and time.monotonic() < deadline:
            time.sleep(0.05)
        if server.fail_next_like:
            raise AssertionError("failed like request did not reach fixture server")
        expect(902, "like", "♡ 1.0k", 1000, False)
        print(json.dumps({"browser": "Chromium", "like_thresholds": [999, 1000, 10000], "favorite": True, "failed_request": True}))
    finally:
        try:
            cli("close")
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


if __name__ == "__main__":
    try:
        main()
    except (AssertionError, OSError, RuntimeError, subprocess.TimeoutExpired) as exc:
        print(exc, file=sys.stderr)
        raise SystemExit(1)
