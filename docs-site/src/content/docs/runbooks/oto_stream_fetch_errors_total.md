---
title: oto_stream_fetch_errors_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **yes** |
| Rule | `OtoStreamFetchErrors` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Failed catch-up reads of `ui_events` by the bridge: the query that turns a doorbell, or a poll tick,
into frames for connected clients.

## What a non-zero rate means

The stream's read path is failing. While it fails, the UI stops updating live — the timeline is
still correct on refresh, because every event is durable and re-fetchable through the API, but
nothing arrives on its own.

It is also a warning about the general pool: this read comes from the pool the UI and the API share,
so a sustained failure here usually has company on `/readyz` and in the API's own error rate. It is
*not* the ingest pool — ingestion can be perfectly healthy while this fails, which is the two-pool
design working.

## What to check

1. The error in the bridge's log (`streaming:` records) — timeout, connection refused, permission,
   or relation-not-found each mean something different.
2. `GET /readyz` → `database`, `migrations`, and the `pools` block for general-pool saturation.
3. Postgres: is `ui_events` partitioned as expected? The partitions are hourly and managed by
   `partitions.manage`; if that job is dead, a read can hit a missing partition. Check
   [`oto_jobs_dead_total`](/oto/runbooks/oto_jobs_dead_total/) for `kind="partitions.manage"`.
4. Whether it started at a migration — `just status`, and `schema_version` from
   `GET /api/v1/version`.

## What to do

- Database unreachable or saturated: the fix is the database, and ingestion is unaffected in the
  meantime.
- Missing partition: run the maintenance path and find out why `partitions.manage` stopped.
  Retention here is DROP PARTITION by design, so a broken sweep is felt on both ends.
- Migration-shaped: roll to the build matching the schema. The bridge reads a fixed shape.
- Tell operators the timeline is refresh-only until it clears; nothing is being lost.
