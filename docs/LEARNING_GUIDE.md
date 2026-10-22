# Learning guide

Read the current implementation in this order:

1. `cmd/gostream`: configuration, database startup, and dependency assembly.
2. `internal/platform/models.go`: persisted entities, uniqueness constraints, and indexes.
3. `internal/platform/server.go`: route registration, authentication middleware, and server setup.
4. `internal/platform/accounts.go`, `videos.go`, `uploads.go`, `video_probe.go`, `video_processing.go`, `media_reconcile.go`, `feed.go`, `recommend.go`, and `social.go`: trace an HTTP request through validation, media source limits and output metadata, job failure reporting, orphan cleanup, recommendation weights, transaction and response.
5. `internal/platform/outbox.go`: follow a committed notification through lease, retry and dead-letter states.
6. `internal/platform/device.go`, `hub.go`, `messages.go`, `groups.go`, and `realtime.go`: trace device IDs, bounded registration and idle cleanup, direct/group WebSocket persistence, direct-history keyset pagination, per-device ACK/cursors, membership, multi-device delivery, origin-only ACK/replay, and Redis relay. Compare `device_test.go`, `messages_test.go`, `hub_multidevice_test.go`, and `realtime_test.go`.
7. `internal/platform/feed_cache.go`, `feed_cache_test.go`, `recommend_test.go`, `mysql_integration_test.go`, `metrics.go`, `server_test.go`, and `feed_bench_test.go`: trace ranking, cache invalidation, SQLite/MySQL verification and measured behavior.
8. `web/app.js`: follow API-driven demonstration flows, especially failed-video retry through resumable upload, video PATCH, and status refresh; compare with `TestMySQLResumableUploadLeaseIntegration` to see why real database checks matter.
9. `docs/ARCHITECTURE.md`, `TECHNICAL_DESIGN.md`, then the remaining guides.

The current package keeps handlers, business rules, and GORM queries together. A useful next exercise is to move transaction rules into application services and queries behind repository interfaces while preserving the tests. Compare the current Redis page cache and later fanout changes against the recorded benchmark and a future k6 run.
