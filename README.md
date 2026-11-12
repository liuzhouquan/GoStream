# GoStream

GoStream is a runnable Go short-video social platform with a static web client, persistent database, resumable media uploads, direct and group messaging, and a transactional notification outbox with optional RabbitMQ delivery. It runs as one API process by default; Redis can relay realtime events between API instances.

## Status

**v1.0 delivery scope and verification are documented in the [PR #64 review](docs/PR64_REVIEW_2026-11-11.md).** The existing A01–A12 index covers user/social features, media processing, Feed, interactions, messaging, notifications/search, permissions, and operations. Formal Feed results include 856,152 continuous requests and 196,612 cold mixed requests without failures. The independently corrected MySQL/MinIO recovery verifies every row in 23 tables (2,003,037 rows) and all 131 referenced objects: 58.47s to readiness and 63.16s through business reads and resumable-upload completion, with zero loss at the explicitly stopped-write snapshot boundary. The 20,000 seeded videos share 100 media objects; this is not a 20,000-file storage benchmark.

The v1.0 scope has passed signoff: [PR #64](https://github.com/liuzhouquan/GoStream/pull/64) and the reliability follow-up [PR #65](https://github.com/liuzhouquan/GoStream/pull/65) are merged with eight successful CI jobs for each tested head and matching merge trees. See the [current delivery status](docs/DELIVERY_STATUS.md) for exact commits, CI evidence and remaining historical diagnostic uncertainties, and the [A01–A12 acceptance index](docs/RELEASE_ACCEPTANCE.md) for verification coverage.

Historical development handoffs and scope criteria are preserved in [SDD and task checklist](docs/SDD.md), [delivery scope](docs/DELIVERY_SCOPE.md), [test standards](docs/TEST_STANDARD.md), and [PROGRESS.md](PROGRESS.md). The machine-readable contract is documented and dynamically validated in [OpenAPI 3.0.3](docs/openapi.yaml). For operations, migration runbooks, and disaster recovery procedures, consult [OPERATIONS.md](docs/OPERATIONS.md).

## Local requirements

- Go 1.27.2 (the project toolchain declared in `go.mod`)
- Node.js is not required for the current static web client
- Docker is optional; Docker Compose runs the API with MySQL, Redis and RabbitMQ. An `observability` profile adds Prometheus and Grafana. The object-storage overlay adds a private MinIO bucket.

The default local profile listens only on `127.0.0.1:8080` and stores data in `./data/gostream.db` using SQLite with WAL, a five-second busy timeout, and foreign-key enforcement. It needs no external services. Run:

```sh
go run ./cmd/gostream
```

Open <http://localhost:8080>. Configure `ADDR`, `DB_DRIVER`, `DB_DSN`, `JWT_SECRET`, `MEDIA_DIR`, and optionally `REDIS_ADDR` with environment variables. For MySQL, `DB_MAX_OPEN_CONNS` and `DB_MAX_IDLE_CONNS` configure the API's `database/sql` pool (defaults: 50 open, 25 idle; idle cannot exceed open). `/metrics` reports pool waits and connections closed because the idle cap was exceeded. The built-in development signing key works only for a direct loopback listener. Set a unique `JWT_SECRET` when using a public listener or a reverse proxy, including a proxy that forwards to loopback. Pending chunks live beside the media directory at `<MEDIA_DIR>.chunks`. Set `FFMPEG_BIN` to an installed FFmpeg executable to enable asynchronous MP4 transcoding and JPEG cover generation; `FFPROBE_BIN` optionally selects the ffprobe executable. With processing enabled, source videos are limited to 10 minutes and 3840×2160 pixels total, with neither side above 4096 pixels. Without FFmpeg, uploaded videos publish without transcoding or probed metadata. Compose installs FFmpeg and enables the worker.

Before starting Compose on a new checkout, generate one persistent secret in the ignored `.env` file:

```sh
[ -f .env ] || printf 'JWT_SECRET=%s\n' "$(openssl rand -hex 32)" > .env
docker compose up --build
```

Keep that value across restarts and API replicas; changing it invalidates existing access tokens. If `.env` already exists, add a unique `JWT_SECRET` there instead of replacing the file. Use `docker compose --profile observability up --build` for Prometheus on port 9090 and Grafana on port 3000. Set `GRAFANA_ADMIN_PASSWORD` for the latter.

Compose also starts RabbitMQ 4.3.7; its management UI is at <http://localhost:15672> (local user `gostream`, password from `RABBITMQ_PASSWORD`, default `gostream-local-rabbitmq-secret`). Use a URL-safe generated password, such as hexadecimal, when overriding it. Outside Compose, set `RABBITMQ_URL` and optionally `RABBITMQ_QUEUE` to enable the same transport; no URL keeps the SQLite development path independent of a broker. Broker downtime leaves notifications in SQL and pending source events in outbox until retry or dead-letter state. `/ready` continues to check SQL only. See [RabbitMQ operations](docs/RABBITMQ.md) before replaying dead events.

The API gives each HTTP request at most 10 minutes to send its headers and body. A slow direct upload can time out; use resumable 4 MiB chunks and retry the affected chunk. WebSocket connections use their own heartbeat deadline after upgrade.

To run the MySQL/Redis/API stack with MinIO, use a fresh Compose project so existing local-media URLs do not point at objects that have not been imported:

```sh
docker compose -p gostream-object -f docker-compose.yml -f docker-compose.object.yml up -d --build
```

The web client stays at <http://localhost:8080>; the MinIO console is at <http://localhost:9001>. The overlay builds a pinned MinIO community server from source, so its first build takes longer. Set `MINIO_ROOT_USER` and `MINIO_ROOT_PASSWORD` before using the stack outside a disposable local environment. `MEDIA_STORE=minio` makes API replicas share one private bucket for uploaded chunks, original videos, transcoded MP4 files, and JPEG covers. `GET /media/:name` streams objects through the API and supports HTTP Range requests. The default `MEDIA_STORE=local` remains available for single-instance development. For an existing local database, stop writers and run the verified `gostream-media import-local` command before switching storage; [media operations](docs/MEDIA_OPERATIONS.md) gives the exact cutover and SQL/object backup sequence. `/ready` checks the database, so it can stay healthy while MinIO is unavailable; object requests then fail and can be retried. See [media design](docs/TECHNICAL_DESIGN.md) and [object tests](docs/TEST_STANDARD.md).

The normal and object overlays both bind API port 8080; stop the normal stack before starting the object stack, or use the isolated two-replica ports in [the test procedure](docs/TEST_STANDARD.md).

Existing installations upgrading to schema v005 must stop all API replicas and back up SQL before starting the new binary. Older replicas cannot read compressed receipts; do not mix old and new versions. Downgrade requires expanding ranges or restoring a pre-upgrade backup. See [receipt lifecycle and migration](docs/DEVICE_ACK_RANGES.md).

## Current design

- Go 1.27.2, Gin, GORM, SQLite for zero-service local development
- MySQL and Redis Compose profile, verified with real end-to-end requests
- Frozen SQLite/MySQL `schema_v001` SQL, v002 outbox index, v003 durable Feed fanout/inbox tables, v004 indexed pull/backfill upgrade, and v005 lossless per-device ACK ranges, with MySQL startup locking and existing-database adoption; an isolated MySQL backup/restore and v002 rollback drill is in `tests/integration/mysql_recovery.sh`
- CI workflow for exact Go version, tests, race checks, vet, build, web syntax, and Compose validation
- Optional Redis cache for anonymous timeline/recommend/hot pages and each viewer's following first page; database fallback and bounded staleness
- Following Feed push/pull index with durable 128-follower batches for authors up to the configured follower threshold and direct reads for higher-follower authors; `FEED_FANOUT_MAX_FOLLOWERS=1000` is the measured default and `0` selects SQL-only. [Design and measured limits](docs/FEED_HYBRID.md)
- Explainable recommendation ranking using interactions, freshness, followed authors, and recently interacted authors
- JWT access tokens and bcrypt password hashing
- Per-peer registration, login, and view-request budgets with HTTP 429 and `Retry-After`; Redis shares counts across instances and a bounded local counter covers outages. See [API semantics](docs/API.md).
- Local or MinIO media storage, single-file and 4 MiB resumable chunk uploads, media ownership checks, file integrity checks, and an offline media import/backup/restore tool
- Durable FFmpeg processing jobs in Compose: source video limits, leased retries, H.264 MP4 output with AAC for audio inputs, generated JPEG cover, and persisted output duration/dimensions
- Author-visible processing failure reasons and hourly cleanup of old unregistered files and unreferenced assets registered over seven days ago
- Creator-page retry controls for transient processing failures and replacement uploads for invalid files
- Database outbox with transactional notification writes, leased dispatch, retry, and dead-letter state
- Paged notification inbox with bounded mark-read updates, unread counts, and WebSocket push
- User and video search with independent ID cursor pages, literal wildcard matching, and playable web results
- One-level comment threads with separate root/reply cursor pages, lazy loading in the web client, and reply notifications
- Direct/group WebSocket messaging, paged direct history, batch recipient ACKs, per-device direct read state and group cursors, bounded device registration with idle cleanup, [lossless adjacent-ID receipt compaction](docs/DEVICE_ACK_RANGES.md), offline replay, and optional Redis cross-instance event relay and presence
- Real two-process IM recovery tests with MySQL/Redis, SIGKILL, per-device ACKs and 105-message backlog synchronization; [guarantees and reproduction](docs/IM.md)
- Multiple WebSocket connections per account (up to eight per API instance), with committed direct/group messages sent to each active device and control replies kept on the originating connection
- `/health`, database-backed `/ready`, `/metrics`, and optional `PPROF_ADDR`
- SQLite is persistent and real; it is not a mock database
- RabbitMQ confirmed publication, durable consumer deduplication, bounded delayed retry and dead letters; [design, recovery and real tests](docs/RABBITMQ.md). Durable broker-backed group push remains a later stage.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/API.md](docs/API.md), [docs/DATABASE.md](docs/DATABASE.md), [docs/TECHNICAL_DESIGN.md](docs/TECHNICAL_DESIGN.md), [docs/LEARNING_GUIDE.md](docs/LEARNING_GUIDE.md), and [docs/INTERVIEW_PREP.md](docs/INTERVIEW_PREP.md).

Performance and verification status is recorded in [docs/PERFORMANCE.md](docs/PERFORMANCE.md) and [docs/RELEASE_ACCEPTANCE.md](docs/RELEASE_ACCEPTANCE.md). On final source `8bef0c5` (`CLOSE-01` / Gate A04), the 24-round formal Compose matrix (`2,002` users / `20,001` published videos) completed 856,152 requests with 0 failures and P95 $\le$ 103.15 ms / P99 $\le$ 131.54 ms ([`feed-warm-cache-continuous-2026-11-10.json`](docs/benchmarks/feed-warm-cache-continuous-2026-11-10.json)), while the isolated MySQL 8.4 cold-SQL 70/20/10 mixed read/write benchmark (`196,612` requests, `120` publishes, 0 failures) passed the 20% Following P95 gate with **34.37% improvement** (`34.016 ms` SQL-only vs `22.325 ms` Hybrid) and cold SQL `feed_recommend` P95 `69.105–87.579 ms` ([`feed-a04-mixed-2026-11-11.json`](docs/benchmarks/feed-a04-mixed-2026-11-11.json)), alongside unchanged-path fanout drain ([`feed-fanout-sustained-drain-30s-2026-11-05.json`](docs/benchmarks/feed-fanout-sustained-drain-30s-2026-11-05.json); historical 2026-11-04 run `#41803621425` recorded 43% at `46.051 ms` vs `26.250 ms`). `CLOSE-02` / Gate A10 formal disaster recovery metrics are archived in [`close-02-recovery-report-2026-11-10.json`](docs/benchmarks/close-02-recovery-report-2026-11-10.json). k6 and multi-host load testing remain outside current scope.

Long validation commands can use the [development timing monitor](docs/DEVELOPMENT_TIMING.md) to report active tests, command deadlines, and GitHub Actions step durations.

## License

GoStream is licensed under the MIT License; see [LICENSE](LICENSE).

The optional MinIO deployment builds the MinIO server from its pinned upstream source under AGPL-3.0. The Go MinIO client dependency is Apache-2.0. Their licenses and notices are separate from GoStream's MIT license; consult those components before redistributing a combined deployment image.

Image validation uses `golang.org/x/image` under BSD-3-Clause. Its [license notice](third_party_licenses/golang.org-x-image-LICENSE) is also copied into the API image.

RabbitMQ delivery uses the RabbitMQ-maintained Go client under BSD-2-Clause; its [license notice](third_party_licenses/rabbitmq-amqp091-go-LICENSE) is included in the API image.

OpenAPI test validation uses [kin-openapi](https://github.com/getkin/kin-openapi), under MIT; its [notice](third_party_licenses/kin-openapi-LICENSE) is retained. This validation dependency is used by tests.
