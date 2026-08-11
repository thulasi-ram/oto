# oto_stream_events_published_total

|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **no — informational.** |

## What it counts

UI events accepted into a subscriber's bounded buffer — the successful counterpart of
[`oto_stream_events_dropped_total`](oto_stream_events_dropped_total.md).

## Why there is no response

It is the denominator that makes drops interpretable:

```
rate(oto_stream_events_dropped_total[5m])
  / (rate(oto_stream_events_published_total[5m]) + rate(oto_stream_events_dropped_total[5m]))
```

That ratio is worth alerting on. The raw published count is not — it scales with connections and
with alert volume, and there is no wrong value.

`published` far exceeding [`oto_stream_events_fetched_total`](oto_stream_events_fetched_total.md) is
expected: one fetched row fans out to every subscribed connection.
