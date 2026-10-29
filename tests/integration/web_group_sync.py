#!/usr/bin/env python3
"""Exercise web group backlog paging, gap sync and cumulative ACK in an isolated Chromium session.

Requires the agent-browser CLI. The HTTP server supplies fixture API responses;
no GoStream database or running demo instance is touched.
"""

import json
import os
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import re
import subprocess
import sys
import threading
import time
from urllib.parse import parse_qs, urlsplit


ROOT = Path(__file__).resolve().parents[2]
WEB_ROOT = Path(os.environ.get("GOSTREAM_WEB_ROOT", ROOT / "web")).resolve()


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
        parsed = urlsplit(self.path)
        path = parsed.path
        query = parse_qs(parsed.query)

        if path == "/api/feed":
            self.json_response(200, {"scene": "recommend", "items": [], "has_more": False})
            return
        if path == "/api/groups":
            self.json_response(
                200,
                {
                    "items": [
                        {"id": 1, "name": "Backlog Group", "owner_id": 42},
                        {"id": 2, "name": "Second Group", "owner_id": 42},
                    ]
                },
            )
            return
        if path == "/api/groups/1/unread":
            self.json_response(200, {"unread_count": 105, "last_ack_id": 0})
            return
        if path == "/api/groups/2/unread":
            self.json_response(200, {"unread_count": 5, "last_ack_id": 0})
            return
        if path == "/api/groups/1/messages":
            if self.server.delay_group_1:
                time.sleep(0.4)
            if self.server.fail_group_1:
                self.json_response(503, {"error": "injected history outage"})
                return
            before = int(query.get("before_id", ["0"])[0])
            top = min(self.server.group_1_max_id, before - 1) if before else self.server.group_1_max_id
            low = max(1, top - 99)
            items = [{"id": i, "group_id": 1, "sender_id": 99, "body": f"msg {i}",
                      "created_at": "2026-10-29T10:00:00Z"} for i in range(top, low - 1, -1)]
            self.json_response(200, {"items": items, "next_cursor": low if items else 0, "has_more": low > 1})
            return
        if path == "/api/groups/2/messages":
            items = [
                {
                    "id": i,
                    "group_id": 2,
                    "sender_id": 99,
                    "body": f"group2 msg {i}",
                    "created_at": "2026-10-29T10:00:00Z",
                }
                for i in range(5, 0, -1)
            ]
            self.json_response(200, {"items": items, "next_cursor": 1, "has_more": False})
            return
        super().do_GET()

    def do_POST(self):
        parsed = urlsplit(self.path)
        path = parsed.path
        ack_match = re.match(r"^/api/groups/(\d+)/messages/(\d+)/ack$", path)
        if ack_match:
            group_id = int(ack_match.group(1))
            msg_id = int(ack_match.group(2))
            with self.server.lock:
                self.server.acks.append((group_id, msg_id))
            if group_id == 1 and self.server.delay_ack:
                time.sleep(0.8)
            self.json_response(200, {"acked": True, "last_ack_id": msg_id})
            return
        self.json_response(404, {"error": "not found"})


def main():
    browser = os.environ.get("AGENT_BROWSER_BIN", "agent-browser")
    session = f"gostream-groupsync-{os.getpid()}"
    server = ThreadingHTTPServer(
        ("127.0.0.1", 0), partial(FixtureHandler, directory=str(WEB_ROOT))
    )
    server.acks = []
    server.lock = threading.Lock()
    server.delay_group_1 = False
    server.group_1_max_id = 105
    server.fail_group_1 = False
    server.delay_ack = False
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

    try:
        cli("open", f"http://127.0.0.1:{server.server_port}/")
        cli(
            "eval",
            "localStorage.setItem('gostream.token','fixture-token');"
            "localStorage.setItem('gostream.user',JSON.stringify({id:42,nickname:'Fixture'}))",
        )
        cli("reload")
        cli("wait", "#page-title")

        # Navigate to chat view
        cli("eval", "activate('chat')")
        cli("wait", '[data-group-open="1"]')

        # Case 1: Open group 1 with 105 unread messages (backlog > 100)
        cli("eval", "document.querySelector('[data-group-open=\"1\"]').click()")
        cli("wait", "#group-log")

        # Let any immediate async calls settle
        time.sleep(0.3)

        # Check: Before paging, only 100 messages are loaded (IDs 6..105).
        # Messages 1..5 have NOT been displayed.
        # Cumulative ACK for 105 MUST NOT have been sent!
        with server.lock:
            initial_acks = list(server.acks)
        if (1, 105) in initial_acks:
            raise AssertionError(
                f"Defect reproduced: premature ACK for message 105 before paging unread backlog! acks={initial_acks}"
            )

        # Check: #group-more button must be present and visible
        more_state = json.loads(
            cli(
                "eval",
                "(()=>{const b=document.querySelector('#group-more');"
                "return {exists:Boolean(b),hidden:b?.hidden||false,disabled:b?.disabled||false}})()",
            )
        )
        if not more_state.get("exists") or more_state.get("hidden"):
            raise AssertionError(f"Expected visible #group-more button, got {more_state}")

        msg_count = json.loads(
            cli("eval", "document.querySelectorAll('#group-log .message-row').length")
        )
        if msg_count != 100:
            raise AssertionError(f"Expected 100 initial messages, got {msg_count}")

        # Now click #group-more to load the remaining 5 messages (IDs 1..5)
        cli("eval", "document.querySelector('#group-more').click()")

        # Wait for all 105 messages to be loaded
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            count = json.loads(
                cli("eval", "document.querySelectorAll('#group-log .message-row').length")
            )
            if count == 105:
                break
            time.sleep(0.05)
        else:
            raise AssertionError(f"Expected 105 messages after loading older history, got {count}")

        # #group-more should now be hidden since has_more is False
        more_hidden = json.loads(
            cli("eval", "Boolean(document.querySelector('#group-more')?.hidden)")
        )
        if not more_hidden:
            raise AssertionError("Expected #group-more to be hidden after exhausting history")

        # NOW that the backlog is completely loaded, cumulative ACK for 105 must have been sent!
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            with server.lock:
                if (1, 105) in server.acks:
                    break
            time.sleep(0.05)
        else:
            with server.lock:
                acks_now = list(server.acks)
            raise AssertionError(
                f"Expected cumulative ACK for 105 after gap was closed, but acks={acks_now}"
            )

        # Case 2: Out-of-order live message and gap sync
        # Group 1 is at 105. Live message 108 arrives (jumping over 106 and 107).
        server.group_1_max_id = 108
        with server.lock:
            server.acks.clear()

        cli(
            "eval",
            "appendGroupMessage({id:108,group_id:1,sender_id:99,body:'msg 108',created_at:new Date().toISOString()})",
        )

        # After sync resolves the gap (fetching 106, 107, 108), ACK for 108 should be sent
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            with server.lock:
                if (1, 108) in server.acks:
                    break
            time.sleep(0.05)
        else:
            with server.lock:
                acks_now = list(server.acks)
            raise AssertionError(f"Expected ACK for 108 after gap sync, but acks={acks_now}")

        # Check all messages 106, 107, 108 are present in order
        messages_106_108 = json.loads(
            cli(
                "eval",
                "(()=>{const rows=[...document.querySelectorAll('#group-log .message-row[data-message-id]')];"
                "return rows.map(r=>Number(r.dataset.messageId)).filter(id=>id>=106)})()",
            )
        )
        if messages_106_108 != [106, 107, 108]:
            raise AssertionError(
                f"Expected [106, 107, 108] rendered in order, got {messages_106_108}"
            )

        # A live hint after 242 missed rows must fetch more than one page.
        server.group_1_max_id = 350
        cli("eval", "appendGroupMessage({id:350,group_id:1,sender_id:99,body:'msg 350'})")
        deadline = time.monotonic() + 6
        while time.monotonic() < deadline:
            with server.lock:
                if (1, 350) in server.acks:
                    break
            time.sleep(0.05)
        else:
            raise AssertionError("multi-page live synchronization did not ACK after filling gap")
        ids = json.loads(cli("eval", "[...document.querySelectorAll('#group-log [data-message-id]')].map(r=>Number(r.dataset.messageId))"))
        if ids != list(range(1,351)):
            raise AssertionError(f"multi-page live sync skipped rows; rendered={len(ids)}")

        # Failed synchronization must leave the verified/ACK boundary at 350.
        server.fail_group_1 = True
        server.group_1_max_id = 353
        cli("eval", "appendGroupMessage({id:353,group_id:1,sender_id:99,body:'msg 353'})")
        time.sleep(0.4)
        with server.lock:
            if any(group==1 and message>350 for group,message in server.acks):
                raise AssertionError("failed history request advanced cumulative ACK")
        server.fail_group_1 = False
        cli("eval", "void syncGroupMessages('1')")
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            with server.lock:
                if (1,353) in server.acks:
                    break
            time.sleep(0.05)
        else:
            raise AssertionError("sync did not recover after failed history request")

        # Case 3: Group switching race condition
        # When switching to Group 2 while Group 1 load is delayed, Group 1's late response
        # must not overwrite Group 2 or send ACKs for Group 1.
        server.delay_group_1 = True
        with server.lock:
            server.acks.clear()

        # Click Group 1, then immediately click Group 2
        cli("eval", "document.querySelector('[data-group-open=\"1\"]').click()")
        cli("eval", "document.querySelector('[data-group-open=\"2\"]').click()")

        # Wait for Group 2 to be rendered
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            panel_title = json.loads(
                cli("eval", "document.querySelector('#group-panel h3')?.textContent")
            )
            if panel_title == "群聊 #2":
                break
            time.sleep(0.05)
        else:
            raise AssertionError(f"Expected Group 2 panel, got {panel_title}")

        # Wait past the delayed group 1 response
        time.sleep(0.6)

        # Panel must still be Group 2
        final_panel_title = json.loads(
            cli("eval", "document.querySelector('#group-panel h3')?.textContent")
        )
        if final_panel_title != "群聊 #2":
            raise AssertionError(
                f"Late response from Group 1 overwrote panel! Title={final_panel_title}"
            )

        # No ACK for Group 1 should have been sent during the switch to Group 2
        with server.lock:
            group_1_acks = [ack for ack in server.acks if ack[0] == 1]
        if group_1_acks:
            raise AssertionError(
                f"Late response from Group 1 sent ACK while viewing Group 2: {group_1_acks}"
            )

        # An ACK sent by a previous group must not update the new group's state.
        server.delay_group_1 = False
        server.delay_ack = True
        cli("eval", "openGroup(1).then(async()=>{while(state.groupHasMore)await loadOlderGroupMessages();})")
        cli("eval", "openGroup(2)")
        time.sleep(1)
        state_after = json.loads(cli("eval", "({group:state.groupID,ack:state.groupLastAckID,sent:state.groupAckedID})"))
        if state_after != {"group":"2","ack":5,"sent":5}:
            raise AssertionError(f"stale ACK crossed group boundary: {state_after}")

        print(
            json.dumps(
                {
                    "browser": "Chromium",
                    "backlog_more_button": True,
                    "backlog_safe_ack": True,
                    "out_of_order_gap_sync": True,
                    "group_switch_discard_stale": True,
                    "multi_page_live_sync": True,
                    "failed_sync_no_ack": True,
                    "stale_ack_ignored": True,
                }
            )
        )
    finally:
        cli("close")
        server.shutdown()


if __name__ == "__main__":
    main()

