---
title: oto_ingest_shed_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `reason` — `pool_exhausted`, `in_flight`, `queue_depth` |
| Registered in | `internal/ingestion/service/metrics.go`, incremented in `internal/ingestion/api/shed.go` |
| Alertable | **yes, on a sustained rate** |
| Rule | `OtoIngestShedding` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Requests answered **503 with a `Retry-After`** as deliberate backpressure.

Shedding is a feature (C17, ADR 0007), not a failure. Alertmanager retries 5xx — and only 5xx —
for about five minutes, so a 503 is a designed and sufficient backpressure channel. A 429 would be
a 4xx, and Alertmanager deletes a notification permanently and silently on any 4xx, which is why
oto has no rate limiter on this path.

So: a brief spike during a storm is the system working. A **sustained** rate is spending the
upstream's retry budget on queueing, and once it is spent the notification is gone.

## The reasons, and what each one actually says

| `reason` | What was full | First thing to look at |
|---|---|---|
| `queue_depth` | the `ingest` River queue is past `MaxQueueDepth` (default 25 000) | the workers, not the database |
| `pool_exhausted` | the ingest pgx pool has no free connection | Postgres latency, and the `ingest` queue workers sharing that pool |
| `in_flight` | the concurrency gate filled and the acquisition budget (`ingest.acquire_timeout`, 500 ms) elapsed | the same two, one level up |

The distinction is the point: "we are out of database" and "the workers are behind" have different
fixes.

## What to check

1. [`oto_job_queue_depth`](/runbooks/oto_job_queue_depth/) for `queue="ingest"`, `state="available"` — the
   25 000 default is roughly two and a half minutes of backlog at 16 workers, chosen to sit inside
   Alertmanager's retry budget.
2. `GET /readyz` — the `pools` block reports saturation without scraping `/metrics`.
3. [`oto_ingest_process_duration_seconds`](/runbooks/oto_ingest_process_duration_seconds/): are batches
   slow, or merely numerous?
4. Postgres: `log_min_duration_statement` is 500 ms in the dev compose; look for the statement that
   got slow, and for lock waits.

## What to do

- `queue_depth`: add `ingest` workers or pods. Do **not** simply raise `MaxQueueDepth` — that
  converts "shed early enough to be retried" into "accept work the upstream will give up on",
  which turns a 202 into a promise oto cannot keep.
- `pool_exhausted`: raise `OTO_DB_INGEST_SHARE_PERCENT` or the total pool, and check
  `OTO_DB_INGEST_STATEMENT_TIMEOUT` is still being honoured. Remember the accept handler and the
  `ingest` queue workers share this pool.
- `in_flight`: `MaxInFlight` should equal the ingest pool size. Setting it higher moves the queue
  into pgx where it is invisible and unmeasured.
- Never respond by returning 429 or 4xx from this path.
