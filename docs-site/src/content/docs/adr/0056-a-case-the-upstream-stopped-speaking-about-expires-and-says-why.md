---
title: 0056 — A Case the upstream stopped speaking about expires, and says why
---
**Status:** Accepted · 2026-10-05 — the owner's ruling, after asking for a manual "End case" button
and being shown SCOPE-BOUNDARY verdict #34.
**Holds:** CONTEXT.md's door *"No human writes a signal's `state`"* and verdict #34 (no manual
resolve), unchanged. Nothing here gives a person a way to end a Case.
**Extends:** the reaper (SPEC §B.4) and `alert_cases.resolve_reason`.
**Relates to:** [0006](/oto/adr/0006-reconciler-is-mandatory/) (the reconciler, and its "warn loudly" on
`send_resolved: false`), [0040](/oto/adr/0040-a-case-is-open-or-closed-and-never-reopened/),
[0044](/oto/adr/0044-a-count-condition-is-a-silence-the-operator-asked-for/) (the authority test §3 passes).

## Context

The owner wanted to end stale Cases by hand. Investigation found why they are stale. A Case ends
only by an upstream resolve or by the reaper, and the reaper misses two common shapes:

1. **No upstream end time.** Alertmanager zeroes `endsAt` on firing webhooks; only a reconcile pass
   that sees the alert live sets `source_ends_at`. A Case known only from webhooks is never a reap
   candidate.
2. **A deleted source.** The reaper finds no single live source for the Case and holds it forever.

It also found that nothing shows either: `last_observed_at` and `source_ends_at` are in the API and
rendered nowhere, and `source_health.warnings` (where `send_resolved: false` is recorded) never
reaches the UI.

## Decision

### 1. Staleness is shown

A Case shows how long since upstream last said anything about it, and, when it cannot expire, why:
no upstream end time, or its source is held (not healthy) or gone. A source shows its warnings and
how many Cases it holds. Facts only; no state changes.

### 2. A deleted source's open Cases expire as `source_removed`

Deleting a source is an act an operator took on configuration. Its open Cases close with
`resolve_reason = 'source_removed'`: the one thing that could have told oto they ended is gone, and
oto says exactly that rather than holding them open on a promise nobody can keep.

### 3. A source may set a max silence; past it, open Cases expire as `silent`

Each source carries an operator-written `max_silence_s` (default **24h**, editable; NULL turns it
off). An open Case under a **healthy** source with no upstream observation for longer than that
closes with `resolve_reason = 'silent'`. Alertmanager repeats a firing notification every
`repeat_interval` (4h by default), so a day of silence means upstream stopped speaking about it.
The health guard still applies: under an unhealthy source oto cannot tell "resolved" from
"Alertmanager is down", so it holds, and §1 shows that it is holding.

### 4. Both read as `expired`

The four alert states are unchanged. A Case closed with `timeout`, `silent` or `source_removed` is
**expired**; only `upstream` is **resolved**. The reason is shown wherever the state is, because
"ended because the source went silent for a day" is not the same fact as "resolved".

## Consequences

- `resolve_reason`'s CHECK and its domain enum widen from two values to four.
- An org with long-firing alerts whose Alertmanager `repeat_interval` exceeds 24h must raise
  `max_silence_s`, or those Cases expire while still firing. The source settings say so next to the
  field.
