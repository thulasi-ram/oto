-- +goose Up
-- +goose StatementBegin

-- Two index gaps found by EXPLAIN against a 60 000-alert dev database.

-- 1. The DEFAULT alert list — no state filter — could range-scan nothing and
--    fell back to a Parallel Seq Scan at 72 ms. alerts_list_idx leads with
--    `state`, so it cannot drive an unfiltered ordered scan, and alerts_open_idx
--    is partial on state IN ('firing','suppressed'), so it excludes resolved and
--    expired rows that an unfiltered list must return.
--
--    This is the primary screen of the product, so it gets the total ordering
--    the keyset actually pages on. The trailing id keeps the keyset total when
--    many alerts share a last_seen_at, which is exactly what a storm produces.
CREATE INDEX alerts_list_all_idx
    ON alerts (org_id, last_seen_at DESC, id DESC);

-- 2. GET /api/v1/snoozes rode alert_snoozes_expiry_idx, which section D.8b wrote
--    for the background expiry sweep and which deliberately does NOT lead with
--    org_id. Correct, but it makes the tenant predicate a filter over a GLOBAL
--    range: one org pages through every other org's snoozes to find its own.
--    Also violates the CONTEXT section 5.7 rule that every composite index
--    starts with org_id. The sweep keeps its own index; this one serves reads.
CREATE INDEX alert_snoozes_active_org_idx
    ON alert_snoozes (org_id, snoozed_until)
    WHERE ended_at IS NULL;

COMMENT ON INDEX alerts_list_all_idx IS
    'Default (state-unfiltered) alert list, keyset-ordered by last_seen_at.';
COMMENT ON INDEX alert_snoozes_active_org_idx IS
    'Tenant-scoped active snooze reads; the sweep uses alert_snoozes_expiry_idx.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS alert_snoozes_active_org_idx;
DROP INDEX IF EXISTS alerts_list_all_idx;

-- +goose StatementEnd
