---
title: oto_stream_notify_received_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **no — informational.** |

## What it counts

LISTEN/NOTIFY doorbells received on the `oto_events` Postgres channel. A doorbell says "there are
new rows"; the bridge then reads `ui_events` for the actual content.

## Why there is no response

The doorbell is an optimisation, not the delivery mechanism. If every notification were lost the
stream would still be correct, because the reconciling poll reads the same rows — just later. That
is why the alertable metric here is
[`oto_stream_poll_recovered_total`](/oto/runbooks/oto_stream_poll_recovered_total/) (the poll doing more work
than it should) rather than this counter going quiet.

Useful comparisons:

- Flat while [`oto_stream_events_fetched_total`](/oto/runbooks/oto_stream_events_fetched_total/) climbs → the
  LISTEN connection is not delivering; check
  [`oto_stream_listener_reconnects_total`](/oto/runbooks/oto_stream_listener_reconnects_total/).
- Climbing while fetches stay flat → doorbells with nothing behind them, or fetches failing
  ([`oto_stream_fetch_errors_total`](/oto/runbooks/oto_stream_fetch_errors_total/)).

Note the dev compose sets `wal_level=logical`, because the SSE bridge depends on this channel.
