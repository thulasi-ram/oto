# oto — SCOPE BOUNDARY (the doctrine)

> **Status:** BINDING doctrine. Cite this document by clause number when refusing a feature.
> **Precedence:** `SPEC.md` > `domain-research.md` > **`SCOPE-BOUNDARY.md`** > `red-team-memo.md` > `architect-proposal.md`.
> Where this document identifies something in `SPEC.md` or `openapi.yaml` that crosses the line, §3 is a
> **delta list**. Another agent applies it. This document does not edit those files.
> **Companion ADR:** `docs/adr/0013-alert-first-scope-boundary.md`.

---

## 0. The question this document answers

> *"We should take care not to become an incident management platform. It's a thin line and we have to know what that thin line is."*

The line is thin because both kinds of product store timestamped facts about bad things, both notify
humans, and both draw timelines. They diverge on **what the record is a fact about**. That is the whole
answer, and everything below is machinery for applying it in thirty seconds.

**oto is a flight recorder.** A flight recorder records the aircraft. It does not fly the plane, roster
the crew, decide who is in command, or write the accident report. It is trusted precisely *because* it
does none of those things.

---

## 1. THE FLIGHT RECORDER TEST (primary — FR-1)

> **FR-1. Complete this sentence about the row the feature writes:**
> **"This row is a fact about ______."**
>
> - If the blank is a **signal** — an Alert, an AlertCase, an AlertGroup, a RuleSnapshot, a
>   Notification, a Delivery, an AlertSource, or an aggregate over those — the feature is **IN**.
> - If the blank is a **person, a team, a rota, a responsibility, a response effort, a meeting, a
>   ticket, or a customer-facing statement** — the feature is **OUT**.
>
> Human actions are IN **only** as timestamped annotations on a signal's timeline, carrying an actor
> as *metadata*. The moment a human becomes the **subject** of a row rather than the **actor** on one,
> the line has been crossed.

The test in one line, for a whiteboard:

> **Actor, never subject.**

### Why this is the right primary test

- It is the difference between `case.acked_by = alice` (a fact about the case: it was
  acknowledged, by whom) and `case.assigned_to = alice` (a fact about Alice: she owes work).
  Identical columns; opposite products.
- It derives R8 for free. A per-person response-time aggregate is a fact about a person → OUT, with no
  additional rule needed.
- It survives the hard cases. keep.dev's product opinion is *"alerts are raw material, incidents are the
  unit of work."* oto's opposite opinion is *"the signal is the unit of record, and the work happens
  elsewhere."* FR-1 is that opinion made mechanical.

---

## 2. Secondary heuristics

Apply in order only when FR-1 does not decide cleanly.

### H-1. The Obligation Heuristic

> Does the feature **create, track, route to, or discharge an obligation on a named human**?
> → **OUT.**

Rotas, assignment, escalation ladders whose stages target people, SLA timers, MTTA targets, "you have 5
minutes to respond", `@alice` pings chosen by oto. A notification policy that routes a fact to a
**destination** is IN. The same policy routing to a **person** is a one-entry rota and is OUT.

### H-2. The Orphan Heuristic

> **Delete every alert oto has ever stored. Does the artefact still have meaning?**
> → **OUT.** *(Configuration is exempt — config is not an artefact.)*

Postmortems, incident records, status pages, rotas, runbook libraries, war-room channels and ticket
queues all survive the deletion of every alert, because they were never about the alerts. Acks,
comments, cases, rule snapshots and delivery records all die with the alert, because they were
facts about it.

### H-3. The Write-Path Heuristic

> Does the feature **change the state of any system other than oto's own database and its outbound
> notification channels**?
> → **OUT of v1**, on safety grounds (R3), *independently of the incident-management line.*

This is a separate axis and MUST NOT be conflated with FR-1. A silence write path is not incident
management — it is a safety-critical world-changing action. Refuse it for the right reason, because the
two refusals have different expiry dates: **FR-1 refusals are permanent; H-3 refusals are earnable.**

---

## 3. The 30-second decision procedure

```
1. Name the row it writes. "This row is a fact about ___."
     person / team / rota / responsibility / response effort / ticket / public statement
        -> OUT. Stop. (FR-1)
     signal / rule / notification / delivery / source / aggregate over those
        -> continue.
2. Does it create, route to, or discharge an obligation on a named human?   -> OUT. (H-1)
3. Delete every alert. Does the artefact still mean something?
     (config is exempt)                                                     -> OUT. (H-2)
4. Does it write to any system outside oto's DB and its channels?           -> OUT OF V1. (H-3)
5. Does it require a new `alert_events.type`?
     -> §N amendment. The closed enum in SPEC §D.4.1 is the doorman.
6. Otherwise -> IN.
```

**Vocabulary is enforcement.** SPEC §A.1 already bans `incident`. This doctrine extends the ban list to:
`escalation policy`, `on-call`, `rota`, `schedule`, `assignee`, `owner`, `responder`, `triage`,
`postmortem`, `war room`, `SLA`, `MTTA`, `severity override`, `close`, `merge`. If a PR introduces one of
these words in a Go identifier, a table name, a JSON field or UI copy, it is presumed over the line until
argued otherwise against FR-1.

---

## 4. Verdicts on 32 candidate features

`IN` = alert-first, build it. `OUT` = incident management or otherwise over a line, refuse it.
`BORDERLINE` = decided by a specific construction; the deciding argument is the whole row.

| # | Feature | Verdict | Deciding argument |
|---|---|---|---|
| 1 | Acknowledge an alert | **IN** | A receipt on a case, past tense. Subject = the case; the human is the actor. |
| 2 | Un-acknowledge | **IN** | Same shape as ack, inverse. |
| 3 | Comment on an alert | **IN** | An immutable `comment.added` event on the signal's timeline. Subject = the alert. |
| 4 | Assign an alert to a person | **OUT** | Subject = a person's workload. Present-tense obligation (H-1). The clearest violation on the list. |
| 5 | Alert grouping (alertname/namespace/fingerprint) | **IN** | A view over signals; groups are machine-derived from Alertmanager, never human-created. |
| 6 | Incident objects spanning alerts | **OUT** | Subject = a human response effort. Survives deletion of every alert (H-2). `incident` is a banned word. |
| 7 | Severity escalation (louder after unacked) | **BORDERLINE → IN as ONE stage only** | One re-notification to the **same channel**, triggered by the signal's own unacked duration, is a fact about the signal. A **second stage**, or a stage targeting a person, is an escalation policy → OUT. See §5.2. |
| 8 | On-call rotas | **OUT** | Subject = people and time. Exists with zero alerts (H-2). |
| 9 | Paging / telephony (phone, SMS, push) | **OUT** | The transport of an obligation to a named human (H-1), plus 24/7 reliability and compliance oto cannot meet. |
| 10 | Status pages | **OUT** | Subject = a customer-facing statement about a service. |
| 11 | Postmortems | **OUT** | Subject = organisational learning. Outlives every alert it cites. |
| 12 | War-room channels | **OUT** | Subject = a conversation between humans. |
| 13 | SLA targets | **OUT** | An obligation on humans, expressed as a deadline (H-1). |
| 14 | MTTA reporting | **OUT** | Mean time to *acknowledge* is a measure of human response speed. Subject = people, however aggregated. R8. |
| 15 | MTTR reporting | **BORDERLINE → IN, renamed** | Mean *firing duration* of an alertname is a property of the signal — already `alert_quality_daily.total_firing_seconds`. IN as "firing duration". OUT the moment it is framed as a human-performance metric. Do not use the acronym. |
| 16 | Runbook links | **IN** | A URL derived from the alert's own `runbook_url` annotation. Subject = the alert. A runbook **library** (authoring, versioning, ownership) is OUT (H-2). |
| 17 | Silences — read (mirror) | **IN** | A fact about an upstream suppression that explains this signal's state. |
| 18 | Silences — write into Alertmanager | **OUT OF V1 (H-3, earnable)** | Not incident management. A world-changing safety-critical write. R3 stands; a future write path is a mirror-then-create, never a local create. |
| 19 | Snooze (suppress oto's *own* notifications until T) | **BORDERLINE → IN** | Subject = this signal's notification, stored only in oto, no cluster write, auto-expiring, visible as a state. Distinct from #18. **See §7 — I argue this belongs in v1 and its absence is the spec's one real mistake.** |
| 20 | "Who is looking at this" — persisted | **OUT** | A stored present-tense claim on a person is assignment wearing a friendlier hat. |
| 21 | "Who is looking at this" — ephemeral presence | **BORDERLINE → IN** | Derived from live SSE connections, never persisted, no obligation, disappears when the tab closes. This is the safe version of #20 and it is genuinely useful. |
| 22 | Slack thread replies from humans, mirrored **into** oto | **OUT** | Subject = a conversation. Also violates C9 (oto never reads Slack back). oto's comment mirror is one-way: oto → Slack. |
| 23 | Auto-remediation | **OUT (H-3)** | Robusta's turf. A cluster write path; changes oto's safety class by an order of magnitude. |
| 24 | Alert quality / noise reports | **IN** | Subject = an alertname per cluster per day. The counterweight feature: it argues for **deleting rules**. |
| 25 | Deploy correlation | **BORDERLINE → IN** | A deploy is a machine event. Correlating it with a signal is enrichment. Subject stays the alert. Becomes OUT if it grows "who deployed it" as a blame surface. |
| 26 | Custom workflows / automation rules | **OUT (H-3)** | A general-purpose action engine is an unbounded write path and a second product. |
| 27 | Ticket creation (Jira) | **BORDERLINE → IN as outbound-only** | Fire-and-forget over the generic `webhook` channel; the ticket URL lands as an **enrichment**, provenanced. OUT the moment oto reads ticket state back or lets it move an alert's state. |
| 28 | Timelines mixing human and machine events | **IN** | The signature view. Humans appear as `actor_kind='user'` — actor, never subject. |
| 29 | Notification policies routing to **channels** | **IN** | Subject = a signal→destination route. |
| 30 | Notification policies routing to **people** | **OUT** | A one-entry rota (H-1). Includes a static per-channel list of individuals to `@`-mention. |
| 31 | Alert ownership by team (a `team` **label** as a filter/route) | **BORDERLINE → IN** | If it is a label already on the alert, oto is reading the signal. **OUT** if oto stores a team registry and assigns alerts to it — that is a directory of humans (H-2). |
| 32 | Watchers / subscriptions | **BORDERLINE → OUT for v1** | "Tell me about alerts matching X" is a notification policy scoped to a person, i.e. a personal route to a human (H-1). The IN-shaped version is a **saved filter** (`views`, deferred) with no notification attached. |
| 33 | Multi-alert correlation | **BORDERLINE → IN later as `correlation`, never as `incidents`** | A derived grouping over signals passes FR-1. It becomes OUT the instant it acquires a human-set severity, a status, an owner, or a create endpoint. |
| 34 | Manual "resolve this alert" button | **OUT** | The signal's state is owned by Alertmanager and mirrored by oto. A human declaring a signal resolved is the exact lie C2/B.2 exist to prevent. **There is no `POST /alerts/{id}/resolve`, ever.** |
| 35 | Merge / close / dismiss alerts | **OUT** | Triage verbs. They make the alert a work item with a human-owned lifecycle. |
| 36 | Per-person response metrics | **OUT** | R8. Unrepresentable in the schema by construction. |
| 37 | Saved views / filters | **IN** (deferred) | Subject = a query. |
| 38 | Delivery audit + manual retry | **IN** | Subject = a delivery. oto's silence must never be indistinguishable from "no alert". |
| 39 | Rule drift diff between cases | **IN** | Subject = a rule definition. The differentiator. |
| 40 | Maintenance windows (suppress notifications during a window) | **BORDERLINE → IN** | Same shape as #19: a fact about which signals notify, stored in oto, no cluster write. OUT if it becomes a calendar of human availability. |

---

## 5. Audit of what is already specified

Everything below crosses, brushes, or props open the line. Ruled `KEEP` / `CUT` / `RESHAPE`.
**This is a delta list. Another agent applies it to `SPEC.md` and `openapi.yaml`.**

### 5.1 Rulings — the acknowledgement and comment surface

| Element | Ruling | Detail |
|---|---|---|
| `ackCase`, `unackCase`, `ackAlertGroup` (openapi); `POST /cases/{id}/ack`, `/unack`, `/alert-groups/{id}/ack`; T9/T10 | **KEEP** | Ack is a receipt on a case, not a claim on a person. The orthogonal `ack_state` axis (B.1) and the openapi copy *"an acked alert is still firing… says 'a human has seen this', not 'this is over'"* are already exactly right. **Guard to add:** ack MUST NOT be presented in UI copy or docs as "take ownership", "I'm on it", or "assign to me". |
| `alert_cases.acked_by`, `acked_by_label`, `acked_at`, `ack_note` | **KEEP** | Attribution as metadata on a signal row. |
| `commentOnAlert`, `commentOnAlertGroup`, `comment.added`, T14 | **KEEP** | Immutable events on a signal timeline; the openapi already forbids edit and delete. **Guards to add:** (a) no `@mention` semantics that select a human recipient; (b) no comment threading or replies-to-comments — that is a conversation, and conversations are OUT; (c) the mirror is one-way oto→Slack (already implied by C9; state it). |
| `alert_events.actor_kind = 'user'`, `actor_id`, `actor_label` | **KEEP** | The literal encoding of *actor, never subject*. This is the schema-level proof that FR-1 holds. |
| `alert_events.type` closed enum (§D.4.1) + *"Adding a type requires a SPEC amendment"* | **KEEP — this is the single strongest existing enforcement mechanism** | Every incident-management feature needs a new event type. Route all such requests through §N and refuse them there, citing FR-1. |

### 5.2 Rulings — escalation (the closest thing to a crossing already in the spec)

| Element | Ruling | Detail |
|---|---|---|
| `notification_policies.escalate_after_s` (D.8); `escalation.check` worker (G.3, G.9); `internal/notification/worker/escalate`; `Reason='escalation'` in the `notifications_reason_ck` CHECK; `escalate_after_seconds` in openapi (×3); `escalation` in the verbosity tables (§H.6) | **RESHAPE** | The mechanism as specified is defensible — one re-notification, to the **same channel**, at most once per group generation, driven by the signal's own unacked duration. The **word** is not. `escalation` is the vocabulary of PagerDuty's escalation policies and it is one field from becoming one. **Becomes:** rename the reason `escalation` → `unacked_reminder` everywhere (enum, CHECK, verbosity tables, Block Kit reply mapping); rename `escalate_after_s` → `unacked_reminder_after_s` and `escalate_after_seconds` → `unacked_reminder_after_seconds`; rename the worker `escalation.check` → `notify.unacked_reminder` and the package `worker/escalate` → `worker/reminder`. **Add a binding clause to §G.9:** *"There is exactly ONE stage, forever. `unacked_reminder_after_s` is a scalar and MUST NEVER become an array, a ladder, or a list of stages. It MUST NEVER acquire a target other than the policy's existing `channel_ids`. A second stage is an escalation policy and is permanently OUT (SCOPE-BOUNDARY §4.7)."* Add `escalation` to the §A.1 banned-word list. |
| `mention_on_escalation` in the Slack channel config JSON Schema (§L.5.1) — pattern permits `<@[UW][A-Z0-9]+>` | **RESHAPE** | A static per-channel list of individuals for oto to ping is a degenerate on-call rota: oto is choosing which named human is responsible (H-1). **Becomes:** rename to `mention_on_reminder`; restrict the item pattern to `^(<!subteam\^S[A-Z0-9]+>|!here|!channel)$` — usergroups and channel-wide only. **Drop individual `<@U…>` mentions.** Rationale: mentioning a usergroup or a channel addresses an *audience*; mentioning an individual names a *responder*. This is the sharpest available line and it is a one-regex change. (Cost acknowledged in §7.) |

### 5.3 Rulings — notification policies

| Element | Ruling | Detail |
|---|---|---|
| `notification_policies` table, `NotificationPolicy` domain type, `listNotificationPolicies`/`createNotificationPolicy`/`updateNotificationPolicy`/`deleteNotificationPolicy`/`previewNotificationPolicy` | **KEEP** | Matchers → channels → reasons is signal→destination routing. IN by FR-1. `previewNotificationPolicy` (dry run, no send) is a *safety* feature and one of the best things in the spec. |
| `notification_policies.channel_ids UUID[]` | **KEEP + guard** | **Add a binding clause:** *"`channel_ids` references `channels` and nothing else. `notification_policies` MUST NEVER gain `user_ids`, `team_ids`, `schedule_id`, `rotation`, `time_of_day`, `days_of_week`, or a second escalation stage. A policy routes to a destination; a policy that routes to a person is a rota."* |
| `notification_policies.throttle` | **KEEP** | Rate of signal→channel. IN. |

### 5.4 Rulings — the `stats` module

| Element | Ruling | Detail |
|---|---|---|
| `stats` module (PERIPHERAL), `stats.rollup` job, `internal/stats/*` | **KEEP** | Alert hygiene is the ethical counterweight (memo §6.1). Keeping it PERIPHERAL is correct. |
| `alert_quality_daily` — keyed `(org_id, day, cluster_key, alertname)`, no user column | **KEEP** | Subject = an alertname. The comment *"per-person response metrics are unrepresentable in this schema by construction"* is the doctrine already working. |
| `alert_quality_daily.acked_cases`; `getAlertQualityStats` sort key `ack_rate` | **KEEP** | Ack **rate per alertname** is a fact about the alert — *did anyone ever care about this rule?* — not about a person. The openapi framing (*"finds the ones nobody ever acknowledges"*) is correct and should not be softened. |
| `alert_quality_daily.total_firing_seconds` | **KEEP** | Signal duration. This is the only legitimate "MTTR". |
| Absence of any ack-latency column | **KEEP — and make it explicit** | **Add a binding clause to §D.10:** *"`alert_quality_daily` MUST NEVER gain `time_to_ack_seconds`, `acked_seconds`, `ack_latency_p50`, or any column measuring the interval between a machine event and a human action. That interval is a measure of people (SCOPE-BOUNDARY §4.14)."* |
| `getStatsOverview` / `StatsOverviewDTO` (open/firing/acked counts, delivery health, source health) | **KEEP** | Counts of signals and of oto's own health. |
| openapi prose: `CaseDTO` description and the glossary entry — *"This is what you acknowledge and what you time for **MTTR**"* (openapi.yaml ~L28, ~L4722) | **RESHAPE** | Replace "what you time for MTTR" with *"whose **firing duration** is measured"*. The acronym imports incident-management vocabulary into the glossary, which is where vocabulary leaks start. |

### 5.5 Rulings — the module map (§I.1), where the doors are propped open

| Element | Ruling | Detail |
|---|---|---|
| `incidents` — DEFERRED-POST-V1, *"Higher-level correlation over groups"* | **RESHAPE** | A module named `incidents` in a binding module map, in a spec whose §A.1 **bans the word `incident`**, is a self-contradiction and a propped-open door. **Becomes:** rename to `correlation`, restate the charter as *"machine-derived groupings over multiple signals, with a stated algorithm; no human create endpoint, no human-set severity, no status, no owner, no lifecycle beyond open/closed"*, and move it to a new **PERMANENTLY-OUT-OF-SCOPE** column entry named `incidents` reading *"Human-coordinated response objects. Permanently out (SCOPE-BOUNDARY §4.6). Hand off — §6."* |
| `oncall` — DEFERRED-POST-V1, *"Schedules, escalation policies, paging"* | **RESHAPE** | "DEFERRED-POST-V1" reads as *not yet*. This one is *not ever*. **Becomes:** reclassify to **PERMANENTLY OUT**, note reading *"Rotas, escalation policies, paging. Permanently out (SCOPE-BOUNDARY §4.8–4.9). oto hands off to PagerDuty / incident.io — §6."* |
| `analytics` — DEFERRED-POST-V1, *"MTTR/MTTA rollups beyond `stats`, subject to R8"* | **RESHAPE** | **CUT MTTA from the charter entirely** — it is not "subject to R8", it is *excluded by* R8, and leaving it in the sentence invites a future engineer to think it is negotiable. **Becomes:** *"Signal-duration and firing-frequency distributions beyond `stats`. Per-person and time-to-human-action metrics are permanently out (R8, SCOPE-BOUNDARY §4.14)."* |
| `audit` — DEFERRED-POST-V1, *"Separate audit log"* | **KEEP + narrow** | An audit log whose subject is *"what humans did"* is a person-centric record. **Add to the note:** *"Scoped to configuration changes (sources, channels, policies, tokens) — facts about oto's own configuration. Human actions on alerts stay in `alert_events` as actor metadata."* |
| `changefeed` — DEFERRED-POST-V1 (deploy correlation) | **KEEP** | A deploy is a machine event; correlating it is enrichment. Correctly deferred, correctly IN-scope-later. |
| `k8scontext`, `views` — DEFERRED-POST-V1 | **KEEP** | Machine facts and saved queries respectively. Both IN by FR-1. |
| `authz` — DEFERRED-POST-V1 | **KEEP** | Access control is not incident management. |

### 5.6 Rulings — schema doors that must be nailed shut

| Element | Ruling | Detail |
|---|---|---|
| `alerts`, `alert_cases`, `alert_groups` column sets | **KEEP + guard** | **Add a binding clause to §D:** *"`alerts`, `alert_cases` and `alert_groups` MUST NEVER gain `assigned_to`, `assignee_id`, `owner_id`, `owner_team_id`, `watchers`, `subscriber_ids`, `incident_id`, `ticket_id`, `status_page_id`, `priority` (human-set), `sla_due_at`, or any nullable person-reference with a present-tense meaning. `acked_by` is past-tense attribution and is the ONLY person reference permitted on a signal row."* This one clause is worth more than the rest of this document, because it is the door every slippery-slope feature in §6 needs to walk through. |
| `users`, `slack_identities` | **KEEP** | Identity for attribution and for Slack button authorship. Not a directory of responsibilities. |
| Absence of `POST /alerts/{id}/resolve` | **KEEP — and state it** | **Add to §E.1 principles:** *"There is no endpoint by which a human sets a signal's `state`. `state` is owned by Alertmanager and mirrored by oto (C2). `ack_state` is the only state axis a human may write."* Currently this is true by omission; omission is not enforcement. |
| Silences read-only (R3), `listSilences`, `getSilence`, Slack silence deep-link button | **KEEP** | Correct, and refused for the **right** reason. **Add a cross-reference:** R3 is an H-3 (safety) refusal, not an FR-1 (scope) refusal — it is *earnable*; the FR-1 refusals in this document are not. |
| §A.1 banned-word list | **RESHAPE** | Extend with: `escalation`, `escalation policy`, `on-call`, `rota`, `schedule`, `assignee`, `owner`, `responder`, `triage`, `postmortem`, `war room`, `SLA`, `MTTA`, `severity override`, `close`, `merge`, `dismiss`. |
| §J acceptance criteria | **KEEP** | Reviewed all 48. None crosses. AC-35 (*"no per-person data anywhere"*) is the doctrine already tested. **Add AC-49:** *"`grep -riE '(assign|assignee|on.?call|rota|escalation policy|postmortem|incident)' internal/ web/src/ db/migrations/` returns no hits outside `docs/` and the SCOPE-BOUNDARY cross-references. A lint rule enforces it."* Vocabulary bans that are not mechanically enforced decay in a quarter (I.3 already makes this argument about layering). |

### 5.7 Reviewed and clean

`grouping` (machine-derived, never human-created), `rules`, `enrichment`, `ingestion`, `streaming`,
`channels`, `notification` intents and deliveries, `silences`, `identity`, `platform`, and all 71
openapi operations not named above. No further crossings found.

---

## 6. The slippery-slope map

The five features most likely to be added later, each of which would irreversibly make oto an incident
management platform. Ranked by *probability of being added by accident*, not by size. The obvious ones —
status pages, postmortems, rotas — are not on this list, because nobody adds those by accident.

### SS-1. Assignment / "who owns this"

- **Why tempting:** `acked_by` already stores a person. The UI already has a name and a face to render.
  Adding "assign" looks like adding one nullable column to a table that already has a person in it.
- **What it drags in:** an assignee lifecycle (assign, reassign, unassign) → notification *to the
  assignee* (H-1 breached) → a "my alerts" view → workload balancing → a rota to pick the default
  assignee → you are now Opsgenie.
- **Door decision:** the §5.6 column ban. `acked_by` is **past-tense attribution**; no present-tense
  person reference may exist on a signal row. Write it into the DDL comment, not just a doc.
- **Can a version ship later without crossing?** **Yes — ephemeral presence.** "3 people are viewing this
  alert", derived live from open SSE connections, never persisted, no obligation, gone when the tab
  closes. It answers the real human need ("is anyone else on this?") with zero stored facts about people.
  Build **that** when the request arrives; it is a `streaming` feature, not an `alerts` one.

### SS-2. A second escalation stage

- **Why tempting:** `escalate_after_s` already exists and works. Making it `escalate_after_s[]` is
  fifteen minutes of work and users will ask within a month of GA.
- **What it drags in:** a stage ladder → per-stage targets → targets that are people → a rota to resolve
  the person → phone/SMS because Slack is not enough at 3am → telephony vendors, 24/7 reliability
  obligations, and compliance. This is the shortest path from oto to PagerDuty and it is four small PRs.
- **Door decision:** §5.2. Rename the concept to `unacked_reminder`, and bind the scalar: *one stage,
  forever, same channels*. The rename is the load-bearing part — as long as the word "escalation" lives
  in the schema, stage two is a natural reading of the word rather than a scope violation.
- **Can a version ship later without crossing?** **No.** Multi-stage escalation is precisely the handoff
  point. When a customer needs it, they need PagerDuty or incident.io, and oto's job is to be a clean
  event source for it (§6, H-5).

### SS-3. Incident objects that span alerts

- **Why tempting:** a storm produces forty alerts and users legitimately want *one thing* to talk about,
  link to, and refer back to. It feels like a grouping feature. It is not.
- **What it drags in:** an incident lifecycle owned by humans → human-set severity → status ("mitigated")
  → roles (commander, scribe) → comms → postmortems → status pages. Every one of these is a natural next
  step from the previous one, and the first one looks harmless.
- **Door decision:** `AlertGroup` is **already** the multi-alert container, and it is derived from
  Alertmanager's own grouping with a durable `group_key` (C3/C4) — **machine-derived, never
  human-created**. Slam: there is no endpoint that creates a grouping object from a human request, and
  §5.5 removes the `incidents` module name from the map.
- **Can a version ship later without crossing?** **Partially yes.** Machine-derived multi-alert
  **`correlation`** — a stated algorithm producing a derived grouping over signals — passes FR-1 and is
  the honest version of this request. What is out forever is a **human-created container with its own
  lifecycle**. The discriminator is one question: *can a human create one, and can they set its state?*

### SS-4. Silence-write, and its well-behaved sibling snooze

- **Why tempting:** the deep-link-to-Alertmanager button is a genuine papercut. A user staring at a
  flapping alert at 02:00 has **no affordance inside oto** to make it stop. This will be the single most
  requested feature, and every request is legitimate.
- **What it drags in (for silence-write):** oto becomes safety-critical — a bug suppresses a real
  incident. That pulls in RBAC (who may silence?), approval flows, an audit trail with legal weight, and
  a testing burden an order of magnitude larger.
- **Door decision:** R3 holds, and the design that keeps the door **open** is that `silences` is a mirror
  keyed by `source_silence_id` — a future write path is "create upstream, then mirror", never "create
  locally and reconcile". That ordering is already in the schema and must stay.
- **Can a version ship later without crossing?** **Yes, both, and they are different features.**
  Silence-write is an H-3 refusal and is *earnable* with an audit trail and a confirmation UX. **Snooze
  is not even a refusal — it is IN today** (§4.19), and I argue in §7 that its absence is the spec's one
  real mistake.

### SS-5. Ticketing (Jira / Linear / ServiceNow)

- **Why tempting:** every enterprise buyer asks in the first call, and it looks like a 200-line HTTP
  client. It is the cheapest-looking feature on this list.
- **What it drags in:** bidirectional sync ("show me the ticket status here") → status mapping → *closing
  the ticket closes the alert* → a `ticket_id` column on `alerts` → a work queue → oto is now the place
  work is tracked, which is the definition of the thing we are refusing to be.
- **Door decision:** **outbound-only, fire-and-forget, and the ticket URL is an `enrichment`** (subject =
  alert, provenanced, versioned) — **never** a first-class `alerts.ticket_id` column. A column implies a
  1:1 relationship oto is expected to maintain; an enrichment is a fact oto observed once.
- **Can a version ship later without crossing?** **Yes.** Ship it as (a) a `webhook` channel payload the
  customer points at their own automation, plus (b) an enrichment recording that a ticket URL was
  associated. What is permanently out is **reading ticket state back** and letting it move an alert.

---

## 7. The integration answer — the handoff contract

If oto is not an incident management platform, it must hand off to one gracefully. A boundary that
produces a dead end is a boundary users route around.

**The posture in one line:** *oto is an upstream producer of signal facts. It is never a downstream
consumer of incident state.*

### What oto exposes

| # | Surface | What a downstream gets |
|---|---|---|
| **H-1** | **Stable, published identity** — `alert_key` (`ak_…`), `group_key` (`gk_…`), case `(alert_id, seq)` | 128-bit, URL-safe, human-copyable, **durable across `alertmanager.yml` edits** (C3/C4). incident.io/PagerDuty/Jira can key their own records on these and the join survives config churn. This is worth more to them than anything else on the list. |
| **H-2** | **The generic `webhook` channel** — a stable `oto.notification.v1` JSON envelope | The primary handoff. Point it at PagerDuty Events API v2, incident.io, keep, or an automation runner. R5's rule that the webhook provider gets **no Slack-specific affordances** is exactly what makes it a clean integration surface rather than a degraded Slack. AC-30 already tests this. |
| **H-3** | **Incident tools as `Channel` providers** | A PagerDuty or incident.io provider is a *destination for a fact about a signal* — **IN by FR-1**. It is deferred by R5 (two impls in v1), not excluded. When the demand arrives, this is a provider, not a scope change. Say so out loud in the pitch: **oto pages nobody; oto tells the thing that pages.** |
| **H-4** | **The full read API** — `listAlerts`, `listAlertEvents`, `getAlertGroupTimeline`, `listAlertCases`, `listAlertNotifications`, `listDeliveries` | Keyset-paginated, PAT-authenticated, RFC 9457 errors. E.1's *"the UI has ZERO private endpoints"* is what makes the handoff possible at all: anything oto can show, a downstream can fetch. Postmortem tooling pulls the forensic record from here. |
| **H-5** | **`getAlertRuleHistory` / `getCaseRule`** — the rule `expr` and `for` **as they were at fire time**, plus version history | **This is oto's unique contribution to someone else's incident workflow.** No incident management platform has it, and it is the single most valuable artefact at postmortem time. Lead the integration story with this, not with "we also have a timeline". |
| **H-6** | **SSE with durable resume** — `streamEvents`, `Last-Event-ID`, 24h replay window | A downstream tails oto without polling and without losing events across its own restarts. |
| **H-7** | **Stable deep links** | Every alert, case and group has a permanent URL an incident record can cite. |
| **H-8** | **Exactly one inbound human verb: acknowledge** | `ackCase` with a PAT is how an incident tool tells oto *"a human has this"*. oto accepts the **receipt**; it does not manage the responder. `commentOnAlert` is the second and last inbound verb, and it is an annotation, not a state change. |

### What oto must NOT try to own

1. **Who is responsible.** Assignment, rotas, escalation chains beyond one same-channel reminder.
2. **The paging transport.** Phone, SMS, push. oto's last mile is a `Channel`, and a `Channel` is a place, not a person.
3. **The incident record.** Its human-set severity, its status, its roles, its comms, its lifecycle.
4. **Customer-facing status.** Status pages, subscriber notifications, uptime claims.
5. **Postmortems and action items.** oto supplies the evidence; it does not host the conclusion.
6. **Work tracking.** Ticket queues, assignment boards, "my alerts".
7. **Any inbound path that mutates a signal's state.** The hardest and most important line:
   **`Alert.state` is owned by Alertmanager, mirrored by oto, and writable by no human and no downstream
   tool.** An incident tool marking its incident "resolved" MUST NOT resolve oto's alert. If it did, oto
   would be reporting a human's belief about the world instead of the world, and the system-of-record
   claim — the entire product — would be dead (memo §3.5).

### The coexistence pitch, stated once so it can be reused

> Many teams will run oto **and** PagerDuty, or oto **and** incident.io. oto is the alert layer: what
> fired, what the rule said at the time, who was told, on which channel, and how it ended. They are the
> human layer: who is awake, who is responsible, and what the organisation does about it. oto is a
> better event source for them than Alertmanager is, because it carries rule provenance, deduplicated
> identity and a complete lifecycle. **Design for coexistence, not replacement** (memo §5e).

---

## 8. Where this line costs us — honestly

Drawing this line is not free, and pretending otherwise would make this document useless as a defence.

**Costs I accept:**

- **Assignment.** Users will ask, repeatedly. Refusing means oto cannot answer "who's on it" and teams
  coordinate in Slack instead. *This is an acceptable loss* — Slack is where they were going to do it
  anyway, and SS-1's ephemeral-presence version recovers most of the value.
- **No paging.** This caps the buyer: oto can never be the only thing a team purchases. The memo (§2.2)
  already says to state this out loud, and §7 turns it from a gap into a positioning statement. Real
  cost, correctly priced.
- **No MTTA / SLA reporting.** A manager *will* ask, and "we deliberately cannot compute that" is a
  harder sentence in an enterprise procurement call than in an engineering one. But R8 is the moral
  centre of this product and the refusal is a feature to the people who actually use it. Cost lands on
  sales, not on love.
- **No incident objects.** During a storm, users get an `AlertGroup` rather than an incident. For
  k8s-shaped grouping this is genuinely enough. Small, real cost at the very top end of severity.
- **`mention_on_reminder` restricted to usergroups and `!here`/`!channel` (§5.2).** This is the ruling in
  this document most likely to be overruled, and I want that on the record: users will want `@alice`, and
  the workaround (create a Slack usergroup of one) is petty. I hold the line because a static list of
  individuals oto pings *is* a rota with the schedule hard-coded — but if the owner overrules exactly one
  thing here, this is the one where the argument is closest.
- **Status pages, postmortems, war rooms.** Zero cost. Nobody expects these from an alert layer.

**Where I think the constraint is a mistake — one specific exclusion:**

> **Snooze is missing, and its absence is a real defect — not a scope decision.**

R3 correctly refuses a **write path into Alertmanager**. But the spec then leaves the user with *no
affordance at all*: the Slack card's silence button is a deep link out to Alertmanager, and there is
nothing in oto's own UI that stops oto's own noise. Search the spec for `snooze` and you find only
`river.JobSnooze`. It is not deferred, not refused, not discussed — it is simply absent, which is worse
than a stated refusal, because absence is not a decision anyone can defend or revisit.

**Snooze is unambiguously IN under this document's own test.** "Suppress *oto's* notifications for this
`alert_key` until T" is a fact about a signal's notification, stored only in oto's database, changing
nothing in the cluster, auto-expiring, attributed, and visible in the UI as a state — exactly the shape
of `is_flapping` and `storm_mode`, which the spec shipped at the time as damping mechanisms with
visible UI states (B.6, and see the note below). It passes FR-1 (subject = a signal), H-1 (no
obligation on anyone), H-2 (dies with the alert), and H-3 (no external write). It is nearer to
`channels.verbosity` than to a silence.

⚠️ **Neither comparison holds today, and the two went different ways.** `storm_mode` is DELETED:
storm damping is removed outright (ADR 0042) and migration `00059` dropped `alert_groups.storm_mode`,
`storm_since`, `groups_storm_ck` and `channels.storm_notice_at`. `alerts.is_flapping` still EXISTS as
a column and is RETIRED IN PLACE (ADR 0041 Amendment 1): no writer remains, every read is intact, and
it keeps the last value it was written — but its UI is gone, the flapping chip and the `?flapping=`
facet having been removed from the web app. The comparison this section draws is therefore historical;
the ruling it argues for is not, because snooze shipped as the first-class, auto-expiring, attributed
alert state described below.

The argument for shipping it in **v1, not v1.1**: the failure mode the memo identifies as fatal is users
muting the Slack channel (§3.4). Storm collapse and flap damping are automatic defences against oto's
noise; snooze is the *manual* one, and without it the only manual control a user has is muting the
channel — which also hides the real incident. **A product whose stated ethic is "default to quiet"
(memo §6.1) that gives the user no quiet button is inconsistent with itself.** Add it as a first-class,
auto-expiring, visible alert state with an attributed `alert.snoozed` / `alert.unsnoozed` event pair, and
a `notifications.suppressed_reason = 'snoozed'`.

Everything else in the owner's constraint I would keep exactly as it is.

---

## 9. Amendment

This document is amended by the same procedure as `SPEC.md` (§N). A request to add a feature ruled `OUT`
in §4 requires an ADR that argues **against FR-1 by name** — not against a symptom, and not by
re-describing the feature in friendlier words. "It's just one column" is not an argument; §6 exists
because it is always just one column.
