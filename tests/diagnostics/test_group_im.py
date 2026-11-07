"""Diagnostic unit tests for Group IM Fanout & Recovery Runner (G08 / A07)."""

import json
import os
from pathlib import Path
import re
import sys
import unittest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))

import group_im_smoke


class GroupIMDiagnosticUnitTest(unittest.TestCase):
    def test_free_port_allocation(self):
        port = group_im_smoke.free_port()
        self.assertIsInstance(port, int)
        self.assertGreater(port, 1024)
        self.assertLessEqual(port, 65535)

    def test_test_uuid_format(self):
        uuid_pattern = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
        for i in [0, 1, 19, 100, 999]:
            u = group_im_smoke.test_uuid(i)
            self.assertEqual(len(u), 36)
            self.assertTrue(uuid_pattern.match(u), f"UUID {u} does not match standard 36-char format")

    def test_percentile_calculation(self):
        self.assertEqual(group_im_smoke.percentile([], 0.5), 0.0)
        self.assertEqual(group_im_smoke.percentile([42.0], 0.95), 42.0)
        data = list(range(1, 101))  # 1 to 100
        p50 = group_im_smoke.percentile(data, 0.50)
        p95 = group_im_smoke.percentile(data, 0.95)
        p99 = group_im_smoke.percentile(data, 0.99)
        self.assertEqual(p50, 50.0)
        self.assertEqual(p95, 95.0)
        self.assertEqual(p99, 99.0)

    def test_mathematical_expected_id_set_calculation(self):
        client_count = 20
        msgs_per_client = 10
        total_messages = client_count * msgs_per_client

        # Simulate generated message IDs and senders
        messages = {}
        for c in range(client_count):
            for m in range(msgs_per_client):
                mid = c * msgs_per_client + m + 1
                messages[mid] = c  # mid -> sender_idx

        self.assertEqual(len(messages), total_messages)

        # For every client k in [0, 19], compute E(k) = { m.ID | m.Sender != k }
        all_deliveries_count = 0
        for k in range(client_count):
            expected_set = {mid for mid, sender in messages.items() if sender != k}
            own_set = {mid for mid, sender in messages.items() if sender == k}

            # Size of E(k) must be exactly total_messages - msgs_per_client (190)
            self.assertEqual(len(expected_set), total_messages - msgs_per_client)
            # Size of own_set must be exactly msgs_per_client (10)
            self.assertEqual(len(own_set), msgs_per_client)
            # Intersection must be empty (client never receives its own message)
            self.assertEqual(len(expected_set & own_set), 0)
            # Union must be all messages
            self.assertEqual(len(expected_set | own_set), total_messages)

            all_deliveries_count += len(expected_set)

        # Total deliveries across all 20 clients must equal 200 * 19 = 3800
        self.assertEqual(all_deliveries_count, total_messages * (client_count - 1))

    def test_websocket_masking_roundtrip(self):
        payload = b'{"type":"group_message","client_id":"k1","body":"hello"}'
        mask = os.urandom(4)
        masked = bytearray(b ^ mask[i % 4] for i, b in enumerate(payload))
        unmasked = bytearray(b ^ mask[i % 4] for i, b in enumerate(masked))
        self.assertEqual(bytes(unmasked), payload)

    def test_report_schema_if_present(self):
        report_path = ROOT / "data/diagnostics/group_im_smoke.json"
        if not report_path.exists():
            self.skipTest("data/diagnostics/group_im_smoke.json not yet generated")

        data = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertIn("status", data)
        self.assertIn("checkpoints", data)
        self.assertEqual(data["status"], "PASS")

        cps = data["checkpoints"]
        required_cps = [
            "fanout_matrix_20_clients",
            "expected_id_set_verified",
            "sender_ack_latency_measured",
            "delivery_latency_measured",
            "duplicate_client_id_idempotent",
            "offline_member_backfill_verified",
            "member_removal_and_reinvite_cursor_advanced",
            "per_device_independent_unread_verified",
            "sql_facts_consistent_zero_duplicates",
        ]
        for cp in required_cps:
            self.assertIn(cp, cps)
            self.assertTrue(cps[cp], f"checkpoint {cp} is not True")

        self.assertGreater(data["sender_ack_latency_ms"]["p50"], 0)
        self.assertGreater(data["delivery_latency_ms"]["p50"], 0)
        self.assertIn("resources", data)


if __name__ == "__main__":
    unittest.main()
