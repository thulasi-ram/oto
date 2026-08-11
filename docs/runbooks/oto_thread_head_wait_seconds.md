# oto_thread_head_wait_seconds

|  |  |
|---|---|
| Type | histogram (50 ms → 900 s) |
| Labels | `outcome` — currently `proceed` |
| Registered in | `internal/platform/jobs/ordering/gate.go` |
| Alertable | **yes** |
| Rule | `OtoThreadHeadWaitHigh` in `deploy/prometheus/oto-rules.yaml` |

## What it observes

How long a delivery had been queued when the gate finally let it proceed — measured from the job's
creation to the moment ordering allowed the send.

This is **time-to-human**: the delay between oto deciding to say something and being allowed to say
it, before the provider call even starts.

## What a rise means

Messages are stacking behind a thread head. Ordering is not optional — one channel thread has one
order, and reordering around a stuck slot would put a resolution above the firing it resolves — so
the gate waits rather than skips, up to the point where it decides the thread needs recovering.

A p95 in the minutes means a channel is effectively broken even though nothing has failed yet:
alerts are being fired, deliveries are being minted, and nobody is seeing them.

## What to check

1. [`oto_thread_order_decisions_total`](oto_thread_order_decisions_total.md): is the wait
   `awaiting_root` (the root never landed) or `awaiting_predecessor` (one slot in flight)?
2. `channel_threads` for the worst thread: `last_sent_seq` versus `next_seq`, and the age of the
   `sending` row. The §G.5 claim lease is 120 s — a `sending` row older than that is treated as
   abandoned.
3. [`oto_job_queue_depth`](oto_job_queue_depth.md) for `deliver_slack`. That queue is narrow on
   purpose (Slack allows about 1 msg/s per channel), so a fan-out spike shows up here as wait.
4. Provider latency: [`oto_job_duration_seconds`](oto_job_duration_seconds.md) for
   `kind="deliver.dispatch"`.

## What to do

- If it is one thread: let the gate recover it, and check
  [`oto_thread_gap_recovered_total`](oto_thread_gap_recovered_total.md) for what it had to skip.
- If it is many threads on one channel: the provider is slow or throttling. Reduce fan-out —
  more `deliver_slack` workers will not beat a per-channel write limit.
- If `awaiting_root` dominates: root posts are failing. Look at the delivery errors for
  `mode="post_root"` and at the channel's health.
