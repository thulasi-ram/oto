# 0054 — A Remedy earns the write path

**Status:** Accepted · 2026-10-02 — settled in a design session with the owner, who authorised
building it the same day. **Depends on** the one `authz` grant in §4: no Remedy ships before it.
**Earns:** SCOPE-BOUNDARY **H-3** for cluster writes — verdict **#23** (auto-remediation) — on the
terms SS-4 set for any write path: an audit trail and a confirmation UX.
**Supersedes in part:** [0013](0013-alert-first-scope-boundary.md) FR-1's refusal of facts about a
**response effort**, for the one noun `Remedy`. Every other FR-1 refusal stands.
**Amends:** [0016](0016-mcp-enrichment-no-firehose.md) — a ToolServer may now expose write Tools;
oto still holds no cluster credential.
**Relates to:** [0052](0052-an-incident-is-drawn-over-cases-and-its-response-is-handed-off.md),
[0053](0053-an-investigator-reads-proposes-and-never-decides-delivery.md).

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

### 2. Approval happens in oto; everything goes outbound

Approve and decline happen in oto's UI and in Slack. Every transition is posted to the declared
Incident as a fact (0052 §5). oto **cannot know** that someone in the incident tool said no — that
is the stated cost of outbound-only; saying no happens in oto. An approval shows the latest facts
sent outbound and expires if not executed within an operator-set time.

### 3. Risk: rules first, a model may only raise it

Operator-written rules over the command (verb, resource kind, namespace, reversibility) set the
baseline. A model may then move a Remedy from single to double approval, **never back**. Anything
the rules cannot parse — `sh -c`, pipes — is double approval. The risk model sees **only** the
command, its target and the rules' verdict, never the Investigation: logs are attacker-writable, and
a log line is the obvious injection path. The approval screen shows the exact command **first**,
above the Investigator's description of it.

### 4. Who may approve

A user holding the approval grant **on that Remedy's ToolServer** — the first and only piece of the
deferred `authz` module, and a prerequisite: no Remedy ships before it. Double approval means two
different such users; one person in Slack and again in the UI counts once. A click from an unlinked
Slack identity is refused with a reply on how to link. A grant is a permission, never an obligation:
it routes nothing to anyone and creates no queue (H-1).

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

## Consequences

- oto's safety class changes by the order of magnitude verdict #23 warned of. A bug is now able to
  change a production cluster, bounded by approval, the risk rules and the ToolServer's RBAC — in
  that order of failure.
- The `authz` grant becomes a dependency of a feature, not a deferred module.
- A self-hosted security review now has a write path to assess. oto's part of it is approval, risk
  and audit; the sandbox is the operator's ToolServer, which oto neither ships nor inspects.
