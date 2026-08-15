---
title: oto_ingest_process_duration_seconds
---
|  |  |
|---|---|
| Type | histogram |
| Labels | `outcome` (includes `skipped` for a batch already processed) |
| Registered in | `internal/ingestion/service/metrics.go` |
| Alertable | **yes, secondary** |
| Rule | covered by `OtoQueueBacklog`; add a p99 rule if you run at volume |

## What it observes

`ingest.process_batch`: the asynchronous half, after the 202. This is where alerts become
occurrences, groups are formed and notifications are minted.

## What a rise means

Nothing is lost when this is slow — the batch is durable — but everything downstream is late: the
Slack message, the timeline, the reminder. And because the `ingest` queue is where the backlog
accumulates, a rise here is the direct cause of `queue_depth` shedding on the accept path.

`outcome="skipped"` is a batch that had already been processed. Non-zero is normal (a retry after a
crash between commit and completion) and needs no response.

## What to check

1. [`oto_job_queue_depth`](/runbooks/oto_job_queue_depth/) for `queue="ingest"` — is it one slow batch or a
   thousand queued ones?
2. [`oto_job_duration_seconds`](/runbooks/oto_job_duration_seconds/) for `kind="ingest.process_batch"`,
   which is the same work seen by the job runtime and carries the outcome split.
3. Batch size (see [`oto_ingest_alerts_total`](/runbooks/oto_ingest_alerts_total/)). A 5 000-alert storm
   batch is *expected* to take seconds.
4. Postgres contention on `alerts`, `occurrences` and `alert_groups`.

## What to do

- Add `ingest` workers before anything else; the queue is a concurrency boundary precisely so this
  is a safe dial.
- If one org dominates, look for a rule with runaway label cardinality — that is a rule problem
  upstream, not a capacity problem here.
