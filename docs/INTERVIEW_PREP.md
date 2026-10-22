# Interview preparation

## Implemented questions

1. **How do equal-timestamp Feed rows remain paginable?** Use a keyset ordered by `(created_at, id)` and carry both in the cursor. See `internal/platform/feed.go`. Interaction scores can change between recommendation pages, so those pages are not a stable snapshot.
2. **How are likes idempotent and counts kept consistent?** A unique `(user_id, video_id, kind)` row represents state. Insert/delete and the corresponding counter change happen in one transaction only when a row changed. See `social.go` and `models.go`.
3. **What closes the SQL-to-notification dual-write gap?** The notification and outbox event are inserted in the business transaction. A worker leases, retries and records dead letters. Redis can relay the committed notification ID to other instances. See `outbox.go` and `realtime.go`.
4. **When is a chat message accepted?** After message, notification and outbox commit, the sender receives ACK. A duplicate client ID with matching content returns the original message; a conflict is rejected. See `hub.go` and `messages.go`.
5. **How does a recipient recover offline messages?** A reconnect replays up to 100 unacknowledged rows; the unread API pages further rows. Recipient ACK is idempotent. See `hub.go` and `messages.go`.
6. **How do group members track unread messages?** Each member has a monotonically increasing last-acknowledged message ID. Unread counts scan later group messages excluding the user's own sends. See `groups.go` and `models.go`.
7. **How does resumable upload avoid corrupt or foreign files?** Session ownership, fixed chunk size, per-chunk hashes, full-file hash, a completion lease and an owned media record. See `uploads.go` and `videos.go`.
8. **How are passwords and access tokens handled?** bcrypt hashes and HS256 JWTs; the browser offers a public WebSocket protocol and an authentication protocol, while the server negotiates only the public one. See `accounts.go`, `server.go` and `hub.go`.
9. **What happens when Redis fails or a write races with cache population?** Cached pages fall back to SQL on Redis errors. A page is stored under the version read before its SQL query; an intervening write increments the version, so later readers use a different key. Following first pages also use the viewer ID and a per-viewer follow version, so one account's follow changes do not invalidate another's cache. A failed invalidation can expose stale data until the five-second TTL expires. See `feed_cache.go`, `feed.go`, `feed_cache_test.go`, and `following_cache_test.go`.
10. **How are sockets reached on another API instance?** Redis Pub/Sub broadcasts only committed IDs; subscribers query SQL and verify recipients or group membership before sending to local sockets. Direct and group reconnect replay plus history APIs cover missed events. Pub/Sub itself is best effort. Expiring Redis entries make cross-instance presence visible. See `realtime.go`, `realtime_test.go`, and `hub.go`.
11. **How does recommendation differ from hot ranking?** Hot uses interaction counts. Recommendation adds fixed time buckets and author affinity from follows and the viewer's last 200 likes/favorites. Its cursor fixes the time anchor, while interaction and preference changes can still move items between pages. See `recommend.go`, `feed.go`, and `recommend_test.go`.
12. **How does video processing survive a crash or duplicate worker?** The video and job are created in one transaction. A conditional lease chooses one worker; an expired lease can be reclaimed, and completing a job checks the lease and original source URL. Temporary files are removed on failed commits; the video row switches to generated media URLs only after the completion transaction succeeds. See `video_processing.go`, `videos.go`, and `video_processing_test.go`.
13. **How is expensive video decoding bounded and metadata made trustworthy?** ffprobe checks the source duration and first video stream dimensions before FFmpeg runs. Invalid metadata permanently fails the job; output metadata is probed again and committed with publication. This avoids claiming input dimensions for a resized output. See `video_probe.go`, `video_processing.go`, and `video_probe_test.go`.
14. **How are failed jobs explained without exposing worker diagnostics, and how are crash orphans removed?** The job stores an internal error and a stable code. Only the author receives an allowlisted code. A periodic worker waits 24 hours, checks asset/video/unfinished-job/avatar references in batches, and deletes only matching generated regular files. Database errors stop deletion. See `videos.go`, `video_processing.go`, `media_reconcile.go`, and their tests.

## Planned questions after later stages

- How would the database outbox publish to RabbitMQ and coordinate consumers across API nodes?
- How could per-video card and count caches stay consistent without invalidating every page?
- What is the cost model for push fanout versus pull merge for high-follower creators?
- How can per-device group read cursors be added without treating Redis Pub/Sub as a durable queue?
- How should FFmpeg jobs and object storage replace local file finalization while preserving upload idempotency?
- Where does `context.Context` cancellation propagate during bounded worker concurrency?

Do not present a topic as implemented until `PROGRESS.md` points to code and verification evidence.
