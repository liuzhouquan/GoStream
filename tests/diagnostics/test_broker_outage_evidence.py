"""A SQL notification inbox alone cannot prove RabbitMQ recovery."""

from contextlib import redirect_stdout
import io
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))
import component_outage_smoke as smoke


class ReachedNextCheckpoint(Exception):
    pass


class BrokerOutageEvidenceTest(unittest.TestCase):
    def test_pending_and_recovered_rows_require_exact_unique_dispatch(self):
        with tempfile.TemporaryDirectory() as directory:
            db_path = str(Path(directory) / "state.db")
            connection = sqlite3.connect(db_path)
            self.addCleanup(connection.close)
            connection.execute("CREATE TABLE outbox_events (event_key TEXT, topic TEXT, published_at TEXT, dead_at TEXT)")
            connection.execute("INSERT INTO outbox_events VALUES ('notification:11','notification.created',NULL,NULL)")
            connection.commit()
            runner = smoke.ComponentOutageRunner("sqlite", "http://api-a", "http://api-b", db_path=db_path)
            runner.require_broker_event_state([11], recovered=False)
            with self.assertRaisesRegex(AssertionError, "source"):
                runner.wait_for_broker_event_recovery([11], timeout=0)
            connection.execute("UPDATE outbox_events SET published_at='now'")
            connection.commit()
            with self.assertRaisesRegex(AssertionError, "delivery"):
                runner.require_broker_event_state([11], recovered=True)
            connection.execute("INSERT INTO outbox_events VALUES ('broker-delivery:notification:11','notification.deliver',NULL,NULL)")
            connection.commit()
            with self.assertRaisesRegex(AssertionError, "not dispatched"):
                runner.require_broker_event_state([11], recovered=True)
            connection.execute("UPDATE outbox_events SET published_at='now'")
            connection.commit()
            runner.require_broker_event_state([11], recovered=True)
            connection.execute("INSERT INTO outbox_events VALUES ('broker-delivery:notification:11','notification.deliver','now',NULL)")
            connection.commit()
            with self.assertRaisesRegex(AssertionError, "count=2"):
                runner.require_broker_event_state([11], recovered=True)

    def test_compose_without_stack_cannot_claim_fault_recovery(self):
        runner = smoke.ComponentOutageRunner("compose", "http://api-a", "http://api-b")
        with patch.object(smoke, "call_api", side_effect=AssertionError("must reject before traffic")):
            with self.assertRaisesRegex(NotImplementedError, "AcceptanceStack"):
                runner.run_all()

    def test_runner_rejects_inbox_rows_without_retained_source(self):
        runner = smoke.ComponentOutageRunner("compose", "http://api-a", "http://api-b", stack=MagicMock())
        users = iter([{"access_token": "a", "user": {"id": 1}},
                      {"access_token": "b", "user": {"id": 2}}])
        sessions = 0

        def json_call(base, path, *args, **kwargs):
            nonlocal sessions
            if path == "/api/users":
                return next(users)
            if path.startswith("/api/messages?"):
                return {"items": [{"body": "persistent message during redis outage fixture"}]}
            if path == "/api/messages/unread":
                return {"items": [{"user_id": 1, "count": 1}]}
            if path == "/api/uploads/sessions":
                sessions += 1
                if sessions > 1:
                    raise ReachedNextCheckpoint("broker check incorrectly passed")
                return {"id": "fixture-session"}
            if path.endswith("/complete"):
                return {"url": "/media/fixture.mp4"}
            if path == "/api/videos":
                return {"id": 3}
            if path == "/api/notifications":
                return {"items": [{"id": 11}, {"id": 12}]}
            return {"items": []}

        def call_api(base, path, *args, **kwargs):
            if path == "/health":
                return 200, {"status": "ok"}, 0
            if path == "/ready":
                return 200, {"status": "ready"}, 0
            return 200, {}, 0

        def sql(query):
            if "outbox_events" in query:
                return "0"  # Missing pending source, despite a populated inbox.
            if "SELECT id FROM notifications" in query:
                return "11" if "'like'" in query else "12"
            return "1"

        socket = MagicMock()
        socket.next_of_type.side_effect = [{"type": "ready"}, {"type": "ack", "client_id": "cli_outage_fixture"}]
        with patch.object(smoke, "json_call", side_effect=json_call), \
             patch.object(smoke, "call_api", side_effect=call_api), \
             patch.object(smoke, "SimpleWebSocket", return_value=socket), \
             patch.object(smoke.secrets, "token_hex", return_value="fixture"), \
             patch.object(smoke.time, "sleep"), \
             patch.object(runner, "query_sql", side_effect=sql), redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(AssertionError, "source"):
                runner.run_all()
        self.assertNotIn("rabbitmq_outage_outbox_retention_and_delivery", runner.checkpoints)


if __name__ == "__main__":
    unittest.main()
