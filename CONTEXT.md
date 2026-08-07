# oto — orientation for implementers

Read this first. Then read `docs/design/SPEC.md` §A–§C and the section for your module.
`SPEC.md` is binding; this file is the map.

---

## 1. What oto is

> The alert history layer your Prometheus stack does not have — self-hosted, with a UI, without
> adopting an AIOps platform or a paid SaaS.

For every alert that has ever fired, oto shows: when it first appeared, every episode since,
**what the rule said at that moment**, who was told, on which channel, in which thread, who
acknowledged it, and how it ended — as one continuous, replayable timeline.

**Why this exists.** Alertmanager holds no history (it is a silence console). Grafana's alert
history only covers Grafana-managed rules. Robusta's UI is SaaS-or-enterprise. Keep is
incident-first and generic. Grafana OnCall OSS was archived on 2026-03-24, leaving a dated,
documented vacuum for a self-hostable, OSS, Slack-first alert layer over an existing
Prometheus/Alertmanager stack.

**What oto is not, in v1:** an incident manager, an on-call/paging system, a workflow engine, a
rule editor, an AI investigator, or anything with a write path into your cluster.

---

## 2. The three commitments

1. **Append-only truth.** Every observation and every action is an immutable `AlertEvent`.
   Current state is a *projection*, never the only record. This is what makes the timeline
   honest. *If you would ever want to `UPDATE` it, it is not an Event.*
2. **Ingestion is decoupled from everything.** The webhook path does exactly two things —
   durably record the raw payload and enqueue work — in one transaction. Slack, Prometheus and
   the enrichment pipeline are **never** on the ingest critical path.
3. **Interfaces before implementations.** `Channel`, `Renderer`, `Enricher`, `AlertmanagerClient`
   are first-class ports with registries. Slack and the generic webhook are the first
   *implementations*, deliberately built as one-of-N.

---

## 3. Domain language (use these exact words)

| Term | Means |
|---|---|
| **Alert** | The **identity of a label set** within `(org, cluster)`. Created on first sight, survives resolution forever. oto's answer to Sentry's *Issue*. |
| **AlertOccurrence** | One **contiguous firing episode** of an Alert, `(alert_id, seq)`. What you ack, what you time for MTTR. |
| **AlertEvent** | One **immutable thing that happened at one instant**. The timeline. Append-only. |
| **AlertGroup** | One **generation of one Alertmanager notification group**, from `(source, receiver, groupLabels)`. **Owns exactly one Slack thread.** |
| **RuleSnapshot** | A content-addressed capture of a Prometheus alerting rule at a point in time. The differentiator. |
| **AlertSource** | One configured Alertmanager (+ optional Prometheus). HA replicas share a Cluster. |
| **Cluster** | Identity/failure domain. `cluster_key` participates in alert identity. |
| **Channel** | A **configured destination instance** ("Slack workspace T123, #sre-alerts"). Not a channel *type*. |
| **Notification** | The channel-agnostic **intent** to communicate one fact. Idempotent. |
| **NotificationDelivery** | **One materialisation** of a Notification on one Channel. Owns retry state and provider ids. |
| **ChannelThread** | The binding of an AlertGroup to `(slack_channel_id, root_ts)`. |
| **Enrichment** | One typed, provenanced result from one named, versioned `Enricher`. |
| **Silence** | A **read-only mirror** of an Alertmanager silence. |

**Banned words:** `incident`; unqualified `event`; `issue`; "notification" meaning a Slack
message (that is a Delivery); "group" meaning a UI grouping (that is a *view*); "alert" meaning
an occurrence or a Slack message.

### The four states

`firing` · `suppressed` · `resolved` · `expired`, plus an **orthogonal** `ack_state`
(`unacked` | `acked`). An acked alert is still firing.

Three rules you must not get wrong:

- **`expired` ≠ `resolved`.** `resolved` requires an explicit upstream `status="resolved"`.
  `expired` means *we stopped hearing about it*. Never fabricate a resolution.
- **`suppressed` is invisible to webhooks.** Alertmanager's `MuteStage` drops muted alerts before
  the webhook. Only the API v2 reconciler can set `suppressed`.
- **Losing sight of an alert is not the alert resolving.** The reaper is *blocked* while
  `source_health.status != 'healthy'`.

---

## 4. Module map

| Module | Mark | Responsibility |
|---|---|---|
| `platform` | CORE | Config, logging, telemetry, **two** DB pools + tx, httpx, authn, jobs, secrets, ratelimit, errs, clock, id. Not a domain. |
| `identity` | CORE | Orgs, users, sessions, PATs and ingest tokens → `Principal` + `TenantScope`. |
| `sources` | CORE | Alertmanager/Prometheus registry, credentials, health; owns the AM v2 + Prom v1 clients. |
| `ingestion` | CORE | Durably accept raw batches, normalise to Observations, run the reconciler. Nothing else. |
| `alerts` | CORE | Identity/dedup, the occurrence state machine, the append-only timeline. The heart. |
| `grouping` | CORE | Durable groups, generations, membership, storm detection. |
| `rules` | CORE | Fetch, content-address, version and diff rule definitions at fire time. |
| `enrichment` | CORE | The `Enricher` port, the budgeted pipeline, caching, provenanced results. |
| `notification` | CORE | Policy matching, idempotent intents, fan-out, thread ordering, throttle/damping. |
| `channels` | CORE | `Channel`/`Provider`/`Renderer` ports + Slack and generic-webhook impls. |
| `streaming` | CORE | `ui_events`, LISTEN/NOTIFY bridge, SSE hub with resume. |
| `silences` | PERIPHERAL | Read-only mirror of AM silences. **No write path.** |
| `stats` | PERIPHERAL | Alert-hygiene accounting. **Never per-person.** |
| `incidents`, `k8scontext`, `changefeed`, `views`, `audit`, `oncall`, `authz`, extra channel providers, anything AI | DEFERRED-POST-V1 | Do not build. Do not stub beyond the ports that already exist. |

**Dependency direction (no cycles, enforced by `depguard` + an arch test):**

```
ingestion ──► alerts ──► grouping ──► notification ──► channels
                │           │              │
                ▼           ▼              ▼
           enrichment    streaming      silences
                │
              rules ──► sources
```

`alerts` **never** imports `notification`. It appends events and enqueues jobs. This is what lets
oto run with notifications entirely disabled — which is how the first correctness tests run.

---

## 5. Layering rules (mechanically enforced — do not work around them)

```
internal/<domain>/
  api/          HTTP handlers, DTOs, mappers, routes
  service/      business logic + PORT INTERFACES it declares itself
  repository/   SQL, row structs, mappers
  domain/       pure Go: entities, invariants, state machine. NO I/O IMPORTS AT ALL.
```

1. `api` MUST NOT import `repository`. `repository` MUST NOT import `api`.
2. `domain` imports neither, and imports no `pgx`, no `net/http`, no `slack-go`. Ever.
3. `service` imports `domain` and its own port interfaces. Concretes are injected by
   `internal/app/container.go`.
4. Cross-domain calls are **service → service**, through an interface declared by the
   **consumer**. Never repository → repository.
5. Three model sets, and the compiler can tell them apart:
   `api.XxxDTO` (json + validate tags) ← `domain.Xxx` (pure) ← `repository.xxxRow` (unexported).
   **No DTO may embed a domain type or a row type.**
6. Every repository method is `(ctx, db.TenantScope, …)`. `TenantScope` has an unexported field
   and can only be built from an authenticated principal. There is no repository method without one.
7. Every composite index starts with `org_id`.
8. List methods take `db.Keyset` and return `db.Cursor`. **There is no `OFFSET` in this codebase.**
9. Transactions travel in `ctx` (`db.FromContext`). There are no `WithTx(tx)` method variants.

---

## 5b. Validation — seven layers, two opposite trust models (SPEC §L)

| # | Layer | Library | Failure |
|---|---|---|---|
| 1 | API DTOs | `go-playground/validator/v10` via `httpx.Bind[T]` **only** | 422 + `violations[]` |
| 2 | Untrusted inbound (webhook, reconciler) | hand-written bounds B1–B17 | `ingest_rejections` row, **still 202** |
| 3 | Domain invariants | value objects + `New…() (T, error)` | typed error; a bug |
| 4 | Provider config | `santhosh-tekuri/jsonschema/v6` | 422 mapped from schema errors |
| 5 | Outbound render | 18 Block Kit checks (V1–V18) | `dead` delivery, payload persisted |
| 6 | Persistence | `CHECK` / `NOT NULL` / `UNIQUE` / `FK` | 500 + alert (means 1–3 have a hole) |
| 7 | Frontend | `valibot` | inline field error / dev-time throw |

**The rule that matters most:** trusted user input and untrusted upstream payloads have
**opposite** policies. API bodies use `DisallowUnknownFields` and reject loudly. Alertmanager
bodies decode leniently (`routeLabels` is undocumented; Grafana sends a superset), reject
per-alert into `ingest_rejections`, and **still return 202** — a 4xx deletes the alert forever.

Other binding rules:
- No handler calls `json.NewDecoder` or `validate.Struct` directly. `httpx.Bind` is the only door.
- Violation `field` paths are **JSON names**, `/`-separated (`matchers/0/name`), never Go names.
- A bound (length, count, enum, regex) lives in **three** places — DTO `validate` tag, domain
  constructor, DDL `CHECK` — and they must be **identical**. `TestValidatorMatchesDDL` enforces it.
- There is no optional `Validate()` in `domain`. If you can construct it, it is valid.
- One JSON Schema per provider serves *both* server validation and the settings form. One source
  of truth; adding a provider needs no UI code.
- Never send a Slack payload that has not passed `render/slack.Validate`. A limit violation is a
  `dead` delivery, never a silent truncation.
- **The repository never validates a business rule** (service's job) but does reject malformed row
  models and is the single place SQLSTATEs become `errs.Kind`.
- Error taxonomy: validation (422, caller can fix) · conflict (409, re-read and retry) ·
  precondition (412, wrong entity state) · upstream-failure (502/504, never the caller's fault) ·
  unavailable (503, our backpressure). A unique violation on an oto-computed key is not an error —
  it is the idempotency mechanism.

## 5c. Brand and UI (SPEC §M)

**oto** (音) is Japanese for *sound* — a chime. Make one clear sound at the right moment.
**Calm by default, unmistakable when it matters.**

Two tiers, strictly separated:
- **Tier A — chrome is pastel.** Surfaces, borders, nav, tables, forms, chart gridlines.
  Chrome never uses a state hue. Brand accent is periwinkle (`#5B54D6` / `#A6A0FF`).
- **Tier B — saturated colour means alert state and nothing else.** No decorative accent, no
  chart series, no hover may use a state hue. Scarcity is what makes it loud.

Each state ships four tokens: `-fill` (pastel surface), `-border` (≥3:1), `-text` (≥4.5:1 on its
fill), `-solid` (saturated, ≥3:1). A critical row = pastel fill + saturated 3 px left bar + dark
red text. Full tokens and **measured** contrast ratios for light and dark are in SPEC §M.4–M.5.

- Colour is never the only channel — every state carries ≥2 of {colour, icon, text label}.
- Severity → **icon**. State → **colour**. (Same split as the Slack card; the only thing shared.)
- Dark mode is the default. No flashing ever; the one urgency pulse dies under
  `prefers-reduced-motion`.
- **The Slack palette (§H.2, Grafana OnCall hexes) is a SEPARATE, UNCHANGED system. Do not
  harmonise it with the UI tokens.** Different substrate, different contrast contract, and the
  OnCall values are the best-tested alert palette that exists. A renderer must not read a
  `--oto-*` token; a stylesheet must not contain a Slack hex.

## 6. Conventions

**Go**
- Module `github.com/otohq/oto`. Go 1.24. Postgres 16+.
- IDs are UUIDv7 from `platform/id.New()`. Never `gen_random_uuid()`.
- All timestamps `TIMESTAMPTZ` in UTC. **Every AlertEvent carries two:** `occurred_at` (upstream
  claim, *displayed*) and `recorded_at` (oto clock, *ordered by*). Never conflate them.
- Errors: stdlib `errors` + `platform/errs`; mapped to RFC 9457 problem+json in exactly one place.
- Enums are `TEXT` + `CHECK` in SQL, typed string constants in Go. No Postgres `ENUM` types.
- Hand-written SQL over `pgx/v5`. No sqlc, no ORM. `squirrel` only for the alert-list filter builder.
- Migrations are goose `.sql` in `db/migrations/`, **expand/contract only**. Never a destructive
  migration in one release. Assume N and N+1 run simultaneously.
- Every job payload carries `"v": <int>`. A worker meeting an unknown version **parks** it.
- `slack-go/slack` is pinned to an exact version, floor **v0.23.1** (earlier versions accept an
  empty signing secret and therefore forged requests).

**Testing**
- Real Postgres via `testcontainers-go`. Mocked DBs lie about SQL semantics.
- Renderers are pure functions with checked-in `testdata/*.golden.json` + a CI Block Kit validator.
- Real Alertmanager/Grafana/Prometheus/Slack payloads live in `test/fixtures/` and are replayed
  deterministically.
- **Write `test/load/storm_test.go` (a 5 000-alert batch) before the feature that handles it.**

**Ingest path — the rules that must never be broken**
- 202 on accept. **503 + `Retry-After` for anything transient. NEVER 429. NEVER 4xx.**
  4xx means Alertmanager permanently deletes the alert.
- No outbound network call on the ingest path. p99 < 250 ms.
- Two connection pools: `ingest` (25 %, 2 s statement timeout, 500 ms acquire) and `general`.
  UI queries must never starve ingestion.
- A 2xx is a promise. Never return 2xx for a payload we failed to persist.

**Slack**
- Title is a `section`, never a `header` (header is `plain_text` — no bold, no links).
- Exactly **one** attachment wraps all blocks. **Colour = STATE. Emoji = severity.**
- `chat.update` in place is primary; thread replies are the exception.
  `repeat interval elapsed` → **update only, never post.**
- `ts` is TEXT, never a float. The durable handle is `(channel_id, ts)`, and `channel_id` comes
  from the API **response**.
- Top-level `text` is a complete sentence — it is the push notification, the search snippet and
  **the only thing screen readers read.**
- Button `value` is an opaque UUID. Never a payload, never trusted.
- Every URL button still delivers an interaction payload you must ack (`oto.noop.*`).
- **oto never reads Slack back.** Our DB is the memory of Slack.

**Product and ethics (these are requirements, not aspirations)**
- **Default to quiet.** Grouping, flap damping and storm collapse are ON by default.
- Flapping and damping are **visible UI states**, never silent suppression. Silence destroys trust.
- **Never claim resolved when we mean expired.**
- **Delivery failure must be visible per alert.** oto's silence must never be indistinguishable
  from "no alert".
- Acknowledgement identity IS stored (operationally necessary). **No per-person response-time
  metrics, leaderboards or aggregates** — not in the API, not in the rollups. A feature you do
  not build cannot be misused.
- Label redaction is applied **before** the raw payload is persisted. Never log full payloads at
  info level. Retention defaults: raw 14 days, events 13 months, both configurable.
- Surface alert hygiene: "this rule fired 200 times and was never acknowledged" is doing more
  good than any enrichment. **The best alert is the one that no longer exists.**

---

## 7. Where to start reading

| You are building | Read |
|---|---|
| Migrations | SPEC §D in full, §C for the key formats |
| `ingestion` | SPEC §C.5, §G.1–G.4, §G.8; ADRs 0006, 0007 |
| `alerts` | SPEC §A, §B, §C.2–C.3, §C.8, §D.4; ADRs 0003, 0004 |
| `grouping` | SPEC §C.4, §B.6, §D.5; ADR 0005 |
| `rules` | SPEC §C.6, §D.6, §F.4; ADR 0009 |
| `notification` + `channels` | SPEC §F.1–F.2, §G.5–G.7, §H in full; ADR 0008 |
| `streaming` | SPEC §E.4, §D.10; ADR 0010 |
| HTTP API | SPEC §E in full, §L.1–L.2, §L.9, §J |
| The SolidJS UI | SPEC §E, §L.8, **§M in full**; ADR 0012 |
| Anything at all | **SPEC §L** — validation is not optional in any module; ADR 0011 |

`docs/design/domain-research.md` is **verified ground truth** about Alertmanager and Slack. When
your instinct disagrees with it, your instinct is wrong. `docs/design/architect-proposal.md` is
superseded and is kept only for rationale; where it conflicts with SPEC.md, SPEC.md wins.

## 8. If the SPEC is ambiguous

**Raise it. Do not choose.** An undocumented choice made by one implementer is a compile error or
a silent behavioural divergence for the next four. Amendments go through `docs/adr/` plus an edit
to the affected SPEC section in the same commit.
