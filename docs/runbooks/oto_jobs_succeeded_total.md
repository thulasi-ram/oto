# oto_jobs_succeeded_total

|  |  |
|---|---|
| Type | counter |
| Labels | `kind`, `queue` |
| Registered in | `internal/platform/jobs/metrics.go` |
| Alertable | **no — informational.** |

## What it counts

Job executions whose handler returned nil.

## Why there is no response of its own

It is the health baseline, not a symptom. Use it as the denominator for failure and snooze ratios,
and as the sanity check that a queue is doing anything at all.

The one genuinely useful derived alert is *absence* for a kind that must always run —
`partitions.manage`, for instance, whose failure to run eventually means no partition exists for
tomorrow's rows. If you add that rule, alert on the periodic kinds specifically rather than on this
metric as a whole; a global drop is already covered by
[`oto_job_queue_depth`](oto_job_queue_depth.md) and `OtoDown`.
