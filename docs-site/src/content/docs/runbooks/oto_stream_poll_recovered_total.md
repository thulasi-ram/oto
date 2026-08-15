---
title: oto_stream_poll_recovered_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | none |
| Registered in | `internal/streaming/service/metrics.go` |
| Alertable | **yes, on a spike — not on being non-zero.** |
| Rule | `OtoStreamPollRecoverySpike` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Events found by the reconciling poll **below the published watermark**: rows that committed out of
sequence order, or notifications that were lost.

The help string is explicit: **non-zero is normal.** `ui_events.seq` is a BIGSERIAL allocated before
commit, so two transactions can commit in the opposite order to their sequence numbers; the poll
exists precisely to catch the row that appeared "behind" the watermark. This is a designed
mechanism, not a repair.

## What a spike means

The poll has stopped being a safety net and started being the delivery mechanism. Either:

- **notifications are being missed** — check
  [`oto_stream_listener_reconnects_total`](/runbooks/oto_stream_listener_reconnects_total/) and
  [`oto_stream_notify_malformed_total`](/runbooks/oto_stream_notify_malformed_total/); or
- **write concurrency has risen sharply**, so more rows commit out of order — usually a storm, and
  self-correcting.

The user-visible effect is latency: events arrive at poll cadence rather than instantly. Nothing is
lost either way, because `ui_events` is the durable spine and the poll reads it.

## What to check

1. Compare with [`oto_stream_notify_received_total`](/runbooks/oto_stream_notify_received_total/). Poll
   recoveries approaching the doorbell rate means the doorbell is not working.
2. Reconnects in the same window — one reconnect is one gap.
3. Ingest volume in the same window: a storm raises this legitimately.
4. Long-running transactions holding `ui_events` writes open (`pg_stat_activity`,
   `state='idle in transaction'`), which widen the out-of-order window.

## What to do

- If reconnects or malformed payloads explain it, fix those; this counter will follow.
- If it is a storm, nothing.
- If neither, look for a long-running writer. Do not respond by shortening the poll interval —
  that hides the cause and puts load on the database.
