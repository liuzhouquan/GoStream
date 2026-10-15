# Testing and performance report

## Functional verification

Run the local suite with the Go version in `go.mod`:

```sh
go test ./...
go test -race ./...
go vet ./...
node --check web/app.js
```

API integration tests use a temporary SQLite database. The current suite checks registration/auth, video publication and public Feed, equal-timestamp cursor traversal, idempotent likes, comments, following Feed and notifications, generated upload paths, and persisted WebSocket delivery/ACK.

## Load-test procedure

Start the API with a representative dataset. Then install k6 and run:

```sh
BASE_URL=http://127.0.0.1:8080 SCENE=timeline VUS=10 DURATION=30s k6 run tests/load/feed.js
```

Repeat with `SCENE=following`, `recommend`, and `hot` after preparing suitable users and videos. Record CPU, memory, SQLite/MySQL settings, data size, host, Go version, k6 version, request rate, P50/P95/P99, and error rate. Use the output to determine whether query indexes, SQL plans, connection pools, Redis caching, or hybrid fanout are warranted.

## Results

No k6 or database load test has been run for GoStream yet. No QPS or latency result is claimed. The current WSL session has Go 1.27.2 and Node 24, but Docker and k6 are unavailable. Unit/integration tests and a small HTTP smoke run validate behavior only; they are not performance measurements.
