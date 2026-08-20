---
title: oto_thread_gap_recovered_total
---
|  |  |
|---|---|
| Type | counter |
| Labels | `reason` — `dead_delivery`, `skipped_delivery`, `missing_delivery`, `thread_dead` |
| Registered in | `internal/platform/jobs/ordering/gate.go` |
| Alertable | **yes.** The help string says: *sustained non-zero means a channel is broken* |
| Rule | `OtoThreadGapRecovered` in `deploy/prometheus/oto-rules.yaml` |

## What it counts

Thread sequence slots the ordering gate advanced **past, without being sent**.

A channel thread hands out strictly increasing `thread_seq` values and the head only moves forward,
so one slot that can never be filled would stall every message behind it forever. Rather than stall,
the gate advances the head and records a `delivery.skipped` event — oto's silence must never be
indistinguishable from "no alert" (SPEC §H.9), so the skip is visible per alert in the UI.

**`already_sent` is deliberately excluded.** A head catching up with a message that *did* land is
convergence, not breakage; counting it would make "sustained non-zero means a channel is broken"
untrue.

## What each reason means

| `reason` | What happened | Was a message lost? |
|---|---|---|
| `dead_delivery` | the delivery hit a terminal provider error | **yes** — that update never reached the channel |
| `skipped_delivery` | a coalesced no-op update | no — nothing needed sending |
| `missing_delivery` | the seq was allocated but no delivery row exists: the allocating transaction rolled back after `next_seq++` | **yes** — the slot can never be filled |
| `thread_dead` | the thread itself is terminal; nothing more will ever send | **yes**, for everything queued behind it |

So a rate dominated by `skipped_delivery` is benign. Any sustained rate of the other three means
people are not being told things oto decided to tell them.

## What to check

1. Split by reason first — the table above is the whole triage.
2. `notification_deliveries` for that thread: `status`, `error_class`, `attempts`. A run of
   `dead` rows points at the channel.
3. The channel's health and UI banner — `config_invalid` and `auth_expired` are the two classes
   that set it, and they are the usual cause of `dead_delivery` en masse.
4. [`oto_jobs_dead_total`](/runbooks/oto_jobs_dead_total/) for `kind="deliver.dispatch"`: the same failures
   seen from the job runtime.
5. `channel_threads` for the thread: `last_sent_seq` versus `next_seq`, and whether the thread is
   marked dead. (`frozen` was a fourth state and is gone — git-bug e5c060b; nothing ever wrote it,
   so a query for it always returned nothing.)
6. For `missing_delivery`: look for an error around the allocating transaction. A rollback after
   `next_seq++` is a bug in the write path, not an operational condition.

## What to do

- **`dead_delivery` / `thread_dead`**: fix the channel (re-authorise, re-invite the bot, correct
  the configuration), then confirm new deliveries land. Past messages are not resent — the timeline
  in oto is the record; the `delivery.skipped` events show precisely what the channel missed.
- **`missing_delivery`**: file it. It means a transaction allocated a sequence number and then
  rolled back, which no configuration causes.
- **`skipped_delivery` only**: no action. Consider excluding it from your alert expression if it
  dominates your rate.
