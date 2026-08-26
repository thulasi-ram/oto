---
title: oto_ingest_duration_seconds
---
|  |  |
|---|---|
| Type | histogram |
| Labels | `outcome` — `accepted`, `duplicate`, `unavailable`, `undecodable`, `unknown_source` |
| Registered in | `internal/ingestion/service/metrics.go` |
| Alertable | **yes** |
| Rule | `OtoIngestLatencyBudget` in `deploy/prometheus/oto-rules.yaml` |

## What it observes

The webhook accept path end to end: decode, validate, commit, enqueue, answer. It stops when oto
answers, not when the batch is processed — that is
[`oto_ingest_process_duration_seconds`](/oto/runbooks/oto_ingest_process_duration_seconds/).

**p99 budget is 250 ms. The hard ceiling is 5 s**, because Alertmanager's retry floor is 10 s: an
accept slower than that is competing with the upstream's own retry.

## What a breach means

The 202 is oto's promise that a batch is durable. Making the upstream wait for it costs the
upstream a connection and, past a few seconds, its patience — which shows up as duplicate
deliveries ([`oto_ingest_duplicate_total`](/oto/runbooks/oto_ingest_duplicate_total/)) and then as gaps.

Read the `outcome` label before anything else:

- `accepted` slow → the commit is slow. A database problem.
- `unavailable` slow → you are queueing before shedding; see
  [`oto_ingest_shed_total`](/oto/runbooks/oto_ingest_shed_total/).
- `undecodable` slow → a huge body being parsed and refused. Check `body_too_large` on
  [`oto_ingest_rejected_total`](/oto/runbooks/oto_ingest_rejected_total/).

## What to check

1. `GET /readyz` → `pools`, for ingest-pool saturation.
2. Postgres slow-query log (`log_min_duration_statement`), and lock waits on `ingest_batches` /
   `alerts`.
3. Batch size: `rate(oto_ingest_alerts_total) / rate(oto_ingest_accepted_total)`. A `group_by`
   change upstream can multiply batch size overnight.
4. Whether the maintenance queue is doing DDL — `partitions.manage` is DDL-adjacent.

## What to do

- Database-bound: give the ingest pool more connections, or the database more of whatever it is
  short of. The two-pool split exists so this cannot be starved by the UI.
- Batch-bound: `MaxAlertsPerBatch` truncation is preferable to unbounded accept latency; see
  `docs/setup/tuning.md`.
- If you cannot fix it quickly, shedding earlier is better than accepting slowly. A 503 is
  retried; a timeout is a lost notification.
