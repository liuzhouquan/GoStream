# Learning guide

Read the current implementation in this order:

1. `cmd/gostream`: configuration, database startup, and dependency assembly.
2. `internal/platform/models.go`: persisted entities, uniqueness constraints, and indexes.
3. `internal/platform/server.go`: route registration, authentication middleware, and server setup.
4. `internal/platform/accounts.go`, `videos.go`, `feed.go`, `social.go`, `notifications.go`, and `messages.go`: read one feature flow end to end.
5. `internal/platform/hub.go`: WebSocket connection ownership and delivery behavior.
6. `internal/platform/server_test.go`: API and reliability expectations.
7. `web`: API-driven demonstration flows.
8. `docs/ARCHITECTURE.md`, `TECHNICAL_DESIGN.md`, then the remaining guides.

The initial package keeps handler, use-case, and database code together so the first runnable flows are easy to trace. A useful next exercise is to move validation and transaction rules into Application services and move GORM queries behind repository interfaces, preserving the tests. Compare the deterministic Feed with later cache/fanout stages using recorded test and load-test evidence.
