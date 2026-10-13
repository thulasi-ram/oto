---
title: 0054 — A Remedy earns the write path
---
**Status:** Accepted · 2026-10-02 — settled in a design session with the owner, who authorised
building it the same day. **Depends on** the one `authz` grant in §4: no Remedy ships before it.
**Earns:** SCOPE-BOUNDARY **H-3** for cluster writes — verdict **#23** (auto-remediation) — on the
terms SS-4 set for any write path: an audit trail and a confirmation UX.
**Supersedes in part:** [0013](/oto/adr/0013-alert-first-scope-boundary/) FR-1's refusal of facts about a
**response effort**, for the one noun `Remedy`. Every other FR-1 refusal stands.
**Amends:** [0016](/oto/adr/0016-mcp-enrichment-no-firehose/) — a ToolServer may now expose write Tools;
oto still holds no cluster credential.
**Amended 2026-10-06 (owner ruling O3):** §3 ruling 1 — rule changes from the app, on a two-person
condition. See the amendment under §3.
**Relates to:** [0052](/oto/adr/0052-an-incident-is-drawn-over-cases-and-its-response-is-handed-off/),
[0053](/oto/adr/0053-an-investigator-reads-proposes-and-never-decides-delivery/).

## Context

The owner wants the Investigator to propose cluster fixes, have humans approve them in oto's UI or
Slack, and have oto execute them. 0052 hands the response to an external tool and 0053 keeps the
Investigator read-only. A Remedy is where both bend: a fix is the most consequential part of the
response, and it is executed from oto.

The owner chose breadth over a fixed set of operations: any write Tool and arbitrary commands, with
responsibility resting on the people who approve and on the permissions the ToolServer was granted.
This ADR is therefore mostly about the controls that make that breadth survivable.

## Decision

### 1. What a Remedy is

A change to a cluster that an Investigator proposes and oto executes only after a human approves it:
`proposed → approved → executed | failed`, or `declined`, or `expired`, each by a named actor. Only an
Investigator proposes (a human-proposed Remedy would make oto a shared kubectl console — a separate
product, addable later as a widening). Responsibility rests with the approvers and with the
ToolServer's permissions.

**A Remedy names the write Tool that would carry it out, or says that none can** (owner's ruling,
2026-10-02). The Investigator proposes what it judges the right change whether or not a configured
Tool can make it; a Remedy with no Tool says so plainly — *"no configured Tool can carry this
out"* — and **cannot be approved**. It can still be declined, and it expires like any other. Its
value is the Finding it stands on: a person can make the change by hand, or configure a Tool.

**As built (review judgment 2, 2026-10-05).** A run's Finding proposes **at most three** Remedies.
A Remedy has a seventh state, `executing` — the executor's claim between `approved` and its
outcome, recorded but never declared as a fact. A failed Remedy says why, from a closed set:
`tool_error` (the Tool answered that the call failed), `outcome_unknown` (it was or may have been
sent and no answer was recorded; a Remedy left `executing` past the Tool's longest call timeout plus
five minutes is swept to this), `tool_unavailable`, `arguments_changed` and `approvals_withdrawn`
(fewer standing grant holders than it needs at the moment of sending). The approval window is the
org tuning key `remedy_approval_window_s` — default 3600, 60 to 86400, an ordinary key any member
may change — and one window is used twice: from the proposal to the approval it needs, and from that
approval to execution.

### 2. Approval happens in oto; everything goes outbound

Approve and decline happen in oto's UI and in Slack. Every transition is posted to the declared
Incident as a fact (0052 §5). oto **cannot know** that someone in the incident tool said no — that
is the stated cost of outbound-only; saying no happens in oto. An approval shows the latest facts
sent outbound and expires if not executed within an operator-set time.

**Restated (owner's ruling F1, 2026-10-05): what "shows the latest facts sent outbound" means on each
surface.** In the UI, the approval screen lists the **last three facts sent outbound about the
Incident** the Remedy is declared to — what each said, when, and whether it landed — above the
approve and decline controls. In Slack nothing more is drawn: the Remedy's card is posted **in the
Incident's thread**, beneath those very facts, which is where a Slack approver already reads them.

**A Remedy on a Case no Incident holds is declared nowhere** (as 0052 §5 has it: no Incident, no
declared destination). It has no outbound fact and so no Slack card and no list of facts on its
approval screen; it is decided on the Case's Investigation panel, and its transitions are on its
record.

### 3. Risk: rules first, a model may only raise it

Operator-written rules over the command (verb, resource kind, namespace, reversibility) set the
baseline. A model may then move a Remedy from single to double approval, **never back**. Anything
the rules cannot parse — `sh -c`, pipes — is double approval. The risk model sees **only** the
command, its target and the rules' verdict, never the Investigation: logs are attacker-writable, and
a log line is the obvious injection path. The approval screen shows the exact command **first**,
above the Investigator's description of it.

**Two rulings on the built rules (owner, 2026-10-05; git-bug eb4f21b).** As first built (5ace8f3),
any org member could replace the rules over `PUT /api/v1/remedy-risk-rules`, and the risk model's
tokens were recorded on the Remedy but not budgeted.

> ⚠️ **Amended 2026-10-06 (owner ruling O3): ruling 1 below is refined, not reversed.** The Settings
> screen may now change the rules, but only by a **two-person change**: one member **proposes** it
> (`POST /api/v1/remedy-risk-rules/changes`, which writes no rule and changes no tier), and a
> **different** member **confirms** it (`…/changes/{id}/confirm`) or anyone **discards** it. Both
> proposing and confirming are browser-session only. The reasoning of ruling 1 stands: a rule that
> says one lets one grant holder approve alone, and rules only ever loosen, so no change is exempt.
> What changed is that "a second person" is now an alternative to "a shell on the host" — the same
> bar, met differently. The second person is enforced by a CHECK on the change's row
> (`remedy_risk_changes_two_people_ck`, migration 00111), not by the application alone; the proposed
> rules are frozen by a trigger so what a confirmer read is what applies; one pending change per org,
> superseded by a newer proposal or by `oto remedy-rules apply`; and the writer of
> `remedy_risk_rules` stays in `internal/app`, now in two places (`remedyrules.go` for the shell,
> `remedyrulechanges.go` for a confirmed change). `test/scope/remedy_risk_rules_routes_test.go` still
> walks the mounted router and holds the route set exactly: the read, propose, confirm, discard.
> `oto remedy-rules` stays, for scripts and for the operator with the shell.

1. **The rules are written from the host shell only, like an approval grant (§4).** A rule that
   says one lets one grant holder approve alone, so a member who could write one could write a
   one-approval rule and then approve alone — the loophole §4 closed for grants. The rules and the
   risk model are one YAML file applied by `oto remedy-rules apply --org SLUG -f rules.yaml`,
   atomically, in one transaction; a malformed file changes nothing and exits non-zero naming the
   problem (`oto remedy-rules show` prints what stands as a file that applies). The HTTP API and the
   Settings screen are **read-only** and say the rules are managed by `oto remedy-rules`. No route
   writes a rule, which a test over the mounted router asserts; the one writer is raw SQL in
   `internal/app`, beside the grant's.
2. **The risk model's tokens count against the org's daily token budget** (ADR 0053 §6), summed
   into the day's spend beside every Investigation's model turns. When the budget is spent the risk
   check does not run, and a Remedy the rules said needs one needs **two**, recorded with that
   reason (`approvals_set_by = risk_model_budget`, migration 00108) — fail closed: a question nobody
   paid for never lets one approval stand.

**As built (review judgment 2, 2026-10-05).** Rules are matched **most severe first**: when several
match, the one that says two wins, and a command no rule matches needs two. The risk model is named
in the same rules file and is asked **only when the rules say one**; an error from it, a turn with no
token usage, or an answer oto cannot read leaves the Remedy at two, recorded `risk_model_failed`.
Three decisions from the review close ways a rule could say one about a command it did not read:

- **A rule that says one names its Tool** (`tool:`). The operator thereby says which Tools are
  kubectl-shaped; a one-approval rule without a Tool is refused by `oto remedy-rules apply`
  (`single_needs_tool`) and by the schema (migration 00110). A two-approval rule may still omit it.
- **A kind oto cannot fold is unparseable.** oto knows each built-in kind's plural, short and
  group-qualified spellings; a command naming any other kind (a CRD's, say) is unparseable, so two
  approvals, and a rule naming one is refused (`unknown_kind`).
- **Applying new rules re-tiers no Remedy already proposed** — its `required_approvals` is frozen at
  proposal. Decline any pending Remedy the new rules would have tiered higher (§4: any member may).

### 4. Who may approve

A user holding the approval grant **on that Remedy's ToolServer** — the first and only piece of the
deferred `authz` module, and a prerequisite: no Remedy ships before it. Double approval means two
different such users; one person in Slack and again in the UI counts once. A click from an unlinked
Slack identity is refused with a reply on how to link. A grant is a permission, never an obligation:
it routes nothing to anyone and creates no queue (H-1).

**Two rulings on who may say yes and who may say no (owner, 2026-10-05).**

1. **Approval needs a browser session or a Slack click — never a token (F5).** A personal access
   token's approval is refused, `403 remedy_approval_needs_a_session`, before the Remedy is read:
   an approval is what makes a command run, a leaked or scripted token must not make one count, and
   two holders' tokens in one script would be an automatic approver — the thing verdict #23 refuses.
   It is the reasoning that made linking a Slack account session-only. A Slack approval is a signed
   click by a linked person, not a token, and stands. Declining stays open to a token.
2. **Any member may decline (F2)** — a human, grant or no grant. Saying no is always the safe
   direction; a mistaken decline costs one re-proposal, since a declined Remedy may be proposed
   again by a later Investigation; and decline is how an operator stops a Remedy tiered under rules
   since tightened (§3).

**Amendment (owner's ruling, 2026-10-02): the grant is given and taken from the host shell only.**
`oto grant remedy-approver --org <slug> --toolserver <name> --email <addr>` creates it and
`oto revoke remedy-approver …` removes it, as subcommands like `oto bootstrap` and
`oto reset-password`. Running one needs a shell on the host and the database credentials — the
authority that could write the row by hand anyway — which also answers who grants the first grant
before any admin role exists. **No HTTP route may ever create, change or delete a grant**; the API
shows a ToolServer's holders read-only (and so will the UI, which has no ToolServer screen yet) (`GET /api/v1/tool-servers/{id}/remedy-approvers`).
The reason is double approval itself: an in-app grant would let one holder mint a second approver
(an alt account) and approve alone. A test walks the mounted router to hold this. An earlier
ruling the same day, a declarative grant in the deployment's configuration, is superseded.

The shape (migration 00103, git-bug 47f67c8): a grant is a row `(org, ToolServer, user)`, unique
per ToolServer and user, stamped with the CLI's clock and `granted_by = cli`. The user is resolved
by email, so a shadow member (no address) can never hold one; a disabled user's grant stays on the
record and stops counting; revoking deletes the row. The ToolServer must exist and must be declared
`write` — a Remedy runs through a write Tool, so a grant on a `read` ToolServer would permit
nothing, and both the CLI and the schema refuse it. Deleting the ToolServer deletes its grants.
Granting one already held, or revoking one not held, is refused rather than ignored, so a typo is
never mistaken for success.

### 5. Where it runs

In the **ToolServer**, under the ServiceAccount and RBAC its operator gave it. oto holds no cluster
credential, which keeps 0016's trust boundary. The write Tool is never in the Investigator's hands;
execution is a separate step that sends the approved arguments exactly.

**oto ships no ToolServer** (owner's ruling, 2026-10-02). Every Tool, read or write, is an MCP server
the operator configures; a reference subchart was considered and refused. Where a command runs, and
how it is sandboxed, is therefore the operator's ToolServer's business, and the trust boundary is
that server's authentication of oto: a ToolServer that accepts oto's credential will run whatever
oto sends it, so oto's approval and risk rules (§3, §4) are the only gate oto owns. The docs must
say this, and must recommend that a write ToolServer run commands under RBAC narrower than the
read one.

### 6. After it runs

A failed Remedy is **never retried automatically** — a retry is a new Remedy and a new approval.
An executed Remedy triggers a follow-up Investigation of its Incident, so the timeline shows whether
it helped.

As built (git-bug a53c8b0; review judgment 2): the follow-up is asked for right after the record of
the Remedy `executed` commits, in its own transaction: one execution asks at most once, a
redelivered execution job asks for none, and a follow-up that cannot be asked for is logged and
never costs the record of what the Tool answered. A failed Remedy asks for none. The Investigator that proposed the Remedy runs it — the same author
on both sides of the change — whether or not it is opted into Incidents, since that opt-in governs
the Incident's own facts. "Its Incident" is the one its facts are declared to; a Remedy on a Case
no Incident holds is followed up on that Case. ADR 0053 §6's controls hold: a switched-off
Investigator or org is unsubscribed (no row, owner ruling O1), a spent budget is recorded
`skipped`/`budget`, the concurrency makes it wait, and it coalesces under the minimum interval
like a membership change, so several Remedies executed together are one follow-up.

## Consequences

- oto's safety class changes by the order of magnitude verdict #23 warned of. A bug is now able to
  change a production cluster, bounded by approval, the risk rules and the ToolServer's RBAC — in
  that order of failure.
- The `authz` grant becomes a dependency of a feature, not a deferred module.
- A self-hosted security review now has a write path to assess. oto's part of it is approval, risk
  and audit; the sandbox is the operator's ToolServer, which oto neither ships nor inspects.
