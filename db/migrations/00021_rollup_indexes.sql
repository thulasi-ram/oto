-- +goose Up
-- +goose StatementBegin

-- Indexes serving GET /api/v1/alerts/rollups (server-side grouping by
-- alertname | namespace | fingerprint) and the -first_seen_at sort on
-- /alert-groups.
--
-- Measured before adding these: the namespace and fingerprint roll-ups fell back
-- to a hash aggregate over a sequential scan, while the alertname axis already
-- served from alerts_name_idx. These give the other two axes the same shape —
-- a streaming GroupAggregate — and make the roll-up keyset predicate an index
-- range rather than a filter.
--
-- Both lead with org_id: a roll-up is always tenant-scoped, and an index that
-- does not lead with org_id makes the tenant predicate a post-scan filter.

-- Serves: rollups grouped by namespace, and namespace-filtered alert lists.
CREATE INDEX alerts_ns_group_idx
    ON alerts (org_id, namespace);

-- Serves: rollups grouped by fingerprint. source_fingerprint is Alertmanager's
-- own FNV-1a label hash, kept alongside oto's identity key for observability.
CREATE INDEX alerts_fp_group_idx
    ON alerts (org_id, source_fingerprint);

-- Serves: sort=-first_seen_at on /alert-groups, which was previously a
-- filter-then-sort. The trailing id keeps the keyset total under equal
-- timestamps, which is common when a storm opens many generations at once.
CREATE INDEX grp_first_seen_idx
    ON alert_groups (org_id, first_seen_at DESC, id DESC);

COMMENT ON INDEX alerts_ns_group_idx  IS 'Roll-up aggregation and filtering by namespace, tenant-scoped.';
COMMENT ON INDEX alerts_fp_group_idx  IS 'Roll-up aggregation by Alertmanager fingerprint, tenant-scoped.';
COMMENT ON INDEX grp_first_seen_idx   IS 'Keyset sort of alert groups by first_seen_at descending.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS grp_first_seen_idx;
DROP INDEX IF EXISTS alerts_fp_group_idx;
DROP INDEX IF EXISTS alerts_ns_group_idx;

-- +goose StatementEnd
