#!/usr/bin/env bash
set -euo pipefail

# Run only against a disposable MySQL container. The script creates two random
# databases, restores a consistent dump into the second, then rehearses a v002
# rollback and forward migration. It never names the application's demo DB.
if [[ $# -ne 1 || -z ${1:-} || -z "${GOSTREAM_RECOVERY_DSN_PREFIX:-}" ]]; then
  echo "usage: GOSTREAM_RECOVERY_DSN_PREFIX='user:password@tcp(host:port)/' bash $0 MYSQL_CONTAINER_ID" >&2
  exit 2
fi

mysql_container=$1
mysql_user=${GOSTREAM_RECOVERY_MYSQL_USER:-gostream}
go_bin=${GOSTREAM_GO_BIN:-go}
if [[ ! $mysql_user =~ ^[A-Za-z0-9_]+$ ]]; then
  echo "GOSTREAM_RECOVERY_MYSQL_USER must contain only letters, digits, and underscores" >&2
  exit 2
fi
required_go=$(awk '$1 == "go" { print "go" $2; exit }' go.mod)
actual_go=$("$go_bin" env GOVERSION)
if [[ $actual_go != "$required_go" ]]; then
  echo "recovery drill requires $required_go; $go_bin reports $actual_go" >&2
  exit 2
fi

umask 077
workdir=$(mktemp -d "${TMPDIR:-/tmp}/gostream-recovery.XXXXXX")
tag=${workdir##*.}
source_db="gostream_recovery_src_${tag}"
restored_db="gostream_recovery_dst_${tag}"
source_created=0
restored_created=0

mysql_root() {
  docker exec -i "$mysql_container" sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names'
}

mysql_db() {
  docker exec -i "$mysql_container" sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names "$1"' sh "$1"
}

cleanup() {
  if (( restored_created )); then
    printf "REVOKE ALL PRIVILEGES ON \`%s\`.* FROM '%s'@'%%';\n" "$restored_db" "$mysql_user" | mysql_root || true
    printf 'DROP DATABASE `%s`;\n' "$restored_db" | mysql_root || true
  fi
  if (( source_created )); then
    printf "REVOKE ALL PRIVILEGES ON \`%s\`.* FROM '%s'@'%%';\n" "$source_db" "$mysql_user" | mysql_root || true
    printf 'DROP DATABASE `%s`;\n' "$source_db" | mysql_root || true
  fi
  rm -rf -- "$workdir"
}
trap cleanup EXIT

assert_query() {
  local database=$1 query=$2 expected=$3 actual
  actual=$(printf '%s\n' "$query" | mysql_db "$database")
  if [[ $actual != "$expected" ]]; then
    echo "assertion failed on $database: expected '$expected', got '$actual' for $query" >&2
    exit 1
  fi
}

printf 'CREATE DATABASE `%s`;\n' "$source_db" | mysql_root
source_created=1
printf 'CREATE DATABASE `%s`;\n' "$restored_db" | mysql_root
restored_created=1
printf "GRANT ALL PRIVILEGES ON \`%s\`.* TO '%s'@'%%';\nGRANT ALL PRIVILEGES ON \`%s\`.* TO '%s'@'%%';\n" \
  "$source_db" "$mysql_user" "$restored_db" "$mysql_user" | mysql_root

echo "Installing schema in isolated database $source_db"
GOSTREAM_TEST_MYSQL_DSN="${GOSTREAM_RECOVERY_DSN_PREFIX}${source_db}?charset=utf8mb4&parseTime=True&loc=Local" \
  "$go_bin" test ./internal/migrations -run '^TestApplyMySQLIntegration$' -count=1

cat <<'SQL' | mysql_db "$source_db"
INSERT INTO users (username, password_hash, nickname, created_at)
VALUES ('recovery-user', 'fixture-hash', 'Recovery fixture', UTC_TIMESTAMP(3));
INSERT INTO videos (author_id, title, media_url, status, created_at)
SELECT id, 'Recovery video', '/media/recovery.mp4', 'published', UTC_TIMESTAMP(3)
FROM users WHERE username = 'recovery-user';
INSERT INTO outbox_events (event_key, topic, payload, available_at, created_at)
VALUES ('recovery-event', 'notification.created', '{}', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3));
SQL

assert_query "$source_db" "SELECT COUNT(*) FROM schema_migrations WHERE \`key\` IN ('schema_v001','schema_v002');" 2
assert_query "$source_db" "SELECT COUNT(*) FROM videos v JOIN users u ON u.id = v.author_id WHERE u.username = 'recovery-user' AND v.title = 'Recovery video';" 1
assert_query "$source_db" "SELECT COUNT(*) FROM outbox_events WHERE event_key = 'recovery-event';" 1

echo "Taking a single-transaction MySQL dump"
docker exec -i "$mysql_container" sh -c \
  'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --single-transaction --routines --triggers --no-tablespaces --set-gtid-purged=OFF "$1"' \
  sh "$source_db" > "$workdir/backup.sql"
test -s "$workdir/backup.sql"
sha256sum "$workdir/backup.sql"

echo "Restoring into isolated database $restored_db"
mysql_db "$restored_db" < "$workdir/backup.sql"
assert_query "$restored_db" "SELECT COUNT(*) FROM schema_migrations WHERE \`key\` IN ('schema_v001','schema_v002');" 2
assert_query "$restored_db" "SELECT COUNT(*) FROM videos v JOIN users u ON u.id = v.author_id WHERE u.username = 'recovery-user' AND v.title = 'Recovery video';" 1
assert_query "$restored_db" "SELECT COUNT(*) FROM outbox_events WHERE event_key = 'recovery-event' AND payload = '{}';" 1
assert_query "$restored_db" "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'outbox_events' AND index_name = 'idx_outbox_dispatch';" 4

echo "Rehearsing v002 rollback and forward migration on $restored_db"
cat <<'SQL' | mysql_db "$restored_db"
DROP INDEX idx_outbox_dispatch ON outbox_events;
DELETE FROM schema_migrations WHERE `key` = 'schema_v002';
SQL
assert_query "$restored_db" "SELECT COUNT(*) FROM schema_migrations WHERE \`key\` = 'schema_v002';" 0
assert_query "$restored_db" "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'outbox_events' AND index_name = 'idx_outbox_dispatch';" 0

GOSTREAM_TEST_MYSQL_RECOVERY_DSN="${GOSTREAM_RECOVERY_DSN_PREFIX}${restored_db}?charset=utf8mb4&parseTime=True&loc=Local" \
  "$go_bin" test ./internal/migrations -run '^TestMySQLRecoveryDrillIntegration$' -count=1
echo "Recovery drill passed: restored rows, rollback state, and repeated forward migration verified"
