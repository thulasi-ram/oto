---
title: oto_ingest_duplicate_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/ingestion/service/metrics.go` |
| Alertable | **no — a steady non-zero rate is healthy.** |

## What it counts

Batches suppressed by `ingest_dedup`: oto had already durably accepted an identical batch, so the
second one was answered 202 without being processed again.

## What a non-zero value means

**It means an HA Alertmanager pair is working as designed.** Alertmanager delivery is at-least-once
and every peer in a cluster sends independently; a two-peer cluster produces roughly one duplicate
per delivered batch. Suppressing them is the whole point of the dedup key.

Do not alert on this being non-zero. Two things are worth *looking* at:

- The ratio `duplicate / (duplicate + accepted)` jumping without a change in Alertmanager's peer
  count. That is usually an upstream retrying because it never saw the 202 — check
  [`oto_ingest_duration_seconds`](/oto/runbooks/oto_ingest_duration_seconds/) for accepts that are slower than
  the upstream's client timeout.
- The ratio going to zero on an HA pair. That means only one peer is delivering, which is a
  problem in Alertmanager, not in oto.

## What to check if you do look

`ingest_batches` for the dedup key, and Alertmanager's `alertmanager_cluster_members`.
