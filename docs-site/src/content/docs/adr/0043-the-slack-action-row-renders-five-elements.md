---
title: 0043 — The Slack action row renders five elements, and the fifth is the snooze pair
---
**Status:** Accepted
**Date:** 2026-08-20
**Resolves:** git-bug `78388fb` — the widened row and the SPEC line that was edited to match it.
**Supersedes:** SPEC §H.7's `actions.elements` row as it read before this change — *"v1 renders at
most **4**"*. The **25** in that row is Slack's and is untouched; only oto's own number moves.
**Code:** `maxRowButtons` (`internal/channels/render/slack/root.go`), **3 → 4**. `maxActionItems`
is unchanged at 25 and V8 still enforces it (§H.7, `validate.go`).
**Goldens:** `root_snoozed.golden.json` (4 buttons + overflow) and `root_unsnoozed.golden.json`
(3 buttons + select + overflow) — the two widest rows oto can draw, pinned in bytes.
**Relates to:** [0018](/adr/0018-slack-distribution-model/) (the card is the product surface),
[0013](/adr/0013-alert-first-scope-boundary/) (why a person is never the subject of an action).

> ⛔ **THIS ADR IS LATE, AND ITS LATENESS IS THE FIRST THING IT RECORDS.** The snooze work raised the
> rendered limit in `root.go` and then **edited SPEC §H.7 to match** — four became five in the
> constraint table — with no ADR behind either half. §N is explicit that a bound moves by opening an
> ADR **and** editing the section **in the same commit**, and its closing sentence is the rule that
> was broken verbatim: *"Implementers who find an ambiguity MUST raise it rather than choose."* An
> implementer who finds a constraint in the way and rewrites the constraint has chosen, and has left
> the next reader a number with no argument under it.
>
> ⭐ **THE CHANGE ITSELF IS NOT REVERSED, AND §4 IS WHY.** Every alternative to widening the row is
> foreclosed by a rule oto already holds — §B.8.6's visible pair, the overflow's five-option ceiling,
> §H.8's 3-second `trigger_id` budget — so reverting to four would delete an affordance the SPEC
> requires in order to honour a number the SPEC only ever chose. What was missing was not a different
> outcome; it was the place to argue this one. This document is that place, written after the fact and
> saying so.

## 1. The decision

**oto's own limit on a root card's `actions` block is FIVE elements**, and the row has exactly two
admissible shapes:

| shape | elements | when |
|---|---|---|
| **4 buttons + the links overflow** | 5 | the alert is snoozed — `:bell: Unsnooze` is the fourth button, and it asks no question |
| **3 buttons + the snooze select + the links overflow** | 5 | the alert is not snoozed — `Snooze` carries durations, so it draws as a static select |

`maxRowButtons = 4` counts **buttons**, not elements. A menu-shaped action does not compete with a
button (`actionsBlock`): the view declares "this action asks a question" by carrying `Options`, and
the renderer answers with a static select whose placeholder is the action's own label. The links
overflow is appended after the button loop and is always given a slot. So the ceiling on elements is
five by construction rather than by a second constant, and a terminal card draws fewer — buttons are
dropped when the episode is over, the overflow never is (§H.4, amended).

## 2. Three numbers, and only one of them is Slack's

Conflating them is how this change came to look like a limit being ignored rather than a taste being
re-argued.

| number | whose | what it is |
|---|---|---|
| **25** | **Slack's.** Block Kit's documented maximum for `actions.elements` | a hard cap. A 26th element is `invalid_blocks` and takes the whole delivery with it, which is why V8 refuses the payload rather than trimming it. **Unchanged, and not the subject of this ADR.** |
| **4** | **oto's, until now** | a readability budget written when the card had exactly three verbs — `Acknowledge`, `Runbook`, `Silence` — plus the overflow. It was never derived from anything; it described the card that existed. |
| **5** | **oto's, now** | the same budget re-derived against a card with four verbs. A fifth of what Slack permits. |

**The old four was a description that had hardened into a constraint.** It fit three verbs and one
overflow with nothing left over, so the first verb added after it was written was guaranteed to
collide with it — and did.

## 3. Why the fourth verb had to be VISIBLE

§B.8.6 does not merely permit a snooze control on the card; it specifies what the card shows:

> The `Snooze` action becomes `:bell: Unsnooze` (`oto.unsnooze`). Buttons are never no-ops (S10/§H.1).

A pair that swaps in place is a **state readout as well as a control**: the reader learns that this
alert is snoozed from the same pixels they would use to end it. Move that control anywhere the reader
must first open something and the readout is gone — the card then shows a snoozed alert identically
to a live one, and §B.6's rule that oto's silence must never be indistinguishable from no alert is
broken on the surface where it matters most. The `*Notifications*` field states the fact in words, and
that field is why the card is not *wrong* without the button; it is not why the button is required.

So the requirement is a **visible** pair, and the budget is a **number oto chose**. When a
requirement and a taste collide, the taste moves. That is the whole decision.

## 4. What was refused

**Hiding `Snooze` in the links overflow.** Refused twice over. It is the wrong *kind* of control:
`overflowMenu` is built from `v.Links`, not from `v.Actions`, and every entry there is *a place to
look, never a thing to change* — the options carry `oto.noop.*` action ids that the handler acks with
a 200 and does nothing else with (S9). Putting a mutating verb in that menu would make one entry of
five behave unlike the other four. And there is no room: the overflow is at Slack's documented
**five-option ceiling** already — `Show timeline`, `Open in Prometheus`, `Open in Alertmanager`,
`Rule history`, `Show all labels` — so `Snooze` would have had to evict one of the five places a
reader looks after an episode ends, which is exactly the shrinking §H.4 was amended to stop.

**A modal.** `views.open` needs a `trigger_id`, and §H.8's 3-second rule is not a latency target but
the shape of the handler: the trigger expires in **3 seconds** and is single-use, so any `views.open`
happens **synchronously before the ack**. That makes a modal the one control whose existence depends
on a budget already spent on verifying the signature and writing the 200. It is an acceptable place
for something rare and elaborate; it is the wrong place for the primary affordance of a feature whose
whole point is that a human can quiet an alert quickly. The snooze **durations** still ride a select
rather than a modal for the same reason (§B.8.6's five presets, no indefinite option).

**Dropping an existing verb to stay at four.** There are three and none is spare: `Acknowledge` is
the receipt, `Runbook` is the upstream annotation and the single most-clicked thing on the card, and
`Silence` is the only action that changes the cluster. Removing one to preserve a number oto picked
would be the number deciding the product.

**Widening further — six, or "whatever fits".** Refused, and this is the arm that keeps the limit a
limit. **Five is the smallest number that satisfies §B.8.6**, which is the only reason it is
admissible. The next widening must make the same argument again, in the open, against a requirement
that names the control it needs — not against available capacity. `maxRowButtons` is not a hint that
25 is available.

## 5. Consequences

- **§H.7's `actions.elements` row now reads five** and states both shapes, so the constraint table and
  `root.go` agree. The stale reading is kept in `root.go`'s comment as history rather than deleted,
  because the SPEC said four for as long as oto shipped three verbs and that is the trap.
- **V8 is unchanged and still the enforcement.** oto's five is a renderer convention; Slack's 25 is
  the rule, and the validator is what refuses a payload rather than trusting the renderer's arithmetic.
  A second menu-shaped action would be *skipped past* rather than allowed to push the row to 26
  (`len(elements) >= maxActionItems-1`), which is a `continue` and deliberately not a `break`: how
  many buttons fit must never silently decide whether the snooze affordance exists.
- **Two goldens pin the ceiling in bytes.** `root_snoozed` is the 4-buttons shape and `root_unsnoozed`
  is the 3-buttons-plus-select shape. A change that widens the row cannot pass without rewriting a
  golden, which is where the next argument gets forced into the open.
- **Nothing else in §H.7 moves.** `button.text` stays 75 characters (visually truncating near 30),
  the overflow stays at five options, `section.fields` stays at ten. This ADR widened one number.

## 6. What this ADR does not decide

- **Whether the row should be one row.** Slack wraps a wide `actions` block on narrow clients and oto
  measures nothing about that. Five elements is argued here as the smallest set that carries the
  required affordances, not as a claim about how any client lays them out.
- **Anything about the thread reply cards.** §H.5's replies carry no action row; the snooze pair is a
  root-card fact.
- **Whether a fifth verb is ever admissible.** It is not pre-refused. It is required to arrive with
  its own ADR, its own foreclosed alternatives, and a golden it had to rewrite.
