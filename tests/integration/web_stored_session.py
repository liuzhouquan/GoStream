#!/usr/bin/env python3
"""Check browser startup after damaged local authentication state.

Requires agent-browser and Chromium. It serves a fixture Feed on loopback and
never connects to a running GoStream API or database.
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


class FixtureHandler(SimpleHTTPRequestHandler):
    def log_message(self, _format, *_args):
        pass

    def do_GET(self):
        if urlsplit(self.path).path == "/api/feed":
            body = json.dumps({
                "scene": "recommend",
                "items": [{
                    "id": 904,
                    "status": "published",
                    "media_url": "/media/fixture.jpg",
                    "title": "Storage fixture",
                    "author": {"nickname": "Fixture"},
                    "like_count": 0,
                    "favorite_count": 0,
                    "comment_count": 0,
                    "play_count": 0,
                }],
                "has_more": False,
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        super().do_GET()


def main():
    browser = os.environ.get("AGENT_BROWSER_BIN", "agent-browser")
    session = f"gostream-stored-session-{os.getpid()}"
    server = ThreadingHTTPServer(
        ("127.0.0.1", 0), partial(FixtureHandler, directory=str(WEB_ROOT))
    )
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

    def snapshot():
        return json.loads(cli("eval", "({cards:document.querySelectorAll('.video-card').length,"
                              "user:localStorage.getItem('gostream.user'),"
                              "token:localStorage.getItem('gostream.token')})"))

    def expect_guest(label):
        deadline = time.monotonic() + 3
        while True:
            current = snapshot()
            if current == {"cards": 1, "user": None, "token": None}:
                return
            if time.monotonic() >= deadline:
                raise AssertionError(f"{label}: expected cleared guest Feed, got {current}")
            time.sleep(0.05)

    try:
        cli("open", f"http://127.0.0.1:{server.server_port}/")
        cli("eval", "localStorage.setItem('gostream.user','{bad json');"
                    "localStorage.setItem('gostream.token','stale-token')")
        cli("reload")
        expect_guest("invalid JSON")

        cli("eval", "localStorage.setItem('gostream.user',"
                    "JSON.stringify({id:7,nickname:'Valid'}));"
                    "localStorage.setItem('gostream.token','valid-fixture')")
        cli("reload")
        cli("wait", '[data-video="904"]')
        valid = json.loads(cli("eval", "({user:state.user?.nickname,token:state.token,"
                                    "storedToken:localStorage.getItem('gostream.token')})"))
        if valid != {"user": "Valid", "token": "valid-fixture", "storedToken": "valid-fixture"}:
            raise AssertionError(f"valid cached session was not preserved: {valid}")

        # A short desktop viewport must keep the sidebar's auth controls usable.
        # In the broken layout its footer falls below the viewport, so a click
        # can hit the page instead of logging out.
        cli("set", "viewport", "1280", "577")
        cli("click", "#auth-open")
        expect_guest("short desktop logout")
        cli("click", "#auth-open")
        if not json.loads(cli("eval", "document.querySelector('#auth-dialog').matches(':modal')")):
            raise AssertionError("short desktop login did not open a modal")
        cli("click", "#auth-dialog .dialog-close button")
        cli("set", "viewport", "1280", "720")

        cli("eval", "localStorage.setItem('gostream.user',"
                    "JSON.stringify({id:'oops',nickname:'Bad'}));"
                    "localStorage.setItem('gostream.token','stale-token')")
        cli("reload")
        expect_guest("malformed user shape")

        cli("eval", "localStorage.setItem('gostream.user',"
                    "JSON.stringify({id:7,nickname:'Valid'}));"
                    "localStorage.removeItem('gostream.token')")
        cli("reload")
        expect_guest("missing token")
        cli("eval", "localStorage.setItem('gostream.user',"
                    "JSON.stringify({id:7,nickname:'Valid'}));"
                    "localStorage.setItem('gostream.token','')")
        cli("reload")
        expect_guest("empty token")
        cli("eval", "localStorage.removeItem('gostream.user');"
                    "localStorage.setItem('gostream.token','orphan-token')")
        cli("reload")
        expect_guest("missing user")
        print(json.dumps({"browser": "Chromium", "invalid_json": True,
                          "valid_session": True, "malformed_shape": True,
                          "missing_token": True, "empty_token": True,
                          "missing_user": True, "short_desktop_auth": True}))
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
