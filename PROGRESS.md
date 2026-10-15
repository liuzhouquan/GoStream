# GoStream development progress

Last updated: 2026-10-15

## Stage 0 — project foundation

- [x] Use the updated Go 1.27.2 toolchain.
- [x] Create a persistent SQLite profile and MySQL adapter using GORM.
- [x] Implement bcrypt registration/login, JWT auth, profile editing, media upload, videos, timeline/following/score-ranked feeds, follows, likes/favorites, comments, play counts, notifications, text search, and direct-message WebSocket/history/ACK.
- [x] Add a responsive API-driven web UI for auth, feeds, upload, profiles, search/follow, notifications, comments, interactions, and chat.
- [x] Add API and schema docs, MIT licensing for new GoStream code, and tests for social and WebSocket flows.
- [x] Verify startup with persistent SQLite and HTTP requests.
- [x] Split handlers into feature source files and correct the equal-timestamp cursor edge case found by integration testing.

## Stages

1. Local runnable application: account/auth, media upload, videos, timeline/recommendation feeds, follows, interactions, comments, search, notifications, basic web client. **Implemented and locally verified.**
2. Reliability: cursor and idempotency review, transactional outbox, retry/dead-letter handling, test hardening and API documentation. **In progress:** cursor and state idempotency are covered; API docs and integration tests exist; the outbox model is only reserved and has no delivery worker. The initial handlers still call GORM directly; splitting Domain/Application/Infrastructure layers remains.
3. Feed scale: Redis page/card/stat caching, hybrid fanout and cache consistency.
4. IM: WebSocket connection hub, message persistence, offline history and ACK are implemented for a single process. Group chat, online status, reconnect delivery protocol, and multi-instance broker adapter remain.
5. Media pipeline: resumable multipart uploads, FFmpeg processing, MinIO adapter.
6. Operations: a MySQL + API Compose profile and k6 starter script are present but not run here; Redis/RabbitMQ integration, metrics, dashboards, and a measured report remain.

## Verification ledger

Commands run on 2026-10-15 using Go 1.27.2:

- `go test ./...` — passed, including account/video/feed/interaction/follow/notification integration and WebSocket persistence, delivery, and ACK coverage.
- `go vet ./...` — passed.
- `go test -race ./...` — passed.
- `node --check web/app.js` — passed.
- HTTP smoke run against persistent SQLite — `/health`, `/`, `/app.js`, `/styles.css`, and `/api/feed` returned 200; registration and publishing returned 201; the published video appeared in Feed.
- Isolated end-to-end smoke run on `127.0.0.1:18080` — health 200, registration 201, publish 201, Feed 200 with the video, and login 200.
- Browser automation opened the UI and confirmed its navigation and Feed structure render. Chinese glyphs are missing in this browser image due to current system font setup.
- Mobile browser check at 390×844 confirmed both navigation groups render and the header avatar opens the login dialog. Chinese glyphs are missing in this browser image due to current system font setup.
- Compose YAML parsed successfully; Docker Compose and FFmpeg were not run because Docker integration and FFmpeg are unavailable in this WSL environment.
- A same-timestamp keyset test caught and verified the UTC cursor comparison fix; a multipart test checks that upload names are generated server-side.

No performance run has been made, so no QPS/P95 figures are claimed.
