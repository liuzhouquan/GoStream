#!/usr/bin/env bash
set -euo pipefail

# An isolated small-fixture correctness smoke, not the full A04 load gate.
# Requires the local Compose MySQL container; never touches the demo schema.
mysql_container=${GOSTREAM_MYSQL_CONTAINER:-gostream-mysql-1}
mysql_port=${GOSTREAM_MYSQL_PORT:-3307}
api_port=${GOSTREAM_MIXED_API_PORT:-18091}
go_bin=${GOSTREAM_GO_BIN:-/usr/local/go/bin/go}
workdir=$(mktemp -d "${TMPDIR:-/tmp}/gostream-mixed.XXXXXX")
schema="gostream_mixed_${workdir##*.}"
api_pid=''
schema_created=0

mysql_root() {
  docker exec -i "$mysql_container" sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names'
}

mysql_db() {
  docker exec -i "$mysql_container" sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names "$1"' sh "$schema"
}

cleanup() {
  local status=$?
  if [[ -n $api_pid ]]; then
    kill "$api_pid" 2>/dev/null || true
    wait "$api_pid" 2>/dev/null || true
  fi
  if (( status != 0 )) && [[ -f $workdir/api.log ]]; then
    rg -n 'Error 1213|Error 1205| 500 |panic:' "$workdir/api.log" | tail -n 20 || true
  fi
  if (( schema_created )); then
    printf 'REVOKE ALL PRIVILEGES ON `%s`.* FROM '\''gostream'\''@'\''%%'\'';\n' "$schema" | mysql_root || true
    printf 'DROP DATABASE IF EXISTS `%s`;\n' "$schema" | mysql_root || true
  fi
  rm -rf -- "$workdir"
}
trap cleanup EXIT

required_go=$(awk '$1 == "go" { print "go" $2; exit }' go.mod)
actual_go=$("$go_bin" env GOVERSION)
if [[ $actual_go != "$required_go" ]]; then
  echo "mixed smoke requires $required_go; $go_bin reports $actual_go" >&2
  exit 2
fi
printf 'CREATE DATABASE `%s`;\n' "$schema" | mysql_root
schema_created=1
printf 'GRANT ALL PRIVILEGES ON `%s`.* TO '\''gostream'\''@'\''%%'\'';\n' "$schema" | mysql_root
dsn="gostream:gostream-local@tcp(127.0.0.1:${mysql_port})/${schema}?charset=utf8mb4&parseTime=True&loc=Local"
"$go_bin" build -o "$workdir/api" ./cmd/gostream
"$go_bin" build -o "$workdir/mixed" ./tests/load/mixed
"$go_bin" run ./tests/load/seed -driver mysql -dsn "$dsn" -scenario hybrid \
  -users 60 -authors 3 -videos 90 -follows-per-user 1 -celebrity-followers 55 \
  -fanout-max-followers 50 -likes 0

api_url="http://127.0.0.1:${api_port}"
ADDR="127.0.0.1:${api_port}" DB_DRIVER=mysql DB_DSN="$dsn" MEDIA_DIR="$workdir/media" \
  JWT_SECRET=isolated-mixed-test-secret-123456789 REDIS_ADDR= FFMPEG_BIN= \
  FEED_FANOUT_MAX_FOLLOWERS=0 "$workdir/api" > "$workdir/api.log" 2>&1 &
api_pid=$!
ready=0
for _ in {1..40}; do
  if ! kill -0 "$api_pid" 2>/dev/null; then break; fi
  if curl --silent --fail "$api_url/ready" > /dev/null; then ready=1; break; fi
  sleep .25
done
if (( ! ready )); then
  echo "isolated API did not become ready" >&2
  exit 1
fi

curl --silent --fail -X POST "$api_url/api/sessions" -H 'Content-Type: application/json' \
  --data '{"username":"load-u-000003","password":"load-fixture-password"}' | \
  python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])' > "$workdir/token"
chmod 600 "$workdir/token"
video_id=$(printf "SELECT id FROM videos WHERE status = 'published' ORDER BY id LIMIT 1;\n" | mysql_db)
followee_id=$(printf "SELECT id FROM users WHERE username = 'load-u-000000';\n" | mysql_db)
"$workdir/mixed" -isolated -base-url "$api_url" -token-file "$workdir/token" \
  -video-id "$video_id" -followee-id "$followee_id" -concurrency 4 -warmup 2s -duration 5s

read -r likes like_rows favorites favorite_rows comments comment_rows < <(
  printf "SELECT like_count, (SELECT COUNT(*) FROM video_actions WHERE video_id = v.id AND kind = 'like'), favorite_count, (SELECT COUNT(*) FROM video_actions WHERE video_id = v.id AND kind = 'favorite'), comment_count, (SELECT COUNT(*) FROM comments WHERE video_id = v.id) FROM videos v WHERE v.id = %s;\n" "$video_id" | mysql_db
)
if [[ $likes != "$like_rows" || $favorites != "$favorite_rows" || $comments != "$comment_rows" ]]; then
  echo "counter mismatch: like=$likes/$like_rows favorite=$favorites/$favorite_rows comment=$comments/$comment_rows" >&2
  exit 1
fi
pending=0
for _ in {1..30}; do
  pending=$(printf 'SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL AND dead_at IS NULL;\n' | mysql_db)
  if [[ $pending == 0 ]]; then break; fi
  sleep .5
done
if [[ $pending != 0 ]]; then
  echo "outbox did not drain after writes: pending=$pending" >&2
  exit 1
fi
dead=$(printf 'SELECT COUNT(*) FROM outbox_events WHERE dead_at IS NOT NULL;\n' | mysql_db)
if [[ $dead != 0 ]]; then
  echo "outbox contains dead events: count=$dead" >&2
  exit 1
fi
echo "counters verified: like=$likes favorite=$favorites comment=$comments; pending_outbox=0"
