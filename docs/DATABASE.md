# Database design

The first schema is defined by GORM models in `internal/platform/models.go` and migrated at startup. The local profile uses a persistent SQLite file; the container profile uses MySQL. Production deployment should use versioned SQL migrations before schema changes become operationally sensitive.

Implemented tables: `users`, `videos`, `follows`, `video_actions`, `comments`, `notifications`, `chat_messages`, and `outbox_events`. Chat conversations are represented by sender/recipient pairs in the current single-chat MVP. Important uniqueness constraints include username, `(follower_id, followee_id)`, `(user_id, video_id, kind)`, `(user_id, event_key)`, and `(sender_id, client_id)`. Feed access uses indexes on status/creation time and author/creation time; comments, notifications, relationships, and message history have supporting indexes.

`outbox_events` is reserved in the schema, but no publisher/consumer uses it yet. GORM auto-migration is convenient for local learning; versioned migration and rollback practice remains future work.
