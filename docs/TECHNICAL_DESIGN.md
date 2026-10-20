# Technical design notes

These notes distinguish current behavior from the next implementation step. They are updated with source changes.

## Feed pagination and ranking

`internal/platform/feed.go` implements the first version. Timeline and following feeds order by `created_at DESC, id DESC`; the opaque cursor carries both values so equal timestamps do not skip or duplicate a row. Feed requests fetch `limit + 1` rows to determine `has_more`. Recommendation and hot currently rank by `3*likes + 5*comments + 4*favorites`, tie-broken by ID. The score is simple and explainable; it is not personalized and has no freshness decay. Because interaction counts can change between requests, score-based pagination does not provide a stable snapshot.

Following Feed is a database join. A later scale stage can add a hybrid index: fan out posts into recent follower inboxes for ordinary creators, and read a creator outbox for high-follower accounts. The write path should use a durable event and idempotent batch cursor so a worker restart cannot lose or multiply fanout work.

## Interaction state

`video_actions` has a unique constraint on `(user_id, video_id, kind)`. A like/favorite transition and its denormalized video count are updated in one database transaction. The insert uses `ON CONFLICT DO NOTHING`; the count changes only when a row was inserted or removed. Repeating PUT does not increment twice, and repeating DELETE does not decrement twice. Notifications have a unique `(user_id, event_key)` constraint so retries do not create duplicate notices.

Counts are maintained synchronously in the same database. A notification and an outbox event are written in the same transaction as a new interaction or comment. The outbox worker leases rows for 30 seconds, retries failed sends with exponential delay, and marks an event dead after eight attempts. The database inbox is authoritative; local WebSocket push is a convenience. The optional Redis page cache is invalidated by advancing a version after successful interaction writes; Redis does not own counters.

## Messaging semantics

The WebSocket hub in `internal/platform/hub.go` is process-local. A message, notification, and outbox event commit before the sender receives an ACK. `(sender_id, client_id)` makes exact retries resolve to the same stored message; reusing an ID with different content is rejected. A recipient reconnect replays up to 100 unacknowledged messages; `GET /api/messages/unread` pages through the rest. Recipient ACK is an idempotent HTTP call that stores `acked_at`.

Groups have an owner, a unique membership row for each user, durable messages, and a per-member monotonically increasing read cursor. Members can read history and send; only owners can invite or remove others. Local group fanout uses the same hub; with `REDIS_ADDR`, a Pub/Sub event causes each other API instance to query current membership and deliver to its local sockets. Direct-message and outbox-notification events are relayed similarly, using committed database IDs rather than payload content. Redis subscription reconnects after interruption. Direct and group publication is best effort, so Pub/Sub outages can delay online delivery; SQL unread/history APIs recover missed data. Outbox notification publication retries failures. Local presence comes from active connections; with Redis, each instance refreshes an expiring per-user/per-node sorted-set entry every 30 seconds, and other instances count unexpired entries. Disconnects remove the entry and persist `last_seen_at`; a crashed node ages out within 75 seconds. Without Redis or during Redis failure, only local presence is visible. Automatic group replay remains future work. The browser offers both `gostream` and `gostream.jwt.<token>` WebSocket subprotocols; the server negotiates only the public `gostream` protocol and reads the second for authentication. The API logger records paths without query parameters. Clients should deduplicate deliveries by message ID.

## Media upload

The authenticated single-file upload handler writes to local disk using random file names and a 250 MiB request cap. A separate resumable flow creates a session, accepts 4 MiB chunks in any order, stores per-chunk SHA256 values, then assembles and hashes the full file. A database completion lease limits concurrent finalization. Incomplete chunks live beside the public media directory and expire after 24 hours; a cleanup worker removes expired data. Published video and cover URLs must belong to the current account. MP4/MOV/WebM signatures and image signatures are checked; JPEG/PNG also have their dimensions decoded. FFmpeg transcoding, thumbnail generation, and object storage remain to be built.

## Cache, broker, and performance

Prometheus instrumentation is active at `/metrics`, with bounded route labels and `gostream_feed_cache_total{result}` for hit/miss/error/invalidation errors. `REDIS_ADDR` enables a five-second cache for anonymous timeline/recommend/hot pages. Cache keys include a Redis version, scene, page size, and cursor hash. Publishing, editing, deleting, liking, favoriting, commenting, and changing an author's displayed profile advance the version after the database write. Authenticated and following feeds bypass the cache. Redis errors open a five-second client circuit and fall back to database reads; failed invalidation can expose an old page for at most the five-second TTL. Play counts intentionally do not invalidate each page. The Compose `observability` profile provisions Prometheus and a Grafana dashboard, but the current WSL environment cannot run Docker. RabbitMQ and FFmpeg are not wired into the API. A three-run in-process Feed benchmark is documented; no k6 load-test results or production cache hit rates are claimed.
