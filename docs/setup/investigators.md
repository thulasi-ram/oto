# Investigators

An **Investigator** is a named, versioned configuration (a model, a prompt, an allowlist of Tools
and budgets) that oto runs against a Case, an Incident or a digest window. Each run is an
**Investigation**. It reads oto's own history and the MCP **ToolServers** you configure, and it
records every model turn and every Tool call as a **Step**. It ends with a **Finding**, which may
carry a **Classification** in your own words, and it may propose **Suggestions** and **Remedies**
that only a human can apply or approve
([ADR 0053](../adr/0053-an-investigator-reads-proposes-and-never-decides-delivery.md)).

**It never decides whether anyone is told.** A Finding changes what people read, never whether a
notification is sent: it never suppresses one, overrides a policy or adds a send. Investigations run
asynchronously, on their own job queue and off the notification path, and nothing that notifies
ever waits for one. A digest is sent on time whether or not its Finding is ready.

This page covers setting one up, what each control does, and where oto's guard ends and your
ToolServer's begins. Every resource on it is configured through the API (there is no settings
screen yet), with an org admin's token.

```
model endpoint ──┐
                 ├──► Investigator (version N) ──► Investigation ──► Steps ──► Finding
ToolServers ─────┘          ▲  allowlist, budgets,                         ├─► Classification
 (access: read)             │  interval, investigates_incidents            ├─► Suggestions (a human applies)
                            │                                               └─► Remedies (two humans approve)
                  a human asks · an Incident is drawn or changes · a digest window closes
```

## 1. A model endpoint

```http
POST /api/v1/model-providers
{"name": "gateway", "base_url": "https://llm.internal.example/v1", "model": "gpt-4o", "api_key": "…"}
```

- **One adapter, three settings.** oto speaks the OpenAI-compatible Chat Completions API with tool
  calling. That means a base URL, a model name and an API key, and nothing vendor-specific.
- **Another provider goes through your gateway.** A model that does not speak that API (or that you
  want to meter, cache or route) is reached through a gateway you run, such as LiteLLM, Bifrost or
  any other that serves the API. oto has no second adapter.
- **The key is sealed**, like a channel credential, and is never returned by any route. It is sent
  only over `https`: a key on an `http` base URL is refused (`model_api_key_needs_https`). A base
  URL with credentials in it (`https://user:key@host/…`) is refused rather than stripped. A
  gateway inside the cluster with no key may use `http`.
- **Redirects are not followed.** A 3xx from the endpoint fails the turn. The key is never
  forwarded to wherever the redirect points.
- **A turn must report token usage.** Every budget below is enforced from it, so a turn with no
  usage fails the run with `usage_missing` and does not run unbudgeted. A gateway that strips
  `usage` cannot be used.
- **One turn writes at most 4096 tokens** (`MaxTurnOutputTokens`), or whatever is left of the run's
  token budget if that is less. A model whose answer is cut at that cap fails the run with
  `model_error`, and the partial text stays in its Step. When the cut came from the run's token
  budget instead, the run is `exhausted`.
- **One answer is read up to 32 MiB.** A larger response body fails the turn with
  `model_answer_too_large`. It does not take the worker down.

## 2. ToolServers: where trust stops

oto ships **no** ToolServer. Every Tool past the built-in ones is served by an MCP server **you** run
(Kubernetes, VictoriaMetrics, VictoriaLogs and so on), reached over HTTP. oto runs no subprocess.

```http
POST /api/v1/tool-servers
{"name": "k8s", "url": "https://mcp-k8s.internal.example/mcp", "access": "read", "token": "…",
 "transport": "streamable_http", "call_timeout_seconds": 15, "max_result_bytes": 16384}
POST /api/v1/tool-servers/{id}/discover
GET  /api/v1/tool-servers/{id}/tools
```

**`access` is the trust boundary, and you declare it.** An Investigator holds **only read-only
Tools** while investigating. What makes a Tool read-only to oto is your declaration on its
ToolServer:

- `access: read`: every Tool it serves only reads. These are the only Tools an Investigator's
  allowlist may name.
- `access: write`: it serves at least one Tool that changes something. **No Investigator ever holds
  a write ToolServer's Tool.** An allowlist naming one is refused, and a run is never offered one.
  Write Tools are what a [Remedy](remedies.md) names.

**A mixed server is declared `write`.** If one server serves both reads and writes, declaring it
`read` would put its write Tools in a model's hands. Split it into two servers, or declare it
`write` and run a separate read server for investigating.

**A Tool its own server marks not read-only is refused even on a `read` server.** MCP lets a server
annotate a Tool with `readOnlyHint`. If a Tool's annotations say `readOnlyHint: false`, oto shows it
as unusable on a `read` ToolServer, with the reason *"the ToolServer says this Tool is not read-only
(MCP readOnlyHint is false); an Investigator holds only read-only Tools — move it to a write
ToolServer"*. An allowlist naming it is refused (`422 investigator_tools_invalid`, the violation `unusable_tool`), and a run that meets it after a
re-discovery records a refused Step. The hint only ever **refuses**. `readOnlyHint: true` does not
make a Tool safe, and a Tool with **no annotations** is held on your declaration alone. So for a
server that does not annotate its Tools, `access` is the only guard.

**The rest of the ToolServer's settings:**

| Setting | Bounds | Default | What it does |
|---|---|---|---|
| `name` | 1–24 chars, `a-z`, digits, `-` | — | Half of every Tool's qualified name, `<toolserver>__<tool>` (e.g. `k8s__pods_list`). That qualified name is what an allowlist names and what the model is offered. It is capped at 64 characters. |
| `url` | `http`/`https`, ≤ 2048 | — | Where the server listens. |
| `transport` | `streamable_http` \| `sse` | `streamable_http` | `sse` is for servers that have not moved to MCP's current transport. |
| `token` | ≤ 4096 | none | Sealed. It is sent only over `https` (`tool_server_token_needs_https`) and **only to the origin you configured**: a request the server points anywhere else is refused (`tool_server_endpoint_off_origin`), never sent without the header. |
| `call_timeout_seconds` | 1–120 | 15 | Per Tool call. A call past it is recorded on its Step as a timeout, and the run continues. |
| `max_result_bytes` | 1024–61440 | 16384 | What one call may hand the model. A longer result is truncated, the truncation is recorded on the Step, and the run continues. |

**Discovery** reads the server's Tool list (at most 200 Tools) with their descriptions and input
schemas. Run it again after the server changes. `GET …/tools` shows each Tool's qualified name and,
for one that cannot be held, `usable: false` with the reason.

**What a Tool returns is redacted** before it is recorded as a Step or read by the model. The rules
are your alert sources' own redaction rules (`redact_labels`, `redact_annotations`), applied by name.
Name-based redaction cannot see a secret that has no name before it. **The real boundary is the
ToolServer's own RBAC:** give a read ToolServer a ServiceAccount that can only read, and only what
you are willing to have a model read.

**A write ToolServer runs under narrower RBAC than a read one.** It is the one path by which oto
changes a cluster. Nothing calls a write Tool except an approved Remedy's single execution. See
[Remedies](remedies.md) for its setup, the approver grant (`oto grant remedy-approver`, host shell
only) and its trust boundary.

## 3. An Investigator

```http
POST /api/v1/investigators
{"name": "triage1", "model_provider_id": "…", "prompt": "You investigate Kubernetes alerts…",
 "tools": ["oto_case_timeline", "oto_rule_at_fire", "oto_prior_findings", "k8s__pods_list"],
 "budgets": {"max_steps": 20, "max_tokens": 200000, "max_wall_seconds": 300},
 "min_interval_seconds": 600, "investigates_incidents": false, "enabled": true}
```

`name` is 1–63 lower-case letters and digits, starting with a letter, because a Case's Finding is
published as the Enrichment `investigator.<name>`.

**Versions.** Changing the model endpoint, the prompt or the allowlist (`PATCH`) writes version N+1.
A Finding names the version that produced it. `enabled`, the budgets, the interval and
`investigates_incidents` change in place and never version. ⚠️ A version does **not** pin your
classification set or a ToolServer's re-discovered Tool descriptions and schemas. Those are read
when each run starts.

**The allowlist** names Tools exactly, with no wildcards, up to 64. A call outside it is refused and
recorded as a Step, and the run continues. It may name:

- **The built-in Tools**, which read oto's own history and call no ToolServer. They run under fixed
  limits of **15 s** per call and **16 KiB** per result. Those limits are not settings.
  - `oto_case_timeline`: the Case's timeline.
  - `oto_rule_at_fire`: the alerting rule as it stood when the Case fired.
  - `oto_prior_findings`: earlier Findings about the same subject. For a Case that means the same
    alert (`alert_key`) the last times it fired, and for a digest the same policy's earlier windows.
  - `oto_member_findings`: an Incident's member Cases' earlier Findings.
  - `oto_digest_cases`: a digest window's Cases, read again at the call.

  A built-in Tool that does not fit the run's subject (a Case Tool on a digest run, say) is not
  offered to it.
- **Read ToolServers' Tools**, by qualified name `<toolserver>__<tool>`.
- **The proposing Tools**: `oto_suggest_count_condition` and `oto_suggest_membership`
  ([Suggestions](#suggestions)), and `oto_propose_remedy` with `oto_write_tools`
  ([Remedies](remedies.md)).

**`oto_classify` is not on the allowlist.** It is offered whenever your org has written classes, and
never otherwise.

**Per-run budgets.** Hitting any of them ends the run `exhausted`. The Finding it reached so far is
kept and marked **partial**.

| Budget | Bounds | Default |
|---|---|---|
| `max_steps`: Tool calls | 1–100 | 20 |
| `max_tokens`: input + output tokens | 1000–2,000,000 | 200,000 |
| `max_wall_seconds` | 10–1800 | 300 |

**Answer-shaping calls are free, up to a cap.** `oto_classify` and the proposing Tools are the
shape of the answer, not a look at anything, so they cost no step. Each is still a Step, because
what the model said is the record. One run may make **50** of them (`MaxFreeCallsPerRun`). Past
that, each is refused on the record and counted against the step budget like any Tool call. One run
proposes at most **5** Suggestions.

**`min_interval_seconds`** (0–86400, default 600) bounds runs **on one subject** that an Incident
starts on its own, when it is drawn or its membership changes. Triggers inside the interval coalesce into the one queued run,
which carries the time it may start. **A human asking is not held to it.**

**`investigates_incidents`** defaults to **false**. An Incident being drawn, or its membership
changing, starts a run only for Investigators opted in with it. So a stock install investigates no
Incident automatically. No Case is investigated automatically, so a Case run is one a human
asked for. An Incident's run covers its member Cases.

**`enabled`** is this Investigator's own switch. Switched off, it is unsubscribed: automatic
triggers leave no row. A human who asks still gets a run recorded `skipped` with reason `disabled`
and a sentence saying which switch.

### What starts a run

| Trigger | Runs for |
|---|---|
| A human asks (`POST /api/v1/cases/{id}/investigations`, `…/incidents/{number}/investigations`) | the Investigator named |
| An Incident is drawn, or a Case is added or removed | every enabled Investigator with `investigates_incidents` |
| A digest window is about to close | the policy's `digest_investigator_id` (below) |

### How a run ends

Six statuses: `queued`, `running`, `completed`, `exhausted`, `failed`, `skipped`. The last three
always carry a reason and a sentence:

| Status | Reasons |
|---|---|
| `exhausted` | `step_budget`, `token_budget`, `wall_time_budget` |
| `failed` | `usage_missing`, `model_error` (unreachable, refused, ended without an answer, or cut at the per-turn cap), `model_changed`, `subject_gone`, `interrupted` (its worker stopped; it is not re-run, because a re-run would pay for every turn twice), `internal` |
| `skipped` | `disabled`, `budget`, `window_closed` |

The Steps are kept, so "why did it say that?" always has an answer: `GET /api/v1/investigations/{id}`.
**The Steps stay in oto.** Only a Finding and its Classification go outbound.

## 4. Where a Finding goes

- **A Case's** Finding is published as the Enrichment `investigator.<name>`, so it appears on the
  Case's card and in the API and SSE like any Enrichment.
- **An Incident's** Finding is on every later Incident fact as `incident.finding`. The `finding`
  Incident fact, which says that a new one arrived, is sent **only** to notification policies whose
  `reasons` name `finding`. It is a subscription, not an extra send (see
  [the webhook envelope](webhook.md)).
- **A digest window's** Finding stays on its run. It is copied onto the digest only if the run ended
  with one by the time the window closed (`digest.finding`).

### Classification

Write your classes with `PUT /api/v1/investigation-classes` (Settings → Classification): up to 50,
each a name and a description. oto ships none. With classes written, a Finding is classified as one
of them or as `unclassified`, which is always admissible and is the right answer under doubt. With
none, nothing is classified. ⚠️ **Paging on a classification is paging on a model's judgement.**

### Suggestions

A Suggestion is a change a Finding proposes: a count condition on a notification policy, or a Case
into an Incident. **Only a human applies one.** Applying it performs the ordinary edit with that
human as the actor, and the Suggestion is its provenance. A Suggestion has no reject verb: it is
applied or it **lapses**, seven days after it was proposed. A count Suggestion whose policy's count
condition was changed after it was proposed is refused when applied (`409 suggestion_stale`) and
writes nothing. Edit the policy by hand instead.

## 5. The digest Investigator

A notification policy with a digest window may name an Investigator to summarise each window:
`digest_investigator_id` on the policy. That requires `digest_window_seconds`, and a `PATCH` that
clears the window without mentioning the Investigator clears both.

- The window's run is **armed ahead of the close** by `wall budget + 2 minutes`, capped at half the
  window. So a run that keeps to its budget ends before the window closes.
- **The digest never waits.** If the run has ended with a Finding when the window closes, the digest
  carries it. Otherwise the digest goes out on time with the built-in body.
- A run **still `queued`** when its window closes (behind the org's concurrency or an interval) ends
  `skipped` with reason **`window_closed`** without calling a model: no digest can carry it.
- A policy's digest runs (completed, skipped, failed) are listed by
  `GET /api/v1/notification-policies/{id}/investigations`, latest first, and shown on the policy in
  the web UI.

## 6. Org controls

Settings → Tuning, or the org settings API.

| Setting | Bounds | Default | On breach |
|---|---|---|---|
| `investigations_enabled` | on/off | on | The kill switch for every Investigator in the org. Nothing new starts, and notifications are unaffected. |
| `investigation_daily_tokens` | 1000–1,000,000,000 | 2,000,000 | Once the day's recorded spend reaches it, a new Investigation is recorded `skipped` with reason `budget` and never queued. It resets at 00:00 UTC. A run already going is bounded by its own budget, so the day can overrun by at most what the runs in flight still had left. |
| `investigation_concurrency` | 1–32 | 2 | A run past it waits `queued` and is never dropped. |

**Capacity across the deployment.** Investigations run on the dedicated `investigate` River queue,
so a minutes-long run never holds an `enrich` slot or a worker a notification waits for. Its width
is 8 per process by default. Set `OTO_JOBS_QUEUE_INVESTIGATE` (`jobs.queue_investigate`) to change
it ([configuration](configuration.md#worker-and-river-queues)). An org's `investigation_concurrency`
above the workers you run never binds.

**Cost is a fact on the Investigation.** Each model-turn Step records its tokens, and the run records
the total. The Investigation panel on the Case and Incident page shows both.
