---
title: oto_job_queue_depth
---
|  |  |
|---|---|
| Type | gauge |
| Labels | `queue`, `state` — River's `available`, `running`, `retryable`, `scheduled`, `discarded`, `cancelled` |
| Registered in | `internal/platform/jobs/metrics.go`; sampled in `internal/platform/jobs/client.go` |
| Alertable | **yes** |
| Rule | `OtoQueueBacklog` in `deploy/prometheus/oto-rules.yaml` |

## What it reports

The backlog, sampled periodically from `river_job` and **reset before each sample**, so a queue that
drains to zero stops reporting its last value. (A stale gauge here would be a backlog alert that
never clears.) A missing series therefore means "no jobs in that state", not "no data".

Read the states separately:

| `state` | Meaning |
|---|---|
| `available` | waiting for a worker — **this is the backlog** |
| `running` | in flight; bounded by worker count |
| `retryable` | failed and scheduled to retry; rising means a dependency is down |
| `scheduled` | deliberately deferred (snoozes, periodic ticks). Non-zero is normal |
| `discarded` / `cancelled` | terminal; correlates with [`oto_jobs_dead_total`](/oto/runbooks/oto_jobs_dead_total/) |

## What a rising `available` means

Work is arriving faster than it is being done. For `queue="ingest"` specifically it has a hard
consequence: past `MaxQueueDepth` (default **25 000**) the accept path starts answering 503, which
is [`oto_ingest_shed_total`](/oto/runbooks/oto_ingest_shed_total/) with `reason="queue_depth"`. That number is
derived, not chosen — roughly two and a half minutes of backlog at 16 workers, deliberately inside
Alertmanager's ~5-minute retry budget.

For `deliver_slack`, depth is latency to a human: every queued job is a message somebody has not
seen yet.

## What to check

1. Which queue, and is `running` at its worker ceiling? If yes it is capacity; if no, the workers
   are not picking work up — check the worker process is alive (`oto worker` / `just worker`) and
   that the queue is registered in its configuration.
2. [`oto_job_duration_seconds`](/oto/runbooks/oto_job_duration_seconds/) for that queue: fewer, slower jobs or
   more, normal ones?
3. `retryable` climbing alongside → a dependency is down; fix that, not the concurrency.
4. `GET /readyz` → `pools`, for the ingest queue specifically (it shares the ingest pool).

## What to do

- Add workers or pods for the specific queue. Queues are concurrency boundaries precisely so this
  is safe and targeted.
- Do not raise `MaxQueueDepth` to stop the shedding alert. Shedding early is what keeps a 202 an
  honest promise; accepting deeper means accepting batches the upstream will abandon.
- `deliver_slack` is intentionally narrow — Slack's per-channel limit is about 1 msg/s. Widening it
  buys contention. Reduce fan-out instead.
