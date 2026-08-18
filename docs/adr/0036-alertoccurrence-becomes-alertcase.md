# 0036 — AlertOccurrence becomes AlertCase, and `case` is argued against FR-1 by name

**Status:** Accepted · 2026-08-18
**Relates to:** [0003](0003-alert-occurrence-event-separation.md) (the three entities — this renames the
middle one and changes nothing about it), [0013](0013-alert-first-scope-boundary.md) (the boundary this
ADR argues against by name), [0024](0024-retention-defaults-and-cold-storage.md) (why the episode row
survives what the event stream does not)
**Amends:** SPEC §A (the `AlertOccurrence` row, `docs/design/SPEC.md:89`), §A.1, §B and §B.1, §D.4,
§I.1; SCOPE-BOUNDARY §5.1 and the §5.6 column ban, re-pointed at the renamed table
**Resolves:** git-bug `dc4d731` — *"The authoritative entity is AlertOccurrence and every surface leads
with Alert"*
**Design note:** `docs-site/.../design/case-and-grouping.md` §3, whose argument this ADR records so the
rename has something binding to cite.

## Context

oto's authoritative entity is named for its position in a data model. SPEC §B (`SPEC.md:134`) states it
outright — *"The authoritative machine runs on **AlertOccurrence**. `Alert.state` and `AlertGroup.state`
are projections"* — and SPEC:89 adds *"This is what you acknowledge and whose **firing duration** is
measured."* The schema agrees: `alert_occurrences` (`db/migrations/00007_alerts.sql:119-208`) carries
`ack_state`, `acked_by`, `acked_by_label`, `acked_at` and `ack_note` under four CHECKs —
`occ_ack_ck`, `occ_acklabel_ck`, `occ_ackorder_ck`, `occ_acknote_ck` (`:177-180`) — plus
`rule_snapshot_id`, `reopen_of`, `resolve_reason` and both clock pairs, while `alerts.ack_state` (`:42`)
is a bare enum with no actor and no timestamp. `occ_one_open_idx` (`:187`) guarantees at most one open
episode per alert and `occ_seq_uniq` (`:155`) makes `seq` gapless, so *"the 12th firing"* is answerable.

*Occurrence* is accurate and says nothing about what a person does with the thing. The product's
differentiator is the replayable firing episode — start, end, ack, outcome, rule snapshot — and it is
the entity a reader meets last, behind a word that describes storage rather than use. ADR 0003 opens
with *"'Alert' is overloaded to uselessness"* and splits three entities to fix it; §A.1 bans `alert`
used to mean an occurrence for the same reason. A name that does not carry its meaning decays into the
wrong one, which is the premise the whole vocabulary section rests on.

**Three facts about the ground truth, because the issue states each of them slightly wrong and a rename
executed off the issue would miss.**

1. **There is no Go identifier `AlertOccurrence`.** `AlertOccurrence` is the SPEC-level term. The Go
   type is `Occurrence` in `internal/alerts/domain/occurrence.go:24`, referred to across the codebase as
   `alerts.Occurrence`; the table is `alert_occurrences`. This ADR is about that symbol.
2. **There are eight `EventOccurrence*` constants, not ten** —
   `internal/alerts/domain/event.go:66, 68, 70, 72, 74, 76, 78, 80`: `occurrence.opened`, `.reopened`,
   `.suppressed`, `.unsuppressed`, `.resolved`, `.expired`, `.acknowledged`, `.unacknowledged`.
3. **`internal/notification/domain/idempotency.go` needs no rename.** `SubjectKind` is declared at
   `:16` and v1 has exactly one member, `SubjectAlertGroup` at `:19`. No occurrence-flavoured subject
   kind exists, so nothing there is touched by this decision.

The blocker is not the mechanical rename. It is that `case` sits adjacent to §A.1's scope-ban family
(`incident`, `triage`, `assignee`, `owner`, `responder`), and §A.1's own rule (`SPEC.md:123-124`) is
that such a word is *"presumed over the line until argued otherwise against **FR-1 by name**"*. `case`
is not on the list; it is close enough to it that claiming the exemption on a technicality would be the
first move of exactly the decay §A.1 exists to prevent. So the argument is made in full, as if the word
were listed.

## Decision

**`AlertOccurrence` becomes `AlertCase`.** Go `alerts.Occurrence` → `alerts.Case`; table
`alert_occurrences` → `alert_cases`; column `occurrence_id` → `case_id`; the eight `occurrence.*` event
types → `case.*`; dedupe keys read `case:{id}:opened`; `RollupOccurrence` → `RollupCase`. **The entity
itself is unchanged** — one contiguous firing episode of one Alert, `(alert_id, seq)`, gapless, at most
one open. No column is added, none is removed, no invariant moves. This is a rename and the argument
for permitting the word.

### 1. Why `case` passes, argued against FR-1 by name

> **FR-1.** Complete the sentence *"this row is a fact about ______."* A **signal** → IN. A **person,
> team, rota, responsibility, response effort, ticket or customer-facing statement** → OUT.
> **Actor, never subject.**

Filled in for an `alert_cases` row: **this row is a fact about one contiguous firing span of one
Alert.** The blank is a signal — an episode of a signal, which SCOPE-BOUNDARY §1 names in its own IN
list. It is not a response effort. No human creates one: a Case is minted by ingest or the reconciler at
T1/T7 and by nothing else, and there is no endpoint that creates one.

**The rename cannot change FR-1's verdict, and that is the strongest thing that can be said for it.**
FR-1 is a test on the *columns*, not on the *label*. The only person reference on the row is
`acked_by` / `acked_by_label` / `acked_at` / `ack_note` — past-tense attribution, already ruled KEEP by
SCOPE-BOUNDARY §5.1 as *"attribution as metadata on a signal row"*, and bound by `occ_ackorder_ck`
(`00007_alerts.sql:179`) to be later than the episode it annotates. The same columns pass or fail the
test whatever the table is called.

The three heuristics, in order:

- **H-1 Obligation.** A Case creates, routes to and discharges no obligation on a named human. Ack is a
  receipt on the episode; it suppresses nothing (`acked` is absent from the `SuppressedReason` set —
  acking means the unacked-reminder intent is never minted). Nobody is told they owe work.
- **H-2 Orphan — the decisive one.** Delete every alert oto has ever stored; does the artefact still
  mean anything? **No.** A Case *is* an alert's firing span: without the alert there is no `(alert_id,
  seq)`, no `started_at`, no episode. This is precisely where a ServiceNow or Zendesk case goes the other
  way — those survive the deletion of every alert, because they were never about the alerts. That
  divergence is the whole permission.
- **H-3 Write-path.** A Case writes nothing outside oto's own database.

### 2. The word's gravity, and the anti-caseload clause that answers it

The honest objection is not to the row, it is to the word. In Salesforce, Zendesk and ServiceNow a
*case* has an owner, a queue, a priority, a status a human sets and a resolution a human writes. That is
the gravity `case` carries, and gravity is how vocabulary drifts: the word arrives, then the concept,
then the column, then the rota. So the permission is granted **with limits, and the limits are the
binding part of this decision** — the same shape as ADR 0013's anti-rota clause.

1. **No human create endpoint, ever.** There is no `POST /cases`. A Case exists because an alert fired
   and for no other reason.
2. **One alert identity, one contiguous span, forever.** A Case never spans two `alert_key`s. The
   moment an object wants to, it is `correlation` (deferred) or an incident (permanently out).
3. **No human-writable `state`.** C2 and §B.2 are untouched: `state` is owned by Alertmanager and
   mirrored by oto. There is no `POST /cases/{id}/resolve`, exactly as there is no
   `POST /alerts/{id}/resolve`. `ack_state` remains the only state axis a human may write.
4. **SCOPE-BOUNDARY §5.6's column ban applies verbatim to `alert_cases`**, and the DDL comment carrying
   it moves with the table: no `assigned_to`, `owner_id`, `owner_team_id`, `watchers`, `subscriber_ids`,
   `incident_id`, `ticket_id`, `sla_due_at`, no human-set `priority`, and no `case_status` distinct from
   `state`. `acked_by` stays the only person reference permitted on the row.
5. **No caseload vocabulary in UI copy or docs.** No "queue", no "my cases", no "open cases assigned
   to", no "resolve case", no case count rendered as a workload. The two inbound human verbs stay
   **acknowledge** and **comment** (SCOPE-BOUNDARY §7, H-8).
6. **No column named `case`.** It is reserved in both Go and SQL. `AlertCase`, `alert_cases` and
   `case_id` are unaffected by that; a bare `case` column or field is not permitted at any point.

### 3. The Case / correlation line

`correlation` (`SPEC.md:3623`, DEFERRED-POST-V1) remains the name for anything tying **disjoint**
signals together, and this rename pulls none of it forward. The two are separated by cardinality, not by
tone:

| | **Case** — in scope, shipped | **correlation** — DEFERRED-POST-V1 | **incident** — permanently out |
|---|---|---|---|
| subject | one signal's firing span | many signals, one stated algorithm | a human response effort |
| alert identities | exactly one | many, disjoint | any, or none |
| created by | ingest / reconciler, always | a stated algorithm, always | a human |
| state owned by | Alertmanager, mirrored | derived | humans |
| survives H-2? | **no** — dies with its alert | no | **yes** — and that is why it is out |

The discriminator between a Case and a correlation is *how many alert identities it spans* — one versus
many. The discriminator between either of them and an incident is H-2. A Case fails H-2 by construction,
which is what makes the word safe here and would not make it safe on any object that spans alerts.

## Consequences

- **Ack becomes Case-only.** `alerts.ack_state` (`00007_alerts.sql:42`) — a bare enum with no actor and
  no timestamp — is removed; `alert_cases` keeps the full ack quartet under its four renamed CHECKs. Ack
  is backward-looking, a receipt for a firing that happened, and its statement stops being true when a
  new episode begins: an alert-scoped ack means a March acknowledgement pre-acknowledges a September
  firing. Every proposed fix for that ("clear the ack on resolve") *is* case-scoping under another name.
  The work is git-bug `8e54b18`; this ADR is the reason it is not a judgement call.
- **Snooze stays Alert-scoped** in `alert_snoozes`, and this ADR is where that asymmetry is recorded
  rather than inferred. Snooze is forward-looking — a mute on notifications that have not happened — and
  its statement must survive across episodes. You can snooze a *resolved* alert, where there is no open
  Case to write it on, and a snooze set at 09:00 until tomorrow spans however many Cases fire in
  between. A field cannot live inside one of the several things it covers. The rule the two verbs share:
  **scope a human verb to the lifetime of the claim it makes.** A snoozed alert still opens a Case and
  is simply not notified about.
- **Eight event types move, and that is a §N contract amendment**, not an internal rename: `case.opened`,
  `.reopened`, `.suppressed`, `.unsuppressed`, `.resolved`, `.expired`, `.acknowledged`,
  `.unacknowledged`. §D.4.1's closed enum is the doorman and it must be walked through the front door.
- **`RollupOccurrence` → `RollupCase` changes a published wire value**, `"occurrence"` → `"case"`
  (`internal/notification/repository/rollup.go:37`), so `api/openapi/openapi.yaml` and the generated TS
  client and validators move with it. `RollupAlert` (`:35`) and `RollupGroup` (`:40`) are untouched.
- **`internal/notification/domain/idempotency.go` is not in the blast radius**, per Context (3).
- **`test/arch/sqltables_test.go` names `alert_occurrences` in four places** (`:135`, `:477`, `:491`,
  `:496`) and must name `alert_cases` after the rename, or the architecture test passes against a table
  that no longer exists.
- **`case` costs nothing at the vocabulary gate.** It matches no rule in `tools/lintvocab/main.go`'s
  `banned` list (`:98-123`) or `columns` list (`:127-134`), and it is not a §A.1 ambiguity ban — §A.1
  bans `alert` *used to mean* an occurrence, which this rename fixes rather than commits.
- **Adding `occurrence` to lintvocab costs something, and not where the issue says.** lintvocab's roots
  are `internal`, `web/src` and `db/migrations` (`main.go:79`) and do **not** include `docs/` — so no
  allowlist for `docs/` or for this ADR is needed or even expressible. What is needed is coverage for
  the migration *history*: `00007_alerts.sql` creates `alert_occurrences` in its **Up** section, which
  the automatic `+goose Down` exemption (`main.go:376-392`) does not reach. Those lines need
  `vocab:allow` markers with a stated reason, or `baseline.txt` entries, before the term can be banned.
- **The doc comment on `alerts.Occurrence` (`occurrence.go:15-18`) is stale independently of this
  rename** — it says the episode is *"what MTTR is measured over"*, and `MTTR` is banned by §A.1 and by
  `lintvocab` (`main.go:121`). It survives only because comments are stripped before matching
  (`main.go:29-38`). Fix it in the same pass; say *firing duration*.
- **ADR 0013 gains a cross-reference to this ADR**, per `dc4d731`'s closing condition. This ADR does not
  edit it.
- **The closing condition is SPEC §P-5's, reused verbatim** (`SPEC.md:5527`): after the rename,
  `occurrence` must not appear in any Go identifier, JSON field, column name or UI string. That
  formulation was written for the `escalation` rename and is the only one in the spec with a track
  record of finishing a rename.
- The ADR mirror under `docs-site/src/content/docs/adr/` is generated by `docs-site/scripts/sync-docs.mjs`
  and needs no hand-written copy.

## Alternatives rejected

- **`Episode`.** Accurate, and it is literally the definition word — SPEC:89 and ADR 0003 both define
  the entity as *"one contiguous firing episode"*. **Rejected by the product owner as too big:** it
  names any bounded stretch of time, carries no alert inside it, and reads as a system-wide noun rather
  than an alert-scoped one. It also makes the definition sentence circular, which costs the definition
  its explanatory power at the exact moment a new reader needs it.
- **`AlertRollup`.** Collides head-on with vocabulary already shipped: `DeliveryRollup`
  (`internal/notification/repository/rollup.go:43`) and its three subjects `RollupAlert`,
  `RollupOccurrence` and `RollupGroup` (`:35`, `:37`, `:40`). In this codebase `rollup` already means
  *aggregated delivery counts over a subject*. Naming the subject after its own aggregation makes
  `RollupAlertRollup` a reachable identifier and leaves no word for the aggregate.
- **`AmAlert` / `OtoAlert`.** Withdrawn rather than merely rejected, on two grounds. First, three
  entities named Alert is precisely the state ADR 0003 was written to escape — *"'Alert' is overloaded to
  uselessness"* — and §A.1 already bans `alert` used to mean an occurrence, so the proposal was banned by
  the list it was meant to satisfy. Second, and decisively, **the upstream-side noun is already taken and
  is not called `AmAlert`:** `alerts.Observation` exists in code
  (`internal/ingestion/service/process.go:213`) as the name for one normalised `alerts[]` element or one
  reconciler `gettableAlert`. Introducing `AmAlert` would have put a second name on a concept that
  already has one. `OtoAlert` additionally names a *set* after its members.
- **Leave it `Occurrence` and fix the surfaces instead** — lead with the entity in the UI, the API and
  the docs, without touching the word. This is the cheapest option and it is the status quo restated:
  every surface that today leads with Alert does so partly because the alternative noun explains
  nothing. The name is the surface an engineer meets first, and it is the one a Slack thread, a URL and
  a support conversation all inherit.
- **Argue that `case` needs no FR-1 defence because §A.1 does not list it.** True on the letter and
  corrosive in practice. §A.1's list is not the boundary; FR-1 is, and the list is its cached result. A
  word admitted because it was not yet written down is how the next word gets in.
