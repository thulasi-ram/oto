# 0024 — Retention defaults, what each boundary destroys, and cold storage

**Status:** Accepted · 2026-08-09
**Decided WITHOUT the owner.** See *How to overturn this*, below — it is designed to be cheap.
**Relates to:** [0014](0014-postgres-only-no-analytical-store.md) (the scale envelope this is derived
from, and the `DETACH` → Parquet escape hatch), [0003](0003-alert-occurrence-event-separation.md)
(why the projections survive what the event stream does not),
[0001](0001-postgres-sole-datastore-river-job-queue.md)
**Amends:** SPEC §D.1 (`raw_retention_days` default), §D.3, §D.4, §D.11
**Resolves:** git-bug `344bf68` — *"Retention defaults delete the history the product is selling, on
no stated requirement"*, and `docs/ORCHESTRATION.md` open question 5.

## Context

oto shipped two retention defaults — raw payloads 14 days, events 13 months — traceable to no stated
requirement. `docs/ORCHESTRATION.md` listed them as an open question for the owner. The charge in the
issue is serious and specific: oto's pitch is *"for every alert that has ever fired"*, 14 days is not
"ever", deletion is by `DROP PARTITION` and therefore irreversible, and the first buyer who needs a
year of history for an audit discovers it was deleted by a number nobody chose.

The issue also asserts something that turns out not to be true, and establishing that was the whole
job: **retention does not delete the history the product is selling.** The rest of this ADR is the
evidence, then the numbers that follow from it.

### 1. What is actually reaped — the complete list

Four tables age out, and only four. Everything else in the schema is unpartitioned and has no reaper.

| Table | Grain | Window | Dropped by |
|---|---|---|---|
| `ingest_batches` | daily | `raw_retention_days` | `partitions.manage` → `oto_drop_partitions_before` |
| `ingest_rejections` | daily | `raw_retention_days` | same |
| `alert_events` | monthly | `event_retention_months` | same |
| `ui_events` | hourly | 24 h (SSE resume buffer, ADR 0010) | same |

Plus three small side tables pruned by row: `ingest_dedup` (10 min), `sessions` (on expiry),
`enrichment_cache` (on expiry). *(`alert_event_keys` is now a fourth — Amendment 1 — and
`idempotency_claims`, added later, a fifth at 24 hours.)*

**Never reaped, by anything, at any setting:** `alerts`, `alert_occurrences`, `alert_groups`,
`alert_group_members`, `rule_snapshots`, `notifications`, `notification_deliveries`,
`channel_threads`, `enrichments`, `silences`, `alert_quality_daily`, ~~`alert_event_keys`~~
(⚠️ `alert_event_keys` is now pruned by row — see
[Amendment 1](#amendment-1--the-alert_event_keys-pruner-exists-and-the-horizon-is-a-floor)).

### 2. What survives each boundary — the reconstructibility answer

Line up that second list against `README.md`'s own sentence. The promise is that for every alert that
has ever fired, oto shows *when it first appeared, every episode since, what the rule said at that
moment, who was told, on which channel, in which thread, who acknowledged it, and how it ended.*

Every clause of it is served by a table retention never touches:

| The promise | Where it lives | Reaped? |
|---|---|---|
| when it first appeared | `alerts.first_seen_at` | never |
| every episode since | `alert_occurrences` (`seq`, `started_at`, `ended_at`, `reopen_of`) | never |
| what the rule said at that moment | `rule_snapshots` via `alert_occurrences.rule_snapshot_id` | never |
| who was told, on which channel | `notifications` + `notification_deliveries` | never |
| in which thread | `channel_threads` (`channel_id`, `ts`) | never |
| who acknowledged it | `alert_occurrences.acked_by_label`, `acked_at`, `ack_note` | never |
| how it ended | `alert_occurrences.state` + `resolve_reason` | never |

**After the 14-day (now 30-day) raw boundary** you can still answer every one of the above, plus every
label and annotation, the flap score, and the daily hygiene rollups. You can no longer answer: *what
were the exact bytes Alertmanager sent?*, *which elements of that payload were rejected and why?*, and
you can no longer replay a stored batch after a parser fix. The raw tables have **no read API at all**
— no path in `api/openapi/openapi.yaml` touches `ingest_batches` or `ingest_rejections` — so they are
today a `psql`-only debugging artifact, not a product surface.

**After the 13-month event boundary** you keep the whole table above. What you lose is the
*instant-by-instant narrative*: the ordered transitions with an actor on each, the pre-rendered
summaries, `enrichment.completed`, `group.storm_started`, `delivery.*` per attempt, and — the one that
matters — **`comment.added` and the note on `occurrence.unacknowledged`, which live nowhere but
`alert_events` and cannot be rebuilt from anything.** `alerts/api/handlers.go` says it plainly: *"a
comment is an event like any other: it cannot be edited or deleted, because the timeline IS the
record."* When the partition goes, the human writing goes with it. The API surface that empties is
`GET /alerts/{id}/events`, `GET /occurrences/{id}/events`, `GET /alert-groups/{id}/timeline`.

So the honest summary is: **retention deletes the narrative, never the record.** That is a real loss,
and a much narrower one than the issue supposed.

### 3. What it actually costs — measured, not estimated

A defence that rests on "storage" has to say how much. Measured on Postgres 17 with the real DDL and
its real indexes, synthetic rows modelled on the actual event mix and a three-alert Alertmanager v4
webhook body (9 labels, 2 annotations, `generatorURL`, per alert):

- **`alert_events`: 752 bytes/row all-in** (393 B heap + 357 B across its four indexes), at 200 000
  rows with every optional FK populated — an upper bound, since `group_id` and `occurrence_id` are
  NULL on many real rows.
- **`ingest_batches`: 1 698 bytes/row** for a 3-alert batch. The 3 305-byte JSON body compresses to
  1 165 B inline; TOAST never engages. That is **~566 B per alert instance persisted.**

At ~8 events per firing episode and ~3 payload appearances per episode (initial plus repeats):

| Alert firings/day | `alert_events` @ 13 mo | `ingest_batches` @ 14 d | @ **30 d** | @ 90 d | @ 13 mo |
|---|---|---|---|---|---|
| 1 000 | 2.4 GB | 24 MB | **51 MB** | 153 MB | 0.7 GB |
| 10 000 | 24 GB | 238 MB | **510 MB** | 1.5 GB | 6.7 GB |
| 100 000 | 237 GB | 2.3 GB | **5.0 GB** | 15 GB | 66 GB |

**The storage defence of 14 days is worth 27 MB at 1 000 firings a day and 270 MB at 10 000.** It does
not survive contact with a number. ADR 0014 itself calls 100 000 firings a day a broken alerting
configuration rather than a scale target.

The event side reads differently. ADR 0014 puts one org's pessimistic ceiling at 10M events/month and
names 50–100M rows as where a single Postgres starts to hurt. 13 months × 10M = **130M rows ≈ 98 GB**,
which is the top of that band exactly.

## Decision

### `raw_retention_days`: 14 → **30**, and it is derived rather than chosen

The one stated requirement a stored raw payload exists to serve is **SPEC acceptance criterion 36** —
*"replaying a stored `ingest_batch` after a parser fix reproduces the same state without duplicate
Slack messages"*. That replay is idempotent only while the batch's event dedupe keys are still in
`alert_event_keys`, which SPEC §D.4 prunes at **30 days**. Past that horizon a replay appends the
timeline a second time, so a payload kept longer cannot be used for the thing it is kept for.

Thirty days is therefore the exact width of the window in which a raw payload is *useful*, and it is
derived from another number in the system rather than picked — the same relationship
`MinRefireGraceSeconds` has with `ingestion/domain.DedupTTL`. **The two are coupled: if the
`alert_event_keys` pruner moves, this moves with it.** The coupling is stated on
`DefaultRawRetention` so a future change to one cannot silently invalidate the other.

14 days was strictly worse: shorter than the horizon, traceable to nothing, and defended by a storage
figure that turns out to be tens of megabytes.

### `event_retention_months`: **13**, kept, on a different reason than the one written down

13 stays, but the recorded justification was wrong. "Thirteen so year-on-year comparisons work" is the
justification for **`alert_quality_daily`** — and that table is never reaped, so year-on-year works at
any event retention. The real constraint is the one from §3: **13 months is the longest default that
keeps one org inside ADR 0014's own scale envelope** at ADR 0014's own stated ceiling. Longer would
ship every install past the point ADR 0014 says to revisit the datastore.

The existing bound (1–120 months) already lets a compliance buyer configure ten years, and that is the
answer to "we need a year for an audit" — a supported setting, not a code change. Above roughly 13
months at high volume, expect ADR 0014's revisit triggers to fire; that is a documented consequence,
not a surprise.

### The per-org setting now reaches the reaper

This was the load-bearing defect underneath the issue. `partitions.manage` read
`Config.Retention` — a **process-global** value — and never looked at `orgs.settings` at all. The
per-org keys added in `a827f4a` were validated, bounded, rendered with an origin and enforced nowhere.
"It is configurable" was not a mitigation, because it was not true.

A partition holds every tenant's rows, so per-org retention cannot be honoured by dropping one. There
are exactly two honest options, and oto takes the second:

- *shortest wins* — silently deletes data an org configured itself to keep. Rejected outright.
- **longest wins — retention is a floor, never a ceiling.** `partitions.manage` now drops at the
  maximum of the deployment's configured window and every org's effective setting. An org that raises
  its window gets what it asked for; an org that lowers its window may keep rows a while longer, which
  costs disk and destroys nothing. Every failure in that fold — unreadable org, failed list — widens
  the window rather than narrowing it, because reading a setting must never be able to cost data.

For a self-hosted single-org install, which is what oto is, the maximum *is* the org's value.

### Cold-storage export: **SCOPED, not built.** Until it exists, the product says so plainly

Ruling it out is untenable. The one thing retention destroys irreversibly is human-authored writing —
comments and unack notes — and no setting recovers it; a buyer who needs the narrative beyond their
window has nothing to buy. ADR 0014 already designed the hatch (*"a cold partition can be
`DETACH PARTITION`'d and exported ... a bolt-on, not a migration"*). Scoping it is finishing a
sentence ADR 0014 started.

The shape, deliberately minimal — this is a **retention hook, not an analytics pipeline**:

- **Trigger.** `partitions.manage`, before its drop step. When export is enabled, a partition with no
  export receipt is **not dropped** — the job logs and retries next hour. That single rule is what
  turns a silent deletion into a visible, blocking one. Plus an operator-invoked
  `oto export events --from --to` for a one-off pull.
- **Shape.** One file per partition, the partition's rows verbatim, no joins and no reshaping. A
  dropped partition is a self-contained month; keeping it that way is what makes the export trivially
  re-loadable and the receipt trivially checkable.
- **Format.** gzipped **JSONL**, not Parquet. Parquet needs a Go writer dependency and a schema
  registry to stay honest across migrations; JSONL is a streaming row scan with zero new dependencies
  and loads into DuckDB (`read_json_auto`) or ClickHouse in one statement. Parquet stays the upgrade
  ADR 0014 names, reachable later from the same files.
- **Where it lands.** A configured directory, `OTO_RETENTION_EXPORT_DIR` — a mounted volume or PVC.
  **Not S3 in v1:** an object-store client is a dependency and a credential surface, and a directory
  is what a Helm chart can mount and what every existing backup tool already covers.
- **Receipt.** A small `retention_exports` table — `(parent, partition_name, row_count, bytes,
  sha256, path, exported_at)` — which is both the drop gate and the operator's proof the month left
  the building.
- **Default: OFF.** A product that writes files nobody asked for is worse than one that does not.
- **Explicitly out of scope:** restore/import (documented as a DuckDB one-liner against the files),
  S3, Parquet, encryption at rest beyond the volume's own, and any per-org filtering — a partition is
  the unit, and it is multi-tenant by construction.

**What oto tells a buyer who asks today:** *"The alert record — identity, every episode, the rule text
at fire time, who was told, on which channel, in which thread, who acknowledged it and how it ended —
is kept indefinitely and is never deleted by retention. The instant-by-instant timeline is kept for 13
months by default and is configurable up to 10 years. Raw upstream payloads are kept 30 days and are a
debugging artifact, not a product surface. There is no cold-storage export yet: if you need the
timeline beyond your configured window, raise the window. The export is designed and not built."*
That is a true sentence, which is the only kind worth giving a buyer.

## Consequences

- Every install gets 16 more days of raw payloads: tens to hundreds of megabytes at realistic volume.
- The two `orgs.settings` retention keys do something for the first time. On an existing multi-tenant
  deployment where some org set a longer window, the first `partitions.manage` tick after this change
  will stop dropping partitions it previously dropped, and disk will grow to the new maximum.
- No migration is needed for the numbers themselves — they are Go constants and a config default. A
  companion migration corrects the stale `14` in `oto_partitions_manage`'s unused parameter default
  and in three `COMMENT ON TABLE` strings, so `\d+` stops disagreeing with the code.
- Until the export exists, the settings UI and `docs/setup/tuning.md` state what each boundary
  destroys, in operator language, including that comments and unack notes are unrecoverable.
- **Two defects surfaced and are NOT fixed here**, because both are separable and neither changes the
  decision:
  1. ✅ **CLOSED — see [Amendment 1](#amendment-1--the-alert_event_keys-pruner-exists-and-the-horizon-is-a-floor).**
     ~~**Nothing prunes `alert_event_keys`.** SPEC §D.4 and its own table comment promise 30 days;
     `pruneRetention` prunes only `ingest_dedup` and `sessions`. The table grows forever. Today this
     makes the 30-day raw default *conservative* rather than wrong — replay is currently safe
     indefinitely — but the moment the pruner is implemented the coupling above becomes live.~~ **The
     pruner is now implemented, so the coupling is live, and Amendment 1 states what that made the
     horizon: a floor of 30 days that widens to the longest `raw_retention_days` any tenant set.**
  2. **`ingest_rejections` has no read API.** Its table comment calls the per-source rejection feed
     "the whole point of the table" and the SPEC's "we never silently drop" promise rests on it, but
     no route in `openapi.yaml` reads it. A rejection nobody can look at, deleted 30 days later, is a
     silent drop with extra steps.

## How to overturn this

**This was decided without the owner, on the evidence in §1–§3 and nothing else.** No customer was
asked, no deployment was measured, and the alert-volume model (8 events and 3 payload appearances per
firing) is reasoned from the schema rather than observed. If the owner has a real buyer with a real
number, that beats all of it.

The decision is deliberately cheap to reverse, in ascending order of cost:

- **The numbers** are two Go constants (`identity/domain.DefaultRawRetention`,
  `DefaultEventRetention`) and one config default. Changing them changes nothing else.
- **"Longest wins"** is three functions in `internal/app`: `Container.effectiveRetention` (the entry
  point, and the config floor it seeds the fold with), `foldRetention` (the reduce over every tenant's
  settings) and `widenToSettingsCeiling` (where a failed read lands — see
  [Amendment 2](#amendment-2--longest-wins-is-three-functions-and-the-fallbacks-widen-for-real)).
  Deleting the three together restores the previous behaviour exactly.
- **Cold storage** is scoped and unbuilt, so ruling it out costs deleting a section of this ADR and
  amending the buyer answer to say the product does not have one and will not.

What is *not* cheap to reverse is the direction: any decision that shortens a retention window and
ships deletes data that cannot come back. Widening is free; narrowing is not. That asymmetry is why
every fallback in the code widens. *(It did not, when this was written — see
[Amendment 2](#amendment-2--longest-wins-is-three-functions-and-the-fallbacks-widen-for-real).)*

## Alternatives rejected

**Leave 14 days and document it.** Rejected: nothing derives 14, the storage it saves is a rounding
error, and it is shorter than the window in which a raw payload is still replayable — so it fails the
one acceptance criterion the table exists to serve.

**90 days or 13 months of raw payloads.** Affordable (1.5 GB and 6.7 GB at 10 000 firings/day), and
rejected because they buy nothing: past the `alert_event_keys` horizon the payload cannot be replayed
safely, and there is no read API through which anyone would look at it. Storage for bytes nothing can
act on.

**Raise event retention to make comments outlive the timeline.** Delays the loss, never fixes it, and
walks straight into ADR 0014's revisit triggers. The correct fix for irreversible loss is an export,
not a longer fuse.

**Move comments out of `alert_events` into their own unpartitioned table.** Would make comments
immortal for free, and is rejected on doctrine: ADR 0003 makes the event stream the truth and every
other table a projection. A comment that is not an event is a second kind of history, and the next
question is which one the timeline renders. If cold storage is ever ruled out, this becomes the
cheapest remaining mitigation and should be reconsidered as an amendment to 0003.

**Shortest-wins for the per-org fold.** Honours a low setting exactly, and silently deletes another
tenant's data to do it. Never acceptable for an irreversible operation.

**Parquet, or S3, in the first export.** Both are the right long-term shape and both add a dependency
and a failure mode to a feature whose entire value is that it runs before a `DROP TABLE`. A file in a
directory can be verified by `ls`.

## Amendment 1 — the `alert_event_keys` pruner exists, and the horizon is a floor

Open defect 1 above is closed. `retention.prune` now sweeps `alert_event_keys` alongside
`ingest_dedup`, `idempotency_claims` and `sessions` — the rows no partition drop can reclaim.
The index `alert_event_keys_prune_idx (created_at)` had been carried since 00007 for exactly this
statement and served no other; migration 00043 rewrites the table comment that stated the sweep as
fact for thirty-six migrations while nothing performed it.

**The horizon is not flatly 30 days, and this is the part that changed the decision above.** The body
of this ADR reads the coupling in one direction — `raw_retention_days` derives from the key horizon —
which is right, and incomplete the moment the pruner is real. `raw_retention_days` is a per-org
setting bounded 1..365, and "longest wins" means the raw partitions of every tenant are kept to the
widest window any single tenant asked for. A key horizon fixed at 30 days would therefore leave an org
on 365 days holding replayable payloads for eleven months after the keys that make them replayable
were deleted, and every replay of one would append the timeline a second time and report success.

So the sweep deletes at the **wider** of `alerts/domain.DedupeKeyRetention` (30 days) and the same
`Container.effectiveRetention` window the partition dropper already computes — **plus one day of
partition grain.** That last day is not slack. `oto_drop_partitions_before` drops a DAILY raw partition
only once its whole range is past `now − rawDays`, so a payload that landed at any hour of day D stays
readable until D+rawDays+1, while a key dies exactly at `created_at + horizon`. Taking `rawDays`
straight would kill the key up to 24h before the batch it guards, and a `partial` batch replayed from
the failed-batch feed inside that window appends its whole timeline a second time and reports success.

The 30 remains the **floor**, and the path that reaches it is the deployment's own
`OTO_RETENTION_RAW_PAYLOADS`, not the per-org setting: `effectiveRetention` seeds the fold with
`Config.Retention.RawPayloads` (shipped at 30 days) and only widens it, so an org on
`raw_retention_days=1` never pulls the horizon down at all. An operator who drops the install-wide raw
window under 720h must not thereby unclaim the keys of episodes that are still open, whose transitions
the reconciler re-applies and which nothing else dedupes. Both directions are pinned by
`TestTheDedupeKeyHorizonIsAFloorThatOnlyWidens`, the grain by
`TestAKeyOutlivesThePartitionHoldingItsPayload` (which asserts the two jobs' clocks against each other
rather than restating the arithmetic), and the constants' equality by
`TestTheShippedRawRetentionIsStillDerivedFromTheKeyHorizon` — prose stating a coupling does not fail
a build.

**What it costs.** One tick is bounded at 20 000 rows, which is arithmetic rather than taste: this
ADR's own volume model is ~8 events per firing episode at 10 000 firings a day, every lifecycle
transition carries a dedupe key, and `retention.prune` is hourly — so the generic 500-row sweep limit
would delete 12 000 a day against 80 000 written, which is a table that still grows forever with a job
in front of it saying otherwise. 20 000 an hour is ~6× the modelled write rate: ~480 000 deletions a
day GROSS, so against the same model's ~80 000 daily arrivals a backlog drains at ~400 000 a day NET,
while remaining a sub-second statement. That is the number to plan with — an install that has been
accumulating keys since it shipped holds tens of millions of them, so it converges over **50 to 75
days**, not over "days" and certainly not in one transaction. The bound is chosen to keep each tick
cheap, not to make the first drain fast.

**What it does not change.** `alert_event_keys` leaves this ADR's "never reaped" list at §2 and joins
the row-pruned side tables. Nothing about the event partitions, the raw partitions or the export
scope moves.

## Amendment 2 — "longest wins" is three functions, and the fallbacks widen for real

"How to overturn this" named `Container.effectiveRetention` as the one function. The reduce now sits
beside it as `foldRetention`, with `widenToSettingsCeiling` as its fallback; the container method is
still the only entry point and still seeds the fold with `Config.Retention`, so deleting the three
together restores the previous behaviour exactly.

**The split exists because this ADR's closing claim — "every fallback in the code widens" — was not
true when it was written.** Two reads stand between the fold and its answer, and both of them
NARROWED on failure. An unreadable tenant list returned the config floor, 30 days as shipped, because
the per-org loop never ran: one `Scopes()` timeout dropped 335 days of raw payloads belonging to an
org configured at the 365-day bound. An unreadable org row was worse-hidden — it `continue`d with the
maximum it had, which is the correct answer for every tenant except the only one whose absence can
change a maximum, the one asking for the longest window, and it logged "keeping the wider window"
while doing it. Both narrowed a drop this ADR records as irreversible: `oto_partitions_manage` issues
`DROP PARTITION`, there is no soft delete, and cold-storage export is still scoped rather than built.

Both now widen to the **settings ceiling** — `Bounds(KeyRawRetention).Max` and
`Bounds(KeyEventRetention).Max`, the 365 days and 120 months of `identity/domain.settingBounds`. It is
the one number reachable *without* the read that cannot be narrower than the answer the read would
have given, because `UpdateOrgSettings` clamps every org to those same bounds. It raises rather than
assigns, so a deployment whose own `OTO_RETENTION_*` already exceeds a per-org bound keeps its wider
window, and a bound the table stops carrying leaves the window untouched instead of handing the
dropper a zero. The cost of the fallback is one hour of disk: the next tick reads the settings again.

`TestEveryFailureInTheRetentionFoldWidensTheWindow` pins both failure paths against the widest
configured org, and pins the all-readable case to an exact answer — an inequality alone is satisfied
by a fold that widens to the ceiling on every tick, which is ten years of `alert_events` partitions
nobody asked for. `TestTheSettingsCeilingOnlyEverWidens` pins the raise-never-assign direction. The
two reads are ports (`app.retentionTenants`, `app.retentionSettings`) for exactly this reason: a pool
and a service cannot be asked to fail, and a rule about failures that no test can reach is a comment —
which is what these two were.
