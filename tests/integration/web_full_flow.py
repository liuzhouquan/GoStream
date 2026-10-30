#!/usr/bin/env python3
"""Component browser workflow across two profiles and a real SQLite/FFmpeg API.

This does not replace final multi-component Compose acceptance.

Exercises two independent Chromium sessions (Alice and Bob) communicating
with a live GoStream server instance on a disposable database. Covers:
1. Registration & login across two accounts
2. Chunk-session upload, real processing and publishing
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
from contextlib import ExitStack
import re
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from urllib.request import urlopen
from im_smoke import stop_test_group

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
        self.cli("close")


def main():
    browser_bin = os.environ.get("AGENT_BROWSER_BIN", "agent-browser")
    go_bin = os.environ.get("GOSTREAM_GO_BIN", "/usr/local/go/bin/go")
    required = re.search(r"^go (\S+)$", (ROOT / "go.mod").read_text(), re.M).group(1)
    actual = run_cmd([go_bin, "env", "GOVERSION"]).stdout.strip()
    if actual != "go" + required:
        raise RuntimeError(f"requires Go {required}; got {actual}")
    ffmpeg = shutil.which(os.environ.get("FFMPEG_BIN", "ffmpeg"))
    ffprobe = shutil.which(os.environ.get("FFPROBE_BIN", "ffprobe"))
    if not ffmpeg or not ffprobe:
        raise RuntimeError("requires real ffmpeg and ffprobe; install them or set FFMPEG_BIN/FFPROBE_BIN")
    with ExitStack() as stack:
        temp_dir = stack.enter_context(tempfile.TemporaryDirectory(prefix="gostream_web_e2e_"))
        db_path, media_dir = os.path.join(temp_dir,"gostream.db"), os.path.join(temp_dir,"media")
        os.makedirs(media_dir)
        server_bin = os.path.join(temp_dir,"gostream_server")
        run_cmd([go_bin,"build","-race","-o",server_bin,"./cmd/gostream"],timeout=120)
        addr = f"127.0.0.1:{find_free_port()}"
        base_url = f"http://{addr}"
        # Explicit environment prevents inherited credentials/shared-service connections.
        env = {"PATH":os.environ.get("PATH",""),"ADDR":addr,"DB_DRIVER":"sqlite",
               "DB_DSN":f"{db_path}?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1",
               "MEDIA_DIR":media_dir,"MEDIA_STORE":"local", "JWT_SECRET":"e2e-test-secret-at-least-32-bytes-long!!",
               "REDIS_ADDR":"","RABBITMQ_URL":"","PPROF_ADDR":"","FFMPEG_BIN":ffmpeg,"FFPROBE_BIN":ffprobe,
               "FEED_FANOUT_MAX_FOLLOWERS":"0"}
        log_path = Path(temp_dir)/"server.log"
        log_file = stack.enter_context(log_path.open("w"))
        def start_api():
            process = subprocess.Popen([server_bin],env=env,cwd=ROOT,stdout=log_file,stderr=subprocess.STDOUT,start_new_session=True)
            return process
        server_proc = start_api()
        def stop_api():
            stop_test_group(server_proc)
            if server_proc.returncode != 0:
                raise RuntimeError(f"API exit={server_proc.returncode}")
        stack.callback(stop_api)
        def wait_ready():
            for _ in range(100):
                if server_proc.poll() is not None:
                    raise RuntimeError("API exited before readiness")
                try:
                    with urlopen(base_url+"/ready",timeout=1) as r:
                        if r.status==200:return
                except OSError:
                    time.sleep(.1)
            raise RuntimeError("API did not become ready")
        wait_ready()
        pid = os.getpid()
        alice = BrowserSession(browser_bin,f"gostream-e2e-alice-{pid}")
        stack.callback(alice.close)
        bob = BrowserSession(browser_bin,f"gostream-e2e-bob-{pid}")
        stack.callback(bob.close)
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
            run_cmd([ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=320x180:r=24", "-t", "8", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", sample_mp4], timeout=60)

            alice.cli("click", '.nav-item[data-view="upload"]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#upload-form'))"))

            alice.cli("upload", '#upload-form input[name="file"]', sample_mp4)
            alice.cli("fill", '#upload-form input[name="title"]', "Alice In Wonderland")
            alice.cli("fill", '#upload-form textarea[name="description"]', "A magical journey")

            # Submit form
            alice.eval_js("document.querySelector('#upload-form').requestSubmit()")
            alice.wait_for(lambda: alice.eval_js("document.querySelectorAll('.video-card').length > 0"))
            video_id = alice.eval_js("document.querySelector('.video-card')?.dataset.video")
            if not video_id:
                raise AssertionError("upload did not produce a video ID")
            alice.wait_for(lambda: alice.eval_js(f"api('/api/videos/{video_id}').then(v=>v.status==='published'&&v.duration_ms>0&&v.width>0&&v.height>0)"), timeout=60)
            results["video_published"] = True
            alice.cli("click", '.nav-item[data-view="profile"]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('[data-refresh-profile]'))"))
            alice.cli("click", '[data-refresh-profile]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('.video-card video'))"))
            alice.eval_js("(()=>{const v=document.querySelector('.video-card video');v.preload='auto';v.load()})()")
            alice.wait_for(lambda: alice.eval_js("(()=>{const v=document.querySelector('.video-card video');return v&&v.readyState>=2&&v.videoWidth>0&&v.duration>0&&!v.error})()"))
            alice.cli("click", f'.video-card[data-video="{video_id}"] [data-play]')
            alice.wait_for(lambda: alice.eval_js("document.querySelector('.video-card video').currentTime>0"))
            results["video_playback"] = True

            # Step 4: Alice Manages Profile & Updates Bio
            alice.cli("click", '.nav-item[data-view="profile"]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#profile-form'))"))
            alice.wait_for(lambda: "Alice In Wonderland" in alice.eval_js("document.querySelector('#content')?.textContent||''"))
            alice.cli("fill", '#profile-form textarea[name="bio"]', "Filmmaker and digital creator")
            alice.eval_js("document.querySelector('#profile-form').requestSubmit()")
            alice.wait_for(lambda: alice.eval_js("api('/api/users/me').then(u=>u.bio==='Filmmaker and digital creator')"))
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
            alice.cli("click", '[data-read-all]')
            alice.wait_for(lambda: alice.eval_js("api('/api/notifications').then(n=>n.unread_count===0)"))
            results["notifications_marked_read"] = True

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
            mobile_rendered = alice.eval_js("window.innerWidth === 390 && window.getComputedStyle(document.querySelector('.sidebar')).position === 'fixed' && document.documentElement.scrollWidth <= window.innerWidth+1")
            alice.cli("click", '.nav-item[data-view="feed"]')
            results["mobile_viewport"] = bool(mobile_rendered)

            # Step 11: Stored Session & Page Reload
            alice.cli("reload")
            alice.wait_for(lambda: "Alice Star" in alice.eval_js("document.querySelector('#session-state')?.textContent||''"))
            results["session_persisted"] = True


            # Verify login and real API restart, retaining this fixture's SQL/media.
            bob.cli("click", "#auth-open")
            bob.cli("click", "#auth-open")
            if bob.eval_js("document.querySelector('#auth-submit').dataset.mode") == "register":
                bob.cli("click", "#auth-switch")
            bob.cli("fill", '#auth-form input[name="username"]', "bob_e2e")
            bob.cli("fill", '#auth-form input[name="password"]', "password123")
            bob.cli("click", "#auth-submit")
            bob.wait_for(lambda: "Bob Builder" in bob.eval_js("document.querySelector('#session-state').textContent"))
            results["login_verified"] = True
            stop_api()
            server_proc = start_api()
            wait_ready()
            alice.cli("reload")
            alice.wait_for(lambda: "Alice Star" in alice.eval_js("document.querySelector('#session-state')?.textContent||''"))
            alice.wait_for(lambda: alice.eval_js(f"api('/api/videos/{video_id}').then(v=>v.status==='published')"))
            alice.cli("click", '.nav-item[data-view="chat"]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#chat-peer'))"))
            alice.cli("fill", "#chat-peer", str(bob_id))
            alice.cli("click", "[data-chat-connect]")
            alice.wait_for(lambda: "Hello Bob! How are you?" in alice.eval_js("document.querySelector('#chat-log')?.textContent||''"))
            results["api_restart_persistence"] = True
        except BaseException:
            print(log_path.read_text()[-16000:],file=sys.stderr)
            raise
        print(json.dumps(results,indent=2))
        if not all(results.values()) or len(results)!=19:
            raise AssertionError("workflow checks incomplete: "+json.dumps(results))


if __name__ == "__main__":
    main()
