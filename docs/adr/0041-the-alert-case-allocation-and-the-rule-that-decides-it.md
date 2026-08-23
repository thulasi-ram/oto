# 0041 — The Alert/Case allocation, and the one rule that decides it

**Status:** Accepted
**Date:** 2026-08-18
**Answers:** ADR 0040 §8, which named "does suppression belong to the Alert?" a real question and
deliberately did not answer it. It does. §4 below.
**Supersedes in part:** ADR 0040 §3 — the derivation table's open half. `Case.AlertState()` no
longer reads `suppression_reason`, because after this ADR there is no `suppressed` state to
derive. The closed half is unchanged and still total.
**Migration:** `00055_suppression_is_an_axis.sql`
**Gate:** `test/scope/allocation_test.go`
**Amended by:**
[Amendment 1](#amendment-1--a-case-is-open-until-its-alert-has-stayed-resolved-for-w) (2026-08-18,
migration `00057`) — the **case retention window W**. §1's rule, §4's axis and §5's ruling are
unchanged; what changes is WHEN a Case closes, and §3's `alert_cases` table gains two columns.

> ⚠️ **§3's allocation is stated as of `00055` and is two columns short.** `alert_cases` gained
> `resolve_pending_at` and `resolve_pending_end_at` in `00057`; their rows, and the reason a third
> table (`case_policy_config`) does not disturb §1's "two tables hold what oto knows about a signal",
> are in Amendment 1. §3's `flap_score` / `is_flapping` row still says where the two columns BELONG,
> and they are now RETIRED IN PLACE — readable, never written again — which the amendment's *the flap
> detector went BLIND* section decides in full.

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
| `flap_score`, `is_flapping` | a judgement about the signal's BEHAVIOUR OVER TIME. Meaningless inside one episode. ⛔ **RETIRED IN PLACE** by Amendment 1: still read from the row, never written again. |
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

## Amendment 1 — a Case is open until its alert has STAYED resolved for W

- **Date**: 2026-08-18
- **Migration**: `00057_a_case_outlives_the_flap.sql`
- **Resolves**: git-bug `1816a42` — *"A flapping alert opens one case per flap, so the only place left
  to reduce the noise is the notification layer"*
- **Amends**: §1's one-sentence Case ("it opens once, it closes once" — still true, but WHEN it closes
  is no longer "on the resolve"), and §3's `alert_cases` table, which gains two columns.
- **Revised 2026-08-18, no migration**: this amendment first recorded the blinded flap score as a
  DEBT and left un-blinding open. It is now DECIDED — **`alerts.flap_score` and `alerts.is_flapping`
  are RETIRED IN PLACE**, the detector and its job are deleted, and the columns stay readable. See
  *the flap detector went BLIND* below for the whole ruling, including the `COMMENT ON COLUMN` text a
  future migration owes.
- **Leaves untouched**: §1's allocation rule, §2's bug filter, §4's axis, §5's ruling on the Case's
  suppression columns, §6's gate. Nothing below reallocates a column; the amendment adds two, on the
  side §1's question already puts them.

### The restatement

A Case is **one firing episode**, and the episode does not end when the alert resolves. It ends when
the alert **has stayed resolved for W**, the case retention window.

```
W = 0   ->  the resolve closes the episode          (the default, and the pre-00057 behaviour)
W > 0   ->  the resolve is RECORDED on the open row; the close falls due W later
            a re-fire inside W finds the episode STILL OPEN and runs T2
```

W=0 is the degenerate case and it is the shipped default: `case_policy_config.retention_window_s` is
`NOT NULL DEFAULT 0`, the table starts empty, and an absent row is 0.

⛔ **IT IS A DELAYED CLOSE AND NEVER A REOPEN, SO ADR 0040 IS UNAMENDED.** `ended_at` is written
once, `case.reopened` and T8 stay retired, `case_terminal_ended` is unchanged, and
`case_one_open_idx` (`UNIQUE (alert_id) WHERE ended_at IS NULL`) keeps its exact meaning — the
retained episode **is** the open one, and no second `seq` is minted. W moves *when* a Case closes; it
does not change *how many times*.

The noise this removes is the noise that used to exist. An alert resolving and re-firing six times
inside W produced six episodes, six root cards, six thread replies and six pings, and since ADR 0040
nothing may merge them. Under W it produces one of each.

### W's axes are ADR 0038's, coarser by exactly one

`case_policy_config (id, org_id, namespace, alertname, retention_window_s, created_at, updated_at)`,
with `case_policy_axes_uniq UNIQUE (org_id, namespace, alertname)` and
`case_policy_window_ck CHECK (retention_window_s BETWEEN 0 AND 86400)`.

| | keys on |
|---|---|
| ADR 0038's group key | `H(org_id, cluster_key, canon({alertname} ∪ {namespace if non-empty}))` |
| W | `(org_id, namespace, alertname)` — **the same axes minus `cluster_key`** |

One window governs the same `alertname` in every cluster of an org. That is deliberate: ADR 0038
records splitting as the safe direction to take later, and starting split would make an operator
write the same number once per cluster to get the obvious behaviour. Sharing the grouping axes is the
point — an operator learns one set of dimensions, not two.

⛔ **An absent namespace is `''` here and NULL everywhere else, and the reason is the index.**
`alerts.namespace` is NULL for both absent and empty because Prometheus treats them as equivalent and
`SplitLabels` omits the axis for both, so they are already one partition. Two NULLs are not equal
under a UNIQUE index, so a nullable column would let one org hold two contradictory windows for one
`alertname`. Every caller normalises through `repository.NormaliseNamespace`, and every read spells
the lookup `COALESCE(alerts.namespace, '')`.

⛔ **There is no org-wide row and no wildcard.** A default lives in code, where it cannot be
half-configured, and an absent row *is* the default. `case_policy_name_ck` makes `alertname`
mandatory, which is what forbids the org-wide row.

### §3's `alert_cases` table, as of `00057`

| column | why it is the episode's |
|---|---|
| `resolve_pending_at` | **oto's clock: when this episode's delayed close falls due** — the last upstream resolve plus W. It moves FORWARD on each fresh resolve inside the window, because the rule is "stayed resolved for W" and not "resolved W ago", and a re-fire clears it. |
| `resolve_pending_end_at` | **the `ended_at` this close will stamp**: the UPSTREAM claim from the resolve observation, clamped to `>= started_at` by §B.3.2. |

Both answer §1's question the same way: they are the receipt for **this** firing's resolve, they mean
nothing about the label set, and they are gone the moment the episode closes. There is no projection
of either onto `alerts`, so §1's "both tables" licence is not invoked and §5 remains the only place
it is.

**`case_policy_config` is a third table and it does not disturb §1's "two tables hold what oto knows
about a signal."** It holds no fact about a signal at all — it is operator policy, keyed on label
axes rather than on an Alert or a Case, and it is joined to neither. It is `notification_policies`'
neighbour, not `alerts`'.

`resolve_pending_end_at` exists so **W is never charged to the signal**. Closing at the sweep's clock
would make every reader of `ended_at` — the case list's duration column, the daily rollup, the
firing-duration statistic (R8) — report an episode W longer than the signal actually burned. The
window is oto's own damper; the signal is not billed for it.

Four CHECKs and one index carry the invariants:

| constraint | claim |
|---|---|
| `case_pending_pair_ck` | `(resolve_pending_at IS NULL) = (resolve_pending_end_at IS NULL)` — both or neither |
| `case_pending_open_ck` | a pending close belongs to an `open` episode only. With `case_terminal_ended` this is what makes the delayed close **single-shot**: the close writes `ended_at` and clears both columns in one UPDATE, so a closed row cannot carry another |
| `case_pending_order_ck` | `resolve_pending_end_at >= started_at` — the same floor `case_order_ck` puts on `ended_at`, applied to the value that is going to become it |
| `case_pending_supp_ck` | `resolve_pending_at IS NULL OR suppression_reason IS NULL` |
| `case_close_due_idx` | `(org_id, resolve_pending_at) WHERE resolve_pending_at IS NOT NULL` — the sweep's whole scan, and empty on every deployment where W is 0 |

`case_pending_supp_ck` is §4 and §5 restated in DDL. An upstream resolve is **positive proof of
non-suppression** — Alertmanager would not have delivered it otherwise, which is the same argument
§B.3.1 uses to let ingest drive T4.

~~so the deferral clears `suppression_reason` and `suppressed_by` exactly as an immediate T5 always
did.~~

> ⚠️ **CORRECTED, 2026-08-19.** That sentence was the origin of a real defect and the clause is now
> unreachable. A silenced case that resolved upstream while W > 0 entered the deferral, which cleared
> the suppression axis while leaving `state = open` — so the transition reported `From=suppressed`,
> `To=firing`, emitted no events, and `CloseDeferred` suppressed the notification. `applyEdge`
> reported `stateChanged`, `projectionFor` wrote `alerts.suppression_reason = NULL`, and the UI
> dropped the silence chip and showed the case FIRING for the length of the window with no
> `case.unsuppressed` event and nobody told.
>
> **The deferral is now refused from the suppressed arm and such a case closes immediately**
> (`cmd.CaseRetention > 0 && from != StateSuppressed`). Preserving the axis across a deferral is not
> available: `case_pending_supp_ck` and `Case.check` both forbid a receipt beside a reason. Clearing
> it is the defect — suppression is an AXIS, not a state, and the end of one is announced by T4.
> §B.8.4 makes the identical ruling for snooze: a silent edge may not move that axis, "or it becomes
> the silent suppression that §B.6 forbids". Nothing W exists for is lost, because MuteStage (C1)
> means a suppressed alert delivers no re-fires to damp in the first place. Held by
> `TestASilencedCaseNeverDefersItsCloseIntoASilentUnsuppression`.

The DDL claim above is unchanged. §5's per-episode record is not lost either: `suppress_count` is
untouched, and it is the column §5 names as the one that makes the per-episode framing explicit.

### The receipt is what keeps "a resolution is never fabricated" true of a background sweep

The close is performed by `case.reap`, on its existing sixty-second cadence
(`internal/app/workers.go:245-252`). A background sweep producing `resolved` looks like the one thing
`00007` says oto must never blur, and it is not, because the sweep can only **spend** a resolution:

1. `closeDueCandidatesSQL` (`internal/alerts/repository/case.go:1035-1042`) selects only rows that
   already carry a receipt, so an episode nobody resolved is unreachable from it.
2. `TriggerCloseDue` refuses the edge when the **freshly re-read** row has no pending close
   (`internal/alerts/domain/lifecycle.go:522-541`) — the same shape as `unreapable`.
3. T6 refuses an episode holding a receipt (`lifecycle.go:602-604`), and `reapCandidatesSQL` filters
   it out with `AND resolve_pending_at IS NULL` (`case.go:997`). Without that, every case inside its
   window would be expired as `timeout` on the next tick — oto claiming it stopped hearing about an
   alert whose resolution it was holding.

Only §B.3's T5 arm, from an explicit upstream `status="resolved"`, writes the two columns.

The re-fire path clears the receipt in **two** places, and both are necessary: the domain clears it on
the Case (`lifecycle.go:474-475`) and `observeSQL` clears it in the UPDATE
(`internal/alerts/repository/case.go:696-697`), because T2 persists through `Observe` rather than
`Transition`.

### W=0 changes nothing, and the T5 arm's ORDER is the whole argument

The pre-00057 T5 statements are unedited and in their original order. Two guards were added **above**
them and one branch **below**:

| position | added | value at W=0 |
|---|---|---|
| above | `if cmd.Trigger == TriggerCloseDue` (`lifecycle.go:522`) | unreachable — the trigger has one caller, `Service.CloseDue`, which iterates a scan that is empty |
| above | `if cmd.CaseRetention > 0` (`lifecycle.go:542`) | false — `CaseRetention` is read only on `TriggerObserveResolved` (`internal/alerts/service/lifecycle.go:389-399`) and `RetentionWindow` returns 0 for a missing row and for a stored 0 alike |
| below | `if o.ClosePending()` (`lifecycle.go:586-589`) | false — only a configured window can have written the columns it reads |

So at W=0 the executed statement sequence *is* the pre-00057 arm. The same shape holds at the two
other sites that grew a branch: `applyEdge`'s notification gate (`service/lifecycle.go:570`) tests
`!r.CloseDeferred`, which is true whenever nothing was deferred, and `Apply`'s early return
(`lifecycle.go:662`) is not taken.

⚠️ **NO TEST HOLDS EITHER OF THE TWO PARAGRAPHS ABOVE, AND THAT IS STATED HERE RATHER THAN LEFT TO BE
DISCOVERED.** The ticket asks for two: one asserting a resolve/re-fire/resolve sequence inside W
yields ONE case with one `started_at`, and one asserting W=0 is byte-for-byte the old behaviour.
Neither exists. No `_test.go` file in this tree names `CaseRetention`, `ClosePending`, `CloseDue`,
`resolve_pending_` or `case_policy` — the mechanism has no gate at any layer, and `test/scope`'s
allocation gate (§6) does not cover the two new columns either. Until those tests land, "W=0 is the
old behaviour" is an argument about the shape of one `switch` arm, and the next edit to it is
unopposed.

### The flap damper left the notification layer, and the CHECK did not follow it

Damping a flap at delivery makes a withheld notification indistinguishable from a signal that never
fired, which §B.6 refuses. W removes the cause, so the delivery-side damper is gone:

- ~~`SuppressedFlapping` is **retired at the writer**: `retiredSuppressedReasons`
  (`internal/notification/domain/suppression.go:104-105`), `Retired()` (`:109`), and the refusal
  inside `Add` (`:171`) — the one road into a `Suppressors` set. A comment saying "do not record
  this" is advice; a refusal at the only writer is a guarantee.~~ ⚠️ **That retirement was
  superseded: the value is DELETED, and so is the mechanism.** Migration `00059` narrows
  `notifications_suppmap_ck` to six values and migration `00060` narrows `notifications_reason_ck`
  to eighteen; neither performs an `UPDATE`, and the maintainer authorised a reset of the only
  database that exists rather than a downlevel mapping. **Retired means the CHECK still admits the
  value; deleted means the CHECK was narrowed** — this is the second, so there is no older row
  left for a decoder to meet. `SuppressedFlapping`, `SuppressedStorm` and
  `retiredSuppressedReasons` are gone from `internal/notification/domain/suppression.go`, which now
  declares six suppressors and no retirement table at all.
- The flapping reply gate in `PlanFor` is **deleted** (`internal/notification/domain/mode.go:336`,
  where the comment stands in its place). It read `in.Flapping && in.Reason != ReasonRuleChanged`.
  A gate there now would drop the ONE reply a damped flap produces and leave the flap invisible in
  the thread.
- `dampReason`'s `flapping` arm is **deleted** (`mode.go:403-418`); one damper remains, `storm`.
- `PlanInput.Flapping` is retired and read by nothing (`mode.go:162`).

Two live spellings survive on purpose, and neither writes a row:
`internal/notification/service/notify.go:566-567` still maps a `"flapping"` drop reason that `PlanFor`
can no longer return, and `Add` swallows it; `internal/notification/api/audit.go:33` still offers
`flapping` as a filter value, which is the **read** side and must keep working.

⛔ **REFUSED, DELIBERATELY: narrowing `notifications_suppmap_ck`.** The ticket's done-when 6 asks for
it. `00018:53-55` admits eight values, of which `no_policy`, `throttled`, `storm`, `snoozed`,
`verbosity`, `channel_disabled` and `duplicate_render` are all live and all still written; and
`00018:71-75` establishes this repo's rule that an enum narrowing with **no downlevel mapping** must
FAIL rather than rewrite history. `notifications` rows have no reaper, so rows spelling `flapping`
exist indefinitely and removing the value would make an audit page error rather than render. The
contraction is a separate migration for whoever drops the last such row, which is not a date anybody
can name.

### The flap detector went BLIND, not dead — and the answer is RETIREMENT IN PLACE

⭐⭐ **DECIDED (2026-08-18, superseding this section's original "debt, unpaid" ruling and open
item 2 below): `alerts.flap_score` and `alerts.is_flapping` are RETIRED IN PLACE. The score is
not taught to see. W at case formation is how oto handles flap noise.**

⚠️ **Why, in one paragraph. W makes the score blind, and the blindness is precisely
anti-correlated with the phenomenon.** `flap_score` was an EWMA over counted lifecycle events:
`stateChangeCountsSQL` (`internal/alerts/repository/event.go`) counts `case.opened`,
`case.reopened`, `case.resolved`, `case.expired`, `case.suppressed`, `case.unsuppressed` and
their six pre-ADR-0036 `occurrence.*` spellings, inside `flap_window_s`; `ScoreFlaps` set
`is_flapping := n >= flap_threshold`. A damped flap appends **none** of those events — `Apply`
returns before the append when the close is deferred (`lifecycle.go`), and the re-fire runs T2,
whose only event is `alert.mutated` and only on a material change. So:

- Six flaps in ten minutes used to append twelve counted events; damped into one episode they append
  **two** — one `case.opened`, one `case.resolved` at the real close.
- `DefaultFlapThreshold = 5` over `DefaultFlapWindow = 7200 s`
  (`internal/platform/tuning/defaults.go:109,127`). Two is below five.
- W's ceiling is `86400` s, twelve times the default flap window, so a single retained episode can
  outlive the whole window the score is computed over.

`is_flapping` therefore read **false exactly when flapping was happening**, and
`alert.flapping_ended` would have been minted *because* the flapping got worse. **A detector that
lies is worse than no detector**, and a lying detector cannot be left in the product as a "visible
state" — the ticket wanted an operator to see a rule missing its `for:`, and a false negative
teaches them the opposite.

**What is DELETED (the Go half).**

| gone | file |
|---|---|
| `AlertRepository.SetFlap` — the ONLY statement that wrote either column | `internal/alerts/repository/alert.go` (tombstone in place) |
| `SetFlap` on the service's `AlertRepository` port | `internal/alerts/service/ports.go` |
| `Service.ScoreFlaps` and `FlapResult` | `internal/alerts/service/sweep.go` (tombstone in place) |
| the `EventCounter` port, `Deps.EventCounts`, `Service.eventCounts` and the container wiring | `internal/alerts/service/deps.go`, `service.go`, `internal/app/container.go` |
| the `flap.score` job: kind, args, `Handlers` field, registration, periodic tick, handler | `internal/platform/jobs/{kinds,args,registry}.go`, `internal/app/workers.go` |
| the timeline events `alert.flapping_started` / `alert.flapping_ended` — ~~**RETIRED, not deleted**~~ **DELETED** (⚠️ see below) | `internal/alerts/domain/event.go` (tombstone in place); migration `00060` narrows `ev_type_ck` to refuse both spellings |

⚠️ **The two damper event types were RETIRED when this amendment was written and are now
DELETED.** The retirement bargain keeps a value declared so a decoder meeting an older row can still
render it, and it buys nothing once no such row can exist: migration `00060` narrows `ev_type_ck` to
refuse `alert.flapping_started` and `alert.flapping_ended` outright, it performs no backfill, and the
maintainer authorised a reset of the only database in the world rather than rewriting a type into a
transition that never happened. **Retired means the CHECK still admits the value; deleted means the
CHECK was narrowed.** `retiredEventTypes` (`internal/alerts/domain/event.go`) survives holding exactly
three entries — `group.member_joined`, `group.member_left` and `case.reopened`, older retirements
whose CHECKs were never narrowed — and neither damper type is among them.

`EventRepository.StateChangeCounts` and `stateChangeCountsSQL` deliberately SURVIVE with no
consumer: `test/arch/eventtype_test.go` registers that statement as one of the three SQL sites
that must spell both the canonical and the legacy form of a lifecycle type.

**What is KEPT, and why that is not a contradiction.** Both columns stay in the schema with their
last value, and every READ keeps working: `alertColumnList`, the `?flapping=` list filter, the
alert rollup, the `alert.history` enrichment, the notification snapshot and the Slack card. This is
the `retiredEventTypes` bargain applied to a pair of columns (`retiredSuppressedReasons` is struck
above and no longer exists) — **readable, unwritable** — and it is why no migration is required to land the retirement. A value
already on a row is a measurement taken at a time, i.e. history; dropping the columns would make
oto unable to render its own past. That is also the difference from storm: `alert_groups.storm_mode`
was LIVE STATE no writer could set again, "a lie with a `NOT NULL DEFAULT false`", so 00059 dropped
it.

⛔ **NO MIGRATION AND NO COLUMN DROP.** `00007` cannot be edited, and its comment now advertises a
standard that is no longer live:

```
COMMENT ON COLUMN alerts.flap_score IS 'Rolling flap metric from the flap.score job. Never negative.';
COMMENT ON COLUMN alerts.is_flapping IS 'A VISIBLE UI state, never silent suppression -- silence destroys trust (SPEC §B.6).';
```

⭐ **The exact text a future migration MUST set** — the one schema change this decision still owes,
and the only one:

```sql
-- +goose StatementBegin
COMMENT ON COLUMN alerts.flap_score IS
  'RETIRED IN PLACE (ADR 0041 Amendment 1, SPEC B.6.2). The last value the retired flap.score job wrote, and it is never written again: there is no writer in the tree. It stays READABLE so history renders -- a value here is a measurement taken at a time, not a live judgement. It went BLIND rather than dead: the case retention window W (00057) damps a flap at CASE FORMATION, so a damped episode appends neither case.opened nor case.resolved and this EWMA read below flap_threshold exactly when the alert was flapping hardest. W is how oto handles flap noise. Never negative (alerts_flap_ck).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alerts.is_flapping IS
  'RETIRED IN PLACE (ADR 0041 Amendment 1, SPEC B.6.2). The last verdict the retired flap.score job wrote, still read by ?flapping=, the alert rollup, the alert.history enrichment and the Slack card, and never written again. It was a VISIBLE UI state and never silent suppression -- and it was retired precisely to keep that promise, because after 00057 it reported false while the alert flapped. A detector that lies is worse than no detector.';
-- +goose StatementEnd
```

Nothing else in the schema changes: `alerts_flap_ck` stays (it still constrains the stored value),
neither column is dropped, and the contraction — if it ever comes — is a file of its own.

⚠️ **The identity half has NOT landed.** `flap_threshold`, `flap_window_s` and
`flap_digest_interval_s` still exist in `internal/identity/domain`, in the settings API and in
`platform/tuning`, and they now configure NOTHING — `deploy/helm/oto/values.yaml` and
`docs/setup/tuning.md` say so in as many words, and `alerts/service.Settings` still carries the two
numbers only because `identity/domain/defaults_derivation_test.go` mirror-tests the defaults through
it. Storm's three keys were
deleted on the "no oto database and no Helm release exists outside a development laptop, so there is
no operator to CrashLoop" argument (ADR 0042, migration 00059); the same argument is available here
and has deliberately not been spent by this change.

### What this amendment refuses, and why

| refused | reason |
|---|---|
| narrowing `notifications_suppmap_ck` to drop `flapping` | above: seven live values, no reaper, and `00018:71-75`'s rule against a narrowing with no downlevel mapping |
| dropping `flap_score` / `is_flapping` | a dropped column makes oto unable to render its own past; the value on a row is history. They are RETIRED IN PLACE instead — readable, unwritable — and a future migration only restates their `COMMENT` |
| teaching the score to see, by minting an `alert_events.type` for the held resolve | it is an API-contract change and a codegen run spent on a second-order detector behind the damper that already works. **W is the flap answer**, and one damper is the point. This is now REFUSED rather than deferred |
| `cluster_key` as a fourth axis on `case_policy_config` | splitting later is safe (ADR 0038); starting split makes an operator write one number per cluster for the obvious behaviour |
| an org-wide or wildcard row | a default in code cannot be half-configured; `case_policy_name_ck` forbids the row that would offer one |
| a §B.4 source-health guard on the due close | §B.4 stops oto INFERRING an ending out of silence, and there is no inference here. A source going dark after a resolve arrived does not un-resolve the alert, and holding the close would keep the episode open for the whole outage — the failure mode W exists to remove |
| closing at the sweep's clock instead of `resolve_pending_end_at` | it would charge W to the signal's firing duration (R8) |
| a writer on `CasePolicyRepository` | the surface that creates these rows is separate from the ingest path that reads them, as `PolicyRepository` (read) is separate from `ConfigRepository` (write), so the evaluator cannot rewrite the rule it is evaluating |

⚠️ **Consequence of the last row: W is settable only by SQL today.** A grep for
`case_policy|retention_window_s|CaseRetention` over `api/`, `web/`, `deploy/` and `docs/` returns
nothing — there is no OpenAPI operation, no settings screen, no Helm value and no SPEC section for
it. The mechanism ships inert and stays inert until that surface exists, which is consistent with
W=0 being the default but means no operator can turn it on through the product.

### What this amendment does not decide

ONE question is open. The second was answered on 2026-08-18 and is struck through below rather than
deleted, so the trade it settled stays legible.

1. **May an unacked reminder fire inside W?** `unackedGroupsSQL`
   (`internal/notification/repository/reminders.go:67-68,77`) selects members on `o.state = 'open'` AND
   `a.state = 'firing'` AND `a.suppression_reason IS NULL`. A deferred close leaves `From == To`, so
   `applyEdge` reports no state change (`internal/alerts/service/lifecycle.go:579`) and
   `alerts.state` stays `firing` for the whole window. An episode inside W therefore still satisfies
   every clause, and a group can be nagged about an alert whose upstream resolve oto is already
   holding. Whether that is right is a product question: the reminder's intent is "nag about what
   somebody is still being paged for", and during W nobody is — but W also exists precisely because
   the alert may be about to fire again.
2. ~~**How does `flap_score` stop being blind?**~~ **DECIDED — it does not.** The detector is
   RETIRED IN PLACE (see *the flap detector went BLIND* above): no new `alert_events.type` is
   minted, the score is not recomputed from another input, `is_flapping` is not re-derived from the
   pending-close columns, and W at case formation is oto's flap answer. The columns keep their last
   value and stay readable. What remains owed is one `COMMENT ON COLUMN` migration, whose exact text
   is above, and the identity-side deletion of three settings keys that now configure nothing.
