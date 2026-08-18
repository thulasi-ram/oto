-- SPEC §B.8, §D.8b -- The suppression decision reads `alert_snoozes`, not a
-- denormalised timestamp on `alerts`.
--
-- 00017 created the side table AND a bare-timestamp projection beside it, and
-- said why the split was deliberate: the authoritative row knows who asked, what
-- they wrote and how the quiet period ended; `alerts.snoozed_until` knows none of
-- those things. What 00017 could not anticipate is that the projection would
-- become the column the NOTIFICATION path reads to decide whether oto speaks --
-- a correctness read, on the hot path, expressed as a string literal in another
-- module's SQL, with no import and no compiler error to break.
--
-- The projection had exactly two remaining justifications and both are gone:
--
--   1. the `?snoozed=` base-table predicate on the alert list. It becomes a LEFT
--      ANTI-JOIN for the main tab and a driving read of `alert_snoozes` for the
--      Quiet tab. Both are cheap for the same reason: `alert_snoozes_active_idx`
--      is UNIQUE (alert_id) WHERE ended_at IS NULL, so the only rows either side
--      can see are the CURRENTLY ACTIVE snoozes -- dozens, never millions. A hash
--      join whose build side is dozens of rows preserves the keyset order the
--      list pages on, so LIMIT still stops early.
--   2. a badge. It becomes a labelled tab whose count carries the worst state
--      inside it, which is legible where a badge on row 400 of a scrolling list
--      never was.
--
-- NO NEW INDEX IS NEEDED, and this is the one place the plan was wrong. The
-- partial index the Quiet tab wants -- alert_snoozes (org_id, snoozed_until)
-- WHERE ended_at IS NULL -- ALREADY EXISTS: 00022 wrote it as
-- `alert_snoozes_active_org_idx` for `GET /api/v1/snoozes`, for precisely the
-- reason it is wanted again here (neither of 00017's three indexes leads with
-- org_id). Its COMMENT is widened below to say what it now serves, rather than a
-- fourth index being written to answer a question one already answers.
--
-- ⛔ ORDERING. Every cross-module read of `alerts.snoozed_until` --
-- `internal/notification/repository/snapshot.go` and
-- `internal/grouping/repository/member.go` -- is redirected to `alert_snoozes`
-- in the SAME change that lands this file. A SQL string literal in a module that
-- does not own the table has no compiler error to catch it, so the redirect
-- cannot be left to a follow-up.

-- +goose Up
-- +goose StatementBegin

DROP INDEX IF EXISTS alerts_snooze_idx;

ALTER TABLE alerts DROP COLUMN IF EXISTS snoozed_until;

COMMENT ON INDEX alert_snoozes_active_org_idx IS
    'Tenant-scoped active snooze reads: GET /api/v1/snoozes, the Quiet tab of the alert list and its count, and the build side of the anti-join that keeps quiet alerts out of the main tab. Partial on ended_at IS NULL, so it holds only the snoozes currently in force. The expiry sweep keeps its own alert_snoozes_expiry_idx.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE alerts ADD COLUMN IF NOT EXISTS snoozed_until TIMESTAMPTZ;

UPDATE alerts a
   SET snoozed_until = s.snoozed_until
  FROM alert_snoozes s
 WHERE s.alert_id = a.id
   AND s.org_id = a.org_id
   AND s.ended_at IS NULL;

CREATE INDEX IF NOT EXISTS alerts_snooze_idx
    ON alerts (org_id, snoozed_until) WHERE snoozed_until IS NOT NULL;

COMMENT ON COLUMN alerts.snoozed_until IS
  'Projection of the ACTIVE alert_snoozes row, written in the same transaction (SPEC §B.8.3). A BARE TIMESTAMP and therefore not a person reference; the attribution lives on alert_snoozes. Snooze is the THIRD ORTHOGONAL AXIS (§B.1): it is not a state, it never touches severity, and a snoozed alert is still rendered as firing.';

COMMENT ON INDEX alert_snoozes_active_org_idx IS
    'Tenant-scoped active snooze reads; the sweep uses alert_snoozes_expiry_idx.';

-- +goose StatementEnd
