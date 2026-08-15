# 0006 — The Alertmanager API v2 reconciler is mandatory, not optional

**Status:** Accepted · 2026-08-07 · **Amended 2026-08-08**, and **amended again
2026-08-09, which SUPERSEDES the first amendment.** Both are kept below, in order.
Read the second one before acting on the first.

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
   Divergence is the canary for every correctness bug in the system and belongs on oto's own
   dashboard. ⛔ This paragraph named a metric `oto_reconcile_divergence` that was never built
   (5bc341a); the count is durable in `source_health.divergence_count`, served by
   `GET /api/v1/sources/{id}/health`, summed by `GET /api/v1/stats/*` and logged by
   `internal/sources/service/reconcile.go`. A dashboard is built from those, not from a series.
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

> **⛔ SUPERSEDED on 2026-08-09.** Its central factual claim — that removing the
> flag would freeze the reaper for firewalled deployments *and that keeping the
> flag saves them from that* — is false. The second half was never checked. It is
> preserved verbatim below because the decision it made is the one the code
> carried for a day, and because the second amendment is only legible next to it.


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

## Amendment — 2026-08-09: the flag is gone, and why the last amendment was wrong

The 2026-08-08 amendment kept `alert_sources.reconcile_enabled` and promised the
switch would be a "documented, surfaced degradation — never a silent one". An
audit of what the flag DOES, rather than what it is for, found both halves
untrue. The flag is removed: migration `00038_reconciler_is_not_optional.sql`
drops the column, and the field is gone from `SourceDTO`, `CreateSourceRequest`
and `UpdateSourceRequest`.

**1. The flag did not do the thing it was kept for.** The last amendment's
argument was that a source whose Alertmanager API is unreachable outbound — a
real deployment, and one this ADR's own Consequences section warns about — would,
without the flag, fail every pass, be marked `unreachable` after three, and have
its expiry frozen forever; so the flag was "strictly better than that". The
second half is false. `source_health.status` has exactly two writers, `Probe`
(the manual `POST /sources/{id}/test`) and the reconcile pass itself; `TouchPush`
deliberately does not move it, because a webhook arriving proves the source can
reach oto and proves nothing about oto reaching the source. A source oto cannot
reach therefore never earns a `healthy` row at all: it is seeded `unknown` by
`SourceRepository.Create` and stays there. `unknown` blocks the reaper exactly as
`unreachable` does. **The firewalled deployment's expiry was frozen with the flag
and without it.** The flag bought it nothing but the absence of an explanation.

**2. What the flag did do, it did to the reachable sources.** Turn it off on a
source oto *can* see and the health projection FREEZES at its last verdict —
`healthy` — because nothing else ever writes that column. The §B.4 reaper guard
then goes on answering "yes, oto can see this source" indefinitely on the
strength of a probe that may be weeks old, while `MuteStage` drops every silenced
alert before the webhook fires. `source_ends_at` stops advancing, `resolve_grace`
elapses, and `occurrence.reap` ends the episode as `expired` /
`resolve_reason='timeout'`.

That is not the fabricated `resolved` the issue report described — oto's state
machine cannot produce one here, and `sweep.go`'s assertion refuses to — but it
is an ENDING RECORDED FOR AN ALERT THAT DID NOT END, timestamped, on the
append-only timeline, for an alert a colleague silenced and which is still
firing. §B.4 exists to stop precisely this, and the flag walked around it by
making "oto decided not to look" indistinguishable from "oto looked and all was
well".

**3. "Never silent" was not implemented.** Of the three enforcements the last
amendment listed: the default was right; the OpenAPI description was right; and
the warning was not. `withReconcileWarning` was called from
`sources/service.Health` alone. `GET /api/v1/sources` builds its rows in
`api.decorate`, straight off `SourceRepository.HealthFor`, and never passes
through the service method — so the "standing warning on every row" the amendment
claimed did not exist. Nor did "the UI renders warnings already": no source
screen in `web/` reads `health.warnings` at all, and there is no toggle for this
flag anywhere in the product. The only way to set it was a hand-written API call,
and the only way to discover it had been set was to make another one.

So the choice was never "surfaced degradation versus removal". It was "silent
degradation versus removal".

**The decision, restored to what this ADR said in the first place:**

> `source.reconcile` is a core component and the only producer of `suppressed`.
> It runs for EVERY live source, on that source's `reconcile_interval_s`. There
> is no per-source opt-out, and a source that must be polled gently is polled
> gently — the interval spans 10 s to 1 h — not not at all.

**What replaces the flag for the deployment that needs it.** A source oto cannot
reach becomes `unreachable`, which blocks expiry for its alerts. That is the same
outcome the flag gave them, arrived at honestly: the health badge says oto cannot
see this Alertmanager, the settings screen says nothing from it will be expired
until that changes, and the operator can act on a fact instead of inheriting a
setting. The costs are one failing HTTP call per interval and a log line, and the
interval is the knob for both.

**Enforced by, as of this amendment:** the column is dropped, so `ListDue` has no
gate to carry and the fan-out schedules every live source; the request bodies
decode with `DisallowUnknownFields` and `additionalProperties: false`, so
`PATCH {"reconcile_enabled": false}` now fails validation NAMING THE FIELD rather
than being ignored — a runbook that still carries it finds out; and
`internal/sources/api/reconcile_not_optional_test.go` asserts that refusal, the
create-side refusal, and that `reconcile_interval_seconds` still tunes.

**Alternative rejected:** keep the flag and make the degradation real — degrade
`source_health.status` while it is off, and travel that untrustworthiness out to
every alert from the source. It is coherent, and it collapses on inspection:
degrading the status blocks the reaper for the lifetime of the source, so the
flag would no longer preserve expiry for anybody, which is the only behaviour it
had. What would remain is a switch whose entire effect is "stop making the HTTP
call, and lose the ability to see a silence" — which is a slower way of spelling
`reconcile_interval_s: 3600`.
