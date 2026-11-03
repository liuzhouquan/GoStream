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

Review after this run found that the original driver paired 20 workers with only 10 reader identities. `actor[worker % len(actors)]` therefore made two workers share each JWT, target video and followee while the driver issued state-changing requests. The six-run report remains an accurate result for that workload, and its consistency checks passed, but shared-account write contention is a workload confound; it is not a clean 20-independent-client comparison and does not identify the Hybrid query as the cause. The harness now assigns 20 reader identities to 20 workers and selects 20 untouched target videos. To respect the API's 30-login-per-minute limit, it logs in 20 readers plus 10 publishers before the 90-second load, then the remaining 10 publishers plus 20 verifiers afterward. The corrected full-size profile has not yet run; the historical P95 result remains below target, the SQL-only default stays in place, and no optimization or root cause is claimed from it.

### Follow-up diagnosis (exploratory, not a root-cause finding)

On a separate disposable MySQL 8.4.11 fixture, refreshing one followed author's `follows.created_at` while keeping its 200 historical inbox rows caused the late-follow branch to find 200 candidate videos, perform 200 unique-key inbox probes, return no rows, and take 0.799 ms in one `EXPLAIN ANALYZE`. A 20-worker direct-query probe measured Hybrid P95 at 6.415 ms before and 10.021 ms after that state change; SQL-only measured 14.490 ms and 14.400 ms. This confirms extra work in the refollow path, but Hybrid remained faster in that isolated SQL probe, so it does not explain the CI end-to-end result by itself.

A separate short localhost handler replay showed both SQL-only and Hybrid slowing in its later sequential phase; it was too short and order-sensitive to attribute the difference. No query change is claimed from these experiments. The next performance change needs a repeatable profile or interleaved full mixed-load experiment that shows a measurable improvement while retaining late-follow and inbox de-duplication correctness.

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
