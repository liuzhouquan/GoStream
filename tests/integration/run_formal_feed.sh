#!/usr/bin/env bash
set -euo pipefail

echo "============================================================"
echo "GoStream Formal Feed Continuous Verification (G07 / A04)"
echo "API A: ${GOSTREAM_API_A:-http://127.0.0.1:18080}"
echo "API B: ${GOSTREAM_API_B:-http://127.0.0.1:18081}"
echo "MySQL: ${GOSTREAM_MYSQL_ADDR:-127.0.0.1:13307}"
echo "============================================================"

MYSQL_ADDR="${GOSTREAM_MYSQL_ADDR:-127.0.0.1:13307}"
DSN="gostream:gostream-local@tcp(${MYSQL_ADDR})/gostream?charset=utf8mb4&parseTime=True&loc=UTC"

echo "[1/4] Seeding hybrid formal dataset (2,000 users, 100 authors, 20,000 videos, 50,000 likes)..."
go run ./tests/load/seed -driver mysql -dsn "$DSN" -scenario hybrid \
  -users 2000 -authors 100 -videos 20000 -follows-per-user 5 \
  -celebrity-followers 1500 -fanout-max-followers 1000 -likes 50000

echo "[2/4] Seeding bench_viewer_prod account, follows and inboxes..."
go run ./tools/seed_formal_viewer.go -driver mysql -dsn "$DSN"

echo "[3/4] Verifying pre-load dataset scale..."
python3 -c '
import os, sys
from pathlib import Path
ROOT = Path(".").resolve()
sys.path.insert(0, str(ROOT / "tests/integration"))
from feed_warm_cache_continuous import check_dataset_scale
scale = check_dataset_scale("compose")
print("Observed scale:", scale)
if not scale.get("meets_formal_scale"):
    print("ERROR: Dataset scale does not meet formal criteria!", file=sys.stderr)
    sys.exit(1)
print("Scale check: PASS")
'

echo "[4/4] Executing 36-minute formal feed warm-cache load matrix..."
echo "Matrix: 4 scenes (following, following_deep, recommend_logged_in, hot) x 2 concurrencies (10, 20) x 3 rounds = 24 rounds"
echo "Timing: 30s warmup + 60s measurement per round = 90s/round (~36 minutes total)"
python3 tests/integration/feed_warm_cache_continuous.py \
  --mode compose \
  --formal-dataset \
  --warmup-seconds 30 \
  --duration-seconds 60 \
  --rounds 3 \
  --report docs/benchmarks/feed-warm-cache-continuous-2026-11-09.json

echo "Formal feed continuous verification completed successfully!"
