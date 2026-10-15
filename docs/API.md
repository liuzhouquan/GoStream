# API reference

JSON errors use `{ "error": "..." }`; authenticated routes require `Authorization: Bearer <token>`.

## Initial route map

| Method | Path | Purpose | Status |
| --- | --- | --- | --- |
| POST | `/api/users` | Register | Implemented |
| POST | `/api/sessions` | Login | Implemented |
| GET/PATCH | `/api/users/me` | Current profile | Implemented |
| POST | `/api/uploads` | Authenticated local media upload (250 MiB limit) | Implemented |
| POST | `/api/videos` | Publish a video from an uploaded media URL | Implemented |
| GET/DELETE | `/api/videos/:id` | Read or author-delete a video | Implemented |
| GET | `/api/feed?scene=...&limit=...&cursor=...` | Timeline, following, score-ranked recommendation/hot feed | Implemented |
| GET | `/api/users/:id/videos` | Public creator videos | Implemented |
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
| POST | `/api/messages/:id/ack` | Recipient delivery ACK | Implemented |
| GET | `/api/ws?token=...` | Realtime direct-message WebSocket | Implemented |

Registration accepts `username`, `password` (8–72 characters), and optional `nickname`; login accepts `username` and `password`. Both return a user and 24-hour access token. Passwords use bcrypt and tokens use HS256 with `JWT_SECRET`.

Upload uses multipart form field `file`; allowed extensions are MP4, WebM, MOV, JPEG, PNG, and WebP. The server enforces a 250 MiB limit. Video creation accepts `title`, optional `description`, `media_url`, and optional `cover_url`.

Feed scenes are `timeline`, `following`, `recommend`, and `hot`; limit defaults to 10 and caps at 50. Timeline and following use descending `(created_at, id)` keyset cursors. Recommendation and hot currently sort by `3*likes + 5*comments + 4*favorites`, then descending video ID. This is a deterministic baseline, not a personalized model. Cursor values are opaque URL-safe base64 JSON. Responses contain `scene`, `items`, `next_cursor`, and `has_more`.

When a valid optional access token is provided, each Feed item also contains `liked` and `favorited` booleans for that viewer. Anonymous Feed requests return both as `false`.

WebSocket clients send `{"type":"message","client_id":"<unique-per-sender>","recipient_id":2,"body":"Hi"}` or `{"type":"ping"}`. The sender receives an `ack` after persistence; an online recipient receives a `message` event and acknowledges receipt through the HTTP ACK route. Offline messages remain available through history. The current connection hub is process-local.
