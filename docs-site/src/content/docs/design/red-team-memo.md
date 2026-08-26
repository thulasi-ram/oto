---
title: oto — Red-Team Memo
---
**Author:** adversarial review (skeptical advisor role)
**Date:** 2026-08-07
**Status:** HISTORICAL — pre-implementation. Written before the first line of oto existed, to be read
before it was written. Kept as the record of the objections the design had to answer.
**Posture:** hostile-but-constructive. Nothing here is a recommendation to stop. Several things here are recommendations to stop doing a *specific thing*.

> **Read this as a document from 2026-08-07, not as a description of oto today.** It is dated by
> construction: every claim about what oto has, lacks or has not yet decided was true of an empty
> repository. oto has since been implemented, and the memo's open questions were closed by ADRs —
> the licence by [0019](/oto/adr/0019-mit-licence/), Kubernetes enrichment by
> [0016](/oto/adr/0016-mcp-enrichment-no-firehose/), Slack distribution by
> [0018](/oto/adr/0018-slack-distribution-model/). Where this memo and
> [SPEC.md](/oto/design/spec/) disagree, the SPEC wins; it sits below the SPEC and above
> `architect-proposal.md` in the precedence order the SPEC declares.

---

## 0. Executive summary — the five objections that matter

1. **"Alert-first + lifecycle timeline" is a feature, not a differentiator.** Grafana Alerting shipped a centralized alert history timeline in 11.2. Keep ships an incident timeline plus an activity/audit log. Robusta's SaaS platform explicitly markets "investigation timelines." You are entering a room where three vendors already have the thing you plan to lead with. The *only* defensible crack is that Keep's timeline is incident-scoped (not alert-scoped) and Grafana's is Grafana-managed-rule-scoped (not external-Alertmanager-scoped). That crack is real but it is one sprint wide for any of them.
2. **"Fetching the rule definition from Prometheus" is a 40-line HTTP client, not a moat.** `GET /api/v1/rules` returns the expr, the `for`, the labels and annotations. Grafana already renders the rule query next to the alert. If a competitor wants this, it is a Tuesday.
3. **Robusta is not "the closest competitor," it is a superset of your stated scope, minus the timeline.** k8s-native, Alertmanager webhook ingestion, enrichment with pod logs / events / CPU graphs, Slack smart grouping *into threads*, dedup, silencers, auto-remediation, AI investigation, MIT-licensed. Your entire v1 as described is a strict subset of Robusta OSS + Robusta Platform, except that Robusta OSS has no web UI at all and self-hosting their UI requires an enterprise plan. **That last clause is your actual opening — and it is a licensing/packaging opening, not a product one.**
4. **Postgres-as-only-store is fine for the alert *entity* and wrong for the alert *event stream*.** These are two different workloads sharing one word. Conflating them is the design error that will bite at ~10–50M events, and the fix is cheap now and expensive later.
5. **Your competitive read has a factual error that changes strategy.** You believe Keep is AGPL. Keep's core is **MIT**, with a proprietary `ee/` directory (AIOps correlation, RBAC, SSO, HA). Robusta OSS is also **MIT**. Neither is AGPL. This means (a) you cannot rely on AGPL scaring off self-hosters from competitors, and (b) *you* choosing AGPL would make you the most restrictive option in a market whose incumbents are permissive. Re-run the licensing decision with correct inputs.

**The one thing genuinely uncovered in the market:** Grafana OnCall OSS entered maintenance mode on 2025-03-11 and was **archived 2026-03-24**, with Cloud Connection and APIs switched off. Grafana's answer is Grafana Cloud IRM — a hosted, paid product. There is now a documented, dated vacuum for a *self-hostable, OSS, Slack-first alert layer over an existing Prometheus/Alertmanager stack, with a real UI you can run yourself*. Robusta won't fill it (UI is platform/enterprise). Keep partly fills it but is generic-AIOps-shaped, not k8s/Prometheus-shaped. **That vacuum, not "lifecycle timeline," is the thesis worth betting on.** Rewrite the pitch accordingly.

---

## 1. Attack on the differentiator

### 1.1 What each incumbent already ships

| Capability | Alertmanager UI | Prometheus UI | Grafana Alerting | Grafana OnCall OSS | Keep | Robusta | oto (planned) |
|---|---|---|---|---|---|---|---|
| Alertmanager webhook ingest | n/a | n/a | via ext. AM | yes | yes | **yes** | yes |
| k8s-native enrichment (pod logs, events) | no | no | no | no | partial | **yes** | planned |
| Slack notification | via receiver | no | yes | **yes (best-in-class)** | yes | **yes** | yes |
| Slack **threading** of related alerts | no | no | no | partial | partial | **yes ("smart grouping")** | yes |
| Persisted alert history | **no** | ALERTS/ALERTS_FOR_STATE series only | yes (Loki-backed) | yes | yes | yes (SaaS) | yes |
| Alert **state-transition timeline UI** | no | no | **yes (11.2+ central history)** | partial | **incident-level** | **yes (SaaS "investigation timelines")** | **alert-level** |
| Rule definition (expr/`for`) shown inline | no | **yes** | **yes** | no | no | no | yes |
| Grouping by name/namespace/fingerprint | grouping only | no | yes | yes | yes | yes | yes |
| **Self-hostable UI, OSS licence** | yes (trivial UI) | yes | yes | **archived 2026-03-24** | **yes (MIT core)** | **no (enterprise)** | **yes** |

### 1.2 The blunt version

- **Alertmanager's own UI** — the honest gap is real: it holds no history. It is a silencer console. GitHub issues #1976 and #2223 have asked for alert history for years and it is explicitly out of scope for the project. Prometheus's `ALERTS`/`ALERTS_FOR_STATE` series are a partial substitute and lose `keep_firing_for` state across restarts (#13957). **This is the strongest true statement in your premise.** Lead with it.
- **Grafana Alerting** — has the timeline you want to build, but only for *Grafana-managed* rules, and requires a Loki (or Prometheus) backend to be configured for state history. If the customer's rules live in Prometheus rule files and route through an external Alertmanager (the entire k8s-prometheus-stack world), Grafana's central history page does not cover them. **This is your second true statement.** It is narrower than you think but it is defensible.
- **Keep** — timeline is incident-scoped. Alert-scoped view shows status, metadata, correlation type, link back to source. So "alert-level lifecycle" is technically uncovered *by Keep*. But Keep's product bet is that alerts are raw material and incidents are the unit of work — which is a *defensible product opinion*, not an oversight. You are betting the opposite. **Be honest that this is a philosophical bet you might lose, not a gap you spotted.**
- **Robusta** — the overlap is near-total on ingestion, enrichment, Slack threading and dedup. Where you are different: their OSS has no web UI, their UI is SaaS-or-enterprise, and their centre of gravity has moved to AI investigation (HolmesGPT) and auto-remediation. They are drifting toward "AI SRE." That drift leaves the boring, deterministic, self-hosted "where did this alert come from and what did it do" surface under-tended. **Your wedge is Robusta's boring flank, not their front.**
- **incident.io / PagerDuty / Opsgenie** — different layer. They start at "a human is now responsible." You start upstream at "an alert exists." Genuinely less overlap, but they will happily tell a buyer they cover it.

### 1.3 Verdict

"Alert-first + lifecycle timeline" as a **headline** will not survive a competitive bake-off. As a **consequence** of a better headline, it is fine. The headline that survives:

> **The alert history layer your Prometheus stack does not have — self-hosted, with a UI, without adopting an AIOps platform or a paid SaaS.**

That framing makes Alertmanager (no history), Grafana (only its own rules), Robusta (no self-hosted UI), and Keep (incident-first, generic) each *fail on a named axis*. Your current framing makes only Alertmanager fail.

### 1.4 What is actually left uncovered (ranked by defensibility)

1. **Self-hosted OSS alert-history + routing UI post-OnCall-archival.** Dated, documented vacuum. Highest confidence.
2. **Rule provenance and drift.** Nobody answers "this alert fired — show me the rule expr *as it was at fire time*, who changed it, and how the threshold has moved." Grafana shows the *current* rule. Fetching the rule *and versioning it* is a genuinely novel, cheap, defensible feature. **This is the strongest specific idea in the memo — it upgrades "we fetch the rule" from a 40-line HTTP client into a real capability.**
3. **Per-alert notification accounting.** "This alert fired 47 times this month, cost 47 Slack pings, 0 acknowledgements, 0 human actions" → a defensible *alert quality* surface that argues for deleting rules. Nobody in this list does alert-hygiene-as-a-product well.
4. **Deterministic, auditable, no-LLM-required.** As every competitor bolts on an AI investigator, "boring and explainable" becomes a real segment (regulated, air-gapped, privacy-sensitive buyers).

---

## 2. Attack on scope — what is the Minimum *Lovable* Product

The owner said "unlimited time, don't oversimplify for MVP." Taking that seriously does **not** mean building everything. Unlimited time is best spent making a small surface *unreasonably good* — that is what makes a product lovable rather than complete. A wide, mediocre v1 is the single most common way a solo/stealth product dies.

### 2.1 Non-negotiable (cut any of these and it is not lovable)

- **Correct, idempotent ingestion.** Push webhook + pull reconciliation. If oto ever shows a wrong state, the product's entire premise (be the system of record) is dead. This is the load-bearing wall.
- **The alert-level timeline, done beautifully.** Firing → notified → acked → re-fired → resolved → stale-expired, with exact timestamps, source of each transition, and who/what caused it. Sentry-style. This is the emotional core; it must be the best-looking thing in the product.
- **Grouping that matches how humans think** — by rule name and namespace, drilling into fingerprints. Not just a flat list.
- **Slack delivery that never double-posts and never loses a thread.** One duplicated storm burns all trust. See §3.
- **Rule definition fetch + snapshot at fire time.** Cheap, and it is your genuinely novel bit (see §1.4.2).
- **Self-hostable in one `helm install`, with no external dependency beyond Postgres.** The go-to-market *is* the install experience for this category.

### 2.2 Cut from v1 (aggressively)

- **Pull API from Alertmanager as a primary path.** Build it as a *reconciliation* job (see §3.5), not as a first-class ingestion mode. Two ingestion modes doubles the correctness surface for near-zero user value.
- **Multi-channel abstraction.** Keep the `Channel` interface — it costs nothing at design time and is good hygiene — but **ship exactly one implementation**. Do not build Teams/email/webhook/PagerDuty sinks. A generic interface with one impl is fine; two impls before you have users is scope theatre.
- **Enrichment beyond labels/annotations/rule.** Do *not* build pod-log fetching, k8s event correlation, or CPU graphs in v1. That is Robusta's home turf, it needs cluster RBAC, and it is where you will lose 6 months. Ship "enrichment = the rule + the label context + links out."
- **Anything AI.** Every competitor is racing there. You cannot win it, and it directly conflicts with the "deterministic and auditable" wedge from §1.4.4.
- **Alert correlation / incident objects.** This is Keep's core competence and a research problem. Adding a half-good correlation engine makes you a worse Keep. Stay alert-first — it is your stated identity, so *actually stay there*.
- **On-call schedules, escalation policies, paging.** The OnCall vacuum is tempting. Resist for v1: phone/SMS/push means telephony vendors, compliance, and 24/7 reliability expectations you cannot meet as a stealth project. Slack-only is a defensible v1 boundary — *but say so out loud*, because it caps who can buy you.
- **RBAC / multi-tenancy / SSO.** Design the schema so `tenant_id` exists from day one (retrofitting is brutal). Do not build the enforcement, UI or SSO until a buyer asks.
- **Silencing / muting.** Tempting and small-looking. It is not: silencing means you now own a write path back into Alertmanager and can *suppress a real incident*. That is a safety-critical feature. Ship read-only first; earn the write path.

### 2.3 The scope cut in one sentence

> **v1 = "the black box flight recorder for your Prometheus alerts, plus a Slack sink that never lies." Nothing else.**

---

## 3. Hard failure modes and the architecture they demand *now*

Each of these has a "cheap now / brutal later" property. That is the filter for what belongs in the initial design.

### 3.1 Alert storms / thundering herd (thousands of alerts in one webhook batch)

**What happens naively:** Alertmanager POSTs a batch. Your handler opens a transaction, loops, inserts, and calls Slack inline. The HTTP request times out. Alertmanager retries the *whole batch*. You now have partial writes plus duplicate processing, and the retries compound into a self-inflicted DoS.

**Architecture-time mitigation:**
- **Split accept from process.** The webhook handler does exactly one thing: durably append the *raw* payload (whole batch, unparsed, with a receipt id) and return `200` in single-digit milliseconds. All parsing, enrichment and notification happens asynchronously. This is the single most important architectural decision in the document.
- Bound the raw payload size and reject (with a loud metric) beyond it, rather than OOMing.
- The processing side must be **batch-aware**: one `COPY`/multi-row upsert per batch, not N round trips.
- **Storm detection as a first-class concept, not a config knob bolted on later.** When >N distinct alerts arrive for one group in a window, the notifier must collapse to a single "storm" summary message with a count and a link, and *suppress* the individuals. Emitting 1,000 Slack messages is worse than emitting zero. This requires the notifier to be able to *decide not to send*, which means the notification decision must be a separate, re-evaluatable step — not a side effect of ingest.
- Alertmanager's own retry semantics: assume the same batch will arrive more than once. See §3.9.

### 3.2 Slack rate limits and notification backlog

**The hard numbers:** `chat.postMessage` is ~**1 message per second per channel** (with short burst tolerance). Separately, since 2025-09-02, non-Marketplace / commercially-distributed apps are throttled on `conversations.history` to **1 req/min with a max page of 15 objects**; custom internal apps get 50+/min. If oto is distributed as a Slack app (not installed as each customer's own internal app), reading channel history is effectively off the table.

**Architecture-time mitigation:**
- **Per-channel serialised delivery with a token bucket keyed on channel id.** Not a global limiter. Not per-worker. The unit of contention is the channel.
- **Honour `Retry-After` on 429 and never fail-open into a retry storm.** Exponential backoff with jitter, per channel.
- **Backlog must be *collapsible*, not just queued.** If 400 messages queue for one channel behind a rate limit, delivering all 400 twenty minutes late is a worse outcome than delivering one "38 alerts fired while we were throttled" summary. This means queued notifications must be **re-evaluated at send time against current alert state**, not frozen at enqueue time. Architecturally: the queue holds *intents* ("notify about group G"), not *rendered messages*.
- **Never depend on `conversations.history`** in the design. Store every `channel_id` + `message_ts` you create; the DB is your memory of Slack, not Slack.
- Decide **now** whether oto is a Marketplace app, a per-customer internal app, or self-hosted-BYO-token. It changes your rate limit tier and therefore your architecture. → §7.

### 3.3 Duplicate delivery from HA Alertmanager pairs

**What happens naively:** A standard HA Alertmanager pair gossips, but the gossip is best-effort — under partition, both send. You post twice.

**Mitigation:** Dedup on `(fingerprint, status, startsAt, endsAt)` — Alertmanager's own notion of identity — with a **unique constraint in the database, not a check in application code**. A read-then-write check races under concurrency; a unique index does not. Add a short (30–60s) dedup window keyed on the same tuple for the *notification* decision, so two near-simultaneous identical notifications collapse. Record which AM instance sent it (source IP / a configured instance label) so operators can *see* the duplication rather than having it silently swallowed.

### 3.4 Flapping and notification fatigue

**What happens naively:** An alert that flips every 90 seconds produces 40 Slack messages an hour. Users mute the channel. The product is now worthless — and worse, dangerous, because the muted channel also hides the real incident.

**Mitigation:**
- **Track flap rate as a stored property of the alert group**, computed on every transition (transitions per hour, EWMA). This must be in the schema at design time; recomputing historically is expensive.
- **Automatic damping**: above a flap threshold, stop posting new messages and instead *update the existing thread* ("flapped 12 more times"). Slack `chat.update` on a known `message_ts` is free of the per-channel post limit pressure and is far kinder to humans.
- **Surface flapping as a first-class UI state**, not a hidden behaviour. If the system quietly suppresses, users lose trust. "We are damping this alert — it flapped 31 times in an hour — click to see why" is trustworthy; silence is not.
- The `for:` duration belongs to Prometheus, not to you. Do not re-implement it. Do *surface* it — "this rule has `for: 0s`, which is why it flaps" is genuinely useful advice and part of the alert-hygiene wedge (§1.4.3).

### 3.5 "Resolved but never resolved" — stale alerts

**What happens naively:** Alertmanager's resolve notification is lost (network blip, your outage, `send_resolved: false` in the receiver config). Your DB shows firing forever. Users see phantom alerts and stop believing the UI. **This is the failure mode most likely to actually kill the product**, because it destroys the "system of record" claim directly.

**Mitigation:**
- **Every alert row carries an `expires_at` derived from Alertmanager's `EndsAt`** (Alertmanager sends a rolling `EndsAt` on every repeat; a firing alert whose `EndsAt` has passed without refresh is stale by definition). A sweeper transitions such alerts to an explicit **`stale`/`expired`** state.
- **`expired` must be a distinct state from `resolved`.** Never fabricate a resolution. Showing "we stopped hearing about this" is honest; showing "resolved" when you do not know is a lie the product cannot afford to tell.
- **This is where the pull API earns its place** — a periodic reconciliation against Alertmanager's `/api/v2/alerts` to find alerts we think are firing that Alertmanager does not know about, and vice versa. Ship it as reconciliation, not as an ingestion path. Emit a metric for reconciliation divergence; it is your canary for every bug in this list.

### 3.6 Label cardinality explosion in Postgres

**What happens naively:** Labels go in a `JSONB` column. Someone has `pod=web-7d9f8b-x2k4l`, `trace_id=...`, `customer_id=...` in their alert labels. Six months in: hundreds of millions of rows, a GIN index larger than the table, and label-filter queries that table-scan.

**Mitigation:**
- **Separate the *identity* labels from the *payload* labels.** Identity labels (alertname, namespace, severity, cluster, the ones you group and filter by) get promoted to typed columns with real btree indexes. Everything else stays in JSONB as opaque payload, indexed only if measured to be needed.
- **Do not index the full JSONB blob with a GIN index by default.** Add `jsonb_path_ops` GIN only on a measured need, and consider a curated allow-list of filterable keys.
- **Cap it.** Reject or truncate alerts beyond N labels or M bytes of labels, with a visible metric. Unbounded ingestion from an untrusted upstream is a resource-exhaustion vector as much as a performance problem.
- **Detect cardinality abuse and tell the user.** "The label `pod` on rule X has 40,000 distinct values; that is why your alert list is slow" is a *feature*, not just a defence. Again: alert hygiene as product.
- **Store label sets deduplicated.** In k8s, the same label set recurs thousands of times. A `label_sets` table keyed by a hash of the canonicalised set, referenced by events, converts a large repeated blob into a foreign key. Do this on day one; migrating a hot 100M-row table later is a weekend you will not enjoy.

### 3.7 Thread parent lost / deleted message / archived channel

**What happens naively:** You store `thread_ts`, later post a reply, Slack returns an error, your worker retries forever, the queue head-of-line blocks, and every other notification for that channel stops.

**Mitigation:**
- **Enumerate the terminal Slack errors and treat them as state transitions, not retries**: `channel_not_found`, `is_archived`, `message_not_found`, `thread_not_found`, `not_in_channel`, `token_revoked`, `account_inactive`. These are permanent. A permanent error must **degrade to posting a fresh root message** (with a "continued from a previous thread" marker) and mark the old thread pointer dead — never retry, never drop silently.
- **The thread pointer is a nullable, invalidatable field with a reason**, not an assumed-valid string.
- **Head-of-line blocking is the real killer.** A per-channel serialised queue must be able to *skip* a poisoned item. Cap attempts, then dead-letter with the alert still visible in the UI marked "notification failed."
- **The UI must show delivery state per alert.** "We could not tell Slack about this" has to be visible in oto, or oto's silence becomes indistinguishable from no alert. This is a moral obligation as much as a technical one (§6).

### 3.8 Clock skew between Alertmanager and oto

**What happens naively:** You mix `startsAt` (their clock) with `created_at` (your clock) on one timeline. Events appear out of order, or in the future. The timeline — your headline feature — visibly lies.

**Mitigation:**
- **Two timestamp columns on every event, always, and never conflate them:** `source_ts` (what the upstream claims) and `observed_ts` (when oto durably recorded it, from oto's clock).
- **Order the timeline by `observed_ts`; display `source_ts`.** Ordering by a remote clock is how you get a "resolved" event above a "firing" event.
- **Detect and surface skew**: on ingest, record `observed_ts - source_ts`; if it exceeds a threshold, badge the alert in the UI and emit a metric. Skew is an operational problem in the *customer's* cluster, and telling them is a feature.
- Store everything as `timestamptz` in UTC. No naive timestamps anywhere, ever.

### 3.9 At-least-once delivery → duplicate Slack messages

**What happens naively:** Worker sends to Slack, crashes before recording the send, retries, posts twice. Users see double. Trust gone.

**Mitigation:**
- **Idempotency keys end to end.** A notification intent has a deterministic id derived from `(group_key, transition_id, channel_id)`. A unique constraint on the *sent* record makes a duplicate send a constraint violation rather than a second message.
- **Record intent-to-send *before* calling Slack** (a claim row), and the resulting `message_ts` after. On recovery, an intent with a claim but no `message_ts` is *ambiguous* — it may or may not have been delivered. **Design the ambiguous case explicitly**: prefer under-delivery for informational transitions, and for critical firing transitions, re-send with an explicit "(possible duplicate after recovery)" marker. Do not pretend the ambiguous case does not exist; distributed systems do not permit exactly-once, and the honest design says so.
- Where Slack supports it, prefer `chat.update` on a known `message_ts` over a new post — update is naturally idempotent.

### 3.10 Back-pressure when Postgres is slow

**What happens naively:** Postgres degrades (autovacuum, a bad query, disk). The webhook handler blocks on connection acquisition. Alertmanager times out and retries. Retries consume more connections. Total collapse, and — worst of all — **alerts are lost during the exact window when things are going wrong in the customer's cluster.** Your product fails precisely when it matters.

**Mitigation:**
- **Bounded connection pool with a short acquisition timeout**, and shed load *deliberately*: return a `503` with `Retry-After` rather than hanging. Alertmanager retrying is a designed behaviour; a hung handler is not.
- **The ingest path must have a shorter timeout and a separate, reserved pool from the read/UI path.** UI queries must never be able to starve ingestion. Two pools, sized independently, from day one.
- Consider an on-disk durable buffer (a local WAL/spool file) in front of Postgres for the raw-append step, so ingestion survives a Postgres outage entirely. This is a genuine architecture-time decision — bolting it on later means re-plumbing the whole ingest path. It is also the difference between "we lost your alerts during the incident" and "we replayed them."
- **Have a golden signal for the ingest path itself** (`accepted`, `rejected`, `spooled`, `lag`), and make it the first thing on your own dashboard. A monitoring product that cannot monitor itself is an embarrassment waiting for a customer to find.

### 3.11 Reprocessing / replay after an outage

**What happens naively:** You come back after 40 minutes down. 40 minutes of alerts arrive at once (or were spooled). You dutifully post 900 Slack messages about things that already resolved. Users revolt.

**Mitigation:**
- **Replay must be state-aware, not event-aware.** Because notification intents are re-evaluated at send time (§3.2), replay naturally collapses: an alert that fired and resolved during the outage produces at most one summary, not two messages.
- **An explicit "catch-up mode"** with a distinct notification policy: on restart, ingest and record *everything* (history must be complete), but notify only on *current* state, with one "while oto was down (14:02–14:41), 37 alerts fired and 31 resolved" digest. History fidelity and notification volume are separate concerns — the architecture must let you turn one up and the other down.
- Because the raw payloads were persisted at accept time (§3.1), **replay is a re-run of the processing stage over stored raw batches** — which also gives you a bug-fix superpower: fix a parser bug, reprocess. Do not throw the raw payloads away. Retain them (with a bounded window, see §6).

### 3.12 Upgrade / migration of in-flight state

**What happens naively:** You deploy a schema change while 4,000 notification intents sit in the queue and 200 Slack threads are open. Old workers write the old shape, new workers read the new shape, and the queue corrupts.

**Mitigation:**
- **Expand/contract migrations, enforced as a rule, not a habit**: add nullable → dual-write → backfill → switch reads → drop. Never a destructive migration in one release.
- **Version the queue payload schema explicitly.** A worker that meets a payload version it does not understand must park it, not guess.
- **The Slack thread pointer must survive schema changes** — treat `(channel_id, message_ts, thread_ts)` as an external, immutable, never-reshaped identity. It is a foreign system's primary key; you do not get to refactor it.
- Assume rolling deploys with N and N+1 running simultaneously. Design for it now; it is a schema-shape decision, not a deploy-script decision.

---

## 4. Challenge to the tech choices

### 4.1 Postgres as the only store

**Where it is genuinely right:** Postgres is the correct system of record for the *alert entity* — current state, grouping, dedup constraints, thread pointers, user annotations, ack state. You need transactional integrity across "record state" + "claim notification," and that is exactly what a relational DB with unique constraints is for. Splitting that across two stores at v1 would be the bigger mistake. **Keep Postgres. This is not a fight worth having.**

**Where it breaks:** the *event stream* — every state transition, every notification attempt, every enrichment fetch. This is append-heavy, immutable, time-ordered, queried by range, and never updated. That workload on a vanilla Postgres table gives you: table bloat, autovacuum pressure on your hottest table, indexes that outgrow RAM, and a `DELETE FROM events WHERE ts < ...` retention job that is itself an outage.

**Rough scale intuition** (order-of-magnitude, verify with your own load test — do not take these as gospel):
- 100 alerts/min sustained ≈ 4.3M events/month with a handful of transitions each. Fine on anything.
- 10 clusters × moderate noise ≈ 50–100M rows/year. **This is where a naive schema starts hurting**: index size, vacuum, and slow `ORDER BY ts DESC LIMIT` over a wide table.
- Beyond that, unpartitioned tables become a retention and vacuum problem more than a query problem.

**Mitigations, all cheap now:**
- **Declaratively partition the event table by time from day one** (monthly or weekly). Retention becomes `DETACH PARTITION` + `DROP` — instant, no vacuum storm. Retrofitting partitioning onto a 100M-row live table is a genuine migration project.
- **Normalise label sets** (§3.6) — the single biggest row-size win.
- **Separate hot from cold**: current alert state in a narrow, heavily-indexed table; the event log wide and append-only. Do not make the UI's list query touch the event log.
- **Consider TimescaleDB as an extension** rather than a second database — you keep one operational surface, one connection pool, one backup story, and get hypertables and compression. Evaluate; do not adopt reflexively.
- **Explicitly reject** a second datastore (ClickHouse/Loki/ES) for v1. It doubles operational burden for a self-hosted product whose *entire go-to-market* is "one helm install." One-binary-plus-Postgres is a competitive advantage against Keep's heavier footprint. Protect it.

### 4.2 Postgres-backed job queue — sufficient, or a trap?

**Verdict: sufficient, and correct for v1 — but only if built with a specific set of properties.** It is a trap only if built naively.

**Why it is right here:** Your queue depth is small (notifications, not video transcoding). Your throughput ceiling is *Slack's* 1 msg/sec/channel, not the queue's. And you get the one thing an external broker cannot give you: **the enqueue and the state change in a single transaction.** With Redis/NATS/SQS you get the dual-write problem — DB commits, broker publish fails, notification lost (or vice versa: notification sent, state not recorded → duplicate). Avoiding dual-write is worth far more than the throughput an external broker would add. Adding Redis/Kafka to a self-hosted product also breaks the one-install promise.

**Where it becomes a trap, and the required properties:**
- `SELECT ... FOR UPDATE SKIP LOCKED` is mandatory. Anything else (polling with `UPDATE ... WHERE status='pending'`) either serialises or races.
- **Long-running transactions holding a job open will wreck autovacuum.** Claim-then-work-then-ack with short transactions; do not hold a transaction across the Slack HTTP call.
- **Dead jobs must be reclaimable** (visibility timeout / heartbeat), or a crashed worker parks a job forever.
- **Per-channel ordering with skip-ahead** (§3.7) — the naive `SKIP LOCKED` pattern gives you no ordering guarantee. You need a channel-scoped lock (a Postgres advisory lock keyed on channel id is a clean fit) so one channel's messages serialise while other channels proceed.
- **Dead-letter table with visibility in the UI**, not silent drops.
- **Bound the queue.** An unbounded queue table during a storm is how the queue becomes the outage.
- **Retention on completed jobs** — the completed-jobs table is a bloat source; partition or aggressively prune.

**When to revisit:** if you ever exceed roughly low-thousands of jobs/sec, or need fan-out to many consumer types. Neither will happen in v1. Do not pre-build for it. Do keep the queue behind a narrow interface so the swap is possible.

### 4.3 SolidJS vs React

**Fair pros for Solid:** genuinely excellent fine-grained reactivity, which is a real fit for a live-updating alert list — no virtual DOM diffing on high-frequency updates, no `useMemo` archaeology, small bundle. Signals are a better mental model than hooks for this exact shape of app (many small independently-updating cells). If the owner is the primary developer and enjoys it, **developer joy on a long solo project is a legitimate, non-trivial technical input** — a project you like working on gets finished.

**Real, honest liabilities:**
- **Component libraries.** This is the concrete cost, not a vague one. React has shadcn/ui, Radix, TanStack, Recharts, MUI, Ant, and every dashboard/table/date-range/virtualised-list component you will need. Solid has Kobalte, Ark UI (which does support Solid), solid-ui, and a much thinner long tail. For an *alert console* — which is 80% dense data table, filters, date ranges, virtualised lists and a timeline chart — this is precisely the worst category to be short on components. **Budget real weeks for building what you would have installed.** Note that TanStack Table and TanStack Virtual are framework-agnostic and do support Solid; that meaningfully softens the worst of it. The timeline visualisation you will build yourself either way (nobody has "sentry-style alert lifecycle timeline" off the shelf), so that headline feature is a wash.
- **Hiring / contribution.** For a stealth solo project this is ~zero cost today. If oto is OSS and wants outside contributors, or if you hire, the pool for Solid is an order of magnitude smaller. **This is a bet on the project's future shape, not on the framework.**
- **AI assistance quality.** Blunt and often unmentioned: models generate better React than Solid, simply from training volume. On a project leaning on agent assistance, that is a real, measurable throughput tax.
- **Ecosystem risk.** Solid is healthy but small. A key maintainer stepping away is a bigger deal than in React's world.

**Where it is genuinely win-win:** Solid's reactivity model is a real fit for a streaming, always-live UI, and the primitives (`createSignal`/`createStore`/`createResource`) map cleanly onto "an alert is a cell that updates." If the plan is deep custom UI (which it is — a bespoke timeline), the component-library gap shrinks, because you were building it anyway.

**Verdict:** **not a liability worth reversing, but do it with eyes open.** The decision hinges on one question only: *is oto ever going to want outside contributors or hires?* If yes, React is the boring correct answer and the framework is not where you should spend novelty budget. If it stays solo/small, Solid is fine and possibly better. **Make that call explicitly (§7) rather than by default.** Whatever you choose, keep the data layer (fetching, caching, live updates) behind a framework-agnostic boundary so the UI is replaceable — that is the cheap hedge, and it costs nothing today.

---

## 5. The "why not just…" test

The honest answers. Where the answer is weak, that is flagged.

**(a) Why not just point Alertmanager at a Slack receiver?**
*Strong answer.* Because Alertmanager forgets. There is no history, no "how often has this fired," no ack, no lifecycle, no cross-restart memory; the UI is a silence console. The Slack receiver's templating is `text/template` in YAML — painful to maintain and impossible to make interactive. And there are no threads: every notification is a new top-level message, so a channel becomes an undifferentiated wall. **This is your best "why not just."** Note the honest counter: a very large number of teams *do* just do this and are content, because their alert volume is low. Your buyer is the team where it has already broken.

**(b) Why not just use Grafana Alerting?**
*Medium answer.* If all your rules are Grafana-managed and you already run Loki, Grafana's central alert history genuinely covers most of this, and it is free. Your honest answer: because your rules live in Prometheus rule files managed by GitOps and route through an external Alertmanager (the kube-prometheus-stack default), Grafana's state history does not cover them; it requires a Loki/Prometheus history backend you may not want to operate; and Grafana's Slack integration is notification-shaped, not conversation-shaped (no threading model, no ack from Slack). **Weakness: a determined user can migrate rules into Grafana.** You must be honest that you are betting on Prometheus-native rule management staying dominant in k8s. That is a reasonable bet, but it is a bet.

**(c) Why not just use Keep?**
*Medium answer.* Keep is generic (130+ providers) and incident-first: alerts are raw material, incidents are the unit of work. If you want a single pane over Datadog + Sentry + Prometheus + New Relic, use Keep — oto will be worse. Choose oto if you are Kubernetes/Prometheus-only and want alert-level fidelity: rule provenance, per-alert lifecycle, k8s-shaped grouping, a lighter footprint. **Weakness: "we're more focused" is the weakest form of differentiation and ages badly.** Also correct your model of Keep: **MIT core, proprietary `ee/`** for AIOps correlation/RBAC/SSO/HA — not AGPL. Their permissive core makes them *harder* to displace, not easier.

**(d) Why not just use Robusta?**
*Weakest answer — this is the one to worry about.* Robusta does k8s-native enrichment better than you will for a long time, has Slack smart grouping into threads, dedup, silencers, auto-remediation, an AI investigator, and an MIT licence. Your honest answers are narrow: (i) **Robusta OSS has no web UI at all, and self-hosting their platform UI requires an enterprise plan** — so "a self-hosted UI without a commercial contract" is a real, checkable gap; (ii) their gravity has moved to AI-driven investigation and remediation, which some buyers (regulated, air-gapped, privacy-conscious, or simply LLM-sceptical) actively do not want; (iii) alert-level lifecycle and rule provenance are not their focus. **If you cannot articulate (d) crisply to a stranger in 20 seconds, do not start building.** Being unable to answer (d) is a stop signal.

**(e) Why not just use PagerDuty / Opsgenie?**
*Strong answer.* Different layer and different price. They begin where a human is paged and are priced per-seat with the assumption that a human's time is the scarce resource. They do not care what your Prometheus rule expr was, they do not live in your cluster, and their k8s context is shallow. Also: Opsgenie's sunset pushed a cohort of teams into re-evaluation, and PagerDuty's per-seat pricing makes "give the whole platform team visibility" expensive. Many teams will run oto *and* PagerDuty — oto as the alert layer, PD as the human-escalation layer. **Design for coexistence, not replacement.** Say this out loud in the pitch; it removes the biggest objection.

---

## 6. Product / moral compass

Taking the owner's "keep a moral compass" seriously. These are not compliance checkboxes; several are product-defining.

**6.1 On-call psychological safety — the central one.**
An alert platform's real output is *interruptions to human beings*, often at 3am. Every feature that makes it easier to send a notification makes it easier to hurt someone. Build the counterweight into the product from the start, not as a v3 "analytics" feature:
- **Make alert *deletion* a first-class, celebrated action.** The best alert is the one that no longer exists. A UI that surfaces "this rule fired 200 times and was never acknowledged — consider deleting it" is doing more good than any enrichment.
- **Never make notification volume a vanity metric.** "12,000 alerts processed!" is a dashboard for a product manager, not for a tired engineer. Prefer "notification volume down 40%."
- **Default to quiet.** Damping, grouping and storm-collapse (§3.1, §3.4) should be *on* by default, opt-out. The default configuration is the ethical statement.

**6.2 Alerting-as-surveillance.**
The moment you store ack/resolve events with user identity, you have built a dataset that answers "who responds slowest?" A manager *will* ask for that view. Decide the position **now**, because the schema encodes it:
- Recommended: **store acknowledgement identity** (operationally necessary — "who is on this?") but **do not build per-person response-time leaderboards**, and consider not building the aggregate at all. A feature you do not build cannot be misused.
- If team-level MTTA/MTTR is built, **aggregate at team level with a minimum group size**, and make individual attribution unavailable rather than merely hidden — hidden data gets exported via API.
- Be explicit in the docs about what is recorded about individuals. Engineers are your users; they will read it, and they will trust you more for it.

**6.3 Data retention and sensitive labels.**
Alert labels routinely carry `namespace` (often a customer name in multi-tenant clusters), `customer_id`, `tenant`, `email`, URLs with tokens in annotations, and occasionally credentials in error-message annotations. If oto persists raw payloads (§3.11), it persists all of it, forever, by default.
- **Configurable retention from day one, with a sane default** (raw payloads shorter than aggregated events — e.g. raw 7–14 days, events 90 days). Partitioning (§4.1) makes this operationally trivial. Not shipping retention is not a neutral omission; it is a decision to hoard.
- **Label redaction / allow-list at ingest**, applied *before* the raw persist, so sensitive labels never land.
- **Never log full alert payloads at info level.** Your own logs become the leak.
- If SaaS is ever offered: this becomes GDPR-relevant personal data (engineer identities, and possibly customer identifiers in labels). DPA, deletion, and export are then table stakes, not extras.

**6.4 Self-hosted vs SaaS trust.**
This category's buyers are platform engineers, structurally sceptical of shipping cluster telemetry to a third party. **Self-hosted-first is both the moral and the commercial answer**, and it is the gap Robusta leaves open (their UI is SaaS-or-enterprise). If SaaS follows, do not degrade the self-hosted version to sell it — the open-core line should fall on *organisational scale* features (SSO, RBAC, multi-cluster fleet management, audit export), never on *correctness or safety* features (delivery reliability, retention controls, redaction). **Rate limiting, dedup and storm protection must never be paywalled.** Paywalling safety is how open-core products lose their communities.

**6.5 Licensing.**
Correct the premise: **Keep's core is MIT** (proprietary `ee/`), **Robusta OSS is MIT**, and **Grafana OnCall was AGPLv3 — and is archived**. Implications:
- Choosing AGPL makes oto the *most* restrictive option in a permissive field. AGPL protects against a hyperscaler hosting you — a risk that is essentially zero at your stage and non-zero only after success. It also deters exactly the corporate self-hosters who are your first users, and their legal teams.
- Realistic options: **Apache-2.0** (max adoption, no protection), **AGPL-3.0** (protection, adoption friction), **BSL/Elastic-style source-available** (protection, but you forfeit "open source" and the goodwill that comes with it), or **open-core** (permissive core + proprietary enterprise dir, which is what both live competitors actually do). → §7.
- Whatever you pick, **you cannot copy code from Keep or Robusta**. MIT permits reuse *with attribution*; a stealth product silently vendoring their code is both a licence breach and a reputational bomb. Read for ideas; write your own.

**6.6 The ethics of "stealth" and of cloning.**
Stealth-as-in-quiet-until-ready is fine and normal. Two lines are worth holding:
- **Studying competitors is legitimate; misrepresenting origin is not.** If oto is meaningfully inspired by Robusta's threading model or Keep's provider architecture, saying so costs nothing and buys credibility. The OSS observability community is small and has a long memory.
- **Do not build a feature-for-feature clone and market it as new.** Beyond the ethics, it is a losing strategy: you will always be a version behind on a surface you did not invent. The §1.4 list exists precisely so you build something they did not.
- One more, unglamorous: **if oto is unreliable, people miss real incidents.** This category has an asymmetric harm profile — a broken todo app wastes time; a broken alert pipeline lets an outage run unattended. That argues for the narrow scope in §2 and for shipping the reliability work *before* the feature work. Being small and correct is the moral choice as well as the strategic one.

---

## 7. Decisions that genuinely need the human

Ranked by how expensive it is to reverse later.

**1. Is oto self-hosted-first OSS, open-core, or a SaaS?**
Options: pure OSS (Apache) / open-core (permissive core + proprietary enterprise) / SaaS-first / source-available (BSL).
Tradeoff: this sets the licence, the tenancy model, the trust story and whether `tenant_id` and RBAC seams must exist in the v0 schema — all painful to retrofit. It is also the decision that determines whether you are attacking Robusta's flank (self-hosted UI) or their front.

**2. Slack app distribution model: Marketplace app, per-customer internal app, or BYO-token self-hosted?**
Options: as above; they are not mutually exclusive but the *primary* one drives design.
Tradeoff: directly sets your API rate-limit tier (non-Marketplace distributed apps are throttled to 1 `conversations.history` req/min with 15-object pages; internal apps get 50+/min), which changes the notifier architecture — not a config flag. Marketplace review also constrains scopes and timelines.

**3. Is oto alert-first *forever*, or does it grow incidents and on-call?**
Options: stay alert-layer and coexist with PagerDuty / grow into incident objects (compete with Keep) / grow into on-call and paging (fill the archived Grafana OnCall vacuum).
Tradeoff: the third is the largest real market opening but drags in telephony, 24/7 reliability obligations and compliance. Choosing "stay alert-first" is fine, but it must be a *stated* boundary because it caps the buyer.

**4. What is the licence, exactly?**
Options: Apache-2.0 / AGPL-3.0 / BSL / open-core split.
Tradeoff: effectively irreversible once contributors exist (relicensing needs every contributor's consent). Note the premise correction: both live competitors are MIT-cored, so AGPL would make you the most restrictive option in the field.

**5. Does oto ever get a write path back into the cluster (silences, remediation)?**
Options: strictly read-only observer / silence-only write / full remediation (Robusta's territory).
Tradeoff: writes make oto safety-critical — a bug can suppress a real incident — and change the RBAC, audit and testing burden by an order of magnitude. Read-only is defensible and much cheaper to be *correct* at.

**6. Will oto ever want outside contributors or hires? (This, and only this, decides SolidJS vs React.)**
Options: solo/small forever → Solid is fine and arguably better; open to contributors/hires → React is the boring correct answer.
Tradeoff: the framework decision is downstream of the people decision; deciding it on technical merit alone is deciding it on the wrong axis.

**7. Data retention defaults, and the position on individual-level metrics.**
Options: retain-everything-forever / bounded defaults with redaction; and: no individual metrics / individual metrics available / team-aggregate only.
Tradeoff: encoded in the schema and in customer trust; both are far harder to walk back after the first customer depends on the data. This is the "moral compass" decision with actual teeth.

**8. Who is the first design partner, and does the k8s-only constraint hold?**
Options: stay Prometheus/Alertmanager-only (sharp, small) / add sources early (broader, becomes a worse Keep).
Tradeoff: focus is your only advantage against three better-resourced competitors, but a single design partner with a non-Prometheus source will pull you off it. Decide the answer before the question arrives with a customer attached.

**9. AI: never, optional, or core?**
Options: deterministic-only as a positioning stance / optional pluggable / core capability.
Tradeoff: "no LLM required" is a genuine and increasingly rare differentiator as Robusta and Keep both lean into AI, and it serves air-gapped and regulated buyers — but it forfeits the narrative most of the market is currently buying.

---

## 8. What to do first (if you take all of the above)

1. Rewrite the one-line pitch around the **post-OnCall self-hosted vacuum**, not around "lifecycle timeline."
2. Answer §5(d) — "why not Robusta" — in 20 seconds, out loud, to someone else. If you cannot, stop and rethink.
3. Make decisions **1, 2 and 4** from §7 before writing schema, because they are schema-shaped.
4. Build in this order: **durable raw ingest → idempotent state machine → reconciliation/staleness → timeline UI → Slack sink.** Note that Slack comes last: it is the demo, but it is the least architecturally load-bearing part, and building it first is what tempts you to skip §3.
5. Write the load test that fires a 5,000-alert batch **before** the feature that handles it.

---

*Nothing in this memo says don't build it. It says: the differentiator you named is not the one you have, the scope you described is about three times too wide for a lovable v1, and six of the twelve failure modes in §3 are unfixable-in-place if you get the schema wrong in week one.*
