---
title: 0023 — Terminal thread states are decided first, and the send is three phases
---
**Status:** Accepted · 2026-08-09 · **Narrowed once** — `frozen` left the thread state
vocabulary on 2026-08-19 (git-bug `e5c060b`, migration `00066`); the decision itself is
intact and the gate order is now `dead` → unsequenced → already-resolved → root →
predecessor → proceed. See the callout under **Decision**.
**Amends:** SPEC §G.7 (which specified the opposite order, and a single-transaction send)
**Relates to:** [0005](/oto/adr/0005-durable-group-key-owns-the-slack-thread/) (the thread a generation owns),
[0008](/oto/adr/0008-slack-update-in-place-primary/) (update-in-place, which is why a duplicate root is the
expensive failure), [0021](/oto/adr/0021-correctness-and-testing-strategy/) (the audit that found both)

## Context

Ordering inside one `ChannelThread` is enforced by a sequence gate: a delivery may send only while
holding the thread's advisory lock and only when `thread_seq == last_sent_seq + 1`. Two decisions
inside that gate turned out to be load-bearing in a way the SPEC had backwards.

**1. The switch tested "no root yet" before "the thread is dead."** As written, a delivery whose mode
needed a root and whose thread had no `provider_thread_id` snoozed for two seconds — and the dead
check sat *below* it, unreachable for exactly the deliveries it existed to rescue.

The witness is one Slack error. A root delivery gets `channel_not_found`; §H.9 marks the thread
`dead`; the thread still has a null `provider_thread_id`, because nothing ever landed. Every reply,
every update and every reminder queued behind it now matches case 1 and snoozes. **A River snooze
consumes no attempt, by design** — an item waiting its turn has not failed — so the attempt ceiling,
the one bound that would eventually turn this into a dead-letter, can never fire. The thread waits at
0.5 Hz for a root that nothing left in it will ever post. No message, no `dead` row, no
`delivery.skipped` event, no metric: the destination is silent and the alert page says *pending*.
For a product whose stated line is "oto's silence must never be indistinguishable from no alert",
that is the worst reachable state.

Worse, it was not a rare interleaving. It needs no race at all — one terminal error on one root
message is the entire reproduction.

**2. The provider call sat inside the transaction that advanced the sequence.** The theory was that
committing the send and `last_sent_seq` together made ordering atomic. It cannot: a network call and
a database write are not committable together, and the attempt inverted the failure it was meant to
prevent. A cancelled context or a failed COMMIT *after* `chat.postMessage` returned rolled the claim
back to `pending`, un-incremented `attempts`, and let the job run again — posting a **second root**
into somebody's channel — while erasing the `sending` status that is the only thing §G.5's ambiguity
latch keys on. The one crash window it was designed to close was the window it opened, silently, and
under ADR 0008's update-in-place model a duplicate root reads to everyone in the channel as a second
incident.

It also held a pooled connection and a thread-wide lock for the duration of a call that a
rate-limited destination is entitled to keep waiting for thirty seconds.

## Decision

**Terminal states are decided first.** The gate's evaluation order is: `dead` → `frozen` →
unsequenced → already-resolved → root → predecessor → proceed. A dead thread yields
`recover_thread`, never a snooze; a frozen thread yields `abandon`. Only then do the waiting
conditions get a chance to answer.

> ⚠️ **NARROWED, 2026-08-19 — `frozen` NO LONGER EXISTS (git-bug `e5c060b`, migration
> `00066`).** Not superseded: nothing in this ADR is falsified. The decision it records —
> terminal states first — stands and is why `dead` is still tested before every waiting condition. But `frozen`
> was never reachable: `Freeze` had no production caller, the group-close sweep never called
> it, and no row could hold the state. It has left `threads_state_ck`, the two `ThreadState`
> vocabularies, the `ThreadStore` port and the gate, so the order is now `dead` →
> unsequenced → already-resolved → root → predecessor → proceed, and
> `abandon`/`thread_frozen` is no longer an emittable verdict on
> `oto_thread_order_decisions_total`. **The argument above is left as written**, because it
> is the record of what was decided in August 2026 and half of it is still load-bearing.

Two bounds close the same hole from the other side:

- **A head with no root recovers immediately.** If an item is at `last_sent_seq + 1`, needs a root
  and there is none, every earlier slot is already resolved — so nothing left in the thread can post
  one. That is not a wait, it is a wedge, and it is answered now rather than in fifteen minutes.
- **`MaxWait` = 15 minutes**, measured from the delivery's `created_at`, converts either waiting
  verdict into `recover_thread` (`root_never_landed`, `head_of_line_stalled`). It exists precisely
  because a snooze consumes no attempt: without it nothing bounds a wait at all. Fifteen minutes is
  set against Alertmanager's `repeat_interval` floor — an item still stuck past it will be superseded
  by a fresher notification anyway.

`MaxWait` bounds the wait, not the wedge, so the contract is completed by a **terminal outcome**: a
`recover_thread` that sweeps nothing, and finds no delivery owning the head, **dead-letters the item**
(`status='dead'`, `error_class='permanent'`), advances the head past its slot and appends the event.
Answering a fruitless recovery with another snooze is the original bug in a different costume.

**The send is three phases**, with the provider call between two transactions:

| | |
|---|---|
| TX 1 | lock → read the thread under it → **re-derive the mode** → decide → claim (`status='sending'`, `attempts+1`) → render → persist the bytes → **COMMIT** |
| — | call the provider. No transaction, no pooled connection, no lock held |
| TX 2 | lock again → `markSent` guarded on `status='sending'` → move `last_sent_seq` |

The claim is durable **before** the call, so a crash anywhere afterwards leaves a row that says
*this may have been sent* — which is exactly what §G.5's 120 s lease and the `ambiguous` latch
consume. Ordering survives the shorter lock because ordering was never the lock's job:
`last_sent_seq` does not move until TX 2, so everything behind this item still sees itself out of
turn. **The lock serialises deciding, not sending.**

One further rule falls out of the same failure and is stated here because it is what makes the
ordering decision meaningful: the mode is **re-derived once, before the gate is asked**, and the gate
and the sender share that single value. The gate reading the row's stale mode while the sender
re-derived its own was a second, independent route to the same silent thread.

## Consequences

- Exactly-once is not claimed, and the SPEC now says where the duplicate can appear: a labelled
  `ambiguous` re-send of a `post_root` after a lost claim. Under-delivering a firing alert is worse
  than a visible, marked duplicate.
- A send that lands but cannot be recorded — the lease expired mid-call and another worker took the
  row — records **nothing** and increments `oto_delivery_claim_lost_total`. That counter is an alert,
  not a statistic: every increment is a message in a channel with no `sent` row behind it.
- A thread can now reach a terminal outcome that says "oto gave up" on the alert page. That is a
  visible failure where there used to be an invisible one, and it will be seen by operators.
- The gate's verdict vocabulary is closed and is a metric label
  (`oto_thread_order_decisions_total{action,reason}`), so every wedge class is countable rather than
  inferred from silence.
- TX 2 runs on a non-cancellable context with a 30 s budget. Shutdown is therefore slightly slower
  than a hard stop, deliberately: losing the row that records a delivered message is how oto forgets
  a send and repeats it.

## Alternatives rejected

**Keep the SPEC's order and rely on the retry ceiling.** This was the implicit design, and it does
not work for one reason: a snooze consumes no attempt. Making snoozes consume attempts instead would
bound the wait — and would spend the retry budget of every message queued behind a busy thread on
waiting rather than on sending, killing exactly the deliveries a heavily-used channel is trying
hardest to deliver.

**Sweep the thread on a timer from `lifecycle`.** A periodic reaper advancing stuck heads would also
end the wedge, and it was rejected because it makes liveness a property of a background job rather
than of the dispatch path. The dispatcher already holds the lock and has already read the thread; the
sweep costs a handful of indexed reads there, and a defect in it fails visibly on the next send
instead of quietly at 60 s intervals.

**Skip the stalled slot instead of re-enqueuing its delivery.** Simplest possible recovery, and
wrong: skipping a slot that may still be in flight publishes a reply before the message it replies
to, and skipping a possibly-sent root orphans every reply behind it. Recovery refuses to move past an
unresolved slot and instead *names* the delivery that owns the head, which the worker re-enqueues.

**Two-phase commit, or an outbox for the provider call.** The honest way to make a network call and a
commit atomic, and unavailable: Slack has no idempotency key and no prepare phase, and oto never
reads Slack back (C9). An outbox moves the same ambiguity one hop without removing it.

**Keep the call inside the transaction but shorten the timeout.** Narrows the duplicate-root window
without closing it, and keeps a pooled connection and a thread-wide lock pinned for the length of a
provider call. A smaller version of a failure that should not exist is still that failure.
