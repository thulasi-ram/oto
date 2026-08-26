---
title: oto_jobs_enqueued_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `kind`, `queue` |
| Registered in | `internal/platform/jobs/metrics.go` |
| Alertable | **no — informational.** |

## What it counts

Jobs inserted into the queue. It is the *input* side of the job runtime.

## Why there is no response of its own

No value is wrong by itself. It earns its keep as a denominator:

- `enqueued − started` over a window is work that has arrived and not begun. That is the same fact
  as [`oto_job_queue_depth`](/oto/runbooks/oto_job_queue_depth/), which is a gauge and cheaper to alert on.
- A step change in `enqueued` for one `kind` with no change in ingest volume is a fan-out change —
  a new policy, a new channel, a new org.

Alert on depth and on dead jobs. Read this one when explaining a graph.
