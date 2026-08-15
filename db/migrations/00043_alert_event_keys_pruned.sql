-- The `alert_event_keys` comment stops being a promise and becomes a
-- description, because `retention.prune` now does the thing it describes.
--
-- ⭐⭐ WHY A COMMENT-ONLY MIGRATION IS THE WHOLE OF THE SCHEMA CHANGE. 00007
-- shipped this table with `alert_event_keys_prune_idx ON alert_event_keys
-- (created_at)` under the line "Serves: the 30-day pruner", and with a table
-- comment stating "Pruned at created_at < now() - 30 days" as fact. The index was
-- real; the pruner was not. Nothing in the tree deleted from this table, ADR 0024
-- listed it under "Never reaped, by anything, at any setting", and its own §6
-- recorded the gap as an open defect. So the index was an unbounded write cost
-- serving exactly one query, and that query had never been written.
--
-- The pruner now exists in Go — `alerts/repository.PruneDedupeKeys`, called from
-- `app.pruneRetention` alongside `ingest_dedup`, `idempotency_claims` and
-- `sessions`, the other rows no partition drop can reclaim. It needs no DDL: the
-- index it rides has been there since 00007 and is the reason this file adds
-- nothing structural. What it does need is for the comment an operator reads at
-- `\d+ alert_event_keys` to say what actually happens, because the old sentence
-- is now wrong in a SECOND way even though its subject finally exists.
--
-- ⛔ THE HORIZON IS NOT FLATLY 30 DAYS, AND SAYING SO WOULD RE-BREAK THE
-- COUPLING THIS TABLE SITS AT THE CENTRE OF. Five places state that
-- `raw_retention_days` defaults to 30 BECAUSE this horizon is 30: SPEC §D.4 and
-- §G.1, ADR 0024, 00036 in this directory, `tuning.DefaultRawRetention` and
-- `identity/domain.settingBounds`. The reason is SPEC acceptance criterion 36 —
-- replaying a stored `ingest_batch` after a parser fix must reproduce the same
-- state without duplicate Slack messages — and that replay is idempotent ONLY
-- while the batch's event keys are still claimed here. Past the horizon a replay
-- appends the timeline a second time and reports success, because zero rows
-- written is documented as the idempotency mechanism working.
--
-- But `raw_retention_days` is a per-org setting bounded 1..365, and
-- `partitions.manage` keeps every raw partition to the LONGEST window any tenant
-- asked for (ADR 0024: retention is a floor, never a ceiling). A tenant holding
-- replayable payloads for a year and keys for thirty days is that silent
-- duplication waiting to happen. So the sweep deletes at the WIDER of the two,
-- reading the window from the same `effectiveRetention` the partition dropper
-- reads, PLUS the one-day partition grain: `oto_drop_partitions_before` keeps a
-- daily partition until its WHOLE range is past the cutoff, so the payload outlives
-- `created_at + rawDays` by up to a day and the key has to outlive it too.
--
-- The 30 stays as the FLOOR, and the path that reaches it is the DEPLOYMENT knob
-- `OTO_RETENTION_RAW_PAYLOADS`, not the per-org setting: `effectiveRetention`
-- starts at the deployment value and only widens, so an org on
-- `raw_retention_days=1` never pulls the window below it. An operator who drops the
-- deployment's own raw window under 720h must not thereby unclaim the keys of
-- episodes that are still open, whose transitions the reconciler re-applies and
-- which nothing else dedupes.
--
-- ⚠️ NO DEFAULT IS DROPPED HERE. `alert_event_keys.created_at` is one of the six
-- columns 00034 deliberately left with a DEFAULT, because
-- `alerts/repository/event.go` omits it in a set-returning batch insert on the
-- ingest path and a DROP DEFAULT would 23502 that write. 00034 called the column
-- "read only by the pruner"; the pruner is what changed, not the column.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). A comment is not data, not a constraint and
-- not a plan input: N and N+1 pods read and write this table identically before
-- and after, and the Down restores the previous text word for word.

-- +goose Up

COMMENT ON TABLE alert_event_keys IS
  'Global idempotency index for alert_events (SPEC §C.8). DELIBERATELY UNPARTITIONED: a unique index on a partitioned table must include the partition key and therefore cannot suppress a duplicate written in a later partition (conflict ruling 14). INSERT ... ON CONFLICT DO NOTHING here, in the same transaction as the event; zero rows means skip the event. PRUNED BY THE HOURLY retention.prune JOB at created_at older than the WIDER of 30 days and one day MORE than the longest raw_retention_days any tenant has configured, in bounded batches so one tick cannot become an outage. That extra day is the partition grain, not slack: oto_drop_partitions_before keeps a daily raw partition until its whole range is past the cutoff, so a payload that landed at any time on day D is still readable, and still replayable, until D plus rawDays plus one. A key that died at D plus rawDays would leave a window in which replaying a stored ingest_batch appends the timeline a SECOND time and reports success. The 30 is the FLOOR and it is the primary number: orgs.settings.raw_retention_days defaults to 30 because of it (ADR 0024, SPEC acceptance criterion 36), since a stored ingest_batch stays replayable only while its keys are still claimed here. The floor is reached by the deployment knob OTO_RETENTION_RAW_PAYLOADS, not by the per-org setting: the window starts at the deployment value and only widens, so an org asking for 1 day cannot pull it down. The widening is what stops the two jobs disagreeing: a tenant that keeps payloads for a year keeps for a year the keys that make them replayable.';

COMMENT ON INDEX alert_event_keys_prune_idx IS
  'Read by ONE statement and no other: the retention.prune sweep in alerts/repository/event.go, DELETE FROM alert_event_keys WHERE ctid IN (SELECT ctid FROM alert_event_keys WHERE created_at < $1 LIMIT $2). It is the inner SELECT that rides this index, and only while the expired tail is a small fraction of the table (a Bitmap Index Scan); on a first sweep where most rows are already past the horizon the planner prefers a Seq Scan, which the LIMIT still bounds. The outer half is a Tid Scan, not an index probe: matching on ctid rather than on (org_id, dedupe_key) is deliberate, because the tuple form plans as a Hash Semi Join over a full Seq Scan of this table and would make every tick cost O(table) instead of O(batch). The C.8 idempotency probe on the write path rides the PRIMARY KEY (org_id, dedupe_key) and never touches this index, so every insert pays for it and only the sweeper spends it. It existed from 00007 with no sweeper to spend it at all.';

-- +goose Down

COMMENT ON INDEX alert_event_keys_prune_idx IS NULL;

COMMENT ON TABLE alert_event_keys IS
  'Global idempotency index for alert_events (SPEC §C.8). DELIBERATELY UNPARTITIONED: a unique index on a partitioned table must include the partition key and therefore cannot suppress a duplicate written in a later partition (conflict ruling 14). INSERT ... ON CONFLICT DO NOTHING here, in the same transaction as the event; zero rows means skip the event. Pruned at created_at < now() - 30 days.';
