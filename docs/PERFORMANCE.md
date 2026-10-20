# Testing and performance report

## Functional verification

Run the local suite with the Go version in `go.mod`:

```sh
go test ./...
go test -race ./...
go vet ./...
node --check web/app.js
```

API integration tests use temporary SQLite databases. The suite covers registration/auth, media ownership, video editing, equal-timestamp Feed cursors, interaction transitions, resumable upload recovery and expiry, direct/group WebSocket delivery, direct and group reconnect replay, presence, outbox retries/dead letters, and Prometheus output. `feed_cache_test.go` uses an in-process Redis-compatible server to check cache hits, mutation invalidation, and database fallback; `realtime_test.go` uses two server instances sharing SQL and a Redis-compatible Pub/Sub server to check cross-instance direct, group, and notification delivery plus remote presence/disconnect. These tests do not measure real Redis performance. A Chrome check also verified group creation, a live WebSocket connection and persisted group history after reopening the conversation.

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

## Remaining measurements

No k6 or MySQL load test has been run for GoStream yet. Docker CLI in this WSL session reports that Docker Desktop WSL integration is disabled; k6 is unavailable. No MySQL throughput, Redis cache hit rate, or cross-node performance figure is claimed.
