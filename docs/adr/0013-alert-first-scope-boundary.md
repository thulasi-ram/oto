# 0013 — oto is alert-first: the Flight Recorder Test defines the scope boundary

**Status:** Accepted · 2026-08-08
**Doctrine:** `docs/design/SCOPE-BOUNDARY.md` (the full test, 32 worked verdicts, the audit delta, the
slippery-slope map and the handoff contract). This ADR records the *decision*; that document is what
engineers cite when refusing a feature.
**Resolves:** red-team memo §7, open decision **3** — *"Is oto alert-first forever, or does it grow
incidents and on-call?"*

## Context

Alert management and incident management look alike from a distance. Both store timestamped facts about
bad things, both notify humans, both draw timelines, both have an "acknowledge" button. Every one of
oto's competitors sits on one side of a line neither of them names: keep.dev's product opinion is
*"alerts are raw material, incidents are the unit of work"*; incident.io, FireHydrant and Rootly start
where a human becomes responsible; PagerDuty starts where a human is woken up.

oto bets the opposite: **the signal is the unit of record, and the work happens elsewhere.** That is a
defensible product opinion, not a gap anyone overlooked — and defending it requires a boundary an
engineer can apply in thirty seconds, because the boundary will be tested one small pull request at a
time. It is never "let's build incident management"; it is always *"it's just one nullable column"*.

Three specific pressures made this urgent now, before any code exists:

1. `SPEC.md` §A.1 **bans the word `incident`**, while §I.1 lists a module named `incidents` as
   DEFERRED-POST-V1. The spec contradicts itself, and the contradiction is a propped-open door.
2. `escalate_after_s`, `escalation.check` and `Reason='escalation'` already exist. They are defensible
   as specified — one reminder, same channel — but "escalation" is PagerDuty's word for a ladder, and
   the schema is one array literal away from being one.
3. `oncall` and `analytics` (charter: *"MTTR/MTTA rollups"*) are classified **DEFERRED-POST-V1**, which
   reads as *not yet*. For these two the answer is *not ever*, and the classification says otherwise.

R8 already answers a narrow version of this question (no per-person metrics). It needs a general form.

## Decision

**oto is alert-first permanently.** The boundary is the **Flight Recorder Test**:

> **FR-1.** Complete the sentence *"this row is a fact about ______."*
> A **signal** (Alert, Occurrence, Group, RuleSnapshot, Notification, Delivery, Source, or an aggregate
> over those) → **IN**. A **person, team, rota, responsibility, response effort, ticket or
> customer-facing statement** → **OUT**.
> Human actions are IN only as timestamped annotations on a signal's timeline, carrying an actor as
> *metadata*. In one line: **actor, never subject.**

Three secondary heuristics, applied in order when FR-1 does not decide cleanly:

- **H-1 Obligation.** Does it create, route to, or discharge an obligation on a named human? → OUT.
- **H-2 Orphan.** Delete every alert; does the artefact still mean something? → OUT. (Config is exempt.)
- **H-3 Write-path.** Does it change any system outside oto's DB and its channels? → **OUT of v1**, on
  safety grounds — a **separate axis** from FR-1, and the only one that is *earnable*.

FR-1 separates `acked_by = alice` (a fact about the occurrence) from `assigned_to = alice` (a fact about
Alice). Identical columns; opposite products. It also derives R8 for free.

Consequential rulings (delta list in SCOPE-BOUNDARY §5, applied by another agent):

- **`escalation` → `unacked_reminder`** throughout: reason enum, `notifications_reason_ck`,
  `escalate_after_s`, the openapi field, the worker and the package. Bound as **exactly one stage,
  forever, same `channel_ids`** — a scalar that must never become an array. `escalation` joins the §A.1
  banned words.
- **`mention_on_escalation` → `mention_on_reminder`**, restricted to usergroups and `!here`/`!channel`.
  Individual `<@U…>` mentions are dropped: a static list of individuals oto pings is a rota with the
  schedule hard-coded.
- **Module map:** `incidents` and `oncall` move from DEFERRED-POST-V1 to **PERMANENTLY OUT**; the
  legitimate part of `incidents` is renamed **`correlation`** (machine-derived groupings over signals,
  no human create endpoint, no human-set state). `analytics` loses MTTA from its charter entirely.
- **Column ban on `alerts` / `alert_occurrences` / `alert_groups`:** no `assigned_to`, `owner_id`,
  `watchers`, `incident_id`, `ticket_id`, `sla_due_at`, or any present-tense person reference.
  `acked_by` is past-tense attribution and is the only person reference permitted on a signal row.
- **No human-writable signal state.** There is no `POST /alerts/{id}/resolve`, ever. `state` is owned by
  Alertmanager and mirrored by oto (C2); `ack_state` is the only state axis a human may write. Currently
  true by omission — omission is not enforcement, so it becomes an explicit §E.1 principle.
- **`stats` and `alert_quality_daily` are KEPT unchanged** and gain one guard: no `time_to_ack`,
  `acked_seconds` or any column measuring the interval between a machine event and a human action.
- **AC-49:** a lint rule greps `internal/`, `web/src/` and `db/migrations/` for the banned vocabulary.
  Bans that are not mechanically enforced decay in a quarter — the same argument §I.3 makes about
  layering rules.

**The handoff is part of the decision, not an afterthought.** oto is an upstream producer of signal
facts and never a downstream consumer of incident state. It exposes durable identity (`alert_key`,
`group_key` — stable across `alertmanager.yml` edits), the generic `oto.notification.v1` webhook
envelope, the full read API and SSE with durable resume, stable deep links, and rule provenance at fire
time — the one artefact no incident platform has and all of them want at postmortem. Exactly two inbound
human verbs exist: **acknowledge** and **comment**. An incident tool marking its incident resolved MUST
NOT resolve oto's alert.

**PagerDuty and incident.io are `Channel` providers, not scope changes.** A destination for a fact about
a signal is IN by FR-1; R5 defers the implementation, it does not exclude it. **oto pages nobody; oto
tells the thing that pages.**

## Consequences

- A future engineer can rule on a feature request in thirty seconds and cite a clause number. The
  refusal is a design property, not a taste argument.
- Assignment, rotas, paging, incident objects, status pages, postmortems, war rooms, MTTA/SLA reporting,
  human-set severity, triage verbs and manual resolve are permanently out. So is reading Slack
  conversation or ticket state back into oto.
- The buyer is capped: oto can never be the only tool a team purchases. This is stated out loud and
  turned into a coexistence pitch rather than hidden (memo §5e).
- Enterprise procurement will ask for MTTA and be told the metric is unrepresentable by construction.
  That cost lands on sales, not on the engineers who use the product daily.
- Ephemeral presence ("3 people are viewing this alert", derived from live SSE connections, never
  persisted) is the sanctioned answer to "who's on it" — it delivers the human value with zero stored
  facts about people.
- **One dissent is recorded rather than suppressed: `snooze` is missing and that is a defect.** It is not
  deferred, not refused, not discussed — merely absent, which is worse, because absence is not a decision
  anyone can revisit. Suppressing *oto's own* notifications for an `alert_key` until T passes FR-1, H-1,
  H-2 and H-3 cleanly; it is nearer to `channels.verbosity` than to a silence, and it is the manual
  sibling of the flap and storm damping the spec already ships as visible states. Without it the only
  quiet button a user has is muting the Slack channel — which also hides the real incident, the failure
  mode the memo calls fatal. A product whose ethic is "default to quiet" that ships no quiet button is
  inconsistent with itself. SCOPE-BOUNDARY §8 argues it belongs in **v1**.

## Alternatives rejected

- **"Stay alert-first" as a stated intention, without a mechanical test.** This is the status quo and it
  is exactly what fails: intentions do not survive twenty individually reasonable pull requests. The
  spec already demonstrates the decay — a banned word and a module bearing that name coexist in one
  binding document.
- **Draw the line at "no on-call and no paging."** Too far out. It permits assignment, incident objects,
  ticket sync and MTTA reporting, all of which cross the line long before telephony does, and each of
  which drags paging in behind it.
- **Draw the line at "read-only, no writes anywhere."** Wrong axis. That is H-3, a *safety* rule with its
  own expiry date (R3 is earnable). Conflating it with FR-1 would either permanently forbid snooze, which
  is legitimately in scope, or make the incident-management refusals look negotiable.
- **Grow into the archived Grafana OnCall vacuum** (memo §1, the largest named market opening). It is
  real, and it is refused deliberately: it drags in telephony vendors, 24/7 reliability obligations and
  compliance that a small self-hosted project cannot meet, and it would put oto head-on against
  PagerDuty instead of alongside it. The vacuum oto fills is the *self-hosted OSS alert-history and
  routing UI*, which is the other half of what OnCall's archival left behind.
