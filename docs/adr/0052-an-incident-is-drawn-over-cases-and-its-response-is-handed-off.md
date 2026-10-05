# 0052 — An Incident is drawn over Cases, and its response is handed off

**Status:** Accepted · 2026-10-02 — settled in a design session with the owner, who authorised
building it the same day; Accepted when the delta list at the end was applied (git-bug `ee9f306`).
§4's at-most-one-Incident clause is still flagged for the owner's review, and Accepting the ADR does
not rule on it.
**Supersedes in part:** [0013](0013-alert-first-scope-boundary.md) — its refusal of incident objects
and its classification of `incidents` as PERMANENTLY OUT. 0013's handoff contract is **not**
superseded; this ADR is built on it. SCOPE-BOUNDARY **SS-3** and verdict **#33**.
**Amends:** [0045](0045-a-case-is-a-conversation-and-a-thread-per-alert-is-accepted.md) — "N alerts,
N conversations" gains one exception.
**Relates to:** [0039](0039-group-close-stays-activity-driven.md) (a grace window returns, for a
different entity), [0042](0042-storm-damping-is-removed.md) §3 and
[0044](0044-a-count-condition-is-a-silence-the-operator-asked-for.md) (the authority test every
grouping here passes), [0053](0053-an-investigator-reads-proposes-and-never-decides-delivery.md),
[0054](0054-a-remedy-earns-the-write-path.md).

## Context

SS-3 refused incident objects partly on the ground that *"`AlertGroup` is **already** the
multi-alert container."* Migration `00069` deleted `alert_groups`, and ADR 0045 accepted that a
storm of N alerts opens N conversations. oto now has **no object that spans more than one alert**,
so the pressure SS-3 described — forty alerts, and people wanting one thing to talk about — is
stronger than when SS-3 was written. An Investigator (0053) adds a second pressure: it needs the
whole storm to say anything useful, not forty fragments of it.

What SS-3 refused is still refused. The owner ruled out every part of the **response** — a lead, a
status such as "mitigated", human-set severity, comms, the write-up — on the ground that better
platforms already do it. What is admitted is the **grouping**, and the **handoff** of the response
to the platform that manages it.

## Decision

### 1. An Incident is a set of one or more Cases, drawn as one story

An Alert has Cases; an Incident spans Cases. The noun is `Incident` because that is what operators
will call it whatever the glossary says (`Arc` was considered and rejected for exactly that reason).

### 2. Two authors, and a model is not one of them

- A **Correlator** — matchers over Cases, optionally a count over a window — written by an operator.
  "Any `severity=critical` Case" draws one-Case Incidents; "≥5 Cases in one `(cluster, namespace)`
  within 10 minutes" draws many-Case ones.
- A **human**, recorded as actor metadata.
- The Investigator only **proposes** membership, as a Suggestion. Every Incident therefore has an
  answer to "why is this an Incident?" that is a rule someone wrote or a person who decided.

### 3. Inside oto, an Incident's state is read off its signals

An Incident is **active** while any member Case is open and **quiet** otherwise. No human writes it.
"Quiet but not fixed" and "fixed but still noisy" are facts about the response and live only in the
external tool. The two may disagree, and that is correct: they describe different things.

### 4. Membership over time

- A later Case matching the Correlator joins while the Incident is active, and within the
  Correlator's operator-written `quiet_grace` after it goes quiet; past that, it draws a new
  Incident. A flapping alert therefore does not draw ten Incidents overnight.
- A **human-drawn** Incident never grows by itself.
- A human may add or remove Cases on any Incident; a Case a human removed is never re-added by a
  Correlator.
- **A Case belongs to at most one Incident.** When several Correlators match one Case, the first in
  the operator's Correlator order draws it; a human may move it to another Incident. Two
  memberships would mean two conversations to post a Case's facts into and two external incidents
  to send them to, with no rule to choose between them. ⚠️ *Decided while the owner was away, to
  unblock implementation; flagged for review.*

`group_close_delay_s` and `refire_grace_s` left with `AlertGroup` (`00069`, `00071`) because their
**entity** was wrong — a grouping no operator could configure — not because a grace window is. This
one is a number an operator wrote, which is the test 0044 set.

### 5. Declared outbound, facts only

- **Declaring is a notification.** `subject_kinds` gains `incident`; "every Incident goes to
  incident.io" is one catch-all policy routed to an incident-tool Channel. Retries, idempotency,
  delivery audit and drills come with it. An org with no such policy has Incidents in oto and sends
  nothing.
- The **outbound mapping** binds an Incident to `(destination, external incident id)`, the way a
  ChannelThread binds a Conversation to a Slack root, so later facts become updates on the same
  external incident.
- **The provider is generic** (owner's ruling, 2026-10-02). No vendor-specific incident provider
  ships. The generic webhook's `incident.id` is stable for the Incident's life and is the
  de-duplication key a receiver keys on, so **oto's own key is the mapping**. If a receiver's 2xx
  response carries `external_url` / `external_id`, oto records them once as the receipt of a
  delivery — the way it records a Slack `ts` — and shows the link; that is not reading the incident
  back. Severity, where a tool needs one, is the receiver's mapping from the member alerts' own
  labels, never a value oto invents. Recipes for specific tools are documentation, not code.
- oto sends **facts, never commands**: drawn, member added or removed, quiet, active again, new
  Finding, and Remedy transitions (0054). It **never** sends PagerDuty's `resolve` or any close or
  status change. An org that wants auto-resolve writes it in its own tool, keyed on oto's metadata.
  This is 0013's rule run the other way: the incident tool does not resolve oto's alert, and oto
  does not resolve the incident tool's incident. ⚠️ *Narrowed by
  [0055](0055-an-incident-tool-integration-is-data-or-a-bridge-never-code-in-oto.md) §4: oto's own
  code and oto's mapping catalog never send a command, but an operator-written payload mapping
  on a webhook Connection may turn a fact into one — the operator's rule, not oto's.*
- Every fact carries a per-Incident **`sequence`** — `1` for `drawn`, allocated in the transaction that records the fact, reused on its retries — so a receiver orders facts that arrive out of order (owner's ruling, 2026-10-04; `00093`).
- **Nothing is read back.** Two-way sync is not considered.

### 6. An Incident may be a conversation

Per Correlator, an operator may say its Incidents are conversations: `conversation_kind` gains
`incident`, and later facts about member Cases post into the Incident's thread instead of opening
their own. Nothing is held back to wait for a Correlator, and nothing already posted moves. This
narrows 0045 to *"N alerts, N conversations — unless an operator-written Correlator says they are
one story."* It passes 0042 §3 where `AlertGroup` did not: the decision about what shares a message
is one the org wrote.

## Consequences

- oto's counts and the incident tool's counts can differ: oto draws Incidents for analysis and
  conversation as well as for declaration. A narrowing policy decides which go out.
- An operator who connects auto-resolve to `quiet` reopens scenario 1 for themselves. The provider
  docs must say so, because auto-resolve is PagerDuty's default.

## Delta list (applied 2026-10-02, git-bug `ee9f306`)

- SCOPE-BOUNDARY: SS-3, verdict #33, and the PERMANENTLY-OUT entry for `incidents`.
- SPEC §I.1 / §I.1.1: `correlation` and `incidents` rows; `subject_kinds`, `conversation_kind`,
  `notifications_subjkind_ck`, `threads_subjkind_ck`, `policies_subjkinds_ck`.
- CONTEXT.md: the `incident` scope ban, §1's "permanently not an incident manager" (still true of
  the response — reword, do not delete), and the ⚠️ markers on **Incident**, **Correlator** and
  **Conversation**.
- `tools/lintvocab`: `incident_id` stays banned on signal rows; the bare word is admitted. *Applied
  as: the rule fires only where the column would sit on `alerts`, `alert_cases`, `notifications` or
  `notification_deliveries`, so an Incident's membership table keys on `incident_id` legally. The
  bare word was never in lintvocab's list — only in the prose bans and AC-49's grep, both amended.*
- SPEC §D.8's DDL (`subject_kinds`, `conversation_kind` and their four CHECKs) is amended by the
  migration that widens them, not ahead of it; §I.1 records the widening as part of the module.
- ADR 0013 and ADR 0045: a pointer to this ADR.
