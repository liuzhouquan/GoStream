# Technical design notes

These notes distinguish current behavior from the next implementation step. They are updated with source changes.

## Feed pagination and ranking

`internal/platform/server.go` owns the first implementation. Timeline and following feeds order by `created_at DESC, id DESC`; the opaque cursor carries both values so equal timestamps do not skip or duplicate a row. Feed requests fetch `limit + 1` rows to determine `has_more`. Recommendation and hot currently rank by the persisted interaction score `3*likes + 5*comments + 4*favorites`, tie-broken by ID. That score is deliberately simple and explainable. It is not personalized, does not yet apply freshness decay, and does not use user preference events.

Following Feed is a database join. A later scale stage can add a hybrid index: fan out posts into recent follower inboxes for ordinary creators, and read a creator outbox for high-follower accounts. The write path should use a durable event and idempotent batch cursor so a worker restart cannot lose or multiply fanout work.

## Interaction state

`video_actions` has a unique constraint on `(user_id, video_id, kind)`. A like/favorite transition and its denormalized video count are updated in one database transaction. Repeating PUT does not increment the count twice; DELETE is safe if the action is already absent. Notifications have a unique `(user_id, event_key)` constraint so retries do not create duplicate notices.

Counts are currently maintained synchronously in the same database. No Redis counter, asynchronous write, or cache invalidation claim is made yet. If an async path is added, write an outbox event in the same transaction as the action and reconcile derived counts from durable action rows.

## Messaging semantics

The WebSocket hub in `internal/platform/hub.go` is process-local. A message is inserted into `chat_messages` before the server returns a sender ACK or attempts immediate delivery. `(sender_id, client_id)` makes client retries resolve to the same stored message. When the recipient is offline, history remains queryable and can be fetched after reconnect. Recipient ACK is a separate HTTP call that stores `acked_at`.

The current implementation is single-chat only. It does not promise delivery ordering across multiple API instances, online presence, group fanout, or automatic replay of unacknowledged messages. Those need a broker-backed connection directory, per-conversation sequence, and an explicit reconnect/replay cursor. The WebSocket handshake token is supplied in the query string because the browser WebSocket API cannot set an Authorization header; deployments should prevent query strings from being retained in access logs.

## Media upload

The authenticated upload handler writes to local disk using server-generated file names and a 250 MiB request cap. Video publication stores a URL and metadata in MySQL or SQLite. Chunked resume, content probing, transcoding, thumbnails, object storage, and cleanup jobs are not implemented yet. The media directory should be backed by a persistent volume in the Compose profile.

## Cache, broker, and performance

Redis, RabbitMQ, Prometheus, Grafana, and FFmpeg are not wired into the current API. The outbox table is only a schema placeholder. Follow the later stage before discussing cache hit rates, message retry guarantees, or performance results. There is no measured GoStream load-test report yet.
