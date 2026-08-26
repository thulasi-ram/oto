---
title: oto — System Design Proposal
---
> **Status:** SUPERSEDED by [SPEC.md](/oto/design/spec/), which is binding and says so in its own preamble.
> **Audience:** engineering and product.
> **Scope as proposed:** the complete v1 architecture for an alert-first, lifecycle-centric alert
> management platform.

> **HISTORICAL. THIS IS THE ORIGINAL PROPOSAL AND IT IS NOT THE SPECIFICATION.** Where it and
> [SPEC.md](/oto/design/spec/) disagree, the SPEC wins, and `CONTEXT.md` is the current domain vocabulary.
> The proposal is kept because it is the record of what was proposed and why, and reading it as
> current will mislead you: it still describes `AlertOccurrence` (now `AlertCase`), transition **T8**
> (retired by ADR 0040 — a Case is strictly terminal), `refire_grace` and `group_close_delay` (both
> **deleted** by git-bug `7287b28`, migration `00071`), group **generations** and the whole
> `alert_groups` entity (**deleted** by git-bug `7570090`, migration `00069` — the conversation is now
> the Case), thread `state='frozen'` (deleted), and a `pagerduty` channel type oto does not have.

---

## 0. Executive summary

`oto` is an **alert-first, lifecycle-centric** alert management platform for Kubernetes/cloud-native estates.

Where keephq positions itself as an *AIOps workflow engine with alerts as fuel*, `oto` positions itself as **the system of record for the life of an alert**. Our product promise is:

> For every alert that has ever fired in your estate, oto can show you: when it first appeared, every time it fired and resolved since, what the rule actually said at that moment, what Kubernetes objects were involved, who was told, on which channel, in which thread, who acknowledged it, and how it ended — as one continuous, replayable timeline.

Three architectural commitments follow from that promise and drive every decision below:

1. **Append-only truth.** Every observation and every action is an immutable `alert_event`. Current state is a *projection*, never the only record. This is what makes a Sentry-style timeline possible and honest.
2. **Ingestion is decoupled from everything.** The webhook path does exactly two things: durably record the raw payload and enqueue work — in one transaction. Slack, Kubernetes, Prometheus and the enrichment pipeline are *never* on the ingestion critical path.
3. **Interfaces before implementations.** `Channel`, `Renderer`, `Enricher`, `AlertSource` are first-class Go ports with registries. Slack, Alertmanager and the k8s enricher are the first *implementations*, deliberately built as one-of-N rather than as the only one.

Non-goals for v1 (explicitly deferred, but designed so they slot in): on-call schedules and escalation, a general workflow/automation engine, log/trace ingestion, an ML noise-reduction layer, alert authoring/rule management.

---

## 1. Domain model & ubiquitous language

### 1.1 The precision problem

The word "alert" is overloaded to the point of uselessness. Prometheus calls a rule an alert; Alertmanager calls a label set an alert; Alertmanager also calls a *batch* of label sets in a webhook an alert group; humans call a Slack message an alert. We fix this with a strict vocabulary. **These terms are binding on code, DB, API and UI copy.**

### 1.2 Core entities

| Term | Definition | Cardinality intuition |
| --- | --- | --- |
| **AlertSource** | A configured upstream we ingest from: one Alertmanager (or an HA pair/trio), plus optionally its paired Prometheus and its cluster. Owns credentials, poll config and push token. | tens |
| **Cluster** | A logical failure/identity domain. Several `AlertSource`s in an Alertmanager HA cluster share one `ClusterKey`. Optionally carries k8s API access for enrichment. | tens |
| **Alert** | **The identity of a label set.** One row per unique, deduplicated alert identity within an org and cluster — i.e. per fingerprint. It is *long-lived*: it is created the first time that label set is ever seen, and it survives resolution. This is oto's answer to Sentry's *Issue*. | millions (bounded by label cardinality) |
| **AlertOccurrence** | **One contiguous firing episode of an Alert.** Opens when the Alert goes firing, closes when it resolves/expires. `(alert_id, seq)` is stable and human-meaningful ("occurrence #7"). A re-fire after resolve creates occurrence #8 on the *same* Alert. This is the unit that gets a Slack thread. | tens of millions |
| **AlertEvent** | **An immutable, append-only record of one thing that happened at one instant** — a state transition, an enrichment completing, a notification being sent, a human acking, a rule definition changing. Never updated, never deleted (only aged out by partition). This is the timeline. | hundreds of millions |
| **AlertGroup** | **A grouping of Alerts presented and notified as a unit.** Derived either from Alertmanager's `groupKey` or from an oto grouping rule (e.g. `[alertname, cluster, namespace]`). Groups have their own lifecycle and their own timeline (a merge of member events). | millions |
| **Enrichment** | A typed, keyed attachment of derived context to an Alert or an Occurrence, produced by a named `Enricher`, with its own status/TTL/provenance. | 5–15 per occurrence |
| **Channel** | A *configured destination instance* — "Slack workspace T123, channel #sre-alerts". Not a channel *type*. Owns credentials and capability set. | tens |
| **NotificationPolicy** | A matcher → channels → timing rule. Decides *whether*, *where* and *how loudly* a notification happens. | tens |
| **Notification** | **The intent to communicate one fact about one subject.** Channel-agnostic and idempotent ("occurrence 4711 resolved"). Created by the notification service, fanned out to deliveries. | ~2–5 per occurrence |
| **NotificationDelivery** | **One materialisation of a Notification on one Channel.** Owns retry state, provider message identifiers, thread sequence and rendered payload hash. | 1–N per notification |
| **ChannelThread** | Persisted binding of a *subject* (usually an Occurrence) to a provider conversation anchor — e.g. Slack `channel=C0123, thread_ts=1699.0001`. Enables replies. | 1 per (subject, channel) |
| **Incident** *(peripheral)* | A human- or rule-created higher-level correlation over multiple AlertGroups/Alerts, with its own state, title, severity and timeline. Optional; alerts are useful without it. | thousands |
| **Silence** | A suppression window. Either **mirrored** from Alertmanager (read-only view + create-through) or **oto-local** (a mute that suppresses notification but not observation). | thousands |

### 1.3 The three-model rule (layering)

Every domain object exists in **exactly three shapes**, and the compiler must be able to tell them apart:

```
  HTTP/JSON  ──►  api.AlertDTO         (json tags, validation tags, versioned, in <domain>/api)
                       ▲  mapper (api/mapper.go)
  business   ──►  model.Alert          (pure Go, no tags, invariants, state machine, in <domain>/model)
                       ▲  mapper (repository/mapper.go)
  storage    ──►  repository.alertRow  (db tags, sqlc-generated, nullable types, in <domain>/repository)
```

Hard rules, enforced by `golangci-lint` + `depguard` + an architecture test:

- `<domain>/api` **must not** import `<domain>/repository`.
- `<domain>/repository` **must not** import `<domain>/api`.
- `<domain>/model` imports neither, and imports no I/O library at all (no `pgx`, no `net/http`).
- `<domain>/service` imports `model` and *port interfaces*; the concrete repository is injected.
- Cross-domain calls go **service → service** through an interface declared by the *consumer*, never repository → repository.

### 1.4 Why `Occurrence` and `Event` are separate (a decision, not an accident)

The brief treats `AlertOccurrence`/`AlertEvent` as one concept. We deliberately split them:

- **Occurrence** is a *stateful interval* with a start and an end. It is what you ack, what you thread, what you count for MTTR. It has ~10 rows per alert per month.
- **Event** is a *stateless instant*. It is what you render on a timeline. It has ~50–500 rows per occurrence.

Collapsing them forces one of two bad outcomes: either you mutate timeline rows (destroying the audit trail), or you have no natural key for "the thing this Slack thread is about". Keeping them separate gives us an immutable log *and* a cheap, indexable projection.

**Rule of thumb for engineers:** *if you would ever want to UPDATE it, it is not an Event.*

---

## 2. Alert lifecycle state machine

### 2.1 Two orthogonal axes (decision)

The single most common modelling error in this space is jamming acknowledgement into the same enum as firing/resolved. An acked alert is still firing. A resolved alert can still be unacked. We therefore model:

- **`occurrence.state`** — *what the world is doing.* Owned by ingestion/reconciliation. Enum: `firing | suppressed | resolved | expired`.
- **`occurrence.ack_state`** — *what humans have done.* Owned by the API. Enum: `unacked | acked`. Carries `acked_by`, `acked_at`, `ack_note`, `ack_expires_at`.
- **`occurrence.closed_at`** — *operator says "we're done here"*, independent of both. Closing an occurrence with an alert still firing is legal and useful (known-bad, tracked elsewhere).
- **`alert.flap_score`** — a derived signal, **not** a state.

### 2.2 Occurrence states

| State | Meaning | Entered by |
| --- | --- | --- |
| `firing` | Alertmanager reports this label set active and not suppressed. | Ingest (push/pull) |
| `suppressed` | Active, but suppressed by an Alertmanager silence, an inhibition rule, an oto-local mute, or a maintenance window. Sub-field `suppression_reason ∈ {silenced, inhibited, muted, maintenance}`. | Ingest / Silences service |
| `resolved` | Alertmanager explicitly told us `status=resolved`, or the alert's `endsAt` passed with a resolve observation. **Terminal.** | Ingest |
| `expired` | We stopped hearing about it. `endsAt` elapsed and `resolve_timeout` grace passed without a resolve. Distinct from `resolved` because it usually means *Prometheus or Alertmanager went away*, not *the problem went away*. **Terminal.** | Reaper job |

`Alert.state` is a denormalised projection = state of the current open occurrence, or `resolved`/`expired` if none is open.

### 2.3 Transition table

| # | From | To | Trigger | Actor | Side effects |
| --- | --- | --- | --- | --- | --- |
| T1 | *(none)* | `firing` | First observation of a fingerprint, or observation of a firing alert with no open occurrence | Ingest | Create `Alert` if new; open occurrence `seq=n+1`; emit `alert.created?`, `occurrence.opened`; enqueue fast-enrich; enqueue notify(`fired`) |
| T2 | `firing` | `firing` | Repeat observation (heartbeat / re-send / `updatedAt` bump) | Ingest | Update `last_observed_at`, `annotations`, `value`; **no** event unless a *material* field changed (severity, annotations, generatorURL, rule hash) → then emit `alert.mutated` |
| T3 | `firing` | `suppressed` | AM reports `status.state=suppressed` / matching silence appears / oto mute created | Ingest, Silences | Emit `occurrence.suppressed`; notify(`suppressed`) as **thread reply**; stop escalation timers |
| T4 | `suppressed` | `firing` | Silence expires/deleted; AM reports active again | Ingest, Silences | Emit `occurrence.unsuppressed`; thread reply; re-arm escalation |
| T5 | `firing`\|`suppressed` | `resolved` | AM `status=resolved` (push) or absent-with-endsAt-past (pull) | Ingest, Reconciler | Close occurrence (`ended_at`, `resolve_reason=upstream`); emit `occurrence.resolved`; notify(`resolved`) as thread reply + root message update; freeze thread |
| T6 | `firing`\|`suppressed` | `expired` | `now > endsAt + resolve_grace` and no observation | Reaper (periodic job) | Close occurrence (`resolve_reason=timeout`); emit `occurrence.expired`; notify(`expired`) if policy says so |
| T7 | `resolved`\|`expired` | *(new occurrence `firing`)* | Same fingerprint fires again **after** `refire_grace` | Ingest | **New occurrence**, `seq+1`, **new root message / new thread**; emit `occurrence.opened` with `reopen_of=<prev>`; bump `alert.total_occurrences`; recompute flap score |
| T8 | `resolved`\|`expired` | `firing` *(same occurrence)* | Same fingerprint fires again **within** `refire_grace` (default 10 min) | Ingest | **Reopen the same occurrence**: clear `ended_at`, `+1 reopen_count`; emit `occurrence.reopened`; post **thread reply** ("re-fired after 3m"); do *not* create a new thread |
| T9 | any | `ack_state=acked` | Human via API / Slack button / Slack `/oto ack` | Human | Emit `alert.acknowledged`; thread reply with actor; pause escalation until `ack_expires_at` |
| T10 | `acked` | `unacked` | Human unack, or `ack_expires_at` passes, or a **new occurrence** opens | Human, Reaper, Ingest | Emit `alert.unacknowledged` with reason |
| T11 | any | `closed_at` set | Human "close" | Human | Emit `alert.closed`; suppress further notifications for this occurrence; root message updated |
| T12 | any | *(no state change)* | Enricher finishes | Enrichment worker | Emit `enrichment.completed`/`failed`; if phase-2, thread reply + root message amend |
| T13 | any | *(no state change)* | Notification/delivery progresses | Notify/Deliver workers | Emit `notification.created`, `delivery.sent`, `delivery.failed`, `delivery.suppressed` |
| T14 | any | *(no state change)* | Rule definition hash differs from previous occurrence | `promrule` enricher | Emit `rule.definition_changed` with a diff — **a headline differentiator** |

### 2.4 Re-fire: the decision, stated plainly

> **A re-fire after resolve is a NEW `AlertOccurrence` on the SAME `Alert` (same fingerprint), and a NEW notification thread — unless it happens within `refire_grace`, in which case it REOPENS the existing occurrence and posts into the existing thread.**

Rationale:

- Same `Alert` because the identity (label set) is identical; splitting would destroy "this has fired 47 times this quarter", which is the product.
- New occurrence because MTTR, ack, and "who was on it" are per-episode, not per-identity.
- New thread because a Slack thread that has been dormant for two days is invisible; re-using it hides a live incident.
- The `refire_grace` exception exists because Prometheus `for:` + scrape jitter routinely produces resolve→fire flaps within minutes, and one thread per flap is exactly the noise we are here to kill.
- Beyond `flap_threshold` occurrences within `flap_window` (default 5 in 30m), the Alert is marked flapping: occurrences still open normally, but the notification policy switches to a **digest** mode (one thread, periodic summary replies) until it stabilises.

### 2.5 AlertGroup lifecycle

Groups have a much simpler machine: `open` (≥1 member occurrence not resolved/expired) → `closed` (all members terminal, after `group_close_delay`, default 5m to avoid churn) → `reopened` (a member fires again while closed, within `group_refire_grace`) or a **new group generation** beyond it (`group.generation` increments). Group generation is what the UI shows as one timeline card.

---

## 3. Deduplication & fingerprinting

### 3.1 What Alertmanager gives us

- **`fingerprint`** (webhook v4 and API v2): a 64-bit FNV-1a hash over the *sorted, complete* label set. Deterministic, stable, identical across push and pull, and identical across members of an Alertmanager HA cluster (it is computed from labels alone, not from AM state).
- **`groupKey`**: a string derived from the routing tree path plus the `group_by` labels. Stable per AM *config*; changes the moment someone edits `alertmanager.yml`.

### 3.2 Decision: trust, but recompute and store both

> **We store Alertmanager's `fingerprint` verbatim as `source_fingerprint`, and we independently compute our own `dedup_key` which is the real dedupe identity. Both are indexed.**

```go
// pkg/alertmodel
func DedupKey(orgID uuid.UUID, clusterKey string, labels LabelSet, policy IdentityPolicy) string {
    ls := policy.Canonicalize(labels) // drop ignored labels, lowercase names, sort
    h := sha256.New()
    h.Write([]byte(orgID.String())); h.Write([]byte{0})
    h.Write([]byte(clusterKey));     h.Write([]byte{0})
    for _, l := range ls {           // sorted
        h.Write([]byte(l.Name)); h.Write([]byte{0x01})
        h.Write([]byte(l.Value)); h.Write([]byte{0x02})
    }
    return "dk_" + base32Lower(h.Sum(nil)[:16]) // 128-bit, url-safe, human-copyable
}
```

Why not just use AM's fingerprint?

1. **64 bits is too few at our horizon.** At tens of millions of distinct series across an org's history, a 64-bit birthday collision is ~1-in-a-few-thousand. A silent identity merge is the worst possible bug in this product. 128 bits removes the concern entirely.
2. **Scoping.** AM's fingerprint has no tenant and no cluster in it. Two clusters with identical `KubePodCrashLooping{namespace="prod",pod="api-0"}` are *different problems*. Our key is scoped by `(org, clusterKey)`.
3. **Identity policy.** We must be able to say "`prometheus_replica` is not part of identity" without asking anyone to change Alertmanager config. AM cannot.
4. **We still keep theirs** because it is the join key for `/api/v2/alerts`, `/api/v2/silences` matching, and debugging against upstream. Reconciliation joins on `source_fingerprint`; product logic uses `dedup_key`.

**Runner-up:** use AM's fingerprint directly as the primary key. Tradeoff: dramatically simpler reconciliation, but no tenant/cluster scoping and no identity policy — we'd be permanently hostage to upstream label hygiene.

### 3.3 Cluster scoping and HA Alertmanagers

`AlertSource` carries a `cluster_key` (operator-assigned, defaults to a slug of the source name). **All members of an Alertmanager HA cluster must be registered with the same `cluster_key`.** Because AM gossips notification state but each replica may webhook independently, the same alert can arrive from `am-0`, `am-1` and `am-2`. Sharing `cluster_key` makes those three arrivals collapse into one `Alert`, one occurrence, one Slack thread. The `source_id` of each observation is still recorded on the `alert_observations`/event row, so "which replica told us" is answerable.

Cross-cluster: **never** merged automatically. If `cluster` is not already a label, `AlertSource` can inject `external_labels` at ingest (`inject_labels` config) so the label set self-describes. Correlating the *same problem* across clusters is what `Incident` is for — a human/rule decision, never an implicit dedupe.

### 3.4 Label cardinality defence

Cardinality is the #1 way this system dies. Defences, in order of application at ingest:

1. **Hard caps (reject + count):** > 64 labels, any label value > 4 KiB, total label set > 16 KiB → the observation is recorded in `ingest_rejections` with the reason and the raw payload, and a `source.cardinality_violation` metric fires. We never silently drop.
2. **Identity policy per org/source:** `ignore_labels` deny-list (default: `prometheus_replica`, `__replica__`, `monitor`, `replica`, `pod_template_hash`) or, for advanced users, an `identity_labels` allow-list. Ignored labels are still *stored* on the alert (in `labels`), just not hashed into identity. This is reversible: changing the policy triggers a background re-key job that rewrites `dedup_key` and merges the affected alerts, recording an `alert.merged` event.
3. **Series budget:** a rolling counter of distinct `dedup_key`s per `(org, alertname)` per 24h. Past `cardinality_budget` (default 5 000), new series for that alertname are **quarantined**: still counted and visible in a "noisy rule" report, but they do not create Alert rows and do not notify. An `alert.quarantined` warning surfaces in the UI with a one-click "raise budget" / "add ignore label".
4. **Promoted columns:** the five labels we filter on constantly (`alertname`, `severity`, `namespace`, `cluster`, `service`) are extracted into real columns with btree indexes; the rest live in `labels JSONB` behind a `jsonb_path_ops` GIN index. This is what keeps the list query fast without indexing arbitrary user labels.

### 3.5 Group key strategy

We compute `group_key = hash(org, cluster_key, grouping_rule_id, group_label_values)` from an **oto grouping rule**, and store AM's `groupKey` alongside as `source_group_key`. Reason: AM's group key silently changes when someone edits routing, which would orphan every open thread. Default grouping rule is `[alertname, cluster, namespace]`, configurable per org, and the UI lets you *view* by any label set (client-driven grouping is a query concern, distinct from the persisted notification grouping).

---

## 4. Module decomposition

Layout: `internal/<domain>/{api,service,repository,model}`.

> **One deliberate deviation from the brief:** the brief says `src/<domain>/...`. We use `internal/<domain>/...`. `internal/` is identical in shape but additionally gives *compiler-enforced* encapsulation — nothing outside the module can import it, which is exactly the boundary discipline the brief is asking for. Public, importable contracts live in `pkg/`. **Flagged for product-owner sign-off (§13).**

### 4.1 CORE PLATFORM modules

| Module | One-line responsibility | Sub-packages |
| --- | --- | --- |
| `identity` | Orgs, users, memberships, API keys, sessions, roles — the tenancy and authn/authz root. | `api` (auth endpoints, org/user/key CRUD) · `service` (OrgService, UserService, TokenService, AuthzService) · `repository` (orgs, users, memberships, api_keys, sessions) · `model` |
| `sources` | Registry and health of upstream Alertmanager/Prometheus/cluster endpoints, their credentials, poll config and push tokens. | `api` (source CRUD, test-connection, health) · `service` (SourceService, HealthService, CredentialService) · `repository` (alert_sources, clusters, source_health) · `model` · `client/` (AM v2 + Prom v1 HTTP clients) |
| `ingestion` | Accept alerts by PUSH and PULL, persist raw batches durably, normalise them into canonical observations — and nothing else. | `api` (webhook receiver, batch inspection) · `service` (ReceiverService, NormalizerService, PollService, ReconcilerService) · `repository` (ingest_batches, ingest_rejections) · `model` · `alertmanager/` (payload v4 + API v2 decoders) |
| `alerts` | The heart: alert identity/dedup, occurrence lifecycle state machine, and the append-only event timeline. | `api` (list/get alerts, occurrences, events, ack/close/comment/snooze) · `service` (AlertService, OccurrenceService, LifecycleService, TimelineService, FlapService) · `repository` (alerts, alert_occurrences, alert_events) · `model` (state machine, transitions, event types) |
| `grouping` | Compute group keys from grouping rules, maintain group membership and group generations. | `api` (group list/get/timeline, grouping-rule CRUD) · `service` (GroupingService, GroupLifecycleService) · `repository` (alert_groups, alert_group_members, grouping_rules) · `model` |
| `enrichment` | Run the pluggable enricher pipeline with budgets and caching; store typed, provenanced results. | `api` (list enrichers, get/refresh enrichments) · `service` (PipelineService, RegistryService, CacheService) · `repository` (enrichments, enrichment_cache) · `model` · `enrichers/` (one package per enricher) |
| `notification` | Match policies, create idempotent notification intents, fan out to deliveries, own retries and thread ordering. | `api` (policy CRUD + dry-run preview, notification/delivery list, retry) · `service` (PolicyService, NotificationService, DispatchService, ThreadService, ThrottleService) · `repository` (notification_policies, notifications, notification_deliveries, channel_threads) · `model` |
| `channels` | The Channel/Renderer ports, the provider registry, credential handling, and provider implementations. | `api` (channel CRUD, test-send, provider descriptors) · `service` (ChannelService, RegistryService, RenderService) · `repository` (channels, channel_credentials) · `model` (`Channel`, `Provider`, `Renderer`, `Capability`) · `providers/{slack,webhook,pagerduty,teams}` · `render/{slack,markdown,text}` |
| `streaming` | Live UI updates: durable UI event log, Postgres LISTEN/NOTIFY bridge, SSE hub with resume. | `api` (`GET /stream`) · `service` (HubService, PublisherService) · `repository` (ui_events) · `model` |
| `platform` *(not a domain)* | Cross-cutting machinery every domain uses: config, logging, telemetry, db/tx, http, auth middleware, jobs, secrets, errors, pagination, rate limiting. | see §12 tree |

### 4.2 PERIPHERAL modules

| Module | One-line responsibility | Sub-packages |
| --- | --- | --- |
| `silences` | Mirror Alertmanager silences, create/expire them through oto, and own oto-local mutes and maintenance windows. | `api` · `service` (SilenceMirrorService, MuteService, MaintenanceService) · `repository` (silences, mutes, maintenance_windows) · `model` |
| `incidents` | Optional higher-level correlation: bundle groups/alerts into a named incident with its own state and timeline. | `api` · `service` (IncidentService, CorrelationService) · `repository` (incidents, incident_members, incident_events) · `model` |
| `k8scontext` | Resolve Kubernetes objects (pod→deployment→owner, node, events) from alert labels via cached informers; consumed by an enricher. | `service` (ObjectResolver, InformerManager) · `repository` (k8s_object_cache) · `model` · `client/` |
| `promcontext` | Fetch and version alert-rule definitions and evaluate context queries against Prometheus; consumed by enrichers. | `service` (RuleService, QueryService, RuleVersionService) · `repository` (prom_rules, prom_rule_versions) · `model` |
| `changefeed` | Ingest deploy/change events (Argo/Flux/GitHub/generic webhook) so alerts can be correlated with what shipped. | `api` (change webhook + CRUD) · `service` (ChangeService, CorrelationService) · `repository` (change_events) · `model` |
| `views` | Saved filters/views, per-user UI preferences, column layouts. | `api` · `service` · `repository` (saved_views, user_prefs) · `model` |
| `analytics` | Rollups and reports: alert volume, MTTA/MTTR, noisiest rules, flap leaderboard, notification success rates. | `api` · `service` (RollupService, ReportService) · `repository` (alert_daily_rollup, notif_daily_rollup) · `model` |
| `audit` | Immutable record of every mutating human/API action, queryable and exportable. | `api` · `service` · `repository` (audit_log) · `model` |
| `oncall` *(stub in v1)* | Placeholder port for schedule/escalation resolution so `NotificationPolicy` can already target "the on-call for team X". | `service` (`Resolver` interface + `StaticResolver` impl only) · `model` |

**Dependency direction (enforced):**

```
ingestion ─► alerts ─► grouping ─► notification ─► channels
                │           │            │
                ▼           ▼            ▼
           enrichment    streaming     silences
                │
        k8scontext, promcontext, changefeed
```

No cycles. `alerts` never imports `notification`; it emits events, and `notification` subscribes via jobs. This is what makes it possible to run oto with notifications entirely disabled.

---

## 5. PostgreSQL schema

Conventions: `uuidv7` primary keys (time-ordered → index locality, and sortable as a tiebreaker cursor), `TIMESTAMPTZ` everywhere, `org_id` first in every composite index, `CITEXT` for names, `JSONB` for label sets and payloads, `created_at`/`updated_at` on mutable tables only.

### 5.1 Tenancy & sources

```sql
CREATE TABLE orgs (
  id            UUID PRIMARY KEY,
  slug          CITEXT NOT NULL UNIQUE,
  name          TEXT   NOT NULL,
  settings      JSONB  NOT NULL DEFAULT '{}',  -- refire_grace, flap thresholds, identity policy defaults
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at    TIMESTAMPTZ
);

CREATE TABLE clusters (
  id            UUID PRIMARY KEY,
  org_id        UUID NOT NULL REFERENCES orgs(id),
  cluster_key   TEXT NOT NULL,                 -- identity domain for dedupe
  display_name  TEXT NOT NULL,
  kube_config   BYTEA,                         -- sealed; nullable (no k8s enrichment)
  UNIQUE (org_id, cluster_key)
);

CREATE TABLE alert_sources (
  id                UUID PRIMARY KEY,
  org_id            UUID NOT NULL REFERENCES orgs(id),
  cluster_id        UUID NOT NULL REFERENCES clusters(id),
  name              CITEXT NOT NULL,
  kind              TEXT NOT NULL,             -- 'alertmanager'
  base_url          TEXT NOT NULL,
  prometheus_url    TEXT,
  auth              JSONB NOT NULL DEFAULT '{}',  -- sealed secret refs
  inject_labels     JSONB NOT NULL DEFAULT '{}',
  identity_policy   JSONB NOT NULL DEFAULT '{}',  -- ignore_labels / identity_labels
  push_enabled      BOOLEAN NOT NULL DEFAULT true,
  push_token_hash   BYTEA,                     -- argon2id of the ingest bearer token
  pull_enabled      BOOLEAN NOT NULL DEFAULT true,
  pull_interval_s   INT NOT NULL DEFAULT 30,
  last_pull_at      TIMESTAMPTZ,
  last_pull_status  TEXT,
  UNIQUE (org_id, name)
);
CREATE UNIQUE INDEX ON alert_sources (push_token_hash) WHERE push_token_hash IS NOT NULL;
```

### 5.2 Ingestion (raw, partitioned, short retention)

```sql
CREATE TABLE ingest_batches (
  id            UUID NOT NULL,
  org_id        UUID NOT NULL,
  source_id     UUID NOT NULL,
  mode          TEXT NOT NULL,          -- 'push' | 'pull'
  received_at   TIMESTAMPTZ NOT NULL,
  checksum      BYTEA NOT NULL,         -- sha256 of canonical body -> retry dedupe
  alert_count   INT  NOT NULL,
  payload       JSONB NOT NULL,
  status        TEXT NOT NULL,          -- 'pending'|'processed'|'failed'|'partial'
  processed_at  TIMESTAMPTZ,
  error         TEXT,
  PRIMARY KEY (id, received_at)
) PARTITION BY RANGE (received_at);      -- DAILY partitions, 14-day retention

CREATE UNIQUE INDEX ON ingest_batches (source_id, checksum, received_at);
CREATE INDEX ON ingest_batches (status, received_at) WHERE status IN ('pending','failed');

CREATE TABLE ingest_rejections (LIKE ingest_batches INCLUDING ALL) PARTITION BY RANGE (received_at);
```

### 5.3 Alerts, occurrences, events

```sql
CREATE TABLE alerts (
  id                    UUID PRIMARY KEY,
  org_id                UUID NOT NULL,
  cluster_id            UUID NOT NULL,
  dedup_key             TEXT NOT NULL,             -- our 128-bit identity
  source_fingerprint    TEXT NOT NULL,             -- Alertmanager's, for joins
  -- promoted labels (hot filters)
  alertname             TEXT NOT NULL,
  severity              TEXT,
  namespace             TEXT,
  service               TEXT,
  cluster_key           TEXT NOT NULL,
  -- full data
  labels                JSONB NOT NULL,
  annotations           JSONB NOT NULL DEFAULT '{}',
  generator_url         TEXT,
  -- projection of current occurrence
  state                 TEXT NOT NULL,             -- firing|suppressed|resolved|expired
  current_occurrence_id UUID,
  ack_state             TEXT NOT NULL DEFAULT 'unacked',
  -- history
  first_seen_at         TIMESTAMPTZ NOT NULL,
  last_seen_at          TIMESTAMPTZ NOT NULL,
  last_state_change_at  TIMESTAMPTZ NOT NULL,
  total_occurrences     INT NOT NULL DEFAULT 0,
  flap_score            REAL NOT NULL DEFAULT 0,
  quarantined_at        TIMESTAMPTZ,
  UNIQUE (org_id, dedup_key)
);

CREATE INDEX alerts_list_idx        ON alerts (org_id, state, last_seen_at DESC, id DESC);
CREATE INDEX alerts_open_idx        ON alerts (org_id, last_seen_at DESC, id DESC)
                                     WHERE state IN ('firing','suppressed');
CREATE INDEX alerts_name_idx        ON alerts (org_id, alertname, last_seen_at DESC);
CREATE INDEX alerts_ns_idx          ON alerts (org_id, cluster_key, namespace, state);
CREATE INDEX alerts_sev_idx         ON alerts (org_id, severity, state, last_seen_at DESC);
CREATE INDEX alerts_labels_gin      ON alerts USING GIN (labels jsonb_path_ops);
CREATE INDEX alerts_text_idx        ON alerts USING GIN (
    to_tsvector('simple', alertname || ' ' || coalesce(annotations->>'summary','')));
CREATE INDEX alerts_srcfp_idx       ON alerts (org_id, cluster_key, source_fingerprint);

CREATE TABLE alert_occurrences (
  id                UUID PRIMARY KEY,
  org_id            UUID NOT NULL,
  alert_id          UUID NOT NULL REFERENCES alerts(id),
  seq               INT  NOT NULL,               -- 1,2,3... per alert
  state             TEXT NOT NULL,
  suppression_reason TEXT,
  started_at        TIMESTAMPTZ NOT NULL,
  ended_at          TIMESTAMPTZ,
  last_observed_at  TIMESTAMPTZ NOT NULL,
  source_starts_at  TIMESTAMPTZ NOT NULL,        -- AM startsAt
  source_ends_at    TIMESTAMPTZ,                 -- AM endsAt (resolve deadline)
  source_updated_at TIMESTAMPTZ,                 -- AM updatedAt, for LWW ordering
  resolve_reason    TEXT,                        -- upstream|timeout|manual
  reopen_count      INT NOT NULL DEFAULT 0,
  ack_state         TEXT NOT NULL DEFAULT 'unacked',
  acked_by          UUID, acked_at TIMESTAMPTZ, ack_note TEXT, ack_expires_at TIMESTAMPTZ,
  closed_by         UUID, closed_at TIMESTAMPTZ,
  notified_at       TIMESTAMPTZ,
  group_id          UUID,
  rule_hash         TEXT,                        -- hash of rule expr at fire time
  value             DOUBLE PRECISION,
  UNIQUE (alert_id, seq)
);

-- INVARIANT: at most one open occurrence per alert. Enforced in the DB, not in Go.
CREATE UNIQUE INDEX occ_one_open_idx ON alert_occurrences (alert_id) WHERE ended_at IS NULL;
CREATE INDEX occ_alert_idx  ON alert_occurrences (org_id, alert_id, seq DESC);
CREATE INDEX occ_group_idx  ON alert_occurrences (org_id, group_id, started_at DESC);
CREATE INDEX occ_reap_idx   ON alert_occurrences (source_ends_at)
                             WHERE ended_at IS NULL AND source_ends_at IS NOT NULL;

CREATE TABLE alert_events (
  id            UUID NOT NULL,                   -- uuidv7 => time-sortable
  org_id        UUID NOT NULL,
  alert_id      UUID,
  occurrence_id UUID,
  group_id      UUID,
  incident_id   UUID,
  type          TEXT NOT NULL,                   -- occurrence.opened, delivery.sent, ...
  at            TIMESTAMPTZ NOT NULL,
  actor_kind    TEXT NOT NULL,                   -- system|ingest|reconciler|enricher|notifier|user|slack
  actor_id      TEXT,
  actor_label   TEXT,                            -- denormalised display name, immutable
  summary       TEXT NOT NULL,                   -- pre-rendered one-liner for the timeline
  payload       JSONB NOT NULL DEFAULT '{}',
  dedupe_key    TEXT,                            -- optional; makes event writes idempotent
  PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);                       -- MONTHLY partitions

CREATE INDEX ev_alert_idx  ON alert_events (org_id, alert_id, at DESC, id DESC);
CREATE INDEX ev_occ_idx    ON alert_events (org_id, occurrence_id, at DESC);
CREATE INDEX ev_group_idx  ON alert_events (org_id, group_id, at DESC, id DESC);
CREATE UNIQUE INDEX ev_dedupe_idx ON alert_events (org_id, dedupe_key, at) WHERE dedupe_key IS NOT NULL;
```

### 5.4 Groups, enrichment, notification, channels

```sql
CREATE TABLE grouping_rules (
  id UUID PRIMARY KEY, org_id UUID NOT NULL, name CITEXT NOT NULL,
  matchers JSONB NOT NULL DEFAULT '[]', group_by TEXT[] NOT NULL,
  priority INT NOT NULL DEFAULT 100, enabled BOOLEAN NOT NULL DEFAULT true,
  UNIQUE (org_id, name)
);

CREATE TABLE alert_groups (
  id UUID PRIMARY KEY, org_id UUID NOT NULL, cluster_id UUID NOT NULL,
  grouping_rule_id UUID NOT NULL, group_key TEXT NOT NULL, generation INT NOT NULL DEFAULT 1,
  source_group_key TEXT, group_labels JSONB NOT NULL, title TEXT NOT NULL,
  state TEXT NOT NULL,                       -- open|closed
  severity TEXT,                             -- max member severity
  open_alert_count INT NOT NULL DEFAULT 0, total_alert_count INT NOT NULL DEFAULT 0,
  first_seen_at TIMESTAMPTZ NOT NULL, last_activity_at TIMESTAMPTZ NOT NULL, closed_at TIMESTAMPTZ,
  incident_id UUID,
  UNIQUE (org_id, group_key, generation)
);
CREATE INDEX grp_list_idx ON alert_groups (org_id, state, last_activity_at DESC, id DESC);

CREATE TABLE alert_group_members (
  group_id UUID NOT NULL, alert_id UUID NOT NULL, occurrence_id UUID NOT NULL,
  org_id UUID NOT NULL, joined_at TIMESTAMPTZ NOT NULL, left_at TIMESTAMPTZ,
  PRIMARY KEY (group_id, occurrence_id)
);
CREATE INDEX gm_alert_idx ON alert_group_members (org_id, alert_id, joined_at DESC);

CREATE TABLE enrichments (
  id UUID PRIMARY KEY, org_id UUID NOT NULL,
  subject_kind TEXT NOT NULL,                -- alert|occurrence|group
  subject_id UUID NOT NULL,
  enricher TEXT NOT NULL, enricher_version INT NOT NULL, phase SMALLINT NOT NULL,
  status TEXT NOT NULL,                      -- ok|partial|failed|skipped|timeout
  payload JSONB NOT NULL DEFAULT '{}',
  error TEXT, duration_ms INT, from_cache BOOLEAN NOT NULL DEFAULT false,
  computed_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ,
  UNIQUE (subject_kind, subject_id, enricher)
);
CREATE INDEX enr_subject_idx ON enrichments (org_id, subject_kind, subject_id);

CREATE TABLE enrichment_cache (
  cache_key TEXT PRIMARY KEY, org_id UUID NOT NULL,
  payload JSONB NOT NULL, computed_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX enr_cache_exp_idx ON enrichment_cache (expires_at);

CREATE TABLE channels (
  id UUID PRIMARY KEY, org_id UUID NOT NULL, type TEXT NOT NULL, name CITEXT NOT NULL,
  config JSONB NOT NULL,                     -- non-secret; validated against provider JSON Schema
  credential_id UUID, capabilities BIGINT NOT NULL DEFAULT 0,
  renderer TEXT NOT NULL DEFAULT 'default',
  enabled BOOLEAN NOT NULL DEFAULT true,
  health_status TEXT, health_checked_at TIMESTAMPTZ,
  UNIQUE (org_id, name)
);

CREATE TABLE channel_credentials (
  id UUID PRIMARY KEY, org_id UUID NOT NULL, kind TEXT NOT NULL,
  sealed BYTEA NOT NULL, key_version INT NOT NULL, rotated_at TIMESTAMPTZ
);

CREATE TABLE notification_policies (
  id UUID PRIMARY KEY, org_id UUID NOT NULL, name CITEXT NOT NULL,
  priority INT NOT NULL, enabled BOOLEAN NOT NULL DEFAULT true,
  matchers JSONB NOT NULL,                   -- label matchers + state/severity predicates
  reasons TEXT[] NOT NULL,                   -- which notification reasons this policy emits
  channel_ids UUID[] NOT NULL,
  group_wait_s INT NOT NULL DEFAULT 15, repeat_interval_s INT NOT NULL DEFAULT 0,
  throttle JSONB NOT NULL DEFAULT '{}',      -- max N per window per subject
  continue_matching BOOLEAN NOT NULL DEFAULT false,
  UNIQUE (org_id, name)
);

CREATE TABLE notifications (
  id UUID PRIMARY KEY, org_id UUID NOT NULL,
  subject_kind TEXT NOT NULL, subject_id UUID NOT NULL,   -- usually 'occurrence'
  alert_id UUID, group_id UUID, occurrence_id UUID,
  reason TEXT NOT NULL,                      -- fired|resolved|expired|suppressed|unsuppressed
                                             -- |reopened|acked|closed|enriched|digest|escalated
  policy_id UUID, state_version INT NOT NULL,
  idempotency_key TEXT NOT NULL,
  status TEXT NOT NULL,                      -- pending|dispatched|partial|delivered|failed|suppressed
  suppressed_reason TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE (org_id, idempotency_key)
);
CREATE INDEX notif_subject_idx ON notifications (org_id, subject_kind, subject_id, created_at DESC);

CREATE TABLE channel_threads (
  id UUID PRIMARY KEY, org_id UUID NOT NULL, channel_id UUID NOT NULL,
  subject_kind TEXT NOT NULL, subject_id UUID NOT NULL,
  provider_conversation_id TEXT,             -- Slack C0123
  provider_thread_id TEXT,                   -- Slack root ts
  root_delivery_id UUID,
  last_sent_seq INT NOT NULL DEFAULT 0,      -- ordering gate
  next_seq      INT NOT NULL DEFAULT 1,      -- allocator
  state TEXT NOT NULL DEFAULT 'opening',     -- opening|open|frozen|failed
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE (channel_id, subject_kind, subject_id)
);

CREATE TABLE notification_deliveries (
  id UUID PRIMARY KEY, org_id UUID NOT NULL,
  notification_id UUID NOT NULL REFERENCES notifications(id),
  channel_id UUID NOT NULL, thread_id UUID REFERENCES channel_threads(id),
  thread_seq INT,                            -- FIFO position within the thread
  role TEXT NOT NULL,                        -- root|reply|amend
  status TEXT NOT NULL,                      -- pending|sending|sent|failed|dead|skipped
  attempts INT NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ,
  rendered JSONB, rendered_hash TEXT,        -- skip no-op amends
  provider_message_id TEXT, provider_ts TEXT, provider_response JSONB,
  error TEXT, error_class TEXT,              -- retryable|permanent|ratelimited
  sent_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL,
  UNIQUE (notification_id, channel_id)       -- idempotent fan-out
);
CREATE INDEX del_thread_seq_idx ON notification_deliveries (thread_id, thread_seq)
                                  WHERE status IN ('pending','sending');
```

### 5.5 UI event log (SSE resume)

```sql
CREATE TABLE ui_events (
  seq        BIGSERIAL,                       -- monotonic; the SSE Last-Event-ID
  org_id     UUID NOT NULL,
  kind       TEXT NOT NULL,                   -- alert.upserted|group.upserted|event.appended|...
  resource   TEXT NOT NULL, resource_id UUID NOT NULL,
  payload    JSONB NOT NULL,                  -- small envelope; client re-reads if it needs more
  at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (seq, at)
) PARTITION BY RANGE (at);                    -- HOURLY partitions, 24h retention
CREATE INDEX ui_ev_org_idx ON ui_events (org_id, seq);
```

### 5.6 Critical query patterns

**(a) Alert list with filters** — the single hottest query.

```sql
SELECT a.* FROM alerts a
WHERE a.org_id = $1
  AND a.state = ANY($2)                        -- alerts_open_idx / alerts_list_idx
  AND ($3::text IS NULL OR a.severity = $3)
  AND ($4::text[] IS NULL OR a.namespace = ANY($4))
  AND ($5::jsonb = '{}'::jsonb OR a.labels @> $5)   -- alerts_labels_gin
  AND (a.last_seen_at, a.id) < ($6, $7)        -- keyset cursor
ORDER BY a.last_seen_at DESC, a.id DESC
LIMIT $8;
```
Planner path: `alerts_open_idx` for the common "open alerts" case (partial index keeps it tiny — open alerts are a rounding error next to total alerts), GIN bitmap-AND when arbitrary labels are filtered. **Keyset, never OFFSET.** Total counts are served from `alert_daily_rollup`, never `COUNT(*)`.

**(b) Timeline for a group** — the signature UI view.

```sql
SELECT e.* FROM alert_events e
WHERE e.org_id = $1 AND e.group_id = $2
  AND e.at >= $3                               -- ALWAYS bounded => partition pruning
  AND (e.at, e.id) < ($4, $5)
ORDER BY e.at DESC, e.id DESC
LIMIT $6;
```
`ev_group_idx` + monthly partition pruning. The API **requires** a time bound (defaulting to the group's `first_seen_at`), so we never scan every partition.

**(c) Dedupe lookup on ingest** — the hottest write-path read.

```sql
-- One statement per observation; no read-then-write race.
INSERT INTO alerts (id, org_id, ..., dedup_key, ...)
VALUES (...)
ON CONFLICT (org_id, dedup_key)
DO UPDATE SET last_seen_at = GREATEST(alerts.last_seen_at, EXCLUDED.last_seen_at),
              annotations  = EXCLUDED.annotations,
              labels       = EXCLUDED.labels
RETURNING *, (xmax = 0) AS was_inserted;
```
Single index probe on the unique constraint; `was_inserted` tells the service whether to emit `alert.created`. Batched with `unnest(...)` so one webhook of 200 alerts is one round-trip.

### 5.7 Growth, retention, partitioning

Sizing assumption for design: **1 000 alerts/min peak, 50M events/month, 5M distinct alerts.**

| Table | Strategy | Retention |
| --- | --- | --- |
| `alert_events` | **RANGE partition monthly** on `at`. `pg_partman` pre-creates 3 months ahead. | 13 months hot; then `DETACH` + export to object storage as Parquet, drop. |
| `ingest_batches`, `ingest_rejections` | RANGE partition **daily**. | 14 days (they exist for replay/debug only). |
| `ui_events` | RANGE partition **hourly**. | 24 hours. |
| `alerts` | **Not partitioned.** Bounded by label cardinality, not time; every access is by `(org_id, dedup_key)` or a filtered list. Partitioning would break the unique constraint we depend on. | Forever (cheap); soft-delete on org delete. |
| `alert_occurrences` | Not partitioned in v1. ~10M rows/year at target load is fine. **Watch item**: if it exceeds ~500M, partition by `started_at` and accept losing the global `one open occurrence` index (replace with a per-partition constraint plus an app-level guard). | 24 months, then archive. |
| `notification_deliveries` | Not partitioned; `rendered` JSONB nulled out after 30 days by a maintenance job (it is the bulk of the bytes). | 12 months. |
| `alert_daily_rollup`, `notif_daily_rollup` | Plain tables, written by a nightly job (not materialised views — we want incremental, not full refresh). | Forever. |

Maintenance is a single `maintenance` River periodic queue: create partitions, detach/drop old, expire `enrichment_cache`, null old `rendered`, recompute rollups, recompute `flap_score`, `ANALYZE` hot tables.

---

## 6. Public HTTP API

**Principles.** Spec-first OpenAPI 3.1 is the contract; the Go server and the TS client are both generated from it. Resource-oriented. `/api/v1` prefix, additive-only evolution. Every response is enveloped. Errors are RFC 9457 `application/problem+json`. Every mutating endpoint accepts `Idempotency-Key`. The UI has **zero** private endpoints.

**Envelope**

```jsonc
{ "data": [ /* ... */ ],
  "page": { "next_cursor": "eyJ0IjoiMjAyNi0...", "has_more": true, "limit": 50 },
  "meta": { "request_id": "01JD...", "elapsed_ms": 14 } }
```

### 6.1 Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| **Ingest** | | |
| `POST` | `/api/v1/ingest/alertmanager/{source_id}` | Alertmanager webhook receiver (bearer source token); returns `202` + batch id. |
| `POST` | `/api/v1/ingest/generic/{source_id}` | Generic JSON alert ingest for non-AM producers (future-proofing the port). |
| `GET` | `/api/v1/ingest/batches` · `/{id}` | Inspect raw batches for debugging; `POST /{id}/replay` re-processes one. |
| **Alerts** | | |
| `GET` | `/api/v1/alerts` | List/filter/search alerts (keyset paginated). |
| `GET` | `/api/v1/alerts/{id}` | One alert with current occurrence and enrichment summary. |
| `GET` | `/api/v1/alerts/{id}/occurrences` | Occurrence history (episode list). |
| `GET` | `/api/v1/alerts/{id}/events` | Alert-scoped timeline. |
| `GET` | `/api/v1/alerts/{id}/enrichments` | All enrichment results with provenance/status. |
| `POST` | `/api/v1/alerts/{id}/enrichments:refresh` | Force re-run of one or all enrichers. |
| `GET` | `/api/v1/alerts/{id}/notifications` | Notifications + deliveries for this alert. |
| `GET` | `/api/v1/alerts/{id}/rule` | The alert's rule definition + version history (**the differentiator**). |
| `POST` | `/api/v1/alerts/{id}/ack` · `/unack` · `/close` · `/reopen` · `/snooze` | Lifecycle actions on the current occurrence. |
| `POST` | `/api/v1/alerts/{id}/comments` | Add a human note to the timeline. |
| `POST` | `/api/v1/alerts:bulk` | Bulk ack/close/snooze by id list or by filter. |
| **Occurrences** | | |
| `GET` | `/api/v1/occurrences/{id}` · `/events` · `/enrichments` · `/notifications` | Episode-scoped detail and timeline. |
| **Groups** | | |
| `GET` | `/api/v1/alert-groups` | List groups (the default UI landing view). |
| `GET` | `/api/v1/alert-groups/{id}` | Group detail + rollup counts. |
| `GET` | `/api/v1/alert-groups/{id}/timeline` | **Merged, ordered lifecycle timeline** — the Sentry-issue view. |
| `GET` | `/api/v1/alert-groups/{id}/alerts` | Member alerts. |
| `POST` | `/api/v1/alert-groups/{id}/ack` · `/close` · `/comments` | Group-level actions, fanned out to members. |
| `GET`/`POST`/`PATCH`/`DELETE` | `/api/v1/grouping-rules[/{id}]` | Manage grouping rules; `POST :preview` dry-runs against recent alerts. |
| **Incidents** *(peripheral)* | | |
| `GET`/`POST`/`PATCH` | `/api/v1/incidents[/{id}]` | Incident CRUD + state transitions. |
| `POST`/`DELETE` | `/api/v1/incidents/{id}/members` | Attach/detach groups or alerts. |
| `GET` | `/api/v1/incidents/{id}/timeline` | Merged incident timeline. |
| **Notification** | | |
| `GET`/`POST`/`PATCH`/`DELETE` | `/api/v1/notification-policies[/{id}]` | Policy CRUD. |
| `POST` | `/api/v1/notification-policies:preview` | Dry-run: "given this alert, who gets told, where, when, rendered how" — **no send**. |
| `GET` | `/api/v1/notifications` · `/{id}` | Audit of notification intents. |
| `GET` | `/api/v1/deliveries` · `/{id}` | Delivery attempts, provider responses, errors. |
| `POST` | `/api/v1/deliveries/{id}:retry` | Manual retry of a dead delivery. |
| **Channels** | | |
| `GET` | `/api/v1/channel-types` | Provider descriptors: capabilities + config JSON Schema (drives dynamic UI forms). |
| `GET`/`POST`/`PATCH`/`DELETE` | `/api/v1/channels[/{id}]` | Channel CRUD. |
| `POST` | `/api/v1/channels/{id}:test` | Send a synthetic alert to verify credentials/formatting. |
| **Sources** | | |
| `GET`/`POST`/`PATCH`/`DELETE` | `/api/v1/sources[/{id}]` | Alertmanager/Prometheus source CRUD. |
| `POST` | `/api/v1/sources/{id}:sync` · `:rotate-token` · `:test` | Force a pull, rotate ingest token, test connectivity. |
| `GET` | `/api/v1/sources/{id}/health` | Poll/push health, lag, error rates. |
| **Silences** | | |
| `GET`/`POST`/`DELETE` | `/api/v1/silences[/{id}]` | Read mirrored AM silences; create/expire through to AM. |
| `GET`/`POST`/`DELETE` | `/api/v1/mutes[/{id}]`, `/maintenance-windows[/{id}]` | oto-local suppression. |
| **Discovery / UX** | | |
| `GET` | `/api/v1/labels` · `/api/v1/labels/{name}/values` | Typeahead for the filter bar. |
| `GET`/`POST`/`DELETE` | `/api/v1/saved-views[/{id}]` | Saved filters. |
| `GET` | `/api/v1/stats/overview` · `/stats/noise` · `/stats/mttr` | Dashboard/analytics. |
| `GET` | `/api/v1/enrichers` | Registered enrichers, phase, health, hit rate. |
| **Real-time** | | |
| `GET` | `/api/v1/stream` | **SSE** live event stream (see §6.4). |
| **Identity** | | |
| `GET` | `/api/v1/me` | Current principal + org + permissions. |
| `GET`/`POST`/`PATCH`/`DELETE` | `/api/v1/orgs/{id}/members`, `/api-keys` | Tenancy administration. |
| **Inbound integrations** | | |
| `POST` | `/api/v1/integrations/slack/interactions` · `/events` · `/commands` | Slack ack buttons, events, slash commands (signature-verified). |
| `POST` | `/api/v1/integrations/changes/{source_id}` | Deploy/change event webhook. |
| **Ops** | | |
| `GET` | `/healthz` · `/readyz` · `/metrics` · `/openapi.json` · `/api/v1/version` | Unversioned operational surface. |

### 6.2 Pagination

**Keyset only.** Cursor is an opaque base64url JSON `{"k": <sort key>, "id": "<uuid>", "d": "desc", "h": "<filter hash>"}`. The filter hash is validated so a cursor cannot be replayed against a different filter (which would silently skip rows). `limit` default 50, max 200. Response returns `next_cursor` and `has_more`; **no `total`** on unbounded collections — a `total` is available on request via `?include=count` and is served approximately from statistics/rollups, with a documented `approximate: true` flag.

### 6.3 Filtering

`GET /api/v1/alerts?state=firing,suppressed&severity=critical&cluster=prod-eu&namespace=payments&alertname=KubePodCrashLooping&label[team]=core&label[!tier]=canary&since=2026-08-01T00:00:00Z&q=oom&ack=unacked&sort=-last_seen_at&include=current_occurrence,enrichments`

- Repeated `label[k]=v` → AND; `label[k]=v1,v2` → IN; `label[!k]=v` → NOT. Compiled to `labels @> ...` / `NOT (labels @> ...)`.
- `include=` controls sub-resource expansion (bounded, whitelisted) so the UI avoids N+1 without us inventing GraphQL.
- A `filter=` mini-DSL (`severity=critical AND namespace=~"pay.*"`) is a **v1.1** addition; the structured params are the stable contract.

### 6.4 Real-time: **SSE** (decision)

> **Decision: Server-Sent Events on a single `GET /api/v1/stream` endpoint, with `Last-Event-ID` resume backed by the `ui_events` table.**

- **Why SSE:** the traffic is entirely server→client. Every client→server action already has a REST endpoint, and Slack interactivity comes in over webhooks — a bidirectional socket buys us nothing and costs us connection state, heartbeats, custom reconnect logic, and a second serialisation path. SSE is plain HTTP: it inherits our auth middleware, our proxies, our tracing, and `EventSource` is 5 lines in the browser.
- **Resume is the killer feature.** `ui_events.seq` is a monotonic bigint. On reconnect the browser automatically sends `Last-Event-ID: <seq>`; we replay from `ui_events` where `seq > N` and then attach to the live feed. **A laptop that slept through an incident wakes into a correct UI.** WebSocket gives us none of this for free.
- **Fan-out:** each API pod runs one `pg` connection issuing `LISTEN oto_events`. Writers `NOTIFY oto_events, '<org_id>:<seq>'` in the same transaction that wrote `ui_events` (payload is deliberately tiny — the 8 kB NOTIFY limit is a trap). The in-process hub reads `ui_events` for the range and fans out to subscribed connections, filtered by org and by the subscriber's declared interest (`?resources=alerts,groups&group_id=...`). Coalescing: per connection, at most one frame per 250 ms per resource id.
- **Backpressure:** slow consumers get a bounded ring buffer; on overflow we drop the buffer and send a single `{"type":"resync"}` frame telling the client to refetch. Never block a writer for a reader.
- **Fallback:** every stream-fed list endpoint also accepts `?since_seq=` for pure polling, used when SSE is disabled by a corporate proxy. **Runner-up: WebSocket** — necessary only if we later add collaborative UI editing or client-initiated subscriptions at high frequency; at that point it is an additive endpoint, not a rewrite.

---

## 7. Async processing

### 7.1 The prime directive

> **Ingestion must complete in a single short Postgres transaction that touches only `ingest_batches` and the job queue. Nothing on the ingest path may make an outbound network call.**

Alertmanager's webhook client has a short timeout and retries aggressively on non-2xx. If our webhook handler is ever behind Slack's rate limiter, an alert storm becomes an outage.

```
POST /ingest/alertmanager/{id}
  ├─ authenticate source token          (1 indexed lookup, argon2id verify, cached)
  ├─ read + size-limit body (≤ 8 MiB)
  ├─ checksum body
  └─ BEGIN
       INSERT INTO ingest_batches (... status='pending')  ON CONFLICT (source_id, checksum, received_at) DO NOTHING
       river.Insert(IngestBatchArgs{BatchID})              -- same tx: transactional outbox
     COMMIT
  └─ 202 Accepted {"batch_id": "..."}       target p99 < 25 ms
```

If the `ON CONFLICT` fires (Alertmanager retried), we return `202` with the original batch id — the retry is a no-op. **Runner-up: process synchronously and only notify asynchronously.** Tradeoff: simpler to debug and gives read-your-writes, but couples webhook latency to Postgres write contention during storms and offers no replay artefact. We chose durability.

### 7.2 Queue: **River** (decision)

> **Decision: [River](https://riverqueue.com) — a Postgres-backed job queue built on pgx v5 and `SELECT ... FOR UPDATE SKIP LOCKED`.**

- **One datastore.** Postgres is already the system of record. Adding Redis or NATS doubles the operational surface, adds a second failure mode, and — critically — makes exactly-once enqueue impossible.
- **Transactional enqueue is the whole game.** `river.InsertTx(ctx, tx, args)` puts the job in the *same transaction* as the domain write. Either the occurrence opened **and** the notify job exists, or neither. This is a transactional outbox with none of the outbox boilerplate (no polling relay, no separate dispatcher).
- Ships with what we need: typed Go args, per-queue concurrency, unique jobs, periodic jobs, snooze, exponential backoff, a dead-letter table, and a queryable job table for a real ops UI.
- **Runner-up: asynq (Redis).** Faster and better at very high throughput, but enqueue is not transactional with our writes — we would have to build a real outbox table plus a relay, reintroducing exactly the complexity we avoided, and take a Redis dependency.
- **Dismissed: Kafka/NATS.** Correct at 100× our volume; at our volume it is an operational tax paid for nothing. If we ever need it, `jobs.Enqueuer` is an interface and the workers do not care.

### 7.3 Queues and workers

| Queue | Workers | Job | Notes |
| --- | --- | --- | --- |
| `ingest` | 16 | `ingest.process_batch` | Normalise → dedupe upsert → occurrence transitions → events → group assignment → enqueue enrich/notify. Batched, one tx per batch. |
| `enrich_async` | 8 | `enrich.run` (phase 2) | Slow enrichers. On completion enqueues `notify.enriched`. |
| `notify` | 8 | `notify.evaluate` | Match policies → create `notifications` + `notification_deliveries` rows. Never calls a provider. |
| `deliver_slack` | 4 | `deliver.slack` | The only place Slack's API is touched. Rate-limited. |
| `deliver_generic` | 8 | `deliver.webhook`, `deliver.pagerduty` | One queue per provider class so a slow provider cannot starve others. |
| `reconcile` | 2 | `source.pull`, `source.reconcile`, `silences.sync`, `rules.sync` | Periodic per source. |
| `lifecycle` | 4 | `occurrence.reap`, `ack.expire`, `group.close`, `flap.score` | Timer-driven state transitions. |
| `maintenance` | 1 | `partitions.manage`, `retention.prune`, `rollups.compute`, `cache.expire` | Nightly / hourly. |

### 7.4 Idempotency & at-least-once

Every job is at-least-once, therefore every job handler is idempotent by construction:

1. **Ingest:** the alert upsert is `ON CONFLICT DO UPDATE`. Event writes carry `dedupe_key` (e.g. `occ:{id}:opened`) under a unique index — a replayed batch produces no duplicate timeline entries.
2. **Notify:** `notifications.idempotency_key = sha256(occurrence_id, reason, state_version, policy_id)`. `state_version` increments on every material occurrence change, so "resolved" for state_version 7 can only ever exist once. A replay hits the unique constraint and is swallowed.
3. **Deliver:** `UNIQUE (notification_id, channel_id)`. The worker claims a delivery with `UPDATE ... SET status='sending', attempts=attempts+1 WHERE id=$1 AND status IN ('pending','failed') RETURNING *` — an optimistic-lock claim; a duplicate worker gets zero rows and exits.
4. **The genuinely hard case — crash after Slack accepted, before we recorded the ts.** Slack has no idempotency key. Our answer: **embed our `delivery_id` in Slack's `metadata` field** on every message. On retry, before sending, the worker calls `conversations.history` scoped to the thread and looks for a message whose `metadata.event_payload.delivery_id` matches. If found, we adopt the existing `ts` and mark the delivery sent. This turns at-least-once into effectively-exactly-once for the only provider where duplicates are actually embarrassing. For providers without a metadata channel, we fall back to a deterministic invisible marker in the payload.

### 7.5 Ordering for thread replies

The requirement: within one Slack thread, the root message must land first, and replies must appear in lifecycle order (`fired` → `enriched` → `acked` → `resolved`). Across threads, order is irrelevant and parallelism is desirable.

> **Decision: per-thread sequence gating with a Postgres advisory lock. No global serialisation, no per-thread queue.**

1. When a delivery is created, it takes `thread_seq = channel_threads.next_seq++` inside the creating transaction. Sequence assignment is therefore totally ordered by the *causal* order of domain events — which is exactly the order we want.
2. The `deliver.*` worker:
   ```go
   // serialize all sends for this thread across all workers
   pg_advisory_xact_lock(hashtextextended(thread_id))
   if d.Role == RoleReply && thread.ProviderThreadID == "" {
       return river.JobSnooze(2 * time.Second)   // root not landed yet
   }
   if d.ThreadSeq != thread.LastSentSeq + 1 {
       return river.JobSnooze(1 * time.Second)   // an earlier reply is still in flight
   }
   ref, err := channel.Deliver(ctx, req)
   // same tx: update delivery + channel_threads.last_sent_seq = d.ThreadSeq
   ```
3. **Gap recovery:** if a delivery goes `dead` (permanent failure), the lifecycle job advances `last_sent_seq` past it and writes a `delivery.skipped` timeline event. A poisoned message can never wedge a thread forever.
4. **Coalescing:** a delivery whose `rendered_hash` equals the last sent hash for the same `(thread, reason)` is skipped as a no-op — this is what stops a flapping alert from producing 40 identical replies.

Why not a per-thread FIFO queue? Because thread count is unbounded (one per occurrence) and no queue system handles millions of ephemeral ordered partitions well. Advisory locks make ordering a *property of the write*, cost one hash, and require no new infrastructure. **Runner-up:** Kafka partitioned by thread key — correct ordering by construction, but a broker plus a partition-count ceiling on concurrent threads.

---

## 8. Notification & Channel abstraction

### 8.1 The interfaces

```go
// internal/channels/model — no I/O, no provider imports.

type Type string // "slack" | "webhook" | "pagerduty" | "msteams"

type Capability uint32
const (
    CapThreading Capability = 1 << iota // replies attach to a parent message
    CapAmend                            // an already-sent message can be edited
    CapRichLayout                       // structured blocks, not just text
    CapInteractive                      // buttons that call back into oto
    CapReactions
    CapAttachments
    CapDedupeKey                        // provider does its own dedupe (PagerDuty)
    CapAckSemantics                     // provider has a native ack (PagerDuty)
)

type Descriptor struct {
    Type            Type
    DisplayName     string
    ConfigSchema    []byte      // JSON Schema; drives the dynamic settings UI
    CredentialKinds []string
    Capabilities    Capability
    Renderers       []RendererID
    RateLimitClass  string
}

// Provider is registered once at boot; it mints Channels from stored config.
type Provider interface {
    Descriptor() Descriptor
    ValidateConfig(ctx context.Context, raw json.RawMessage) error
    Open(ctx context.Context, cfg ChannelConfig, cred Credential) (Channel, error)
}

// Channel is the delivery port. It knows nothing about alerts.
type Channel interface {
    Capabilities() Capability
    Deliver(ctx context.Context, req DeliverRequest) (DeliverResult, error)
    Amend(ctx context.Context, ref MessageRef, msg RenderedMessage) (DeliverResult, error)
    Probe(ctx context.Context) error   // health check / test-send
    Close() error
}

type DeliverRequest struct {
    Message    RenderedMessage
    ReplyTo    *MessageRef  // nil => root message
    DeliveryID uuid.UUID    // embedded in provider metadata for crash recovery
    DedupeKey  string       // used only if CapDedupeKey
    Intent     Intent       // Fired | Resolved | Acked | Suppressed | Enriched | Digest
}

type MessageRef struct {
    ConversationID string // Slack channel C0123 / PD service / webhook URL id
    MessageID      string // Slack ts
    ThreadID       string // Slack root ts
    ProviderKey    string // PagerDuty dedup_key etc.
}

type DeliverResult struct {
    Ref         MessageRef
    DeliveredAt time.Time
    Raw         json.RawMessage
}
```

Errors are typed, and the classification — not the provider — drives retry policy:

```go
type Error struct {
    Class      ErrorClass // Retryable | RateLimited | Permanent | ConfigInvalid | AuthExpired
    RetryAfter time.Duration
    Provider   string
    Cause      error
}
```

`ConfigInvalid` and `AuthExpired` do not retry; they flip `channels.health_status` and raise an in-app banner. That is the difference between "Slack is flaky" and "your token was revoked three days ago and nobody noticed" — the second is a product feature.

### 8.2 Rendering is strictly separate from delivery

```go
// The channel-agnostic, denormalised read model. Built ONCE per notification.
type NotificationView struct {
    Org        OrgRef
    Alert      AlertView
    Occurrence OccurrenceView
    Group      *GroupView
    Reason     Reason
    Enrichments map[string]EnrichmentView  // keyed by enricher name
    Rule       *RuleView                   // expr, for, labels, version, changed-since
    Related    []RelatedAlertView
    Changes    []ChangeEventView
    Actions    []Action                    // Ack, Silence 1h, Open in oto, Runbook, Graph
    Links      Links
    Timeline   []TimelineEntry             // recent, for compact digests
}

type Renderer interface {
    ID() RendererID
    Supports(Capability) bool
    Render(ctx context.Context, v *NotificationView, o Options) (RenderedMessage, error)
}

type RenderedMessage struct {
    Fallback string          // plain text — always populated, always usable
    Summary  string          // notification/push preview text
    Payload  json.RawMessage // channel-native (Slack Block Kit JSON, PD event JSON)
    Hash     string          // sha256(Payload) — used to skip no-op amends
    Metadata map[string]string
}
```

Three properties this buys us:

1. **Renderers are pure functions** → golden-file tested. Every Block Kit layout has a checked-in `testdata/*.golden.json`, and a Block Kit validator runs in CI. We never discover a broken layout in production.
2. **`RenderedMessage` is persisted** on the delivery row *before* the send. If Slack is down for an hour, the retry sends exactly the bytes we intended at the time, not a re-render against mutated state.
3. **The renderer is swappable per channel** (`channels.renderer`), so a customer can have a terse `#noise` channel and a verbose `#sre-critical` channel from the same notification.

### 8.3 Slack implementation specifics

- **Root message** (occurrence opened): a Block Kit message — header with severity emoji + colour bar, `alertname`, cluster/namespace/service context row, the summary/description annotations, an enrichment section (k8s owner, pod state, recent-deploy note), a fields block for key labels, and an actions block: `Ack` · `Silence 1h` · `Runbook` · `View in oto` · `Graph`.
- **Thread replies** for every subsequent lifecycle fact: enriched, acked (with who), suppressed, re-fired within grace, resolved (with duration).
- **Root amend** on every state change: the colour bar and a status line (`🔴 FIRING 12m` → `🟢 RESOLVED after 14m` → `🔇 SILENCED by @ram until 14:00`). This is what makes the channel scannable — you can tell the state of everything without opening a thread.
- **Thread state** lives in `channel_threads`: `provider_conversation_id` = Slack channel id, `provider_thread_id` = root message `ts`. Created in `state='opening'` by the notify worker; the delivery worker flips it to `open` and records the `ts` in the same transaction as the delivery row. Replies simply pass `thread_ts = provider_thread_id`. On resolve+`group_close_delay` the thread moves to `frozen`; a later re-fire beyond `refire_grace` creates a *new* row for the new occurrence.
- **Interactivity**: the `Ack` button posts to `/api/v1/integrations/slack/interactions`. We verify the Slack signature, map the Slack user to an oto user (by email, with an unlinked-user fallback that records the Slack handle), invoke `AlertService.Ack` — the *same service the REST API calls* — and reply ephemerally. There is exactly one ack code path.
- **Rate limits**: `chat.postMessage` is ~1/sec per channel. A Postgres token-bucket (`rate_limit_buckets`, one `UPDATE ... RETURNING` per token) keyed by `slack:{workspace}:{channel}` gates the `deliver_slack` queue across all worker pods; on HTTP 429 we `river.JobSnooze(Retry-After)`. **Runner-up:** Redis token bucket — faster, but a new dependency for a limiter that runs at single-digit QPS.

### 8.4 Adding a second channel type

To add **PagerDuty**, an engineer writes exactly:

1. `internal/channels/providers/pagerduty/provider.go` — `Descriptor()` (capabilities `CapDedupeKey|CapAckSemantics`, notably **no** `CapThreading`), `ValidateConfig`, `Open`.
2. `internal/channels/providers/pagerduty/channel.go` — `Deliver` maps `Intent` → Events API v2 `event_action` (`Fired→trigger`, `Resolved→resolve`, `Acked→acknowledge`) with `dedup_key = occurrence_id`.
3. `internal/channels/render/pagerduty/renderer.go` + golden files.
4. One line in `internal/channels/service/registry.go`.
5. A migration row for the descriptor cache (optional).

**Nothing else changes.** No migration to the notification tables, no change to policies, no change to the UI (the settings form renders itself from `ConfigSchema`).

Capability negotiation handles the impedance mismatch centrally, in `DispatchService`, never in providers:

| Situation | Capability | Behaviour |
| --- | --- | --- |
| Reply on a threading channel | `CapThreading` | Thread reply. |
| Reply on a non-threading channel | — | Suppress by default; if `policy.reply_mode=repost`, send a standalone message with a back-reference. |
| State change on an amendable channel | `CapAmend` | Edit the root message in place. |
| State change on a dedupe-key channel | `CapDedupeKey` | Send a new event with the same `dedup_key`; provider collapses it. |
| Buttons on a non-interactive channel | — | Render as links to the oto UI instead. |

---

## 9. Enrichment pipeline

### 9.1 Interface

```go
// internal/enrichment/model
type Phase int
const (
    PhaseFast  Phase = 1 // must finish inside the pre-notification budget
    PhaseAsync Phase = 2 // runs after the first notification; produces a thread reply
)

type Enricher interface {
    Name() string          // stable id, e.g. "k8s.object"
    Version() int          // bump => cache invalidation + re-run on next occurrence
    Phase() Phase
    DependsOn() []string   // DAG edges, e.g. runbook depends on prom.rule
    Timeout() time.Duration
    Applicable(s *Subject) bool
    Enrich(ctx context.Context, s *Subject) (Result, error)
}

type Subject struct {
    Alert       AlertSnapshot          // labels, annotations, fingerprint
    Occurrence  OccurrenceSnapshot
    Source      SourceRef              // which AM/Prom/cluster
    Prior       map[string]Result      // results from already-completed enrichers
}

type Result struct {
    Status   Status                 // Ok | Partial | Skipped | Failed | Timeout
    Payload  any                    // typed struct, marshalled to JSONB
    CacheKey string                 // "" => not cacheable
    TTL      time.Duration
    Warnings []string
}
```

### 9.2 Two-phase execution (decision)

> **Decision: a two-phase pipeline with a hard wall-clock budget on phase 1.**

The tension is real: enrichment is what makes the Slack message *lovable*, but waiting on the Kubernetes API is what makes it *late*. Splitting resolves it.

- **Phase 1 (fast, pre-notification):** total budget **1 500 ms**, per-enricher timeout 500 ms, run concurrently within DAG levels. Enrichers here must be cache-friendly and local: `prom.rule` (cached per rule hash), `runbook` (pure label/annotation mapping), `alert.history` (one indexed Postgres query), `silence.match` (local mirror), `ownership` (label→team map). When the budget expires the pipeline stops, records `Status=Timeout` for the stragglers, and **notification proceeds anyway**. Notification is *never* blocked; the budget is a ceiling, not a wait.
- **Phase 2 (async, post-notification):** no shared budget, per-enricher timeout up to 30 s. `k8s.object` (pod/owner/node/events, informer-backed but may miss cache), `deploy.correlation`, `graph.snapshot` (rendered PromQL image), `log.sample` (future). On completion the pipeline **amends the root Slack message** and posts **one coalesced thread reply** ("+3 enrichments") — coalesced by a 10 s debounce so five enrichers do not produce five replies.

### 9.3 Ordering, failure, caching

- **Ordering:** `DependsOn()` forms a DAG; the pipeline topologically sorts it into levels and runs each level concurrently. A cycle is a boot-time panic (caught by a registry unit test). Dependents receive predecessors' results in `Subject.Prior`.
- **Partial failure is normal, not exceptional.** Every enricher's outcome is a row in `enrichments` with its own `status`, `error` and `duration_ms`. One failure never fails the pipeline; a dependent of a failed enricher is marked `Skipped` with the reason. Both are rendered honestly in the UI ("k8s context unavailable: cluster prod-eu unreachable") — an enrichment that silently disappears is worse than one that visibly failed.
- **Timeouts:** enforced by `context.WithTimeout` per enricher **and** a parent context for the phase budget. Every outbound client (`promcontext`, `k8scontext`) is context-aware with its own circuit breaker; three consecutive failures open the breaker for 60 s and the enricher self-reports `Skipped` without making a call.
- **Caching, two layers:**
  1. In-process `ristretto` LRU + `singleflight` — collapses the thundering herd when 300 alerts from one storm all want the same rule definition.
  2. `enrichment_cache` table keyed by `cache_key`, shared across pods, TTL'd. Cache keys are content-addressed and version-stamped: `rule:{source_id}:{alertname}:{rule_group_hash}` (TTL 1h), `k8sobj:{cluster}:{ns}:{kind}:{name}:{resource_version}` (TTL 5m), `owner:{cluster}:{ns}:{pod}` (TTL 15m). Bumping `Enricher.Version()` invalidates by prefix.
- **Storage:** results land in `enrichments`, unique on `(subject_kind, subject_id, enricher)`, upserted so a refresh overwrites. Attached to the **occurrence** when episode-specific (pod state at fire time), to the **alert** when identity-stable (ownership, runbook). Every row records `computed_at`, `from_cache`, `duration_ms` and `enricher_version` — full provenance, always visible in the UI.

### 9.4 The v1 enricher set

| Enricher | Phase | Produces |
| --- | --- | --- |
| `prom.rule` | 1 | Alert rule `expr`, `for`, labels, annotations, rule group, **rule version hash + diff vs previous occurrence**. |
| `runbook` | 1 | Runbook URL from `runbook_url` annotation or an org-level `alertname → url` template map. |
| `alert.history` | 1 | Occurrence count 24h/7d/30d, flap score, last resolve duration, MTTR for this alert. |
| `silence.match` | 1 | Matching AM silences and oto mutes, with who created them and when they expire. |
| `ownership` | 1 | Owning team/user from label mapping; drives routing and the "who to ping" line. |
| `related.alerts` | 1 | Other alerts firing in the same namespace/cluster in ±10 min — the cheap correlation win. |
| `k8s.object` | 2 | Resolved Pod/Deployment/StatefulSet/Node, owner chain, container statuses, restart counts, recent k8s Events, resource requests vs limits. |
| `deploy.correlation` | 2 | Change events in the same namespace/service within a lookback window, with a confidence score. |
| `graph.snapshot` | 2 | Rendered PNG of the rule's expression over the firing window. |

`prom.rule` + `rule.definition_changed` is the feature no competitor has: *"this alert fired because someone lowered the threshold from 90% to 70% two hours ago"* is a support ticket resolved before it is opened.

---

## 10. Technology choices

### 10.1 Go

| Concern | **Pick** | One-line justification | Runner-up & tradeoff |
| --- | --- | --- | --- |
| HTTP router | **chi v5** | `http.Handler` all the way down, mature composable middleware, zero magic, and `oapi-codegen` targets it natively. | `net/http` ServeMux 1.22 — no dependency, but we'd hand-roll the middleware stack we'd immediately need. |
| DB driver | **pgx v5** | Native protocol, `COPY`, `LISTEN/NOTIFY`, first-class JSONB and array support; `database/sql` throws all of that away. | `pgx` via `database/sql` — portable, but loses NOTIFY and batch. |
| Query layer | **sqlc** (+ hand-written pgx for dynamic queries) | Generates typed Go from real SQL, so the repository layer is thin, reviewable, and physically incapable of leaking an ORM entity into the API. | `ent` — superb graph traversal and codegen, but its entity type becomes *the* model and the three-model rule quietly dies. `gorm` rejected: implicit behaviour, weak Postgres feature coverage. |
| Dynamic queries | **squirrel** | The alert-list filter combinatorics genuinely need a builder; sqlc alone would mean 40 near-identical queries. | Hand-concatenated SQL — fast to write, an injection audit forever. |
| Migrations | **goose** | Plain `.sql` files, embeddable via `embed.FS`, supports Go migrations for data backfills, and runs as a subcommand of our own binary. | `atlas` — declarative and much smarter about drift, but a heavier toolchain and another concept for on-call to learn. |
| Config | **koanf** | Layered defaults→file→env→flags, small, no global singletons, no forced 12 dependencies. | `viper` — ubiquitous, but a large transitive tree and global state. |
| Logging | **`log/slog`** + `slog-otel` | Stdlib means every library can log into our handler without an adapter; structured JSON in prod, pretty in dev. | `zerolog` — measurably faster, but non-stdlib and we are nowhere near log-throughput-bound. |
| Validation | **go-playground/validator** at the DTO boundary + hand-written invariants in `model` | Struct tags handle "is this a valid UUID/enum/range"; real domain rules belong in Go, not tags. | `ozzo-validation` — nicer for conditional rules, less ecosystem support. |
| Job queue | **river** | Postgres-backed, pgx-native, transactional `InsertTx` = free outbox, plus periodic and unique jobs. | `asynq` — better raw throughput, but Redis and no transactional enqueue. |
| API spec | **spec-first OpenAPI 3.1 + `oapi-codegen`** (Go) + `openapi-typescript`/`openapi-fetch` (TS) | "API-first, another UI must be boltable" is only true if the spec is the *source*, not a by-product; one spec generates both sides, and drift is a CI failure. | `huma` (code-first) — lovely DX, but the spec becomes an artefact of the implementation. |
| Errors | **stdlib `errors` + `internal/platform/errs`** | Typed codes mapped to HTTP/problem+json in exactly one place. | `pkg/errors` — deprecated. |
| Observability | **OpenTelemetry** traces + `prometheus/client_golang` metrics | We are an alerting product; being exemplary at emitting RED/USE metrics and Prometheus rules for ourselves is table stakes. | OTel metrics only — cleaner, but our users' Prometheus wants a `/metrics` endpoint today. |
| Slack SDK | **slack-go/slack** | Typed Block Kit builders, signature verification, socket mode — writing this by hand is weeks of bugs. | Raw HTTP — fewer deps, all the bugs. |
| Kubernetes | **client-go** + shared informers | Informers give a warm cache so `k8s.object` is a memory lookup, not an API call per alert. | Direct API calls — trivially simple, and it will melt the API server during a storm. |
| Testing | **testify** + **testcontainers-go** (real Postgres) + golden files + `httptest` | Alert logic is 90% SQL semantics and payload shape; mocked DBs and hand-diffed JSON both lie. | `dockertest` + sqlmock — lighter, catches less. |
| Contract tests | **schemathesis / prism** against the OpenAPI spec in CI | Guarantees the spec and server never diverge, which is the entire premise of "boltable second UI". | Manual review — free, and wrong within a month. |
| Mocks | **`go.uber.org/mock`** | Generated from our own port interfaces; only used at true boundaries (Slack, k8s, Prom). | `moq`/`counterfeiter` — fine, less common. |
| Fake upstreams | **`test/fixtures` + `httptest` replay servers** | Real Alertmanager/Prometheus/Slack payloads checked into the repo, replayed deterministically. | Live sandbox — flaky CI. |
| DI | **explicit constructor wiring in `internal/app`** | ~40 constructors in one readable file beats a code generator or a runtime container; the dependency graph should be *readable*. | `wire` — compile-time safe, but codegen friction on every change. |
| Lint | **golangci-lint** with `depguard` layering rules + an arch test | The three-model rule and the no-cycles rule must be mechanically enforced or they decay in a quarter. | Code review — humans are inconsistent. |

### 10.2 SolidJS frontend

| Concern | **Pick** | One-line justification | Runner-up & tradeoff |
| --- | --- | --- | --- |
| Build | **Vite 6 + TypeScript strict** | Solid's first-class toolchain; instant HMR, trivial proxying to the Go API in dev. | SolidStart — SSR we do not need for an authenticated internal dashboard. |
| Router | **@solidjs/router** | Official, supports nested layouts and route-level data loading, which maps cleanly onto list→group→timeline. | Hand-rolled — no. |
| Data fetching | **@tanstack/solid-query** + generated `openapi-fetch` client | Caching, dedupe, background refetch and stale-while-revalidate solved; the typed client makes API drift a compile error. | `createResource` — zero deps, but we would reimplement query caching badly. |
| Real-time | **native `EventSource`** wrapped in a Solid store, merged into the Query cache | Solid's fine-grained reactivity means an SSE-driven store update re-renders exactly one table row — this is Solid's superpower, so use it. | Poll-only — simpler, visibly worse for a live alert wall. |
| Styling | **Tailwind CSS v4 + CSS custom properties for theme tokens** | Dense, information-heavy operational UI is exactly where utility classes win; tokens keep dark mode (the default for an ops tool) honest. | vanilla-extract — better type safety, much slower iteration. |
| Components | **Kobalte** (headless, ARIA-correct) + our own design system on top | We need correct dialogs/comboboxes/menus without inheriting someone's visual identity. | `ark-ui`/`park-ui` — more batteries, heavier and more opinionated. |
| Tables | **@tanstack/solid-table** + **@tanstack/solid-virtual** | The alert list must render 10 000 rows with sorting, grouping and column control without dying. | Hand-rolled — column/grouping logic is a deceptive time sink. |
| Charts | **uPlot** in a thin Solid wrapper | 40 kB, renders 100 k time-series points at 60 fps; alert-rate sparklines and rule graphs are its exact use case. | Chart.js / ECharts — prettier defaults, 10× the bundle. |
| Forms | **@modular-forms/solid** + **ajv** for provider JSON Schemas | Channel config forms are *generated from the provider's JSON Schema*, so the settings UI needs no code when a provider is added. | Hand-rolled forms — a new form per provider, defeating the abstraction. |
| Icons | **lucide-solid** | Consistent, tree-shakeable, covers the ops vocabulary. | Heroicons — narrower set. |
| Dates | **date-fns** + `Intl` | Timeline rendering needs relative + absolute + timezone-correct formatting; no moment-sized bundles. | Luxon — heavier. |
| Testing | **Vitest** + **@solidjs/testing-library** + **Playwright** | Unit for stores/renderers, component for tables, Playwright for the alert→timeline→ack journey against a seeded Postgres. | Cypress — slower, worse parallelism. |
| Package manager | **pnpm** | Fast, strict, correct in a monorepo-ish `web/` folder. | npm — slower, looser. |

---

## 11. Multi-tenancy & multi-cluster

### 11.1 Decision

> **Yes — model `Org` (tenant) from day one, as a mandatory `org_id` column on every domain table, in a single shared schema, with tenancy enforced in the repository layer rather than by convention.**

Even for a single-tenant, self-hosted first customer.

**Why now, not later:**

1. **Retrofitting tenancy is the single most expensive refactor in this class of system.** It touches every table, every index (the *prefix* changes, so every index is rebuilt), every query, every cache key, every job payload and every API route. Doing it on day one costs one column and one index prefix. Doing it in month twelve costs a quarter and a migration window.
2. **We need the concept anyway.** "Team A's alerts, Team A's Slack channels, Team A's policies" is a day-one requirement even inside one company. `Org` is that boundary whether or not we ever sell multi-tenant SaaS.
3. **It forces correct scoping habits.** With `org_id` mandatory, "list all firing alerts" cannot be written without a tenant, so cross-tenant leaks are impossible to write accidentally rather than merely discouraged.
4. **It is the only path to hosted.** The product owner may want SaaS later; without tenancy that is a rewrite.

**Enforcement, three layers:**

- Every repository method takes a `TenantScope` value object (not a bare `uuid.UUID`) that can only be constructed from an authenticated principal. There is no repository method without one. An arch test asserts every exported repository method's first parameter after `ctx` is a `TenantScope`.
- Every composite index starts with `org_id`, so tenant scoping is free at the planner level.
- **Postgres Row-Level Security is enabled on every tenant table as a backstop**, with `SET LOCAL app.org_id` on transaction start. RLS is defence-in-depth, not the primary mechanism — an app bug then produces zero rows, not someone else's alerts.

**Runner-up: schema-per-tenant.** Stronger isolation and trivially per-tenant restore, but connection-pool explosion, N× migration runtime, and cross-tenant analytics become a nightmare. Revisit only if a customer contractually demands physical separation — at which point it becomes *database*-per-tenant with the same code, which our design already permits (the pool is resolved per org).

### 11.2 Multi-cluster

Multi-cluster is **not** multi-tenancy and must not be conflated.

- One `Org` owns many `Cluster`s. One `Cluster` owns one or more `AlertSource`s (HA Alertmanager replicas share a `cluster_key`).
- `cluster_key` is part of the dedupe identity (§3.3), so identical alerts in `prod-eu` and `prod-us` are **separate Alerts** — correct, because they are separate problems with separate blast radii.
- Cross-cluster correlation is an explicit, visible act: an `Incident`, created by a human or by a correlation rule. Never an implicit merge.
- Cluster is a first-class filter, grouping key and routing matcher throughout the UI and API.
- A cluster can be `degraded` (its source is unreachable). Rather than silently expiring every alert from it, the reaper checks `source_health` first; if the source is down, occurrences are held in `firing` and a single `source.unreachable` banner is raised. **Losing sight of an alert is not the same as the alert resolving** — this is one of the highest-value correctness details in the whole system.

### 11.3 Users, auth, roles

- Users are global; `memberships` binds user→org with a role.
- Roles v1: `owner`, `admin` (sources/channels/policies), `responder` (ack/close/comment/silence), `viewer` (read-only). Permission checks are a central `authz.Can(principal, action, resource)` — a capability table, not `if user.Role == "admin"` scattered across handlers.
- Principals: browser session cookie (OIDC/OAuth via `coreos/go-oidc`, plus local password auth for self-hosted), `Bearer oto_pat_*` API tokens (hashed, scoped, expiring), and **ingest tokens** in a separate namespace (`oto_ingest_*`) scoped to exactly one source and one endpoint — an ingest token can never read an alert.

---

## 12. Repository layout

```
oto/
├── README.md
├── CLAUDE.md                                # agent/contributor conventions
├── go.mod  go.sum
├── Makefile                                 # dev, test, lint, generate, migrate
├── .golangci.yml                            # incl. depguard layering rules
├── sqlc.yaml
├── .github/workflows/{ci.yml,release.yml,spec-drift.yml}
│
├── api/openapi/
│   ├── openapi.yaml                         # ROOT CONTRACT (spec-first)
│   ├── paths/{alerts,groups,occurrences,incidents,channels,policies,sources,silences,stream,identity}.yaml
│   ├── components/{alert,occurrence,event,group,enrichment,notification,channel,source,common,errors}.yaml
│   └── examples/
│
├── cmd/oto/
│   ├── main.go
│   ├── serve.go            # API server
│   ├── worker.go           # river workers (all queues, or --queues=)
│   ├── migrate.go          # up / down / status / create
│   ├── seed.go             # dev fixtures
│   ├── replay.go           # replay an ingest batch (ops tool)
│   └── version.go
│
├── internal/
│   ├── app/
│   │   ├── container.go                     # explicit DI wiring
│   │   ├── routes.go                        # mounts every domain's api.Router
│   │   ├── workers.go                       # registers every river worker
│   │   ├── enrichers.go                     # enricher registry
│   │   ├── providers.go                     # channel provider registry
│   │   └── periodic.go                      # periodic job schedule
│   │
│   ├── platform/
│   │   ├── config/{config.go,load.go,defaults.go,validate.go}
│   │   ├── log/{logger.go,middleware.go,redact.go}
│   │   ├── telemetry/{otel.go,metrics.go,tracing.go,health.go}
│   │   ├── db/{pool.go,tx.go,manager.go,scope.go,jsonb.go,listen.go,retry.go}
│   │   ├── migrate/{runner.go,migrations/}  # goose .sql, embed.FS
│   │   ├── httpx/
│   │   │   ├── server.go  router.go  problem.go  render.go  binding.go
│   │   │   ├── cursor.go  filter.go  sse.go  ratelimit.go
│   │   │   └── middleware/{requestid,logging,recover,cors,auth,tenant,timeout,metrics}.go
│   │   ├── authn/{principal.go,session.go,oidc.go,pat.go,ingesttoken.go}
│   │   ├── authz/{authz.go,capabilities.go,roles.go}
│   │   ├── jobs/{client.go,queues.go,worker.go,periodic.go,enqueuer.go}
│   │   ├── secrets/{sealer.go,aesgcm.go,keyring.go}
│   │   ├── ratelimit/{bucket.go,pgbucket.go}
│   │   ├── errs/{errors.go,codes.go,http.go}
│   │   ├── clock/{clock.go,fake.go}
│   │   ├── id/{uuidv7.go,slug.go}
│   │   └── validate/{validate.go,rules.go}
│   │
│   ├── identity/
│   │   ├── api/{handler.go,dto.go,mapper.go,routes.go}
│   │   ├── service/{org.go,user.go,membership.go,token.go,session.go,ports.go}
│   │   ├── repository/{org.go,user.go,membership.go,apikey.go,mapper.go,queries/}
│   │   └── model/{org.go,user.go,role.go,principal.go}
│   │
│   ├── sources/
│   │   ├── api/{handler.go,dto.go,mapper.go,routes.go}
│   │   ├── service/{source.go,cluster.go,health.go,credential.go,ports.go}
│   │   ├── repository/{source.go,cluster.go,health.go,mapper.go,queries/}
│   │   ├── model/{source.go,cluster.go,identitypolicy.go}
│   │   └── client/{alertmanager.go,prometheus.go,types.go,doc.go}
│   │
│   ├── ingestion/
│   │   ├── api/{webhook.go,batch.go,dto.go,routes.go}
│   │   ├── service/{receiver.go,normalizer.go,poller.go,reconciler.go,ports.go}
│   │   ├── repository/{batch.go,rejection.go,mapper.go,queries/}
│   │   ├── model/{batch.go,observation.go,rejection.go}
│   │   ├── alertmanager/{webhook_v4.go,apiv2.go,decode.go,testdata/}
│   │   └── worker/{process_batch.go,pull_source.go,reconcile_source.go}
│   │
│   ├── alerts/
│   │   ├── api/{alert_handler.go,occurrence_handler.go,event_handler.go,action_handler.go,
│   │   │        dto.go,mapper.go,filter.go,routes.go}
│   │   ├── service/{alert.go,occurrence.go,lifecycle.go,timeline.go,dedupe.go,flap.go,ports.go}
│   │   ├── repository/{alert.go,occurrence.go,event.go,mapper.go,queries/}
│   │   ├── model/{alert.go,occurrence.go,event.go,eventtype.go,state.go,transition.go,labels.go}
│   │   └── worker/{reap_occurrences.go,expire_acks.go,score_flaps.go}
│   │
│   ├── grouping/{api,service,repository,model,worker}
│   ├── enrichment/
│   │   ├── api/{handler.go,dto.go,mapper.go,routes.go}
│   │   ├── service/{pipeline.go,registry.go,cache.go,dag.go,ports.go}
│   │   ├── repository/{enrichment.go,cache.go,mapper.go,queries/}
│   │   ├── model/{enricher.go,result.go,subject.go,phase.go}
│   │   ├── enrichers/{promrule,runbook,alerthistory,silencematch,ownership,
│   │   │              relatedalerts,k8sobject,deploycorrelation,graphsnapshot}/
│   │   └── worker/{run_async.go}
│   │
│   ├── notification/
│   │   ├── api/{policy_handler.go,notification_handler.go,delivery_handler.go,preview.go,dto.go,mapper.go,routes.go}
│   │   ├── service/{policy.go,matcher.go,notification.go,dispatch.go,thread.go,throttle.go,view.go,ports.go}
│   │   ├── repository/{policy.go,notification.go,delivery.go,thread.go,mapper.go,queries/}
│   │   ├── model/{policy.go,notification.go,delivery.go,thread.go,reason.go,view.go}
│   │   └── worker/{evaluate.go,deliver.go,retry.go}
│   │
│   ├── channels/
│   │   ├── api/{handler.go,descriptor.go,dto.go,mapper.go,routes.go}
│   │   ├── service/{channel.go,registry.go,render.go,capability.go,ports.go}
│   │   ├── repository/{channel.go,credential.go,mapper.go,queries/}
│   │   ├── model/{channel.go,provider.go,capability.go,message.go,errors.go,intent.go}
│   │   ├── providers/
│   │   │   ├── slack/{provider.go,channel.go,client.go,interactions.go,ratelimit.go,recover.go}
│   │   │   ├── webhook/{provider.go,channel.go}
│   │   │   ├── pagerduty/{provider.go,channel.go}
│   │   │   └── msteams/{provider.go,channel.go}
│   │   └── render/
│   │       ├── slack/{renderer.go,blocks.go,root.go,reply.go,digest.go,testdata/*.golden.json}
│   │       ├── markdown/{renderer.go,testdata/}
│   │       └── text/{renderer.go,testdata/}
│   │
│   ├── streaming/{api/stream.go,service/{hub.go,publisher.go,listener.go},repository/uievent.go,model/event.go}
│   ├── silences/{api,service,repository,model,worker}
│   ├── incidents/{api,service,repository,model}
│   ├── k8scontext/{service/{resolver.go,informer.go},repository,model,client}
│   ├── promcontext/{service/{rule.go,query.go,version.go},repository,model}
│   ├── changefeed/{api,service,repository,model}
│   ├── views/{api,service,repository,model}
│   ├── analytics/{api,service,repository,model,worker}
│   ├── audit/{api,service,repository,model}
│   └── oncall/{model/resolver.go,service/static.go}
│
├── pkg/
│   ├── alertmodel/{labelset.go,fingerprint.go,dedupkey.go,matcher.go}   # importable, stable
│   └── otoclient/                            # generated Go API client
│
├── db/
│   ├── migrations/{00001_init.sql, ...}
│   ├── queries/{alerts.sql,occurrences.sql,events.sql,groups.sql,notifications.sql,...}
│   └── seeds/{dev.sql,demo.sql}
│
├── web/
│   ├── package.json  pnpm-lock.yaml  vite.config.ts  tsconfig.json  tailwind.config.ts  index.html
│   ├── src/
│   │   ├── main.tsx  App.tsx
│   │   ├── routes/
│   │   │   ├── index.tsx
│   │   │   ├── alerts/{index.tsx,[id].tsx}
│   │   │   ├── groups/{index.tsx,[id].tsx,[id].timeline.tsx}
│   │   │   ├── incidents/{index.tsx,[id].tsx}
│   │   │   └── settings/{sources.tsx,channels.tsx,policies.tsx,grouping.tsx,members.tsx}
│   │   ├── api/{client.ts,generated/schema.d.ts,sse.ts,queries/{alerts,groups,channels,policies}.ts}
│   │   ├── features/
│   │   │   ├── alert-list/{AlertTable.tsx,FilterBar.tsx,LabelFilter.tsx,BulkActions.tsx}
│   │   │   ├── alert-detail/{AlertHeader.tsx,EnrichmentPanel.tsx,RulePanel.tsx,OccurrenceList.tsx}
│   │   │   ├── timeline/{Timeline.tsx,TimelineEntry.tsx,entries/*.tsx,TimelineFilter.tsx}
│   │   │   ├── channels/{ChannelForm.tsx,SchemaForm.tsx,TestSend.tsx}
│   │   │   ├── policies/{PolicyEditor.tsx,MatcherBuilder.tsx,PolicyPreview.tsx}
│   │   │   └── live/{LiveIndicator.tsx,useLiveStream.ts}
│   │   ├── components/{ui/*,Table,Dialog,Toast,Badge,SeverityDot,RelativeTime,LabelChip,EmptyState}
│   │   ├── design/{tokens.css,theme.ts,severity.ts}
│   │   └── lib/{format.ts,cursor.ts,labels.ts,store.ts}
│   └── e2e/{alerts.spec.ts,timeline.spec.ts,ack.spec.ts}
│
├── deploy/
│   ├── helm/oto/{Chart.yaml,values.yaml,templates/}
│   ├── kustomize/{base,overlays}
│   ├── compose/docker-compose.yml
│   └── prometheus/{oto-rules.yaml,oto-dashboard.json}       # we alert on ourselves
│
├── test/
│   ├── fixtures/{alertmanager,prometheus,slack,k8s}/
│   ├── integration/{ingest_test.go,lifecycle_test.go,notify_test.go,dedupe_test.go,refire_test.go}
│   ├── contract/{openapi_test.go}
│   └── harness/{postgres.go,fakeslack.go,fakeam.go,builders.go}
│
├── tools/{tools.go,generate.go}
└── docs/
    ├── design/architect-proposal.md          # this document
    ├── adr/{0001-postgres-as-queue.md,0002-sse-over-websocket.md,0003-dedup-key.md,
    │        0004-occurrence-vs-event.md,0005-multitenancy-day-one.md,0006-spec-first-openapi.md,
    │        0007-two-phase-enrichment.md,0008-refire-semantics.md}
    ├── runbooks/  api/  CONTRIBUTING.md  DOMAIN.md
```

---

## 13. Open questions for the product owner

1. **`internal/` vs `src/`** — we propose `internal/<domain>/{api,service,repository,model}`, identical in shape to the mandated layout but with compiler-enforced encapsulation. Naming-only deviation; needs a yes/no.
2. **Deployment model for v1** — self-hosted single-tenant Helm chart, hosted SaaS, or both? The architecture supports both, but it changes onboarding, secret management (KMS vs local keyring) and OIDC priorities.
3. **`refire_grace` and flap thresholds** — we propose 10 min / 5-in-30-min defaults. These are the single most user-visible tuning knobs (they decide how noisy Slack is). Worth validating against a real Alertmanager config.
4. **Incidents in v1 or v1.1?** Marked peripheral. Including them is roughly three weeks; excluding them makes cross-cluster correlation manual.
5. **Slack interactivity scope** — do we ship Ack/Silence *buttons* in v1 (requires a Slack app with interactivity, a public URL or Socket Mode, and app-distribution work), or v1 links-only into the oto UI?
6. **Second channel type for v1** — building one more (generic webhook is cheapest, PagerDuty most valuable) is the only real proof the abstraction holds. Strongly recommended.
7. **Retention defaults** — 13 months of events is our proposal; regulated customers may need 7 years, which makes cold-storage export a v1 requirement rather than a v1.1 one.
8. **Alert rule management** — explicitly out of scope. Confirm we are *read-only* against Prometheus rules and will not drift into being a rule editor.
