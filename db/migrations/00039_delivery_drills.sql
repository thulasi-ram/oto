-- DELIVERY DRILLS -- one synthetic alert, pushed through the REAL pipeline, and
-- the provenance mark that keeps it out of every number the product sells.
--
-- ⭐⭐ WHY. `POST /channels/{id}/test` renders one card and hands it to the
-- provider. It answers "does my token work". It cannot answer the question an
-- operator actually has on day one -- "will an alert reach my channel, in a
-- thread, with the right card, and be recorded?" -- because it skips ingestion,
-- alert identity, grouping, the policy match, the rule snapshot, the
-- `channel_threads` sequencing, the ordering gate and the delivery row. Every
-- failure mode oto has lives in exactly the stages it skips. A drill runs them.
--
-- ⭐⭐ THE HARD PART IS NOT SENDING THE ALERT. It is making sure the alert oto
-- manufactures never becomes evidence. `alert_quality_daily`, the dashboard
-- overview and the alert list are the numbers this product is sold on; a drill
-- that inflated them would be worse than no drill at all, because a wrong number
-- is believed and a missing feature is merely absent.
--
-- ⭐ HOW A SYNTHETIC IS MARKED, AND WHY IT IS NOT A LABEL. `alerts.synthetic` is
-- a column set from the PROVENANCE OF THE BATCH -- `ingest_batches.mode =
-- 'synthetic'`, which only the authenticated drill endpoint can write and which
-- appears nowhere in any payload. The obvious alternative, a reserved label, is
-- wrong twice: a label arrives from the wire, so any Alertmanager on earth could
-- forge it and evict its own alerts from oto's statistics; and a label
-- participates in `alert_key` (§C.2), so marking an alert would CHANGE ITS
-- IDENTITY -- the same rule firing before and after someone added the label would
-- be two different Alerts. The mark has to be a fact about how the row got here,
-- not a fact in the row.
--
-- ⛔ A COLUMN COSTS A MIGRATION AND MUST BE THREADED THROUGH EVERY AGGREGATE, and
-- that is the price. The reads that were changed are enumerated on the column
-- comments below. If you add an aggregate over `alerts`, `alert_groups` or
-- `notification_deliveries`, it excludes synthetics or it is wrong.
--
-- ⛔ `alert_groups.synthetic` IS DENORMALISED ON PURPOSE. The dashboard counts
-- open/closed/storm groups straight off this table; reaching `alerts` through
-- `alert_group_members` for every group in the window turns one indexed count
-- into a nested loop. One boolean, written once at generation-open time, is the
-- cheaper honest answer.
--
-- DISPOSAL, AND WHY IT DOES NOT CONTRADICT ADR 0024. ADR 0024's promise is that
-- `alerts`, `alert_occurrences`, `alert_groups`, `notifications`,
-- `notification_deliveries` and `channel_threads` are NEVER reaped -- "retention
-- deletes the narrative, never the record". That promise is about the record of
-- an UPSTREAM SIGNAL. A drill's rows are not a record of anything upstream:
-- nothing fired, no cluster was involved, oto manufactured every byte. They are
-- therefore in a different retention class entirely, and they are cleaned up by a
-- ROW-LEVEL PRUNE inside `retention.prune` -- joining `ingest_dedup`, `sessions`
-- and `enrichment_cache`, the three side tables ADR 0024 already lists as pruned
-- by row. No partition is dropped, and no row that records something a cluster
-- actually did is touched.
--
-- ⭐ WHAT SURVIVES DISPOSAL IS THE RECEIPT. `delivery_drills` keeps its own row,
-- with the frozen staged outcome, after its synthetic signal rows are gone --
-- the same shape ADR 0024 designed for `retention_exports`. The operator can
-- still answer "did the delivery path work last Tuesday" a year later; what they
-- cannot do is find a fake alert in their history.
--
-- FR-1 (CONTEXT.md §1b): a `delivery_drills` row is a fact about a Notification
-- and its Deliveries -- a signal, IN. `started_by` is ACTOR metadata in the
-- `acked_by` mould (past-tense attribution, ON DELETE SET NULL, a frozen label
-- beside it), never a subject: nothing here says anybody owes work.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Three additive columns with defaults, one
-- CHECK widened (a widening is always safe under N/N+1 -- release N writes a
-- strict subset of what N+1 permits), and one new table. Nothing to backfill; a
-- release-N reader that has never heard of `synthetic` simply sees every existing
-- row as false, which is what they are.

-- +goose Up

-- --------------------------------------------------------------- the mark

ALTER TABLE alerts       ADD COLUMN synthetic BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE alert_groups ADD COLUMN synthetic BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN alerts.synthetic IS
  'PROVENANCE, not payload: true exactly when this Alert was first observed from an ingest batch whose mode is synthetic — a delivery drill oto manufactured. NEVER derived from a label, because a label is forgeable by any upstream and participates in alert_key (SPEC §C.2), so marking one would change its identity. EVERY AGGREGATE EXCLUDES THESE: alert_quality_daily (all three CTEs in stats/repository/rollup.go), the dashboard overview, the alert list and roll-up defaults, and the label name/value typeaheads. A new aggregate over this table excludes them too, or it is wrong.';
COMMENT ON COLUMN alert_groups.synthetic IS
  'Denormalised from the observations that opened this generation, so the dashboard group counts can exclude drills with an indexed predicate instead of a nested loop through alert_group_members. Written once, at generation-open time; never read back onto the domain entity, because it is a reporting fact and not a group invariant.';

-- Serves: the drill reaper, and the anti-join shape of every aggregate above.
-- Partial, so the index holds only the handful of drill rows that exist and the
-- ordinary alert list never pays for it.
CREATE INDEX alerts_synthetic_idx       ON alerts       (org_id, id) WHERE synthetic;
CREATE INDEX alert_groups_synthetic_idx ON alert_groups (org_id, id) WHERE synthetic;

-- ------------------------------------------------------- the batch provenance

-- `synthetic` joins push and reconcile. It is the mode of a batch the DRILL
-- ENDPOINT accepted -- an authenticated operator action -- and it is the only
-- mode no Alertmanager can cause, which is the entire point.
ALTER TABLE ingest_batches DROP CONSTRAINT ingest_batches_mode_ck;
ALTER TABLE ingest_batches ADD  CONSTRAINT ingest_batches_mode_ck
  CHECK (mode IN ('push','reconcile','synthetic'));

COMMENT ON COLUMN ingest_batches.mode IS
  'push = an Alertmanager webhook. reconcile = a GET /api/v2/alerts sweep. synthetic = a delivery drill oto manufactured for itself (SPEC C18 for the first two). A batch mode is set by the code path that ACCEPTED the batch and never by anything in the body, which is what makes synthetic unforgeable from the wire.';

-- ----------------------------------------------------------- delivery_drills

CREATE TABLE delivery_drills (
  id                UUID        PRIMARY KEY,
  org_id            UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  source_id         UUID        NOT NULL REFERENCES alert_sources(id) ON DELETE CASCADE,

  -- The nonce that ties every artefact back to this drill. It is written into the
  -- synthetic payload as the `oto_drill` label, so the alert, the occurrence and
  -- the group are all findable by jsonb containment against alerts_labels_gin --
  -- which works whatever the source's ignore_labels does to alert_key.
  drill_label       TEXT        NOT NULL,
  severity          TEXT        NOT NULL,

  -- The batch the drill pushed. NULL only in the window between minting the row
  -- and the accept returning, and on a drill whose accept failed.
  batch_id          UUID,

  -- Artefact ids, cached as each stage discovers them. They are a DISPOSAL
  -- MANIFEST, not the source of truth: the staged result is always recomputed
  -- from the live rows while the drill is running.
  alert_id          UUID,
  occurrence_id     UUID,
  group_id          UUID,
  notification_id   UUID,

  status            TEXT        NOT NULL DEFAULT 'running',
  -- The frozen staged result, written ONCE when the drill reaches a terminal
  -- state. Before that it is NULL and every read recomputes; after it, every read
  -- returns these bytes verbatim, so an operator refreshing the page after
  -- disposal still sees what happened.
  outcome           JSONB,
  failed_stage      TEXT,

  started_by        UUID        REFERENCES users(id) ON DELETE SET NULL,
  started_by_label  TEXT        NOT NULL,

  started_at        TIMESTAMPTZ NOT NULL,
  deadline_at       TIMESTAMPTZ NOT NULL,
  finished_at       TIMESTAMPTZ,
  disposed_at       TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT drills_status_ck   CHECK (status IN ('running','passed','failed','timed_out')),
  CONSTRAINT drills_label_ck    CHECK (drill_label ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
  CONSTRAINT drills_sev_ck      CHECK (length(btrim(severity)) BETWEEN 1 AND 64),
  CONSTRAINT drills_actor_ck    CHECK (length(btrim(started_by_label)) BETWEEN 1 AND 200),
  CONSTRAINT drills_outcome_ck  CHECK (outcome IS NULL OR jsonb_typeof(outcome) = 'object'),
  -- A terminal drill has an outcome and a finish time; a running one has neither.
  -- Frozen together so no read can ever see a settled verdict with no evidence.
  CONSTRAINT drills_final_ck    CHECK ((status = 'running') = (finished_at IS NULL)),
  CONSTRAINT drills_frozen_ck   CHECK ((finished_at IS NULL) = (outcome IS NULL)),
  CONSTRAINT drills_stage_ck    CHECK (failed_stage IS NULL OR status = 'failed'),
  CONSTRAINT drills_deadline_ck CHECK (deadline_at > started_at),
  CONSTRAINT drills_finish_ck   CHECK (finished_at IS NULL OR finished_at >= started_at),
  CONSTRAINT drills_disposed_ck CHECK (disposed_at IS NULL OR finished_at IS NOT NULL),
  CONSTRAINT drills_time_ck     CHECK (updated_at >= created_at)
);

-- Serves: the drill list on the sources screen, newest first, and the "is one
-- already running for this source" precondition.
CREATE INDEX drills_source_idx ON delivery_drills (org_id, source_id, started_at DESC, id DESC);
-- Serves: the retention.prune sweep -- finish what has run out of time, dispose
-- what has been finished long enough. Partial so it stays the size of the work.
CREATE INDEX drills_open_idx   ON delivery_drills (org_id, deadline_at) WHERE finished_at IS NULL;
CREATE INDEX drills_dispose_idx ON delivery_drills (org_id, finished_at)
  WHERE finished_at IS NOT NULL AND disposed_at IS NULL;

COMMENT ON TABLE delivery_drills IS
  'One end-to-end rehearsal of the notification path, driven by a synthetic Alert oto manufactures and pushes through the REAL ingest endpoint. The row is the RECEIPT: it outlives the synthetic signal rows it created, which are disposed of by retention.prune. A drill is a fact about a Notification and its Deliveries — a signal (CONTEXT.md FR-1) — and never a fact about a person.';
COMMENT ON COLUMN delivery_drills.drill_label IS
  'The nonce written into the synthetic payload as the `oto_drill` label. Every artefact is found by `labels @> {"oto_drill": <this>}` against alerts_labels_gin, which is robust to whatever the source ignore_labels does to alert_key.';
COMMENT ON COLUMN delivery_drills.outcome IS
  'The frozen staged result, written exactly once when the drill settles. NULL means the drill is still running and every read recomputes the stages from the live rows — the drill never guesses at a stage it can observe.';
COMMENT ON COLUMN delivery_drills.failed_stage IS
  'The FIRST stage that failed, by name. This is the whole operator-facing value of a drill: not "it did not work" but "the policy matched nothing", "the thread would not open", "Slack said not_in_channel".';
COMMENT ON COLUMN delivery_drills.started_by IS
  'Who ran the drill. ACTOR metadata in the acked_by mould — past-tense attribution, nulled when the user goes, with a frozen label beside it so the receipt stays readable. NO per-person metric is derived from it (SPEC R8).';
COMMENT ON COLUMN delivery_drills.disposed_at IS
  'When the synthetic signal rows this drill created were deleted. ADR 0024 promises the SIGNAL tables are never reaped; that promise is about the record of an upstream event, and a drill records none — so it is pruned by row, like ingest_dedup and sessions, and never by dropping a partition.';

-- +goose Down

DROP TABLE delivery_drills;

ALTER TABLE ingest_batches DROP CONSTRAINT ingest_batches_mode_ck;
ALTER TABLE ingest_batches ADD  CONSTRAINT ingest_batches_mode_ck
  CHECK (mode IN ('push','reconcile'));

COMMENT ON COLUMN ingest_batches.mode IS 'push = an Alertmanager webhook. reconcile = a GET /api/v2/alerts sweep. Both produce Observations for the same state machine (SPEC C18).';

DROP INDEX alert_groups_synthetic_idx;
DROP INDEX alerts_synthetic_idx;

ALTER TABLE alert_groups DROP COLUMN synthetic;
ALTER TABLE alerts       DROP COLUMN synthetic;
