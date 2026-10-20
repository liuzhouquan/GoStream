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

Open <http://localhost:8080>. Configure `ADDR`, `DB_DRIVER`, `DB_DSN`, `JWT_SECRET`, `MEDIA_DIR`, and optionally `REDIS_ADDR` with environment variables. Set a unique `JWT_SECRET` outside local development. Pending chunks live beside the media directory at `<MEDIA_DIR>.chunks`. The MySQL/Redis Compose profile starts with `docker compose up --build`; use `docker compose --profile observability up --build` for Prometheus on port 9090 and Grafana on port 3000. Set `GRAFANA_ADMIN_PASSWORD` for the latter.

## Current design

- Go 1.27.2, Gin, GORM, SQLite for zero-service local development
- MySQL driver available for the container profile
- Optional Redis cache for anonymous timeline/recommend/hot Feed pages; database fallback and bounded staleness
- JWT access tokens and bcrypt password hashing
- Local media storage, single-file and 4 MiB resumable chunk uploads, media ownership checks, and file integrity checks
- Database outbox with transactional notification writes, leased dispatch, retry, and dead-letter state
- Direct/group WebSocket messaging, persisted ACKs, offline direct-message replay, group unread cursors, and optional Redis cross-instance event relay and presence
- `/health`, database-backed `/ready`, `/metrics`, and optional `PPROF_ADDR`
- SQLite is persistent and real; it is not a mock database
- RabbitMQ, MinIO, FFmpeg, and durable cross-instance group push are later stages

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/API.md](docs/API.md), [docs/DATABASE.md](docs/DATABASE.md), [docs/TECHNICAL_DESIGN.md](docs/TECHNICAL_DESIGN.md), [docs/LEARNING_GUIDE.md](docs/LEARNING_GUIDE.md), and [docs/INTERVIEW_PREP.md](docs/INTERVIEW_PREP.md).

Performance and verification status is recorded in [docs/PERFORMANCE.md](docs/PERFORMANCE.md). A measured in-process Feed benchmark is included; Docker integration and k6 load testing remain pending.

## License

GoStream is licensed under the MIT License; see [LICENSE](LICENSE).
