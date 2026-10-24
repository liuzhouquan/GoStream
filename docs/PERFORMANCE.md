# Testing and performance report

## Functional verification

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

Repeat with `SCENE=following`, `recommend`, and `hot` after preparing suitable users and videos. Record CPU, memory, SQLite/MySQL settings, data size, host, Go version, k6 version, request rate, P50/P95/P99, and error rate. Use the output to determine whether query indexes, SQL plans, connection pools, Redis caching, or hybrid fanout are warranted.

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

## Remaining measurements

k6 and multi-host load tests have not been run. MySQL/Redis results above cover anonymous recommendation and authenticated following first pages on one host; authenticated recommendation and cross-node performance remain unmeasured. FFmpeg processing throughput, queue wait time, and CPU use have not been benchmarked.

The 2026-10-23 search work was verified functionally with SQLite tests, a rolled-back MySQL literal-wildcard test, and browser paging over temporary 13-user/13-video fixtures. These checks measure correctness only. Substring `LIKE` queries use a leading wildcard and have no search-throughput result; full-text indexing and load measurements remain future work for larger catalogs.

## Media processing verification

On 2026-10-21, the Compose image installed FFmpeg. A one-second 160×90 synthetic MP4 generated by that FFmpeg binary was uploaded through the public API. The first attempt revealed that a Go nanosecond lease value did not compare equal to MySQL's `DATETIME(3)` stored value during job completion. Truncating leases to milliseconds fixed the issue. After the corrected image was rebuilt, the original leased test job completed and `tests/integration/media_smoke.py` passed twice, including on the final image. The fresh jobs produced 3,785-byte MP4s and 6,984-byte JPEG covers, switched video status from `processing` to `published`, and appeared in public Feed. `ffprobe` confirmed H.264 video at 160×90. The fixture has no audio; FFmpeg is configured to encode present audio to AAC. The script verified MP4/JPEG signatures and media HTTP responses. This is a functional smoke test with one tiny fixture, not a transcoding benchmark or codec-compatibility matrix.

On 2026-10-22, after adding ffprobe preflight and output metadata, the rebuilt Compose API processed the same one-second sample as video 1008. `tests/integration/media_smoke.py` confirmed publication, public Feed visibility, MP4/JPEG signatures, and `duration_ms=1000`, `width=160`, `height=90` in the API response. A separate 9.1 KiB synthetic MP4 with a reported 601-second duration and 16×16 frames was uploaded and published through the API. It became `failed`; the MySQL processing job recorded exactly one attempt, a dead-letter timestamp, and `unsupported video: duration must be between 0 and 10 minutes`. The rejected user's rows, source file, and fixture were removed. These are functional checks of one accepted and one rejected sample, not a throughput or codec compatibility measurement.

After adding stable failure codes and orphan-file reconciliation on 2026-10-22, `go test ./...`, `go test -race ./...`, and `go vet ./...` passed with Go 1.27.2. Unit tests check code assignment and author visibility, plus reference, age, symlink, database-error and 100-file batch-boundary protection in media cleanup. A rebuilt Compose API received a two-frame MP4 with ffprobe duration 601 seconds through `tests/integration/media_failure_smoke.py`. Video 1010 failed with author-only `processing_error: "duration_limit"`; the MySQL job showed one attempt and a dead-letter state. The rejected test user, rows, source file, and fixture were removed. The existing success smoke processed the one-second sample again as video 1011, confirming public Feed visibility, MP4/JPEG signatures, and 1000 ms/160×90 output metadata. A controlled two-day-old unregistered file was then placed in the Compose media volume and the API restarted. The file disappeared, `/ready` returned 200, and `gostream_media_reconciliation_total{result="removed"}` was 1. These are functional checks; no media throughput claim follows from them.

The creator-page retry test on 2026-10-22 found a separate upload bug in the real MySQL profile: a new resumable session attempted to write `0000-00-00` to `lease_until`, which MySQL strict mode rejected with error 1292. Changing the field to a nullable timestamp and clearing it on rollback/completion allowed the rebuilt API to migrate the column to `datetime(3) NULL`. `TestMySQLResumableUploadLeaseIntegration` then passed create, incomplete completion, chunk upload, and completed lease checks inside a rolled-back transaction. In an isolated browser session, a 601-second video showed the readable duration error; selecting a valid MP4 in its replacement form used resumable upload and PATCH, then the creator page showed `processing` followed by `published` after refresh. MySQL stored 1000 ms, 160×90 output metadata. The browser fixture rows and media files were removed. This verifies the workflow with one small video and does not measure upload or processing throughput.

On 2026-10-23, SQLite tests checked registered-asset age, video/cover/avatar/job references, deleted-video retention, video/avatar replacement, missing and recently modified files, symlinks, unsafe paths, a 101-row scan, repeat runs and database errors. The Go 1.27.2 full test suite, race suite and vet passed. The rebuilt Compose API migrated five media reference/age indexes and returned `/ready` 200. A focused MySQL integration test used a rolled-back transaction and temporary files to verify unreferenced deletion and deleted-video retention. A separate controlled eight-day-old asset inserted into the live Compose database and media volume was removed on API restart; its row count became 0, the file was absent, and `gostream_media_reconciliation_total{result="asset_removed"}` reached 1. This is a functional check of a tiny fixture, not a media-cleanup throughput measurement.
