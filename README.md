# GoStream

GoStream is a runnable Go short-video social platform with a static web client, persistent database, resumable media uploads, direct and group messaging, and a database-backed notification worker. It runs as one API process by default; Redis can relay realtime events between API instances.

## Status

This repository is under active staged development. See [delivery scope](docs/DELIVERY_SCOPE.md), [test and acceptance standards](docs/TEST_STANDARD.md), and [PROGRESS.md](PROGRESS.md) for the target release, evidence requirements, and current status. Do not treat planned modules as implemented features.

## Local requirements

- Go 1.27.2 (the project toolchain declared in `go.mod`)
- Node.js is not required for the current static web client
- Docker is optional; Docker Compose runs the API with MySQL and Redis. An `observability` profile adds Prometheus and Grafana. The object-storage overlay adds a private MinIO bucket.

The default local profile stores data in `./data/gostream.db` using SQLite with WAL, a five-second busy timeout, and foreign-key enforcement. It needs no external services. Run:

```sh
go run ./cmd/gostream
```

Open <http://localhost:8080>. Configure `ADDR`, `DB_DRIVER`, `DB_DSN`, `JWT_SECRET`, `MEDIA_DIR`, and optionally `REDIS_ADDR` with environment variables. Set a unique `JWT_SECRET` outside local development. Pending chunks live beside the media directory at `<MEDIA_DIR>.chunks`. Set `FFMPEG_BIN` to an installed FFmpeg executable to enable asynchronous MP4 transcoding and JPEG cover generation; `FFPROBE_BIN` optionally selects the ffprobe executable. With processing enabled, source videos are limited to 10 minutes and 3840×2160 pixels total, with neither side above 4096 pixels. Without FFmpeg, uploaded videos publish without transcoding or probed metadata. Compose installs FFmpeg and enables the worker. The MySQL/Redis Compose profile starts with `docker compose up --build`; use `docker compose --profile observability up --build` for Prometheus on port 9090 and Grafana on port 3000. Set `GRAFANA_ADMIN_PASSWORD` for the latter.

To run the MySQL/Redis/API stack with MinIO, use a fresh Compose project so existing local-media URLs do not point at objects that have not been imported:

```sh
docker compose -p gostream-object -f docker-compose.yml -f docker-compose.object.yml up -d --build
```

The web client stays at <http://localhost:8080>; the MinIO console is at <http://localhost:9001>. The overlay builds a pinned MinIO community server from source, so its first build takes longer. Set `MINIO_ROOT_USER` and `MINIO_ROOT_PASSWORD` before using the stack outside a disposable local environment. `MEDIA_STORE=minio` makes API replicas share one private bucket for uploaded chunks, original videos, transcoded MP4 files, and JPEG covers. `GET /media/:name` streams objects through the API and supports HTTP Range requests. The default `MEDIA_STORE=local` remains available for single-instance development. For an existing local database, stop writers and run the verified `gostream-media import-local` command before switching storage; [media operations](docs/MEDIA_OPERATIONS.md) gives the exact cutover and SQL/object backup sequence. `/ready` checks the database, so it can stay healthy while MinIO is unavailable; object requests then fail and can be retried. See [media design](docs/TECHNICAL_DESIGN.md) and [object tests](docs/TEST_STANDARD.md).

The normal and object overlays both bind API port 8080; stop the normal stack before starting the object stack, or use the isolated two-replica ports in [the test procedure](docs/TEST_STANDARD.md).

## Current design

- Go 1.27.2, Gin, GORM, SQLite for zero-service local development
- MySQL and Redis Compose profile, verified with real end-to-end requests
- Frozen SQLite/MySQL `schema_v001` SQL, v002 outbox index, v003 durable Feed fanout/inbox tables, and v004 indexed pull/backfill upgrade, with MySQL startup locking and existing-database adoption; an isolated MySQL backup/restore and v002 rollback drill is in `tests/integration/mysql_recovery.sh`
- CI workflow for exact Go version, tests, race checks, vet, build, web syntax, and Compose validation
- Optional Redis cache for anonymous timeline/recommend/hot pages and each viewer's following first page; database fallback and bounded staleness
- Following Feed push/pull index with durable 128-follower batches for ordinary authors and direct reads for higher-follower authors; configure `FEED_FANOUT_MAX_FOLLOWERS` (`0` SQL-only default, `1000` to enable the current hybrid strategy). [Design and measured limits](docs/FEED_HYBRID.md)
- Explainable recommendation ranking using interactions, freshness, followed authors, and recently interacted authors
- JWT access tokens and bcrypt password hashing
- Local or MinIO media storage, single-file and 4 MiB resumable chunk uploads, media ownership checks, file integrity checks, and an offline media import/backup/restore tool
- Durable FFmpeg processing jobs in Compose: source video limits, leased retries, H.264 MP4 output with AAC for audio inputs, generated JPEG cover, and persisted output duration/dimensions
- Author-visible processing failure reasons and hourly cleanup of old unregistered files and unreferenced assets registered over seven days ago
- Creator-page retry controls for transient processing failures and replacement uploads for invalid files
- Database outbox with transactional notification writes, leased dispatch, retry, and dead-letter state
- Paged notification inbox with bounded mark-read updates, unread counts, and WebSocket push
- User and video search with independent ID cursor pages, literal wildcard matching, and playable web results
- One-level comment threads with separate root/reply cursor pages, lazy loading in the web client, and reply notifications
- Direct/group WebSocket messaging, paged direct history, batch recipient ACKs, per-device direct read state and group cursors, bounded device registration with idle cleanup, offline replay, and optional Redis cross-instance event relay and presence
- Multiple WebSocket connections per account (up to eight per API instance), with committed direct/group messages sent to each active device and control replies kept on the originating connection
- `/health`, database-backed `/ready`, `/metrics`, and optional `PPROF_ADDR`
- SQLite is persistent and real; it is not a mock database
- RabbitMQ broker integration and durable cross-instance group push are later stages

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/API.md](docs/API.md), [docs/DATABASE.md](docs/DATABASE.md), [docs/TECHNICAL_DESIGN.md](docs/TECHNICAL_DESIGN.md), [docs/LEARNING_GUIDE.md](docs/LEARNING_GUIDE.md), and [docs/INTERVIEW_PREP.md](docs/INTERVIEW_PREP.md).

Performance and verification status is recorded in [docs/PERFORMANCE.md](docs/PERFORMANCE.md). An in-process benchmark, a local HTTP driver with authenticated request support, three-run MySQL/Redis cache comparisons, and an isolated 2,000-user hybrid Feed comparison are included. The hybrid read path regressed against SQL-only for the measured low-follow-count viewer; it remains an optimization target. k6 and multi-host load testing remain pending.

## License

GoStream is licensed under the MIT License; see [LICENSE](LICENSE).

The optional MinIO deployment builds the MinIO server from its pinned upstream source under AGPL-3.0. The Go MinIO client dependency is Apache-2.0. Their licenses and notices are separate from GoStream's MIT license; consult those components before redistributing a combined deployment image.

Image validation uses `golang.org/x/image` under BSD-3-Clause. Its [license notice](third_party_licenses/golang.org-x-image-LICENSE) is also copied into the API image.
