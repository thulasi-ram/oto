---
title: oto_stream_events_fetched_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **no — informational.** |

## What it counts

`ui_events` rows read from Postgres by the bridge and handed to the hub. This is the *source* side
of the stream: one row here fans out to every connected subscriber, which is why
[`oto_stream_events_published_total`](/oto/runbooks/oto_stream_events_published_total/) is normally much larger.

## Why there is no response

No value is wrong. Its uses are comparative:

- Against [`oto_stream_notify_received_total`](/oto/runbooks/oto_stream_notify_received_total/): fetches should
  track doorbells. Far more fetches than doorbells means the reconciling poll is carrying the
  stream — see [`oto_stream_poll_recovered_total`](/oto/runbooks/oto_stream_poll_recovered_total/).
- Against ingest volume: events are the UI's view of work done. Flat fetches while ingest is busy
  means events are not being written, which is an ingestion problem, not a streaming one.

Failures of this read are counted separately and *are* alertable:
[`oto_stream_fetch_errors_total`](/oto/runbooks/oto_stream_fetch_errors_total/).
