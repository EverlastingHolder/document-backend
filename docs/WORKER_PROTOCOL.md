# Private OCR job protocol

## Boundary

This protocol is for the internal Go job execution service and the private OCR
consumer. It is not a public client API. The consumer has no PostgreSQL
credentials. It receives messages from RabbitMQ, reads an image through the
read-only image-store mount, and calls the private Go worker API. Worker
credentials are separate from client bearer credentials. Do not log tokens,
field payloads, image bytes, file paths, or OCR diagnostics.

The public `GET /v1/passport/jobs/{job_id}` remains a later stage. These worker
routes do not make the public polling endpoint available. Stage 4 relay,
private worker API, Python consumer and lifecycle code are implemented; the
delivery/recovery acceptance gate is still in progress. Compose has a private
`queue` profile for controlled integration, not production deployment or OCR
quality acceptance.

## RabbitMQ message

The JSON object has exactly these fields:

```json
{
  "schema_version": 1,
  "event_id": "opaque UUIDv4",
  "job_id": "opaque UUIDv4",
  "attempt_no": 1
}
```

`schema_version` is integer `1` on the wire. PostgreSQL stores the event
schema version as text `v1`; the Go relay validates that stored version before
encoding the envelope. This message is distinct from the newline-JSON
`passport-russiandocsocr-worker/v1` protocol used by the legacy synchronous
child process. Messages contain no image, owner, credential, path, OCR text, or
passport data.

## Private Go routes

All routes require the worker-only bearer credential and are served on a
separate Go listener bound to an explicit local address. Worker credentials use
the `pwt1_` token purpose and `passport-worker-credentials/v1` hash registry;
they are separate from public client `pbt1_` credentials. The Go process loads
only the hash registry; the raw token is provisioned to the OCR consumer through
the procedure in [WORKER_RUNTIME.md](WORKER_RUNTIME.md). Deployment must
restrict routing to the OCR consumer; binding to a local/private address does
not configure a firewall or replace the credential. Requests use JSON, reject
unknown/duplicate properties, and use the message identity and fencing token
as correlation values.

### Claim

`POST /internal/worker/v1/claim` takes the RabbitMQ envelope unchanged. A
successful response is one of:

```json
{"decision":"execute","execution":{
  "attempt_token":"opaque UUIDv4",
  "image_key":"opaque UUIDv4",
  "media_type":"image/jpeg",
  "size_bytes":123,
  "attempt_no":1,
  "deadline_at":"UTC timestamp",
  "lease_expires_at":"UTC timestamp",
  "pipeline_version":"configured by Go",
  "hard_processing_timeout_ms":480000,
  "completion_budget_ms":120000
}}
```

```json
{"decision":"wait","retry_after_ms":1000}
```

```json
{"decision":"ack"}
```

`execute` is returned only after PostgreSQL has committed the processing state
and a new attempt with a unique fencing token. The consumer opens `image_key`
under its read-only mount; it must not accept a path from the message. Go
configuration supplies `pipeline_version` and the hard processing timeout.
The attempt lease must exceed the hard processing timeout plus the configured
completion budget. `wait` means a live lease or a not-yet-due retry exists; the
consumer keeps the delivery unacknowledged and waits before claiming again.
It must not use an immediate `nack/requeue` loop. `ack` means PostgreSQL
already proves the message is terminal, superseded, or durably resolved. The
timeout numbers in the example are illustrative values, not configuration
defaults or a processing SLO.

### Complete

`POST /internal/worker/v1/complete` takes the envelope fields, `attempt_token`,
and a `fields` object containing exactly the 11 canonical source names:
`surname`, `given_name`, `patronymic`, `gender`, `birth_date`, `birth_place`,
`series`, `number`, `issue_date`, `issuer`, and `department_code`. Each value is
a string or `null`; values are bounded and cannot contain NUL. The consumer
does not provide `request_id` or `pipeline_version`.

Go adapts the fields to the existing internal extraction type and calls the
existing canonical `api.MapExtraction` mapper with `job_id` as request ID and
the configured pipeline version. In one PostgreSQL transaction it stores the
canonical `RecognitionResponse`, marks the job completed, and marks the attempt
completed. A duplicate matching completion is acknowledged idempotently. A
conflicting duplicate cannot overwrite the saved response; Go returns an ACK
decision with a safe conflict category for observability.

### Fail and reject

`POST /internal/worker/v1/fail` takes the envelope, `attempt_token`, and one
closed error code: `image_unprocessable`, `processing_failed`,
`ocr_unavailable`, or `processing_timeout`. `image_unprocessable` is terminal.
Other codes schedule a bounded retry with database `available_at` and a new
outbox event, or a terminal failed state when the configured attempt limit is
reached. The attempt update, job transition, and next outbox event are atomic.

`POST /internal/worker/v1/reject` takes only the envelope. For a known queued
event whose payload cannot be processed, Go commits a terminal
`processing_failed` state before returning ACK. A known event with a valid
event/job/attempt identity but unsupported wire schema version is handled this
way. Unknown identities are rejected for dead-letter handling and do not
modify a job.

Complete, fail, and reject callbacks return `{"decision":"ack"}` only after a
durable terminal, retry, or superseded-state decision. If the event is known
but another attempt still owns a live lease, a callback may return
`{"decision":"wait","retry_after_ms":1000}`; the value comes from the
operator-configured Go `queue_processing.poll_interval`. The consumer keeps the
RabbitMQ delivery unacknowledged and delays before retrying. Invalid/unknown
delivery identity is a `400` and should be dead-lettered; generic storage
unavailability is `503` and must not be acknowledged.

For all routes, transient database failure or unknown commit outcome is an
error response and must not be acknowledged. The worker acknowledges RabbitMQ
only after a durable `ack` decision or after a retry has been committed.
Late completions with an old fencing token cannot replace a newer attempt or
terminal result.

## Outbox and recovery

The Go relay leases due outbox rows in a short PostgreSQL transaction, commits
the lease, then publishes with publisher confirms and mandatory routing. It
marks the row published only if that lease is still current. A crash after a
broker confirm but before the database update can produce a duplicate
delivery, so the consumer protocol is idempotent. Publish failures release the
current lease, record only a fixed safe category, and set a bounded retry time.

Recovery scans expired processing leases and transactionally schedules a
retry or terminal failure. The RabbitMQ message carries only the four-field
envelope described above; PostgreSQL owns attempt and job state. Dead-letter
storage is diagnostic and is not the source of truth for job completion.
