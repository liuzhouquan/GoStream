# GoStream

GoStream is a runnable Go short-video social platform with a static web client, persistent database, resumable media uploads, direct and group messaging, and a database-backed notification worker. It runs as one API process by default; Redis can relay realtime events between API instances.

## Status

This repository is under active staged development. See [PROGRESS.md](PROGRESS.md) for implemented, verified, and planned work. Do not treat planned modules as implemented features.

## Local requirements

- Go 1.27.2 (the project toolchain declared in `go.mod`)
- Node.js is not required for the current static web client
- Docker is optional; Docker Compose runs the API with MySQL and Redis. An `observability` profile adds Prometheus and Grafana.

The default local profile stores data in `./data/gostream.db` using SQLite. It needs no external services. Run:

```sh
go run ./cmd/gostream
```

Open <http://localhost:8080>. Configure `ADDR`, `DB_DRIVER`, `DB_DSN`, `JWT_SECRET`, `MEDIA_DIR`, and optionally `REDIS_ADDR` with environment variables. Set a unique `JWT_SECRET` outside local development. Pending chunks live beside the media directory at `<MEDIA_DIR>.chunks`. Set `FFMPEG_BIN` to an installed FFmpeg executable to enable asynchronous MP4 transcoding and JPEG cover generation; `FFPROBE_BIN` optionally selects the ffprobe executable. With processing enabled, source videos are limited to 10 minutes and 3840×2160 pixels total, with neither side above 4096 pixels. Without FFmpeg, uploaded videos publish without transcoding or probed metadata. Compose installs FFmpeg and enables the worker. The MySQL/Redis Compose profile starts with `docker compose up --build`; use `docker compose --profile observability up --build` for Prometheus on port 9090 and Grafana on port 3000. Set `GRAFANA_ADMIN_PASSWORD` for the latter.

## Current design

- Go 1.27.2, Gin, GORM, SQLite for zero-service local development
- MySQL and Redis Compose profile, verified with real end-to-end requests
- Optional Redis cache for anonymous timeline/recommend/hot pages and each viewer's following first page; database fallback and bounded staleness
- Explainable recommendation ranking using interactions, freshness, followed authors, and recently interacted authors
- JWT access tokens and bcrypt password hashing
- Local media storage, single-file and 4 MiB resumable chunk uploads, media ownership checks, and file integrity checks
- Durable FFmpeg processing jobs in Compose: source video limits, leased retries, H.264 MP4 output with AAC for audio inputs, generated JPEG cover, and persisted output duration/dimensions
- Author-visible processing failure reasons and hourly cleanup of old generated files left without database references
- Creator-page retry controls for transient processing failures and replacement uploads for invalid files
- Database outbox with transactional notification writes, leased dispatch, retry, and dead-letter state
- Direct/group WebSocket messaging, persisted ACKs, offline direct-message replay, group unread cursors, and optional Redis cross-instance event relay and presence
- `/health`, database-backed `/ready`, `/metrics`, and optional `PPROF_ADDR`
- SQLite is persistent and real; it is not a mock database
- RabbitMQ, MinIO, and durable cross-instance group push are later stages

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/API.md](docs/API.md), [docs/DATABASE.md](docs/DATABASE.md), [docs/TECHNICAL_DESIGN.md](docs/TECHNICAL_DESIGN.md), [docs/LEARNING_GUIDE.md](docs/LEARNING_GUIDE.md), and [docs/INTERVIEW_PREP.md](docs/INTERVIEW_PREP.md).

Performance and verification status is recorded in [docs/PERFORMANCE.md](docs/PERFORMANCE.md). An in-process benchmark, a local HTTP driver with authenticated request support, and three-run MySQL/Redis Compose comparisons for anonymous recommendation and authenticated following pages are included. k6 and multi-host load testing remain pending.

## License

GoStream is licensed under the MIT License; see [LICENSE](LICENSE).
