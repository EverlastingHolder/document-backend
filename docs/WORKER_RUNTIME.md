# Private OCR worker runtime

The Go API owns public HTTP, PostgreSQL state transitions, canonical result
mapping, and the RabbitMQ outbox relay. The Python OCR consumer is a private
worker. It has no public listener and no PostgreSQL credentials. RabbitMQ
messages contain only `schema_version`, `event_id`, `job_id`, and `attempt_no`;
the Go worker API supplies the fenced image metadata and commits lifecycle
changes. See [WORKER_PROTOCOL.md](WORKER_PROTOCOL.md) for the wire contract and
state transitions.

Stage 4 adds the separate Go worker listener, publisher, consumer, and recovery
loop. The public `GET /v1/passport/jobs/{id}` result route remains a later-stage
deliverable. `POST /v1/passport/jobs` queues work; a worker ACK only confirms a
durable execution decision, not that a client has read the result. Retention,
automatic deletion, and a client completion deadline are not currently
promised.

## Separate worker credentials

Client and worker tokens are different credential purposes. Client tokens use
the `pbt1_` prefix and the
`passport-client-credentials/v1` registry. Worker tokens use the `pwt1_`
prefix and the `passport-worker-credentials/v1` registry. The Go process loads
only the hash registry for the corresponding listener. A client registry is
not valid for worker routes, and a worker registry is not valid for public
client authentication.

Generate a worker credential on an administrative machine in a new private
directory outside every Git checkout:

```sh
umask 077
mkdir -m 700 /srv/passport-admin/worker-credential-YYYYMMDD
go run ./cmd/client-token -purpose worker -private-dir /srv/passport-admin/worker-credential-YYYYMMDD
```

The command writes one mode-0600 raw token file and one mode-0600 hash-only
registry file. It prints artifact paths, never the raw token. Deliver the
`.token` file through the operator's secure provisioning channel to the OCR
worker. Install the generated `.registry.json` on the API as the worker
registry, or merge its single hash entry into the existing strict registry
while preserving the schema and other authorized entries. Never put the raw
token in API configuration, PostgreSQL, logs, a repository, or an application
package.

For rotation, generate another token with the existing `-worker-id` so the
worker principal remains stable. Install the updated hash registry and restart
the API, then securely provision the new raw token to the worker and restart
it. Revoke by removing the old hash entry, installing the registry, and
restarting the API. A worker token cannot create jobs, choose an owner, or use
the public client-authentication routes.

## Worker-only HTTP API

The API binds a second, concrete private address configured by
`queue_processing.private_listener_address`. It never registers these routes
on the public listener. Requests require exactly one valid worker `Bearer`
credential and strict JSON. Authentication happens before the request body is
read. The request body is bounded; duplicate and unknown JSON fields are
rejected.

| Route | Request | Response |
| --- | --- | --- |
| `POST /internal/worker/v1/claim` | Flat envelope: integer `schema_version`, UUIDv4 `event_id`, UUIDv4 `job_id`, positive `attempt_no` | `decision` is `execute`, `wait`, or `ack`; an execute result contains only the private image key, MIME type, byte size, attempt token/number, lease/deadline, processing/completion budgets, and pipeline version. |
| `POST /internal/worker/v1/complete` | Flat envelope plus `attempt_token` and the 11 nullable source fields in `fields` | `decision` is `ack` only after the canonical Go result and terminal state are committed; `wait` keeps the RabbitMQ delivery unacknowledged. |
| `POST /internal/worker/v1/fail` | Flat envelope plus `attempt_token` and one closed error category | `decision` is `ack` after the retry or terminal failure is committed; `wait` keeps the delivery unacknowledged. |
| `POST /internal/worker/v1/reject` | Flat envelope only; unsupported positive schema versions are allowed for a known poison event | `decision` is `ack` after a known queued event is durably terminalized; `wait` keeps an active delivery unacknowledged. |

The worker must keep a delivery unacknowledged for `wait` and transient HTTP
errors. It may ACK only when Go returns `decision: ack`. PostgreSQL is
authoritative; RabbitMQ ACK is sent only after Go confirms a committed state.
Claim waits use a database-derived `retry_after_ms`; callback waits include a
polling hint derived by the API from the explicit `queue_processing.poll_interval`;
the worker caps it by its configured wait interval.
Unknown event/job identities are permanent delivery errors and can be
dead-lettered. A late completion cannot replace a newer attempt because the
attempt token fences writes. Error responses do not include SQL, filesystem,
broker, or image details.

The Go claim response supplies `deadline_at`, `lease_expires_at`,
`hard_processing_timeout_ms`, and `completion_budget_ms`. The worker must
respect these values, use a per-request HTTP timeout no greater than the
completion budget, and avoid an OCR attempt when the lease cannot cover its
remaining processing and callback budget. Go recovery reclaims expired
attempts; there is no lease heartbeat.

The outbox publisher and worker declare the same durable exchange, quorum work
queue, routing key, dead-letter exchange/queue, and consumer acknowledgement
timeout. The API checks the topology before serving when RabbitMQ is reachable.
A temporary broker outage is logged as a safe category; the API still accepts
durable jobs in PostgreSQL and the relay retries in the background. A reachable
broker with incompatible topology or rejected credentials fails startup
closed. The consumer's
`consumer_ack_timeout_ms` must exactly match the API's
`queue_processing.consumer_ack_timeout`; API config requires at least five
minutes and more than the attempt lease plus completion budget. The fixed
topology is declared in the worker runtime and `internal/outbox`.
The quorum queue disables RabbitMQ's default delivery-count limit so transient
worker/channel loss does not dead-letter a valid job after a fixed number of
redeliveries. OCR retry limits remain database-controlled by `max_attempts`;
broker redelivery is not an OCR attempt. This permits indefinite redelivery
during a long outage, so operators must monitor queue depth and restore the
worker/API boundary. Permanent malformed or unknown deliveries are explicitly
sent to the dead-letter queue by the consumer.

## API container and image files

`deploy/Dockerfile.api` builds a Go-only API image. It contains neither Python
nor OCR model files. `deploy/api-entrypoint.sh` starts as root only to copy the
nonsecret API config and four required API files (`postgres_dsn`, `broker_dsn`,
`client_credentials`, and `worker_credentials`) into the new private
`/run/passport-api` tmpfs and then drops to the configured numeric application
UID/GID with `su-exec`.
Password files for PostgreSQL and RabbitMQ are not copied into the API
container; the DSN files already contain the connection credentials.

Use `deploy/api.queue.example.json` as a field reference and replace its
pipeline version, private addresses, limits, paths, and explicit timing policy
with operator-selected values. The API config is mounted read-only at
`/run/config/api.queue.json`. Every file path in the config points to the
prepared tmpfs secret or to a separately mounted runtime path. Compose binds
the operator-selected `PASSPORT_API_STATE_ROOT` read/write to
`/var/lib/passport/state` for API logs and metrics. Prepare it as a private
0700 directory owned by the API UID/GID; it is not shared with the worker.
Point it at the existing state directory to retain the configured log and
metrics files across container restarts. The entrypoint never recursively
changes its ownership or copies it to tmpfs. No queue policy durations are
implicit defaults.

The queue-only Go image sets `sync_ocr_enabled` to `false`, so it does not
start the legacy Python recognizer. The existing synchronous route remains
available for compatibility and returns its existing `503 ocr_unavailable`
response in this mode. Configurations without `sync_ocr_enabled` retain the
legacy default and start Python as before.

Set `PASSPORT_IMAGE_ROOT` to the exact existing mode-0700 private image
directory used by the asynchronous intake. Compose binds that directory
read/write into the API at `/var/lib/passport/images` and read-only into the
worker at the same path.
The API and worker must run with the same UID/GID. The entrypoint never
recursively changes the image directory's owner and never replaces it with an
empty volume. Existing files and PostgreSQL rows must remain paired during
deployment; a permission or UID mismatch fails closed for operator repair.
Compose defaults both IDs to `10001` for a new private image root. For an
existing root, set `PASSPORT_APP_UID` and `PASSPORT_APP_GID` to its current
owner and group before starting the API and worker.

The worker config, mounted at `/run/config/ocr-worker.json`, separately supplies
RabbitMQ and the worker raw token file, the Go private worker URL, the same
image root (read-only), and the pinned OCR source/model mounts. Its strict JSON
must set `expected_upstream_version` and `expected_model_lock_version` to the
operator-reviewed snapshot versions. Startup checks the source version and
the model lock's version, sizes, and SHA-256 hashes; it does not update the
snapshot. Do not mount the PostgreSQL DSN, API client registry, or database
credentials into the worker. Do not publish RabbitMQ or the private worker
listener on host ports.
