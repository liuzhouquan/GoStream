"""Deterministic recovery evidence without logging business rows or credentials."""
import hashlib
import json
from pathlib import Path
import re
import sqlite3


def quote_identifier(value):
    if not re.fullmatch(r'[a-zA-Z_][a-zA-Z_0-9]*', value):
        raise ValueError('unexpected recovery SQL identifier')
    return '`' + value + '`'


def capture_sql_snapshot(mode, database, mysql_query=None):
    """Hash every column of every row, preserving NULL/whitespace and row multiplicity.

    JSON_ARRAY escapes embedded tabs/newlines. MySQL caller must enable --raw so
    batch output doesn't add another backslash-escaping layer. Sorting JSON rows
    makes the fingerprint independent of query plans and physical row order.
    Only table names, counts and hashes leave this function.
    """
    connection = sqlite3.connect(database) if mode == 'sqlite' else None
    try:
        if connection is not None:
            tables = [r[0] for r in connection.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
        else:
            tables = mysql_query('SHOW TABLES;').splitlines()
        result = {}
        for table in tables:
            quoted = quote_identifier(table)
            if connection is not None:
                columns = [r[1] for r in connection.execute(f'PRAGMA table_info({quoted})')]
            else:
                columns = [r.split('\t')[0] for r in mysql_query(f'SHOW COLUMNS FROM {quoted};').splitlines()]
            expression = ', '.join(quote_identifier(c) for c in columns)
            sql = f'SELECT JSON_ARRAY({expression}) FROM {quoted};'
            if connection is not None:
                rows = [r[0] for r in connection.execute(sql)]
            else:
                rows = mysql_query(sql).splitlines()
            digest = hashlib.sha256()
            digest.update(json.dumps(columns, ensure_ascii=False).encode())
            for row in sorted(rows):
                encoded = row.encode('utf-8')
                digest.update(len(encoded).to_bytes(8, 'big'))
                digest.update(encoded)
            result[table] = {'columns': columns, 'rows': len(rows), 'sha256': digest.hexdigest()}
        return result
    finally:
        if connection is not None:
            connection.close()


def assert_sql_snapshot(expected, actual):
    if expected.keys() != actual.keys():
        raise AssertionError('SQL recovery table set changed')
    for table in expected:
        if expected[table] != actual[table]:
            raise AssertionError(f'SQL recovery rows or values changed in {table}')


def read_media_manifest(directory):
    directory = Path(directory)
    data = json.loads((directory / 'manifest.json').read_text())
    if data.get('version') != 1:
        raise AssertionError('unsupported media manifest version')
    manifest = {}
    for item in data['objects']:
        key = item['key']
        if not re.fullmatch(r'media/[0-9a-f]{32}\.(mp4|mov|webm|jpg|jpeg|png|webp)|chunks/[0-9a-f]{32}/[0-9]{3}/[0-9a-f]{64}', key):
            raise AssertionError('invalid media manifest key')
        if key in manifest:
            raise AssertionError('duplicate media manifest key')
        payload = (directory / 'objects' / key).read_bytes()
        digest = hashlib.sha256(payload).hexdigest()
        if len(payload) != item['size'] or digest != item['sha256']:
            raise AssertionError(f'media backup digest mismatch: {key}')
        manifest[key] = {'size': len(payload), 'sha256': digest}
    return manifest


def assert_media_coverage(references, manifest):
    missing = set(references) - set(manifest)
    if missing:
        raise AssertionError(f'SQL-referenced media objects missing from snapshot: {len(missing)}')


class TempSpaceSamples:
    """Phase-boundary samples; explicitly not a continuously measured peak."""
    def __init__(self, root):
        self.root = Path(root)
        self.samples = {}

    def sample(self, phase):
        self.samples[phase] = sum(p.stat().st_size for p in self.root.rglob('*') if p.is_file())
        return self.samples[phase]
