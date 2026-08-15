---
title: oto_ingest_alerts_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/ingestion/service/metrics.go` |
| Alertable | **no — informational.** |

## What it counts

Individual alerts accepted for processing, as opposed to the batches that carried them
([`oto_ingest_accepted_total`](/runbooks/oto_ingest_accepted_total/)). Incremented only for a batch that
was not a duplicate.

## Why there is no response

There is no value of this counter that is by itself wrong. It is a denominator and a shape:

- `rate(oto_ingest_alerts_total[5m]) / rate(oto_ingest_accepted_total[5m])` is your mean batch
  size, which is the number `docs/setup/tuning.md` wants when you size the storm thresholds and
  it is what makes the 5 000-alert batch cap meaningful.
- A step change in that ratio without a matching change in alert volume usually means a
  `group_by` change upstream, not an oto problem.

Alert on the batch counter, the rejection counter and the latency histogram instead. This one is
for reading a graph after the fact.
