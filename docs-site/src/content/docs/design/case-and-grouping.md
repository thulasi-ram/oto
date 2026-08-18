---
title: oto — the Case, the derived group, and where the three axes live
---
> **Status:** Design decisions agreed 2026-08-17/18; **shipped 2026-08-18** in migrations
> 00048–00052 and the code that rides them. The two arguments that needed recording before the
> code could land are now recorded: [ADR 0036](/adr/0036-alertoccurrence-becomes-alertcase/)
> (the rename, argued against FR-1 by name) and
> [ADR 0038](/adr/0038-the-group-key-is-derived-from-the-alerts-own-labels/) (the derived key).
> **Two proposals here were deliberately NOT adopted**, and both are marked in place below:
> group close is still driven by `last_activity_at` rather than by "no live case with this split
> key" (§5.3), and `alert_groups.receiver` / `source_group_key` were **kept as provenance**
> rather than allowed to go vestigial (§5.2). One item stays open: the split key has been
> replayed only against synthetic fixtures, never against production payloads (§11).
> **Precedence:** below `SPEC.md` and `SCOPE-BOUNDARY.md`. Where this document contradicts
> either, `SPEC.md` won — it has since been amended to match what shipped, and this document is
> kept as the record of *why*, not as a description of the current schema.
> **Supersedes in part:** ADR 0005's assignment of the Slack thread to an Alertmanager-derived
> group key, and the `filters.ts` rule that snoozed alerts stay in the default list.
> **Tickets:** the work is filed in `git bug`; see *What was filed*, below.
> **Companion:** [`case-and-grouping-handoff`](/design/case-and-grouping-handoff/) — session
> state, tooling traps, and where to start.

---

## 0. The question this document answers

*"Alertmanager already groups. What is oto's grouping for, what is a firing episode called,
and which of ack, snooze and state belong to which noun?"*

The conversation that produced this document started from a narrower question — whether the
grouping tuning knobs should be scoped per namespace like notification policies — and ended
by rewriting the vocabulary. The narrow answer is in §1; everything after it is why.

---

## 1. The narrow answer: grouping tuning stays org-wide

`storm_threshold`, `storm_window`, `storm_cooldown` and `group_close_delay` remain single
per-org values. Scoping them by matcher was considered and rejected, on three grounds:

1. **`Origin` is deliberately two-valued.** `identity/domain/settings.go` argues that a third
   origin is "inventing a hierarchy". Scoped tuning forces `default → org → policy`.
2. **The unit is wrong, not the scope.** Storm collapse counts distinct alerts joining *one
   generation*. If a namespace's flood spreads across forty generations, no per-namespace
   threshold helps — the aggregation unit is upstream, not the number.
3. **Notification policies already carry the per-scope levers** that matter: matchers, reasons,
   channels, throttle, reminder delay. Extra filtering goes there.

Two constraints on that last point, both discovered rather than assumed:

- **Throttle is per-subject**, not per-scope (`notification/service/ports.go:48`). Four hundred
  alerts in one namespace are four hundred subjects with four hundred budgets. Throttle caps one
  subject repeating; it does nothing about volume across a namespace.
- **Every matcher in oto sees group labels only.** There is one match entry
  (`notify.go:269` → `policy.go:93`) and it is fed `snap.Group.GroupLabels`; the reminder path
  uses `g.GroupLabels` too (`reminder.go:127`). If a label is not in the upstream `group_by`,
  a matcher on it matches nothing — and fails quietly, as a `no_policy` suppression.

That second constraint is what makes the rest of this document necessary.

---

## 2. What Alertmanager's grouping actually is

`group_by` is **a declared notification-batching boundary, never a relatedness claim.**

The schema settled this on its own: `alert_group_members` had `PRIMARY KEY (group_id,
occurrence_id)` — one episode, many groups — and multi-membership was reachable via
`continue: true`, which `amroutes.go:31` names as "where multi-delivery comes from". Identity is
not many-to-many. A thing cannot be identical to two different things. (That join table was
dropped by 00051 once §5 removed the only legitimate way to be in two groups at once; the
argument it made about `group_by` is why, not a description of today's schema.)

What `group_by` does control:

| | effect |
|---|---|
| `group_by` | which alerts share an envelope; which labels are stamped as `groupLabels`; group **size**, hence exposure to both truncation caps |
| `group_wait` | how long before a group's first send |
| `group_interval` | how often a group re-sends after a change |
| `repeat_interval` | how often an unchanged group re-sends anyway |

Three corrections to the intuitions that usually accompany this:

- **It does affect how many alerts arrive.** Alertmanager's `max_alerts` truncates `alerts[]`
  and reports only a count in `truncatedAlerts`; oto truncates again at
  `MaxAlertsPerBatch = 10000` (`ingestion/domain/bounds.go:33`). Coarser grouping means bigger
  groups means more truncation. Those alerts are never sent, not merely delayed.
- **It does not control frequency.** It controls *partitioning*. Finer grouping is not faster —
  it is more independent timer chains, each paying its own `group_wait`, at the same cadence.
- **The payload is cumulative membership, not a delta.** A fourth alert joining an existing
  group produces a re-send carrying all four. This is why `notification_reason` needs separate
  values for `first notification`, `new alerts added` and `repeat interval elapsed`, and why
  `DedupTTL` exists.

**Absence from `alerts[]` means nothing.** MuteStage drops muted alerts before the webhook, so
a disappearance may be a mute, a truncation, or simply a non-resend. `NormaliseStatus` encodes
the asymmetry: an unknown status becomes `firing`, because over-reporting a firing alert is
recoverable by the reconciler and a fabricated resolution is not. **Resolution cannot be
detected by diffing group membership across payloads.**

---

## 3. Vocabulary

### 3.1 `AlertOccurrence` becomes `AlertCase`

The entity is unchanged: one contiguous firing episode, `(alert_id, seq)`, gapless, at most one
open per alert. The rename makes it the noun the product leads with. **Shipped in 00052** —
`alert_occurrences` is now `alert_cases`, `alerts.Occurrence` is now `alerts.Case`. The
`occurrence.*` values already written into `alert_events` were deliberately left unrewritten —
that log is append-only, monthly-partitioned and retained thirteen months — and are read through
a translation instead: `NewEventType` accepts the pre-rename spelling and returns the canonical
`case.*` value, so `occurrence` never reaches a client.

`case` sits adjacent to §A.1's scope-ban family (`incident`, `triage`, `assignee`, `owner`,
`responder`), and §A.1's own rule is that such a word is *"presumed over the line until argued
otherwise against FR-1 by name"*. The argument is made in §3.2 and was recorded, as required,
before the rename landed: [ADR 0036](/adr/0036-alertoccurrence-becomes-alertcase/).

Rejected alternatives: **`AlertRollup`** — `rollup` already means aggregated delivery counts
(`RollupAlert`, `RollupOccurrence`, `RollupGroup`); **`Episode`** — accurate, already the
definition word, rejected by the owner as too broad; **`AmAlert`/`OtoAlert`** — three entities
named Alert is the state ADR 0003 was written to escape, and `OtoAlert` named a *set* after its
members.

### 3.2 The Case / correlation line

| Case — in scope | correlation — DEFERRED-POST-V1 |
|---|---|
| subject is a **signal** | subject is a **response effort** (FR-1) |
| machine-created, always | human-created |
| one alert identity, one contiguous span | spans disjoint signals |
| state owned by Alertmanager, mirrored | state owned by humans |
| human verbs: acknowledge, comment | severity, status, roles, comms |
| no present-tense person reference | assignment, ownership, watchers |
| cannot outlive its alert | survives the deletion of every alert (H-2) |

**The test is H-2.** Delete every alert — does the object still mean anything? If yes it is an
incident and it is out. A Case is meaningless without its alert, because it *is* that alert's
firing span.

Note `case` is reserved in Go and SQL. `AlertCase` / `alert_cases` are fine; a *column* named
`case` is not.

---

## 4. The three axes

`state`, `ack_state` and `snooze` are orthogonal — an alert can be firing **and** acked **and**
snoozed at once, and all three are displayed. Each now lives at exactly one authoritative home,
and only `state` is additionally projected onto `alerts`.

```
  axis        authoritative home        projected onto alerts?
  ─────       ──────────────────        ──────────────────────
  state       alert_cases               YES — the only question that still has an
                                        answer when nothing is firing, and it leads
                                        four indexes on the list path
  ack_state   alert_cases               NO  — alerts.ack_state dropped by 00049
  snooze      alert_snoozes             NO  — alerts.snoozed_until dropped by 00048
```

### 4.1 Why ack is a Case concept

Ack is **backward-looking**: a receipt for a firing that happened. Its statement stops being
true when a new episode begins.

- It cannot exist without an episode: `case_ackorder_ck CHECK (acked_at IS NULL OR acked_at >=
  started_at)` (`occ_ackorder_ck` before the 00052 rename).
- Alert-scoped ack means a March acknowledgement pre-acknowledges a September firing, which
  never reaches anyone's queue. Every fix for that — "clear the ack on resolve" — *is*
  case-scoping under another name.
- Ack suppresses nothing. `acked` is absent from the eight `SuppressedReason` values; acking
  means the unacked-reminder intent is never minted. Snooze suppresses and leaves a row.

### 4.2 Why snooze is an Alert concept

Snooze is **forward-looking**: a mute on notifications that have not happened. Its statement
must survive across episodes — that is its entire purpose.

- You can snooze a *resolved* alert. There would be no case row to write it on.
- A snooze set at 09:00 until tomorrow spans however many cases fire in between. A field cannot
  live inside one of the several things it covers.
- Inventing a case to hold a snooze would fabricate a `state` in the one table whose job is to
  mirror the world.

**A snoozed alert still creates a Case and is simply not notified about.** The alternative — not
creating the case — breaks four invariants at once: the history lies, `alerts.state` has no
honest value, snooze becomes a suppression of the *signal* (explicitly banned in `snooze.go`),
and the reconciler re-opens the case anyway.

### 4.3 The rule the two verbs share

> **Scope a human verb to the lifetime of the claim it makes.**

Ack and snooze are not siblings but duals: same actor, same orthogonality to state, mirror-image
temporal direction, each landing on the smallest subject on which its statement stays true.

---

## 5. The derived group key

`AlertGroup` stops mirroring Alertmanager and starts being computed from the alert's own labels.

```
  before    GroupKey = f(org, source_id,  receiver,  AM's groupLabels)   keys.go:167
  shipped   GroupKey = f(org, cluster_key, alertname, namespace-or-∅)    ADR 0038, 00050
```

The axes are the ones `alerts/domain/labels.go` already promotes, minus the two that must not
split: `severity` (an escalation is the same problem getting worse, and group severity is an
aggregate that only means something if both live in one group) and `pod`/`instance` (the thing
being grouped). `service` is deliberately omitted at first; add it only on evidence.

**The key must be fixed, not configurable.** A tunable split key reinvents `group_by` inside oto
and re-inherits the problem it was built to escape. SPEC's `correlation` charter already words
the requirement: *"machine-derived groupings… with a **stated** algorithm"* — stated, not
configured.

### 5.1 Why a label-based split is structurally safe

Alert identity is the label set. Change any label and it is a different Alert with its own
cases. Therefore **an alert's split key is immutable for its whole life**, and a label-based
rule can never move an alert between threads — which matters because Slack threads cannot be
re-parented. The residual risk is choosing too finely up front, not re-parenting.

This is also why splitting is the only safe direction. Splitting is decidable at receipt from
data in hand; merging needs alerts that have not arrived yet, so it requires re-implementing
`group_wait` inside oto.

### 5.2 What it buys

```
  ✓ ADR 0005 survives UNAMENDED — the group still owns exactly one thread
  ✓ routing precision: the group's labels become oto's chosen axes, so a matcher on
    `namespace` no longer depends on the operator's group_by
  ✓ the reconciler path works — computable without groupLabels, which
    GET /api/v2/alerts does not return (today those groups get an empty receiver
    and no groupLabels, per 00008:81)
  ✓ continue:true double-threading disappears — receiver leaves the key
  ✓ alert_groups.receiver / source_group_key leave the key; ingest_batches
    already records both, with the raw payload and the truncation count
```

**Caveat.** Dropping `receiver` from the key merges two routes that deliberately separate the
same alerts. `cluster_key` must be what distinguishes them — which it should be anyway, since
alert identity is already `(org, cluster)`.

> **Shipped differently (00050).** This document expected `alert_groups.receiver` and
> `source_group_key` to become vestigial. **Both columns were kept**, as provenance rather than
> identity: they record what the upstream said when the generation was first delivered into, and
> `receiver` is still the `listAlertGroups` `receiver=` filter and still echoed in the webhook
> envelope. Their column comments now read *"PROVENANCE ONLY since ADR 0038: … It is NOT part of
> `group_key`."* Nothing else in this section changed.

### 5.3 What it deletes

With the key derived from the case's own labels, membership is *computed*, not recorded:

- Case → Group becomes **many-to-one**, like Case → Alert. Multi-membership disappears with
  `receiver`.
- `alert_group_members` collapses into `alert_cases.group_id`, a column that already exists.
  **Done in 00051.** The live membership of a generation is now
  `alert_cases WHERE group_id = $1 AND ended_at IS NULL`; there is no join table and no
  `left_at`.
- `group.member_joined` and `group.member_left` become redundant — implied by `case.opened` and
  `case.resolved`/`case.expired` respectively. They were facts about the **case** phrased as if
  the group were the actor. **Retired in 00051**, and — unlike the eight renamed `occurrence.*`
  types — they stay on the contract as themselves, because they name a fact that stopped
  existing rather than the same fact under a new word.
- `group.opened`, `group.closed`, `group.storm_started`, `group.storm_ended` survive. Those are
  genuinely facts about the group.

**A live defect this exposed.** `Leave` was implemented at three layers — `member.go:122`,
`service.go:393`, `ports.go:51` — emitted `group.member_left` at `service.go:406`, and **had no
production caller**. So `left_at` was never set, every "current members" read matched everything
that had ever joined, `gm_current_idx` narrowed nothing, the point-in-time replay was monotonic
by construction, and an alert that resolved and re-fired inside one generation appeared twice.
It went unnoticed because group close is driven by `last_activity_at`, not by membership.
`Leave` was deleted with the table it wrote to: **there is no membership verb any more**, at any
layer. An episode is in a generation because its own `group_id` says so, and it is live because
its `ended_at` is NULL.

> **Not adopted.** This document also argued the close condition should change with the rest —
> **close on "no live case with this split key"** rather than on an activity timestamp, so a
> generation cannot close over a live case. **It was deliberately left activity-driven.** The
> close sweep still selects `state = 'open' AND last_activity_at < $2`. The membership defect
> that motivated the change is gone on its own (there is no stale `left_at` to be wrong about),
> so the remaining question — whether a quiet generation with a live case in it should be
> allowed to close — is a behaviour change to be argued separately, not a side effect of
> deleting a join table.

---

## 6. The Quiet tab

`alerts.snoozed_until` existed to serve a base-table filter predicate and a badge. Both
disappeared when snoozed alerts got their own tab, and the column went with them in 00048.

```
  main tab    unsnoozed only
                LEFT JOIN alert_snoozes … WHERE s.alert_id IS NULL
                streams: the build side is only ACTIVE snoozes, a tiny relation,
                so keyset order is preserved and LIMIT still stops early
  Quiet tab   snoozed only
                drives FROM alert_snoozes into alerts by primary key — faster
                than today's scan of alerts_snooze_idx
                gets the same three group-by axes
```

One index to add: `alert_snoozes (org_id, snoozed_until) WHERE ended_at IS NULL`. Neither
existing index leads with `org_id` for this question. **This is the one place the plan was
wrong:** that index already existed. 00022 wrote it as `alert_snoozes_active_org_idx` for
`GET /api/v1/snoozes`, for precisely the reason it was wanted again here, so 00048 widened its
`COMMENT` instead of adding a fourth index.

**This reverses a stated principle** — `filters.ts:78`: *"hiding snoozed alerts from the default
list is how an incident is lost."* The reversal is defensible and must be recorded next to that
comment: a snooze badge on row 400 of a scrolling list is already invisible, whereas a tab
labelled **Quiet (12 · 2 firing)** is not, and unlike the badge it makes the total legible at a
glance. **The badge must carry the worst state inside it**, or the reversal is not safe.

The Quiet tab is also the safety surface the 30-day maximum requires: without a list of what you
are currently not being told, a snooze becomes permanent by forgetfulness. The query already
exists — `alert_snoozes_expiry_idx`, minus the "has run out" predicate.

---

## 7. Who may read what

`notification` read snooze state from `alerts.snoozed_until` until 00048
(`snapshot.go:110, :120, :129, :505`). That was a correctness read on the hot path expressed as a
string literal in SQL, with no import and no compiler error to break.

It now reads `alert_snoozes`, which is both cheap and better:

- The active-snooze partial index is a tiny relation; a hash join costs approximately nothing
  whether the group holds five members or five thousand.
- The coupling narrows from a wide, hot, frequently-changing table to a single-concept one.
- It unlocks the explanation `events.go:171` already says is needed — *"an operator who
  unsnoozes and still hears nothing needs to already know why"* — because the authoritative row
  carries `snoozed_by`, `note` and `ended_reason`, and a bare timestamp does not.

The same applies to `a.ack_state` at `snapshot.go:110/:129` feeding `view.go:359`: the group
card's per-member ack comes from the member's own case, since 00049. There is nothing to join
through — after 00051 the members of a generation *are* its cases (`alert_cases.group_id`,
`ended_at IS NULL`), so the member row **is** a case and carries `ack_state` itself.

---

## 8. Rejected alternatives, recorded

| considered | rejected because |
|---|---|
| Per-namespace grouping tuning | third `Origin` tier; and the unit, not the scope, is the problem |
| oto opaque to AM's grouping entirely | AM's `group_by`/`group_wait` are doing free noise reduction the product's promise rests on; going opaque before a grouping layer exists ships a regression |
| Merging across AM envelopes | needs alerts that have not arrived; re-implements `group_wait` inside oto |
| A generic per-alert settings/"rule" table | `rule` is taken; and a generic table is the "it's just one nullable column" vector ADR 0013 names. A table named for one verb refuses the second one structurally |
| A narrow `alert_active_snooze` mapping table | same joins, one more table, two more write paths; a fourth shape for one of three sibling axes |
| Dropping `alerts.state` too | it is a filter and leads four indexes; filtering on a joined case breaks keyset pagination |
| Two event tables (`AlertCaseEvents` + `AlertEvents`) | loses the total order that is the point of one append-only log; duplicates partitioning; weakens `alert_event_keys`, which is deliberately unpartitioned so the guarantee is global |
| Not creating a Case for a snoozed alert | four invariants at once — see §4.2 |

---

## 9. What is *not* changing

- `Alert` remains the identity of a label set and is **not** a projection of anything upstream.
  Alertmanager has no cross-time identity concept; `alert_key` is oto's invention.
- `alert_events` remains one table, multi-subject via nullable FKs and namespaced types.
- Retention nesting is unchanged: `ingest_batches` 30 days ⊂ `alert_events` 13 months ⊂
  `alerts`/cases forever. Each layer is independently authoritative — which is why the
  projections are written in the same transaction as the truth rather than derived from it.
- Slack needs no snooze card; suppression already records itself readably.

### 9.1 On replay

The upstream half of an Alert is replayable from stored observations within the raw-retention
window, subject to two documented supersession refusals (`replay.go`: `LimbOvertaken`,
`LimbClosed`). The human half never is. Now that `ack_state` (00049) and `snoozed_until` (00048)
have left `alerts`, the projection is upstream-derived with no human column — but that is a
property of the table, not a claim that the product's state can be rebuilt from bytes.

---

## 10. What was filed

Six tickets in `git bug`:

| id | title | labels |
|---|---|---|
| `dc4d731` | The authoritative entity is AlertOccurrence and every surface leads with Alert | high · contract |
| `8e54b18` | `alerts.ack_state` claims an alert is acknowledged when the firing it referred to has closed | medium · contract |
| `9318a6e` | The notification path decides whether to be quiet by reading a denormalised timestamp, not `alert_snoozes` | high · notification |
| `bc691fa` | The Slack thread's identity is derived from a grouping oto does not control and cannot reproduce | high · contract |
| `fe73f9a` | `Leave` is implemented at three layers, emits an event, and is called from nowhere | medium · ui |
| `33665d1` | The roll-up's planner note reports missing indexes that migration 00021 wrote | low · docs |

`8e54b18` and `9318a6e` are independent of each other and of `dc4d731`. `fe73f9a` depends on
`bc691fa`. `33665d1` is unrelated to the rest and was found on the way past.

---

## 11. Open items

- **The Case-list acked count grouped by namespace** needs `alerts.namespace`, so it joins
  `alert_cases → alerts`. Decide whether that facet is worth the join or whether the case list
  groups by something case-native.
- **Ack across a re-fire inside the grace window.** A re-fire inside `refire_grace` reopens the
  *same* case. The ack should survive that — if the system has already decided this is the same
  problem returning, the person who said "I've got this" still has it. Confirmed in discussion;
  needs to be asserted by a test.
- **`service` as a fourth split axis.** Omitted at first. Add only if real threads are observed
  mixing unrelated services.
- **Validating the split key against production payloads.** Still open, and it is the sharpest
  one left: the key shipped ahead of its own validation. The tool exists — `tools/groupreplay`
  computes the derived key over stored `ingest_batches.payload` bodies (retained 30 days) and
  reports the resulting group-size distribution against the thread count Alertmanager's own
  grouping produced — but **it has only been run against synthetic fixtures.** Replay a real
  week before treating the three axes as settled.
