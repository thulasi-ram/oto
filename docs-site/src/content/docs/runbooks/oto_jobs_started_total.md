---
title: oto_jobs_started_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `kind`, `queue` |
| Registered in | `internal/platform/jobs/metrics.go` |
| Alertable | **no — informational.** |

## What it counts

Job executions begun: incremented after the payload-version gate passes and before the handler
runs. Every started job ends on exactly one of `succeeded`, `snoozed`, `failed` — and `dead` is a
subset of the last.

## Why there is no response of its own

It is the denominator for every rate you actually alert on:

- `rate(oto_jobs_failed_total) / rate(oto_jobs_started_total)` — the failure ratio, which is
  meaningful where a raw failure count is not.
- `started − succeeded − failed − snoozed` should be ≈ 0 over a long window. A persistent gap means
  jobs are being started and never finishing — check for a handler blocking without a context
  deadline.

`started` going to zero for a kind while `enqueued` keeps rising is the workers being gone, and
that is visible sooner and more cheaply on [`oto_job_queue_depth`](/runbooks/oto_job_queue_depth/).
