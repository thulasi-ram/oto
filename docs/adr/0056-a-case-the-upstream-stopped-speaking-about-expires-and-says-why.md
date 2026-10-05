# 0056 — A Case the upstream stopped speaking about expires, and says why

**Status:** Accepted · 2026-10-05 — the owner's ruling, after asking for a manual "End case" button
and being shown SCOPE-BOUNDARY verdict #34.
**Holds:** CONTEXT.md's door *"No human writes a signal's `state`"* and verdict #34 (no manual
resolve), unchanged. Nothing here gives a person a way to end a Case.
**Extends:** the reaper (SPEC §B.4) and `alert_cases.resolve_reason`.
**Relates to:** [0006](0006-reconciler-is-mandatory.md) (the reconciler, and its "warn loudly" on
`send_resolved: false`), [0040](0040-a-case-is-open-or-closed-and-never-reopened.md),
[0044](0044-a-count-condition-is-a-silence-the-operator-asked-for.md) (the authority test §3 passes).
**Amended:** 2026-10-05, same day — [Amendment 1](#amendment-1--2026-10-05-ha-clusters-existing-sources-and-the-release-flag)
(HA clusters expire on all of their sources; existing sources start with the expiry off; the
deletion waits a resolve grace; `cluster_id` is immutable; the two new expiries ship behind a flag).

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

Each source carries an operator-written `max_silence_s` (default **24h** for a new source, editable;
NULL turns it off; an existing source starts off — Amendment 1). An open Case under a **healthy** source with no upstream observation for longer than that
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

## Amendment 1 — 2026-10-05: HA clusters, existing sources and the release flag

The owner's rulings R1–R3 and the judged fixes to the reaper, landed in `a62ac3f`. Each one narrows
§2 or §3; none gives a person a way to end a Case.

### R1 — an HA cluster's Cases expire, on all of its sources

§1–§3 were written as if a cluster had one source, and the reaper acted only under **exactly one**
live source, so every Alertmanager HA pair's Cases were held forever. Now `timeout` and `silent`
may end a Case only when its cluster has **at least one live source and every live source is
healthy** (SPEC §B.4). An HA pair is two witnesses: the replica oto cannot see might be the one still
carrying the alert, so one unhealthy replica holds every open Case on the cluster, and a held Case
names only the replicas the guard could not vouch for, never a healthy sibling.

`silent`'s threshold is the cluster's **effective** max silence: the **longest** `max_silence_s`
among its live sources, or **off** when any live source's is NULL. Off on one replica is off for the
cluster, because an operator who turned it off for a source did not consent to its sibling's number
ending that source's alerts. Every expiry, `timeout` included, re-reads the cluster's live set inside
its transaction and stands down unless it equals the set the guard proved.

### R2 — existing sources start with max silence off

Migration `00094` adds `max_silence_s` **bare** and only then sets its default to 86400. Every source
that exists when it runs reads NULL (off); a source registered afterwards takes a day. This is what
keeps ADR 0044 §3's line true for this threshold: the reaper acts only on **a number an operator
wrote**. A day is *offered* — the default on a new source, which an operator registering it can read
back and change, and the value the settings field shows when someone turns it on for an existing one
— and **nothing is backfilled**, so no Case that already exists is swept away on the first tick by a
threshold nobody chose for its source. The bounds are **1 hour** (below Alertmanager's own default
`repeat_interval`, so a mistyped minute count is refused rather than expiring every live Case) to
**30 days**.

### B5 — `source_removed` waits a resolve grace after the deletion

§2's Cases expire once no live source feeds the cluster **and** the newest removal is a
`resolve_grace` old, checked in the scan and again in the expiring transaction. A source registered
on the cluster inside that grace — the usual shape of "delete it and re-add it with the right URL" —
stands the expiry down. Deleting one replica of an HA pair ends nothing: the other still speaks for
every Case they shared.

### R3 — `cluster_id` is immutable

A source's cluster is what the reaper's live-set test is about, so moving a source between clusters
would silently make one cluster's Cases `source_removed`-eligible and hand another a witness it never
had. `PATCH /api/v1/sources/{id}` refuses `cluster_id` by name, as it refuses `kind`; re-home a
source by registering a new one and deleting the old.

### B2 — the two new expiries ship behind a flag, off

`silent` and `source_removed` run only when `jobs.expire_silent_and_removed`
(`OTO_JOBS_EXPIRE_SILENT_AND_REMOVED`) is `true`; it defaults to **`false`** in this release. Off,
`case.reap` is exactly the `timeout` sweep it was before `00094`. `timeout` is not behind the flag.
The rollout is two-phase, expand/contract (CONTEXT.md §6):

1. **Ship the release.** Migration `00094` and every reader that can spell `silent` and
   `source_removed` (API, UI, channel cards, webhook consumers) go out together, with the writer
   dark. Nothing produces either reason yet.
2. **Turn the flag on** once every replica runs this release and webhook consumers have been told
   the `resolve_reason` set widened (docs/setup/webhook.md). A later release may flip the default.

⚠️ **Once the flag has been on, rolling back below this release needs `goose down 00094` first.**
The older release cannot spell either reason; `00094`'s Down rewrites them to `timeout` (still
expired) before narrowing the CHECK. Rolling back with the flag never turned on needs nothing: no
row carries either reason.

The UI cannot see the flag — the API does not expose it — so every `silent` and `source_removed`
forecast on screen is worded "if enabled" rather than promising an expiry the reaper may not run.
