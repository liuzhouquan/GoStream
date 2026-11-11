"""Same-count corruption and missing objects must fail actual recovery comparisons."""
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest
import os
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'integration'))
from recovery_snapshot import capture_sql_snapshot, assert_sql_snapshot, assert_media_coverage


class RecoverySnapshotTest(unittest.TestCase):
    def make_database(self, path):
        with sqlite3.connect(path) as db:
            db.executescript('CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT); CREATE TABLE message_device_ack_ranges (user_id INTEGER, first_message_id INTEGER, last_message_id INTEGER);')
            db.executemany('INSERT INTO users VALUES (?, ?)', [(1, ' padded\tname\n'), (2, None)])
            db.execute('INSERT INTO message_device_ack_ranges VALUES (1, 10, 12)')

    def test_same_count_identity_replacement_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'db.sqlite'; self.make_database(path)
            expected = capture_sql_snapshot('sqlite', str(path))
            with sqlite3.connect(path) as db: db.execute('UPDATE users SET id=9 WHERE id=2')
            with self.assertRaisesRegex(AssertionError, 'users'):
                assert_sql_snapshot(expected, capture_sql_snapshot('sqlite', str(path)))

    def test_same_count_ack_corruption_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'db.sqlite'; self.make_database(path)
            expected = capture_sql_snapshot('sqlite', str(path))
            with sqlite3.connect(path) as db: db.execute('UPDATE message_device_ack_ranges SET last_message_id=99')
            with self.assertRaisesRegex(AssertionError, 'message_device_ack_ranges'):
                assert_sql_snapshot(expected, capture_sql_snapshot('sqlite', str(path)))

    def test_whitespace_and_null_are_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'db.sqlite'; self.make_database(path)
            expected = capture_sql_snapshot('sqlite', str(path))
            assert_sql_snapshot(expected, capture_sql_snapshot('sqlite', str(path)))
            with sqlite3.connect(path) as db: db.execute("UPDATE users SET nickname='padded name' WHERE id=1")
            with self.assertRaisesRegex(AssertionError, 'users'):
                assert_sql_snapshot(expected, capture_sql_snapshot('sqlite', str(path)))

    def test_missing_seeded_object_is_rejected(self):
        key = 'media/' + 'a' * 32 + '.mp4'
        with self.assertRaisesRegex(AssertionError, 'missing'):
            assert_media_coverage({key}, {})
        assert_media_coverage({key}, {key: {'size': 12, 'sha256': 'b' * 64}})


class RecoveryModeTest(unittest.TestCase):
    def test_formal_sqlite_is_rejected_before_building(self):
        import migration_backup_recovery_smoke as recovery
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(sys, 'argv', ['recovery', '--mode', 'sqlite', '--formal-scale', '--report', str(Path(directory) / 'report.json')]), patch.dict(os.environ, {}, clear=True), patch.object(recovery.subprocess, 'run') as build:
                with self.assertRaisesRegex(ValueError, 'SQLite is component-only'):
                    recovery.main()
                build.assert_not_called()
