# Testing and performance report

## Functional verification

Run the local suite with the Go version in `go.mod`:

```sh
go test ./...
go test -race ./...
go vet ./...
node --check web/app.js
```

API integration tests use temporary SQLite databases. The suite covers registration/auth, media ownership, video editing, equal-timestamp Feed cursors, preference/recency ranking, recommendation cursor paging, interaction transitions, resumable upload recovery and expiry, direct/group WebSocket delivery, direct and group reconnect replay, presence, outbox retries/dead letters, and Prometheus output. `feed_cache_test.go` uses an in-process Redis-compatible server to check cache hits, mutation invalidation, and database fallback; `realtime_test.go` uses two server instances sharing SQL and a Redis-compatible Pub/Sub server to check cross-instance direct, group, and notification delivery plus remote presence/disconnect. These tests do not measure real Redis performance. A Chrome check also verified group creation, a live WebSocket connection and persisted group history after reopening the conversation.

`mysql_integration_test.go` is opt-in because it needs a migrated MySQL database. Set `GOSTREAM_TEST_MYSQL_DSN` to the same DSN used by the API, then run `go test ./internal/platform -run '^TestMySQLRecommendationIntegration$' -count=1`. It inserts authors, a viewer, follows, interactions and videos inside one transaction, checks three recommendation cursor pages, and rolls back. This test passed against the Compose MySQL 8.4 instance on 2026-10-21; the `users` and `videos` tables were empty afterward.

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

The seed inserts synthetic published rows with placeholder media URLs, solely for Feed measurement. The driver warms the endpoint with 20 requests, then measures complete HTTP GETs, counts non-200/error responses, and reports completed requests per elapsed wall time and client-side latency percentiles. This is a small local diagnostic, not a distributed load generator. A k6 script is also available for a later container/MySQL run:

To seed the Compose MySQL after `docker compose up -d --build`, use `go run ./tests/load/seed -driver mysql -dsn 'gostream:gostream-local@tcp(127.0.0.1:3307)/gostream?charset=utf8mb4&parseTime=True&loc=Local' -videos 1000`. This creates synthetic rows in the active database; use an isolated Compose project or database for repeatable tests.

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

## Remaining measurements

k6 and multi-host load tests have not been run. MySQL/Redis results above are local, anonymous first-page measurements only; authenticated recommendation and cross-node performance remain unmeasured. FFmpeg video processing remains unimplemented.
