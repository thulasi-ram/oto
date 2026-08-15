---
title: 0024 — Retention defaults, what each boundary destroys, and cold storage
---
**Status:** Accepted · 2026-08-09
**Decided WITHOUT the owner.** See *How to overturn this*, below — it is designed to be cheap.
**Relates to:** [0014](/adr/0014-postgres-only-no-analytical-store/) (the scale envelope this is derived
from, and the `DETACH` → Parquet escape hatch), [0003](/adr/0003-alert-occurrence-event-separation/)
(why the projections survive what the event stream does not),
[0001](/adr/0001-postgres-sole-datastore-river-job-queue/)
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

This list is not a backlog of tables waiting to be partitioned. Partitioning them was evaluated and
**declined**, because on each one the constraint a partition key would have to join is the product
invariant itself — see
[Amendment 3](#amendment-3--the-never-reaped-list-is-not-a-partitioning-backlog-and-the-blocker-is-uniqueness).

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

## Amendment 3 — the never-reaped list is not a partitioning backlog, and the blocker is uniqueness

The §1 list gets read as a backlog. It is not one. Partitioning the high-growth tables the way the
event tables are partitioned was evaluated end to end, and the answer is **no for eight of them, and
for most of them not ever.**

**The blocker is not volume and not query shape. It is that Postgres requires a unique index on a
partitioned table to include every partition key column, and on these eight the uniqueness constraint
*is* the product invariant.** SPEC §C14 hit this once and solved it once: *"Idempotency moves to two
small **unpartitioned** side tables: `alert_event_keys` and `ingest_dedup`."* The comment above
`alert_event_keys` (`00007_alerts.sql:286`) makes the argument in one line, about `alert_events`:
*"because it would have to include `recorded_at`, and the whole point is to suppress a SECOND write at
a DIFFERENT time."*

Do not take that on trust — two statements against an empty Postgres 11 or later settle it. Declaring
`UNIQUE (org_id, alert_key)` on a table `PARTITION BY RANGE (first_seen_at)` gives
`ERROR: unique constraint on partitioned table must include all partitioning columns`. Add
`first_seen_at` to the tuple and it succeeds — and so does the partial form, where a
`UNIQUE INDEX (alert_id, started_at) WHERE ended_at IS NULL` partitioned on `started_at` is accepted
without complaint and then holds two rows with `ended_at IS NULL` for one `alert_id`, one either side
of a boundary. **There is no third option:** the only DDL that compiles converts a GLOBAL guarantee
into a PER-PARTITION one, which makes this a schema decision and not a tuning one.

Nine constraints across the eight tables break this way. Each row is the whole argument for that table:

| Table | The constraint a partition key would have to join | What a date buys the bug |
|---|---|---|
| `alerts` | `alerts_key_uniq UNIQUE (org_id, alert_key)` (`00007_alerts.sql:54`) | The dedup identity itself. `00007_alerts.sql:101`: *"Dedup is enforced by alerts_key_uniq, NEVER by a read-then-write check."* An alert re-seen after its partition rolls becomes a second Alert with `first_seen_at` reset. |
| `alert_occurrences` | `occ_one_open_idx UNIQUE (alert_id) WHERE ended_at IS NULL` (`00007_alerts.sql:187`) | Exactly two open episodes, one per side. Its own comment: *"An Occurrence is a CONTIGUOUS episode; two open at once is definitionally a bug."* |
| `alert_occurrences` | `occ_seq_uniq UNIQUE (alert_id, seq)` (`00007_alerts.sql:155`) | Two episodes both claiming `seq = 4`. |
| `notifications` | `notifications_idem_uniq UNIQUE (org_id, idempotency_key)` (`00011_notification.sql:219`) | The §C.7 intent identity. An at-least-once `notify.evaluate` retried across a boundary mints a second intent and fans out a second Slack post. |
| `notification_deliveries` | `deliveries_fanout_uniq UNIQUE (notification_id, channel_id, mode)` (`00024_delivery_mode_uniq.sql:26`) | A duplicate Slack message for the same fact on the same channel in the same mode. |
| `channel_threads` | `threads_subject_uniq UNIQUE (channel_id, subject_kind, subject_id)` (`00011_notification.sql:168`) | One generation owns two threads, against ADR 0005. |
| `rule_snapshots` | `rule_snapshots_content_uniq UNIQUE (org_id, source_id, rule_name, rule_group, rule_file, rule_fingerprint)` (`00040_rule_snapshots_per_key_uniq.sql:57`) | Content addressing, destroyed. §C.6 drift is *"the newest snapshot differs from the one bound to the previous occurrence"*, so every boundary manufactures false drift. |
| `alert_groups` | `groups_key_gen_uniq UNIQUE (org_id, group_key, generation)` (`00008_grouping.sql:47`) | The widest blast radius of the set: three inbound FKs (`alert_group_members.group_id`, `alert_occurrences.group_id`, `notifications.group_id`). |
| `alert_group_members` | `PRIMARY KEY (group_id, occurrence_id)` (`00008_grouping.sql:100`) | Weakens "one membership row per (group, occurrence)" to gain `joined_at`, and buys nothing. |

**Relocating an invariant would still buy nothing, because none of these tables' hot queries carry a
lower time bound.** `alerts_list_idx (org_id, state, last_seen_at DESC, id DESC)`
(`00007_alerts.sql:74`) serves the §D.12(a) keyset with no floor at all, so partitioning `alerts`
prunes zero partitions and turns that index into an all-partition merge append. Only
`del_dead_idx (org_id, created_at DESC) WHERE status = 'dead'` (`00011_notification.sql:313`) has
`org_id` as its sole equality, and so is the only index in the set a range bound could prune.

**What is NOT declined: `notification_deliveries`** — it has that one prunable index and, *inferred
from its growth shape rather than measured*, the highest growth rate of the eight. The intuitive growth
model is wrong: it grows **per fan-out target, per (notification × channel × mode), never per delivery
attempt.** `attempts` is incremented in place (`deliveries.go:288`) and capped at 32 by
`deliveries_attempts_ck`; retry and requeue UPDATE the same row, and since `00024` each mode carries
its own, so one fact is 2 rows — not 32. A table retried hard for a week does not grow at all.

The design, so the day a trigger fires is an execution: `deliveries_fanout_uniq` moves off the parent
into a small unpartitioned `delivery_keys` keyed `(notification_id, channel_id, mode)`, the §C14
pattern again. `notifications` can only follow, never lead — its FK child moves in the same migration,
and the FK needs the partition key denormalised onto it. Grain **monthly on `created_at`**, proposed
and not measured. Price the fifth parent first: `oto_partitions_manage` is four literal
`IF to_regclass(…)` blocks (`00005_partitions.sql:174-211`, re-emitted whole by
`00036_retention_defaults.sql:40-89`), and a new parameter is a Postgres *overload* rather than a
replacement, so it drags in the 3-arg `DROP`, `managePartitionsSQL` (`internal/app/workers.go:314`),
`Container.managePartitions` (`:333`), `am_route_timings_test.go:507`'s `pronargs = 3`, and
`test/harness/postgres.go:271`'s zero-argument call.

**The set is eight; §1's list is twelve.** `alert_event_keys` is no longer unreaped (Amendment 1) and
`silences` mirrors Alertmanager's own set. `alert_quality_daily` is the case that proves the rule: its
`PRIMARY KEY (org_id, day, cluster_key, alertname)` (`00014_ops.sql:44`) **already carries the date**,
so it is the one table here that could be partitioned without weakening anything — the blocker really
is uniqueness, not membership of this list; it stays whole because it is bounded by org × day ×
distinct alertname and is ADR 0014's own answer to analytical scale. `enrichments` is the fourth and
the honest gap: unpartitioned, unreaped, growing with the occurrences it annotates, and carrying
`enrichments_subject_uniq UNIQUE (subject_kind, subject_id, enricher)` (`00010_enrichment.sql:29`),
which breaks exactly as the nine above do. It was not in the investigated set; its absence from it is
not a different answer.

**The reopen condition, written down and NOT instrumented.** The trigger is ADR 0014's, unchanged and
numeric, and it is restated here as a named threshold so it survives being read out of context:

1. **alert-list p95 above 200 ms with a warm cache**,
2. **a rollup job overrunning its own interval**,
3. **a single org exceeding roughly 10M events per month.**

**Written down is all they are, and this amendment deliberately leaves them that way.** Every metric oto
exports is a counter, a queue gauge, or a *write-path* histogram — `oto_ingest_duration_seconds`,
`oto_ingest_process_duration_seconds`, `oto_job_duration_seconds`, `oto_thread_head_wait_seconds`,
`oto_clock_skew_seconds`. **There is no read-path or HTTP handler latency metric of any kind, and
nothing anywhere carries a warm/cold cache dimension**, so trigger 1 is unmeasured.
`oto_ingest_alerts_total` is an unlabelled counter of *alerts accepted for processing* — neither
`alert_events` rows nor per-org — so trigger 3 is unmeasured. Trigger 2 is the near miss:
`oto_job_duration_seconds{kind="stats.rollup"}` emits the duration, but the interval it must be
compared against is a schedule, not a series, so nothing evaluates the condition. That gap is stated
rather than closed on purpose: a threshold with a fabricated dashboard behind it is worse than a
threshold with none. **There is no alert on these numbers, no panel, and nothing that will page.**
Reopening starts by building the measurement for whichever trigger you believe has fired — the number
you bring is the first new evidence since ADR 0014 was written, so say where it came from.

**What pins this.** Nothing does, and that is right for a decision that changes no code: a test
asserting eight tables are unpartitioned would pin an accident rather than a choice. What is checkable
is the evidence — the nine constraints above at the file and line given, the four `IF to_regclass`
blocks in `00005_partitions.sql:174-211`, and SPEC §C14. Drop or relocate any one of them and the row
naming it here is what stops being true.
