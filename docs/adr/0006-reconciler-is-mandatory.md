# 0006 — The Alertmanager API v2 reconciler is mandatory, not optional

**Status:** Accepted · 2026-08-07

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
