---
title: oto_stream_events_dropped_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **yes**, as a ratio |

## What it counts

UI events discarded because a subscriber's bounded buffer was full. **oto never blocks a writer for
a reader**: a slow browser cannot slow down ingestion, so its buffer is dropped instead.

A drop is not silent data loss. The subscriber is immediately sent a `resync` frame with
`reason="buffer_overflow"` — "everything you have is suspect, refetch" — which is why this metric
moves together with [`oto_stream_resync_total`](/oto/runbooks/oto_stream_resync_total/).

## What a sustained rate means

Clients cannot keep up. Either a storm is producing frames faster than a browser can consume them,
or one client is on a bad connection. The user-visible effect is a timeline that keeps re-fetching
instead of updating smoothly — annoying, never wrong: `ui_events` is durable and the refetch is
authoritative.

## What to check

1. The ratio, not the raw count:
   `rate(dropped) / (rate(published) + rate(dropped))`, against
   [`oto_stream_connections`](/oto/runbooks/oto_stream_connections/).
2. Is it correlated with an ingest spike? Compare with
   [`oto_ingest_alerts_total`](/oto/runbooks/oto_ingest_alerts_total/). A storm explains it.
3. Is it one pod? Drops concentrated on a single pod with normal connection counts points at that
   pod, not at the clients.
4. [`oto_stream_events_coalesced_total`](/oto/runbooks/oto_stream_events_coalesced_total/): if coalescing is
   *not* rising during the same window, frames are arriving spread out rather than bunched, which
   points at a slow reader rather than a fast writer.

## What to do

- Storm-driven: nothing. The coalescing window and the drop-then-resync design exist so that a
  storm degrades the UI's smoothness and nothing else.
- Sustained outside storms: look at client network conditions and at how many tabs one operator has
  open; each is a separate buffer.
- Do not respond by making the buffer unbounded. An unbounded buffer moves the failure from a
  resync frame to pod memory, where it takes the API with it.
