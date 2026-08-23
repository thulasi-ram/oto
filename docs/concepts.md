# oto concepts

This document is the canonical description of oto's domain model: every noun the system has, what
each is keyed by, how they contain one another, and the path one Prometheus alert takes from the
moment a rule fires to the moment somebody acknowledges it in Slack. `CONTEXT.md` is the short
orientation, `docs/design/SPEC.md` is the binding specification, and this file sits between them and
is self-contained.

oto is an alert history layer that sits behind an existing Prometheus/Alertmanager stack. It records
every alert that has ever fired, every firing episode since, what the alerting rule said at that
instant, who was told on which channel in which thread, who acknowledged it, and how it ended — as
one replayable timeline, in a web UI and in Slack. It is a flight recorder, not an incident manager,
and §6 says exactly where that line falls and how it is enforced.

---

## 1. The model at a glance

```
Org
└── Cluster                                  (identity / failure domain)
    │   └── AlertSource ──── one Alertmanager (+ optional Prometheus)
    │        └── Silence    (read-only mirror; no write path)
    └── Alert                                (identity of a label set; forever)
        │   ├── Snooze      (0..1 active; suppresses oto's own notifications)
        │   └── ack_state   (unacked | acked)  ── orthogonal to state
        └── AlertCase                         (one contiguous firing episode)
            ├── RuleSnapshot                  (content-addressed rule at fire time)
            ├── Enrichment ×N                 (one typed result per Enricher)
            ├── AlertEvent ×N                 (append-only; the timeline)
            └── Conversation                  (kind=case, id=this Case; exactly one)
                └── ChannelThread ×N          (per Channel: slack_channel_id + root_ts)

Delivery side, hung off the org rather than off the signal:

Connection ──▶ Channel ×N     FAN-OUT: one org-wide provider setup serves many
                              destinations, each with its own verbosity

NotificationPolicy   matchers + reasons + subject kind ──▶ ChannelIDs (1..16)
                     + template_id, throttle, count condition, digest window

Notification                  one idempotent intent to communicate ONE fact
└── NotificationDelivery ×N   FAN-OUT: one per Channel the policy names, ≤ 16
    └── ChannelThread         one root message updated in place, ≤ 30 replies
```

Those two fan-out points are where ordering and idempotency come from. Everything else is 1:1, or
1:many with an obvious owner: an Alert has many Cases but at most one open, and a Case has exactly
one Conversation.

---

## 2. The nouns

Each entry: a definition, what it is keyed by, and the one thing people get wrong.

### 2.1 Signal

**Alert** — the identity of a label set. Created on first sight and never deleted; an Alert survives
resolution forever, which is the whole point of the product.
*Keyed by* `alert_key`: SHA-256 of `(org_id, cluster_key, canonical labels minus the source's
ignore_labels)`, truncated to 128 bits, rendered as `ak_` plus 26 base32hex characters, with dedup
enforced by `UNIQUE (org_id, alert_key)` rather than a read-then-write check.
*Commonly wrong:* treating an Alert as a thing that happens. It *exists*; the happening is an
AlertCase, so "it fired three times last month" is three Cases of one Alert.

**AlertCase** — one contiguous firing episode of an Alert. The object you acknowledge, and the object
whose **firing duration** is measured.
*Keyed by* `(alert_id, seq)`, `seq` starting at 1. `alert_cases.state` holds `open` or `closed` and
nothing else; the four-way reading is derived.
*Commonly wrong:* assuming a Case can reopen. `open → closed` happens once; a re-fire opens the next
`seq`, unacknowledged, whatever the clock says, because an acknowledgement is a receipt for *one*
firing (ADR 0040; the `refire_grace` setting that blurred it went in migration 00071).

**AlertEvent** — one immutable thing that happened at one instant. Append-only; the UI timeline reads
this table directly, and every current-state column elsewhere is a *projection* of it.
*Keyed by* an id, monthly-partitioned on `(org_id, occurred_at)`. Roughly thirty types spelled
`noun.verb`: `alert.created`, `case.opened`, `case.resolved`, `case.acknowledged`, `alert.snoozed`,
`rule.snapshot_captured`, `rule.definition_changed`, `delivery.sent`, `comment.added`.
*Commonly wrong:* reaching for `UPDATE`. If you would ever want to update the row, it is not an Event;
corrections are appended, not applied.

**RuleSnapshot** — a content-addressed capture of one Prometheus alerting rule at one instant: the
expression, the `for` duration, the labels and the annotations as they were when this Case opened.
The headline differentiator, and the reason `rule_changed` is a fact rather than a guess.
*Keyed by* its content hash, so an unchanged rule across a thousand Cases is stored once; bound to a
Case by a `rule.snapshot_captured` event (ADR 0009, "Rule snapshot versioning at fire time").
*Commonly wrong:* expecting it to be current. It is what the rule said *then* — the only version that
explains why the alert fired.

### 2.2 Ingest

**AlertSource** — one configured Alertmanager, optionally paired with a Prometheus for rule fetching.
Owns its credentials, its health status, and its own ingest token.
*Keyed by* a uuid, scoped to an org, assigned to exactly one Cluster. HA replicas are several
AlertSources sharing one Cluster, which stops a three-replica mesh minting three Alerts for one set.
*Commonly wrong:* treating a source as a tenant boundary. The org is the tenant boundary and the
Cluster the identity boundary; a source is just a connection.

**Cluster** — an identity and failure domain, *keyed by* `(org_id, cluster_key)` with the key an
operator-chosen slug. `cluster_key` participates in the alert key, so the same label set in
`prod-us-east-1` and in `staging` is two Alerts, not one.
*Commonly wrong:* reading it as a Kubernetes cluster. It is whatever failure domain you want alert
identity partitioned by; Kubernetes is the common case, not the definition.

**Silence** — a read-only mirror of an Alertmanager silence, synced by the reconciler so the UI can
say *why* something is suppressed.
*Keyed by* the upstream silence id within a source.
*Commonly wrong:* trying to create one. oto has no write path into your cluster, and a contract test
fails if the OpenAPI document grows a mutating silence operation. This refusal rests on safety, and
is the one in oto that can be earned later.

**Enrichment / Enricher** — an **Enricher** is a named, versioned plugin answering one question about
a firing alert; an **Enrichment** is one typed, provenanced result from one: what was asked, what
came back, which version answered, when.
*Keyed by* `(case_id, enricher_name, version)` for caching. The pipeline is budgeted — past the
pre-notification budget oto sends the card and lets late results arrive as an `enriched` follow-up
(ADR 0016, "MCP enrichment, no firehose").
*Commonly wrong:* putting an Enricher on the ingest path. It runs as its own River job, and if it is
slow the alert still goes out on time.

### 2.3 Delivery

**Connection** — one org-wide provider setup: a Slack workspace's bot token and team id, or a
webhook's shared secret.
*Keyed by* a uuid, scoped to an org; many Channels reference one Connection, and that sharing is why
the noun exists (ADR 0047, "A channel answers to a connection", migration 00075).
*Commonly wrong:* expecting it to carry behaviour. It carries credentials and nothing else — no
renderer, no verbosity, no thread-update preference, no health; those live on the Channel.

**Channel** — a configured destination *instance*: "Slack workspace T123, `#sre-alerts`", not a
channel *type*.
*Keyed by* a uuid referencing a Connection; carries the renderer, health, and `verbosity` (`all`,
`status_changes`, `firing_and_resolved`, `firing_only`).
*Commonly wrong:* assuming verbosity can silence a channel. It gates **thread replies only** — a
channel silently going quiet is indistinguishable from an alert that never fired, which is the failure
mode oto exists to prevent.

**NotificationPolicy** — the routing rule: matchers, reasons, a subject kind and the destinations to
send to; optionally a template, a throttle, a count condition and a digest window.
*Keyed by* a uuid, scoped to an org, ordered by `priority`. Bounds enforced by CHECK constraints and
mirrored in Go: at most 32 matchers, at most 15 reasons (exactly the size of the Reason vocabulary,
by rule), 1 to 16 channel destinations, priority 0..10000. Evaluation is **priority-ordered, LOWER
FIRST, first match wins** — priority 0 is considered before priority 100.
*Commonly wrong:* looking for a way to route to a person. The struct has `ChannelIDs []uuid.UUID` and
no other target field: no `user_ids`, no `schedule_id`, no `time_of_day`. See §6.

**Matcher** — one label predicate, using Alertmanager's four operators with Alertmanager's semantics:
`=`, `!=`, `=~`, `!~`. Both regex forms are **fully anchored**, so `severity=~"crit"` does not match
`critical-but-ignorable`.
*Keyed by* nothing — a Matcher is a value stored inline on the policy.
*Commonly wrong:* expecting more expressiveness. Matchers were chosen over a general expression
language deliberately (ADR 0017, "Matchers over CEL"), and they match **labels only** — no time of
day, no day of week, no "unless someone is awake", each of those being a person-shaped predicate in
a schedule costume.

**Reason** — why oto is communicating. A closed vocabulary of exactly fifteen values:

```
fired        all_resolved  repeat        suppressed    unsuppressed
expired      refired       acked         unacked       snoozed
unsnoozed    enriched      rule_changed  comment       digest
```

A Reason is a fact about a signal, never a routing decision about a human, and it is the single
input deciding update-in-place versus thread reply. Two footnotes: `refired` is **retired** —
nothing has produced it since ADR 0040, but rows carry it and a renderer must still draw one — and
`digest` is the only Reason no transition produces, because a clock tick mints it.
*Commonly wrong:* adding one. The set has only ever shrunk; §7 lists the four values that left and
why.

**Notification** — the channel-agnostic, idempotent intent to communicate one fact, one per fact no
matter how many channels see it.
*Keyed by* a uuid plus an idempotency key derived from `(subject, reason, salient state)`, so a
redelivered `notify.evaluate` job re-derives the same key and writes nothing new.
*Commonly wrong:* calling a Slack message a notification. A Slack message is a NotificationDelivery;
the Notification is the decision that the message should exist.

**NotificationDelivery** — one materialisation of one Notification on one Channel, owning the retry
state, the rendered payload, the provider message id and the `thread_seq` that fixes its place in
order.
*Keyed by* `(notification_id, channel_id)`; `thread_seq` is allocated `next_seq++` inside the
creating transaction, so sequence order *is* the causal order of domain events.
*Commonly wrong:* assuming a failed delivery means a failed notification. Fifteen channels can
succeed and one go dead; the Notification is a fact and stays one.

**NotificationTemplate** — one whole notification message an operator wrote: a document you read top
to bottom, not a set of overrides (ADR 0050, "A notification template is one whole message";
migrations 00078, 00079).
*Keyed by* a uuid, scoped to an org, selected by `notification_policies.template_id`. Three formats:

| Format | What it is | Portable |
|---|---|---|
| `card` | Markdown plus `:::fields` and `{{ actions }}`, parsed to oto's document IR, compiled per provider | yes |
| `text` | one flat string | yes |
| `raw` | literal Slack Block Kit JSON | no — Slack only |

It carries **no `when` clause**: the policy naming it already has the matchers, so a condition here
would be a second router. Two properties are load-bearing. Every interpolated value in `card` is
markdown-escaped unconditionally, with no opt-out, so a label cannot become syntax; and every
failure path in the renderer returns "no" and nothing else, so the caller builds oto's built-in card
and the alert goes out anyway. **A template cannot kill a delivery.**
*Commonly wrong:* expecting to override the colour. Colour encodes state and is never the author's; a
template that could paint a firing card green could lie about the one fact the eye reads first.

**Conversation** — what a thread is *about*, as the pair `(conversation_kind, conversation_id)`. Two
kinds: `case` (id = an `alert_cases.id`) and `digest` (id = a `notification_policies.id`).
*Keyed by* that pair. It decides which facts share a message and holds exactly **one** Case — a new
Case always means a new thread — while a digest's conversation is keyed by the policy that asked for
it, one per policy per channel (ADR 0045, "A case is a conversation and a thread per alert is
accepted"; migrations 00064, 00069).
*Commonly wrong:* reading it as a correlation or an incident. Correlation is deferred and would need
a stated algorithm; incidents are permanently out of scope.

**ChannelThread** — the binding of a Conversation to a physical place: `(slack_channel_id, root_ts)`.
*Keyed by* `(conversation_kind, conversation_id, channel_id)`. Carries `next_seq` (the allocator),
`last_sent_seq` (the ordering gate), `reply_count`, and a state that may be terminal with a
`dead_reason`. `root_ts` is a **string**, never a float — rounding silently breaks threading, invisibly,
until a reply lands in the wrong place. At 30 replies the next root-touching delivery starts a fresh
card.
*Commonly wrong:* assuming a dead thread means a dead channel. The thread *pointer* is dead; a fresh
root recovers it.

**Digest** — a policy's windowed summary: over the last N seconds, this many Cases opened matching
me, sent in *addition* to whatever else the policy routes.
*Keyed by* a Conversation of kind `digest` with the policy's id (migrations 00058, 00070).
*Commonly wrong:* reading it as a throttle or a damper. A `throttle` records that it withheld a fact;
a digest withholds none. oto does not decide to be quiet about a firing.

### 2.4 Human action

Every human action is a timestamped annotation on a signal's timeline, carrying an actor as
*metadata*. **Actor, never subject.**

**Acknowledge / unacknowledge** — "somebody has seen this." Writes `ack_state` on the Alert, appends
`case.acknowledged` or `case.unacknowledged`, and mints an `acked` / `unacked` Notification that
updates the root card in place. `ack_state` is the **only** state axis a human may write; there is
no endpoint to resolve, close, merge, dismiss or reopen. An ack does not stop the alert firing and
does not carry over to the next Case.

**Snooze** — "be quiet about this Alert until T." Suppresses oto's own notifications for one
`alert_key` until a fixed time. Stored only in oto (`alert_snoozes`), attributed, visible, and
auto-expiring: minimum 5 minutes, maximum 30 days, **never indefinite**.
*Keyed by* `(org_id, alert_key)` for the active one; it records itself as
`notifications.suppressed_reason = 'snoozed'`.
*Commonly wrong:* three things. It is not a `suppression_reason` — that column mirrors Alertmanager's
four reasons and nothing else — and it is not a `state`. A snoozed alert is still firing and **must
still be rendered as firing**; colouring it calm would be a lie. A snoozed *and* silenced alert
shows both facts, and snooze touches nothing in your cluster.

**Comment** — a human speaking into the timeline. Appends `comment.added`, mints a `comment`
Notification, lands as a thread reply.

**Delivery Drill** — a synthetic, marked, end-to-end delivery test: oto injects a fake alert, drives
it through all four River jobs — `ingest.process_batch`, `enrich.run`, `notify.evaluate`,
`deliver.dispatch` — and reports what arrived (ADR 0027, "Delivery drills and the synthetic mark").
Its rows carry a `synthetic` mark that must propagate all the way down, and a drill that cannot find
its own mark on the far end fails. The 90-second limit is a *deadline*, not a timeout: nothing is
cancelled when it passes, only what the drill will claim.

One noun in the source is internal only. A **Stanza** is a named, ordered unit of a rendered message
(`title`, `body`, `fields`, `members`, `trail`, `rule`, `actions`, `footer`) — how the Slack
renderer talks about the blocks Go builds. Nothing an operator writes names one; it named an
override slot under ADR 0037, and ADR 0050 withdrew that.

---

## 3. The pipeline

```
                    Prometheus evaluates an alerting rule; `for` elapses
                                          ▼
                    Alertmanager: group, inhibit, MuteStage, notify
                                          │
                    MuteStage DROPS suppressed alerts HERE, before the
                    webhook — so a webhook arrival is positive proof of
                    non-suppression, and only the reconciler can ever
                    witness suppression BEGIN.
                                          ▼
        POST /api/v1/ingest/alertmanager/{source_id}
        Credential: that source's own ingest token — no session cookie,
        no PAT. Body cap 8 MiB, backpressure applied BEFORE the body is
        read; 202 on durable persist or duplicate, 503 on anything
        transient, never a 4xx for anything retryable.
                                          ▼
        ┌───────────────────────────────────────────────────────────┐
        │  ONE INGEST TRANSACTION — it does exactly two things:     │
        │    1. record the raw batch          → ingest_batches      │
        │    2. enqueue ingest.process_batch  (transactional outbox)│
        │  Slack, Prometheus and enrichment are NEVER on this path. │
        └───────────────────────────────────────────────────────────┘
                                          ▼
        River job: ingest.process_batch — normalise to Observations
                                          │
                     ┌────────────────────┴───────────────────┐
                     ▼                                        ▼
        IDENTITY / DEDUP                             per-alert rejections
        alert_key = SHA-256(org_id, cluster_key,     recorded, rest of the
          canonical labels − ignore_labels)          batch processed, 202
        UNIQUE (org_id, alert_key) decides;
        never a read-then-write check
                     ▼
        ALERTCASE STATE MACHINE
          an open Case for this Alert?  ── yes ──▶ same Case, append event
                     │ no
                     ▼
          open AlertCase (alert_id, seq+1), state = open,
          ack_state = unacked                      ── ALWAYS unacked
                     │
                     ├──▶ append AlertEvent case.opened      (immutable)
                     ├──▶ enqueue rules fetch → RuleSnapshot (content-addressed)
                     │      differs from the previous Case's? → rule_changed
                     ├──▶ enqueue enrich.run → Enrichment ×N (budgeted)
                     └──▶ enqueue notify.evaluate
                                          ▼
        ┌──────────────────────── notify.evaluate ─────────────────────┐
        │  POLICY EVALUATION — priority-ordered, LOWER FIRST, first     │
        │  match wins. Matchers see labels only.                        │
        │       ├── no match ──▶ nothing is sent, and that is recorded  │
        │       ▼ match                                                 │
        │  Reason, from the closed 15-value vocabulary                  │
        │       ├── snoozed / throttled / under the count condition?    │
        │       │   ──▶ notifications.suppressed_reason — still a ROW   │
        │       ▼                                                       │
        │  MINT ONE Notification, idempotent on (subject, reason,       │
        │  salient state); a redelivered job re-derives the same key    │
        │  and writes nothing.                                          │
        │       ▼                                                       │
        │  CONVERSATION RESOLUTION — (kind=case, id=this AlertCase).    │
        │  Exactly one Case per conversation, so a new Case always      │
        │  means a new thread.                                          │
        │       ▼                                                       │
        │  FAN-OUT: one NotificationDelivery per Channel the policy     │
        │  named — 1..16 — each taking next_seq++ inside THIS tx, so    │
        │  thread_seq order is the causal order of domain events.       │
        └──────────────────────────────────────────────────────────────┘
                                          ▼
        River job: deliver.dispatch, one per NotificationDelivery
                                          ▼
        ╔═════════ THREAD-ORDER GATE — TERMINAL STATES FIRST ═════════╗
        ║  thread dead?         ─▶ recover: start a fresh root        ║
        ║  unknown state?       ─▶ abandon                            ║
        ║  seq not allocated?   ─▶ abandon (unsequenced)              ║
        ║  already resolved?    ─▶ out of order, drop quietly         ║
        ║  root not landed?     ─▶ wait for root (recover if it can   ║
        ║                          never land)                        ║
        ║  predecessor unsent?  ─▶ wait for predecessor (recover if   ║
        ║                          the head of line has stalled)      ║
        ║  otherwise            ─▶ PROCEED, in order                  ║
        ╚═════════════════════════════════════════════════════════════╝
                                          ▼
        RENDER — a pure function of (NotificationView, RenderOptions).
        The policy's NotificationTemplate is resolved at claim time and
        passed in; EVERY template failure path falls back to oto's
        built-in card, so a template can never kill a delivery.
                                          │
                     ┌────────────────────┴───────────────────┐
                     ▼                                        ▼
        ROOT CARD: posted once, then                 THREAD REPLY: gated by
        UPDATED IN PLACE thereafter.                 channels.verbosity —
        NEVER gated by verbosity: a card             all | status_changes |
        that stopped updating would be a             firing_and_resolved |
        lie. Ceiling 30 replies, then a              firing_only
        fresh root.                                           │
                     └────────────────────┬───────────────────┘
                                          ▼
                    A human sees the card and acts:
                    Acknowledge · Snooze · Comment
                                          ▼
                    append another AlertEvent (case.acknowledged /
                    alert.snoozed / comment.added)
                                          ▼
                    MINT THE NEXT Notification (acked / snoozed /
                    comment) — same Conversation, same ChannelThread,
                    next thread_seq
                                          ▼
                    THE SAME ROOT CARD IS UPDATED IN PLACE
                    └──────── and the loop closes here ────────┘

In parallel, off the same transactions, never blocking a delivery:

        every state change ──▶ INSERT INTO ui_events (durable log)
                                          ▼
                    Postgres LISTEN / NOTIFY bridge, plus a 2 s
                    reconciling poll — a NOTIFY can be lost
                                          ▼
                    SSE hub with Last-Event-ID resume (ADR 0010)
                                          ▼
                    the UI updates live; a browser that was asleep
                    replays from exactly where it stopped
```

---

## 4. State machines

### Alert

```
                  first sight
                       │
                       ▼
   ┌──────────────▶ firing ◀─────────────┐
   │              │   │  ▲               │
   │              │   │  └── unsuppressed (ingest OR reconciler)
   │              │   └────▶ suppressed  (reconciler ONLY)
   │              │
   │              ├── upstream status="resolved" ──▶ resolved
   │              └── oto stopped hearing about it ▶ expired
   │                                                   │
   └───────────────── it fires again ──────────────────┘
                     (a NEW AlertCase, seq+1)
```

- **`expired` is not `resolved`.** `resolved` requires an explicit upstream `status="resolved"`;
  `expired` means oto stopped hearing about it. Never fabricate a resolution — a fabricated one is a
  lie in the permanent record, which is the only asset the product has.
- **`suppressed` can only be *entered* by the reconciler**, because MuteStage drops muted alerts
  before the webhook fires. Either the reconciler *or* ingest can leave it, since a webhook arrival is
  positive proof of non-suppression. The asymmetry is deliberate.
- **Losing sight of an alert is not it resolving.** The reaper writing `expired` is blocked while the
  source's health is not `healthy`.

### AlertCase

```
     open ──────────────▶ closed         (once, and only this way)
       │                    │
       │                    ├─ resolve_reason = 'upstream' → reads as resolved
       │                    └─ resolve_reason = 'timeout'  → reads as expired
       │
       └─ suppression_reason set / cleared while open
          (mirrors Alertmanager's four reasons; NOT snooze)

                   NOTE: there is no closed → open edge
                   a re-fire opens (alert_id, seq+1), unacked
```

Say **an episode is open or closed**, and **an alert is firing, suppressed, resolved or expired**.
Swapping the two vocabularies is how `alert_cases.state` came to hold four values before migration
00054 narrowed it to two.

### The two orthogonal axes

```
      state:     firing ── suppressed ── resolved ── expired
                    │
   ack_state:    unacked ⇄ acked        ← the only axis a human may write
                    │
     snooze:     none ⇄ snoozed until T ← 5 min .. 30 days, never indefinite
```

An acked alert is still firing. A snoozed alert is still firing, and is rendered as firing. Neither
axis touches `state`, which no human ever writes.

---

## 5. Two worked examples

Acme Corp runs `checkout-api` in cluster `prod-us-east-1`. The rule `CheckoutAPIHighErrorRate`
carries `severity="critical"`.

### 5.1 A normal fire, notify, ack, resolve

1. Prometheus evaluates the rule; `for: 5m` elapses; the alert reaches Alertmanager.
2. No silence, no inhibition: it passes MuteStage and POSTs to the ingest endpoint with that
   **AlertSource**'s ingest token, and one transaction records the raw batch and enqueues
   `ingest.process_batch`. 202 returned.
3. The job computes the **alert_key**. No **Alert** exists, so one is created, state `firing`.
4. No open **AlertCase** exists, so `(alert_id, seq=1)` opens `unacked`; `case.opened` is appended.
5. A **RuleSnapshot** is stored by content hash. No previous Case, so no `rule_changed`.
6. `notify.evaluate` walks the **NotificationPolicies** lowest priority first; `critical →
   #sre-alerts` matches and handles the **Reason** `fired`.
7. One **Notification** is minted, idempotent on `(this Case, fired, firing)`.
8. **Conversation** `(case, this case id)` has no thread yet; one **NotificationDelivery** is created
   for `#sre-alerts` at `thread_seq = 1`.
9. The gate sees no root and this delivery *is* the root, so it proceeds; the **root card** lands and
    its `root_ts` is stored as a string on the **ChannelThread**.
10. An SRE clicks **Acknowledge**: `case.acknowledged` is appended, `ack_state` becomes `acked`.
11. A second Notification (`acked`) takes `thread_seq = 2`; the **root card is updated in place** and
    now names who acknowledged. A thread reply lands only if `verbosity` allows it.
12. The bad deploy is rolled back; Alertmanager sends `status="resolved"`.
13. The Case closes with `resolve_reason = 'upstream'`, the Alert reads `resolved`, and an
    `all_resolved` Notification updates the same root card. **Firing duration** is the Case's span.
14. Nothing is deleted. The Alert, its Case, its timeline and its RuleSnapshot are there next quarter.

### 5.2 A re-fire, with the rule changed in between

1. Three weeks later somebody widens the rule: threshold 5% → 2%, `for` 5m → 2m. Nobody writes it down.
2. `checkout-api` degrades; Prometheus fires; the same ingest endpoint receives the batch.
3. The **alert_key** is unchanged — same labels, same Cluster — so the same **Alert** is found,
   currently reading `resolved`.
4. Case `seq=1` is **closed**, and a closed Case never reopens: **AlertCase `(alert_id, seq=2)`** opens
   `unacked`. The ack from three weeks ago was a receipt for the first firing and does not carry over.
5. The new **RuleSnapshot**'s content hash **differs** from `seq=1`'s, so `rule.definition_changed` is
   appended alongside `rule.snapshot_captured`.
6. `notify.evaluate` mints two Notifications: `fired`, and **`rule_changed`** for the drift.
7. **Conversation** `(case, seq-2's case id)` is a *new* Conversation, because a Conversation holds
   exactly one Case — so a new ChannelThread and a new root card appear in `#sre-alerts`, and the
   three-week-old thread is untouched and still readable.
8. The `rule_changed` delivery takes `thread_seq = 2`, waits for the root, then proceeds.
9. The card names the drift: threshold 5% → 2%, `for` 5m → 2m, with the snapshot's provenance.
10. The SRE on duty stops debugging the service. It did not get worse; the rule got louder.
11. The Alert's timeline shows both episodes end to end: `seq=1`'s fire, ack and resolution, three
    weeks of nothing, then `seq=2`'s fire with the rule diff attached.
12. Both RuleSnapshots are retained, content-addressed, and diffable against each other.
13. Nobody had to find the commit or trust anybody's recollection of the old threshold. That is the
    whole product; everything else in this document exists to make steps 5 and 9 possible.

---

## 6. The boundary

oto deliberately does not do the following, and will not: incident management (no incident object,
no status page, no postmortem); on-call and paging (oto does not know who is on call and must never
learn); assignment and ownership (no signal row has a person-reference column — `acked_by` is
past-tense attribution and the only exception); unprompted reminders *at all* (the single reminder
stage that once existed was withdrawn, and it was the last one — there is no first stage for a
second to follow); human writes to a signal's `state` (no resolve, close, merge, dismiss or reopen
endpoint; `ack_state` is the only state axis a human may write); any write path into your cluster;
and measuring people (firing duration measures the signal — `MTTR`, `MTTA` and `SLA` measure humans
and are banned words).

This is enforced mechanically, not by convention. Two mechanisms do the work.

**The vocabulary linter.** `tools/lintvocab` walks `internal/`, `web/src/` and `db/migrations/` and
fails the build on **19 rules** — 13 banned concept words (`escalation`, `on-call`, `rota`,
`assignee`, `owner`, `responder`, `triage`, `postmortem`, `war room`, `SLA`, `MTTA`, `MTTR`,
`occurrence`) and 6 forbidden column names (`assigned_to`, `owner_id`, `watchers`, `incident_id`,
`ticket_id`, `sla_due_at`). It matches stems, not exact spellings, because `escalate_after_seconds`
is the same violation as `escalation` and that is precisely how the word survived the first pass. It
runs in `just lint-vocabulary`, in `just ci` and in GitHub Actions, and its baseline can only
shrink. This is acceptance criterion AC-49 of `docs/design/SCOPE-BOUNDARY.md`.

**The policy struct.** `notification_policies` has `ChannelIDs []uuid.UUID` and no other target
field. "Route to a person" is not something an operator can express, a client can request or a
migration can add — it requires editing the struct definition and defending that in an ADR arguing
against the Flight Recorder Test by name.

That test is one sentence. Complete this about the row your feature writes: *"This row is a fact
about ______."* A **signal** — Alert, AlertCase, RuleSnapshot, Notification, Delivery, AlertSource,
or an aggregate over those — is in. A **person, team, rota, responsibility, response effort, ticket
or customer-facing statement** is out. Human actions are in **only** as timestamped annotations
carrying an actor as metadata: `case.acked_by = alice` is a fact about the case, *it was
acknowledged, by whom*, while `case.assigned_to = alice` would be a fact about Alice, *she owes
work*. Identical columns; opposite products.

Refusals on that test are permanent; refusals made on *safety* grounds — the read-only silence
mirror — are a separate axis and the only ones that can be earned later. The full argument is ADR
0013, "Alert-first scope boundary", clause by clause in
[`docs/design/SCOPE-BOUNDARY.md`](design/SCOPE-BOUNDARY.md).

---

## 7. Retired vocabulary

These words named real things in earlier versions of the design. They are gone, and **none of them
may be resurrected.** Each row says what replaced it and where it went, so that meeting the old
spelling in a comment, a migration or an ADR does not read as a live concept.

| Retired | Replaced by | Where it was retired |
|---|---|---|
| **AlertGroup** | nothing — the **Conversation** is the **Case** | migration 00069: `alert_groups` dropped, `alert_cases.group_id` with it, module `grouping` (20 files, 5 243 LOC) deleted. ADRs 0005, 0038, 0039 superseded by ADR 0045. |
| **Wording** (per-Stanza override) | **NotificationTemplate** — one whole message | ADR 0037 → ADR 0050; migrations 00078, 00079 |
| **unacked_reminder** | nothing | withdrawn outright; the Reason value and the sweep are deleted, not retired |
| **escalation** | **nothing — there is no replacement term** | ADR 0013; banned by `tools/lintvocab` with no substitute offered, which is the point: oto has no concept in that shape |
| **refire_grace** | nothing — a re-fire always opens the next `seq` | ADR 0040; migration 00071 deleted the setting, its bounds, its derived default and both API surfaces |
| **storm damping**, Reason `storm`, `group.storm_*` events | nothing | ADR 0042; migration 00060 |
| thread state **frozen** | threads are live, or terminal with a `dead_reason` | migration 00066 |
| **AlertOccurrence** | **AlertCase** | ADR 0036; `occurrence` is a banned stem, deliberately unbounded so `alert_occurrences` and `total_occurrences` also fail |
| Reasons `new_alerts`, `some_resolved` | nothing — a conversation holds one Case | migration 00069; arithmetic over a set of one has no answer to give |

`AlertGroup` needs one extra note, because it is the noun that tries hardest to come back. A value
still spelling `alert_group` in a `conversation_kind` or `subject_kind` field is **rejected, not
canonicalised**: the decoder is total over the two kinds that exist and returns the empty value for
anything else, which the CHECK constraint refuses at the write. Do not add a canonicalising arm —
mapping the old spelling onto `case` would widen the kind to mean "generation or case", and a
generation could hold many Cases where a Conversation may not.

**Deferred, post-v1** — not retired, simply not built, and not to be stubbed beyond the ports that
already exist: **correlation** (once called "incidents"), **k8scontext**, **changefeed**, **views**,
**audit** (configuration changes only), **authz**, extra channel providers, anything AI-shaped.

**Permanently out** — there is no version of oto containing incidents, on-call, assignment,
multi-stage escalation, paging, status pages, postmortems, SLA/MTTA measurement, manual
resolve/merge/close, or watchers. `docs/design/SPEC.md` §I.1.1 names the tool to use instead for
each; adding any of them needs an ADR arguing against the Flight Recorder Test by name.

---

## 8. Where to read next

| Document | What it is for |
|---|---|
| [`CONTEXT.md`](../CONTEXT.md) | The short orientation for implementers: the domain-language table, the module map, the four doors that must stay shut. Read it before touching code. |
| [`docs/design/SPEC.md`](design/SPEC.md) | The binding specification. Large — navigate to the section for your module rather than reading it through. |
| [`docs/design/SCOPE-BOUNDARY.md`](design/SCOPE-BOUNDARY.md) | Every scope question, argued clause by clause, with the verdict and the reasoning. Where AC-49's banned-word list is defined. |
| `docs/adr/` | Fifty architecture decision records. The ones this document leans on most: [0013](adr/0013-alert-first-scope-boundary.md) (scope boundary), 0040 (a case never reopens), 0045 (a case is a conversation), 0050 (a template is one whole message), 0009 (rule snapshots at fire time), 0023 (terminal states first), 0010 (SSE with durable resume). |
| `docs/setup/` | Operator-facing setup: [`slack.md`](setup/slack.md) for the Slack app, [`tuning.md`](setup/tuning.md) for the shipped defaults and what to change. |
| [`docs/runbooks/README.md`](runbooks/README.md) | One runbook per exported metric, keyed by metric name. |
