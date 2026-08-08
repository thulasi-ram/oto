# 0006 — The Alertmanager API v2 reconciler is mandatory, not optional

**Status:** Accepted · 2026-08-07 · **Amended 2026-08-08** (see "Amendment" below)

## Context
Alertmanager's notification pipeline runs `MuteStage` **before** `RetryStage`, and `MuteStage`
*drops* muted alerts from the slice that continues down the pipeline. The consequence is
absolute: **suppressed alerts are never delivered to a webhook, at all.** The webhook
`alerts[].status` enum is only `firing | resolved`; there is no `suppressed` value on the wire.

So from a webhook's perspective, "someone silenced this", "this resolved and stopped arriving",
and "Alertmanager died" are **the same observation: silence**.

An architecture that treats the webhook as the only input therefore cannot render silence state,
and will eventually claim an alert resolved when it was merely silenced — which destroys the
"system of record" claim directly.

## Decision
`source.reconcile` is a **core** component of the `ingestion` module, running every
`reconcile_interval_s` (default 30 s) per source. It is **not** a second ingestion mode: it emits
`Observation`s into the same state machine, and there remains exactly one write path into `alerts`.

It is the **only** producer of the `suppressed` state (transitions T3/T4), reading
`GET /api/v2/alerts?active=true&silenced=true&inhibited=true&unprocessed=true` and taking
`status.state` plus `silencedBy` / `inhibitedBy` / `mutedBy`.

It also owns three other jobs:
1. `GET /api/v2/status` → Alertmanager version, effective `send_resolved` (a receiver with
   `send_resolved: false` means we will never see resolves — warn loudly), and server time for
   clock-skew measurement.
2. **Divergence accounting.** Open in oto but absent upstream → a reap candidate. Present
   upstream but absent in oto → we missed a webhook; recover it and count it.
   `oto_reconcile_divergence` is the canary for every correctness bug in the system and belongs
   on oto's own dashboard.
3. **Source health**, which gates the reaper (ADR 0007's sibling rule): three consecutive
   failures mark the source `unreachable`, and an unreachable source **blocks expiry**.

## Consequences
- oto can honestly render "silenced by @ram until 14:00, because: maintenance window" — which is
  impossible from webhooks and is a visible product advantage.
- `expired` can be a trustworthy state, because we know the difference between "the alert went
  away" and "we lost the ability to see it".
- Costs one HTTP call per source per 30 seconds and one more failure mode to handle.
- Requires network reachability from oto to Alertmanager's API, not merely the reverse. This is
  called out in the install docs.

## Alternatives rejected
- **Webhook-only:** cannot render suppression, and will lie about resolution. Non-viable.
- **Pull as a first-class ingestion mode:** doubles the correctness surface (two write paths into
  the state machine) for near-zero additional user value.
- **Infer suppression from the absence of notifications:** indistinguishable from a dead
  Alertmanager. This is precisely the failure mode most likely to kill the product.

## Amendment — 2026-08-08: what "mandatory" actually means

A security and conformance review found this ADR and the code disagreeing.
`PATCH /api/v1/sources/{id} {"reconcile_enabled": false}` returns 200, and the
due-list gates on that column (`alert_sources.reconcile_enabled`), so the
component this ADR calls **mandatory** is switchable off per source. The ADR and
the code cannot both be right, so this amendment says which is.

**The flag stays. The ADR's word was too strong.**

"Mandatory" was always about the ARCHITECTURE, not about every row: the decision
being defended is that oto is not webhook-only, that `source.reconcile` is a core
component rather than an optional integration, and that there is exactly one
write path into `alerts`. None of that is weakened by a source that has it turned
off. What the original text got wrong is that it read as a per-source guarantee,
and this ADR's own Consequences section already contradicts that — it names
"network reachability from oto to Alertmanager's API, not merely the reverse" as
a REQUIREMENT. A source behind a one-way network path is a real deployment. With
no flag, such a source produces a failing reconcile every 30 seconds forever,
three consecutive failures mark it `unreachable`, and an unreachable source
**blocks the reaper** — so removing the opt-out would not give those users
reconciliation, it would permanently freeze expiry for them. That is strictly
worse than the honest "off".

So the decision is amended to:

> `source.reconcile` is a core component and the only producer of `suppressed`.
> It runs for every source by default. It may be turned off per source, and doing
> so is a **documented, surfaced degradation** — never a silent one.

**What "never silent" is enforced by**, as of this amendment:

1. `reconcile_enabled` defaults to `true` on create (`sources/api.toDraft`), and
   soft-deleting a source is the only thing that clears it implicitly.
2. A source with it off carries a standing `reconcile_disabled` warning on
   `GET /api/v1/sources/{id}/health` and on every row of `GET /api/v1/sources`
   (`sources/service.withReconcileWarning`). The UI renders warnings already.
3. The OpenAPI description of `reconcile_enabled` states the consequence in the
   words an operator needs: **oto can never observe a silenced or inhibited alert
   for this source, and will show an upstream-muted alert as firing
   indefinitely.**

**What it deliberately does NOT do:** the warning does not change
`source_health.status`. Making a reconcile-disabled source permanently
`degraded` would block the reaper for the lifetime of that source, so nothing
routed through it would ever expire — a correctness rule turned into an unbounded
leak of open alerts. The operator is told; the state machine keeps its semantics.

**Alternative rejected:** remove the flag entirely. It reads cleaner and it is
worse for exactly the users this ADR's Consequences section already warned about
— see the reaper-freeze argument above.
