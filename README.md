# GoStream

GoStream is a learning project for a short-video social platform written in Go. Its first runnable version is a single API process with a durable database and web client. Redis, message-broker workers, object storage, and separated service layers are later stages.

## Status

This repository is under active staged development. See [PROGRESS.md](PROGRESS.md) for implemented, verified, and planned work. Do not treat planned modules as implemented features.

## Local requirements

- Go 1.27.2 (the project toolchain declared in `go.mod`)
- Node.js is not required for the current static web client
- Docker is optional; Docker Compose runs the API with MySQL. Redis and RabbitMQ integration remain future stages.

The default local profile stores data in `./data/gostream.db` using SQLite. It needs no external services. Run:

```sh
go run ./cmd/gostream
```

Open <http://localhost:8080>. Configure `ADDR`, `DB_DRIVER`, `DB_DSN`, `JWT_SECRET`, and `MEDIA_DIR` with environment variables. Set a unique `JWT_SECRET` outside local development. The MySQL Compose profile starts with `docker compose up --build`.

## Current design

- Go 1.27.2, Gin, GORM, SQLite for zero-service local development
- MySQL driver available for the container profile
- JWT access tokens and bcrypt password hashing
- Local media storage, with a storage interface planned before object-storage support
- SQLite is persistent and real; it is not a mock database
- Redis and RabbitMQ are integration targets for later phases; current code must not claim their behavior until adapters and failure handling are implemented

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/API.md](docs/API.md), [docs/DATABASE.md](docs/DATABASE.md), [docs/TECHNICAL_DESIGN.md](docs/TECHNICAL_DESIGN.md), [docs/LEARNING_GUIDE.md](docs/LEARNING_GUIDE.md), and [docs/INTERVIEW_PREP.md](docs/INTERVIEW_PREP.md).

Performance and verification status is recorded in [docs/PERFORMANCE.md](docs/PERFORMANCE.md). Current scope does not yet include chunk-resume upload, transcoding, groups, broker-backed IM, Redis caching, RabbitMQ workers, or Prometheus/Grafana.

## License

GoStream is licensed under the MIT License; see [LICENSE](LICENSE).
