-- SPEC §A.1, §P-20, SCOPE-BOUNDARY AC-49 -- comment text only, no DDL.
--
-- A `COMMENT ON` is not a comment. It is a row in `pg_description` that ships to
-- every operator, every schema dump and every introspection tool, and it is the
-- one place a banned word can survive a code review forever because nobody greps
-- the database. Two of them did:
--
--   alert_occurrences                        -- "what you time for MTTR"
--   alert_quality_daily.total_firing_seconds -- "the raw material for MTTR"
--
-- MTTR measures a HUMAN's repair time. oto measures how long a SIGNAL fired, and
-- has no idea who was awake (SPEC §A.1, R8). The distinction is the product.
--
-- The migrations that wrote the old text (00007, 00014) are history and are not
-- edited; this one supersedes them, so a database migrated last year and one
-- created this morning describe themselves identically.
--
-- This migration changes no table, column, type, constraint or index. It is safe
-- on a live system and holds no lock worth naming.

-- +goose Up

COMMENT ON TABLE alert_occurrences IS
  'One CONTIGUOUS FIRING EPISODE of an Alert, identified by (alert_id, seq). This is what you acknowledge and whose firing duration is measured -- the duration belongs to the SIGNAL and no per-person timing is stored anywhere (SPEC §A.1, R8). At most one may be open per Alert, enforced by occ_one_open_idx.';

COMMENT ON COLUMN alert_quality_daily.total_firing_seconds IS
  'Summed firing duration of the episodes that closed on this day. A property of the SIGNAL: it says how long the cluster was unhappy, never how long a person took, and no per-person timing is stored anywhere (SPEC §A.1, R8).';

-- +goose Down

-- vocab:allow — a Down restores the prior text verbatim or it is not a rollback.
COMMENT ON TABLE alert_occurrences IS
  'One CONTIGUOUS FIRING EPISODE of an Alert, identified by (alert_id, seq). This is what you acknowledge and what you time for MTTR. At most one may be open per Alert, enforced by occ_one_open_idx.';

COMMENT ON COLUMN alert_quality_daily.total_firing_seconds IS
  'Summed firing duration, the raw material for MTTR without storing a per-person timing anywhere.';
