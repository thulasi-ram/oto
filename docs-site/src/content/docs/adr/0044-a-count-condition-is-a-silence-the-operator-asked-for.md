---
title: 0044 — A count condition is a silence the operator asked for, and `below_threshold` is how oto says so
---
**Status:** Accepted — **in full, §5's precedence rank included.** The rank was recorded as an
implementation decision pending ratification when this ADR was written; the owner ratified it on
2026-08-20 and it is now a ruling like the rest of this document.
**Date:** 2026-08-20
**Resolves:** git-bug `7570090`, stage 6's remaining half — the replacement that ticket names for
hardcoded flap detection.
**Migrations:** `00072_a_policy_binds_a_subject_and_counts_it.sql` — `notification_policies`
gains `subject_kinds`, `count_min`, `count_window_s` and four CHECKs; and
`00073_a_floor_can_record_its_own_silence.sql` — `notifications_suppmap_ck` widens from six values
to seven, plus `notif_policy_idx`.
**Amends:** SPEC §B.8.2 (the precedence chain, six values → seven), §D.8 (the two tables' DDL),
§H.6 and the `NotificationSuppressedReason` schema in `api/openapi/openapi.yaml`.
**Relates to:** [0042](/adr/0042-storm-damping-is-removed/) — this is the ADR a reader of 0042 needs in
order to be told why a new damper is admissible at all. 0042 is **not** superseded: §3's ruling that
oto may not withhold on its own judgement is the test this axis had to pass, and §4's "nothing
replaces it *at the notification layer*" is unchanged, because what arrives here is a **policy
column**, not a detector.

> ⚠️ **§5'S RANK IS AN IMPLEMENTER'S CHOICE AND IS LABELLED AS ONE THROUGHOUT.** `below_threshold`
> ranks between `throttled` and `verbosity` in the shipped chain. The argument for that position —
> ceiling before floor — is stated in §5 because a chain with no argument is worse than a chain with a
> reviewable one, but **it is reasoning produced by this change and not a ruling recovered from
> anywhere.** No SPEC clause, no earlier ADR and no ticket ranks a ceiling against a floor; the
> question was not asked before this axis existed. The value's ADMISSIBILITY (§2–§4) is
> ticket-authorised and is the decision this document records; its POSITION is a choice made in order
> to ship, offered here to be overturned cheaply. Overturning it moves one slice literal
> (`suppressorOrder`), one SPEC line and one contract description, and breaks nothing stored.

## 1. The decision

A notification policy may carry a **count condition**: *stay silent until at least `count_min` of
these have happened inside `count_window_s`*. When the count falls short, oto **records the
notification and sends nothing**, with `suppressed_reason = 'below_threshold'`.

The vocabulary therefore holds **seven** suppression reasons, not six:

```
channel_disabled → no_policy → snoozed → throttled → below_threshold → verbosity → duplicate_render
```

What is counted is fixed by the schema rather than by a comment: the **distinct subjects of the one
`subject_kinds` value the policy binds**, for that policy, over the sliding window
`[now - count_window_s, now]`, **including the fact being evaluated** and **including rows this same
floor already suppressed**. So five re-fires of one Case count once, and a policy holding its tongue
still climbs towards its own threshold.

## 2. It is `throttled`'s dual, and that is what makes it cheap

The same two policy fields, read with the opposite sense:

| | says | reads |
|---|---|---|
| `throttled` | "you have already been told this many times in this window" | a **ceiling** |
| `below_threshold` | "this has not happened enough times in this window yet" | a **floor** |

A policy may carry both, and a single evaluation may be refused by either. Nothing new is measured
and nothing new is detected: the evaluator counts rows in `notifications`, which is why `00073`'s only
structural addition is an index — `notif_policy_idx ON notifications (org_id, policy_id, created_at
DESC)` — the same shape `notif_conversation_idx` already has for the throttle. `notifications.policy_id`
has carried an FK since `00011` and no index; a foreign key creates none on the referencing side.

## 3. Why this is admissible when `flapping` and `storm` were not

This is the test the value had to pass, and it is the only reason this ADR exists rather than a line
in a migration header.

ADR 0042 deleted `storm`, and ADR 0041 Amendment 1 deleted the `flapping` damper with it
(migration `00059` narrowed `notifications_suppmap_ck` to six for both), on one principle: **oto may be
quiet for a reason outside its own judgement — nowhere to send, a human asking for less, a provider
rate-limiting, nothing changed — and its own opinion that a real firing was not worth mentioning is
not on that list.** `flapping` compared against `DefaultFlapThreshold = 5` over
`DefaultFlapWindow = 7200 s`: constants welded into Go, which no operator could see, read back or
change. That is oto holding an opinion about somebody else's signal.

`below_threshold` produces a silence with **the same shape** — *this keeps happening, say nothing
yet* — and the only thing separating it from the value the codebase spent two ADRs deleting is
**where the number comes from**. It comes from `notification_policies.count_min` over
`count_window_s`: two columns an operator **wrote**, can **read back** through
`GET /api/v1/notification-policies`, and can **clear with one `PATCH`**. The silence it produces is
the one they asked for, by name, on a policy they can see.

**A damper whose threshold is the operator's is their request. A damper whose threshold is ours is our
opinion.** That is the line, it is the line ADR 0042 drew, and this axis is on the permitted side of
it. Every value in the seven is still either the **absence of a destination** (`channel_disabled`,
`no_policy`), **a human asking for less** (`snoozed`, `verbosity`, `throttled`, `below_threshold`), or
**nothing to say** (`duplicate_render`). Not one is a judgement, and nothing may add one back.

## 4. The count needs a unit, which is why the binding arrives in the same migration

`00072` adds `subject_kinds` and the count together rather than in two migrations, because a count is
a number of *somethings* and the something has to be named.

- `policies_count_subject_ck CHECK (count_min IS NULL OR cardinality(subject_kinds) = 1)`. A count
  condition on an **unrestricted** binding has no unit; on a **two-kind** binding it has two, and
  summing an alert-subject fact (an identity, true across every firing it ever had) with a
  case-subject fact (one firing episode) produces a number that is about nothing.
- `policies_count_pair_ck CHECK ((count_min IS NULL) = (count_window_s IS NULL))` — **symmetric**,
  unlike `policies_digest_pair_ck`. A digest's window alone is a complete instruction; neither half of
  a count condition means anything alone. A threshold over unbounded history is not evaluable, and a
  window with no threshold counts facts and compares the number against nothing. Both would silently
  mute or silently un-mute a channel.
- `count_min` is **2..10000**. Two, because the fact being evaluated is itself inside the window, so a
  threshold of one clears unconditionally and states no condition — the same argument that keeps zero
  out of `policies_digest_floor_ck`.
- `count_window_s` is **60..86400** — the **throttle's** window bound, not the digest's, and with no
  divisor rule, because this window is not tiled against the UTC day.
- `subject_kinds` is `NOT NULL DEFAULT '{}'` and **empty means every kind**, which is what every row
  written before `00072` says and is today's behaviour exactly. The direction matters: the failure
  mode on this path is a `no_policy` **suppression**, not an error anybody sees.

## 5. The rank — ratified

`below_threshold` sits **directly below `throttled` and above `verbosity`**. The reasoning, offered as
reasoning:

- The two are the same two policy columns read with opposite senses, so they belong **adjacent**, and
  both belong above `verbosity`, which is a property of a **destination** rather than of the policy.
- The **ceiling ranks first of the two** because a spent cap is the **active** fact — oto has been
  speaking about this conversation and has stopped, against a number the operator has already been hit
  by — whereas an unmet floor is the ordinary **resting** state of every policy that carries one: for
  most of its window a count condition is unmet by design. A resting state that outranked an active
  damper would mask it on every policy carrying both, which is the same argument that already puts
  `verbosity` below `throttled`.

✅ **RATIFIED BY THE OWNER, 2026-08-20.** The history is kept deliberately, because it is the part a
future reader needs: "ceiling before floor" appeared in no SPEC clause and in no earlier ADR when this
change shipped. It was this change's answer to a question nothing had asked, and it was recorded as a
proposal — here, in `suppression.go`'s `suppressorOrder` comment, in SPEC §B.8.2 and in migration
`00073` — precisely so it could not be mistaken for a ruling recovered from the design record. The
owner then made it one. **So the rank above is now authority, and this section is where it comes
from** — not SPEC §B.8.2, which records it, and not `suppressorOrder`, which implements it.

Reversing it remains cheap by construction, and that stays true after ratification: one slice literal,
one SPEC line, one contract description and this section. **Nothing stored depends on the order** —
the chain decides *which* reason a suppressed row records at write time, and a row already written
keeps the reason it was written with. A reversal is therefore a new ADR superseding this section, not
a migration.

## 6. Consequences

- **`notifications_suppmap_ck` admits seven values.** `00073`'s `ADD CONSTRAINT` cannot fail on
  existing data — it only widens — and its **`Down` refuses** if any row records `below_threshold`,
  with the query to inspect them. Deleting a recorded suppression deletes the only evidence oto chose
  silence, so the rollback makes that a deliberate act rather than a way to get the migration to run.
- **A new operator-facing axis is a new thing to explain.** The reason appears in the API contract with
  its own prose, in SPEC §B.8.2's chain, and in §D.8's column comment and CHECK.
  §N.1/§N.4's three-places rule applies to the pair `count_min` /
  `count_window_s` exactly as it applies to any bound: the DTO `validate` tag, the domain constructor
  and the DDL CHECK, or `TestValidatorMatchesDDL` fails.
- **It is not `flapping` under a new name and must not be described as one.** The two produce the same
  shape of quiet by a different author, and the operator-facing copy says whose number it is.
- **`subject_kinds` overlaps `reasons`, and the overlap is conceded.** `Reason.Subject()` is total, so
  as a *filter* the binding is derivable from a hand-narrowed reason list. It earns its column as the
  count's **unit**, and as a declaration an operator can read without knowing the Reason-to-subject map
  by heart.

## 7. What this ADR does not decide

- **The rank.** §5, explicitly.
- **Whether a count condition should ever be able to schedule.** It may not, ever
  (SCOPE-BOUNDARY §4.8). `subject_kinds` is an altitude, not a calendar.
- **Whether oto ever regains an automatic damper.** It does not. This axis is admissible *because* the
  threshold is the operator's; a future value whose threshold is oto's is refused by ADR 0042 §3 and by
  this document's §3, and neither needs re-litigating.
