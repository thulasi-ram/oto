---
title: oto_stream_connections
---
|  |  |
|---|---|
| Type | gauge |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **no — informational.** |

## What it reports

Live SSE connections attached to **this pod**. One browser tab with the timeline open is one
connection.

## Why there is no response

Any value is legitimate: zero at 3 a.m. means nobody is looking, which is not a fault. It is a
denominator and a capacity input, not a symptom.

Where it is useful:

- Sizing. Each connection holds a bounded buffer; per-pod connection count is what you multiply.
- Explaining [`oto_stream_events_dropped_total`](/runbooks/oto_stream_events_dropped_total/) and
  [`oto_stream_resync_total`](/runbooks/oto_stream_resync_total/) — drops per connection is the meaningful
  ratio, not drops alone.
- Load-balancer sanity: a pod stuck at zero while its peers carry hundreds is a routing problem,
  visible here first.

Note that this is not a proxy for "the UI works". A client that reconnects in a loop keeps this
number healthy while seeing nothing; `listener_reconnects` and `resync` are the metrics that show
that.
