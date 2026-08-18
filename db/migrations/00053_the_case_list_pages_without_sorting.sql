-- +goose Up

-- ⭐ TWO INDEXES WIDENED BY ONE COLUMN, SO THE CASE LIST STOPS SORTING.
--
-- `GET /api/v1/cases` (§E.3b) is the operator's first screen: the open cases
-- they have not acknowledged, newest first. It pages by keyset on
-- `(started_at DESC, id DESC)` — the id is the tiebreak, and it is not
-- decorative: one Alertmanager batch opens every case in it at the SAME
-- INSTANT, so `started_at` alone is not a total order and a page boundary
-- inside a batch is not reproducible.
--
-- Both indexes it rides already existed and both stopped one column short of
-- the sort key, so Postgres matched the equality and the range and then paid an
-- Incremental Sort for the tiebreak. The sort is bounded — one batch, not one
-- org — so this is a papercut rather than a defect, which is why the read
-- shipped without it. It is still a sort node on the busiest page in the
-- product.
--
-- ⛔ WIDENED IN PLACE, NOT ADDED ALONGSIDE. Each new index has the OLD one's
-- leading columns and the OLD one's predicate, so it answers every query its
-- predecessor did — including `escalation.check` (§G.9) on the ack index, and
-- the `occ` CTE of `stats.rollup` plus `relatedAlertsSQL` on the started index.
-- A second index would serve nothing extra and would be written on every
-- episode open, on the hottest insert path oto has.
--
-- 00042's argument for `(org_id, started_at)` — "a third key column IN FRONT OF
-- started_at would put the range back off the leading edge" — is unharmed:
-- `id` goes BEHIND the range, where it costs the range nothing.

DROP INDEX IF EXISTS case_ack_idx;
CREATE INDEX case_ack_idx ON alert_cases (org_id, ack_state, started_at DESC, id DESC)
  WHERE ended_at IS NULL;

-- +goose StatementBegin
COMMENT ON INDEX case_ack_idx IS
  'Serves the unacked-and-still-open queue -- the default view of GET /api/v1/cases -- and escalation.check (SPEC G.9). Partial on ended_at IS NULL so it stays the size of the LIVE case set rather than of every episode ever opened. Carries the whole keyset sort key (started_at DESC, id DESC) so the LIMIT stops the scan and no Sort node appears; before 00053 it stopped at started_at and the id tiebreak cost an Incremental Sort bounded by one Alertmanager batch.';
-- +goose StatementEnd

DROP INDEX IF EXISTS case_started_idx;
CREATE INDEX case_started_idx ON alert_cases (org_id, started_at, id);

-- +goose StatementBegin
COMMENT ON INDEX case_started_idx IS
  'Serves the occ CTE of stats.rollup -- every episode an org OPENED on one UTC day -- the candidates CTE of relatedAlertsSQL which asks the same shape on the request path, and since 00053 the UNFILTERED page of GET /api/v1/cases. Two key columns and a tiebreak: the range stays on the leading edge after org_id, and id sits BEHIND it where it costs the range nothing while completing the keyset sort key. Postgres scans it backwards for the list page ORDER BY started_at DESC, id DESC.';
-- +goose StatementEnd

-- +goose Down

DROP INDEX IF EXISTS case_ack_idx;
CREATE INDEX case_ack_idx ON alert_cases (org_id, ack_state, started_at DESC)
  WHERE ended_at IS NULL;

-- +goose StatementBegin
COMMENT ON INDEX case_ack_idx IS
  'Serves the "unacked and still open" queue, and escalation.check (SPEC G.9).';
-- +goose StatementEnd

DROP INDEX IF EXISTS case_started_idx;
CREATE INDEX case_started_idx ON alert_cases (org_id, started_at);

-- +goose StatementBegin
COMMENT ON INDEX case_started_idx IS
  'Serves the occ CTE of stats.rollup -- every episode an org OPENED on one UTC day -- and the candidates CTE of relatedAlertsSQL, which asks the same shape on the request path. Two columns and no more: a third key column in front of started_at would put the range back off the leading edge.';
-- +goose StatementEnd
