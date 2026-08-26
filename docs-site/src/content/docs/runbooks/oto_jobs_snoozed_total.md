---
title: oto_jobs_snoozed_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `kind`, `queue`, `reason` |
| Registered in | `internal/platform/jobs/metrics.go` |
| Alertable | **yes, but only on a sustained rate for one `reason`.** A snooze is not a failure |

## What it counts

Job executions deferred **without consuming an attempt**. Snoozing is how oto waits for a condition
instead of burning its retry budget on one — a rate-limited channel that snoozed 40 times still has
all 21 attempts.

Every snooze carries a reason, and that is enforced: snoozing without one makes a busy queue
indistinguishable from a broken one. The reasons seen in the tree today:

| `reason` | Where | Meaning |
|---|---|---|
| `not_due` | dispatch | the delivery's `next_attempt_at` has not arrived |
| `rate_limited` | job runtime and dispatch | the provider asked for a delay; `Retry-After` is honoured exactly |
| ordering-gate reasons — `awaiting_root`, `awaiting_predecessor` | dispatch | the thread head has not reached this slot yet (see [`oto_thread_order_decisions_total`](/oto/runbooks/oto_thread_order_decisions_total/)) |

## What a sustained rate means

Per reason:

- **`rate_limited` sustained** — you are fanning out faster than the provider will accept. Slack's
  per-channel write limit is about one message a second, which is why `deliver_slack` is a narrow
  queue: more workers buy contention, not throughput.
- **`awaiting_*` sustained** — deliveries are stacking behind a thread head. Cross-check
  [`oto_thread_head_wait_seconds`](/oto/runbooks/oto_thread_head_wait_seconds/); if that is also climbing, one
  thread is stalled and the gate is doing its job of not reordering around it.
- **`not_due` sustained** — normal. It is the scheduler polling work that is not ready.

## What to check

1. `sum by (reason) (rate(oto_jobs_snoozed_total[15m]))` against
   `rate(oto_jobs_succeeded_total[15m])` for the same kind. Snoozes far exceeding successes means
   nothing is getting through.
2. For `rate_limited`: the channel, and how many channels one policy targets.
3. For `awaiting_*`: `channel_threads.last_sent_seq` versus the queued deliveries for that thread.

## What to do

- Reduce fan-out, or accept the pacing. Slack's limit is not negotiable and oto respecting it is
  correct behaviour.
- If a thread is genuinely stuck, the gate will recover it and record why — see
  [`oto_thread_gap_recovered_total`](/oto/runbooks/oto_thread_gap_recovered_total/).
