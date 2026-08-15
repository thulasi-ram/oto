# oto_stream_resync_total

|  |  |
|---|---|
| Type | counter |
| Labels | `reason` — `buffer_overflow`, `replay_window_exceeded` |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **yes**, on a sustained rate per reason |
| Rule | `OtoStreamResyncStorm` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

`resync` frames sent to clients. A resync means "everything you are holding is suspect — refetch
from the API". It is a correctness mechanism, not an error: oto would rather tell a client to start
again than let it render stale state.

| `reason` | Cause |
|---|---|
| `buffer_overflow` | that connection fell behind and its buffer was dropped — see [`oto_stream_events_dropped_total`](oto_stream_events_dropped_total.md) |
| `replay_window_exceeded` | the client's `Last-Event-ID` is older than the 24 h `ui_events` retention, or the gap exceeds `MaxReplayRows` (10 000) |

## What a sustained rate means

- **`buffer_overflow` sustained**: clients cannot keep up, exactly as the help string says. Every
  resync is a full refetch, so a loop here turns one slow client into API load.
- **`replay_window_exceeded` sustained**: clients are reconnecting with event ids far behind the
  head. Normal for a laptop that was closed overnight (the replay window *is* the 24 h retention —
  a longer promise would be one oto cannot keep). Sustained during working hours means clients are
  disconnected for long stretches, or something is replaying a stale cursor.

## What to check

1. Split by reason; they have nothing in common except the frame.
2. For `buffer_overflow`: go to
   [`oto_stream_events_dropped_total`](oto_stream_events_dropped_total.md).
3. For `replay_window_exceeded`: is `ui_events` retention what you think? The partitions are hourly
   and dropped at 24 h by `partitions.manage`. If that job is failing, retention shrinks —
   check [`oto_jobs_dead_total`](oto_jobs_dead_total.md) for `kind="partitions.manage"`.
4. Gaps in `seq` are **normal** — it is a global sequence and a rolled-back transaction consumes a
   value. A client that treats a gap as loss will resync forever; if a bespoke client is doing
   that, this counter is where it shows.

## What to do

- Client-side loops: fix the client's cursor handling before touching the server.
- Storm-driven overflow: no action; see the dropped-events page.
- Never widen the replay promise beyond `ui_events` retention. The two are the same number on
  purpose.
