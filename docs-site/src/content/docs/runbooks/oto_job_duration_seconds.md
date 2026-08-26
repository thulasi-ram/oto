---
title: oto_job_duration_seconds
---
|  |  |
|---|---|
| Type | histogram (5 ms → 300 s) |
| Labels | `kind`, `queue`, `outcome` — `succeeded`, `snoozed`, `failed` |
| Registered in | `internal/platform/jobs/metrics.go` |
| Alertable | **yes**, per kind |
| Rule | `OtoJobsSlow` in `deploy/prometheus/oto-rules.yaml` |

## What it observes

Wall time of one job execution, handler only. The buckets span the real range on purpose: a Slack
delivery is tens of milliseconds, a 5 000-alert ingest batch is seconds, a partition sweep is
minutes.

**There is no single threshold.** Read it per `kind`:

| Kind | What "slow" means |
|---|---|
| `deliver.dispatch` | over a second or two = the provider, not oto |
| `ingest.process_batch` | scales with batch size; see [`oto_ingest_process_duration_seconds`](/oto/runbooks/oto_ingest_process_duration_seconds/) |
| `source.reconcile` | scales with the number of firing alerts upstream |
| `partitions.manage`, `retention.prune`, `stats.rollup` | minutes are normal; these are DDL-adjacent and single-worker |

## What a rise means

Latency here becomes depth on [`oto_job_queue_depth`](/oto/runbooks/oto_job_queue_depth/) and, for the ingest
queue, 503s on [`oto_ingest_shed_total`](/oto/runbooks/oto_ingest_shed_total/). Slow jobs are how a healthy
system becomes a shedding one.

Watch the `outcome` split: `failed` executions that are also slow usually mean a timeout is being
hit rather than an error being returned promptly, which wastes the whole retry budget on waiting.

## What to check

1. p99 by kind, then by queue. One kind is usually responsible.
2. For database-bound kinds: Postgres slow-query log and lock waits.
3. For `deliver.*`: the provider's own latency, and whether snoozes for `rate_limited` are climbing
   ([`oto_jobs_snoozed_total`](/oto/runbooks/oto_jobs_snoozed_total/)).
4. Whether a maintenance sweep overlaps the spike — `maintenance` is a single worker doing
   DDL-adjacent work and can contend with everything else.

## What to do

- Scale the queue that is slow, not the process. Queues are the isolation boundary; a wedged Slack
  workspace must not be able to stall ingestion.
- Cap the work per job before adding workers where the work is inherently serial (Slack per-channel
  writes, maintenance DDL).
