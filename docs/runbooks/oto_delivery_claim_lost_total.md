# oto_delivery_claim_lost_total

|  |  |
|---|---|
| Type | counter |
| Labels | `mode` — `post_root`, `update_root`, `thread_reply`, `broadcast_reply` |
| Registered in | `internal/notification/service/metrics.go`; incremented in `internal/notification/service/dispatch.go` |
| Alertable | **YES — page on it.** The code says: *this counter is an alert, not a statistic* |
| Rule | `OtoDeliveryClaimLost` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Sends that **reached the provider** and could **not be recorded**, because the row was no longer
this worker's to write: the §G.5 claim lease expired mid-call and another worker reclaimed it, or a
recovery resolved the slot.

## Why every single increment matters

There is now a message in somebody's channel with no `sent` row behind it. Concretely:

- oto has forgotten a delivery it made, so the timeline is missing a message that exists;
- the slot may be retried or recovered, so oto **may send it again** — duplicates in the channel;
- for `post_root`, the thread's root handle was not written, so subsequent replies may start a
  second thread for the same group.

Nothing is retried automatically here, deliberately: returning an error would retry a send that has
already happened. The counter, plus the log line, are the entire recovery mechanism.

## What to check

1. The log record — it carries everything needed to find the orphan:
   ```
   msg="notification: a delivery landed but its claim was gone; nothing was recorded"
   delivery_id= thread_id= mode= provider_message_id= provider_conversation_id= attempts=
   ```
   `provider_message_id` is the message in Slack. Go and look at it.
2. `notification_deliveries` for `delivery_id`: what status is it in now, and did another worker
   complete it?
3. `channel_threads` for `thread_id`: is `provider_thread_id` set, and does it match the orphan?
4. [`oto_job_duration_seconds`](oto_job_duration_seconds.md) for `kind="deliver.dispatch"` —
   compare the provider-call latency against the 120 s lease.

## What to do

- **Sustained non-zero means the claim lease is shorter than the provider's real latency.** That is
  the diagnosis the code states outright. Confirm with the duration histogram; if p99 of a dispatch
  is anywhere near 120 s, the lease is the problem, not the provider.
- Check for **two workers on the same queue with clock disagreement**. The lease is time-based, so
  a skewed pod reclaims rows that are not stale.
- Look for pods being killed mid-send (OOM, aggressive rolling restarts): a worker that dies
  holding a claim releases it by expiry, and the retry lands on the same message.
- After the fact, reconcile by hand: the timeline entry can be recovered from
  `provider_message_id`, and duplicates in the channel should be explained rather than deleted.
