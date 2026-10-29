# RabbitMQ notification delivery

Notifications support RabbitMQ 4.3.7 using the RabbitMQ-maintained `amqp091-go` v1.15.0 client. The real MySQL/broker failure matrix is implemented and has run locally. A06 remains partially accepted: the remaining fault scenarios are listed below. Execution evidence belongs in [`PROGRESS.md`](../PROGRESS.md).

## Data flow and guarantees

The business transaction continues to write the notification inbox and `notification.created` outbox row together. In broker mode the outbox publisher sends a persistent, versioned event with its stable event key, mandatory routing and publisher confirms. Only a confirmed, routed publication can mark that source outbox row published.

The consumer validates the envelope against the committed source row and notification. Its transaction inserts a `notification.deliver` outbox row with unique key `broker-delivery:<source-event-key>`. This row is both the durable consumer effect and its deduplication record. The consumer ACKs only after commit. Redelivery after commit but before ACK finds the same row. The existing leased worker dispatches delivery rows to local sockets and Redis; these rows are never republished to RabbitMQ. No schema migration or second volatile task queue is needed.

```mermaid
flowchart LR
  A[Business transaction] --> B[Notification + source outbox]
  B --> C[Confirmed persistent publication]
  C --> D[RabbitMQ quorum queue]
  D --> E[Consumer SQL transaction]
  E --> F[Unique durable delivery outbox]
  E --> G[ACK after commit]
  F --> H[Local WebSocket + Redis]
```

Real-time delivery remains at least once. A delivery worker can send before losing its lease, so a later attempt can repeat a socket/Redis hint. SQL inbox/history remain authoritative. Consumer idempotency means one durable delivery row, not exactly-once WebSocket display.

## Queue and failure contract

One database uses one configured queue namespace. All its API workers must use the same broker configuration. The durable direct exchange routes to a bounded durable quorum queue. A durable quorum dead-letter queue is bound before source declaration. Source arguments select `reject-publish`, at-least-once dead lettering, a delivery limit of eight, and RabbitMQ 4.3's failed-delivery retry delay (one-second minimum, thirty-second maximum). A transient SQL failure uses `basic.reject(requeue=true)` so it counts as a failed delivery; malformed/conflicting events are rejected without requeue. A database failure must not be ACKed.

Publisher and consumer use separate connections. Publication has bounded network I/O and confirmation waits; uncertain outcomes are retried with the same event ID. Consumers reconnect and redeclare topology after disconnect. Startup with an unavailable broker leaves the HTTP API available and retries in the existing SQL outbox. `/ready` retains its database-only contract. SQL outbox attempts remain bounded at eight; dead SQL rows and broker dead letters require explicit operator replay after the cause is fixed.

SQLite without `RABBITMQ_URL` retains direct SQL-outbox dispatch for quick development. Compose enables broker delivery by default. A one-node Compose quorum queue survives a broker process restart but does not provide multi-node availability. Queue definitions are versioned names; changing immutable queue arguments requires an explicit queue migration. Deduplication rows must be retained while broker replay remains possible.

## Configuration and operations

| Setting | Behavior |
| --- | --- |
| `RABBITMQ_URL` | Optional `amqp://` or `amqps://` URI. Empty keeps direct SQL-outbox dispatch. Credentials are excluded from broker errors. |
| `RABBITMQ_QUEUE` | Defaults to `gostream.notifications.v1`; 1–100 ASCII letters, digits, dots, underscores or hyphens; cannot start with `amq.`. Use a separate namespace for each database. |
| `RABBITMQ_PASSWORD` | Compose broker password; the documented default is for local development. Override with a URL-safe generated value, for example hexadecimal. Existing RabbitMQ volumes retain their user credentials. |

Compose exposes AMQP on `127.0.0.1:5672` and management on `127.0.0.1:15672`. The broker data has its own named volume. The isolated object-test overlay removes broker host ports so it can run alongside the demo; APIs use the Compose network. Publisher and consumer connections have separate lifecycles; the application handles reconnects rather than enabling the client's experimental recovery feature. Shutdown cancels and joins workers before closing the publisher and database.

Monitor `gostream_rabbitmq_messages_total{result="published|accepted|duplicate|retry|dead|reconnect"}`, `gostream_rabbitmq_connected{role="publisher|consumer"}`, existing outbox metrics, queue depth and the `.dead` queue. The publisher connection gauge records its last observed state, not an active dependency probe. `accepted`/`duplicate` count committed consumer outcomes; a subsequent ACK failure can still cause redelivery. `dead` counts explicit invalid-event rejections; retry exhaustion is visible in the broker DLQ, not that counter.

Two `published_at` values have different meanings: a source row records confirmed broker acceptance; a delivery row records completion of the local/Redis dispatch attempt. Neither means a user has read or ACKed a WebSocket message. The SQL notification inbox is available independently of both.

After fixing a failed dependency or poison event, an operator can use the shipped executable:

```sh
# Reset exactly one dead, unpublished SQL event. Active/published rows are refused.
docker compose exec api /usr/local/bin/gostream-events -outbox-key notification:42

# Republish at most ten DLQ entries, confirm each publication, then ACK that entry.
docker compose exec api /usr/local/bin/gostream-events -dead-letters 10
```

Outside Compose, run `go run ./cmd/gostream-events` with the same flags. SQL replay requires explicit `DB_DRIVER` and `DB_DSN` for an existing migrated database; DLQ replay requires `RABBITMQ_URL` and the correct queue namespace. This is a local operator command, not a public HTTP endpoint. It does not migrate a database. Replaying a permanently invalid envelope sends it back to the DLQ, so inspect and fix the cause first. Interrupted replay can publish twice; the unique delivery key makes this safe for the durable effect. Replay does not reset an already completed delivery job, and intentionally does not remove deduplication records.

## Executed tests and reproduction

With Go 1.27.2 and a running MySQL 8.4 container whose internal environment contains `MYSQL_ROOT_PASSWORD`:

```sh
python3 tests/integration/rabbitmq_smoke.py \
  --go-bin /usr/local/go/bin/go \
  --mysql-container gostream-mysql-1 \
  --mysql-addr 127.0.0.1:3307
```

The script creates a random schema/user and a disposable RabbitMQ 4.3.7 container with explicit temporary host ports. It migrates the schema, runs `TestRabbitMQNotificationIntegration` with the race detector, builds and starts the real API, creates accounts/follow notification, checks automatic completion of both worker stages, sends SIGTERM and checks exit zero, and exercises both operator commands. Cleanup removes its broker, schema, user and temporary files. It never writes demo tables. Docker access and network access to pull the pinned image are required. CI's `rabbitmq-integration` job runs the same script against a dedicated MySQL service.

The real Go matrix checks:

- Confirmed publication before a missing SQL completion update, consumer commit before lost ACK, one delivery job and the actual WebSocket notification.
- Two consumers processing sixteen concurrent duplicate deliveries: one accepted effect and fifteen duplicates.
- An unbound mandatory route refusing source completion, followed by topology recovery.
- Invalid envelopes reaching a persistent quorum DLQ.
- A transient SQL failure recovering after delayed retry; persistent SQL failure exhausting its delivery limit and recovering through confirmed DLQ replay.
- Broker stop/start while a committed SQL event remains pending, followed by reconnection and delivery.

Direct Go invocation needs `GOSTREAM_TEST_RABBITMQ_URL` and `GOSTREAM_TEST_RABBITMQ_MYSQL_DSN` pointing to a dedicated migrated schema with an empty outbox. `GOSTREAM_TEST_RABBITMQ_CONTAINER` optionally names a **disposable** container the test is allowed to stop/start; without it, the restart subcase skips. The script supplies all three. Ordinary Go suites skip this matrix when external configuration is absent. SQLite unit tests separately cover strict envelopes, conflicting duplicates, rollback, cancellation, unavailable broker errors and targeted SQL replay.

The final local matrix passed all seven subcases in 48.12 seconds; API lifecycle and operator checks passed and cleanup reported zero remaining schemas/users. These are correctness timings, not a throughput benchmark. The first fixture failed because it asserted a deliberately hidden notification field; another attempt exposed Docker's randomly allocated host port changing on restart. The fixture now checks public notification fields and explicitly binds ports, retaining its state and recovery assertions.

## Remaining acceptance work

The smoke script additionally kills a real API after a committed follow request, restarts it and checks delivery, then drains five source events inserted while the API is stopped. This demonstrates general process restart and a small finite backlog. The kill is not gated on publisher-confirm/consumer-commit/ACK state; it may happen after delivery has completed. Sequential duplicate unit tests do not represent concurrent sustained load.

Actual producer/consumer kills at exact commit/confirm/ACK windows, unavailable/full DLQ targets, sustained backlog, multi-node quorum availability and the final combined MySQL/Redis/RabbitMQ/MinIO browser workflow remain unverified. No MQ throughput or exactly-once real-time delivery claim is made. Broker transport currently handles notifications; IM history/ACK and group hints retain SQL/Redis.

## Primary references

- [Publisher confirms in Go](https://www.rabbitmq.com/tutorials/tutorial-seven-go)
- [Consumer acknowledgements and confirms](https://www.rabbitmq.com/docs/confirms)
- [Quorum retry, delivery limits and at-least-once dead lettering](https://www.rabbitmq.com/docs/quorum-queues)
- [Pinned client release](https://github.com/rabbitmq/amqp091-go/releases/tag/v1.15.0)
