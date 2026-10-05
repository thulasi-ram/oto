---
title: 0053 — An Investigator reads, proposes, and never decides delivery
---
**Status:** Accepted · 2026-10-02 — settled in a design session with the owner, who authorised
building it the same day (git-bug `8f1f071` carries the model-provider ruling recorded in §3).
**Amended 2026-10-05** — after the review of the built code, with four owner rulings; see
[the amendment](#amendment--2026-10-05-what-the-built-investigator-does-and-four-owner-rulings)
below. Where it and a section above differ, the amendment governs.
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

> ⚠️ Amended 2026-10-05 (A1): the `finding` Incident fact is a subscription, not an extra send.

### 3. Where it runs

> ⚠️ Amended 2026-10-05 (A2, A3, A4): it runs on its own `investigate` queue, not in the
> enrichment phase; a digest window's Finding is not an Enrichment; and "read-only" is defined.

Asynchronously, as a River job in the enrichment phase; a Finding is published as an Enrichment
(`investigator.<name>`), so cards, API and SSE need nothing new. While investigating it holds only
read-only Tools. **Memory is not a new store**: it is oto's own history — prior Findings on the same
`alert_key`, the Case timeline, the rule as it stood at fire time — offered as built-in Tools. The
Steps are kept, so "why did it say that?" always has an answer.

**The model provider is generic** (owner's ruling, 2026-10-02). Models are reached through one
port, and the first adapter speaks the OpenAI-compatible Chat Completions API with tool calling, so
one adapter configured by base URL, model name and a sealed key reaches any provider or gateway
that serves it. No provider's name appears in domain code. The port must report token usage — the
§6 budgets are enforced from it — and a turn without usage fails the run rather than running
unbudgeted. A native adapter for one vendor is a later widening behind the same port.

**What the port is built from** (owner's rulings, 2026-10-02, on accepting this ADR):

- **One adapter, on the official `github.com/openai/openai-go` SDK**, speaking Chat Completions with
  tool calling and configured by exactly three things: a base URL, a model name and a sealed API
  key (sealed like a channel credential). The SDK is a transport; its types stop at the adapter.
- **A provider that does not speak that API is reached through the operator's gateway** (LiteLLM,
  Bifrost, or any other that serves it), never by a second adapter in oto — until a tool-calling
  fidelity gap is shown in a test, which is the only argument for a native one.
- **No agent framework** (langchaingo, eino, genkit and the like). oto's own loop calls the port,
  records every turn, Tool call and result as a Step, and enforces every §6 budget itself, because
  a framework that owns the loop owns exactly the two things this ADR exists to make readable.
- **ToolServers are reached with the official MCP client, `github.com/modelcontextprotocol/go-sdk`.**

### 4. Subjects and triggers

> ⚠️ Amended 2026-10-05 (A5, A6, A7; owner ruling O2): Incident triggers are an opt-in per
> Investigator, the digest window is a trigger, and a run "reads" the last Finding through a Tool.

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

> ⚠️ Amended 2026-10-05 (A8–A12; owner rulings O1, O4): a switched-off Investigator is an
> unsubscription, a human request is outside the interval, a digest run its window outlived is
> `skipped`/`window_closed`, what a version does not pin, and where cost is recorded.

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

- ~~Every Finding, its classification and its Steps go outbound to the declared incident as facts.~~
  ⛔ **Superseded 2026-10-05 by owner ruling O3 (A14):** only the Finding and its Classification go
  outbound; the Steps stay in oto.
- An org with no ToolServer still gets Investigations over oto's own history.

## Amendment — 2026-10-05: what the built Investigator does, and four owner rulings

The Investigator was built on this ADR (git-bug `3b3e68b..7cffe6b`, then the Remedy work and two
fix batches) and then reviewed. The review found the code consistent with the doctrine in most
places where it differed from the text. In those places the text was underspecified, and this
amendment records what is now true. In four places the doctrine itself had to be decided, and the
owner ruled on 2026-10-05 (O1–O4). Each item names the section it amends.

### §2 — It never decides whether anyone is told

**A1. The `finding` Incident fact is a subscription, not an extra send.** 0052 §5 lists "new
Finding" as an Incident fact. It reaches only notification policies whose `reasons` name `finding`,
and migration 00099 added it to no existing policy. An operator choosing to be told that a Finding
arrived is a routing decision the operator made. The Investigator did not decide a send.

### §3 — Where it runs

**A2. On its own queue, not in the enrichment phase.** An Investigation runs as
`investigations.run` on the dedicated `investigate` River queue, so a minutes-long run never holds
an `enrich` slot or a worker a notification waits for. An approved Remedy's single write call
(`remedies.execute`, 0054) shares the queue. Its width is a deployment setting
(`jobs.queue_investigate`, default 8). Each org's `investigation_concurrency` (§6) narrows that
org's share.

**A3. Where a Finding is published.** A Case's or an Incident's Finding is published as an
Enrichment (`investigator.<name>`). **A digest window's is not.** An Enrichment is keyed by subject
alone, and it would be overwritten every window. So a digest window's Finding stays on its run and
is copied onto the digest it was ready for (`notifications.digest_finding`, git-bug `3e96f5a`).

**A4. What "read-only" means.** It is the operator's declaration on each ToolServer: `access: read`
or `access: write`. Only a `read` server's Tools may be on an allowlist. A Tool its own server marks
not read-only (MCP `readOnlyHint: false`) is refused **even on a `read` server**: it is unusable,
an allowlist naming it is refused, and a run meeting it records a refused Step. The hint can only
refuse a Tool. A `true` hint or a missing annotation leaves the declaration as the guard. **A mixed
server is declared `write`.** Operators are told this in `docs/setup/investigators.md`.

### §4 — Subjects and triggers

**A5 (owner ruling O2). Incident triggers are an opt-in, default off.** An Incident being drawn,
or its membership changing, starts an Investigation only for an enabled Investigator with
`investigates_incidents = true`. That column defaults to `false`. A stock install therefore
investigates no Incident automatically, and an operator chooses explicitly, for cost. "Drawing an
Incident starts one Investigation" (and the matching done-when clause of git-bug `74ea849`) is read
as "for each Investigator opted in".

**A6. The digest window is a trigger, and its opt-in is the policy's.** §4 lists `digest` as a
subject, and a digest cannot have a run without a trigger. A notification policy names the
Investigator that summarises its windows (`notification_policies.digest_investigator_id`, NULL by
default). Each window's run is armed `DigestLead` before the window closes. `DigestLead` is the
Investigator's wall-time budget plus two minutes (one tick to notice, one for the job to start),
capped at half the window. So a run that keeps to its budget ends before the close. The digest
never waits for it. If the run has not ended with a Finding when the window closes, the digest goes
out on time with the built-in body.

**A7. "Each seeing the last one's Finding" means *able to read* it.** Memory is offered as
built-in Tools (§3). So each Investigation is able to read the last one's Finding, through
`oto_prior_findings` (the same subject's earlier Findings) and `oto_member_findings` (an
Incident's member Cases' Findings) when its allowlist holds them. It is not handed the Finding
unasked.

### §6 — The minimal control set

**A8 (owner ruling O1). A switched-off Investigator or org is an unsubscription.** "Enabled →
Nothing starts" is kept. Automatic triggers (an Incident's, a digest window's) **leave no row**
for a switch the operator deliberately turned off: `enabled = false` is itself the readable record,
and a row per digest window (up to 288 a day) for it would be noise. **A human who asks still gets a
row**, `skipped` with reason `disabled` and a sentence naming which switch, because a person asked
and must be answered. The daily budget is not a switch, and it stays recorded per run asked for
(`skipped`/`budget`).

Both switches behave this way on both automatic triggers: `IncidentChanged` and
`ArmDigestInvestigations` read the org's `investigations_enabled` before asking any Investigator,
and return without a row when it is off. A run already `queued` when a switch goes off is still
ended `skipped`/`disabled` when its job begins, because that row already exists and must end.

**A9. A human request is not held to the minimum interval.** The table scopes coalescing to the
triggers an Incident makes on its own: the draw and its membership changes. A human asking always
gets a run of their own.

**A10 (owner ruling O4). A digest run its window outlived is skipped.** A digest window's
Investigation that is still `queued` when its window closes ends `skipped` with reason
`window_closed`, without calling a model. It may have been held behind the org's concurrency or an
interval. Its Finding would be read by nothing: the window's digest has already gone out with the
built-in body, and a digest is never amended. The skipped row keeps it recorded, never silent, and
no tokens are spent for nothing. Migration 00106 adds the reason. A policy's digest runs are
listed by `GET /api/v1/notification-policies/{id}/investigations`.

**A11. What a version does not pin.** A version pins the model endpoint and model, the prompt and
the allowlist. It does **not** pin the org's classification set (§5), or a ToolServer's Tool
descriptions and input schemas as last discovered. Those are read when each run starts. Recording
a hash of either on the run is a possible follow-up. It would need a migration and the API.

**A12. Cost is a fact on the Investigation.** "Cost is a fact on the timeline" is not how it was
built. Each model-turn Step records its tokens, and the run records its total. Both are shown in
the Investigation panel on the Case and Incident pages, and the day's spend for the budget is read
from the Steps. A Case-timeline event for it is a possible follow-up.

### The loop's own rules (§3, §5, §6)

**A13.** These were decided in the code and are now doctrine:

- **Six statuses:** `queued`, `running`, `completed`, `exhausted`, `failed`, `skipped`. The last
  three always carry a reason and a sentence.
- **Tool names.** A ToolServer's Tool is held and offered as `<toolserver>__<tool>`. Built-in Tools
  are `oto_*` and call no ToolServer. They run under fixed limits of 15 s per call and 16 KiB per
  result.
- **`oto_classify` is offered whenever the org has classes**, outside the allowlist, with an enum of
  exactly those classes plus `unclassified`. With no classes nothing is offered, and the Finding
  carries **no classification**, not `unclassified`.
- **Answer-shaping calls cost no step.** `oto_classify` and the proposing Tools
  (`oto_suggest_count_condition`, `oto_suggest_membership`, `oto_propose_remedy`) are the shape of
  the answer, not a look at anything. Each is still a Step. They are free up to **50 per run**
  (`MaxFreeCallsPerRun`). Past that, each is refused on the record and counts against the step
  budget, so no loop of them is unbounded.
- **At most 5 Suggestions per run** (`MaxSuggestionsPerRun`). A Suggestion lapses a fixed **seven
  days** after it was proposed. That is not a setting. A count Suggestion whose policy's condition
  changed after it was proposed is refused when applied (`suggestion_stale`), and nothing is
  written.
- **One turn writes at most 4096 tokens** (`MaxTurnOutputTokens`), or the budget's remainder if
  that is smaller. An answer cut at oto's cap is `failed`/`model_error`, and one cut by the budget is
  `exhausted`. Only the **final** turn's text is a Finding. A final turn with no text, or a
  refusal, is `failed`/`model_error`.

### Consequences

**A14 (owner ruling O3). The Steps stay in oto.** Only a Finding and its Classification go
outbound: `incident.finding` on Incident facts, `digest.finding` on a digest that carried one, and
the Enrichment on a Case's facts. `incident.finding` and `digest.finding` carry the
`investigation_id`, so a reader can find the Steps through oto's API. The Steps hold redacted but raw Tool output, and the outbound surface is
the conclusion.
