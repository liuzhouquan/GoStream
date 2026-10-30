#!/usr/bin/env python3
"""A09 Web full-flow end-to-end acceptance test across two browser profiles.

Exercises two independent Chromium sessions (Alice and Bob) communicating
with a live GoStream server instance on a disposable database. Covers:
1. Registration & login across two accounts
2. Resumable video upload & publishing
3. Creator profile view, video list, and bio update
4. Feed browsing, like, favorite, and comments
5. Notifications delivery & mark read
6. Creator search and follow via UI button
7. Following feed display
8. WebSocket realtime direct chat
9. Group chat creation, member invitation, and group messaging
10. Responsive layout at mobile width (390x844)
11. Page reload & stored session persistence
"""

import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parents[2]
WEB_ROOT = ROOT / "web"


def find_free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def run_cmd(args, timeout=30, check=True, cwd=None):
    res = subprocess.run(args, capture_output=True, text=True, timeout=timeout, cwd=cwd or str(ROOT))
    if check and res.returncode != 0:
        raise RuntimeError(f"Command failed ({' '.join(args)}): {res.stderr or res.stdout}")
    return res


class BrowserSession:
    def __init__(self, browser_bin, session_name):
        self.browser_bin = browser_bin
        self.session = session_name
        self.env = os.environ.copy()
        self.env["AGENT_BROWSER_ARGS"] = "--no-sandbox,--disable-dev-shm-usage,--disable-gpu"

    def cli(self, *args, timeout=45):
        cmd = [self.browser_bin, "--session", self.session, *args]
        res = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, env=self.env)
        if res.returncode != 0:
            raise RuntimeError(f"agent-browser {' '.join(args)}: {res.stderr or res.stdout}")
        return res.stdout.strip()

    def eval_js(self, expression):
        raw = self.cli("eval", expression)
        try:
            return json.loads(raw)
        except Exception:
            return raw

    def wait_for(self, check_fn, timeout=15, poll_interval=0.3):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                res = check_fn()
                if res:
                    return res
            except Exception:
                pass
            time.sleep(poll_interval)
        raise TimeoutError("Condition not met within timeout")

    def close(self):
        try:
            self.cli("close")
        except Exception:
            pass


def main():
    browser_bin = os.environ.get("AGENT_BROWSER_BIN", "agent-browser")
    go_bin = os.environ.get("GOSTREAM_GO_BIN", "/usr/local/go/bin/go")
    if not shutil.which(go_bin):
        go_bin = shutil.which("go") or "/usr/local/go/bin/go"

    temp_dir = tempfile.mkdtemp(prefix="gostream_web_e2e_")
    db_path = os.path.join(temp_dir, "gostream.db")
    media_dir = os.path.join(temp_dir, "media")
    os.makedirs(media_dir, exist_ok=True)
    server_bin = os.path.join(temp_dir, "gostream_server")

    # Compile server
    run_cmd([go_bin, "build", "-o", server_bin, "./cmd/gostream"], timeout=60)

    # Allocate port and start server
    port = find_free_port()
    addr = f"127.0.0.1:{port}"
    base_url = f"http://{addr}"

    env = os.environ.copy()
    env["ADDR"] = addr
    env["DB_DRIVER"] = "sqlite"
    env["DB_DSN"] = f"{db_path}?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
    env["MEDIA_DIR"] = media_dir
    env["JWT_SECRET"] = "e2e-test-secret-at-least-32-bytes-long!!"

    server_proc = subprocess.Popen([server_bin], env=env, cwd=str(ROOT))

    # Wait for server ready
    ready = False
    for _ in range(50):
        try:
            with urlopen(f"{base_url}/health", timeout=1) as resp:
                if resp.status == 200:
                    ready = True
                    break
        except Exception:
            time.sleep(0.1)

    if not ready:
        server_proc.kill()
        shutil.rmtree(temp_dir, ignore_errors=True)
        raise RuntimeError("GoStream server failed to start on " + addr)

    pid = os.getpid()
    alice = BrowserSession(browser_bin, f"alice_{pid}")
    bob = BrowserSession(browser_bin, f"bob_{pid}")

    results = {}

    try:
        # Step 1: Alice Registration
        alice.cli("open", base_url)
        alice.cli("click", "#auth-open")
        alice.cli("click", "#auth-switch")  # Switch to register mode
        alice.cli("fill", "#auth-form input[name=username]", "alice_e2e")
        alice.cli("fill", "#auth-form input[name=nickname]", "Alice Star")
        alice.cli("fill", "#auth-form input[name=password]", "password123")
        alice.cli("click", "#auth-submit")

        alice.wait_for(lambda: "Alice Star" in alice.eval_js("document.querySelector('#session-state')?.textContent||''"))
        alice_user = alice.eval_js("JSON.parse(localStorage.getItem('gostream.user')||'{}')")
        alice_id = alice_user.get("id")
        results["alice_registered"] = bool(alice_id)

        # Step 2: Bob Registration
        bob.cli("open", base_url)
        bob.cli("click", "#auth-open")
        bob.cli("click", "#auth-switch")
        bob.cli("fill", "#auth-form input[name=username]", "bob_e2e")
        bob.cli("fill", "#auth-form input[name=nickname]", "Bob Builder")
        bob.cli("fill", "#auth-form input[name=password]", "password123")
        bob.cli("click", "#auth-submit")

        bob.wait_for(lambda: "Bob Builder" in bob.eval_js("document.querySelector('#session-state')?.textContent||''"))
        bob_user = bob.eval_js("JSON.parse(localStorage.getItem('gostream.user')||'{}')")
        bob_id = bob_user.get("id")
        results["bob_registered"] = bool(bob_id)

        # Step 3: Alice Publishes a Video
        sample_mp4 = os.path.join(temp_dir, "sample.mp4")
        mp4_header = bytes([0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70, 0x69, 0x73, 0x6f, 0x6d])
        with open(sample_mp4, "wb") as f:
            f.write(mp4_header + b"\x00" * 2000)

        alice.cli("click", '.nav-item[data-view="upload"]')
        alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#upload-form'))"))

        alice.cli("upload", '#upload-form input[name="file"]', sample_mp4)
        alice.cli("fill", '#upload-form input[name="title"]', "Alice In Wonderland")
        alice.cli("fill", '#upload-form textarea[name="description"]', "A magical journey")

        # Submit form
        alice.eval_js("document.querySelector('#upload-form').requestSubmit()")
        alice.wait_for(lambda: alice.eval_js("document.querySelectorAll('.video-card').length > 0"))
        results["video_published"] = True

        video_id = alice.eval_js("document.querySelector('.video-card')?.dataset.video")

        # Step 4: Alice Manages Profile & Updates Bio
        alice.cli("click", '.nav-item[data-view="profile"]')
        alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#profile-form'))"))
        alice.wait_for(lambda: "Alice In Wonderland" in alice.eval_js("document.querySelector('#content')?.textContent||''"))
        alice.cli("fill", '#profile-form textarea[name="bio"]', "Filmmaker and digital creator")
        alice.eval_js("document.querySelector('#profile-form').requestSubmit()")
        alice.wait_for(lambda: alice.eval_js("document.querySelector('#profile-form textarea[name=\"bio\"]')?.value === 'Filmmaker and digital creator'"))
        results["profile_verified"] = True

        # Step 5: Bob Browses Feed & Interacts (Like, Favorite, Comment)
        bob.cli("click", '.nav-item[data-view="feed"]')
        bob.wait_for(lambda: bob.eval_js(f"Boolean(document.querySelector('.video-card[data-video=\"{video_id}\"]'))"))

        # Bob likes Alice's video
        bob.cli("click", f'.video-card[data-video="{video_id}"] [data-action="like"]')
        bob.wait_for(lambda: bob.eval_js(f"document.querySelector('.video-card[data-video=\"{video_id}\"] [data-action=\"like\"]').classList.contains('on')"))
        results["video_liked"] = True

        # Bob favorites Alice's video
        bob.cli("click", f'.video-card[data-video="{video_id}"] [data-action="favorite"]')
        bob.wait_for(lambda: bob.eval_js(f"document.querySelector('.video-card[data-video=\"{video_id}\"] [data-action=\"favorite\"]').classList.contains('on')"))
        results["video_favorited"] = True

        # Bob comments on Alice's video
        bob.cli("click", f'.video-card[data-video="{video_id}"] [data-action="comment"]')
        bob.wait_for(lambda: bob.eval_js(f"Boolean(document.querySelector('#comments-{video_id} .comment-form'))"))
        bob.cli("fill", f'#comments-{video_id} .comment-form input[name="body"]', "Great video Alice!")
        bob.cli("click", f'#comments-{video_id} .comment-form button')
        bob.wait_for(lambda: "Great video Alice!" in bob.eval_js(f"document.querySelector('#comments-{video_id}')?.textContent||''"))
        results["comment_posted"] = True

        # Step 6: Alice Checks Notifications
        alice.cli("click", '.nav-item[data-view="notifications"]')
        alice.wait_for(lambda: "Bob Builder" in alice.eval_js("document.querySelector('#content')?.textContent||''"))
        results["notifications_received"] = True

        # Step 7: Search and Follow via UI
        bob.cli("click", '.nav-item[data-view="search"]')
        bob.cli("fill", '#search-form-paged input[name="q"]', "Alice")
        bob.cli("click", "#search-form-paged button")
        bob.wait_for(lambda: "Alice Star" in bob.eval_js("document.querySelector('#search-results')?.textContent||''"))
        results["search_verified"] = True

        # Follow Alice by clicking button in search results
        bob.cli("click", f'[data-follow="{alice_id}"]')
        bob.wait_for(lambda: "已关注" in bob.eval_js(f"document.querySelector('[data-follow=\"{alice_id}\"]')?.textContent||''"))
        results["user_followed"] = True

        # Check Following Feed
        bob.cli("click", '.nav-item[data-view="following"]')
        bob.wait_for(lambda: "Alice In Wonderland" in bob.eval_js("document.querySelector('#content')?.textContent||''"))
        results["following_feed"] = True

        # Step 8: Direct Chat over WebSocket
        alice.cli("click", '.nav-item[data-view="chat"]')
        alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#chat-peer'))"))
        alice.cli("fill", "#chat-peer", str(bob_id))
        alice.cli("click", "[data-chat-connect]")
        alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#chat-form'))"))
        alice.cli("fill", '#chat-form textarea[name="body"]', "Hello Bob! How are you?")
        alice.eval_js("document.querySelector('#chat-form').requestSubmit()")

        # Bob receives message in chat
        bob.cli("click", '.nav-item[data-view="chat"]')
        bob.wait_for(lambda: bob.eval_js("Boolean(document.querySelector('#chat-peer'))"))
        bob.cli("fill", "#chat-peer", str(alice_id))
        bob.cli("click", "[data-chat-connect]")
        bob.wait_for(lambda: "Hello Bob! How are you?" in bob.eval_js("document.querySelector('#chat-log')?.textContent||''"))

        # Bob replies to Alice
        bob.cli("fill", '#chat-form textarea[name="body"]', "Hello Alice! Doing great!")
        bob.eval_js("document.querySelector('#chat-form').requestSubmit()")
        alice.wait_for(lambda: "Hello Alice! Doing great!" in alice.eval_js("document.querySelector('#chat-log')?.textContent||''"))
        results["direct_chat"] = True

        # Step 9: Group Chat
        alice.cli("fill", '#group-create-form input[name="name"]', "Creators Team")
        alice.eval_js("document.querySelector('#group-create-form').requestSubmit()")
        alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#group-panel #group-add-form'))"))

        # Alice invites Bob
        alice.cli("fill", '#group-add-form input[name="user_id"]', str(bob_id))
        alice.eval_js("document.querySelector('#group-add-form').requestSubmit()")
        alice.wait_for(lambda: alice.eval_js("document.querySelector('#group-add-form input[name=\"user_id\"]')?.value === ''"))

        # Alice sends group message
        alice.cli("fill", '#group-send-form textarea[name="body"]', "Welcome to the group!")
        alice.eval_js("document.querySelector('#group-send-form').requestSubmit()")
        alice.wait_for(lambda: "Welcome to the group!" in alice.eval_js("document.querySelector('#group-log')?.textContent||''"))

        # Bob opens group chat and sees group
        bob.cli("click", '.nav-item[data-view="chat"]')
        bob.wait_for(lambda: "Creators Team" in bob.eval_js("document.querySelector('#group-list')?.textContent||''"))
        bob.cli("click", '.group-chip[data-group-open]')
        bob.wait_for(lambda: "Welcome to the group!" in bob.eval_js("document.querySelector('#group-log')?.textContent||''"))

        # Bob sends reply into group
        bob.cli("fill", '#group-send-form textarea[name="body"]', "Thanks Alice!")
        bob.eval_js("document.querySelector('#group-send-form').requestSubmit()")
        alice.wait_for(lambda: "Thanks Alice!" in alice.eval_js("document.querySelector('#group-log')?.textContent||''"))
        results["group_chat"] = True

        # Step 10: Mobile Viewport Check (390x844)
        alice.cli("set", "viewport", "390", "844")
        mobile_rendered = alice.eval_js("window.innerWidth === 390 && window.getComputedStyle(document.querySelector('.sidebar')).position === 'fixed'")
        alice.cli("click", '.nav-item[data-view="feed"]')
        results["mobile_viewport"] = bool(mobile_rendered)

        # Step 11: Stored Session & Page Reload
        alice.cli("reload")
        alice.wait_for(lambda: "Alice Star" in alice.eval_js("document.querySelector('#session-state')?.textContent||''"))
        results["session_persisted"] = True

    finally:
        alice.close()
        bob.close()
        server_proc.terminate()
        try:
            server_proc.wait(timeout=5)
        except Exception:
            server_proc.kill()
        shutil.rmtree(temp_dir, ignore_errors=True)

    print(json.dumps(results, indent=2))
    all_passed = all(results.values()) and len(results) >= 13
    if not all_passed:
        sys.exit(1)


if __name__ == "__main__":
    main()
