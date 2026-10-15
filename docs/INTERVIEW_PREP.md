# Interview preparation

## Implemented questions

1. **How do you avoid duplicate Feed rows when timestamps match?** Use a keyset ordered by `(created_at, id)` and include both values in the next cursor. See `internal/platform/server.go` (`feedCursor`, `encodeCursor`, `decodeCursor`, `feed`).
2. **How do you make likes idempotent?** A unique action row represents current state; the API inserts/deletes it and recomputes the counter in one database transaction. See `internal/platform/models.go` (`VideoAction`) and `internal/platform/server.go` (`action`).
3. **When does a chat send count as accepted?** After the database insert commits, the sender receives an ACK. Live delivery is best effort; offline users can query stored history and ACK receipt separately. See `internal/platform/hub.go` (`websocket`) and `internal/platform/server.go` (`history`, `ackMessage`).
4. **What does the WebSocket client ID protect against?** A unique `(sender_id, client_id)` constraint makes a sender retry resolve to the original message row. It does not prevent duplicate network delivery; recipient handlers should deduplicate by message ID.
5. **How do passwords and access tokens work?** Passwords are bcrypt hashes and access tokens are signed with HS256. See `internal/platform/server.go` (`register`, `login`, `issueToken`, `userFromToken`).

## Planned questions after later stages

- How would a transaction outbox avoid the dual-write gap between SQL and RabbitMQ?
- How do Redis page/card caches remain consistent after delete or interaction changes?
- What is the cost model for push fanout versus pull merge for high-follower creators?
- How does a multi-instance IM deployment route a recipient's connection and replay missed messages?
- How do chunk checksums and upload-session leases support safe resumable media upload?
- Where does `context.Context` cancellation propagate during bounded worker concurrency?

Do not present a topic as implemented until `PROGRESS.md` points to code and verification evidence.
