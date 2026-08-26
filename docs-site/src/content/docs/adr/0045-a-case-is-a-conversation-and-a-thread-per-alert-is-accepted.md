---
title: 0045 — A Case is a conversation, and one Slack thread per alert is the accepted cost
---
**Status:** Accepted — this is a **product ruling**, not an implementation decision. The behaviour
already shipped with git-bug `7570090`; what was missing was the authority to keep it.
**Date:** 2026-08-20
**Resolves:** the open question git-bug `7570090` left behind — *"`N` alerts now open `N` Slack
threads, by construction. This needs a product ruling"* — recorded in SPEC §C.4 and, as an
unwritable acceptance criterion, in §J's AC-14.
**Migrations:** none. `00069_a_conversation_is_a_case.sql` already deleted `alert_groups`,
`alert_cases.group_id` and `notifications.group_id`; this document rules on the consequence, and a
ruling has no DDL.
**Amends:** SPEC §C.4 (the ⛔ block that asked for this ruling is now a ✅ block that records it) and
§J's AC-14 (a deferral becomes a live, testable assertion). **§H.3 is not amended** — *"posted once
per Case conversation — one Case, one root, always"* already says what this ADR authorises, which is
the point: the behaviour was never in doubt, only its licence.
**Relates to:** [0042](/oto/adr/0042-storm-damping-is-removed/) — 0042 removed the *delivery-time* damper
and this document rules on what the removal of the *structural* one costs. **0042 is not
superseded and is not weakened**: its §3 principle — oto may be quiet for a reason outside its own
judgement and never for its own opinion — is the reason a per-alert conversation is the only
honest default here. [0038](/oto/adr/0038-the-group-key-is-derived-from-the-alerts-own-labels/) — the ADR
that made the split key derived from the alert's own labels. Its amendment stands; the **entity**
it keyed is gone, so the key it specifies now identifies nothing and is read only by
`tools/groupreplay` (SPEC §C.4). [0040](/oto/adr/0040-a-case-is-open-or-closed-and-never-reopened/) and
[0041](/oto/adr/0041-the-alert-case-allocation-and-the-rule-that-decides-it/) — what a Case is, and the
rule that allocates one.

> ⚠️ **THIS ADR RATIFIES A BEHAVIOUR THAT WAS ALREADY LIVE, AND SAYS SO FIRST.** `7570090` deleted
> the group and shipped the fan-out; two places in the SPEC then carried the consequence as an
> **unruled** product risk for two days. That is the right way round — a consequence nobody has
> authorised belongs on the record as unauthorised — but it is not a resting state, because a SPEC
> that asks its reader for a decision cannot be tested against. This document is the decision. It
> adds no behaviour, deletes no code, and its entire content is authority.

## 1. The decision

**One Case is one conversation, and a conversation is one Slack root card.** A burst of `N` firing
alerts therefore opens `N` conversations and posts `N` root cards. **This is accepted.**

Three clauses follow from it and are equally binding:

1. **oto withholds nothing.** Nothing in the delivery path declines, delays, downgrades or collapses a
   notification because the channel is busy. A burst produces one notification per triggering change,
   exactly as a trickle does (ADR 0042 §1, unchanged).
2. **The digest is the only collapse mechanism, and it is opt-in.** `notification_policies` carries
   `digest_window_s` and a floor; at the top of a window the evaluator counts the Cases that opened
   inside it and says so once (SPEC §H). An org that configures none gets one card per Case. Nothing
   is opted in on an operator's behalf.
3. **There is no structural replacement for the group, and none is admissible on today's terms.** A
   mechanism that decided which facts share a message *for* the operator is precisely what
   `alert_groups` was, and its deletion was ruled on because the row held no fact anybody supplied.

## 2. Why the collapse could not simply be kept

The group looks, from a distance, like a feature that was thrown away with the entity. It was not
separable from it.

`alert_groups` was one *generation of a notification grouping*, keyed by
`(org, cluster, alertname, namespace-or-∅)` off the alert's own labels (ADR 0038), and it owned exactly
one Slack thread. **Deciding which facts share a message is the definition of a NotificationPolicy** —
an operator-authored row — and the group row held no fact an operator supplied and none upstream sent.
It held a decision oto made, identically for every org, forever. Keeping the collapse would have meant
keeping oto's opinion about which of somebody else's signals belong in one conversation, expressed as
four hardcoded axes.

That is the same shape ADR 0042 §3 refuses for dampers, and the argument transfers without alteration:
**a threshold whose author is the operator is their request; a threshold whose author is ours is our
opinion.** A configurable delivery-time collapse key was in fact designed, built, and **reverted**
(migration `00065`) under the ruling that a conversation holds exactly one Case;
`notification_policies.group_by` existed for one release and is gone. So the choice ruled on here is
not "collapse or not" — it is "oto's collapse or the operator's", and the operator's is the digest.

## 3. What this costs, stated as a number

**A 300-alert burst is 300 Slack root cards. A 500-alert burst is 500.** The relationship is `N → N`
for every `N`, with no threshold anywhere in the code, and this is the cost the ruling accepts.

Two figures appear in the record for one phenomenon and neither is wrong: §J's AC-14 states it at
**300** because that is the burst that criterion has always described, and SPEC §C.4 with `test/load`
states it at **500** because 500 is the load driver's batch. Nothing reads either number.

What is *not* degraded, and is still asserted by tests:

- **No alert is lost.** Ingest accepts the batch inside the §G.2 budget, chunked at 500 per
  transaction, and every rejection is a content rejection with a reason and a metric (§C.9.1).
- **No thread wedges.** The ordering gate walks to the head of every one of the `N` threads, and gap
  recovery bounds the pathological case (§G.7.3).
- **One root per thread.** `test/load` still asserts `SlackRoots == Threads` — oto never opens a
  conversation it does not remember, nor forgets one it opened. That assertion survives; the ones
  below did not.

Slack's own rate limits are the real operational ceiling on the tail of a very large burst, and they
are the destination's constraint rather than a judgement of oto's. Nothing in this ruling licenses
oto to pre-empt them by going quiet.

## 4. What was deleted rather than retargeted, and why that is the honest outcome

This is the part of the ruling with a cost that is not operator-visible, and it is recorded here
because a deleted gate is easy to mistake later for a gate nobody thought to write.

`test/load` lost **three assertions**, each with a tombstone at its old site naming the number it used
to demand:

| Deleted | What it demanded | Why it could not be retargeted |
|---|---|---|
| The **`O(groups)` rollup bound** (`test/load/driver_test.go`, the `report` block and property 3) | a 500-alert batch performs a number of rollups proportional to the **groups** it touched, not to the alerts — the suite's headline claim | A conversation holds one Case and a Case belongs to one Alert, so "rollups per alert" is **1 by definition**. Retargeting at Cases produces a bound that cannot fail: a green gate over nothing. |
| The **`chatter ≤ alerts/10`** ratio (`if r.AlertsAccepted >= 200 && r.SlackCalls*10 > r.AlertsAccepted`) | oto's own Slack calls stay an order of magnitude below the accepted alert count | Its own comment named grouping as the whole mechanism. One Case per Alert puts a 500-alert burst at roughly **100 %** — the exact figure the old comment used to describe *"a receiver that posts per alert"*. Any ratio today's behaviour satisfies would be a bound fitted to the measurement, which is the opposite of a budget. |
| The **`SlackRoots != 1`** shape — the storm-era claim that a burst costs *one* root | one root card for a whole burst | It has no subject. There is no entity for `N` alerts to share a root through. |

**The ratio is still measured and still published** — `slack_calls` against `alerts_accepted` in
`test/load`'s report and `RESULTS.md` — so the number does not leave the record. Only the *claim*
about it does. `test/load/doc.go` says in as many words that the package now proves durability and
ordering and **does not speak to volume**.

## 5. Consequences

- **A 300-alert burst is 300 Slack root cards, and that is now a specified outcome rather than a risk
  under review.** §J's AC-14 asserts it. The old inverted criterion — *"300 alerts arriving for one
  group produce one root card and one Slack thread"* — is kept there as a historical note, because a
  reader who remembers the promise needs to find where it went.
- **The load suite has no fan-out bound, and that is a real gap.** Whether a per-Case fan-out needs a
  ceiling of its own — and what that ceiling would be a budget *for*, given that oto may not withhold
  — is not answered by this ruling. It is left open deliberately: inventing a number now would
  produce exactly the measurement-fitted bound §4 refuses. The honest state is a documented gap with a
  published measurement beside it.
- **The digest carries the entire weight of noise reduction, so its ergonomics are now load-bearing
  product surface.** Before this ruling it was one collapse mechanism among two; it is the only one.
  A digest that is hard to discover or hard to configure is, after this ADR, a product defect and not
  a nice-to-have.
- **`ComputeGroupKey` survives for measurement only** (SPEC §C.4). Its one caller is
  `tools/groupreplay`, which answers *"would these axes have over-split or over-merged this org's real
  payloads"* — carried as an explicit `//oto:reachable-ok`. It writes no row and keys no thread, and
  its continued existence is not a route back.
- **`AlertGroup` stays a retired noun.** The vocabulary ban (CONTEXT.md, SPEC §A.1) is unaffected, and
  a value still spelling `alert_group` is **rejected, not canonicalised**. This ruling accepts the
  *cost* of the deletion; it does not soften the deletion.

## 6. What this ADR does not decide

- **What bounds a per-Case fan-out.** §5, explicitly. It needs a number nobody can derive from the
  current record.
- **Whether oto regains an automatic damper.** It does not. ADR 0042 §3 and ADR 0044 §3 both refuse a
  damper whose threshold is oto's, and neither needs re-litigating; the fan-out accepted here is the
  *consequence* of that refusal, not a reason to revisit it.
- **Whether a future operator-authored mechanism may collapse conversations.** Unaddressed rather than
  forbidden. The digest is one such mechanism today. Anything new would have to be a policy column an
  operator wrote, could read back and could clear with one `PATCH` — the ADR 0044 §3 test — and would
  need its own ADR.
- **Correlation.** **Deferred, not decided here** (ADR 0013, SCOPE-BOUNDARY — where `incident` is the
  word that is permanently banned, not `correlation`). Whether oto ever relates many alerts into one
  narrative is a separate question with its own boundary; the `N → N` fan-out ruled on here is what
  today's *absence* of correlation looks like from the Slack channel, and it must not be read as an
  argument either for or against acquiring it.
