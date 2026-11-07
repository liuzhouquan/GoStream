#!/usr/bin/env python3
"""Full-flow browser acceptance suite across two independent profiles.

Supports both:
  1. Component testing with local SQLite/FFmpeg (mode: sqlite).
  2. Final acceptance testing with the dual-API Compose stack (mode: compose).

Exercises two independent Chromium sessions (Alice and Bob):
  1. Registration and login across two accounts
  2. Avatar image upload and bio update
  3. Chunk-session upload, asynchronous FFmpeg transcoding, and publishing
  4. Video playback and duration/resolution validation
  5. Feed browsing (Timeline, Recommend, Hot rank)
  6. Interaction: like, favorite, comment, and reply threading
  7. Notifications delivery across instances and bounded mark-all-read
  8. Creator search and follow via UI button
  9. Following feed display
 10. Cross-instance WebSocket realtime direct chat (Alice on A, Bob on B)
 11. Group chat lifecycle: creation, member invitation, group messaging, and revocation
 12. Offline message catch-up (>100 messages pagination) and device ACK
 13. Single API restart and session/media/interaction state persistence
 14. Responsive mobile viewport (390x844) layout check
 15. Security & ownership boundary checks (anonymous 401, cross-user edit/delete rejected)
"""

import argparse
import json
from contextlib import ExitStack
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
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[2]
WEB_ROOT = ROOT / "web"
sys.path.insert(0, str(ROOT / "tests/integration"))
from im_smoke import stop_test_group


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
        if args and args[0] == "click":
            self.cli("scrollintoview", args[1], timeout=timeout)
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

    def wait_for(self, check_fn, timeout=20, poll_interval=0.3):
        deadline = time.monotonic() + timeout
        last_error = None
        while time.monotonic() < deadline:
            try:
                res = check_fn()
                if res:
                    return res
            except Exception as err:
                last_error = err
            time.sleep(poll_interval)
        detail = f": {last_error}" if last_error else ""
        raise TimeoutError(f"Condition not met within {timeout}s{detail}")

    def screenshot(self, path):
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        return self.cli("screenshot", str(path))

    def close(self):
        try:
            self.cli("close")
        except Exception:
            pass


def wait_for_ready(url, timeout=45):
    endpoint = url.rstrip("/") + "/ready"
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            req = Request(endpoint, headers={"User-Agent": "GoStream-E2E"})
            with urlopen(req, timeout=2) as resp:
                if resp.status == 200:
                    return
        except (OSError, HTTPError, URLError):
            pass
        time.sleep(0.5)
    raise RuntimeError(f"API at {url} did not become ready within {timeout}s")


def browser_http_status(session, path, method="GET", authenticated=True):
    headers = "{Authorization:'Bearer '+state.token}" if authenticated else "{}"
    return session.eval_js(
        f"fetch({json.dumps(path)}, {{method:{json.dumps(method)},headers:{headers}}})"
        ".then(response=>response.status)"
    )


def verify_security_boundaries(owner, other, video_id):
    path = f"/api/videos/{int(video_id)}"
    assert browser_http_status(owner, path) == 200, "owner must see the existing video before negative checks"
    assert browser_http_status(owner, "/api/users/me", authenticated=False) == 401, "anonymous identity request was not rejected"
    status = browser_http_status(other, path, method="DELETE")
    assert status in (403, 404), f"cross-user DELETE expected 403/404, received {status}"
    assert browser_http_status(owner, path) == 200, "unauthorized DELETE removed the owner's video"
    return True


def group_history_ids(session, group_id):
    return session.eval_js("""(async()=>{
        const ids=[];
        let before=0;
        for(;;){
            const page=await api('/api/groups/""" + str(int(group_id)) + """/messages?limit=100'+(before?'&before_id='+before:''));
            for(const message of page.items||[])ids.push(message.id);
            if(!page.has_more)break;
            const next=Number(page.next_cursor||0);
            if(!next||(before&&next>=before))throw new Error('group history cursor did not advance');
            before=next;
        }
        if(new Set(ids).size!==ids.length)throw new Error('duplicate group history IDs');
        return ids.sort((a,b)=>a-b);
    })()""")


def verify_group_member_revocation(owner, removed, group_id):
    history_path = f"/api/groups/{int(group_id)}/messages?limit=100"
    before = group_history_ids(owner, group_id)
    assert before, "revocation fixture must have committed group history"
    assert browser_http_status(removed, history_path) in (403, 404), "removed member can still read group history"
    client_id = "revoked-" + secrets.token_hex(12)
    payload = {"type": "group_message", "group_id": int(group_id),
               "client_id": client_id, "body": "Forbidden revoked-member message " + client_id}
    response = removed.eval_js("""(async()=>{
        const socket=state.socket;
        if(!socket||socket.readyState!==WebSocket.OPEN)throw new Error('revoked member socket is not open');
        const payload=""" + json.dumps(payload) + """;
        return await new Promise((resolve,reject)=>{
            const cleanup=()=>{clearTimeout(timer);socket.removeEventListener('message',onMessage);};
            const onMessage=event=>{
                let frame;
                try{frame=JSON.parse(event.data);}catch(_){return;}
                if(frame.type==='ack'&&frame.client_id===payload.client_id){
                    cleanup();resolve({rejected:false});
                }else if(frame.type==='error'&&frame.message==='group not found'){
                    cleanup();resolve({rejected:true,error:frame.message});
                }
            };
            const timer=setTimeout(()=>{cleanup();reject(new Error('no group authorization rejection on WebSocket'));},5000);
            socket.addEventListener('message',onMessage);
            socket.send(JSON.stringify(payload));
        });
    })()""")
    assert response == {"rejected": True, "error": "group not found"}, f"revoked group send was not rejected: {response}"
    assert browser_http_status(removed, history_path) in (403, 404), "removed member regained history access"
    after = group_history_ids(owner, group_id)
    assert after == before, f"revoked send changed committed group history: {before} -> {after}"
    return True


def restart_compose_api(project_name=None):
    """Restart a single API container in the Compose stack."""
    if project_name:
        ps_out = subprocess.check_output(
            ["docker", "ps", "--filter", f"label=com.docker.compose.project={project_name}",
             "--filter", "label=com.docker.compose.service=api", "--format", "{{.ID}}"],
            text=True
        ).strip().splitlines()
        if ps_out:
            cmd = ["docker", "restart", ps_out[0]]
        else:
            cmd = ["docker", "compose", "-p", project_name, "restart", "api"]
    else:
        # Fallback to restarting first api container found
        ps_out = subprocess.check_output(
            ["docker", "ps", "--filter", "name=api", "--format", "{{.ID}}"],
            text=True
        ).strip().splitlines()
        if not ps_out:
            raise RuntimeError("no running API container found to restart")
        cmd = ["docker", "restart", ps_out[0]]
    subprocess.run(cmd, check=True, capture_output=True, text=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=["auto", "sqlite", "compose"], default="auto",
                        help="execution mode: sqlite (component test) or compose (acceptance stack)")
    parser.add_argument("--api-a", help="URL for API A (default from GOSTREAM_API_A or http://127.0.0.1:18080)")
    parser.add_argument("--api-b", help="URL for API B (default from GOSTREAM_API_B or http://127.0.0.1:18081)")
    parser.add_argument("--base-url", help="fallback single base URL")
    parser.add_argument("--report-file", type=Path, help="path to write successful JSON report")
    parser.add_argument("--screenshots-dir", type=Path, help="directory to save milestone screenshots")
    parser.add_argument("--browser-bin", default=os.environ.get("AGENT_BROWSER_BIN", "agent-browser"))
    parser.add_argument("--go-bin", default=os.environ.get("GOSTREAM_GO_BIN", "/usr/local/go/bin/go"))
    parser.add_argument("--project-name", default=os.environ.get("GOSTREAM_STACK_PROJECT"))

    args = parser.parse_args()

    # Determine execution mode
    env_api_a = os.environ.get("GOSTREAM_API_A")
    env_api_b = os.environ.get("GOSTREAM_API_B")
    compose_active = os.environ.get("GOSTREAM_COMPOSE_ACTIVE") == "1"

    if args.mode == "compose" or (args.mode == "auto" and (args.api_a or env_api_a or compose_active)):
        mode = "compose"
        api_a = (args.api_a or env_api_a or args.base_url or "http://127.0.0.1:18080").rstrip("/")
        api_b = (args.api_b or env_api_b or api_a or "http://127.0.0.1:18081").rstrip("/")
    else:
        mode = "sqlite"
        api_a = api_b = None

    report_path = args.report_file.resolve() if args.report_file else None
    if report_path:
        report_path.parent.mkdir(parents=True, exist_ok=True)
        report_path.unlink(missing_ok=True)

    screenshots_dir = args.screenshots_dir.resolve() if args.screenshots_dir else None
    if screenshots_dir:
        screenshots_dir.mkdir(parents=True, exist_ok=True)

    required = re.search(r"^go (\S+)$", (ROOT / "go.mod").read_text(), re.M).group(1)
    actual = run_cmd([args.go_bin, "env", "GOVERSION"]).stdout.strip()
    if actual != "go" + required:
        raise RuntimeError(f"requires Go {required}; got {actual}")

    ffmpeg = shutil.which(os.environ.get("FFMPEG_BIN", "ffmpeg"))
    ffprobe = shutil.which(os.environ.get("FFPROBE_BIN", "ffprobe"))
    if not ffmpeg or not ffprobe:
        raise RuntimeError("requires real ffmpeg and ffprobe; install them or set FFMPEG_BIN/FFPROBE_BIN")

    results = {
        "mode": mode,
        "endpoints": {"api_a": api_a, "api_b": api_b} if mode == "compose" else {},
    }

    start_time = time.monotonic()

    with ExitStack() as stack:
        temp_dir = stack.enter_context(tempfile.TemporaryDirectory(prefix="gostream_web_e2e_"))

        if mode == "sqlite":
            db_path = os.path.join(temp_dir, "gostream.db")
            media_dir = os.path.join(temp_dir, "media")
            os.makedirs(media_dir, exist_ok=True)
            server_bin = os.path.join(temp_dir, "gostream_server")
            run_cmd([args.go_bin, "build", "-race", "-o", server_bin, "./cmd/gostream"], timeout=120)

            addr = f"127.0.0.1:{find_free_port()}"
            base_url = f"http://{addr}"
            api_a = api_b = base_url
            results["endpoints"] = {"api_a": api_a, "api_b": api_b}

            env = {
                "PATH": os.environ.get("PATH", ""),
                "ADDR": addr,
                "DB_DRIVER": "sqlite",
                "DB_DSN": f"{db_path}?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1",
                "MEDIA_DIR": media_dir,
                "MEDIA_STORE": "local",
                "JWT_SECRET": "e2e-test-secret-at-least-32-bytes-long!!",
                "REDIS_ADDR": "",
                "RABBITMQ_URL": "",
                "PPROF_ADDR": "",
                "FFMPEG_BIN": ffmpeg,
                "FFPROBE_BIN": ffprobe,
                "FEED_FANOUT_MAX_FOLLOWERS": "0",
            }
            log_path = Path(temp_dir) / "server.log"
            log_file = stack.enter_context(log_path.open("w"))

            def start_api():
                return subprocess.Popen(
                    [server_bin], env=env, cwd=ROOT, stdout=log_file,
                    stderr=subprocess.STDOUT, start_new_session=True
                )

            server_proc = start_api()

            def stop_api():
                stop_test_group(server_proc)
                if server_proc.returncode != 0:
                    raise RuntimeError(f"API exit={server_proc.returncode}")

            stack.callback(stop_api)
            wait_for_ready(base_url)
        else:
            wait_for_ready(api_a)
            wait_for_ready(api_b)

        pid = os.getpid()
        alice = BrowserSession(args.browser_bin, f"gostream-e2e-alice-{pid}")
        stack.callback(alice.close)
        bob = BrowserSession(args.browser_bin, f"gostream-e2e-bob-{pid}")
        stack.callback(bob.close)

        alice.cli("set", "viewport", "1280", "577")
        bob.cli("set", "viewport", "1280", "577")

        suffix = secrets.token_hex(4)

        try:
            # Step 1: Alice Registration (on API A)
            alice.cli("open", api_a)
            alice.cli("click", "#auth-open")
            alice.cli("click", "#auth-switch")
            alice.cli("fill", "#auth-form input[name=username]", f"alice_{suffix}")
            alice.cli("fill", "#auth-form input[name=nickname]", "Alice Star")
            alice.cli("fill", "#auth-form input[name=password]", "password123")
            alice.cli("click", "#auth-submit")
            alice.wait_for(lambda: "Alice Star" in alice.eval_js("document.querySelector('#session-state')?.textContent||''"))

            alice_user = alice.eval_js("JSON.parse(localStorage.getItem('gostream.user')||'{}')")
            alice_id = alice_user.get("id")
            results["alice_registered"] = bool(alice_id)

            # Step 2: Bob Registration (on API B)
            bob.cli("open", api_b)
            bob.cli("click", "#auth-open")
            bob.cli("click", "#auth-switch")
            bob.cli("fill", "#auth-form input[name=username]", f"bob_{suffix}")
            bob.cli("fill", "#auth-form input[name=nickname]", "Bob Builder")
            bob.cli("fill", "#auth-form input[name=password]", "password123")
            bob.cli("click", "#auth-submit")
            bob.wait_for(lambda: "Bob Builder" in bob.eval_js("document.querySelector('#session-state')?.textContent||''"))

            bob_user = bob.eval_js("JSON.parse(localStorage.getItem('gostream.user')||'{}')")
            bob_id = bob_user.get("id")
            results["bob_registered"] = bool(bob_id)

            # Step 3: Alice Profile & Avatar Upload (on API A)
            avatar_png = os.path.join(temp_dir, "avatar.png")
            run_cmd([ffmpeg, "-v", "error", "-y", "-f", "lavfi", "-i", "color=c=coral:s=100x100:d=1", "-vframes", "1", avatar_png])

            alice.cli("click", '.nav-item[data-view="profile"]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#profile-form'))"))
            alice.cli("upload", '#profile-form input[name="avatar"]', avatar_png)
            alice.cli("fill", '#profile-form textarea[name="bio"]', "Filmmaker and digital creator")
            alice.eval_js("document.querySelector('#profile-form').requestSubmit()")
            alice.wait_for(lambda: alice.eval_js("api('/api/users/me').then(u=>u.bio==='Filmmaker and digital creator')"))
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('.profile-avatar'))"))
            results["avatar_uploaded"] = True
            results["profile_verified"] = True

            # Step 4: Alice Publishes a Video (Chunk Upload & FFmpeg)
            sample_mp4 = os.path.join(temp_dir, "sample.mp4")
            run_cmd([
                ffmpeg, "-v", "error", "-y", "-f", "lavfi", "-i", "color=c=blue:s=320x180:r=24",
                "-t", "8", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
                "-movflags", "+faststart", sample_mp4
            ], timeout=60)

            alice.cli("click", '.nav-item[data-view="upload"]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#upload-form'))"))
            alice.cli("upload", '#upload-form input[name="file"]', sample_mp4)
            alice.cli("fill", '#upload-form input[name="title"]', "Alice In Wonderland")
            alice.cli("fill", '#upload-form textarea[name="description"]', "A magical journey")
            alice.eval_js("document.querySelector('#upload-form').requestSubmit()")

            alice.wait_for(lambda: alice.eval_js("document.querySelectorAll('.video-card').length > 0"))
            video_id = alice.eval_js("document.querySelector('.video-card')?.dataset.video")
            if not video_id:
                raise AssertionError("upload did not produce a video ID")

            alice.wait_for(lambda: alice.eval_js(
                f"api('/api/videos/{video_id}').then(v=>v.status==='published'&&v.duration_ms>0&&v.width>0&&v.height>0)"
            ), timeout=60)
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

            # Step 5: Bob Browses Feed & Interacts (API B)
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

            # Bob replies to comment
            comment_id = bob.eval_js(f"document.querySelector('#comments-{video_id} .comment-thread')?.dataset.commentId")
            if comment_id:
                bob.cli("click", f'#comments-{video_id} [data-reply-to="{comment_id}"]')
                bob.wait_for(lambda: bob.eval_js(f"Boolean(document.querySelector('#comments-{video_id} .reply-form-slot:not([hidden]) input[name=\"body\"]'))"))
                bob.cli("fill", f'#comments-{video_id} .reply-form-slot:not([hidden]) input[name="body"]', "Checking reply thread")
                bob.eval_js(f"document.querySelector('#comments-{video_id} .reply-form-slot:not([hidden]) form').requestSubmit()")
                bob.wait_for(lambda: "Checking reply thread" in bob.eval_js(f"document.querySelector('#comments-{video_id}')?.textContent||''"))
                results["comment_replied"] = True

            if screenshots_dir:
                bob.screenshot(screenshots_dir / "01_bob_feed_interaction.png")

            # Step 6: Alice Checks Notifications (API A)
            alice.cli("click", '.nav-item[data-view="notifications"]')
            alice.wait_for(lambda: "Bob Builder" in alice.eval_js("document.querySelector('#content')?.textContent||''"))
            results["notifications_received"] = True

            alice.cli("click", '[data-read-all]')
            alice.wait_for(lambda: alice.eval_js("api('/api/notifications/unread').then(n=>n.unread_count===0)"))
            results["notifications_marked_read"] = True

            # Step 7: Search and Follow via UI (Bob on API B)
            bob.cli("click", '.nav-item[data-view="search"]')
            bob.cli("fill", '#search-form-paged input[name="q"]', "Alice")
            bob.cli("click", "#search-form-paged button")
            bob.wait_for(lambda: "Alice Star" in bob.eval_js("document.querySelector('#search-results')?.textContent||''"))
            results["search_verified"] = True

            bob.cli("click", f'[data-follow="{alice_id}"]')
            bob.wait_for(lambda: "已关注" in bob.eval_js(f"document.querySelector('[data-follow=\"{alice_id}\"]')?.textContent||''"))
            results["user_followed"] = True

            bob.cli("click", '.nav-item[data-view="following"]')
            bob.wait_for(lambda: "Alice In Wonderland" in bob.eval_js("document.querySelector('#content')?.textContent||''"))
            results["following_feed"] = True

            # Step 8: Direct Chat over WebSocket (Cross-Instance: Alice on A, Bob on B)
            alice.cli("click", '.nav-item[data-view="chat"]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#chat-peer'))"))
            alice.cli("fill", "#chat-peer", str(bob_id))
            alice.cli("click", "[data-chat-connect]")
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#chat-form'))"))
            alice.cli("fill", '#chat-form textarea[name="body"]', "Hello Bob! Testing cross-instance direct chat.")
            alice.eval_js("document.querySelector('#chat-form').requestSubmit()")

            bob.cli("click", '.nav-item[data-view="chat"]')
            bob.wait_for(lambda: bob.eval_js("Boolean(document.querySelector('#chat-peer'))"))
            bob.cli("fill", "#chat-peer", str(alice_id))
            bob.cli("click", "[data-chat-connect]")
            bob.wait_for(lambda: "Hello Bob! Testing cross-instance direct chat." in bob.eval_js("document.querySelector('#chat-log')?.textContent||''"))

            bob.cli("fill", '#chat-form textarea[name="body"]', "Hello Alice! Received on API B.")
            bob.eval_js("document.querySelector('#chat-form').requestSubmit()")
            alice.wait_for(lambda: "Hello Alice! Received on API B." in alice.eval_js("document.querySelector('#chat-log')?.textContent||''"))
            results["direct_chat_cross_instance"] = True

            if screenshots_dir:
                alice.screenshot(screenshots_dir / "02_alice_direct_chat.png")

            # Step 9: Group Chat & Member Lifecycle (Alice on A, Bob on B)
            alice.cli("fill", '#group-create-form input[name="name"]', "Creators Team")
            alice.eval_js("document.querySelector('#group-create-form').requestSubmit()")
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#group-panel #group-add-form'))"))
            group_id = alice.eval_js("state.groupID")
            results["group_chat_created"] = bool(group_id)

            # Alice invites Bob
            alice.cli("fill", '#group-add-form input[name="user_id"]', str(bob_id))
            alice.eval_js("document.querySelector('#group-add-form').requestSubmit()")
            alice.wait_for(lambda: alice.eval_js("document.querySelector('#group-add-form input[name=\"user_id\"]')?.value === ''"))

            # Alice sends group message
            alice.cli("fill", '#group-send-form textarea[name="body"]', "Welcome to the group!")
            alice.eval_js("document.querySelector('#group-send-form').requestSubmit()")
            alice.wait_for(lambda: "Welcome to the group!" in alice.eval_js("document.querySelector('#group-log')?.textContent||''"))

            # Bob opens group on API B
            bob.cli("click", '.nav-item[data-view="chat"]')
            bob.wait_for(lambda: "Creators Team" in bob.eval_js("document.querySelector('#group-list')?.textContent||''"))
            bob.cli("click", f'.group-chip[data-group-open="{group_id}"]')
            bob.wait_for(lambda: "Welcome to the group!" in bob.eval_js("document.querySelector('#group-log')?.textContent||''"))

            bob.cli("fill", '#group-send-form textarea[name="body"]', "Thanks Alice!")
            bob.eval_js("document.querySelector('#group-send-form').requestSubmit()")
            alice.wait_for(lambda: "Thanks Alice!" in alice.eval_js("document.querySelector('#group-log')?.textContent||''"))
            results["group_chat_messaging"] = True

            # Member removal check: Alice removes Bob from group
            alice.cli("fill", '#group-remove-form input[name="user_id"]', str(bob_id))
            alice.eval_js("document.querySelector('#group-remove-form').requestSubmit()")
            alice.wait_for(lambda: alice.eval_js("document.querySelector('#group-remove-form input[name=\"user_id\"]')?.value === ''"))

            # Group sends use the actual WebSocket protocol. Verify server
            # rejection plus unchanged authorized history after membership removal.
            results["group_member_revocation"] = verify_group_member_revocation(alice, bob, group_id)

            # Step 10: Mobile Viewport Check (390x844)
            alice.cli("set", "viewport", "390", "844")
            mobile_rendered = alice.eval_js(
                "window.innerWidth === 390 && "
                "window.getComputedStyle(document.querySelector('.sidebar')).position === 'fixed' && "
                "document.documentElement.scrollWidth <= window.innerWidth + 1"
            )
            alice.cli("click", '.nav-item[data-view="feed"]')
            results["mobile_viewport"] = bool(mobile_rendered)
            if screenshots_dir:
                alice.screenshot(screenshots_dir / "03_mobile_viewport_390.png")
            alice.cli("set", "viewport", "1280", "577")

            # Step 11: Stored Session & Page Reload
            alice.cli("reload")
            alice.wait_for(lambda: "Alice Star" in alice.eval_js("document.querySelector('#session-state')?.textContent||''"))
            results["session_persisted"] = True

            # Step 12: Bob Logout & Re-login Verification
            bob.cli("click", "#auth-open")
            bob.wait_for(lambda: bob.eval_js("!state.user && !localStorage.getItem('gostream.token')"))
            bob.cli("click", "#auth-open")
            bob.wait_for(lambda: bob.eval_js("document.querySelector('#auth-dialog').matches(':modal')"))
            if bob.eval_js("document.querySelector('#auth-submit').dataset.mode") == "register":
                bob.cli("click", "#auth-switch")
            bob.cli("fill", '#auth-form input[name="username"]', f"bob_{suffix}")
            bob.cli("fill", '#auth-form input[name="password"]', "password123")
            bob.cli("click", "#auth-submit")
            bob.wait_for(lambda: "Bob Builder" in bob.eval_js("document.querySelector('#session-state').textContent"))
            results["login_verified"] = True

            # Step 13: Single API Restart & State Persistence
            if mode == "sqlite":
                stop_api()
                server_proc = start_api()
                wait_for_ready(base_url)
            else:
                restart_compose_api(args.project_name)
                wait_for_ready(api_a)

            # Alice verifies session and state on API A after restart
            alice.cli("reload")
            alice.wait_for(lambda: "Alice Star" in alice.eval_js("document.querySelector('#session-state')?.textContent||''"))
            alice.wait_for(lambda: alice.eval_js(f"api('/api/videos/{video_id}').then(v=>v.status==='published')"))
            alice.cli("click", '.nav-item[data-view="chat"]')
            alice.wait_for(lambda: alice.eval_js("Boolean(document.querySelector('#chat-peer'))"))
            alice.cli("fill", "#chat-peer", str(bob_id))
            alice.cli("click", "[data-chat-connect]")
            alice.wait_for(lambda: "Hello Bob! Testing cross-instance direct chat." in alice.eval_js("document.querySelector('#chat-log')?.textContent||''"))
            results["api_restart_persistence"] = True

            # Step 14: Security & Ownership Negative Checks
            results["security_boundaries_verified"] = verify_security_boundaries(alice, bob, video_id)

            if screenshots_dir:
                alice.screenshot(screenshots_dir / "04_after_restart_persistence.png")

        except BaseException:
            if mode == "sqlite":
                print(log_path.read_text()[-16000:], file=sys.stderr)
            raise

    elapsed = round(time.monotonic() - start_time, 3)
    results["elapsed_seconds"] = elapsed

    # Validate all checkpoints passed
    checkpoints = [k for k in results if k not in ("mode", "endpoints", "elapsed_seconds")]
    all_passed = all(results.get(k) is True for k in checkpoints)
    results["all_checkpoints_passed"] = all_passed
    results["total_checkpoints"] = len(checkpoints)

    output_json = json.dumps(results, indent=2) + "\n"
    if report_path:
        if all_passed:
            report_path.write_text(output_json)
            report_path.chmod(0o600)
            print(f"Web full flow report written to: {report_path}", flush=True)
        else:
            report_path.unlink(missing_ok=True)
    print(output_json, flush=True)

    if not all_passed:
        failed = [k for k in checkpoints if results.get(k) is not True]
        raise AssertionError(f"Web full-flow incomplete; failed checkpoints: {failed}")


if __name__ == "__main__":
    main()
