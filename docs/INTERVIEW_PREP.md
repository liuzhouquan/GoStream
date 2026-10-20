# Interview preparation

## Implemented questions

1. **How do equal-timestamp Feed rows remain paginable?** Use a keyset ordered by `(created_at, id)` and carry both in the cursor. See `internal/platform/feed.go`. Interaction scores can change between recommendation pages, so those pages are not a stable snapshot.
2. **How are likes idempotent and counts kept consistent?** A unique `(user_id, video_id, kind)` row represents state. Insert/delete and the corresponding counter change happen in one transaction only when a row changed. See `social.go` and `models.go`.
3. **What closes the SQL-to-notification dual-write gap?** The notification and outbox event are inserted in the business transaction. A worker leases, retries and records dead letters. See `outbox.go`; explain why online socket delivery is still local to one process.
4. **When is a chat message accepted?** After message, notification and outbox commit, the sender receives ACK. A duplicate client ID with matching content returns the original message; a conflict is rejected. See `hub.go` and `messages.go`.
5. **How does a recipient recover offline messages?** A reconnect replays up to 100 unacknowledged rows; the unread API pages further rows. Recipient ACK is idempotent. See `hub.go` and `messages.go`.
6. **How do group members track unread messages?** Each member has a monotonically increasing last-acknowledged message ID. Unread counts scan later group messages excluding the user's own sends. See `groups.go` and `models.go`.
7. **How does resumable upload avoid corrupt or foreign files?** Session ownership, fixed chunk size, per-chunk hashes, full-file hash, a completion lease and an owned media record. See `uploads.go` and `videos.go`.
8. **How are passwords and access tokens handled?** bcrypt hashes and HS256 JWTs; the browser offers a public WebSocket protocol and an authentication protocol, while the server negotiates only the public one. See `accounts.go`, `server.go` and `hub.go`.
9. **What happens when Redis fails or a write races with cache population?** Anonymous pages fall back to SQL on Redis errors. A page is stored under the version read before its SQL query; an intervening write increments the version, so later readers use a different key. A failed invalidation can expose stale data until the five-second TTL expires. See `feed_cache.go`, `feed.go`, and `feed_cache_test.go`.

## Planned questions after later stages

- How would the database outbox publish to RabbitMQ and coordinate consumers across API nodes?
- How could per-video card and count caches stay consistent without invalidating every page?
- What is the cost model for push fanout versus pull merge for high-follower creators?
- How does a multi-instance IM deployment route a recipient's connection and replay missed messages?
- How should FFmpeg jobs and object storage replace local file finalization while preserving upload idempotency?
- Where does `context.Context` cancellation propagate during bounded worker concurrency?

Do not present a topic as implemented until `PROGRESS.md` points to code and verification evidence.
