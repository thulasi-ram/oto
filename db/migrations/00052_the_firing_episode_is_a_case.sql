-- ADR 0036 -- `AlertOccurrence` becomes `AlertCase`. The entity does not move:
-- one contiguous firing episode of one Alert, `(alert_id, seq)`, gapless, at most
-- one open. No column is added, none is removed, no invariant changes. This file
-- is the schema half of a rename, and the argument for the word is in the ADR.
--
-- ⭐⭐ WHY A RENAME IS WORTH A MIGRATION AT ALL. `Occurrence` describes where the
-- row sits in a data model and says nothing about what a person does with it, and
-- SPEC §A.1's premise is that a name which does not carry its meaning decays into
-- the wrong one. This is the entity oto is FOR -- the replayable firing episode,
-- start, end, ack, outcome, rule snapshot -- and it was the one a reader met last.
--
-- ⛔ THE ANTI-CASELOAD CLAUSE IS THE BINDING PART, and it is schema, not prose.
-- `case` is admitted on the strength of ADR 0036 §1-2 and nothing else: no create
-- endpoint, one alert identity per Case, no human-writable `state`, and
-- SCOPE-BOUNDARY §5.6's column ban applies verbatim to `alert_cases` -- no
-- `assigned_to`, `owner_id`, `owner_team_id`, `watchers`, `subscriber_ids`,
-- `incident_id`, `ticket_id`, `sla_due_at`, no human-set `priority`, and no
-- `case_status` distinct from `state`. `acked_by` stays the only person reference
-- on the row. There is also NO COLUMN NAMED `case`, ever: it is reserved in both
-- Go and SQL. `alert_cases` and `case_id` are unaffected by that, a bare `case` is
-- not permitted at any point, and `test/scope/forbidden_columns_test.go` is what
-- refuses the first one.
--
-- ⭐⭐ WHAT THIS FILE DOES NOT REWRITE, AND WHY THAT IS THE WHOLE DESIGN.
-- `alert_events` holds eight values -- `occurrence.opened`, `.reopened`,
-- `.suppressed`, `.unsuppressed`, `.resolved`, `.expired`, `.acknowledged`,
-- `.unacknowledged` -- and it is MONTHLY-PARTITIONED, APPEND-ONLY AND RETAINED
-- THIRTEEN MONTHS. An `UPDATE ... WHERE type LIKE 'occurrence.%'` would rewrite
-- every lifecycle row in thirteen months of partitions inside goose's single
-- transaction: MVCC doubles the table on disk, autovacuum is blocked for the
-- duration, and the deploy that runs it is the deploy that is down. It could not
-- even be COMPLETE -- a partition detached for cold storage is not reached by any
-- statement here.
--
-- So the eight values stay on disk exactly as written, and the CODE reads both
-- spellings:
--
--   * `alerts/domain.NewEventType` accepts a pre-rename spelling and returns the
--     CANONICAL `case.*` value, so `occurrence` never reaches a client. The public
--     `AlertEventType` enum lists thirty-six values, all canonical, and did not
--     grow. This is the difference between a RENAME and the RETIREMENT 00051 did
--     to `group.member_joined`/`group.member_left`: those two named a fact that
--     stopped existing, so they stayed on the contract as themselves; these eight
--     name the same fact under a new word, and a client should not have to know
--     two names for one thing.
--   * The three registered SQL filters (`groupTrailSQL`, `stateChangeCountsSQL`,
--     `rollupDaySQL`) spell BOTH, and are registered so the gate can prove every
--     value in them is still one the column can hold: `test/arch` walks
--     the persisted set rather than the contract set for exactly this reason.
--
-- The line drawn: SMALL, BOUNDED, UNPARTITIONED tables are rewritten here because
-- the rewrite is instant, exact and reversible. `alert_events` is not, and is read
-- through a translation instead. `alert_events.dedupe_key` is a historical copy of
-- a key whose uniqueness lives in `alert_event_keys` (00007 says so on the column),
-- so it is left alone with the rest of that table.
--
-- ⚠️ `alert_event_keys` IS REWRITTEN AND MUST BE, because it is not history -- it
-- is a live idempotency claim. The dedupe key for an episode moved from
-- `occ:{id}:opened` to `case:{id}:opened`. Leave the claims spelled the old way and
-- a replay of a stored `ingest_batch` -- which SPEC acceptance criterion 36 promises
-- is idempotent for as long as the raw payload is kept -- computes a key that
-- matches nothing and appends the timeline a SECOND time, reporting success while
-- doing it. The table is unpartitioned and pruned at thirty days, so the rewrite is
-- small, and the substitution is a bijection over a fixed prefix, so the Down is
-- exact.
--
-- ⚠️ `river_job` IS REWRITTEN THROUGH A GUARD, KIND AND ARGS BOTH. `jobs/kinds.go`
-- says these strings are a durable wire contract and that renaming one strands
-- every queued job of that kind; `occurrence.reap` becomes `case.reap`, and the
-- `occurrence_id` key in `enrich.run` and `notify.evaluate` payloads becomes
-- `case_id`, so the in-flight rows survive the deploy that renames them. River
-- owns its own tables and its own migrator (00016), and
-- `oto migrate up` runs goose FIRST, so on a fresh database `river_job` does not
-- exist yet -- hence `to_regclass`. This is DML against a value column, never DDL
-- against River's schema.
--
-- ⚠️ ONE FROZEN BLOB IS DELIBERATELY LEFT: `delivery_drills.outcome`. 00039 says a
-- settled drill "returns these bytes verbatim", so rewriting a stage name inside it
-- would contradict the column's own contract for an operator artefact that is
-- disposable by design. `failed_stage`, which is read back as a `StageName`, does
-- move.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). A rename is not expandable: the old and the new
-- name are the same object, so there is no window in which both exist. This must
-- ship WITH the binary that speaks the new names, and the Down below is exact --
-- every statement is a catalogue rename or a bijective substitution, so a rollback
-- restores the schema byte for byte rather than approximately.

-- +goose Up

-- ------------------------------------------------------------- the table

ALTER TABLE alert_occurrences RENAME TO alert_cases;  -- vocab:allow -- the old name, in the file that retires it.

-- ------------------------------------------------------------- the columns
--
-- RENAME COLUMN is a catalogue update. It rewrites no heap, and it carries every
-- CHECK expression, index definition and FK that names the column with it --
-- including `ev_subject_ck` on the partitioned `alert_events`, which is why that
-- table's rename costs nothing while an UPDATE on it would cost everything.

ALTER TABLE alerts              RENAME COLUMN current_occurrence_id TO current_case_id;  -- vocab:allow -- the old name, in the file that retires it.
ALTER TABLE alerts              RENAME COLUMN total_occurrences     TO total_cases;  -- vocab:allow -- the old name, in the file that retires it.
ALTER TABLE alert_events        RENAME COLUMN occurrence_id         TO case_id;  -- vocab:allow -- the old name, in the file that retires it.
ALTER TABLE notifications       RENAME COLUMN occurrence_id         TO case_id;  -- vocab:allow -- the old name, in the file that retires it.
ALTER TABLE delivery_drills     RENAME COLUMN occurrence_id         TO case_id;  -- vocab:allow -- the old name, in the file that retires it.
ALTER TABLE alert_quality_daily RENAME COLUMN occurrences           TO cases;  -- vocab:allow -- the old name, in the file that retires it.
ALTER TABLE alert_quality_daily RENAME COLUMN acked_occurrences     TO acked_cases;  -- vocab:allow -- the old name, in the file that retires it.

-- ------------------------------------------------------------- the constraints
--
-- 00023 normalised these to the `occ_` prefix. They become `case_`, which keeps
-- the property that made `occ_` worth having: the error Postgres raises names the
-- table it came from.

ALTER TABLE alert_cases RENAME CONSTRAINT occ_seq_uniq       TO case_seq_uniq;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_state_ck       TO case_state_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_supreason_ck   TO case_supreason_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_resreason_ck   TO case_resreason_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_ackstate_ck    TO case_ackstate_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_terminal_ended TO case_terminal_ended;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_seq_ck         TO case_seq_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_reopen_ck      TO case_reopen_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_order_ck       TO case_order_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_obs_ck         TO case_obs_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_src_order_ck   TO case_src_order_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_suppress_ck    TO case_suppress_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_suppby_ck      TO case_suppby_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_resolve_ck     TO case_resolve_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_resolve_map_ck TO case_resolve_map_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_ack_ck         TO case_ack_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_acklabel_ck    TO case_acklabel_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_ackorder_ck    TO case_ackorder_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_acknote_ck     TO case_acknote_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_reopenof_ck    TO case_reopenof_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_time_ck        TO case_time_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_sver_ck        TO case_sver_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_supcount_ck    TO case_supcount_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_group_fk       TO case_group_fk;
ALTER TABLE alert_cases RENAME CONSTRAINT occ_rule_fk        TO case_rule_fk;

ALTER TABLE alerts RENAME CONSTRAINT alerts_occ_ck         TO alerts_case_ck;
ALTER TABLE alerts RENAME CONSTRAINT alerts_current_occ_fk TO alerts_current_case_fk;

-- ------------------------------------------------------------- the indexes

-- The PRIMARY KEY index is auto-named after the table it was created on, and
-- RENAME TO does not follow it: without this line the catalogue keeps saying
-- `alert_occurrences_pkey` on a table that no longer has that name, which is the
-- one place the old word would survive where nothing scans for it.
ALTER INDEX alert_occurrences_pkey RENAME TO alert_cases_pkey;  -- vocab:allow -- the old name, in the file that retires it.

ALTER INDEX occ_one_open_idx   RENAME TO case_one_open_idx;
ALTER INDEX occ_alert_idx      RENAME TO case_alert_idx;
ALTER INDEX occ_group_idx      RENAME TO case_group_idx;
ALTER INDEX occ_reap_idx       RENAME TO case_reap_idx;
ALTER INDEX occ_ack_idx        RENAME TO case_ack_idx;
ALTER INDEX occ_started_idx    RENAME TO case_started_idx;
ALTER INDEX occ_group_live_idx RENAME TO case_group_live_idx;
ALTER INDEX ev_occ_idx         RENAME TO ev_case_idx;
ALTER INDEX notif_occurrence_idx RENAME TO notif_case_idx;  -- vocab:allow -- the old name, in the file that retires it.

-- ------------------------------------------------------------- the small values
--
-- Three closed enums carry the word as data. All three tables are bounded and
-- unpartitioned or hourly-and-24-hour, so the CHECK swap plus rewrite is instant.

ALTER TABLE enrichments DROP CONSTRAINT enrichments_subjkind_ck;
UPDATE enrichments SET subject_kind = 'case' WHERE subject_kind = 'occurrence';  -- vocab:allow -- the old name, in the file that retires it.
ALTER TABLE enrichments ADD CONSTRAINT enrichments_subjkind_ck
  CHECK (subject_kind IN ('alert','case','group'));

ALTER TABLE ui_events DROP CONSTRAINT ui_events_kind_ck;
ALTER TABLE ui_events DROP CONSTRAINT ui_events_res_ck;
UPDATE ui_events SET kind     = 'case.upserted' WHERE kind     = 'occurrence.upserted';  -- vocab:allow -- the old name, in the file that retires it.
UPDATE ui_events SET resource = 'case'          WHERE resource = 'occurrence';  -- vocab:allow -- the old name, in the file that retires it.
ALTER TABLE ui_events ADD CONSTRAINT ui_events_kind_ck CHECK (kind IN
  ('alert.upserted','case.upserted','group.upserted','event.appended',
   'delivery.updated','source.health'));
ALTER TABLE ui_events ADD CONSTRAINT ui_events_res_ck CHECK (resource IN
  ('alert','case','group','alert_event','delivery','source'));

UPDATE delivery_drills SET failed_stage = 'case' WHERE failed_stage = 'occurrence';  -- vocab:allow -- the old name, in the file that retires it.

-- The live idempotency claims. `occ:` is four characters, so the substitution is a
-- fixed-width prefix swap and cannot touch a key that did not start with it.
--
-- ⚠️ ONE KEY CARRIES THE WORD TWICE. T10's automatic unack is keyed
-- `occ:{id}:unacknowledged:new_occurrence` — the prefix AND the reason — and the
-- reason moved with the entity to `new_case` (`alerts/domain.UnackReasonNewCase`).
-- A prefix-only rewrite would leave a claim no binary ever computes again, which
-- is the same duplicate-append this whole statement exists to prevent.
UPDATE alert_event_keys
   SET dedupe_key = 'case:' || replace(substring(dedupe_key from 5),
                                       ':unacknowledged:new_occurrence',  -- vocab:allow -- the old name, in the file that retires it.
                                       ':unacknowledged:new_case')
 WHERE dedupe_key LIKE 'occ:%';

-- The queued jobs, if River has been migrated yet: the reaper's KIND, and the
-- `case_id` payload key on `enrich.run` and `notify.evaluate`.
--
-- ⚠️ `river_job.args` IS A PERSISTED DTO AND IS THE EASY ONE TO MISS. `jobs/args.go`
-- carries json tags exactly as an API DTO does, and a row queued by the previous
-- binary is decoded against the NEW tags: leave the key spelled the old way and
-- `enrich.run` reads a nil case id and fails every enrichment in flight across the
-- deploy, silently, as a job error rather than a schema error. The queue is small
-- and live, so this is the one rewrite here that is about minutes of data.
-- +goose StatementBegin
DO $$
BEGIN
  IF to_regclass('public.river_job') IS NOT NULL THEN
    EXECUTE 'UPDATE river_job SET kind = ''case.reap'' WHERE kind = ''occurrence.reap''';  -- vocab:allow -- the old name, in the file that retires it.
    EXECUTE 'UPDATE river_job
                SET args = (args - ''occurrence_id'')
                           || jsonb_build_object(''case_id'', args -> ''occurrence_id'')  -- vocab:allow -- the old name, in the file that retires it.
              WHERE args ? ''occurrence_id''';  -- vocab:allow -- the old name, in the file that retires it.
  END IF;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------- the descriptions
--
-- `COMMENT ON` writes a shipped `pg_description` row, which `tools/lintvocab`
-- scans as source text rather than skipping as a comment. Every sentence below
-- that used to say `occurrence` now says what it always meant.

COMMENT ON TABLE alert_cases IS
  'One CONTIGUOUS FIRING EPISODE of an Alert, identified by (alert_id, seq). This is what you acknowledge and whose firing duration is measured -- the duration belongs to the SIGNAL and no per-person timing is stored anywhere (SPEC §A.1, R8). At most one may be open per Alert, enforced by case_one_open_idx. It is an AlertCase and NOT a caseload item: nothing creates one but ingest and the reconciler, it never spans two alert identities, its state is Alertmanager mirrored rather than human-set, and SCOPE-BOUNDARY §5.6 column ban applies to it verbatim (ADR 0036).';
COMMENT ON COLUMN alert_cases.org_id IS 'Denormalised from alerts so every composite index can lead with org_id (CONTEXT.md §5 rule 7).';
COMMENT ON COLUMN alert_cases.seq IS 'Episode number within the Alert, 1-based and gapless.';
COMMENT ON COLUMN alert_cases.suppression_reason IS 'Why it is suppressed: silence | inhibition | mute_time_interval | active_time_interval. Present exactly when state=suppressed (case_suppress_ck).';
COMMENT ON COLUMN alert_cases.ended_at IS 'OTO clock. NULL exactly while the episode is open (case_terminal_ended).';
COMMENT ON COLUMN alert_cases.resolve_reason IS 'upstream (an explicit status=resolved arrived) or timeout (we stopped hearing about it). The pair state/resolve_reason is locked together by case_resolve_map_ck so oto can never claim resolved when it means expired.';
COMMENT ON COLUMN alert_cases.reopen_of IS 'The previous Case when this episode re-fired within the grace window (transition T7, SPEC §B.5).';
COMMENT ON COLUMN alert_cases.state_version IS 'Optimistic lock. Every state transition is a compare-and-set on this value; a lost CAS is a conflict, never a silent overwrite.';

COMMENT ON COLUMN alerts.state IS 'Projection of the current Case: firing | suppressed | resolved | expired. expired is NOT resolved -- resolved requires an explicit upstream status=resolved; expired means we stopped hearing about it. Never fabricate a resolution.';
COMMENT ON COLUMN alerts.current_case_id IS 'The latest AlertCase.';
COMMENT ON TABLE alerts IS
  'The IDENTITY of a label set within (org, cluster_key) -- oto answer to Sentry Issue. Created on first sight and never deleted; resolution ends a Case, not an Alert. Everything below first_seen_at is a PROJECTION of alert_events, kept for query speed, never the only record. It carries NO ack: an acknowledgement is a receipt for one firing episode and lives on alert_cases, because a claim projected onto this row would outlive the firing it was about.';

COMMENT ON COLUMN alert_events.dedupe_key IS 'Optional idempotency token, e.g. case:{id}:opened. Its uniqueness is enforced in alert_event_keys, NOT here -- see that table. Rows written before ADR 0036 carry the earlier `occ:` prefix and are read as history; the live claims were rewritten with the rename.'; -- vocab:allow -- names the pre-rename key prefix so an operator reading pg_description can tell why two shapes exist on disk.

COMMENT ON COLUMN enrichments.subject_kind IS 'What was enriched: alert | case | group.';
COMMENT ON COLUMN ui_events.kind IS 'Closed enum from §E.4: alert.upserted | case.upserted | group.upserted | event.appended | delivery.updated | source.health. The SSE event: name.';

COMMENT ON COLUMN alert_quality_daily.cases IS 'Episodes that STARTED on this day for this (cluster, alertname).';
COMMENT ON COLUMN alert_quality_daily.acked_cases IS 'Episodes acknowledged by SOMEBODY. Never by whom -- that is the whole point (R8). Capped at cases.';

COMMENT ON INDEX case_group_live_idx IS
  'Serves listCurrentMembersSQL -- the keyset page behind GET /alert-groups/{id}/alerts, the twenty-row top_alerts preview behind GET /alert-groups/{id} which the ack, snooze and unsnooze replies also render, the fan-out candidate read and the current-member count. It carries both equalities, the partial predicate and the whole sort key, so the LIMIT stops the scan and no Sort node appears. PARTIAL on ended_at IS NULL so it stays the size of the LIVE membership rather than of the generation history: the replay reads (allMembersSQL, membersAtSQL) and the rollup want the ended episodes back and are meant to ride case_group_idx instead.';
COMMENT ON INDEX notif_case_idx IS
  'Serves the delivery roll-up of one firing episode: was anybody told about THIS outage, and did it land. Partial because case_id is NULL on every group-scoped intent, which is most of them.';
COMMENT ON COLUMN alert_cases.group_id IS
  'THE MEMBERSHIP. Which AlertGroup generation this episode belongs to, and the only record of it since alert_group_members was dropped. Written once, when the episode opens, and never moved: the group key is derived from the alert own labels (ADR 0038) and alert identity IS its label set, so an episode split key is fixed for its whole life. NULL means the group key could not be computed and the signal was recorded groupless, which is the deliberate degradation in the ingest orchestrator. started_at is when it joined and ended_at is when it left; membership is still history, it is just the episode own history now.';

-- +goose Down

COMMENT ON COLUMN alert_cases.group_id IS
  'THE MEMBERSHIP. Which AlertGroup generation this episode belongs to, and the only record of it since alert_group_members was dropped. Written once, when the episode opens, and never moved: the group key is derived from the alert own labels (ADR 0038) and alert identity IS its label set, so an episode split key is fixed for its whole life. NULL means the group key could not be computed and the signal was recorded groupless, which is the deliberate degradation in the ingest orchestrator. started_at is when it joined and ended_at is when it left; membership is still history, it is just the episode own history now.';

-- +goose StatementBegin
DO $$
BEGIN
  IF to_regclass('public.river_job') IS NOT NULL THEN
    EXECUTE 'UPDATE river_job SET kind = ''occurrence.reap'' WHERE kind = ''case.reap''';
    EXECUTE 'UPDATE river_job
                SET args = (args - ''case_id'')
                           || jsonb_build_object(''occurrence_id'', args -> ''case_id'')
              WHERE args ? ''case_id''';
  END IF;
END
$$;
-- +goose StatementEnd

UPDATE alert_event_keys
   SET dedupe_key = 'occ:' || replace(substring(dedupe_key from 6),
                                      ':unacknowledged:new_case',
                                      ':unacknowledged:new_occurrence')
 WHERE dedupe_key LIKE 'case:%';

UPDATE delivery_drills SET failed_stage = 'occurrence' WHERE failed_stage = 'case';

ALTER TABLE ui_events DROP CONSTRAINT ui_events_kind_ck;
ALTER TABLE ui_events DROP CONSTRAINT ui_events_res_ck;
UPDATE ui_events SET kind     = 'occurrence.upserted' WHERE kind     = 'case.upserted';
UPDATE ui_events SET resource = 'occurrence'          WHERE resource = 'case';
ALTER TABLE ui_events ADD CONSTRAINT ui_events_kind_ck CHECK (kind IN
  ('alert.upserted','occurrence.upserted','group.upserted','event.appended',
   'delivery.updated','source.health'));
ALTER TABLE ui_events ADD CONSTRAINT ui_events_res_ck CHECK (resource IN
  ('alert','occurrence','group','alert_event','delivery','source'));

ALTER TABLE enrichments DROP CONSTRAINT enrichments_subjkind_ck;
UPDATE enrichments SET subject_kind = 'occurrence' WHERE subject_kind = 'case';
ALTER TABLE enrichments ADD CONSTRAINT enrichments_subjkind_ck
  CHECK (subject_kind IN ('alert','occurrence','group'));

ALTER INDEX notif_case_idx     RENAME TO notif_occurrence_idx;
ALTER INDEX ev_case_idx        RENAME TO ev_occ_idx;
ALTER INDEX case_group_live_idx RENAME TO occ_group_live_idx;
ALTER INDEX case_started_idx   RENAME TO occ_started_idx;
ALTER INDEX case_ack_idx       RENAME TO occ_ack_idx;
ALTER INDEX case_reap_idx      RENAME TO occ_reap_idx;
ALTER INDEX case_group_idx     RENAME TO occ_group_idx;
ALTER INDEX case_alert_idx     RENAME TO occ_alert_idx;
ALTER INDEX case_one_open_idx  RENAME TO occ_one_open_idx;
ALTER INDEX alert_cases_pkey   RENAME TO alert_occurrences_pkey;

ALTER TABLE alerts RENAME CONSTRAINT alerts_current_case_fk TO alerts_current_occ_fk;
ALTER TABLE alerts RENAME CONSTRAINT alerts_case_ck         TO alerts_occ_ck;

ALTER TABLE alert_cases RENAME CONSTRAINT case_rule_fk        TO occ_rule_fk;
ALTER TABLE alert_cases RENAME CONSTRAINT case_group_fk       TO occ_group_fk;
ALTER TABLE alert_cases RENAME CONSTRAINT case_supcount_ck    TO occ_supcount_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_sver_ck        TO occ_sver_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_time_ck        TO occ_time_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_reopenof_ck    TO occ_reopenof_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_acknote_ck     TO occ_acknote_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_ackorder_ck    TO occ_ackorder_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_acklabel_ck    TO occ_acklabel_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_ack_ck         TO occ_ack_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_resolve_map_ck TO occ_resolve_map_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_resolve_ck     TO occ_resolve_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_suppby_ck      TO occ_suppby_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_suppress_ck    TO occ_suppress_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_src_order_ck   TO occ_src_order_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_obs_ck         TO occ_obs_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_order_ck       TO occ_order_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_reopen_ck      TO occ_reopen_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_seq_ck         TO occ_seq_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_terminal_ended TO occ_terminal_ended;
ALTER TABLE alert_cases RENAME CONSTRAINT case_ackstate_ck    TO occ_ackstate_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_resreason_ck   TO occ_resreason_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_supreason_ck   TO occ_supreason_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_state_ck       TO occ_state_ck;
ALTER TABLE alert_cases RENAME CONSTRAINT case_seq_uniq       TO occ_seq_uniq;

ALTER TABLE alert_quality_daily RENAME COLUMN acked_cases     TO acked_occurrences;
ALTER TABLE alert_quality_daily RENAME COLUMN cases           TO occurrences;
ALTER TABLE delivery_drills     RENAME COLUMN case_id         TO occurrence_id;
ALTER TABLE notifications       RENAME COLUMN case_id         TO occurrence_id;
ALTER TABLE alert_events        RENAME COLUMN case_id         TO occurrence_id;
ALTER TABLE alerts              RENAME COLUMN total_cases     TO total_occurrences;
ALTER TABLE alerts              RENAME COLUMN current_case_id TO current_occurrence_id;

ALTER TABLE alert_cases RENAME TO alert_occurrences;

COMMENT ON TABLE alert_occurrences IS
  'One CONTIGUOUS FIRING EPISODE of an Alert, identified by (alert_id, seq). This is what you acknowledge and whose firing duration is measured -- the duration belongs to the SIGNAL and no per-person timing is stored anywhere (SPEC §A.1, R8). At most one may be open per Alert, enforced by occ_one_open_idx.';
COMMENT ON COLUMN alert_occurrences.org_id IS 'Denormalised from alerts so every composite index can lead with org_id (CONTEXT.md §5 rule 7).';
COMMENT ON COLUMN alert_occurrences.seq IS 'Episode number within the Alert, 1-based and gapless.';
COMMENT ON COLUMN alert_occurrences.suppression_reason IS 'Why it is suppressed: silence | inhibition | mute_time_interval | active_time_interval. Present exactly when state=suppressed (occ_suppress_ck).';
COMMENT ON COLUMN alert_occurrences.ended_at IS 'OTO clock. NULL exactly while the episode is open (occ_terminal_ended).';
COMMENT ON COLUMN alert_occurrences.resolve_reason IS 'upstream (an explicit status=resolved arrived) or timeout (we stopped hearing about it). The pair state/resolve_reason is locked together by occ_resolve_map_ck so oto can never claim resolved when it means expired.';
COMMENT ON COLUMN alert_occurrences.reopen_of IS 'The previous occurrence when this episode re-fired within the grace window (transition T7, SPEC §B.5).';
COMMENT ON COLUMN alert_occurrences.state_version IS 'Optimistic lock. Every state transition is a compare-and-set on this value; a lost CAS is a conflict, never a silent overwrite.';

COMMENT ON COLUMN alerts.state IS 'Projection of the current occurrence: firing | suppressed | resolved | expired. expired is NOT resolved -- resolved requires an explicit upstream status=resolved; expired means we stopped hearing about it. Never fabricate a resolution.';
COMMENT ON COLUMN alerts.current_occurrence_id IS 'The latest AlertOccurrence. FK added below, after alert_occurrences exists.';
COMMENT ON TABLE alerts IS
  'The IDENTITY of a label set within (org, cluster_key) -- oto answer to Sentry Issue. Created on first sight and never deleted; resolution ends an Occurrence, not an Alert. Everything below first_seen_at is a PROJECTION of alert_events, kept for query speed, never the only record. It carries NO ack: an acknowledgement is a receipt for one firing episode and lives on alert_occurrences, because a claim projected onto this row would outlive the firing it was about.';

COMMENT ON COLUMN alert_events.dedupe_key IS 'Optional idempotency token, e.g. occ:{id}:opened. Its uniqueness is enforced in alert_event_keys, NOT here -- see that table.';

COMMENT ON COLUMN enrichments.subject_kind IS 'What was enriched: alert | occurrence | group.';
COMMENT ON COLUMN ui_events.kind IS 'Closed enum from §E.4: alert.upserted | occurrence.upserted | group.upserted | event.appended | delivery.updated | source.health. The SSE event: name.';

COMMENT ON COLUMN alert_quality_daily.occurrences IS 'Episodes that STARTED on this day for this (cluster, alertname).';
COMMENT ON COLUMN alert_quality_daily.acked_occurrences IS 'Episodes acknowledged by SOMEBODY. Never by whom -- that is the whole point (R8). Capped at occurrences.';

COMMENT ON INDEX occ_group_live_idx IS
  'Serves listCurrentMembersSQL -- the keyset page behind GET /alert-groups/{id}/alerts, the twenty-row top_alerts preview behind GET /alert-groups/{id} which the ack, snooze and unsnooze replies also render, the fan-out candidate read and the current-member count. It carries both equalities, the partial predicate and the whole sort key, so the LIMIT stops the scan and no Sort node appears. PARTIAL on ended_at IS NULL so it stays the size of the LIVE membership rather than of the generation history: the replay reads (allMembersSQL, membersAtSQL) and the rollup want the ended episodes back and are meant to ride occ_group_idx instead. It replaces gm_current_idx, which was partial on a left_at nothing ever wrote and therefore narrowed nothing at all.';
COMMENT ON INDEX notif_occurrence_idx IS
  'Serves the delivery roll-up of one firing episode: was anybody told about THIS outage, and did it land. Partial because occurrence_id is NULL on every group-scoped intent, which is most of them.';
COMMENT ON COLUMN alert_occurrences.group_id IS
  'THE MEMBERSHIP. Which AlertGroup generation this episode belongs to, and the only record of it since alert_group_members was dropped. Written once, when the episode opens, and never moved: the group key is derived from the alert own labels (ADR 0038) and alert identity IS its label set, so an episode split key is fixed for its whole life. NULL means the group key could not be computed and the signal was recorded groupless, which is the deliberate degradation in the ingest orchestrator. started_at is when it joined and ended_at is when it left; membership is still history, it is just the episode own history now.';
