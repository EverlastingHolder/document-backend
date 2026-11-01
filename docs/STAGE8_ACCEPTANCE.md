# Stage 8: bounded OCR and burst run

This report records one controlled local run of the selected Go API and Python OCR worker. It demonstrates that this configuration processed the tested batch; it is not a general throughput or latency promise.

## Fixture quality check

- Five development captures represent four distinct document groups; one document is represented by two captures.
- All five serial OCR requests completed in five attempts, with no retry or timeout.
- The private reviewed gold covers 14 individually user-adjudicated fields: 12 confirmed and 2 corrected. The normalized comparison matched 11 and mismatched 3; all three mismatches were in the canonical `issuer` field across three captures. This leaves OCR quality work open. No OCR or gold values are published. Another 41 fields were left unscored because they were not individually adjudicated.
- This is a development set, not a held-out evaluation. The result does not establish a full-document accuracy rate.

## 100-request burst

- 100 concurrent upload requests were accepted with HTTP `202`; there were no `429`, `503`, network failures, or other request errors.
- The same valid 4,749,111-byte JPEG was submitted 100 times with distinct idempotency keys. This exercises job intake and worker throughput, not 100 different documents or 100 quality samples.
- The tested configuration used 8 upload admission slots and one OCR consumer on a 4-vCPU, 6-GiB test VM. All 100 jobs completed in 100 attempts, with no retries, failures, timeouts, or expired leases. Database attempt intervals showed a maximum overlap of one; this is an indirect concurrency observation, not a model-call counter.
- Upload acceptance latency: p50 3.313 s, p95 4.858 s, maximum 4.991 s. Acceptance-to-terminal latency: p50 255.508 s, p95 499.415 s, maximum 527.454 s. Processing is serialized by the single consumer; the terminal latency distribution reflects queueing across the batch.
- All 100 terminal results were acknowledged with `204`, and subsequent owner status reads returned `410`. At the end of the run, queued/unacked/dead-letter messages, outbox rows, image files/bytes, and result payloads were all zero; technical job/idempotency tombstones remain by design.
- Sampled container memory peaked at 3.18 GiB and CPU at 393% of the four allocated vCPUs. The final OCR child measurement was RSS/PSS 2.89 GiB with about 2.83 GiB anonymous memory. No cgroup OOM event was observed.

## Limits of this evidence

The requested real-OCR workload run and 100-request burst completed, but overall Stage 8 acceptance remains open because the quality comparison found three mismatches and the broader fault matrix was not repeated here. This was one bounded run on one test VM, using one repeated image and the configured single OCR consumer. It does not establish performance on varied documents, another machine or model snapshot, larger payloads, longer sustained traffic, or multiple OCR consumers. It is not a load guarantee or service-level objective. The burst run did not repeat crash/restart fault injection for every API, relay, worker, PostgreSQL, RabbitMQ, and filesystem boundary. The asynchronous queue has no promised job deadline; jobs can remain queued when no consumer is available.

The observed worker memory rise during the burst is recorded separately from source-level object lifetime inspection. The pipeline retains its most recent `PipelineResults` until the next image starts; this alone does not prove an unbounded leak. Longer soak testing and memory observation under varied images remain useful follow-up work.
