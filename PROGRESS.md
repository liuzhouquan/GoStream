# GoStream development progress

Last updated: 2026-10-21

## Stage 0 — project foundation

- [x] Use the updated Go 1.27.2 toolchain.
- [x] Create a persistent SQLite profile and MySQL adapter using GORM.
- [x] Implement bcrypt registration/login, JWT auth, profile editing, media upload, videos, timeline/following/score-ranked feeds, follows, likes/favorites, comments, play counts, notifications, text search, and direct-message WebSocket/history/ACK.
- [x] Add a responsive API-driven web UI for auth, feeds, upload, profiles, search/follow, notifications, comments, interactions, and chat.
- [x] Add API and schema docs, MIT licensing for new GoStream code, and tests for social and WebSocket flows.
- [x] Verify startup with persistent SQLite and HTTP requests.
- [x] Split handlers into feature source files and correct the equal-timestamp cursor edge case found by integration testing.
- [x] Bind uploads to their owner, validate media content, and enforce author ownership for video editing/deletion.
- [x] Add resumable 4 MiB chunk uploads, SHA256 integrity checks, session leases, and expired chunk cleanup; connect the web publisher.
- [x] Add notification outbox processing with leasing, retry, and dead-letter state; add realtime notification events.
- [x] Add offline message replay, unread paging, idempotent recipient ACK, and conflict checking for duplicate client IDs.
- [x] Add group membership and owner permissions, group WebSocket messages, history paging, unread/read cursors, local presence and last-seen tracking; verify creation and persisted history in Chrome.
- [x] Add database readiness, Prometheus metrics, an optional pprof listener, and Compose monitoring configuration.
- [x] Add optional Redis caching for anonymous Feed pages, mutation-based version invalidation, fallback on Redis failure, and a regression test using a Redis-compatible test server.
- [x] Relay direct, group, and notification socket events between API instances through optional Redis Pub/Sub and track cross-instance online state with expiring Redis entries; verify two server instances with shared SQL and a Redis-compatible test server.
- [x] Replay up to 100 unacknowledged group messages on WebSocket reconnect and signal when history must be fetched for larger backlogs.
- [x] Add a reproducible SQLite Feed seeder and standard-library HTTP load driver; measure 3,000 local loopback requests for timeline and hot scenes against 1,000 video rows.
- [x] Give recommendation Feed explainable freshness, followed-author and recent interacted-author weights; bind scene and time anchor into its cursor and test ranking/pagination.
- [x] Add opt-in durable FFmpeg processing jobs with conditional leases, retry/dead-letter state, H.264 MP4 output (AAC when audio exists), JPEG covers, owner-only processing visibility and stale-job protection. Enable in Compose and verify with a real uploaded video against MySQL.

## Stages

1. Local runnable application: account/auth, media upload, videos, timeline/recommendation feeds, follows, interactions, comments, search, notifications, basic web client. **Implemented and locally verified.**
2. Reliability: cursor and idempotency review, transactional outbox, retry/dead-letter handling, test hardening and API documentation. **Partly implemented:** a database-backed outbox worker dispatches notifications and records dead letters. The initial handlers still call GORM directly; splitting Domain/Application/Infrastructure layers remains.
3. Feed scale: short-lived Redis page caching, version invalidation, a preference/recency-aware recommendation baseline, and initial MySQL/Redis load measurements are implemented. Per-video cards/stat caching, hybrid fanout, and authenticated recommendation measurements remain.
4. IM: Direct and group WebSocket messages, persistence, offline replay/ACK, group unread cursors, and local online presence are implemented. Both direct and group reconnects replay up to 100 pending messages; larger backlogs require history/unread paging. Optional Redis Pub/Sub relays committed direct, group, and notification events between API instances. Expiring Redis presence entries provide a cross-instance online check when enabled. Pub/Sub delivery is best effort. Cross-device read cursors remain.
5. Media pipeline: local resumable upload and integrity checks are implemented. Compose enables a durable FFmpeg worker that transcodes to MP4 and generates JPEG covers before public Feed publication; local SQLite can opt in through `FFMPEG_BIN`. MinIO/object storage and video metadata limits remain.
6. Operations: MySQL/API/Redis Compose was built with Go 1.27.2 and verified through real registration, upload, publish, Feed, interaction, follow, and notification requests. The optional Prometheus/Grafana profile was started and its scrape target and provisioned dashboard verified. HTTP metrics, readiness, pprof, an in-process benchmark, local SQLite results, and three-run MySQL/Redis cached-versus-uncached results are present. k6 and a multi-host load test remain.

## Verification ledger

Commands run through 2026-10-21 using Go 1.27.2:

- `go test ./...` — passed, including account/video/feed/interaction/follow/notification, upload resume, outbox, direct/group messaging and presence, group reconnect replay, plus Redis cache hit/invalidation/fallback and two-instance socket routing checks using an in-process Redis-compatible test server.
- `go vet ./...` — passed.
- `go test -race ./...` — passed.
- `node --check web/app.js` — passed.
- HTTP smoke run against persistent SQLite — `/health`, `/`, `/app.js`, `/styles.css`, and `/api/feed` returned 200; registration and publishing returned 201; the published video appeared in Feed.
- Isolated end-to-end smoke run on `127.0.0.1:18080` — health 200, registration 201, publish 201, Feed 200 with the video, and login 200.
- Browser automation opened the UI and confirmed its navigation and Feed structure render. Chinese glyphs are missing in this browser image due to current system font setup.
- Mobile browser check at 390×844 confirmed both navigation groups render and the header avatar opens the login dialog. Chinese glyphs are missing in this browser image due to current system font setup.
- On 2026-10-21, Docker Desktop WSL integration was available. `docker compose up -d --build` produced healthy MySQL 8.4, Redis 8.2.9, and API services. Real HTTP requests verified registration, media upload, video publishing, timeline/recommend/following Feeds, like count, follows and notifications; Prometheus reported Redis cache hits. Temporary load-test data was removed, and Compose restarted with an empty database before the media pipeline work.
- `docker compose --profile observability up -d --build` started Prometheus and Grafana. Prometheus's `up` query returned `1` for `api:8080`; Grafana's health endpoint returned `database: ok` and its search API listed the provisioned GoStream API dashboard. The stack remains running for local inspection.
- `GOSTREAM_TEST_MYSQL_DSN=... go test ./internal/platform -run '^TestMySQLRecommendationIntegration$' -count=1` — passed against the real Compose MySQL; preference-aware ranking and three cursor pages matched expected order. The fixture transaction rolled back, and the database remained empty.
- `python3 tests/integration/media_smoke.py --video-file /tmp/gostream-sample.mp4` — passed against Compose MySQL with FFmpeg enabled, including a repeat on the final rebuilt image (video 7). The worker produced a 3,785-byte MP4 and 6,984-byte JPEG cover and published it into public Feed. A prior run exposed MySQL `DATETIME(3)` lease precision mismatch; the worker now truncates lease timestamps to milliseconds, and the earlier stuck test job also completed after its lease was released. Media-demo rows remain in the local database for inspection.
- A same-timestamp keyset test caught and verified the UTC cursor comparison fix; a multipart test checks that upload names are generated server-side.

`BenchmarkTimelineFeed1000` was run three times against a 1,000-video SQLite memory database: 358,682, 362,115, and 373,384 ns/op, with about 93 KB and 881 allocations per operation. These are in-process benchmark results, not QPS or P95 measurements. Details are in `docs/PERFORMANCE.md`.

A built API process was also measured over loopback HTTP against an isolated 1,000-video SQLite file: 3,000 requests at concurrency 10 gave 1,912.3 requests/s and P95 9.949 ms for timeline, and 1,556.9 requests/s and P95 10.885 ms for hot, with zero failures in either single run. The dataset and local limitations are in `docs/PERFORMANCE.md`.

After the recommendation change, a fresh 1,000-video SQLite file yielded 1,634.6 requests/s and P95 10.098 ms for anonymous recommendation, versus 1,891.4 requests/s and P95 8.752 ms for hot on the same process and dataset. Both single runs had 3,000 successful requests at concurrency 10. Authenticated recommendation remains unmeasured.

On the same 1,001-video Compose MySQL dataset, the anonymous recommendation page was measured three times per variant at concurrency 10 and 3,000 requests/run. The cached API container yielded 3,211.4, 3,784.4, and 4,054.1 requests/s; an identical temporary API container with Redis disabled yielded 1,165.3, 1,155.8, and 1,078.8 requests/s. All 18,000 measured requests succeeded. Redis metrics showed 9,058 hits and 7 misses after the tests. These are warm first-page, one-host results, not general platform capacity; see `docs/PERFORMANCE.md`.
