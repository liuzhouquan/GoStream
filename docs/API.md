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
| GET | `/api/users/:id/videos` | Public creator videos | Implemented |
| GET | `/api/users/:id/presence` | Local online state and last-seen timestamp | Implemented |
| PUT/DELETE | `/api/users/me/following/:id` | Follow/unfollow | Implemented |
| GET | `/api/users/me/following`, `/api/users/me/followers` | Relationship lists | Implemented |
| PUT/DELETE | `/api/videos/:id/like` | Like state | Implemented |
| PUT/DELETE | `/api/videos/:id/favorite` | Favorite state | Implemented |
| POST/GET | `/api/videos/:id/comments` | Create/list comments | Implemented |
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

For resumable upload, create a session with `{"filename":"clip.mp4","size":123456,"sha256":"<optional 64-character hex digest>"}`. The response returns a random `id`, `chunk_size` (4 MiB), and `expires_at`. Send bytes to `PUT /api/uploads/sessions/:id/chunks/0`, then higher indices. The final chunk may be shorter. `GET /api/uploads/sessions/:id` returns `uploaded_chunks` for retry after interruption. `POST /api/uploads/sessions/:id/complete` returns a media URL and actual SHA256; repeated completion is safe. Sessions expire after 24 hours. Chunk bodies must match their exact expected size; a repeated index with different bytes returns 409.

Feed scenes are `timeline`, `following`, `recommend`, and `hot`; limit defaults to 10 and caps at 50. Timeline and following use descending `(created_at, id)` keyset cursors. Recommendation and hot currently sort by `3*likes + 5*comments + 4*favorites`, then descending video ID. This is a deterministic baseline, not a personalized model. Cursor values are opaque URL-safe base64 JSON. Responses contain `scene`, `items`, `next_cursor`, and `has_more`.

When a valid optional access token is provided, each Feed item also contains `liked` and `favorited` booleans for that viewer. Anonymous Feed requests return both as `false`.

If `REDIS_ADDR` is configured, anonymous timeline/recommend/hot pages use a five-second Redis cache. A successful write that changes a video card or interaction score advances the shared cache version. Authenticated and following Feed requests read the database; Redis errors also fall back to database reads. Play counts on a cached card may lag by up to five seconds.

WebSocket clients connect to `/api/ws` with two offered subprotocols: `gostream` and `gostream.jwt.<access_token>`. Browser clients use `new WebSocket(url, ['gostream', 'gostream.jwt.' + token])`; the server selects `gostream`. Clients send `{"type":"message","client_id":"<unique-per-sender>","recipient_id":2,"body":"Hi"}`, `{"type":"group_message","client_id":"<unique-per-sender>","group_id":1,"body":"Hi group"}`, or `{"type":"ping"}`. The sender receives an `ack` after persistence. Online recipients receive `message` or `group_message` events. Direct messages are replayed up to 100 per reconnect; the unread API provides further pages. Group messages are available through history, with a per-member ACK cursor. Reusing a client ID with different content is rejected. Notification events use `{"type":"notification","notification":{...}}`. The connection hub and online presence are process-local.
