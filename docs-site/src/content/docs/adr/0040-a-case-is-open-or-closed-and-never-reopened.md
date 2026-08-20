---
title: ADR 0040 — A Case is `open` or `closed`, and it is never reopened
---
> ⛔ **SUPERSEDED IN PART — §6'S LAST BULLET RULED THAT `refire_grace` STAYS, AND THE SETTING IS NOW
> DELETED.** `refire_grace_s` and `group_close_delay_s` no longer exist: git-bug `7287b28`, migration
> `00071`, which strips both keys from `orgs.settings` and restates the column comment as the seven
> keys `settingsJSON` actually reads. **§1 through §5, §7 and §8 are untouched**, and so is the whole
> of §6 except that one bullet: a Case is still strictly terminal, T8 is still deleted, and
> `Decision.DropsAck` is still the only road out of a closed episode.
>
> ⭐ **THE RULING WAS REVERSED BY ITS OWN PREMISE, NOT BY A CHANGE OF TASTE.** The bullet argued the
> key stays *because it still constrains two numbers outside the lifecycle layer*. Both of those
> numbers have since gone. `group_close_delay_s` timed the close of an `alert_groups` **generation**,
> and git-bug `7570090` / migration `00069` deleted the entity, so the delay had nothing left to
> close. `MinRefireGraceSeconds = 2 × DedupTTL` was a comment and a test name, never a runtime read,
> and the bound it justified is restated at the §C.5 replay window itself. ⚠️ The "pin" between the
> two was never enforced anyway — two independent `1200 * time.Second` literals, with the only check
> comparing the two DEFAULTS, so two operator-written settings could contradict each other freely.
> Deleting **both** is what makes that moot; deleting one would have left the trap armed with its
> tripwire removed.
>
> ⚠️ **THE STANDING RULE THE BULLET INVOKED IS UNCHANGED, AND IT IS NOT WHAT DECIDED THIS.** Deleting
> a settings key is still a contract change of its own. oto is unreleased, `git tag` lists nothing,
> and the owner's ruling is **delete, do not retire** — a knob that clamps, validates and reports an
> origin while changing no outcome is a vocabulary entry the next person has to rule out. See
> SPEC §B.3 (the paragraph that used to quote this bullet and now reverses it) and migration `00071`.

- **Status**: Accepted · **superseded in part** by git-bug `7287b28` / migration `00071` — see the
  banner above and §6's last bullet.
- **Date**: 2026-08-18
- **Supersedes in part**: ADR 0036 (`AlertOccurrence` becomes `AlertCase`) — the entity is unchanged, its `state` column is not.
- **Reverses**: the "an acknowledgement survives a re-fire inside `refire_grace`" intention behind SPEC §B.3 transition **T8**. See §6, which exists so a future reader does not restore it.
- **Migration**: `00054_the_case_is_open_or_closed.sql`

## 1. The decision

`alert_cases.state` holds **`open`** or **`closed`** and nothing else.

The four §B.2 values — `firing | suppressed | resolved | expired` — describe **the Alert**.
They stay on `alerts.state`, unchanged, and on every alert-shaped object in the API.

A Case is **strictly terminal**: it opens once, it closes once, and nothing reopens it.
A re-fire opens the **next** episode at `seq + 1`, **unacknowledged**, always. `reopen_count`
and `reopen_of` are dropped, transition **T8** is deleted from the table, and the
`case.reopened` event type is **retired** rather than removed.

## 2. Why the column was wrong, rather than merely wide

An AlertCase is one ephemeral firing episode of one Alert. The Alert is the identity —
created on first sight, never deleted, outliving every one of its firings. Once you hold that
distinction, three of the four values stop being facts an episode is entitled to assert:

| value | whose fact it is |
|---|---|
| `suppressed` | **the Alert's.** A silence mutes a label set, not one firing of it. Migration 00017 already said `state` MUST NEVER carry a snooze; this is the same argument with a different suppressor. |
| `resolved` / `expired` | **a claim about WHY the episode ended**, which `resolve_reason` has recorded since 00007. The state column was a second spelling of one fact, and `case_resolve_map_ck` existed purely to stop the two spellings disagreeing. |
| `firing` | what is left, and it is just "not ended" — which `ended_at` and `case_terminal_ended` were already saying. |

So the column said four things, three of which belonged elsewhere and one of which was a
restatement of the timestamp beside it. Narrowing it is subtraction, not loss.

## 3. Nothing is lost, and that is checkable

The four-way reading is **derived**, and the derivation is total:

```
state='open'   AND suppression_reason IS NULL      ->  firing
state='open'   AND suppression_reason IS NOT NULL  ->  suppressed
state='closed' AND resolve_reason = 'upstream'     ->  resolved
state='closed' AND resolve_reason = 'timeout'      ->  expired
```

`case_resolve_ck` makes `resolve_reason` present exactly when closed and `case_resreason_ck`
bounds it to those two values, so the bottom half is exhaustive. The new `case_suppress_ck`
keeps `suppression_reason` off a closed row, so the top half is. `Case.AlertState()` is that
table in Go; `Case.check()` is what makes it total.

**`resolve_reason` needed no widening.** `upstream` *is* resolved and `timeout` *is* expired —
the two spellings were always one-to-one, which is precisely why the map constraint could
exist. What changed is that the distinction now has exactly one home instead of two, and the
constraint that guaranteed a closed episode *has* a reason became load-bearing rather than
redundant: without it, a closed Case could say nothing at all about how it ended.

This is also what makes migration 00054's `Down` **lossless for `state`**. All four values are
rebuilt from the rows: `resolve_reason` names resolved apart from expired on the closed half,
and `suppression_reason` names suppressed apart from firing on the open half.

## 4. In SQL, the open half asks the ALERT — deliberately, and at a cost of nothing

Go derives `suppressed` from the Case's own `suppression_reason`, because `Case.check()`
re-proves the invariant on every construction. **SQL does not**, and the asymmetry is on purpose.

After 00054 nothing *CHECKS* `suppression_reason` against a state — the biconditional had a
`state = 'suppressed'` side, and that side no longer exists. A query that read the column as
"this is suppressed" would be trusting an invariant the schema stopped enforcing. `alerts.state`
is a first-class CHECKed enum, so every aggregate that needs `firing` apart from `suppressed`
joins the Alert and reads it there.

The join is safe on exactly the rows that ask for it: `case_one_open_idx` is `UNIQUE (alert_id)
WHERE ended_at IS NULL`, so an **open** case *is* its Alert's `current_case_id` and `a.state`
*is* that episode's state. The `o.state = 'open'` half of the predicate is not decoration —
without it, a re-fired alert's old closed episodes would be counted as firing, because
`alerts.state` is a projection of the *current* episode.

It costs nothing. `memberRollupSQL` — the group card's "12 alerts, 3 firing, 9 resolved" — has
joined `alerts` since it was written, for `max(a.severity)`. The derivation reads one more
column off a row the plan already fetched by primary key. Measured plans, before and after, are
identical in shape: `case_group_idx` drives, `alerts_pkey` probes.

## 5. `?open=` is gone, and `?state=` inherited its spelling

`GET /api/v1/cases` shipped both a four-valued `state` filter and a separate `open` boolean.
While `state` had four values those were genuinely different questions — `open` asked about
liveness, `state` narrowed within it, and only `open` produced a predicate the planner could
match a partial index against. With two values they are one axis, so `open` is removed.

⚠️ **The planner still cannot read `state`, and that was measured rather than assumed.** A
partial index is matched against the query's own restriction clauses and *never* against the
table's CHECK constraints, so `case_terminal_ended` does not help it. On Postgres 17, over 200k
rows, asking for the unacknowledged open page:

| predicate | plan |
|---|---|
| `state = 'open'` | Index Scan on the **full** `(org_id, started_at, id)` index; `state` and `ack_state` both heap filters |
| `ended_at IS NULL` | **Index Only Scan** on the partial ack index, both equalities as Index Cond |
| both | the partial index, plus a redundant `state` filter that can never fail — and a selectivity estimate multiplied twice for one restriction (211 rows against 5299) |

So `alerts/repository.ListCases` emits the state axis **as** `ended_at IS NULL` / `IS NOT NULL`:
the one spelling that is both correct and visible. The third row is why it is not emitted
alongside — a doubly-counted estimate is a worse input to every later join decision, for no rows
saved.

## 6. ⛔ The reversal: an acknowledgement does not survive a re-fire

**This section exists because the tree used to say the opposite, and a future agent reading an
older handoff must not "fix" this back.**

SPEC §B.3 had two rows out of a terminal state under `observe firing`:

- **T8**, taken when the re-fire landed within `refire_grace`: it CLEARED `ended_at` on the
  closed episode, let it run again, and kept the acknowledgement that had been taken on it.
- **T7**, taken outside the window: it opened a new episode, unacknowledged.

T8 is deleted. Every re-fire is T7.

The argument is the one 00049 made when it dropped `alerts.ack_state`: **an acknowledgement is a
receipt for one firing**, and the second firing is not the one that was signed for. A gap in the
firing is exactly the event a receipt should not cross — if the alert stopped and started again,
somebody should look at it again. T8 made "how long was the gap?" decide whether a human's
attention was still assumed, which put a *clock* in charge of a claim about a *person*.

It also made a Case non-terminal, and a non-terminal Case is a much larger cost than it looks:
`ended_at` could go back to NULL, so `case_one_open_idx` could be re-entered by a row that had
left it; the reaper and ingest could race over an episode that was closed a moment ago; and
every query that treated `closed` as final was quietly wrong for one window's width.

Consequences, all deliberate:

- `reopen_count` and `reopen_of` are dropped. `seq` is 1-based and gapless, so the episode a new
  one succeeds is the row at `seq - 1`; `reopen_of` was a second spelling of that.
- `Decision.DropsAck` is now the only road out of a closed episode, so no acknowledgement
  survives any re-fire, however quickly it arrives.
- `refire_grace` no longer decides a transition — ⛔ **AND THE SETTING IS DELETED. THIS BULLET RULED
  THE OTHER WAY AND IS SUPERSEDED** (git-bug `7287b28`, migration `00071`). It read: *"the setting
  stays — under its own name, in `orgs.settings`, with its bounds unchanged. That is the decision, not
  a deferral … Renaming, re-homing and removal are all refused."* The superseded wording is kept here
  because the argument was sound on its premises and it is the premises that failed.

  The bullet rested on the key still constraining **two numbers outside the lifecycle layer**, each
  held by its own test: a FLOOR of `2 × ingestion/domain.DedupTTL` (`MinRefireGraceSeconds = 600`,
  derived from the §C.5 replay window, `TestTheReplayWindowIsStrictlyInsideRefireGrace`), and
  `DefaultGroupCloseDelay` pinned **at or above** `DefaultRefireGrace`, equal at 1200 s
  (`TestGroupCloseDelayDoesNotDefeatTheRefireGrace`). Both are gone. `group_close_delay_s` timed the
  close of an `alert_groups` **generation**, and migration `00069` deleted the entity; the floor was a
  comment and a test name rather than a runtime read, and the bound it justified is restated at the
  replay window itself. ⚠️ The pin was never enforced in the first place — two independent
  `1200 * time.Second` literals, with the only check comparing the two DEFAULTS, so two
  operator-written settings could contradict each other freely.

  ⭐ **THE PART OF THE BULLET THAT STANDS** is where the question belongs: *how much of a gap should be
  tolerated before a re-fire counts as a separate episode* is a **case formation** question, not a
  lifecycle-boundary one, so a future knob for it is a **new key with new semantics** and never this
  one wearing a new label. And the standing rule it invoked is unchanged: deleting a settings key is a
  contract change of its own. What decided this was the owner's **delete, do not retire** ruling on an
  unreleased product — a knob that clamps, validates and reports an origin while changing no outcome is
  a ghost reference at launch. ⚠️ **There is no operator copy for `refire_grace` any more**; the
  sentence this bullet used to prescribe described it as the number `group_close_delay` is tied to, and
  both keys left together.

## 7. `case.reopened` is retired, not deleted

`alert_events` is append-only, monthly-partitioned and retained thirteen months, and rows
spelling `case.reopened` — and the pre-ADR-0036 `occurrence.reopened` — already exist. Removing
the value from the closed enum would make `NewEventType` **reject history**: a timeline that
errors rather than renders.

So it follows 00051's `group.member_joined` pattern exactly: the constant stays, it stays
parseable, it stays canonicalising from the legacy spelling, it stays in `AllEventTypes()` and
therefore in `components.schemas.AlertEventType`, it stays in all three hand-written SQL `type
IN (…)` predicates in **both** spellings — and **nothing may append it**.

⚠️ **It needed a second refusal, and that is the one difference from 00051.** The two member
events were emitted from *another module*, so `AppendTimelineEvent` was the only door and one
check there closed it. `case.reopened` was minted by `alerts`' own transition table and reached
the column through `alerts/service.appendEvents`, which the seam explicitly does not cover. The
refusal is therefore made at **both** writers, and the transition rows that constructed the
value are gone as well.

## 8. What was deliberately NOT decided

> **ANSWERED BY ADR 0041.** Suppression is an **axis**, not a state: `alerts.state` narrowed to
> `firing | resolved | expired` and gained `suppression_reason` / `suppressed_by` beside it
> (migration 00055). The Case's copies **stay**, as this section says, and the ADR states why they
> are not a duplicate — the Alert's pair is the live axis, the Case's is one firing's record.
> Two claims below are superseded: `Case.AlertState()` no longer reads `suppression_reason`, so
> §3's derivation table has a one-line open half, and `Case.SuppressedBy()` gates on the reason
> directly rather than on `AlertState`.

`alert_cases.suppression_reason` and `suppressed_by` **stay on the Case**, as a per-firing record
of which upstream object muted that firing. Whether suppression belongs to the Alert instead is
a real question and this ADR does not answer it. The migration therefore makes the minimal
change: the one CHECK that named `state = 'suppressed'` becomes the half of itself the schema can
still state — a **closed** case cannot be suppressed — and the columns keep their names, their
types and their rows.

What now enforces their consistency, precisely:

- **`case_suppress_ck`** (DDL): `suppression_reason IS NULL OR state = 'open'`.
- **`case_supreason_ck`** (DDL, unchanged): the value is one of the four Alertmanager reasons.
- **`case_suppby_ck`** (DDL, unchanged): `suppressed_by` is a JSON object.
- **`Case.check()`** (Go): the same one-directional rule, re-proved on every construction and
  every transition.
- **`Case.SuppressedBy()`** (Go): returns the zero value unless a `suppression_reason` is present,
  so witnesses left on a row by anything else are invisible to a renderer. (It asked `AlertState()`
  when this ADR was written; ADR 0041 took suppression out of that reading, so it now asks the
  field. Same question, one fewer hop.)

There is **no longer a constraint that an open, suppressed episode HAS a reason** — and there
cannot be one, because "suppressed" is now the *reading of* having one. That direction became a
tautology rather than a check, and §4 is why no SQL relies on it.
