# GoStream development progress

Last updated: 2026-10-22

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
- [x] Cache each authenticated viewer's following first page for five seconds, invalidate on follow/unfollow and shared video-card changes, and test isolation, cursor bypass, and Redis fallback.
- [x] Relay direct, group, and notification socket events between API instances through optional Redis Pub/Sub and track cross-instance online state with expiring Redis entries; verify two server instances with shared SQL and a Redis-compatible test server.
- [x] Replay up to 100 unacknowledged group messages on WebSocket reconnect and signal when history must be fetched for larger backlogs.
- [x] Add a reproducible SQLite Feed seeder and standard-library HTTP load driver; measure 3,000 local loopback requests for timeline and hot scenes against 1,000 video rows.
- [x] Give recommendation Feed explainable freshness, followed-author and recent interacted-author weights; bind scene and time anchor into its cursor and test ranking/pagination.
- [x] Add opt-in durable FFmpeg processing jobs with conditional leases, retry/dead-letter state, H.264 MP4 output (AAC when audio exists), JPEG covers, owner-only processing visibility and stale-job protection. Enable in Compose and verify with a real uploaded video against MySQL.
- [x] Probe source video duration and dimensions before transcoding, permanently reject inputs outside the short-video limits, and persist probed output metadata with publication. Show duration in video cards.
- [x] Expose stable processing failure codes only to the author and show readable reasons in the creator page; reconcile old unregistered media files after a 24-hour grace period with database reference checks and a Prometheus counter.
- [x] Add creator-page controls to retry transient failures or replace invalid video files, and fix a MySQL strict-mode zero-date failure in resumable upload sessions with a nullable lease and a real MySQL regression test.

## Stages

1. Local runnable application: account/auth, media upload, videos, timeline/recommendation feeds, follows, interactions, comments, search, notifications, basic web client. **Implemented and locally verified.**
2. Reliability: cursor and idempotency review, transactional outbox, retry/dead-letter handling, test hardening and API documentation. **Partly implemented:** a database-backed outbox worker dispatches notifications and records dead letters. The initial handlers still call GORM directly; splitting Domain/Application/Infrastructure layers remains.
3. Feed scale: short-lived Redis page caching for anonymous scenes and each viewer's following first page, version invalidation, a preference/recency-aware recommendation baseline, and local MySQL/Redis comparisons for anonymous recommendation and authenticated following first pages are implemented. Per-video cards/stat caching, hybrid fanout, and authenticated recommendation measurements remain.
4. IM: Direct and group WebSocket messages, persistence, offline replay/ACK, group unread cursors, and local online presence are implemented. Both direct and group reconnects replay up to 100 pending messages; larger backlogs require history/unread paging. Optional Redis Pub/Sub relays committed direct, group, and notification events between API instances. Expiring Redis presence entries provide a cross-instance online check when enabled. Pub/Sub delivery is best effort. Cross-device read cursors remain.
5. Media pipeline: local resumable upload and integrity checks are implemented. Compose enables a durable FFmpeg worker with source duration/dimension limits, MP4 transcode, JPEG cover generation, and output metadata persisted before public Feed publication; local SQLite can opt in through `FFMPEG_BIN`. A periodic worker removes old generated files without database references. MinIO/object storage, frame-by-frame validation, and garbage collection of registered but unused assets remain.
6. Operations: MySQL/API/Redis Compose was built with Go 1.27.2 and verified through real registration, upload, publish, Feed, interaction, follow, and notification requests. The optional Prometheus/Grafana profile was started and its scrape target and provisioned dashboard verified. HTTP metrics, readiness, pprof, an in-process benchmark, local SQLite results, and three-run MySQL/Redis cached-versus-uncached results are present. k6 and a multi-host load test remain.

## Verification ledger

Commands run through 2026-10-22 using Go 1.27.2:

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
- `go test ./...`, `go test -race ./...`, `go vet ./...`, `node --check web/app.js`, `docker compose config --quiet`, and `git diff --check` — passed after the following-cache and authenticated load-driver changes.
- The rebuilt Compose API passed a real MySQL/Redis following Feed smoke: repeat read increased `gostream_feed_cache_total{result="hit"}`, and unfollow immediately removed the author's video from the page. The temporary viewer and related rows were deleted.
- A temporary 1,000-video author and viewer were used for three alternating 3,000-request runs per variant at concurrency 10. All 18,000 authenticated following requests succeeded. Cached throughput was 3,199.6–3,487.6 requests/s; uncached was 869.7–902.0 requests/s. Full conditions and latency figures are in `docs/PERFORMANCE.md`. The fixtures, token file, and temporary container were removed; the API remained ready.
- After the ffprobe change, `go test ./...`, `go test -race ./...`, `go vet ./...`, `node --check web/app.js`, Python syntax and Compose config checks passed. The rebuilt API migrated the MySQL video table and processed a real 1-second upload (video 1008), storing `duration_ms=1000`, `width=160`, and `height=90`; the result appeared in Feed. A generated 601-second, 16×16 MP4 was rejected through real API upload/publish: the job had `attempts=1`, `dead_at` set, and an explicit duration error. The rejected fixture rows and files were removed.
- A same-timestamp keyset test caught and verified the UTC cursor comparison fix; a multipart test checks that upload names are generated server-side.
- After failure-code and orphan-reconciliation changes, `go test ./...`, `go test -race ./...`, `go vet ./...`, `node --check web/app.js`, Python bytecode compilation, Compose config validation, and `git diff --check` passed. The unit tests cover stable error codes, owner-only visibility, database errors, asset/video/job/avatar references, recent files, symlinks, and the 100-file batch boundary.
- Rebuilt the Compose API on 2026-10-22. `tests/integration/media_failure_smoke.py` uploaded a synthetic 601-second MP4: video 1010 failed with author-only `duration_limit`; its MySQL job showed one attempt and a dead-letter state. The fixture user, row, and file were removed. `tests/integration/media_smoke.py` then published a real one-second fixture as video 1011 with an MP4, JPEG cover, and 1000 ms/160×90 metadata.
- A controlled two-day-old unregistered file in the Compose media volume was removed on API restart; `/ready` passed and `gostream_media_reconciliation_total{result="removed"}` increased to 1. The controlled file was verified absent after startup.
- A real browser retry initially exposed MySQL error 1292: resumable upload creation tried to insert a zero `lease_until` datetime. After using a nullable lease and clearing it on rollback/completion, the rebuilt Compose schema showed `lease_until datetime(3) NULL`. `TestMySQLResumableUploadLeaseIntegration` passed create, incomplete completion/release, chunk upload, and completed lease checks against the real MySQL instance; its transaction rolled back. `go test ./...`, `go test -race ./...`, `go vet ./...`, `node --check web/app.js`, Compose config validation, and `git diff --check` passed afterward.
- In an isolated browser session, a 601-second video failed with owner-only `duration_limit`; the creator page showed the readable reason and replacement form but hid same-source retry. Selecting a valid MP4 ran the resumable uploader and changed the video from failed to processing, then published after a status refresh with 1000 ms/160×90 metadata. The fixture account, video, jobs, assets, session, and media files were removed; `/ready` still passed.

`BenchmarkTimelineFeed1000` was run three times against a 1,000-video SQLite memory database: 358,682, 362,115, and 373,384 ns/op, with about 93 KB and 881 allocations per operation. These are in-process benchmark results, not QPS or P95 measurements. Details are in `docs/PERFORMANCE.md`.

A built API process was also measured over loopback HTTP against an isolated 1,000-video SQLite file: 3,000 requests at concurrency 10 gave 1,912.3 requests/s and P95 9.949 ms for timeline, and 1,556.9 requests/s and P95 10.885 ms for hot, with zero failures in either single run. The dataset and local limitations are in `docs/PERFORMANCE.md`.

After the recommendation change, a fresh 1,000-video SQLite file yielded 1,634.6 requests/s and P95 10.098 ms for anonymous recommendation, versus 1,891.4 requests/s and P95 8.752 ms for hot on the same process and dataset. Both single runs had 3,000 successful requests at concurrency 10. Authenticated recommendation remains unmeasured.

On the same 1,001-video Compose MySQL dataset, the anonymous recommendation page was measured three times per variant at concurrency 10 and 3,000 requests/run. The cached API container yielded 3,211.4, 3,784.4, and 4,054.1 requests/s; an identical temporary API container with Redis disabled yielded 1,165.3, 1,155.8, and 1,078.8 requests/s. All 18,000 measured requests succeeded. Redis metrics showed 9,058 hits and 7 misses after the tests. These are warm first-page, one-host results, not general platform capacity; see `docs/PERFORMANCE.md`.
