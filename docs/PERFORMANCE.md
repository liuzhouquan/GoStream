# Testing and performance report

## Following branch-review characterization, 2026-10-31

The [three-branch review](BRANCH_REVIEW_2026-10-31.md) found that the proposed 500-video SQLite benchmark did not represent a completed push inbox: GORM applied default:true and all 500 videos remained pull-eligible; the follow timestamp also selected the late-follow branch. After explicit state/time correction and exact page checks, three 100-operation runs gave SQL-only 440067/428639/406127 ns/op and hybrid 866695/825537/789622 ns/op. [Raw output](benchmarks/following-sqlite-review-2026-10-31.txt) preserves the command/environment. These are current-mode comparisons on an in-memory database, not before/after optimization evidence or MySQL HTTP P95. Hybrid is slower in this fixture; the A04 gates were still open at that time.

## Following Feed disjoint-branch experiment, 2026-11-02

The hybrid query now excludes partial fanout rows from the inbox branch and excludes already-inboxed videos from the late-follow branch, allowing `UNION ALL` instead of distinct deduplication. `TestHybridFollowingMergesOverlappingSourcesOnce` covers both overlap cases through the HTTP handler. On the same local 500-video SQLite fixture, the before/after hybrid medians were 742093 and 706417 ns/op across three 100-iteration runs (4.8% lower); SQL-only medians were 382460 and 382524 ns/op. [Raw output](benchmarks/following-union-all-2026-11-02.txt). This is a local in-process microbenchmark with a single author and no network, MySQL, Redis, mixed writes, or P95 measurement. It does not establish an A04 performance gain. At the time of this experiment, the required full-size HTTP comparison had not run; the later result is recorded below.

## Sustained Feed HTTP driver, 2026-11-02

`tests/load/http` retains fixed-request mode and now also accepts `-warmup 30s -duration 60s -concurrency 20`. Each worker sends requests continuously until the measured window ends; already-started requests finish and their extra drain time is reported separately. The JSON `-report-file` contains successful request latency samples in microseconds, status counts, failures, bytes, window, drain, throughput and P50/P95/P99. Only successful response latencies contribute to percentiles; any non-200, transport, read, or close error makes the process exit non-zero. Token contents are never written to the report.

Example for an already-started isolated API and token file:

```sh
/tmp/gostream-feed-driver \
  -url 'http://127.0.0.1:18086/api/feed?scene=following&limit=20' \
  -token-file /tmp/gostream-feed-token \
  -concurrency 20 -warmup 30s -duration 60s \
  -report-file /tmp/feed-sql-only-1.json
```

Repeat three times for each read configuration on the same fixture, restart the API between `FEED_FANOUT_MAX_FOLLOWERS=0` and `1000`, and use a distinct report path per run. For the cache-free A04 comparison leave `REDIS_ADDR` empty in both variants. Record `uname`, `lscpu`, `free`, MySQL settings/`EXPLAIN ANALYZE`, database and fanout/outbox state alongside the JSON; this driver does not collect process CPU/RSS or prove cursor completeness by itself. The driver change itself contained no full-size performance result; the later workflow measurement is recorded below.

The manual [`Feed A04 Acceptance` workflow](../.github/workflows/feed-a04.yml) automates the read comparison on a disposable MySQL 8.4 service. Trigger it with `gh workflow run feed-a04.yml --ref main`. It seeds 2,000 users, 100 authors, 20,000 videos, 50,000 interaction rows and the completed fanout inbox; then it alternates three SQL-only and three hybrid runs at 20 workers, 30 seconds of warmup and 60 seconds of measurement. Each API restart uses the same database and JWT secret with Redis and FFmpeg disabled. Before each run it compares the first 40 following-Feed IDs against the initial SQL-only result. The workflow samples API CPU/RSS, records host/MySQL settings and raw successful latency samples, and uploads these reports as an artifact even when the 20% median-P95 gate fails. A failed comparison is evidence that the read-P95 subgate remains unmet; a green run only closes that subgate. This read-only acceptance does not cover mixed writes, fanout drain, Redis caching, or two-instance failure, which remain separate A04 gates.

## Full-size A04 read-only comparison, 2026-11-02

[Workflow run #41452090999](https://github.com/liuzhouquan/GoStream/actions/runs/41452090999) ran the prescribed 20-worker, 30-second warmup and 60-second measurement three times for each mode on GitHub Actions `ubuntu-24.04`. The fixture contained 2,000 users, 10,999 follows, 20,000 videos, 19,800 fanout jobs, 1.9 million inbox rows and 50,000 video actions; fanout and outbox pending counts were both zero before measurement. MySQL was 8.4.13, Go was 1.27.2, and Redis/FFmpeg were disabled. The SQL-only mode used `FEED_FANOUT_MAX_FOLLOWERS=0`; hybrid used `1000`. All first 40 following-Feed IDs matched.

| Mode / round | P50 ms | P95 ms | P99 ms | Successful req/s | Requests / errors | API CPU avg / peak RSS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 40.617 | 64.503 | 74.397 | 488.28 | 29,297 / 0 | 61.45% / 35,764 KiB |
| SQL-only 2 | 40.104 | 63.883 | 73.506 | 493.27 | 29,596 / 0 | 61.76% / 34,980 KiB |
| SQL-only 3 | 40.108 | 63.928 | 72.920 | 493.08 | 29,585 / 0 | 61.62% / 36,316 KiB |
| Hybrid 1 | 23.999 | 41.265 | 51.441 | 791.05 | 47,463 / 0 | 109.61% / 37,156 KiB |
| Hybrid 2 | 24.010 | 40.163 | 49.256 | 796.75 | 47,805 / 0 | 108.22% / 36,996 KiB |
| Hybrid 3 | 24.021 | 40.811 | 50.329 | 792.18 | 47,531 / 0 | 110.23% / 37,580 KiB |

The median P95 fell from 63.928 ms to 40.811 ms, a measured 36.16% improvement against the 20% read-comparison threshold. Median successful throughput was 493.08 req/s for SQL-only and 792.18 req/s for hybrid. Every run completed its full 60-second window with only HTTP 200 responses. API CPU/RSS were sampled 180 times over about 89.6 seconds per process (including startup, warmup, measurement and teardown), so those process averages are not limited to the 60-second request window; CPU above 100% means the process used more than one logical CPU on average. The shared runner had 4 vCPUs and 15 GiB host memory; MySQL used a 128 MiB InnoDB buffer pool and `REPEATABLE-READ` isolation.

[The compressed machine-readable report](benchmarks/feed-a04-2026-11-02.json.gz) retains all six successful latency-sample arrays and per-run metrics. The workflow also uploaded the original reports as [artifact 13146136126](https://github.com/liuzhouquan/GoStream/actions/runs/41452090999/artifacts/13146136126). This is evidence for the full-fixture, cache-free read-P95 subgate only. It is one synthetic-data run on a shared CI host; mixed reads/writes, Redis cache behavior, fanout drain under writes, two-instance failures, recommended/hot Feed targets and production capacity remain unmeasured.

## Twenty-client direct IM load, 2026-10-31

[The IM load report](IM_LOAD.md) runs two real API processes with 20 WebSocket accounts and 1,000 cross-instance direct messages per round, three rounds. Normal-build sender ACK P95 was 40.037 / 37.890 / 41.809 ms; recipient delivery P95 was 495.732 / 496.730 / 425.608 ms. Each round checked every intended identity and payload. Retries, 125 offline messages, complete history/unread paging and independent device ACKs ended with exactly 3,125 persisted messages. A separate race build completed the same workload. [Machine-readable summary](benchmarks/im-load-summary-2026-10-31.json) links lossless compressed raw samples, environment and resource data.

This was a short closed-loop test with at most 20 sender commands awaiting ACK, 256-byte bodies and 100ms-target process sampling on a four-vCPU CI runner. It does not measure saturation, sustained traffic, group fanout, ten-video processing or the final Compose deployment. Initial registration respects the shared Redis limit outside the measurement window. Report CPU includes background and metric work; sampled RSS and scrape durations are retained with their limits. A07 remains partial.

## Device ACK sparse-query comparison, 2026-10-31

[The measured query work package](DEVICE_ACK_QUERY.md) compares 1,000/10,000 sparse ranges at first/middle/tail/exhausted cursors on one isolated MySQL 8.4.13 transaction. The batched snapshot reader replaces a correlated range containment query. In the 10,000-range fixture, three alternating samples had median first-page costs of 1185.553 ms (v005) and 7.220 ms (batch), and tail costs of 3172.522 / 7.872 ms. All returned IDs matched expected pages. Empty-page cost did not improve. [Raw samples, environment and EXPLAIN ANALYZE](benchmarks/device-ack-query-2026-10-31.json) are retained. These are warm SQL-path timings on a shared four-vCPU CI runner, excluding production BEGIN/COMMIT, HTTP, auth and device registration; they are not HTTP P95, WS throughput or A07 load acceptance.

## Functional verification

The 2026-10-26 hybrid following Feed changes have correctness evidence from SQLite and disposable MySQL 8.4 databases, including same-timestamp merging and v004 backfill. The original v003 comparison below found a read regression. A later complete-size fixture and indexed v004 query improved the result, but its comparison still missed the 20% P95 goal; the 2026-11-02 read-only workflow result above supersedes that result for the cache-free read subgate. Earlier following-Feed numbers below describe the SQL join plus Redis first-page cache and are not a hybrid before/after comparison. A small mixed read/write smoke and a committed MySQL two-connection lease-takeover test were added on 2026-10-27. Cached-read targets, full-size mixed load, an actual API process crash, and sustained backlog drain tests remain open.

## Mixed read/write correctness smoke, 2026-10-27

`tests/load/mixed` sends 70% Feed reads (following/recommend/hot), 20% like/unlike/favorite/unfavorite, and 10% follow/unfollow/comment requests. It requires `-isolated`, an explicit API origin, a token file and target IDs because it writes persistent comments and interactions. It reports status failures and client-side P50/P95/P99 for each operation and overall. The reproducible `bash tests/integration/mixed_smoke.sh` script builds the current Go 1.27.2 API and driver, creates a random disposable MySQL schema, seeds 60 users/90 videos/111 follows, runs a 2-second read warmup and 5-second measured phase at concurrency 4, checks video counters against action/comment rows, waits for outbox drain, then removes the schema, grants and token.

This ran against local Compose MySQL 8.4 on the WSL2 AMD Ryzen 7 5800H host, with Redis and FFmpeg disabled. The first pre-fix run had 6 HTTP 500 responses among 1,139 attempts (all on unlike); MySQL logged error 1213 deadlocks on concurrent deletion. At the time, the handler retried the entire transaction up to four times after a 1213, with bounded backoff. A final post-fix smoke completed 1,907/1,907 requests in 5.006 seconds: 380.9 successful requests/s, overall P50 7.688 ms, P95 22.762 ms, P99 30.139 ms, zero HTTP errors. Following Feed P95 was 9.737 ms and recommendation P95 14.878 ms in this small fixture. The target video ended with zero likes and favorites, matching zero action rows; 95 comments matched `comment_count=95`; the outbox reached zero pending and zero dead events before teardown. The separate real MySQL two-connection regression checks counters against rows.

## Concurrent action deadlock regression, 2026-11-02

The MySQL CI run exposed HTTP 500 responses in `TestMySQLConcurrentVideoActionsIntegration`: concurrent requests for the same actor/video toggled `like` and `favorite`, and MySQL returned error 1213. A local MySQL 8.4.11 trace identified competing gap/insert-intention locks on the `video_actions.ux_action` unique index during the idempotent upsert. Locking the video row first removed the deadlocks but serialized no-op actions too, so that approach was rejected. The handler retains the atomic action/counter transaction and retries the whole transaction up to ten times with equal jitter. This does not prevent InnoDB from selecting deadlock victims; it gives the aborted transaction more chances to complete without returning a 500.

The regression now drives 20 workers across two independent API database handles, all contending on the same idempotency keys, for 80 toggles per worker. The original four-retry code failed 9 of 10 local runs, returning 71 HTTP 500 responses in 16,000 requests. Ten retries with jitter passed 30 consecutive runs against a disposable MySQL 8.4.11 instance; each run checked that action rows and video counters agreed. This is a deliberately concentrated write-hotspot test, not the mixed Feed workload or a throughput/capacity claim. Local Go 1.27.2 full tests, full race, vet and API/events/media builds passed; the repository MySQL 8.4.13 integration test passed in all eight PR #27 checks before merge as `6f18cdc`.

The smoke was repeated after its cleanup path changed: 1,731/1,731 requests succeeded in 5.013 seconds with overall P95 24.937 ms; one like, zero favorites and 86 comments all matched rows, and the outbox drained with no dead events. These are correctness and driver checks, not A04 performance acceptance: the fixture is much smaller than 2,000 users/20,000 videos/50,000 interactions, uses one actor and one write target, runs 4 rather than 20 workers for 5 rather than 60 seconds, and has no three-run distribution, CPU/memory trace or cached Redis variant. It does not establish a throughput improvement or capacity limit. For the required full-size run, seed an isolated schema as described below, start an API on that schema, save the fixture actor's JWT to a private file, and run `go run ./tests/load/mixed -isolated -base-url http://127.0.0.1:18086 -token-file /tmp/gostream-token -video-id <published-video-id> -followee-id <other-user-id> -concurrency 20 -warmup 30s -duration 60s` three times, recording the database, outbox, resource and cache metrics specified in [`TEST_STANDARD.md`](TEST_STANDARD.md).

The offline media recovery drill on 2026-10-24 used Go 1.27.2, isolated MySQL 8.4 and source-built MinIO. It copied one 24-byte signature-only media fixture and one 4 MiB resumable chunk, then restored a 22,367-byte MySQL dump and both objects into a new database and bucket; the object-mode API returned the original URL bytes and Feed row and uploaded the remaining 24-byte chunk before completing the old upload session. Each file was checked against the manifest's SHA-256, and the SQL migration markers and destination bytes were checked. This is a correctness drill with a small fixture, not a throughput, RTO or RPO measurement. A larger corpus and timed recovery remain to be run before making capacity claims. The command and exact result are in [`PROGRESS.md`](../PROGRESS.md); the operational sequence is in [`MEDIA_OPERATIONS.md`](MEDIA_OPERATIONS.md).

Run the local suite with the Go version in `go.mod`:

```sh
go test ./...
go test -race ./...
go vet ./...
node --check web/app.js
```

API integration tests use temporary SQLite databases. The suite covers registration/auth, media ownership, video editing, video processing privacy/retry/atomic completion and delete races, video probe limits and one-attempt rejection, equal-timestamp Feed cursors, preference/recency ranking, recommendation cursor paging, interaction transitions, resumable upload recovery and expiry, direct history pagination, batch ACK ownership, per-device direct/group unread isolation and replay, direct/group WebSocket delivery, direct and group reconnect replay, presence, outbox retries/dead letters, and Prometheus output. `feed_cache_test.go` uses an in-process Redis-compatible server to check anonymous cache hits, mutation invalidation, and database fallback. `following_cache_test.go` checks viewer isolation, follow/unfollow and like invalidation, cursor bypass, version TTL, and Redis failure fallback. `realtime_test.go` uses two server instances sharing SQL and a Redis-compatible Pub/Sub server to check cross-instance direct, group, and notification delivery plus remote presence/disconnect. These tests do not measure real Redis performance. Device lifecycle tests cover the 32-ID cap, inactive eviction, expiry replay and upgrade backfill; a transactional MySQL integration test covers the new indexes and cleanup. Device registration throughput has not been measured. Notification tests cover same-timestamp ID pages, owner isolation, bounded/legacy read updates, and database failures; the MySQL integration test checks pagination and bounded read behavior. A browser run loaded 35 notifications across two pages and left a later 36th notice unread after marking the earlier set. Notification pagination throughput has not been measured. A Chrome check also verified group creation, a live WebSocket connection and persisted group history after reopening the conversation.

`mysql_integration_test.go` is opt-in because it needs a migrated MySQL database with no existing published videos. Set `GOSTREAM_TEST_MYSQL_DSN` to that isolated database's DSN, then run `go test ./internal/platform -run '^TestMySQLRecommendationIntegration$' -count=1`. It inserts authors, a viewer, follows, interactions and videos inside one transaction, checks three recommendation cursor pages, and rolls back. This test passed against the Compose MySQL 8.4 instance on 2026-10-21 before demo media rows were added; the `users` and `videos` tables were empty afterward. The test now fails early with a clear message if published rows already exist.

The object-backed chunk retry path now reads and hashes an existing object before returning success. First-time uploads do not incur this extra object read. The new real MySQL/MinIO fault test proves same-size corruption is repaired, but it is a correctness test; retry latency and bandwidth have not been benchmarked.

## Measured in-process Feed baseline

On 2026-10-20, `BenchmarkTimelineFeed1000` ran on Linux/amd64 under WSL with Go 1.27.2 and an AMD Ryzen 7 5800H. The benchmark seeds 1,000 published videos in a SQLite in-memory database and issues complete `GET /api/feed?scene=timeline&limit=20` handler calls. Command:

```sh
go test ./internal/platform -run '^$' -bench '^BenchmarkTimelineFeed1000$' -benchtime=2s -count=3 -benchmem
```

| Run | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| 1 | 358,682 | 92,999 | 881 |
| 2 | 362,115 | 92,827 | 881 |
| 3 | 373,384 | 92,844 | 881 |

This measures one process and one in-memory SQLite dataset, including HTTP handler work and JSON serialization. It does not measure network throughput, MySQL, cache behavior, QPS or P95. It is a baseline for comparing later Feed changes.

## Load-test procedure

The repository includes a standard-library HTTP driver and a SQLite data seeder. Use an isolated database and start the built API in another terminal:

```sh
go run ./tests/load/seed -dsn /tmp/gostream-load.db -videos 1000
go build -o /tmp/gostream-load-api ./cmd/gostream
ADDR=127.0.0.1:18080 DB_DRIVER=sqlite DB_DSN=/tmp/gostream-load.db MEDIA_DIR=/tmp/gostream-load-media JWT_SECRET=local-performance-test-secret-123456 /tmp/gostream-load-api
```

In another terminal:

```sh
go run ./tests/load/http -url 'http://127.0.0.1:18080/api/feed?scene=timeline&limit=20' -requests 3000 -concurrency 10
```

For authenticated scenes, save a test account's `access_token` in a private file and pass `-token-file /path/to/token.txt`. The token is read from the file and is not printed by the load driver. The k6 script accepts `TOKEN` for authenticated requests.

The seed inserts synthetic published rows with placeholder media URLs, solely for Feed measurement. The driver warms the endpoint with 20 requests, then measures complete HTTP GETs, counts non-200/error responses, and reports completed requests per elapsed wall time and client-side latency percentiles. This is a small local diagnostic, not a distributed load generator.

To seed the Compose MySQL after `docker compose up -d --build`, use `go run ./tests/load/seed -driver mysql -dsn 'gostream:gostream-local@tcp(127.0.0.1:3307)/gostream?charset=utf8mb4&parseTime=True&loc=Local' -videos 1000`. This creates synthetic rows in the active database; use an isolated Compose project or database for repeatable tests.

A k6 script is also available for a later container/MySQL run:

```sh
BASE_URL=http://127.0.0.1:8080 SCENE=timeline VUS=10 DURATION=30s k6 run tests/load/feed.js
```

Repeat with `SCENE=following`, `recommend`, and `hot` after preparing suitable users and videos. Record CPU, memory, SQLite/MySQL settings, data size, host, Go version, k6 version, request rate, P50/P95/P99, and error rate. Use the output to determine whether query indexes, SQL plans, connection pools, Redis caching, or the current hybrid fanout should be redesigned.

## Hybrid following Feed: isolated MySQL comparison, 2026-10-26

The new `tests/load/seed -scenario hybrid` mode requires an empty database, applies numbered migrations, and creates deterministic accounts, follows, videos, completed ordinary-author jobs and their inbox rows. It materializes the index directly, so setup time does **not** measure worker throughput. Reproduce in a separate schema; never point this mode at the demo database:

```sh
go run ./tests/load/seed -driver mysql -dsn '<isolated MySQL DSN with parseTime=True&loc=UTC>' \
  -scenario hybrid -users 2000 -authors 100 -videos 20000 \
  -follows-per-user 2 -celebrity-followers 1500 -fanout-max-followers 1000
go build -o /tmp/gostream-feed-load-api ./cmd/gostream
# Start the same binary against that schema, with REDIS_ADDR and FFMPEG_BIN empty.
# Run first with FEED_FANOUT_MAX_FOLLOWERS=0, then restart it with 1000.
# Log in as load-u-000100 / load-fixture-password and save access_token privately.
go run ./tests/load/http -url 'http://127.0.0.1:18086/api/feed?scene=following&limit=20' \
  -token-file /tmp/gostream-feed-load-token -requests 3000 -concurrency 10
```

The measured run used WSL2 Linux 6.18.33.2 on an AMD Ryzen 7 5800H (16 logical CPUs), 13 GiB RAM, Go 1.27.2 and MySQL 8.4.11 in Docker with a 128 MiB InnoDB buffer pool. The schema contained 2,000 users, 5,299 follows, 20,000 seeded videos, 19,800 completed ordinary-author jobs, 760,000 inbox rows, and one high-follower author with 1,499 followers. A real API publication added one ordinary-author video, job and 38 inbox rows before the final controlled comparison: the final counts were 20,001 videos, 19,801 completed jobs and 760,038 inbox rows. `information_schema` reported about 145.7 MB of table data and indexes before that publication. The viewer followed two ordinary authors and the high-follower author. Both API variants used the same final binary and database, no Redis page cache, no FFmpeg, sequential process restarts, identical file redirection for request logs, 20 warmups per run, and 3,000 GETs at concurrency 10 per measured run. The first 20 video IDs and the next cursor page's 20 IDs matched exactly.

| Variant | Run | Success | Requests/s | P50 | P95 | P99 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| SQL-only pull | 1 | 3000/3000 | 542.1 | 17.648 ms | 26.602 ms | 34.152 ms |
| SQL-only pull | 2 | 3000/3000 | 540.4 | 17.993 ms | 26.875 ms | 32.149 ms |
| SQL-only pull | 3 | 3000/3000 | 552.3 | 17.731 ms | 25.700 ms | 31.568 ms |
| Hybrid push/pull | 1 | 3000/3000 | 346.0 | 27.849 ms | 40.035 ms | 47.476 ms |
| Hybrid push/pull | 2 | 3000/3000 | 379.2 | 25.398 ms | 37.355 ms | 44.987 ms |
| Hybrid push/pull | 3 | 3000/3000 | 389.6 | 24.444 ms | 35.865 ms | 53.853 ms |

The median P95 was 26.602 ms for SQL-only and 37.355 ms for hybrid, about 40% worse for hybrid in this fixture. Median throughput was 542.1 versus 379.2 requests/s. A single 3,000-request cursor-page run was also slower with hybrid: 38.461 ms P95 and 363.3 requests/s versus 23.439 ms and 587.4 requests/s for SQL-only. MySQL `EXPLAIN` showed the pull branch reading three followed authors (estimated 200 videos per author) and sorting, while the push branch additionally read the viewer's inbox index and joined videos/follows. Since this viewer follows only two ordinary authors, the extra branch cost exceeds the saved work. This is a negative result, not a performance claim for the new path. The runs are sequential, local loopback and warm, with other demo containers on the same WSL2 host; CPU, memory and mixed-write effects were not isolated. The threshold needs workload-based tuning or a cheaper pull branch before A04 can pass.

For one ordinary-author API publication using a real one-second MP4 with FFmpeg disabled, the job reached `done_at` in 2.201 seconds as observed by 200 ms database polling; it used two worker claims and inserted 38 inbox rows for 38 current followers. This checks one small backlog and the 60-second goal for that one case. It does not prove 60-second drain under concurrent publication, restart, or two API instances. The worker now exposes `gostream_feed_fanout_batches_total{result=...}`, `gostream_feed_fanout_completion_seconds` and a 30-second-sampled `gostream_feed_fanout_pending_jobs` gauge; the completion counter/histogram are covered by a unit test.

## Indexed hybrid following Feed: complete-size fixture, 2026-10-26

A second isolated MySQL 8.4 schema used `tests/load/seed -scenario hybrid -users 2000 -authors 100 -videos 20000 -follows-per-user 5 -celebrity-followers 1500 -fanout-max-followers 1000 -likes 50000`. It contained exactly 2,000 users, 20,000 published videos, 10,999 follows, 50,000 like rows with matching counters, 19,800 completed ordinary-author jobs, 1,900,000 inbox rows, and a high-follower author with 1,499 followers. The fixture materializes completed jobs, so these measurements exclude fanout write cost. After v004 migration, 19,800 videos had `feed_pull=0` and 200 had `feed_pull=1`. The same Go 1.27.2 API binary, isolated database, JWT secret, no Redis cache and no FFmpeg were used in both modes. The host was the same WSL2/Ryzen 7 5800H system and MySQL container as the earlier comparison. The first page and its cursor were identical in SQL-only and indexed hybrid modes for the mixed viewer; the second hybrid page contained 20 rows.

To reproduce, create an empty isolated MySQL schema, pass its DSN to `go run ./tests/load/seed -driver mysql -dsn '<isolated DSN>'` with the flags above, and build `go build -o /tmp/gostream-feed-api ./cmd/gostream` plus `go build -o /tmp/gostream-feed-driver ./tests/load/http`. Start that API on `127.0.0.1:18086` with the same DSN and secret, `REDIS_ADDR=`, `FFMPEG_BIN=`, and first `FEED_FANOUT_MAX_FOLLOWERS=0`; stop and restart it with `1000` for each alternating run. Save a JWT for `load-u-000100` or `load-u-001500` from `/api/sessions` to a private token file, then run:

```sh
/tmp/gostream-feed-driver -url 'http://127.0.0.1:18086/api/feed?scene=following&limit=20' \
  -token-file /tmp/gostream-feed-token -requests 3000 -concurrency 10
```

Before adding the v004 pull index, the one-query hybrid implementation had three 3,000-request P95 results of 31.692, 30.520 and 30.424 ms, versus SQL-only results of 34.275, 36.243 and 34.193 ms on the same full fixture. This reversed the original v003 regression but missed the 20% goal. MySQL `EXPLAIN ANALYZE` showed its pull branch still inspecting 1,200 followed-author videos and testing fanout jobs, taking about 11.1 ms in one warm execution.

The v004 measurement below used a prebuilt HTTP driver, 20 warmups per run, 3,000 requests at concurrency 10, process restart between variants, and alternating SQL-only (`FEED_FANOUT_MAX_FOLLOWERS=0`) and hybrid (`1000`) runs. Both variants logged to `/dev/null`. Each run completed 3,000/3,000 requests without an HTTP error. The mixed viewer `load-u-000100` followed five ordinary authors plus the high-follower author; `load-u-001500` followed only five ordinary authors. Times are client-side loopback latencies.

| Viewer | Variant | Run | Requests/s | P50 | P95 | P99 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Mixed | SQL-only | 1 | 509.5 | 19.000 ms | 26.006 ms | 30.113 ms |
| Mixed | Hybrid | 1 | 595.8 | 16.372 ms | 23.771 ms | 28.024 ms |
| Mixed | SQL-only | 2 | 498.9 | 19.284 ms | 26.776 ms | 31.974 ms |
| Mixed | Hybrid | 2 | 604.7 | 15.974 ms | 23.837 ms | 27.896 ms |
| Mixed | SQL-only | 3 | 482.0 | 19.882 ms | 28.785 ms | 36.844 ms |
| Mixed | Hybrid | 3 | 499.5 | 19.425 ms | 28.679 ms | 33.794 ms |
| Ordinary only | SQL-only | 1 | 537.2 | 17.917 ms | 25.199 ms | 30.622 ms |
| Ordinary only | Hybrid | 1 | 580.2 | 16.792 ms | 24.428 ms | 28.276 ms |
| Ordinary only | SQL-only | 2 | 543.5 | 17.811 ms | 24.879 ms | 28.531 ms |
| Ordinary only | Hybrid | 2 | 570.5 | 17.160 ms | 24.280 ms | 27.973 ms |
| Ordinary only | SQL-only | 3 | 499.5 | 19.392 ms | 27.980 ms | 33.626 ms |
| Ordinary only | Hybrid | 3 | 594.1 | 16.457 ms | 23.787 ms | 28.262 ms |

The mixed viewer's median P95 fell from 26.776 to 23.837 ms (11.0%); the ordinary-only viewer's median fell from 25.199 to 24.280 ms (3.6%). One warm `EXPLAIN ANALYZE` of the indexed hybrid query took about 0.5 ms overall, with the pull branch using `idx_video_feed_pull` for 21 rows. The earlier query took about 11.4 ms overall in its single execution. Those SQL timings are diagnostic samples, not client latency percentiles. Host and request latency varied between runs; these results do not establish the 20% A04 target. The driver gives 20 request warmups, not the 30-second warmup/60-second run required by the acceptance plan. Deep pages, 20-way concurrency, CPU/memory during load, mixed writes, publication cost, two-instance failure and 60-second backlog drain still need measurement.

## Local HTTP results

On 2026-10-20, the built API ran on WSL2 Linux 6.18.33.2, AMD Ryzen 7 5800H (16 logical CPUs), Go 1.27.2, SQLite file with 1,000 synthetic published videos by one author, no Redis, and loopback HTTP. The API used `DB_DSN=/tmp/gostream-load-nYSuzt/load.db` with no explicit WAL tuning. The standard-library driver issued 3,000 measured requests per scene at concurrency 10 after 20 warmups. These are single runs:

| Scene | Success / total | Requests/s | P50 | P95 | P99 |
| --- | ---: | ---: | ---: | ---: | ---: |
| Timeline | 3000 / 3000 | 1912.3 | 4.653 ms | 9.949 ms | 13.487 ms |
| Hot | 3000 / 3000 | 1556.9 | 5.936 ms | 10.885 ms | 13.070 ms |

The hot query sorts by a computed interaction score and was slower in this one run. Confirm with repeated runs, SQL plans, and a realistic MySQL dataset before attributing the difference to a specific bottleneck. Loopback results do not represent internet latency, concurrent uploads or messaging, container overhead, or a production MySQL/Redis deployment.

On 2026-10-21, after the rule-based recommendation change, a new built API process ran against another 1,000-video SQLite file on the same WSL2 host, again without Redis. It used 20 warmups and 3,000 measured requests per scene at concurrency 10. These are single runs, with one fixture author and no authenticated preference data:

| Scene | Success / total | Requests/s | P50 | P95 | P99 |
| --- | ---: | ---: | ---: | ---: | ---: |
| Recommendation, anonymous | 3000 / 3000 | 1634.6 | 5.645 ms | 10.098 ms | 13.533 ms |
| Hot | 3000 / 3000 | 1891.4 | 4.882 ms | 8.752 ms | 10.636 ms |

The recommendation SQL performs additional score calculations and an indexed follow check. It was slower than hot in this run, but this fixture does not measure the authenticated preference queries. Repeated MySQL tests with varied authors and interaction histories are needed before setting scale thresholds or claiming an optimization.

## Compose MySQL/Redis comparison

On 2026-10-21, Docker Desktop WSL integration became available. `docker compose up -d --build` built the API with Go 1.27.2 and started MySQL 8.4 and Redis 8.2.9. `/ready` passed, and real API requests verified registration, upload, publish, following/recommend/timeline Feed, like count, follow, notification, and a Redis cache hit. A 1,000-row synthetic fixture brought MySQL to 1,001 published videos by two authors. The same API image was run in two containers on the same WSL2 host against this MySQL: the normal `REDIS_ADDR=redis:6379` service at port 8080, and a temporary `REDIS_ADDR=` service at port 18083. Both used the same anonymous `recommend&limit=20` first-page URL. Each run had 20 warmups followed by 3,000 measured requests at concurrency 10; all requests succeeded.

| Variant | Run | Requests/s | P50 | P95 | P99 |
| --- | ---: | ---: | ---: | ---: | ---: |
| Redis page cache | 1 | 3211.4 | 2.898 ms | 4.694 ms | 6.365 ms |
| Redis page cache | 2 | 3784.4 | 2.463 ms | 3.967 ms | 5.957 ms |
| Redis page cache | 3 | 4054.1 | 2.367 ms | 3.587 ms | 4.570 ms |
| MySQL read, no page cache | 1 | 1165.3 | 8.372 ms | 11.638 ms | 14.702 ms |
| MySQL read, no page cache | 2 | 1155.8 | 8.351 ms | 11.952 ms | 14.251 ms |
| MySQL read, no page cache | 3 | 1078.8 | 8.904 ms | 13.512 ms | 17.045 ms |

The median observed throughput was 3,784.4 versus 1,155.8 requests/s, about 3.3× for this warm first-page, read-only scenario. The API's cumulative `gostream_feed_cache_total` counter read 9,058 hits and 7 misses after the runs and earlier smoke requests. This comparison does not cover authenticated recommendations, cursor pages, writes, mixed traffic, many authors, or larger datasets. It is a local measurement, not a production capacity claim. The temporary container and all test-only data were removed; Compose was restarted on empty volumes and `/ready` passed.

## Authenticated following first-page comparison

On 2026-10-22, the rebuilt Go 1.27.2 API used Compose MySQL 8.4 and Redis 8.2.9 on the same WSL2 host. A temporary author had 1,000 synthetic published videos; one temporary viewer followed that author. Three existing demo videos belonged to other authors and were excluded by the following query. A second container used the same API image, MySQL, and JWT settings with `REDIS_ADDR` empty. Both containers returned identical first-page video IDs for `scene=following&limit=20`. The standard-library HTTP driver sent a JWT from a private token file, warmed each endpoint with 20 requests, then measured 3,000 requests at concurrency 10. Runs alternated cached and uncached variants; all 18,000 requests succeeded.

| Variant | Run | Requests/s | P50 | P95 | P99 |
| --- | ---: | ---: | ---: | ---: | ---: |
| Redis page cache | 1 | 3199.6 | 2.909 ms | 4.911 ms | 7.421 ms |
| Redis page cache | 2 | 3424.0 | 2.788 ms | 4.288 ms | 5.287 ms |
| Redis page cache | 3 | 3487.6 | 2.728 ms | 4.179 ms | 5.326 ms |
| MySQL read, no page cache | 1 | 869.7 | 10.935 ms | 16.185 ms | 24.067 ms |
| MySQL read, no page cache | 2 | 882.4 | 10.937 ms | 15.892 ms | 19.146 ms |
| MySQL read, no page cache | 3 | 902.0 | 10.802 ms | 15.351 ms | 17.743 ms |

Median observed throughput was 3,424.0 versus 882.4 requests/s, about 3.9× for this warm first page. The rebuilt API's cumulative cache counter showed 9,058 hits and 9 misses after the runs and functional smoke requests. This measures repeated reads for one viewer and one author. It does not establish performance for cache misses, cursor pages, many active viewers, concurrent follow/video writes, or multiple hosts. The temporary viewer, author, 1,000 videos, token file, Redis version key, and uncached container were removed; `/ready` still passed.

## A04 mixed read/write acceptance preflight (2026-11-02)

The new manual workflow runs three fresh full-size fixtures for each mode (SQL-only and Hybrid), with 20 workers, 30 seconds of read warmup, then 60 seconds of a 70/20/10 Feed/interaction/social workload. After the measured window it uses the real API to publish 20 videos and checks action/comment counters, follower Feed visibility, outbox state, and fanout completion. A passing six-run result requires zero failed requests or consistency mismatches, all 20 hybrid fanout jobs to complete within 60 seconds, and at least 20% lower median following-Feed P95 for Hybrid. The six-run CI outcome is recorded below; implementation of the workflow does not imply the performance target passed.

One local preflight run per mode verified the full workload and publish/fanout path on Go 1.27.2 and MySQL 8.4.11. The host was WSL2 on a Ryzen 7 5800H (16 logical CPUs); MySQL used a 128 MiB InnoDB buffer pool. Each run seeded 2,000 users, 20,000 videos, 10,999 follows, 50,000 actions, 1,900,000 inbox rows, and 100 media assets. SQL-only completed 16,905 measured requests with zero failures; following Feed P95 was 48.795 ms, 20 videos published, and no fanout job was expected. Hybrid completed 14,659 measured requests with zero failures; following Feed P95 was 76.664 ms, all 20 publishes succeeded, all 20 fanout jobs drained in 0.134 seconds, and each published video was visible to its follower. Both runs ended with zero action/comment counter mismatches, zero outbox backlog/dead events, and zero follower Feed visibility failures.

These were single-run local smoke checks performed sequentially, Hybrid before SQL-only. They validate harness branches and correctness assertions; their P95 values are not a controlled comparison and must not be used to claim a performance gain or regression. The disposable MySQL container and schemas were removed. The six-run GitHub Actions result is the required performance evidence.

## Full-size A04 mixed read/write acceptance, 2026-11-02

[Workflow run #41478569849](https://github.com/liuzhouquan/GoStream/actions/runs/41478569849) completed all six runs on GitHub Actions `ubuntu-24.04`: three fresh fixtures per mode, 20 workers, 30 seconds of warmup, and 60 seconds measured at the 70/20/10 Feed/interaction/social ratio. Go was 1.27.2, MySQL was 8.4.13 with a 128 MiB buffer pool, and Redis/FFmpeg were disabled. Each fixture had 2,000 users, 10,999 follows, 20,000 videos, 19,800 completed fanout jobs, 1.9 million inbox rows, 50,000 actions, and 100 media assets. After each load window the workflow published 20 videos through the API and checked counters, follower Feed visibility, outbox and fanout state.

| Mode / round | Following P50 | Following P95 | Following P99 | Load successes / errors | API CPU avg | Peak RSS | Fanout jobs / drain |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 23.996 ms | 41.248 ms | 49.046 ms | 10,704 / 0 | 33.10% | 34,992 KiB | 0 / n/a |
| Hybrid 1 | 17.808 ms | 56.414 ms | 73.098 ms | 10,703 / 0 | 51.16% | 36,224 KiB | 20 / 0.059 s |
| Hybrid 2 | 19.665 ms | 58.873 ms | 74.327 ms | 10,631 / 0 | 51.36% | 36,724 KiB | 20 / 0.059 s |
| SQL-only 2 | 23.215 ms | 41.233 ms | 53.348 ms | 10,567 / 0 | 33.02% | 34,944 KiB | 0 / n/a |
| SQL-only 3 | 23.156 ms | 40.967 ms | 51.839 ms | 10,653 / 0 | 33.18% | 35,728 KiB | 0 / n/a |
| Hybrid 3 | 17.553 ms | 55.689 ms | 73.770 ms | 10,683 / 0 | 51.04% | 37,308 KiB | 20 / 0.061 s |

Every measured HTTP request succeeded. All 120 API publishes succeeded; the 60 hybrid fanout jobs completed within 0.061 seconds at maximum; counter mismatches, Feed visibility failures, pending/dead outbox events, and pending fanout jobs were all zero. Despite those correctness results, the SQL-only median following P95 was **41.233 ms** and the Hybrid median was **56.414 ms**. Hybrid was 36.82% slower, missing the required 20% improvement by a wide margin. Hybrid P50 was lower, but that does not satisfy a P95 gate. The default remains `FEED_FANOUT_MAX_FOLLOWERS=0`.

The summary and per-run aggregate metrics are retained in [`benchmarks/feed-a04-mixed-2026-11-02.json`](benchmarks/feed-a04-mixed-2026-11-02.json); the workflow artifact contains the raw request latency samples and API logs.

### Benchmark fixture qualification

Review after this run found that the original driver paired 20 workers with only 10 reader identities. `actor[worker % len(actors)]` therefore made two workers share each JWT, target video and followee while the driver issued state-changing requests. The six-run report remains an accurate result for that workload, and its consistency checks passed, but shared-account write contention is a workload confound; it is not a clean 20-independent-client comparison and does not identify the Hybrid query as the cause. The harness now assigns 20 reader identities to 20 workers and selects 20 untouched target videos. To respect the API's 30-login-per-minute limit, it logs in 20 readers plus 10 publishers before the 90-second load, then the remaining 10 publishers plus 20 verifiers afterward. The corrected full-size run is recorded below; it also missed the P95 target. The SQL-only default stays in place, and neither run isolates a query-level root cause.

## Corrected 20-client A04 mixed read/write acceptance, 2026-11-03

[Workflow run #41566643755](https://github.com/liuzhouquan/GoStream/actions/runs/41566643755) reran all six full-size cases with 20 distinct reader identities and 20 distinct video/followee targets. It used GitHub Actions `ubuntu-24.04` with four vCPUs, Go 1.27.2, MySQL 8.4.13, a 128 MiB InnoDB buffer pool, `REPEATABLE-READ`, and Redis/FFmpeg disabled. Each fresh schema contained 2,000 users, 10,999 follows, 20,000 videos, 19,800 completed fanout jobs, 1.9 million inbox rows and 50,000 actions. Each run had a 30-second read warmup and a 60-second 70/20/10 Feed/interaction/social window.

| Mode / round | Following P50 | Following P95 | Following P99 | Load successes / errors | API CPU avg | Peak RSS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 31.845 ms | 54.041 ms | 65.917 ms | 7,636 / 0 | 31.98% | 35,192 KiB |
| Hybrid 1 | 17.439 ms | 40.284 ms | 80.097 ms | 7,753 / 0 | 50.98% | 36,360 KiB |
| Hybrid 2 | 21.913 ms | 68.163 ms | 87.912 ms | 7,661 / 0 | 50.80% | 36,864 KiB |
| SQL-only 2 | 30.771 ms | 53.055 ms | 62.743 ms | 7,644 / 0 | 32.42% | 34,524 KiB |
| SQL-only 3 | 30.895 ms | 52.392 ms | 61.697 ms | 7,610 / 0 | 32.26% | 34,680 KiB |
| Hybrid 3 | 22.866 ms | 72.945 ms | 95.004 ms | 7,647 / 0 | 50.81% | 36,280 KiB |

Across the three fixtures per mode, SQL-only median following P95 was **53.055 ms** and Hybrid median was **68.163 ms**. Hybrid was 28.48% slower, so the required 20% improvement still fails. All 45,951 measured requests succeeded; all 120 API video publications succeeded; all 60 Hybrid fanout jobs completed, with the slowest Hybrid drain at 0.071074 seconds. Counter mismatches, missing Feed publications, pending/dead outbox events, and pending fanout jobs were zero. Hybrid API CPU averaged about 51%, compared with about 32% for SQL-only; this is a correlation to investigate, not a root-cause finding. The corrected run confirms that the P95 gate still fails with independent actors, but it does not isolate SQL time from API processing or runner effects. Keep `FEED_FANOUT_MAX_FOLLOWERS=0` by default.

The compact metrics are in [`benchmarks/feed-a04-mixed-2026-11-03.json`](benchmarks/feed-a04-mixed-2026-11-03.json); the original per-request samples are in [artifact 13199773388](https://github.com/liuzhouquan/GoStream/actions/runs/41566643755/artifacts/13199773388). The run log's summary line incorrectly reported ten publishes and included SQL-only drain times in the “Hybrid” maximum; the per-run JSON confirms 20 publishes per run and the maximum Hybrid drain stated above. The runner now derives both displayed values from the report and tests them.

## A04 mixed load with API-side following diagnostics, 2026-11-03

[Workflow run #41573060866](https://github.com/liuzhouquan/GoStream/actions/runs/41573060866) repeated the corrected six-run 20-reader comparison with the bounded Feed route histogram. It used GitHub Actions `ubuntu-24.04` (four virtual CPUs, AMD EPYC 7763), Go 1.27.2, MySQL 8.4.13, a 128 MiB InnoDB buffer pool, `REPEATABLE-READ`, and Redis/FFmpeg disabled. Each fresh schema had 2,000 users, 10,999 follows, 20,000 videos, 19,800 completed fanout jobs, 1.9 million inbox rows and 50,000 actions. Each run used a 30-second following Feed warmup and a 60-second 70/20/10 mixed window.

| Mode / round | Client following P50 | Client following P95 | Client following P99 | API mean | API P95 bucket (approx.) |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 31.544 ms | 53.649 ms | 63.217 ms | 32.139 ms | (50, 60] ms |
| Hybrid 1 | 22.547 ms | 70.299 ms | 92.272 ms | 28.326 ms | (60, 70] ms |
| Hybrid 2 | 23.220 ms | 73.680 ms | 93.100 ms | 29.247 ms | (70, 80] ms |
| SQL-only 2 | 32.215 ms | 54.607 ms | 64.031 ms | 32.749 ms | (50, 60] ms |
| SQL-only 3 | 32.044 ms | 54.644 ms | 64.516 ms | 32.674 ms | (50, 60] ms |
| Hybrid 3 | 17.779 ms | 32.486 ms | 37.990 ms | 18.089 ms | (25, 40] ms |

The SQL-only median client P95 was **54.607 ms** and the Hybrid median was **70.299 ms**, so Hybrid was 28.74% slower and the required 20% improvement still failed. All 45,737 mixed requests and 120 API publishes succeeded. Counter, Feed visibility, outbox and fanout checks passed; Hybrid fanout drained in at most 0.073683 seconds. The default remains SQL-only.

The API histogram sampled about 3,000 successful following requests in each 60-second window. Its P95 bucket was (50, 60] ms for all SQL-only rounds, (60, 70] and (70, 80] ms in the first two Hybrid rounds, and (25, 40] ms in Hybrid round three. This coarse server-side distribution broadly moves with the high/low client-tail rounds, but the third Hybrid round is much faster and the P95 buckets are intervals rather than exact values. It does not isolate SQL time from the rest of Feed handling and does not establish a query-level cause. MySQL also logged repeated redo-log checkpointer-lag warnings during the run, which is an additional shared-runner measurement limitation to retain when interpreting the spread.

The API exposes `gostream_feed_stage_duration_seconds{scene,stage,outcome}` for Feed data-loading stages. `candidate_query` covers candidate loading plus author preload; `interaction_state` includes its SQL read and in-memory mapping; recommendation context/cursor include their SQL reads and local processing. Following requests additionally record `candidate_sql` as the complete candidate query and scan, `candidate_query_start` as the elapsed `Rows()` call through obtaining `sql.Rows`, `candidate_row_next` as the sum of `sql.Rows.Next()` calls, and `candidate_row_scan` as GORM scan-loop time minus that `Next()` time. `candidate_query_start` includes pool checkout and initial database response; `candidate_row_next` includes result delivery, driver decoding, and `database/sql` iteration; `candidate_row_scan` includes GORM column/field mapping and `Scan` work, so it is not pure CPU time. Cache hits and requests rejected before data loading are not represented. These local elapsed stages do not measure MySQL execution time separately from network or API work, and should not be summed as a substitute for route latency. The `Successful Feed stage P95 (API elapsed)` Grafana panel is diagnostic only. The A04 mixed driver snapshots route, stage, and database/sql pool wait and idle-closure deltas at the warmup/measurement boundary and retains bounded P95 bucket intervals and counter deltas in each round. These deltas cover the 60-second measured window; pool metrics come from `sql.DB.Stats()` and represent connection-pool waits and connections dropped because the idle cap was exceeded, not MySQL execution time. SQLite regressions cap the pool at one connection, induce a real waiter or idle closure, and check that `/metrics` exports the live counters.

The [compact JSON report](benchmarks/feed-a04-route-histograms-2026-11-03.json) records the environment and six per-run client/server metrics. [Artifact 13202642309](https://github.com/liuzhouquan/GoStream/actions/runs/41573060866/artifacts/13202642309) retains the raw request latencies, API logs, MySQL status and fixture evidence. The workflow exited nonzero because the documented P95 gate was not met; report retention and disposable-schema cleanup both completed.

## A04 full-size Feed stage histograms, 2026-11-03

[Workflow run #41579403460](https://github.com/liuzhouquan/GoStream/actions/runs/41579403460) exercised the new stage instrumentation in the same six-run, 20-reader mixed-load acceptance. The workflow used Go 1.27.2, MySQL 8.4.13 with a 128 MiB InnoDB buffer pool and `REPEATABLE-READ`, four virtual AMD EPYC 7763 vCPUs, and disabled Redis/FFmpeg. Every fresh schema contained 2,000 users, 10,999 follows, 20,000 videos, 19,800 completed fanout jobs, 1.9 million inbox rows, 50,000 actions and 100 media assets. Each round used 30 seconds of warmup and 60 seconds of a 70/20/10 Feed/interaction/social workload. The run order was SQL-only 1, Hybrid 1, Hybrid 2, SQL-only 2, SQL-only 3, Hybrid 3.

| Mode / round | Client P50 | Client P95 | Client P99 | API route mean | API route P95 bucket |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 32.030 ms | 54.445 ms | 63.603 ms | 32.438 ms | (50, 60] ms |
| Hybrid 1 | 23.166 ms | 74.257 ms | 94.021 ms | 29.871 ms | (70, 80] ms |
| Hybrid 2 | 22.998 ms | 75.158 ms | 96.276 ms | 29.412 ms | (70, 80] ms |
| SQL-only 2 | 31.486 ms | 54.546 ms | 64.336 ms | 32.112 ms | (50, 60] ms |
| SQL-only 3 | 31.934 ms | 54.377 ms | 65.426 ms | 32.487 ms | (50, 60] ms |
| Hybrid 3 | 22.367 ms | 71.287 ms | 91.723 ms | 27.873 ms | (70, 80] ms |

The median client following P95 was **54.445 ms** for SQL-only and **74.257 ms** for Hybrid, a **36.39% regression** against the required 20% improvement. Hybrid's P50 was lower in each round, while its P95/P99 were higher. All 45,324 mixed HTTP requests and 120 API video publications succeeded. Counter, Feed visibility, outbox and fanout checks passed; all 60 Hybrid fanout jobs completed within 0.075205 seconds. The acceptance workflow failed only because the P95 target was not met, and the SQL-only default remains in place.

| Mode / round | Candidate query mean | Candidate query P95 bucket | Interaction state mean | Interaction state P95 bucket |
| --- | ---: | ---: | ---: | ---: |
| SQL-only 1 | 28.334 ms | (40, 50] ms | 3.853 ms | (5, 10] ms |
| Hybrid 1 | 25.893 ms | (60, 70] ms | 3.735 ms | (5, 10] ms |
| Hybrid 2 | 25.478 ms | (70, 80] ms | 3.690 ms | (5, 10] ms |
| SQL-only 2 | 28.012 ms | (40, 50] ms | 3.850 ms | (5, 10] ms |
| SQL-only 3 | 28.317 ms | (40, 50] ms | 3.926 ms | (5, 10] ms |
| Hybrid 3 | 23.924 ms | (60, 70] ms | 3.710 ms | (5, 10] ms |

Candidate-query mean elapsed time was lower in the Hybrid rounds, but its coarse P95 bucket was higher; interaction-state P95 buckets were the same for both modes. This points to a different tail distribution within the candidate-loading stage, but is not proof that the SQL plan or MySQL execution caused the client tail. Stage timers include API-side GORM scanning/preload work, and histogram buckets are approximate. Across the 30-second read-only warmup plus 60-second mixed-load phase, global MySQL `Questions` deltas were 84,225–85,947 for SQL-only and 130,193–131,254 for Hybrid; sampled API CPU averaged 33.58–34.81% and 51.48–51.56%, respectively. The external MySQL status snapshots bracketed both the read-only warmup and measured workload; API CPU/RSS sampling also spans that driver lifetime. These are mode-correlated counters, not per-query attribution. InnoDB row-lock wait deltas did not show the same clear separation. The run did not capture Performance Schema statement/wait deltas, pprof, or query plans, so the cause remains unconfirmed.

The [compact stage report](benchmarks/feed-a04-stage-histograms-2026-11-03.json) preserves per-round client percentiles, API route and stage histograms, basic consistency outcomes, API CPU/RSS and warmup-plus-measurement MySQL status deltas. [Artifact 13205477186](https://github.com/liuzhouquan/GoStream/actions/runs/41579403460/artifacts/13205477186) retains raw client samples and logs. This run predates the database diagnostics described below and did not capture Performance Schema statement/wait deltas.

## A04 database diagnostics, 2026-11-03

The first full-size run with database diagnostics, [workflow #41585998119](https://github.com/liuzhouquan/GoStream/actions/runs/41585998119), used 20 workers with three fresh fixtures per mode. SQL-only median following P95 was **50.775 ms**; Hybrid was **70.745 ms**, a **39.33% regression** against the required 20% improvement. All mixed requests, publishes, counter checks, Feed visibility checks and outbox/fanout drains passed. The `database/sql` pool wait delta was zero in all six measured windows, so that run did not observe pool wait saturation. The schema digest query returned four digests in each round, primarily transaction-control and connection initialization statements; it did not provide representative per-query evidence for the API workload.

A disposable local MySQL 8.4 reproduction used the repository's GORM/MySQL driver for one parameterized insert and ten parameterized selects. The schema digest snapshot showed the DDL and connection setup statements, while account event counters recorded 11 `statement/com/Execute` events. This confirms that the digest output from the full-size run is incomplete for the prepared-query traffic in this configuration; it does not identify which SQL statement or API operation caused the P95 regression. The A04 runner now retains command-level prepared execution counts and timer totals alongside the digests, clearly labeled as aggregates. The digest table groups events by schema and digest; account event summaries group by account and event class, so prepared command aggregates cannot identify an individual SQL statement ([MySQL statement summary tables](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-summary-tables.html), [statement event tables](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-tables.html)). This diagnosis does not change the client P95 gate. The first full-size sample of these counters is recorded below.

### Full-size prepared-event follow-up, 2026-11-03

[Workflow #41590181656](https://github.com/liuzhouquan/GoStream/actions/runs/41590181656) reran the corrected six-round A04 mixed workload on Go 1.27.2, MySQL 8.4.13 and a four-vCPU GitHub runner. SQL-only median following P95 was **53.334 ms**; Hybrid was **75.102 ms**, a **40.81% regression** against the required 20% improvement. All 46,630 mixed requests and 120 publishes succeeded, and consistency checks passed. The workflow failed only the performance gate; SQL-only remains the default.

| Mode / round | Client following P95 | Prepared execute events | Mean event timer | `statement/sql/set_option` events | Candidate-query API P95 bucket |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 52.538 ms | 77,274 | 18.828 ms | 3,661 | (40, 50] ms |
| SQL-only 2 | 53.883 ms | 78,116 | 18.709 ms | 3,708 | (40, 50] ms |
| SQL-only 3 | 53.334 ms | 77,930 | 18.734 ms | 3,773 | (40, 50] ms |
| Hybrid 1 | 75.102 ms | 118,348 | 10.768 ms | 11,790 | (70, 80] ms |
| Hybrid 2 | 75.689 ms | 119,377 | 10.640 ms | 11,852 | (60, 70] ms |
| Hybrid 3 | 74.222 ms | 119,482 | 10.650 ms | 11,760 | (60, 70] ms |

All six measured windows again had zero database/sql pool waits. Hybrid produced about 1.53 times as many prepared execute events and about 3.18 times as many `statement/sql/set_option` events. That event class is a connection-setup clue in this workload, but its aggregate does not prove how many connections were created or which statement caused the latency. The API did not yet export `MaxIdleClosed`, so idle-pool churn was not directly measured. The account-level execute timer is also an aggregate across statements: its lower mean in Hybrid must not be read as lower latency for the candidate query. The [compact report](benchmarks/feed-a04-prepared-and-pool-diagnostics-2026-11-03.json) retains the per-round values and workload metadata; the [workflow artifact](https://github.com/liuzhouquan/GoStream/actions/runs/41590181656) contains raw latency samples and logs.

The API now applies explicit, configurable MySQL pool caps (50 open / 25 idle by default) and exports `gostream_database_pool_max_idle_closed_total`. The first comparison after that change is recorded below; it tests connection reuse but makes no advance claim about the P95 gate.

### Full-size A04 after explicit MySQL pool caps, 2026-11-03

[Workflow #41595760362](https://github.com/liuzhouquan/GoStream/actions/runs/41595760362) ran the same six-round workload on commit `444ca7b`, using the API's MySQL pool defaults of 50 open / 25 idle connections. It completed 45,110 mixed requests and 120 publishes with zero request/publish errors and all counter, Feed visibility, outbox and fanout checks passing. SQL-only median following P95 was **53.502 ms** and Hybrid was **77.990 ms** (Hybrid **45.77% slower**); the +20% gate still fails and SQL-only remains the default.

| Mode / round | Client following P95 | Candidate query mean | Candidate query P95 bucket | Pool waits | MaxIdleClosed | MySQL Threads_connected before → after |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 52.868 ms | 28.343 ms | (40, 50] ms | 0 | 0 | 6 → 23 |
| SQL-only 2 | 53.946 ms | 28.024 ms | (40, 50] ms | 0 | 0 | 6 → 23 |
| SQL-only 3 | 53.502 ms | 28.417 ms | (40, 50] ms | 0 | 0 | 6 → 23 |
| Hybrid 1 | 77.990 ms | 26.224 ms | (70, 80] ms | 0 | 0 | 6 → 23 |
| Hybrid 2 | 80.659 ms | 27.640 ms | (70, 80] ms | 0 | 0 | 6 → 23 |
| Hybrid 3 | 75.034 ms | 24.507 ms | (60, 70] ms | 0 | 0 | 6 → 23 |

There were no pool waits or connections closed for exceeding the configured idle cap. At the end of the warmup-plus-measurement interval, MySQL had 23 connected sessions in every case versus three in every case in the immediately preceding run without explicit caps. The account event-class counter `statement/sql/set_option` fell from 3,661–3,773 to 17 for SQL-only and from 11,760–11,852 to 17 for Hybrid. The fixture DSN specifies `charset=utf8mb4`, and go-sql-driver/mysql v1.7.0 issues `SET NAMES` when opening a connection with a charset parameter (`connection.go`, `handleParams`). This is strong evidence of far fewer connection-setup statements with the larger idle pool; it is still an event aggregate, not a direct connection count.

The pool change therefore addresses real connection churn, but it did **not** improve the Hybrid client P95 in that A04 run. Hybrid's candidate-query mean remained below SQL-only, while its approximate candidate-query P95 bucket was 20–30 ms higher and interaction-state buckets were similar. The stage timer includes GORM result scanning and author preload as well as SQL execution; it does not identify which part is slow. The compact [pool comparison report](benchmarks/feed-a04-pool-reuse-2026-11-03.json) and [run artifact](https://github.com/liuzhouquan/GoStream/actions/runs/41595760362) retain the per-round evidence.

### A04 Go profiles and full-load follow-up, 2026-11-03

[Workflow #41601458252](https://github.com/liuzhouquan/GoStream/actions/runs/41601458252) ran on merged commit `18fa787` with the same six unprofiled acceptance fixtures plus two separate diagnostic fixtures. It completed **62,549 successful mixed requests, zero request failures, 120 publishes, and all consistency checks**. The acceptance gate still failed: SQL-only median following P95 was **45.027 ms** and Hybrid was **59.553 ms** (Hybrid **32.26% slower**). SQL-only following `candidate_query` P95 buckets were `(25, 40] ms` in all three rounds; Hybrid buckets were `(50, 60] ms` in all three, despite lower candidate-stage means in two rounds. Interaction-state buckets stayed `(5, 10] ms` in both modes. The six measured runs recorded zero database/sql pool waits and zero idle-cap closures.

| Diagnostic profile fixture | CPU profile sample | Highest flat CPU symbol | Goroutines at snapshot | MySQL result-buffer reads | IO-wait goroutines | Mutex/semaphore waits found |
| --- | ---: | --- | ---: | ---: | ---: | ---: |
| SQL-only round 4 | 10.41 s sampled over 60 s | `internal/runtime/syscall/linux.Syscall6`, 2.55 s (24.50%) | 78 | 20 | 44 | 0 |
| Hybrid round 4 | 11.22 s sampled over 60 s | `internal/runtime/syscall/linux.Syscall6`, 2.38 s (21.21%) | 78 | 21 | 41 | 0 |

These are separate diagnostic fixtures; they are not included in the six-round P95 calculation. Hybrid's `followingVideos` accounted for 1.70 s cumulative CPU samples across its 60-second profile, with 1.61 s under GORM `Find`; building the SQL string accounted for about 10 ms. Neither profile shows one Go computation dominating CPU. At the goroutine snapshots, about 20 request goroutines per mode were inside the MySQL driver's result-buffer read path; there was no pool wait and no mutex/semaphore wait visible. This points to time waiting for MySQL responses as a useful next measurement, but the snapshots do not distinguish server query time from socket/response time and do not prove a query-plan defect. The [compact profile report](benchmarks/feed-a04-pprof-2026-11-03.json), four compressed raw files in the [profile artifact directory](benchmarks/feed-a04-pprof-2026-11-03/), and [workflow artifact](https://github.com/liuzhouquan/GoStream/actions/runs/41601458252) preserve the evidence and full logs.

The A04 driver currently uses go-sql-driver/mysql v1.7.0 with `interpolateParams` disabled. That driver prepares, executes, and closes a statement for parameterized `Query`/`Exec` calls in this mode; with interpolation enabled, it sends one interpolated query string, reducing protocol round trips ([driver v1.7.0 documentation](https://github.com/go-sql-driver/mysql/blob/v1.7.0/README.md#interpolateparams)). A local disposable MySQL 8.4 probe confirmed a concrete observability difference: the non-interpolated prepared request appeared as `statement/com/Execute` with `SQL_TEXT` and `DIGEST_TEXT` null in `events_statements_history_long`; the interpolated request appeared as `statement/sql/select` with SQL text and a digest. MySQL documents that history-long retains recent completed statement events across all threads and that statement event tables include SQL text/digests ([MySQL 8.4 history table](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-events-statements-history-long-table.html), [statement digest fields](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-digests.html)).

The A04 workflow exposes an **opt-in experiment only** for `interpolateParams=true`; its default remains false, and the selected value is recorded in reports. The first full-size enabled run is recorded below. The fixture data is synthetic. Do not enable this option in production based on these measurements: interpolation can expose bound values in SQL-text diagnostics, and this comparison does not isolate its latency effect.

### Interpolated full-size mixed workload, 2026-11-03

[Workflow #41607464424](https://github.com/liuzhouquan/GoStream/actions/runs/41607464424) ran on commit `f83b27a` with interpolation enabled across all six acceptance fixtures and both separate profile fixtures. The six measured rounds completed **64,015 successful mixed requests, zero request failures, 120 API publishes, and all consistency checks**. The workflow failed only the +20% client P95 gate: SQL-only median following P95 was **38.483 ms** and Hybrid was **55.515 ms**, so Hybrid was **44.26% slower**. The absolute values were lower than run #41601458252, but the runs were not paired or randomized; this cannot establish that interpolation improved latency.

| Mode / round | Client following P95 | Candidate-query mean | Candidate-query P95 bucket | Interaction-state mean | Interaction-state P95 bucket |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 37.646 ms | 18.147 ms | (25, 40] ms | 2.491 ms | (5, 10] ms |
| SQL-only 2 | 38.483 ms | 18.818 ms | (25, 40] ms | 2.885 ms | (5, 10] ms |
| SQL-only 3 | 39.869 ms | 19.370 ms | (25, 40] ms | 2.812 ms | (5, 10] ms |
| Hybrid 1 | 53.649 ms | 16.834 ms | (40, 50] ms | 2.957 ms | (5, 10] ms |
| Hybrid 2 | 55.515 ms | 17.402 ms | (40, 50] ms | 3.103 ms | (5, 10] ms |
| Hybrid 3 | 58.164 ms | 19.264 ms | (50, 60] ms | 2.915 ms | (5, 10] ms |

The API candidate stage has a higher P95 bucket for Hybrid in every round, while interaction-state buckets match. This localizes the observed tail to candidate loading, which includes the database call and GORM row/preload work; it does not separate MySQL execution from response transfer or Go scanning.

The separate 60-second profiles sampled 8.97 seconds of CPU for SQL-only and 9.21 seconds for Hybrid. `internal/runtime/syscall/linux.Syscall6` was the largest flat symbol (22.30% and 19.33% of sampled CPU); neither profile had a single dominant Go computation. Each goroutine snapshot had 43 IO-wait goroutines, including 20 MySQL result-buffer reads, and no mutex/semaphore wait match. The profile fixtures were excluded from the P95 gate. These snapshots still cannot tell whether MySQL execution, socket transfer, or result consumption accounts for those waits.

Interpolation made normalized SQL statements visible in Performance Schema for some, but not all, rounds. Digest snapshots were available for SQL-only rounds 2–3 and Hybrid round 2. In SQL-only, the following query digest averaged 16.423/16.523 ms and reported about 32.6–33.0 million examined rows over the diagnostic window. The available Hybrid following candidate digest averaged 4.431 ms and 4.37 million examined rows, but its histogram count exceeded its statement count by five, so the runner correctly withheld its percentile buckets. These schema-level statement averages include the driver's 30-second read-only warmup and 60-second mixed workload; they are not per-request following P95 values.

The same diagnostic windows recorded hot-ranking digests averaging about 198–204 ms and recommendation-ranking digests about 385–397 ms, each examining roughly 30–34 million rows. Those operations were part of both mixed workloads and may contribute database contention, but the aggregates do not show whether or how much they caused the SQL-only/Hybrid difference. Two of six digest snapshots were unavailable; that run retained only the generic `performance_schema_query_failed` reason, so their failing query stage is unknown. The benchmark runner now adds a sanitized failure stage and error to future diagnostic records.

The [compact report](benchmarks/feed-a04-interpolate-2026-11-03.json) preserves the six client percentiles, API stage buckets, digest availability and selected query counters without raw request logs or credentials. No production DSN or Feed query change follows from this run. The subsequent full-size rerun and its captured failure stage are recorded below.

### Interpolated diagnostic rerun, 2026-11-03

[Workflow #41614057041](https://github.com/liuzhouquan/GoStream/actions/runs/41614057041) repeated the six acceptance rounds and two separate profile fixtures on the same synthetic 2,000-user / 20,000-video dataset with interpolation enabled. It completed 64,134 mixed requests with zero failures, 120 API publishes, and all counter, visibility, outbox, fanout and consistency checks passing. Pool wait and MaxIdleClosed remained zero in all six rounds; maximum Hybrid fanout drain was 0.063394 seconds. The only failed acceptance gate was the required +20% following P95 improvement: SQL-only median P95 was **37.587 ms**, Hybrid was **59.134 ms**, making Hybrid **57.33% slower**. This is not evidence that interpolation caused the difference because runs were neither paired nor randomized.

| Mode / round | Client following P95 | Candidate-query mean | Candidate-query P95 bucket | Interaction-state mean | Interaction-state P95 bucket |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 38.046 ms | 19.159 ms | (25, 40] ms | 2.695 ms | (5, 10] ms |
| SQL-only 2 | 37.587 ms | 19.514 ms | (25, 40] ms | 2.806 ms | (5, 10] ms |
| SQL-only 3 | 37.329 ms | 19.375 ms | (25, 40] ms | 2.647 ms | (5, 10] ms |
| Hybrid 1 | 59.702 ms | 19.119 ms | (50, 60] ms | 2.854 ms | (5, 10] ms |
| Hybrid 2 | 58.119 ms | 17.397 ms | (50, 60] ms | 3.162 ms | (5, 10] ms |
| Hybrid 3 | 59.134 ms | 19.122 ms | (50, 60] ms | 2.895 ms | (5, 10] ms |

The API candidate-stage P95 bucket was (25, 40] ms for SQL-only and (50, 60] ms for Hybrid in every round, while interaction-state buckets matched. This narrows the observed API stage but still combines database execution, result transfer and GORM row/preload work.

Performance Schema digest snapshots were available in five of six rounds. In Hybrid round 3, digest collection failed at stage digest_snapshot_parse with “statement histogram snapshot contains a duplicate bucket”; workload requests and acceptance checks still passed. An implementation-level explanation is an inference: both digest queries converted NULL DIGEST values to the same synthetic __OTHER__ identity, which can merge unrelated histogram rows with equal bucket numbers. MySQL documents NULL digest rows for statements that cannot be retained in the digest table and groups histogram rows by schema, digest and bucket ([statement digests](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-digests.html), [statement histogram summary](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-histogram-summary-tables.html)). The failed raw rows were not retained, so the exact collision is not directly confirmed. The current branch filters NULL digests consistently from both summary and histogram snapshots; this run predates that change. Its regression test verifies the query boundary, and a post-merge A04 run is still required to verify live digest collection.

Even the available following digest histograms have counts 2–7 higher than their statement counts, so their per-digest percentile buckets remain null. The concurrent recommendation ranking statements averaged about 405–426 ms and hot ranking about 205–213 ms, examining roughly 32–34 million rows per digest window. These summaries include warmup plus all mixed routes and do not show how much those operations affect following latency. The separate 60-second CPU profiles parsed with Go 1.27.2: Linux syscall was the largest flat symbol at 18.26% for SQL-only and 16.26% for Hybrid; runtime synchronization, GC scanning and allocation followed. The profiles do not identify a single dominant Go computation or distinguish server execution from socket transfer and result scanning.

The compact [rerun report](benchmarks/feed-a04-interpolate-rerun-2026-11-03.json) contains per-round acceptance, stage, pool and digest evidence without raw request samples or SQL text. Neither this run nor the code-only NULL digest filter establishes the Hybrid latency root cause. The filter has since been merged; its full-size verification is recorded below.

### Post-merge NULL digest verification, 2026-11-03

[Workflow #41619221597](https://github.com/liuzhouquan/GoStream/actions/runs/41619221597) ran on merge commit dc39594 with interpolation enabled. It completed 49,186 mixed requests with zero failures, 120 API publishes, and all counter, visibility, outbox, fanout and consistency checks passing. Pool wait and MaxIdleClosed remained zero in all six rounds; maximum Hybrid fanout drain was 0.069 seconds. All six Performance Schema digest snapshots parsed successfully, and the duplicate-bucket error from the preceding run did not recur. The workflow exited nonzero only because the required +20% following P95 gate failed: SQL-only median P95 was **44.804 ms**, Hybrid was **63.722 ms**, so Hybrid was **42.22% slower**.

| Mode / round | Client following P95 | Candidate-query mean | Candidate-query P95 bucket | Interaction-state mean | Interaction-state P95 bucket |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 46.308 ms | 25.127 ms | (40, 50] ms | 2.820 ms | (5, 10] ms |
| SQL-only 2 | 44.804 ms | 24.270 ms | (40, 50] ms | 2.965 ms | (5, 10] ms |
| SQL-only 3 | 43.966 ms | 24.177 ms | (25, 40] ms | 2.990 ms | (5, 10] ms |
| Hybrid 1 | 65.257 ms | 21.765 ms | (60, 70] ms | 2.573 ms | (5, 10] ms |
| Hybrid 2 | 63.722 ms | 21.308 ms | (50, 60] ms | 2.570 ms | (5, 10] ms |
| Hybrid 3 | 61.159 ms | 19.821 ms | (50, 60] ms | 2.625 ms | (5, 10] ms |

This run supports that excluding NULL digest rows avoids the previously observed parser failure, but it is one full-size rerun and cannot prove the failure impossible. Following histogram and statement counts still differ slightly in five rounds: SQL-only by +1, 0, +1 and Hybrid by +2, +3, +2. Therefore only SQL-only round 2 has a per-digest P95 bucket (34.674, 36.308] ms; the other following digest P95 values remain null by design. The API candidate-stage P95 buckets were (40, 50], (40, 50], (25, 40] ms for SQL-only and (60, 70], (50, 60], (50, 60] ms for Hybrid; interaction-state buckets matched. This stage evidence still includes database execution, result transfer and Go scanning/preload work.

The per-SQL digest means for the following candidate were 21.86–23.54 ms for SQL-only and 5.46–5.87 ms for Hybrid, with roughly 24.6–26.4 million versus 3.58–3.69 million rows examined. Those Performance Schema totals span the 30-second warmup and 60-second mixed window, while client P95 is a request-tail measurement; the aggregates are not directly comparable and do not explain why the Hybrid client tail is higher. The separate 60-second CPU profiles parsed with Go 1.27.2: Linux syscall was the largest flat symbol at 15.08% for SQL-only and 13.90% for Hybrid; futex, GC scanning, synchronization and JSON/allocation work followed. No single Go computation accounts for the observed latency gap, and the profiles cannot separate MySQL execution, socket transfer and response scanning.

The compact [post-merge report](benchmarks/feed-a04-null-digest-fix-verification-2026-11-03.json) records every round, digest count, execution order and profile summary without raw request samples or SQL text. The diagnostics fix is verified for this run; the Hybrid performance target remains open. The A04 runner now randomizes which mode starts each adjacent same-round pair and stores the seed and realized order. Each mode still uses an isolated, freshly seeded fixture because the mixed workload writes state; this reduces time-order bias but is not same-database pairing. Its first full-size seeded run is recorded next.

### Seeded round-order verification, 2026-11-03

[Workflow #41625136743](https://github.com/liuzhouquan/GoStream/actions/runs/41625136743) ran on merge commit `0496d0a` with `interpolateParams=true` and schedule seed `41625136743`. The runner recorded this order: SQL-only r1, Hybrid r1, SQL-only r2, Hybrid r2, Hybrid r3, SQL-only r3. Each adjacent pair contains one run per mode, and SQL-only started two of three pairs. The fixtures remained separate and freshly seeded. The run used Go 1.27.2, MySQL 8.4.13 with a 128 MiB InnoDB buffer pool, and an Ubuntu 24.04 GitHub runner with four vCPUs.

It completed **46,892** mixed requests with zero failures, **120/120** API publishes, zero counter/visibility/outbox/fanout mismatches, and zero DB pool waits or MaxIdleClosed events. The workflow exited nonzero only because the required +20% median following P95 gate failed: SQL-only was **49.174 ms**, Hybrid **64.791 ms** (**31.76% slower**).

| Mode / round | Client following P95 | API following P95 bucket | Candidate-query mean | Candidate-query P95 bucket | Interaction-state mean / P95 bucket |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 48.155 ms | (40, 50] ms | 25.672 ms | (40, 50] ms | 3.026 ms / (5, 10] ms |
| Hybrid 1 | 64.791 ms | (60, 70] ms | 21.295 ms | (60, 70] ms | 2.720 ms / (5, 10] ms |
| SQL-only 2 | 49.174 ms | (40, 50] ms | 26.338 ms | (40, 50] ms | 3.042 ms / (5, 10] ms |
| Hybrid 2 | 28.481 ms | (25, 40] ms | 11.797 ms | (10, 25] ms | 2.905 ms / (5, 10] ms |
| Hybrid 3 | 68.980 ms | (60, 70] ms | 22.782 ms | (60, 70] ms | 2.772 ms / (5, 10] ms |
| SQL-only 3 | 51.031 ms | (50, 60] ms | 27.248 ms | (40, 50] ms | 2.995 ms / (5, 10] ms |

The Hybrid round-2 P95 is much lower than its other two rounds, so the six observations still show substantial run-to-run variability; randomized pair start order alone cannot remove it. MySQL emitted repeated InnoDB redo checkpointer-lag warnings during the job. They are an environment signal, not proven to be the cause of any particular P95. The separate 60-second CPU profiles parsed with Go 1.27.2: Linux syscall was the top flat symbol at 23.30% for SQL-only and 21.44% for Hybrid. Both profiles also show futex, GC scanning and JSON/row processing; they do not isolate the end-to-end latency source.

Performance Schema digest snapshots parsed in four of six rounds. SQL-only r1 and Hybrid r3 again failed at `digest_snapshot_parse` with `statement histogram snapshot contains a duplicate bucket`, although NULL DIGEST rows were already filtered. The parser did not retain duplicate row identity, so the cause remains unconfirmed. MySQL documents the histogram grouping and unique index as `(SCHEMA_NAME, DIGEST, BUCKET_NUMBER)` ([statement histogram summary tables](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-histogram-summary-tables.html)). The follow-up change now selects and keys both `SCHEMA_NAME` and `DIGEST`, and includes the complete bucket identity in duplicate errors. This matches the documented grouping key; it is not yet verified to eliminate the observed duplicate. Another full-size run must check both digest availability and the +20% gate.

The [compact seeded-run report](benchmarks/feed-a04-seeded-order-verification-2026-11-03.json) records the actual seed, order, workload, per-round P95/stage buckets, diagnostic availability and profile summaries without retaining raw request samples. Seeded order reduces fixed time-order bias but these are still independent fixture runs, not same-database pairs.

### Live histogram aggregation verification, 2026-11-03

[Workflow #41639479241](https://github.com/liuzhouquan/GoStream/actions/runs/41639479241) ran commit `3f9e038` with `interpolateParams=true`, Go 1.27.2, MySQL 8.4.13, and an Ubuntu 24.04 runner with 4 vCPUs and 15 GiB RAM. The fixture had 2,000 users, 20,000 videos, 10,999 follows, 50,000 interactions, and 1.9 million inbox rows. It completed **62,054 mixed requests with zero failures**, **120/120 publishes**, and all counter, Feed visibility, outbox, and fanout checks. Pool wait and idle-cap closure counts were zero; maximum Hybrid fanout drain was 0.063 seconds.

The required +20% median following P95 gate still failed: SQL-only median was **41.125 ms** and Hybrid **63.362 ms**, making Hybrid **54.07% slower**. The realized order was SQL-only r1, Hybrid r1, Hybrid r2, SQL-only r2, SQL-only r3, Hybrid r3.

| Mode / round | Client following P95 | API following P95 bucket | Candidate-query mean / P95 bucket | Interaction-state mean / P95 bucket |
| --- | ---: | ---: | ---: | ---: |
| SQL-only 1 | 41.125 ms | (25, 40] ms | 20.240 ms / (25, 40] ms | 2.810 ms / (5, 10] ms |
| Hybrid 1 | 63.362 ms | (50, 60] ms | 20.124 ms / (50, 60] ms | 2.820 ms / (5, 10] ms |
| Hybrid 2 | 64.547 ms | (50, 60] ms | 21.536 ms / (50, 60] ms | 2.825 ms / (5, 10] ms |
| SQL-only 2 | 41.627 ms | (25, 40] ms | 20.079 ms / (25, 40] ms | 2.884 ms / (5, 10] ms |
| SQL-only 3 | 40.023 ms | (25, 40] ms | 20.376 ms / (25, 40] ms | 2.963 ms / (5, 10] ms |
| Hybrid 3 | 61.307 ms | (50, 60] ms | 19.543 ms / (50, 60] ms | 2.849 ms / (5, 10] ms |

All six Performance Schema digest snapshots parsed after the histogram query grouped by schema, digest, bucket number, and bucket bounds, retaining `MAX(COUNT_BUCKET)`. MySQL documents the histogram grouping key as `(SCHEMA_NAME, DIGEST, BUCKET_NUMBER)` and a unique index on those columns ([statement histogram summary tables](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-histogram-summary-tables.html)). The preceding run observed the same bucket identity and bounds with counts 0 and 1 during live load. The source of those inconsistent observations is unconfirmed; taking the largest observed cumulative count is a diagnostic sampling choice, not proof of a stable snapshot. Some per-digest histogram counts still differ from statement counts, so their per-statement P95 remains unavailable by design. The client P95 gate is independent of these diagnostics.

The digest report now retains that strict `p95_bucket_ms` behavior and adds `histogram_count_difference` plus `observed_histogram_p95_bucket_ms`. The latter computes a bucket percentile using the observed histogram sample count; it is diagnostic and does not assert complete statement coverage. Neither field changes the client-side +20% acceptance gate.

The API `candidate_query` P95 bucket was higher in all three Hybrid rounds while the `interaction_state` buckets matched; this localizes the observed tail to the candidate stage but does not isolate SQL execution, row transfer, or Go result processing. No production Feed query or DSN change is justified by this run. The compact [histogram aggregation report](benchmarks/feed-a04-histogram-aggregation-verification-2026-11-03.json) records the schedule, round metrics, environment, consistency checks, and digest availability without raw request samples or SQL text. The default remains SQL-only while the +20% Hybrid target is open.

### Follow-up diagnosis (exploratory, not a root-cause finding)

On a separate disposable MySQL 8.4.11 fixture, refreshing one followed author's `follows.created_at` while keeping its 200 historical inbox rows caused the late-follow branch to find 200 candidate videos, perform 200 unique-key inbox probes, return no rows, and take 0.799 ms in one `EXPLAIN ANALYZE`. A 20-worker direct-query probe measured Hybrid P95 at 6.415 ms before and 10.021 ms after that state change; SQL-only measured 14.490 ms and 14.400 ms. This confirms extra work in the refollow path, but Hybrid remained faster in that isolated SQL probe, so it does not explain the CI end-to-end result by itself.

A separate short localhost handler replay showed both SQL-only and Hybrid slowing in its later sequential phase; it was too short and order-sensitive to attribute the difference. No query change is claimed from these experiments. The next performance change needs a repeatable profile or interleaved full mixed-load experiment that shows a measurable improvement while retaining late-follow and inbox de-duplication correctness.

### Observed histogram P95 follow-up, 2026-11-03

[Workflow #41646149894](https://github.com/liuzhouquan/GoStream/actions/runs/41646149894) ran commit `7f1d6e9` with `interpolateParams=true`, Go 1.27.2, MySQL 8.4.13, and an Ubuntu 24.04 runner with four vCPUs and 15 GiB RAM. The fixture had 2,000 users, 20,000 videos, 10,999 follows, 50,000 video actions, and 1.9 million inbox rows. It completed **90,679 mixed requests with zero failures**, **120/120 publishes**, all consistency checks, and no pool waits or idle-cap closures. Maximum Hybrid fanout drain was 0.055 seconds.

The +20% median following P95 gate remains open: SQL-only was **27.199 ms** and Hybrid **38.182 ms** (**40.38% slower**). The realized order was Hybrid r1, SQL-only r1, Hybrid r2, SQL-only r2, SQL-only r3, Hybrid r3.

| Mode / round | Client following P95 | API candidate mean / P95 bucket | MySQL candidate digest mean / observed histogram P95 bucket | Histogram count difference | Rows examined |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 27.435 ms | 13.162 ms / (10, 25] ms | 9.640 ms / (18.197, 19.055] ms | +4 | 46,599,945 |
| SQL-only 2 | 26.874 ms | 12.579 ms / (10, 25] ms | 9.246 ms / (18.197, 19.055] ms | +4 | 47,586,690 |
| SQL-only 3 | 27.199 ms | 12.913 ms / (10, 25] ms | 9.293 ms / (18.197, 19.055] ms | +6 | 47,558,034 |
| Hybrid 1 | 40.947 ms | 12.828 ms / (25, 40] ms | 2.827 ms / (6.918, 7.244] ms | +37 | 6,084,372 |
| Hybrid 2 | 38.182 ms | 12.018 ms / (25, 40] ms | 2.829 ms / (6.918, 7.244] ms | +37 | 6,011,628 |
| Hybrid 3 | 38.073 ms | 12.033 ms / (25, 40] ms | 2.742 ms / (6.607, 6.918] ms | +35 | 6,188,196 |

The Hybrid candidate-query digest had a lower observed MySQL histogram P95 bucket and lower mean server timer wait in all three rounds; its rows-examined totals were also lower. The API candidate-stage P95 bucket was higher for Hybrid in all three rounds, while interaction-state buckets matched at (5, 10] ms. This localizes the tail outside the MySQL server timer for this digest in this run. It does not identify the cause: the Performance Schema window spans warmup plus measurement, while API and client histograms cover measured successful following requests; the API `candidate_query` stage also covers GORM result handling and the Author preload. The observed histogram percentile is explicitly diagnostic because sample and statement counts differ; strict per-statement P95 remains null, and the client P95 gate is unchanged. Protocol transfer, row scanning, preload work, timing-window differences, and query-population differences remain hypotheses, not findings.

The follow-up instrumentation retains `candidate_query` as the complete loading duration and adds `candidate_sql` for the main GORM query/result scan plus `author_preload` for the batched Author lookup and mapping. The full-size follow-up and its measured stage breakdown are recorded below. No Feed SQL change is justified by these measurements alone. The sanitized [observed histogram report](benchmarks/feed-a04-observed-histogram-p95-2026-11-03.json) contains the preceding run without raw SQL or request samples.

### Candidate SQL and Author preload breakdown, 2026-11-03

[Workflow #41652328803](https://github.com/liuzhouquan/GoStream/actions/runs/41652328803) tested commit `bb28dd1` (merged as `8a7ece0`) with Go 1.27.2, MySQL 8.4.13, and an Ubuntu 24.04 GitHub runner with 4 vCPUs and 15 GiB RAM. The 20,000-video fixture used 20 workers, 30 seconds of warmup, and 60 seconds measured per round; `interpolateParams` was enabled. The measured order was SQL-only r1, Hybrid r1, Hybrid r2, SQL-only r2, Hybrid r3, SQL-only r3.

The run completed **47,305 mixed requests with zero failures**, **120/120 publishes**, all counter, Feed visibility, outbox, and fanout consistency checks, and zero pool waits or idle-cap closures. Maximum Hybrid fanout drain was **0.073 seconds**. The required +20% client following P95 gate still failed: SQL-only median was **46.833 ms** and Hybrid was **64.798 ms** (**38.36% slower**).

| Mode / round | Client following P95 | `candidate_query` mean / P95 bucket | `candidate_sql` mean / P95 bucket | `author_preload` mean / P95 bucket | MySQL candidate mean / observed P95 bucket | Histogram count difference |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 47.421 ms | 25.714 ms / (40, 50] ms | 21.030 ms / (25, 40] ms | 4.676 ms / (10, 25] ms | 25.557 ms / (39.811, 41.687] ms | +3 |
| Hybrid 1 | 64.798 ms | 21.091 ms / (60, 70] ms | 16.458 ms / (50, 60] ms | 4.625 ms / (10, 25] ms | 6.848 ms / (15.136, 15.849] ms | +6 |
| Hybrid 2 | 68.408 ms | 21.849 ms / (60, 70] ms | 17.335 ms / (50, 60] ms | 4.504 ms / (10, 25] ms | 6.965 ms / (15.136, 15.849] ms | +8 |
| SQL-only 2 | 46.833 ms | 24.772 ms / (40, 50] ms | 20.101 ms / (25, 40] ms | 4.663 ms / (10, 25] ms | 24.011 ms / (39.811, 41.687] ms | 0 |
| Hybrid 3 | 64.497 ms | 20.697 ms / (60, 70] ms | 16.183 ms / (50, 60] ms | 4.508 ms / (10, 25] ms | 6.821 ms / (15.136, 15.849] ms | +8 |
| SQL-only 3 | 45.921 ms | 25.146 ms / (40, 50] ms | 20.491 ms / (25, 40] ms | 4.649 ms / (10, 25] ms | 23.897 ms / (38.019, 39.811] ms | +3 |

In every round, Hybrid `candidate_sql` P95 was in (50, 60] ms while SQL-only was in (25, 40] ms. The Author preload P95 bucket stayed in (10, 25] ms and interaction-state stayed in (5, 10] ms for both modes. This places the observed API tail in the candidate SQL/result-fetch stage rather than the measured Author lookup or interaction stage. `candidate_sql` includes the GORM `Find` call and result scan; it does not separate connection acquisition, database response, network delivery, and Go scanning.

The matched MySQL candidate digest had lower mean server timer wait and a lower observed P95 bucket for Hybrid, with about 3.14–3.18 million rows examined versus 22.8–24.4 million for SQL-only. Histogram and statement counts differed in five of six rounds, so strict per-digest P95 stayed null except SQL-only round 2, whose counts matched. The Performance Schema interval includes warmup as well as measurement, and its observed histogram P95 remains diagnostic. These server metrics do not prove which part of the API `candidate_sql` stage causes its higher Hybrid tail.

Separate 60-second round-4 CPU profiles had similar top flat symbols: Linux syscall accounted for 23.81% of SQL-only and 22.90% of Hybrid samples; GORM scan cumulative samples were 16.67% and 14.95%. These diagnostic-only profiles do not include the acceptance rounds or measure blocked wall time, and they do not isolate the tail source. The compact [stage-breakdown report](benchmarks/feed-a04-feed-stage-breakdown-2026-11-03.json) records all six rounds, fixture and runner metadata, aggregate digest counters, and profile summaries without raw request samples, SQL text, or profile payloads. Keep SQL-only as the default while the Hybrid acceptance target remains open.

### Following candidate row timings, 2026-11-04

The handler now keeps the same candidate SQL and uses the existing GORM scan routine through an explicit `sql.Rows` loop so it can time three parts independently. `candidate_query_start` measures `Rows()` until the row stream is returned; `candidate_row_next` sums the wall time spent in each `Rows.Next()` call; `candidate_row_scan` subtracts that time from the full GORM scan loop. The latter two are diagnostic approximations: `Next()` includes MySQL driver work and result delivery, while the remainder includes GORM metadata, type conversion, and field mapping. They do not isolate server execution, pure network time, or pure CPU.

The public SQLite Feed path and metric outcomes have been regression-tested, and the A04 parser/report now requires and retains these following-stage samples. The first full-size mixed run with this instrumentation is [workflow #41761034278](https://github.com/liuzhouquan/GoStream/actions/runs/41761034278), testing commit `302a04f` with Go 1.27.2 and MySQL 8.4.13. It used the 2,000-user/20,000-video fixture, 20 workers, 30 seconds warmup, 60 seconds measured per round, and `interpolateParams=false`.

All **45,013 mixed requests succeeded**, all **120 video publishes succeeded**, the counter/visibility/outbox/fanout checks passed, and pool wait counters remained zero. The maximum Hybrid fanout drain was 0.074 seconds. The workflow failed only because the required +20% client-P95 improvement was not met: SQL-only median following P95 was **53.590 ms**, Hybrid was **77.341 ms** (**44.32% slower**).

| Mode / round | Client following P95 | `candidate_query_start` mean / P95 bucket | `candidate_sql` mean / P95 bucket | `candidate_query` mean / P95 bucket |
| --- | ---: | ---: | ---: | ---: |
| SQL-only 1 | 52.097 ms | 20.655 ms / (25, 40] ms | 20.900 ms / (25, 40] ms | 27.588 ms / (40, 50] ms |
| Hybrid 1 | 77.341 ms | 18.543 ms / (60, 70] ms | 18.807 ms / (60, 70] ms | 25.300 ms / (70, 80] ms |
| SQL-only 2 | 54.626 ms | 21.907 ms / (25, 40] ms | 22.161 ms / (25, 40] ms | 28.894 ms / (40, 50] ms |
| Hybrid 2 | 76.022 ms | 19.377 ms / (60, 70] ms | 19.632 ms / (60, 70] ms | 26.151 ms / (70, 80] ms |
| Hybrid 3 | 77.426 ms | 19.907 ms / (60, 70] ms | 20.177 ms / (60, 70] ms | 26.873 ms / (70, 80] ms |
| SQL-only 3 | 53.590 ms | 21.317 ms / (25, 40] ms | 21.563 ms / (25, 40] ms | 28.341 ms / (40, 50] ms |

The Hybrid candidate-query means were lower, but their P95 buckets were consistently higher. `candidate_query_start` and `candidate_sql` had P95 buckets of (60, 70] ms for Hybrid versus (25, 40] ms for SQL-only. In both modes, `candidate_row_next` and `candidate_row_scan` stayed in (0, 1] ms; Author preload stayed in (10, 25] ms and interaction-state in (5, 10] ms. This points to a repeatable tail in the `Rows()`-startup portion of the candidate load, but does not establish whether connection acquisition, MySQL execution/metadata response, or another driver-level wait is responsible. MySQL Performance Schema did not identify the candidate query digest in these reports; account-level prepared-execute totals aggregate many different statements. The CPU profiles were captured in separate diagnostic rounds and cannot measure blocked wall time.

The compact [row-timing report](benchmarks/feed-a04-row-timing-2026-11-04.json) records all six rounds and measurement limits; the full raw samples and profiles remain in [artifact 13290574711](https://github.com/liuzhouquan/GoStream/actions/runs/41761034278/artifacts/13290574711). Keep SQL-only as the default. The +20% Hybrid gate remains open, and this result does not justify changing query semantics. The next experiment should identify the candidate statement's server-side wait/plan under this same workload before attempting another query change.

### Candidate SQL digest comparison with parameter interpolation, 2026-11-04

[Workflow #41764641949](https://github.com/liuzhouquan/GoStream/actions/runs/41764641949) repeated the same six-round fixture with `interpolateParams=true`, which exposed the Following query as a Performance Schema digest. It completed **47,946 mixed requests with zero failures**, **120/120 publishes**, all consistency checks, and zero pool waits. SQL-only median following client P95 was **45.552 ms**; Hybrid was **60.869 ms** (**33.62% slower**), so the +20% gate remained unmet.

| Mode / round | Client following P95 | API `candidate_query_start` P95 bucket | API `candidate_sql` P95 bucket | MySQL candidate mean / observed P95 bucket | Rows examined | Digest histogram count difference |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 45.552 ms | (25, 40] ms | (25, 40] ms | 24.218 ms / (38.019, 39.811] ms | 24,028,362 | +1 |
| SQL-only 2 | 45.577 ms | (25, 40] ms | (25, 40] ms | 23.607 ms / (38.019, 39.811] ms | 24,482,817 | 0 |
| SQL-only 3 | 45.535 ms | (25, 40] ms | (25, 40] ms | 23.670 ms / (38.019, 39.811] ms | 24,496,230 | +2 |
| Hybrid 1 | 60.869 ms | (50, 60] ms | (50, 60] ms | 6.587 ms / (14.454, 15.136] ms | 3,226,524 | +5 |
| Hybrid 2 | 60.087 ms | (50, 60] ms | (50, 60] ms | 6.607 ms / (14.454, 15.136] ms | 3,221,232 | +8 |
| Hybrid 3 | 62.488 ms | (50, 60] ms | (50, 60] ms | 6.711 ms / (14.454, 15.136] ms | 3,232,488 | +6 |

The matched Hybrid candidate digest examined roughly 3.22 million rows per round, compared with 24.0–24.5 million for SQL-only, and its server timer mean was lower. The API candidate-start P95 bucket was still higher for Hybrid; `candidate_row_next` and `candidate_row_scan` remained in (0, 1] ms for both modes. This separates the SQL statement identity and its aggregate server time from the API-side tail, but the database snapshot covers the **30-second read-only Following warmup plus the 60-second mixed measurement**. The API histogram covers only the measured 60 seconds. In addition, MySQL histogram counts differed from statement counts by 1/0/2 for SQL-only and 5/8/6 for Hybrid; the observed histogram P95 is diagnostic, and strict per-statement P95 is available only for SQL-only round 2. These windows and count gaps prevent attributing the mixed-load API tail to a specific network or driver delay.

The compact [interpolated row-timing report](benchmarks/feed-a04-row-timing-interpolated-2026-11-04.json) retains all six rounds and these limitations; raw logs and profiles remain in [artifact 13292264553](https://github.com/liuzhouquan/GoStream/actions/runs/41764641949/artifacts/13292264553). The two A04 runs used different interpolation settings and schedules, so compare each mode pair within its run; their absolute P95 values are not a controlled before/after comparison. Keep SQL-only as the default.

The A04 runner now executes the 30-second read-only warmup as a separate phase, snapshots Performance Schema after warmup, then snapshots again after the 60-second mixed load. The report retains combined, warmup-only, and measured-only digest deltas; API CPU/RSS and request histograms cover the measured phase. Local Go tests, `go vet ./...`, and all 46 A04 helper tests pass. The first post-merge attempt [#41771365565](https://github.com/liuzhouquan/GoStream/actions/runs/41771365565) stopped at helper validation before building the driver because an older statement-digest assertion omitted the new `window` field. After updating the strict expectation, full-size phase-aligned validation completed in [run #41772842953](https://github.com/liuzhouquan/GoStream/actions/runs/41772842953); the performance gate remains open and the measured-only results follow.

### Phase-aligned full-size A04 run, 2026-11-04

Run [#41772842953](https://github.com/liuzhouquan/GoStream/actions/runs/41772842953) used Go 1.27.2, MySQL 8.4.13, a four-vCPU GitHub runner, a 128 MiB InnoDB buffer pool, and the 2,000-user / 10,999-follow / 20,000-video / 1.9-million-inbox fixture. With `interpolateParams=true`, 20 workers ran a separate 30-second read-only Following warmup and a 60-second measured 70/20/10 mixed workload for each of three fresh fixtures per mode. The seed-balanced order was Hybrid 1, SQL-only 1, SQL-only 2, Hybrid 2, SQL-only 3, Hybrid 3.

All **66,093 mixed requests succeeded**, all **120 video publishes succeeded**, all counter/visibility/outbox/fanout checks passed, database pool waits were zero, and maximum Hybrid fanout drain was **0.063 seconds**. The workflow failed only the required client-P95 gate: SQL-only median Following P95 was **35.715 ms**, Hybrid was **51.648 ms** (**44.61% slower**), versus the required 20% improvement.

| Mode / round | Client Following P95 | API `candidate_query_start` P95 bucket | Measured MySQL candidate digest P95 | Rows examined | Histogram count difference |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 35.650 ms | (25, 40] ms | (23.988, 25.119] ms, strict | 4,951,710 | 0 |
| Hybrid 1 | 51.648 ms | (40, 50] ms | (41.687, 43.652] ms, observed only | 371,532 | +1 |
| SQL-only 2 | 35.715 ms | (25, 40] ms | (23.988, 25.119] ms, strict | 4,967,844 | 0 |
| Hybrid 2 | 52.335 ms | (40, 50] ms | (41.687, 43.652] ms, strict | 369,012 | 0 |
| SQL-only 3 | 37.213 ms | (25, 40] ms | (25.119, 26.303] ms, strict | 4,871,760 | 0 |
| Hybrid 3 | 22.968 ms | (10, 25] ms | (12.589, 13.183] ms, strict | 377,664 | 0 |

The phase snapshots now show that the candidate-query digest P95 is materially higher in Hybrid rounds 1 and 2, and its interval matches the API candidate-start bucket; round 2 has exact statement/histogram counts. Both the API `candidate_row_next` and `candidate_row_scan` P95 buckets stayed below 1 ms, while Hybrid round 3 had a lower candidate-query interval in both measurements. This is evidence that MySQL candidate-query time contributes to the measured tail in the two slow Hybrid rounds; it does not identify the underlying MySQL wait or prove one cause. Hybrid examined about 370–378 thousand rows versus 4.87–4.97 million for SQL-only, so fewer examined rows alone did not prevent the Hybrid tail. The Hybrid digest histogram count differed by one only in round 1, making its server P95 an observed diagnostic interval rather than a strict P95; rounds 2 and 3 have strict counts. All three warmup digest histograms have count gaps, so their P95s are observed only. The read-only warmup's Hybrid observed digest P95 was about 8.3–9.1 ms, but it runs all 20 workers as Following readers, unlike the mixed measured phase; those phases are intentionally reported separately.

Measured instance-wide InnoDB row-lock deltas also tracked the two slow Hybrid rounds: Hybrid 1/2 saw 335/307 waits and 8,827/7,304 ms accumulated wait time, compared with SQL-only 1/2 at 253/198 waits and 5,799/2,741 ms. Hybrid 3 was lower at 167 waits/1,615 ms, near SQL-only 3 at 184/1,357 ms. These MySQL status counters aggregate all statements in the isolated instance; they are not tied to the candidate SELECT and do not show that its non-locking read waited on those row locks. Treat the correlation as a lead for phase-scoped write/wait analysis, not a root-cause finding.

PR #53 added snapshots of `performance_schema.events_waits_summary_by_account_by_event_name` at baseline, post-warmup, and post-measurement boundaries. It reports per-account event counts and timer deltas for total, warmup-only, and measured-only windows. The summaries aggregate all instrumented events for that MySQL user and are not correlated to a SQL digest. All 52 initial A04 helpers and the Go checks passed; first full-size behavior and the parser correction are recorded below.

The follow-up full-size run [#41779977239](https://github.com/liuzhouquan/GoStream/actions/runs/41779977239) used the same 4-vCPU, MySQL 8.4.13, 128 MiB buffer-pool fixture and completed all six measured rounds. It passed all 47,459 mixed requests, 120 publishes, and consistency checks, with zero DB-pool waits; it failed only the +20% P95 gate. SQL-only median Following P95 was 46.362 ms and Hybrid was 60.860 ms (31.27% slower). The per-round Hybrid client P95s were 60.860, 26.086, and 73.604 ms, while SQL-only was 46.362, 45.964, and 48.528 ms. API `candidate_sql` P95 buckets were (50,60], (10,25], and (60,70] ms for Hybrid, compared with (25,40] ms in all SQL-only rounds; the `candidate_query_start` buckets matched those intervals. This continues to show large Hybrid round variation and does not identify its cause.

That run also exposed a bug in the new wait diagnostics: every snapshot was marked unavailable with `unexpected account wait event class`. A minimal live MySQL 8.4.13 reproduction showed the row was the special `idle` event for the `gostream` account (three idle events in the helper check). MySQL documents `idle` as a wait event generated while a socket is waiting for the next client request and included in wait-event summaries ([MySQL 8.4 wait-event summary tables](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-wait-summary-tables.html), [socket instances](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-socket-instances-table.html)). The parser now accepts `idle` and reports its connection-idle counter and timer separately so a long idle duration cannot dominate ranked database wait events. A full-size post-fix run verified all six wait snapshots; details follow. See the sanitized [phase wait report](benchmarks/feed-a04-phase-wait-events-2026-11-04.json) and raw [first-run artifact 13298974008](https://github.com/liuzhouquan/GoStream/actions/runs/41779977239/artifacts/13298974008).

### Full-size A04 wait-parser follow-up, 2026-11-04

The post-fix run [#41784966658](https://github.com/liuzhouquan/GoStream/actions/runs/41784966658) took 18 minutes 30 seconds, consistent with six 30-second warmups, six 60-second measurements, fixture setup, plus two diagnostic rounds. It completed **89,229 mixed requests**, **120 publishes**, and all consistency checks with zero request failures and zero database-pool waits. The only failed gate was client Following P95: SQL-only median was **27.115 ms** and Hybrid was **39.578 ms** (**45.97% slower** than SQL-only; required improvement is 20%). All six phase-scoped wait snapshots were available after the parser fix.

| Mode / round | Client Following P95 | API `candidate_sql` P95 bucket | MySQL Following digest mean | MySQL observed P95 bucket | Histogram count difference |
| --- | ---: | ---: | ---: | ---: | ---: |
| SQL-only 1 | 28.134 ms | (10, 25] ms | 9.917 ms | (19.055, 19.953] ms | +1, observed only |
| SQL-only 2 | 27.115 ms | (10, 25] ms | 9.068 ms | (18.197, 19.055] ms | 0, strict |
| SQL-only 3 | 27.004 ms | (10, 25] ms | 8.850 ms | (18.197, 19.055] ms | 0, strict |
| Hybrid 1 | 42.165 ms | (25, 40] ms | 9.165 ms | (31.623, 33.113] ms | +1, observed only |
| Hybrid 2 | 36.572 ms | (25, 40] ms | 7.436 ms | (27.542, 28.840] ms | 0, strict |
| Hybrid 3 | 39.578 ms | (25, 40] ms | 8.025 ms | (30.200, 31.623] ms | 0, strict |

The measured Following digest P95 is higher for Hybrid in all three rounds, while its mean is similar or lower. This is the strongest evidence so far that the Hybrid candidate statement itself has a server-side tail under this workload; it still does not identify which branch or plan operator causes it. The account-level measured wait totals were similar across both variants: `wait/io/table/sql/handler` about 1.26–1.27 million ms, `wait/io/file/sql/io_cache` 7.9–8.3 seconds, and `wait/io/file/sql/binlog` 4.05–4.17 seconds per 60-second account window. Idle connection time was reported separately at 226–234 seconds. These counters aggregate every statement for the account and cannot explain the Following query's tail. The sanitized [post-fix report](benchmarks/feed-a04-phase-wait-events-2026-11-04.json) retains each round and its count-gap limits; raw data is [artifact 13301891599](https://github.com/liuzhouquan/GoStream/actions/runs/41784966658/artifacts/13301891599).

The A04 runner now has an opt-in capture that runs `EXPLAIN FORMAT=JSON` using the production `followingCandidateQuery` builder, after wait/digest snapshots and outside the measured window. It records one SQL-only and one Hybrid first-page plan without changing the client-P95 gate. The capture has passed the A04 helper tests and compiled as a Go test, but has not yet run against a full-size MySQL fixture. Until that plan is available, keep SQL-only as the default and treat the Hybrid UNION/sort/anti-join shape only as the leading hypothesis, not a confirmed root cause.

## Remaining measurements

k6 and multi-host load tests have not been run. MySQL/Redis results above cover anonymous recommendation and authenticated following first pages on one host; authenticated recommendation and cross-node performance remain unmeasured. FFmpeg processing throughput, queue wait time, and CPU use have not been benchmarked.

The 2026-10-23 search work was verified functionally with SQLite tests, a rolled-back MySQL literal-wildcard test, and browser paging over temporary 13-user/13-video fixtures. These checks measure correctness only. Substring `LIKE` queries use a leading wildcard and have no search-throughput result; full-text indexing and load measurements remain future work for larger catalogs.

## Media processing verification

On 2026-10-21, the Compose image installed FFmpeg. A one-second 160×90 synthetic MP4 generated by that FFmpeg binary was uploaded through the public API. The first attempt revealed that a Go nanosecond lease value did not compare equal to MySQL's `DATETIME(3)` stored value during job completion. Truncating leases to milliseconds fixed the issue. After the corrected image was rebuilt, the original leased test job completed and `tests/integration/media_smoke.py` passed twice, including on the final image. The fresh jobs produced 3,785-byte MP4s and 6,984-byte JPEG covers, switched video status from `processing` to `published`, and appeared in public Feed. `ffprobe` confirmed H.264 video at 160×90. The fixture has no audio; FFmpeg is configured to encode present audio to AAC. The script verified MP4/JPEG signatures and media HTTP responses. This is a functional smoke test with one tiny fixture, not a transcoding benchmark or codec-compatibility matrix.

On 2026-10-22, after adding ffprobe preflight and output metadata, the rebuilt Compose API processed the same one-second sample as video 1008. `tests/integration/media_smoke.py` confirmed publication, public Feed visibility, MP4/JPEG signatures, and `duration_ms=1000`, `width=160`, `height=90` in the API response. A separate 9.1 KiB synthetic MP4 with a reported 601-second duration and 16×16 frames was uploaded and published through the API. It became `failed`; the MySQL processing job recorded exactly one attempt, a dead-letter timestamp, and `unsupported video: duration must be between 0 and 10 minutes`. The rejected user's rows, source file, and fixture were removed. These are functional checks of one accepted and one rejected sample, not a throughput or codec compatibility measurement.

After adding stable failure codes and orphan-file reconciliation on 2026-10-22, `go test ./...`, `go test -race ./...`, and `go vet ./...` passed with Go 1.27.2. Unit tests check code assignment and author visibility, plus reference, age, symlink, database-error and 100-file batch-boundary protection in media cleanup. A rebuilt Compose API received a two-frame MP4 with ffprobe duration 601 seconds through `tests/integration/media_failure_smoke.py`. Video 1010 failed with author-only `processing_error: "duration_limit"`; the MySQL job showed one attempt and a dead-letter state. The rejected test user, rows, source file, and fixture were removed. The existing success smoke processed the one-second sample again as video 1011, confirming public Feed visibility, MP4/JPEG signatures, and 1000 ms/160×90 output metadata. A controlled two-day-old unregistered file was then placed in the Compose media volume and the API restarted. The file disappeared, `/ready` returned 200, and `gostream_media_reconciliation_total{result="removed"}` was 1. These are functional checks; no media throughput claim follows from them.

The creator-page retry test on 2026-10-22 found a separate upload bug in the real MySQL profile: a new resumable session attempted to write `0000-00-00` to `lease_until`, which MySQL strict mode rejected with error 1292. Changing the field to a nullable timestamp and clearing it on rollback/completion allowed the rebuilt API to migrate the column to `datetime(3) NULL`. `TestMySQLResumableUploadLeaseIntegration` then passed create, incomplete completion, chunk upload, and completed lease checks inside a rolled-back transaction. In an isolated browser session, a 601-second video showed the readable duration error; selecting a valid MP4 in its replacement form used resumable upload and PATCH, then the creator page showed `processing` followed by `published` after refresh. MySQL stored 1000 ms, 160×90 output metadata. The browser fixture rows and media files were removed. This verifies the workflow with one small video and does not measure upload or processing throughput.

On 2026-10-23, SQLite tests checked registered-asset age, video/cover/avatar/job references, deleted-video retention, video/avatar replacement, missing and recently modified files, symlinks, unsafe paths, a 101-row scan, repeat runs and database errors. The Go 1.27.2 full test suite, race suite and vet passed. The rebuilt Compose API migrated five media reference/age indexes and returned `/ready` 200. A focused MySQL integration test used a rolled-back transaction and temporary files to verify unreferenced deletion and deleted-video retention. A separate controlled eight-day-old asset inserted into the live Compose database and media volume was removed on API restart; its row count became 0, the file was absent, and `gostream_media_reconciliation_total{result="asset_removed"}` reached 1. This is a functional check of a tiny fixture, not a media-cleanup throughput measurement.

On 2026-10-25, the image-validation tests accepted complete JPEG/PNG/WebP payloads and rejected truncated payloads through direct and resumable uploads in local mode and against real MinIO. The output fault test used real MinIO behind a proxy that replaced successful MP4 and JPEG PUT responses with 503. On both SQLite and a migrated isolated MySQL 8.4 database, the first two attempts cleaned unregistered objects and left the video hidden; a third attempt published and both final object hashes matched. The fault test supplies small already generated bytes to isolate the upload/SQL boundary, while the earlier Compose smoke covers actual FFmpeg transcoding. These are correctness and failure-recovery checks, not latency or throughput measurements. Full image decoding adds CPU and memory work; its cost and large-file output retry time have not been measured.
