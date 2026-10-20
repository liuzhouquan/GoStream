# Learning guide

Read the current implementation in this order:

1. `cmd/gostream`: configuration, database startup, and dependency assembly.
2. `internal/platform/models.go`: persisted entities, uniqueness constraints, and indexes.
3. `internal/platform/server.go`: route registration, authentication middleware, and server setup.
4. `internal/platform/accounts.go`, `videos.go`, `uploads.go`, `feed.go`, and `social.go`: trace an HTTP request through validation, transaction and response.
5. `internal/platform/outbox.go`: follow a committed notification through lease, retry and dead-letter states.
6. `internal/platform/hub.go`, `messages.go`, `groups.go`, and `realtime.go`: trace direct/group WebSocket persistence, membership, local delivery, Redis relay, replay and ACK.
7. `internal/platform/feed_cache.go`, `feed_cache_test.go`, `metrics.go`, `server_test.go`, and `feed_bench_test.go`: trace cache invalidation, fallback, verification and measured behavior.
8. `web`: API-driven demonstration flows.
9. `docs/ARCHITECTURE.md`, `TECHNICAL_DESIGN.md`, then the remaining guides.

The current package keeps handlers, business rules, and GORM queries together. A useful next exercise is to move transaction rules into application services and queries behind repository interfaces while preserving the tests. Compare the current Redis page cache and later fanout changes against the recorded benchmark and a future k6 run.
