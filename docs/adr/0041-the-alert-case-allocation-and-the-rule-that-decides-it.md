# ADR 0041 — The Alert/Case allocation, and the one rule that decides it

- **Status**: Accepted
- **Date**: 2026-08-18
- **Answers**: ADR 0040 §8, which named "does suppression belong to the Alert?" a real question and
  deliberately did not answer it. It does. §4 below.
- **Supersedes in part**: ADR 0040 §3 — the derivation table's open half. `Case.AlertState()` no
  longer reads `suppression_reason`, because after this ADR there is no `suppressed` state to
  derive. The closed half is unchanged and still total.
- **Migration**: `00055_suppression_is_an_axis.sql`
- **Gate**: `test/scope/allocation_test.go`

## 1. The rule

Two tables hold what oto knows about a signal, and every new column lands on one of them. The
question that decides which is one sentence:

> **Is this fact true of the signal across ALL its firings, or only of this ONE firing?**

- **Across all of them → `alerts`.** The Alert is the *identity* of a label set. It is created the
  first time that label set is seen, it is never deleted, and it outlives every episode.
- **Only this one → `alert_cases`.** A Case is ONE firing episode: it opens once, it closes once,
  and the next firing is a different row (ADR 0040).

For a **verb**, the same question has a second phrasing that is easier to answer out loud:

> **Does it still mean something after this firing ends?**

- **Snooze → Alert.** "Be quiet about *this signal* until 6pm" survives the outage ending and the
  next one starting. It was never about an episode.
- **Ack → Case.** "I have seen *this*" is a receipt for one firing. The next firing is not the one
  that was signed for, so it starts unacknowledged (ADR 0040 §6, migration 00049).

⛔ **"Both tables" is not an answer, and neither is "whichever is convenient to query."** The
`alerts` row already carries a *projection* of the current episode — `state`, `current_case_id` —
written by one statement, `setProjectionBatchSQL`. A column may appear on both tables only when
the Alert's copy is that projection and the Case's copy is the episode's own record, and the ADR
that adds it must say which is which. §5 does exactly that for suppression, and it is the only
place in the schema where the pattern is licensed.

## 2. The rule is not a taste, it is a bug filter

Three defects in this tree were the same mistake, and the rule catches all three before they are
written:

| defect | what was allocated wrong | what it cost |
|---|---|---|
| `alerts.ack_state` (dropped, 00049) | a receipt for ONE firing, stored on the identity | an acknowledgement silently carried across a re-fire, so an alert nobody had looked at read as seen |
| `alert_cases.state` holding `suppressed` (narrowed, 00054) | a statement about the WORLD, stored as a phase of an episode's life | three of four values on that column belonged elsewhere; a `case_resolve_map_ck` existed only to stop two spellings of one fact disagreeing |
| `alerts.state` holding `suppressed` (this ADR) | an ORTHOGONAL AXIS, stored in the slot `firing` needed | every `state = 'firing'` reader under-counted by exactly the alerts somebody had silenced |

`internal/alerts/domain/snooze.go:25-32` is the argument in its clearest form and it predates all
three: an alert can be firing AND acked AND snoozed at once, and all three are displayed, so none
of them may be a value of a column that holds one of the others.

## 3. The allocation

As of migration `00055`. Both tables carry `id`, `org_id`, `created_at`, `updated_at`; those are
row bookkeeping and are not allocated by the rule.

### `alerts` — true across every firing

| column | why it is the identity's |
|---|---|
| `cluster_id`, `cluster_key` | where the signal comes from; fixed for the life of the row |
| `alert_key` | **the identity itself** — `ComputeAlertKey` over the full label set minus the source's `ignore_labels` |
| `source_fingerprint` | the upstream's own name for the same label set |
| `alertname`, `namespace`, `service`, `severity` | denormalised *members of the hashed label set*. Severity is inside `alert_key`, so a different severity is a different Alert. It is immutable by construction, not by policy. |
| `labels` | the hashed set, whole |
| `annotations`, `generator_url` | last-writer-wins metadata about the signal; not episode-scoped |
| `synthetic` | whether the signal is a drill; a property of the identity |
| `first_seen_at` | when this label set was first seen, ever |
| `last_seen_at` | when it was last seen, across all firings — the list's sort key |
| `total_cases` | how many times it has fired |
| `flap_score`, `is_flapping` | a judgement about the signal's BEHAVIOUR OVER TIME. Meaningless inside one episode. |
| `state` | ⭐ **the projection** of the current episode's phase: `firing \| resolved \| expired` |
| `current_case_id` | ⭐ the projection's subject: which episode `state` is about |
| `last_state_change_at` | when that projection last moved |
| `suppression_reason`, `suppressed_by` | ⭐ **the axis** (§4): is Alertmanager delivering this signal right now, and which upstream objects say not |

Snooze lives in its own table, `alert_snoozes`, keyed by `alert_key` — Alert-level by the verb
rule, and a table rather than a column because a snooze has its own start, end, actor and note.

### `alert_cases` — true of one firing only

| column | why it is the episode's |
|---|---|
| `alert_id` | which signal fired |
| `seq` | 1-based, gapless: *which* firing of it |
| `group_id` | which group generation THIS firing joined; the next one may join another |
| `state` | `open \| closed` — this episode is running, or it has ended (ADR 0040) |
| `started_at`, `ended_at` | the episode's own boundaries |
| `resolve_reason` | `upstream \| timeout` — **why this firing ended**; nothing else records it |
| `last_observed_at` | when this episode was last confirmed alive; drives the reaper |
| `source_starts_at`, `source_ends_at`, `source_updated_at` | the UPSTREAM's clock readings for this firing |
| `observed_skew_ms` | the gap between oto's clock and the source's, measured on this firing |
| `value` | the sample that fired *this time* |
| `rule_snapshot_id` | the rule text as it read WHEN THIS EPISODE OPENED. The rule can be edited between firings; that is the point of the snapshot. |
| `ack_state`, `acked_by`, `acked_by_label`, `acked_at`, `ack_note` | the receipt for this firing (00049, ADR 0040 §6) |
| `suppression_reason`, `suppressed_by`, `suppress_count` | ⭐ **the episode's record** of being muted while it ran (§5) |
| `state_version` | the compare-and-set token for this row |

### The columns that may never appear on either

AC-50, ADR 0013 and ADR 0036's anti-caseload clause: nothing matching
`assigned|owner|watcher|subscriber|incident|ticket|sla_|^case$|case_status|priority`. That is a
different question — *whose* fact it is, a signal's or a human's — and it has its own gate,
`test/scope/forbidden_columns_test.go`. This ADR's gate is its sibling, not its replacement.

## 4. Suppression is an axis, and it was in the state column

`alerts.state` used to admit `firing | suppressed | resolved | expired`, so `suppressed` sat in the
slot `firing` needed. Ask the rule: *is "Alertmanager is not delivering this" a phase of the
signal's life?* No. It is a statement about a **different system's routing decision**, and the
signal goes on firing underneath it. A silence is the single most common thing an operator does to
a firing alert, so the alerts this lost were not an edge case: `state = 'firing'` under-counted by
exactly the set somebody had silenced, and "is anything still on fire?" could not be answered from
the column whose entire job is to answer it.

The schema had been carrying the workaround since 00007 — `alerts_open_idx` could not spell
"live" as `state = 'firing'` and spelled it `state IN ('firing','suppressed')` instead.

So `state` narrows to `firing | resolved | expired`, and suppression becomes two columns beside it:

```
alerts.state              = 'firing'                  -- the signal IS firing
alerts.suppression_reason = 'silence'                 -- AND Alertmanager is not delivering it
alerts.suppressed_by      = {"silencedBy":["abc"]}    -- AND this is who says so
```

Three axes now, exactly as `snooze.go:25-32` demanded and as `ack_state` already worked:

| axis | column | subject |
|---|---|---|
| the signal's phase | `alerts.state` | oto's reading of the world |
| Alertmanager's delivery | `alerts.suppression_reason` / `suppressed_by` | **another system**, observed |
| oto's own quiet | `alert_snoozes` | **oto**, decided |
| a human's attention | `alert_cases.ack_state` | **a person**, per firing |

⛔ **Snooze is still NOT a `suppression_reason`.** The enum mirrors Alertmanager's four reasons —
`silence`, `inhibition`, `mute_time_interval`, `active_time_interval` — and nothing else. Adding
`snoozed` would make oto report "Alertmanager is suppressing this" when the truth is "a human asked
oto to be quiet": a lie about the world, in the columns whose only job is to mirror it. The gate in
§6 enforces this by refusing any snooze-stemmed column on either table.

`alerts_open_idx` can now state liveness without a disjunction: `WHERE state = 'firing'`.

## 5. The ruling on the Case's suppression columns: they stay

`alert_cases.suppression_reason`, `suppressed_by` and `suppress_count` are **kept**, and the two
copies are not a duplication.

The rule separates them cleanly, because they answer different questions:

- **`alerts.suppression_reason` is the LIVE axis** — *is this signal being delivered right now?*
  It is a projection of the current episode onto the identity, written by the same statement that
  writes `state` and `current_case_id`, and it is what every "show me what is firing" query reads.
- **`alert_cases.suppression_reason` is THIS FIRING'S RECORD** — *was this outage muted while it
  ran, by what, and how many times?* `suppress_count` makes the per-episode framing explicit: it
  counts suppressions **of one firing**, and it is meaningless on an identity. A silence has a time
  window, so it can mute episode 3 and not episode 4; "this firing was silenced" is therefore a
  fact about one firing, which is the right-hand side of the rule.

⚠️ **The evidence was checked before the ruling, and it does not carry the ruling.** The case
timeline **does** record both suppression edges as first-class events — `case.suppressed` (T3) and
`case.unsuppressed` (T4), `internal/alerts/domain/event.go:71-74`, appended in the same transaction
as the column write from the transition table at `internal/alerts/domain/lifecycle.go:148-153` and
`:172-177`, with counter-based §C.8 keys. So dropping the columns would **not** have destroyed the
history; it would have turned "was this episode suppressed" from a column read into a fold over an
append-only, partitioned table. The columns stay on their own merits, not because nothing else
remembers.

They also still have live readers that are asking the per-episode question:
`internal/notification/repository/snapshot.go:224,233` (the case snapshot a message is rendered
from), `internal/channels/render/slack/reply.go:454-457`, `internal/channels/render/webhookjson/renderer.go:180`,
`internal/alerts/api/dto.go:134-135`, and `web/src/features/alerts/detail/CasePanel.tsx`.

## 6. The gate

`test/scope/allocation_test.go`, modelled on `test/scope/forbidden_columns_test.go` and asserting
against the **live schema** — `information_schema` and `pg_constraint` on a database that has had
every migration applied — never against the migration text. The three failures that a grep cannot
see are the same three that file lists: DDL that never went through `db/migrations/`, an
expand/contract whose contract half was never deployed, and a `+goose Down` that restores what the
Up removed.

It enforces four claims, each one a sentence from this ADR:

1. **No ack-stemmed column on `alerts`** (`ack_state`, `acked_by`, `acked_at`, `ack_note`, …).
   §1's verb rule: a receipt does not outlive the firing it was written for. This is the column
   00049 dropped, and the gate is what stops it coming back.
2. **No snooze-stemmed column on `alerts` or `alert_cases`.** A snooze is a row in
   `alert_snoozes` with its own lifecycle; a column would be a second, lossy spelling of it — and
   on `alert_cases` it would also be the allocation error §1 forbids outright.
3. **`alerts.state` admits `firing`, `resolved`, `expired` and nothing else.** Read out of
   `pg_constraint.consrc` for `alerts_state_ck`; `suppressed` and `snoozed` are named as
   forbidden members explicitly, so the failure says *which* axis leaked back into the enum.
4. **`alert_cases.state` admits `open` and `closed` and nothing else** (ADR 0040), with `firing`
   and `suppressed` named as forbidden members for the same reason.

Like its sibling it carries an **anti-vacuity guard** — every table must answer with a plausible
number of columns, and every constraint the gate reads must actually have been found — and a
**planted-violation test** that adds each forbidden column and each forbidden enum member inside a
rolled-back transaction and asserts the gate reports exactly those. A gate no migration can fail
is a gate whose correctness nothing has ever tested.

## 7. What this ADR does not decide

- **The reasons → subject mapping.** Which `notification` suppression reasons are facts about the
  Alert and which about the Case is a separate allocation and is explicitly out of scope here.
- **Whether `alerts.suppression_reason` should become its own table**, the way snooze did, if oto
  ever needs to record *when* suppression started and ended on the identity rather than on the
  episode. Today the episode's timeline answers that and the column is a projection; if that stops
  being true, this is the ADR to amend.
