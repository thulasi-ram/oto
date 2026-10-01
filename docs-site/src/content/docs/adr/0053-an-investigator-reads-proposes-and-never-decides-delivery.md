---
title: 0053 — An Investigator reads, proposes, and never decides delivery
---
**Status:** Proposed · 2026-10-02 — settled in a design session with the owner.
**Builds on:** [0016](/oto/adr/0016-mcp-enrichment-no-firehose/) — cluster context through MCP, on demand,
async only, recorded as a snapshot. An Investigator is that decision with a model in the loop.
**Supersedes in part:** CONTEXT.md's module map, which lists *"anything AI"* as DEFERRED-POST-V1.
**Holds:** [0042](/oto/adr/0042-storm-damping-is-removed/) §3 — unchanged and binding on everything here.
**Relates to:** [0052](/oto/adr/0052-an-incident-is-drawn-over-cases-and-its-response-is-handed-off/),
[0054](/oto/adr/0054-a-remedy-earns-the-write-path/),
[0044](/oto/adr/0044-a-count-condition-is-a-silence-the-operator-asked-for/).

## Context

The owner asked for an agent in the shape of HolmesGPT / OpenSRE: it reads Kubernetes, VictoriaLogs
and VictoriaMetrics through MCP servers, classifies alerts, writes digests, and — as first asked —
decides whether an alert is sent at all. The last part is what 0042 deleted twice (`flapping`,
`storm`), and the reason has not changed: a bad suppression is invisible, and the one failure a
flight recorder exists to make impossible. Gating delivery on a model would also put an external
API and its latency on the notification path, which 0016 constraint 1 forbids.

## Decision

### 1. Vocabulary

**Investigator** (a named, versioned configuration: model, prompt, Tools, budgets) ·
**ToolServer** (one configured MCP server; where trust stops) · **Tool** · **Investigation** (one run
against one subject, frozen when it ends) · **Step** (one immutable transcript entry) · **Finding**
(what it concluded) · **Classification** · **Suggestion**. Definitions are in CONTEXT.md. Not
"agent": in a VictoriaMetrics stack that is `vmagent`, and in 0016 it is the rejected in-cluster
daemon.

### 2. It never decides whether anyone is told

A Finding is never an input to whether a notification is sent — not to suppress, not to override a
policy, not to send more. The Investigator changes what people **read**, never **whether** they are
told. Noise reduction arrives as **Suggestions** (e.g. "add `count_min=3` to this policy"), which a
human applies and which then act deterministically. A Suggestion is applied or lapses; it has no
reject verb, so it is never a queue.

### 3. Where it runs

Asynchronously, as a River job in the enrichment phase; a Finding is published as an Enrichment
(`investigator.<name>`), so cards, API and SSE need nothing new. While investigating it holds only
read-only Tools. **Memory is not a new store**: it is oto's own history — prior Findings on the same
`alert_key`, the Case timeline, the rule as it stood at fire time — offered as built-in Tools. The
Steps are kept, so "why did it say that?" always has an answer.

### 4. Subjects and triggers

Subjects are `case | incident | digest | policy`. A subject may have many Investigations over time,
each seeing the last one's Finding; the latest is shown. Triggers: an Incident is drawn; its
membership changes (with a minimum interval on the Investigator); a human asks; a Remedy is executed
(0054). Not on quiet. A Case already in an Incident gets no Investigation of its own automatically —
the Incident's covers it — so a forty-Case storm costs a handful of runs, not forty.

### 5. Classification is the operator's vocabulary

A Finding's class is chosen from a closed set the operator wrote, or `unclassified`, which is always
admissible and is the right answer under doubt. oto ships no classes — no `noise`, which would be
oto's opinion of someone else's signal. A Finding keeps its class if the set later changes. Because
classifications travel outbound (0052 §5) and an external tool **may page on them**, the docs must
say plainly: paging on a classification is paging on a model's judgement.

### 6. The minimal control set

Every control is a number an operator can read back, and hitting one is **recorded, never silent**.

| Control | Scope | On breach |
|---|---|---|
| **Enabled** | org, and each Investigator | Nothing starts. A kill switch, effective for runs not yet begun. |
| **Step budget** — max Tool calls | one Investigation | Ends `exhausted`; whatever Finding it reached is kept and marked partial. |
| **Token budget** — max input + output tokens | one Investigation | As above. |
| **Wall-time budget** | one Investigation | As above. |
| **Daily token budget** | org | New Investigations are recorded as `skipped` with reason `budget`, not queued. Resets at UTC midnight. |
| **Concurrency** — max running Investigations | org | Waits in the job queue; never dropped. |
| **Minimum interval** between runs on one subject | each Investigator | Membership-change triggers inside the interval coalesce into one run. |
| **Tool allowlist** — named Tools, no wildcards | each Investigator | A call outside it is refused and recorded as a Step. |
| **Per-call timeout** and **result size cap** | each Tool call | The Step records the timeout or the truncation; the run continues. |

Every Investigation records the tokens it spent, so cost is a fact on the timeline. Changing an
Investigator's model, prompt or allowlist makes a new version; a Finding names the version that
produced it.

## Consequences

- Every Finding, its classification and its Steps go outbound to the declared incident as facts.
- An org with no ToolServer still gets Investigations over oto's own history.
