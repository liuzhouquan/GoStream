# Database Query Plans and Index Analysis

This document validates the query execution plans, index coverage, and keyset pagination characteristics across GoStream's core modules (A08 Notification and Search, A07 Direct and Group IM, A04 Hybrid Feed, and A05 Comments/Interactions). It satisfies the query-plan review requirements noted in [`docs/DATABASE.md`](DATABASE.md) and [`docs/TEST_STANDARD.md`](TEST_STANDARD.md).

Automated regression tests are in [`internal/platform/query_plan_test.go`](../internal/platform/query_plan_test.go).

---

## 1. Query Plan Matrix

| Feature / Endpoint | Query Pattern | Primary Index | Index Columns | Access Type | Extra / Optimization |
| --- | --- | --- | --- | --- | --- |
| `GET /api/notifications` | `WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 31` | `idx_notification_user_id` | `(user_id, id DESC)` | `range` | Keyset pagination, index-backed ordering, no filesort |
| `GET /api/notifications/unread` | `SELECT COUNT(*) WHERE user_id = ? AND read_at IS NULL` | `idx_notification_unread` | `(user_id, read_at, id DESC)` | `ref` | **Covering Index** (index-only scan) |
| `PATCH /api/notifications/read` | `UPDATE SET read_at = ? WHERE user_id = ? AND read_at IS NULL AND id <= ?` | `idx_notification_unread` | `(user_id, read_at, id DESC)` | `range` | Bounded cursor update; higher IDs remain unread |
| `GET /api/messages` | `WHERE sender_id = ? AND recipient_id = ? AND id < ? ORDER BY id DESC LIMIT 51` | `idx_message_thread_id` | `(sender_id, recipient_id, id DESC)` | `range` | Keyset pagination, index-backed ordering, no filesort |
| `GET /api/messages/unread` (legacy) | `WHERE recipient_id = ? AND acked_at IS NULL ORDER BY id ASC LIMIT 101` | `idx_message_unacked` | `(recipient_id, acked_at)` | `ref` | Backlog retrieval |
| `GET /api/messages/unread` (device) | `LEFT JOIN message_device_acks ON ... WHERE recipient_id = ? AND acked_at IS NULL AND mda.id IS NULL` | `idx_message_unacked` + `ux_message_device_ack` | `(user_id, device_id, message_id)` | `ref` + `ref` | Anti-join with **Covering Index** on device ACK receipts |
| `GET /api/groups/:id/messages` | `WHERE group_id = ? AND id < ? ORDER BY id DESC LIMIT 51` | `idx_group_message` | `(group_id, created_at)` | `ref` | Filter by `group_id`, orders by `id DESC` |
| `GET /api/feed?scene=timeline` | `WHERE status = 'published' AND created_at < ? ORDER BY created_at DESC, id DESC LIMIT 21` | `idx_video_feed` | `(status, created_at DESC)` | `range` | Public video recency pagination |
| `GET /api/users/:id/videos` | `WHERE author_id = ? AND status = 'published' ORDER BY created_at DESC LIMIT 50` | `idx_video_author` | `(author_id, created_at DESC)` | `ref` | Author video recency index |
| `GET /api/feed?scene=following` (hybrid inbox) | `WHERE user_id = ? AND created_at < ? ORDER BY created_at DESC, video_id DESC LIMIT 21` | `idx_feed_inbox_user` | `(user_id, created_at DESC, video_id DESC)` | `range` | **Covering Index** on inbox table |
| Recommendation interacted authors | `SELECT video_id FROM video_actions WHERE user_id = ? ORDER BY id DESC LIMIT 200` | `idx_action_recent` | `(user_id, id DESC)` | `ref` | Bounded interaction history lookup |
| `GET /api/videos/:id/comments` | `WHERE video_id = ? AND parent_id IS NULL AND created_at < ? ORDER BY created_at DESC, id DESC LIMIT 31` | `idx_comment_thread` | `(video_id, parent_id, created_at DESC, id DESC)` | `range` | Compound root thread keyset pagination |
| `GET /api/search?type=users` | `WHERE (username LIKE ? OR nickname LIKE ?) AND id < ? ORDER BY id DESC LIMIT 21` | `PRIMARY` | `(id)` | `range` | Backward PK scan with literal LIKE filter |
| `GET /api/search?type=videos` | `WHERE status = 'published' AND (title LIKE ? OR description LIKE ?) AND id < ? ORDER BY id DESC LIMIT 21` | `idx_video_feed` / `PRIMARY` | Multiple | `range` | Status index filter + keyset pagination |

---

## 2. Notification Keyset Pagination and Unread Plans

### Keyset Page Query
```sql
SELECT * FROM `notifications`
WHERE user_id = ? AND id < ?
ORDER BY id DESC
LIMIT 31;
```
- **SQLite Plan**: `SEARCH notifications USING INDEX idx_notification_user_id (user_id=? AND id<?)`
- **MySQL Plan**: `table=notifications type=range key=idx_notification_user_id Extra=Using where`
- **Result**: Zero temporary sorting tables or filesort. Keyset pagination on `(user_id, id DESC)` guarantees constant-time page access regardless of page depth.

### Unread Count Query
```sql
SELECT COUNT(*) FROM `notifications`
WHERE user_id = ? AND read_at IS NULL;
```
- **SQLite Plan**: `SEARCH notifications USING COVERING INDEX idx_notification_unread (user_id=? AND read_at=?)`
- **MySQL Plan**: `table=notifications type=ref key=idx_notification_unread Extra=Using where; Using index`
- **Result**: Pure index-only covering query. No table data pages are fetched from disk.

---

## 3. Direct Message History and Multi-Device Anti-Join

### Direct History Keyset Query
```sql
SELECT * FROM `chat_messages`
WHERE sender_id = ? AND recipient_id = ? AND id < ?
ORDER BY id DESC
LIMIT 31;
```
- **SQLite Plan**: `SEARCH chat_messages USING INDEX idx_message_thread_id (sender_id=? AND recipient_id=? AND id<?)`
- **MySQL Plan**: `table=chat_messages type=range key=idx_message_thread_id Extra=Using index condition`
- **Result**: Keyset cursor advances strictly down the conversation thread without filesort.

### Multi-Device Pending Anti-Join Query
```sql
SELECT chat_messages.* FROM `chat_messages`
LEFT JOIN `message_device_acks` ON
  message_device_acks.message_id = chat_messages.id AND
  message_device_acks.user_id = ? AND
  message_device_acks.device_id = ?
WHERE chat_messages.recipient_id = ? AND
  chat_messages.acked_at IS NULL AND
  message_device_acks.id IS NULL
ORDER BY chat_messages.id ASC
LIMIT 101;
```
- **SQLite Plan**: `SEARCH chat_messages USING INDEX idx_message_unacked (recipient_id=? AND acked_at=?) | SEARCH message_device_acks USING COVERING INDEX ux_message_device_ack (user_id=? AND device_id=? AND message_id=?) LEFT-JOIN`
- **MySQL Plan**: `chat_messages` accesses `idx_message_unacked` (`ref`), and `message_device_acks` accesses `ux_message_device_ack` (`ref` with `Not exists` anti-join optimization).
- **Result**: Both tables are accessed through indexed lookups. Receipt lookups use the unique compound covering index `(user_id, device_id, message_id)`.

---

## 4. Redundant Index Review

[`docs/DATABASE.md`](DATABASE.md) identified two legacy indexes created in `v001` that warranted retirement review:

1. **`idx_notification_page (user_id, created_at DESC)`**:
   - In `v001`, notification paging originally ordered by `created_at DESC`.
   - The API was subsequently revised to strictly use `(user_id, id DESC)` keyset cursors (`idx_notification_user_id`) to avoid skipping notifications with identical timestamps.
   - **Finding**: No application endpoint queries notifications by `created_at`. All keyset page requests exclusively utilize `idx_notification_user_id`. `idx_notification_page` is not selected by any runtime query.
   - **Recommendation**: Retain in frozen `v001` schema for migration reproducibility; mark as candidate for dropping in a future versioned migration (`schema_v005`).

2. **`idx_message_thread (sender_id, recipient_id, created_at)`**:
   - In `v001`, chat history was indexed on `created_at`.
   - The application was updated to order conversation history by `id DESC` (`idx_message_thread_id`).
   - **Finding**: Direct chat history endpoints exclusively query by `(sender_id, recipient_id, id DESC)`. The `created_at` index is never selected by any application query.
   - **Recommendation**: Retain in frozen `v001` schema for baseline compatibility; mark as candidate for dropping in a future versioned migration (`schema_v005`).

---

## 5. Feed Inboxes and Comments Keyset Plans

### Feed Inbox Page
```sql
SELECT * FROM `feed_inboxes`
WHERE user_id = ? AND created_at < ?
ORDER BY created_at DESC, video_id DESC
LIMIT 21;
```
- **SQLite Plan**: `SEARCH feed_inboxes USING COVERING INDEX idx_feed_inbox_user (user_id=? AND created_at<?)`
- **MySQL Plan**: `type=range key=idx_feed_inbox_user Extra=Using where; Using index`
- **Result**: `idx_feed_inbox_user` covers all columns (`user_id`, `created_at`, `video_id`), completely eliminating secondary table lookups.

### Comment Thread Keyset
```sql
SELECT * FROM `comments`
WHERE video_id = ? AND parent_id IS NULL AND created_at < ?
ORDER BY created_at DESC, id DESC
LIMIT 31;
```
- **SQLite Plan**: `SEARCH comments USING INDEX idx_comment_thread (video_id=? AND parent_id=? AND created_at<?)`
- **MySQL Plan**: `type=range key=idx_comment_thread Extra=Using where`
- **Result**: The 4-column compound index `(video_id, parent_id, created_at DESC, id DESC)` satisfies the equality conditions, the `created_at` range condition, and the final tie-breaker ordering on `id DESC` without filesort.

---

## 6. Search Execution Plans

### User Search Keyset
```sql
SELECT * FROM `users`
WHERE (username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!') AND id < ?
ORDER BY id DESC
LIMIT 21;
```
- **Plan**: `SEARCH users USING INTEGER PRIMARY KEY (rowid<?)`
- **Behavior**: MySQL and SQLite scan the `PRIMARY KEY` descending starting from `id < ?`, evaluating the pattern on matching candidate rows and stopping immediately once `LIMIT + 1` matches are found. This avoids loading the entire table into a temporary filesort buffer.

### Video Search Keyset
```sql
SELECT * FROM `videos`
WHERE status = 'published' AND (title LIKE ? ESCAPE '!' OR description LIKE ? ESCAPE '!') AND id < ?
ORDER BY id DESC
LIMIT 21;
```
- **Behavior**: Substring `LIKE` queries with leading wildcards cannot utilize B-tree index prefixes on `title` or `description`. The query planner uses `status` index filtering or PK range scans. For larger production catalogs, full-text search (e.g. MySQL `FULLTEXT` or external search indexing) remains the appropriate scale path.
