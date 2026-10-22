# API reference

JSON errors use `{ "error": "..." }`; authenticated routes require `Authorization: Bearer <token>`.

## Initial route map

| Method | Path | Purpose | Status |
| --- | --- | --- | --- |
| POST | `/api/users` | Register | Implemented |
| POST | `/api/sessions` | Login | Implemented |
| GET/PATCH | `/api/users/me` | Current profile | Implemented |
| POST | `/api/uploads` | Authenticated local media upload (250 MiB limit) | Implemented |
| POST | `/api/uploads/sessions` | Create a resumable upload session | Implemented |
| GET | `/api/uploads/sessions/:id` | Read completed chunk indices and session status | Implemented |
| PUT | `/api/uploads/sessions/:id/chunks/:index` | Upload one raw 4 MiB chunk | Implemented |
| POST | `/api/uploads/sessions/:id/complete` | Assemble, verify and register media | Implemented |
| POST | `/api/videos` | Publish a video from an uploaded media URL | Implemented |
| GET/PATCH/DELETE | `/api/videos/:id` | Read, author-edit or author-delete a video | Implemented |
| GET | `/api/feed?scene=...&limit=...&cursor=...` | Timeline, following, score-ranked recommendation/hot feed | Implemented |
| GET | `/api/users/:id/videos` | Public creator videos; owner also sees processing/failed | Implemented |
| GET | `/api/users/:id/presence` | Local online state and last-seen timestamp | Implemented |
| PUT/DELETE | `/api/users/me/following/:id` | Follow/unfollow | Implemented |
| GET | `/api/users/me/following`, `/api/users/me/followers` | Relationship lists | Implemented |
| PUT/DELETE | `/api/videos/:id/like` | Like state | Implemented |
| PUT/DELETE | `/api/videos/:id/favorite` | Favorite state | Implemented |
| POST/GET | `/api/videos/:id/comments` | Create a comment or page through top-level comments | Implemented |
| GET | `/api/videos/:id/comments/:comment_id/replies` | Page through one comment's replies | Implemented |
| POST | `/api/videos/:id/views` | Increment play count | Implemented |
| GET | `/api/notifications`, `/api/notifications/unread` | Notification inbox and unread count | Implemented |
| PATCH | `/api/notifications/read` | Mark all notifications read | Implemented |
| GET | `/api/search?q=...` | Search users and video text | Implemented |
| GET | `/api/messages?user_id=...` | Direct-message history | Implemented |
| GET | `/api/messages/unread?after_id=...&limit=...` | Page through unacknowledged messages | Implemented |
| POST | `/api/messages/:id/ack` | Recipient delivery ACK | Implemented |
| POST/GET | `/api/groups` | Create and list joined groups | Implemented |
| POST | `/api/groups/:id/members` | Group owner invites a user | Implemented |
| DELETE | `/api/groups/:id/members/:user_id` | Owner removes a member or a member leaves | Implemented |
| GET | `/api/groups/:id/messages?before_id=...&limit=...` | Group history, newest first | Implemented |
| GET | `/api/groups/:id/unread` | Count unread group messages | Implemented |
| POST | `/api/groups/:id/messages/:message_id/ack` | Advance member read cursor | Implemented |
| GET | `/api/ws` | Realtime WebSocket authenticated by subprotocol | Implemented |
| GET | `/health`, `/ready`, `/metrics` | Liveness, database readiness, Prometheus metrics | Implemented |

Registration accepts `username`, `password` (8–72 characters), and optional `nickname`; login accepts `username` and `password`. Both return a user and 24-hour access token. Passwords use bcrypt and tokens use HS256 with `JWT_SECRET`.

Single-file upload uses multipart field `file`; allowed extensions are MP4, WebM, MOV, JPEG, PNG, and WebP. The server enforces a 250 MiB limit and records the uploader. Video publication accepts `title`, optional `description`, `media_url`, and optional `cover_url`; the video and cover must be files uploaded by the same account. `PATCH /api/videos/:id` accepts those same editable fields and checks author ownership. Avatar updates through `PATCH /api/users/me` also require an image uploaded by that account.

When `FFMPEG_BIN` is enabled (the Compose default), `POST /api/videos` returns a video with `status: "processing"`. Only its author can read it via `GET /api/videos/:id` or see it in `GET /api/users/:id/videos`; public Feed/search/profile queries include only `published` videos. The worker uses ffprobe (`FFPROBE_BIN`, default `ffprobe`) before decoding. It rejects a missing video stream, unknown or nonpositive duration, duration over 10 minutes, a frame side over 4096 pixels, or more than 3840×2160 total pixels. Invalid source metadata marks the job and video failed on the first attempt. Accepted input is transcoded to H.264 MP4 with AAC if audio exists, and a JPEG cover is generated unless a user cover was supplied. The published video includes `duration_ms`, `width`, and `height` measured from the transcoded output. Other processing errors retry up to three attempts before `status: "failed"`. A failed video returns an author-only `processing_error` code: `invalid_media`, `missing_video`, `invalid_duration`, `duration_limit`, `resolution_limit`, or `processing_failed`. Internal worker error text is never included. The author can retry by PATCHing `media_url` to an owned upload, including the same source URL; deleting a processing or failed video is allowed. Without `FFMPEG_BIN`, publication remains immediate and these metadata fields are zero. Existing published rows from before this feature also retain zero metadata until reprocessed.

For resumable upload, create a session with `{"filename":"clip.mp4","size":123456,"sha256":"<optional 64-character hex digest>"}`. The response returns a random `id`, `chunk_size` (4 MiB), and `expires_at`. Send bytes to `PUT /api/uploads/sessions/:id/chunks/0`, then higher indices. The final chunk may be shorter. `GET /api/uploads/sessions/:id` returns `uploaded_chunks` for retry after interruption. `POST /api/uploads/sessions/:id/complete` returns a media URL and actual SHA256; repeated completion is safe. Sessions expire after 24 hours. Chunk bodies must match their exact expected size; a repeated index with different bytes returns 409. On the creator page, failed videos with a generic processing error offer a same-source retry; every failed video offers a replacement-file form. Both PATCH the owned video after upload and can be checked with the page's status refresh control.

Feed scenes are `timeline`, `following`, `recommend`, and `hot`; limit defaults to 10 and caps at 50. Timeline and following use descending `(created_at, id)` keyset cursors. Hot sorts by `3*likes + 5*comments + 4*favorites`, then descending video ID. Recommendation adds 12 points for age under 24 hours or 5 for age under seven days, 12 for a followed author, and 6 for an author in the viewer's recent likes/favorites. Anonymous recommendation uses interaction and age only. Recommendation cursors carry a scene and time anchor, expire after 24 hours, and should not be reused for another scene. Cursor values are opaque URL-safe base64 JSON. Responses contain `scene`, `items`, `next_cursor`, and `has_more`.

When a valid optional access token is provided, each Feed item also contains `liked` and `favorited` booleans for that viewer. Anonymous Feed requests return both as `false`.

`POST /api/videos/:id/comments` accepts `{"body":"..."}` for a top-level comment or `{"body":"...","parent_id":123}` to reply to a top-level comment on the same published video. Bodies are trimmed and limited to 2,000 UTF-8 bytes. Replies to replies are rejected with 400. `GET /api/videos/:id/comments` returns only top-level comments; `GET /api/videos/:id/comments/:comment_id/replies` returns that root's direct replies. Both accept `limit` (default 20, maximum 50) and an opaque `cursor`; responses contain `items`, `next_cursor`, and `has_more`, ordered newest first by `(created_at, id)`. Top-level rows include `reply_count` when nonzero. A cursor belongs to one video and one comment thread; reusing it elsewhere returns 400. Listing is public, while posting requires authentication. Each comment notifies the video author unless they wrote it; a reply also notifies the parent author unless they wrote the reply or are the video author. The comment counter and notification outbox entries commit with the comment.

If `REDIS_ADDR` is configured, anonymous timeline/recommend/hot pages and each authenticated viewer's following first page use a five-second Redis cache. The following cache key includes the viewer ID and a version advanced by follow/unfollow. A successful write that changes a video card or interaction score advances the shared cache version. Authenticated non-following requests and following cursor pages read the database; Redis errors also fall back to database reads. Play counts on a cached card may lag by up to five seconds.

WebSocket clients connect to `/api/ws` with two offered subprotocols: `gostream` and `gostream.jwt.<access_token>`. Browser clients use `new WebSocket(url, ['gostream', 'gostream.jwt.' + token])`; the server selects `gostream`. Each API instance allows up to eight simultaneous sockets per account; a ninth handshake receives 429. Clients send `{"type":"message","client_id":"<unique-per-sender>","recipient_id":2,"body":"Hi"}`, `{"type":"group_message","client_id":"<unique-per-sender>","group_id":1,"body":"Hi group"}`, or `{"type":"ping"}`. The originating socket receives an `ack` after persistence; `pong`, validation errors, and reconnect replay also go only to that socket. Committed direct/group messages are sent to every active recipient device and echoed to the sender's other devices, including devices connected to other API instances when Redis relay is enabled. Notification events use `{"type":"notification","notification":{...}}` and reach all of the user's local sockets. Direct and group messages each replay up to 100 pending rows on reconnect. `sync_required` or `group_sync_required` asks clients to page unread/history APIs for larger backlogs. A direct message's `acked_at` and a group's member read cursor are shared by the account across devices; independent per-device read positions are not implemented. Reusing a client ID with different content is rejected. Redis Pub/Sub delivery is best effort; clients should use unread/history APIs after reconnect. Presence falls back to process-local state if Redis is unavailable.
