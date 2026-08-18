-- git-bug 5ee536e.
--
-- `00036` said two things about the raw tables that later changes made false, and
-- it is the file a maintainer opens to decide whether lowering
-- `raw_retention_days` is safe -- so both errors land on the same decision.
--
-- ⛔ `00036` IS NOT EDITED, AND THAT IS THIS REPO'S RULE RATHER THAN CAUTION.
-- An applied migration states the schema as it was AT ITS OWN VERSION. `00036:99`
-- invokes the same rule on itself for the `alert_events` rename, declining to
-- rewrite its own comment and pointing at 00052 instead. Correcting a shipped
-- comment in place would make the migration history describe a database that
-- never existed, so the correction is a new version, here.
--
-- WHAT WAS FALSE:
--
-- 1. "THE 30 IS DERIVED, NOT CHOSEN (ADR 0024): it is the alert_event_keys
--    idempotency horizon ... Move one number and you move both." The derivation
--    is gone. `oto replay` gates on SUPERSESSION, not on age
--    (`internal/ingestion/service/replay.go`), and the horizon it named was
--    already unreachable besides: `rawPartitionGrainDays` is 1 precisely so the
--    keys outlive the raw partition. ADR 0024 Amendment 4 -- "the thirty days is
--    chosen, and what it actually buys" -- records the number as CHOSEN.
--
-- 2. "Nothing a product surface renders is served from here; there is no API that
--    reads this table", and on the sibling, "⚠️ No API reads this table yet, so
--    the rejection feed this table exists to serve does not exist and the rows age
--    out unseen". Both endpoints now exist. That is the sentence that matters to
--    the operator sizing a volume: shortening retention takes two screens with it,
--    and the schema told them it would take nothing.

-- +goose Up

-- +goose StatementBegin
COMMENT ON TABLE ingest_batches IS
  'One raw, durably-recorded webhook body or reconciler sweep. Written inside the short ingest transaction (SPEC §G.1) and processed asynchronously; this table is the reason a 2xx is a promise. DAILY partitions on received_at, retention orgs.settings.raw_retention_days (default 30). ⭐ THE 30 IS CHOSEN, NOT DERIVED (ADR 0024 Amendment 4). It was once presented as the alert_event_keys idempotency horizon, but a replay is refused when an alert the batch would touch has MOVED ON since the batch was received -- not when the batch reaches an age -- so no horizon bounds it and the two numbers no longer move together. ⛔ AND THIS TABLE IS READ BY A PRODUCT SURFACE: GET /api/v1/sources/{id}/failed-batches serves it. Lowering raw_retention_days shortens that screen, which is the cost the old comment said did not exist.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON TABLE ingest_rejections IS
  'Every observation oto refused to normalise, with the offending element kept. Writing a row here is what makes it legitimate to return 202 for a partially bad payload -- a 4xx would make Alertmanager delete the alert forever (SPEC §G.2). We never silently drop. DAILY partitions on received_at, retention orgs.settings.raw_retention_days (default 30). ⭐ THE REJECTION FEED THIS TABLE EXISTS TO SERVE NOW EXISTS: GET /api/v1/sources/{id}/rejections reads it, so the rows no longer age out unseen and lowering raw_retention_days shortens that screen too.';
-- +goose StatementEnd

-- +goose Down

-- Byte-identical to what 00036 shipped, so a rolled-back database is in the state
-- its migration history describes -- including the two claims that were true of
-- the release 00036 belonged to.

-- +goose StatementBegin
COMMENT ON TABLE ingest_batches IS
  'One raw, durably-recorded webhook body or reconciler sweep. Written inside the short ingest transaction (SPEC §G.1) and processed asynchronously; this table is the reason a 2xx is a promise. DAILY partitions on received_at, retention orgs.settings.raw_retention_days (default 30). THE 30 IS DERIVED, NOT CHOSEN (ADR 0024): it is the alert_event_keys idempotency horizon, past which SPEC acceptance criterion 36''s replay-after-a-parser-fix would append the timeline a second time — so a payload kept longer cannot be used for the thing it is kept for. Move one number and you move both. Nothing a product surface renders is served from here; there is no API that reads this table.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON TABLE ingest_rejections IS
  'Every observation oto refused to normalise, with the offending element kept. Writing a row here is what makes it legitimate to return 202 for a partially bad payload — a 4xx would make Alertmanager delete the alert forever (SPEC §G.2). We never silently drop. DAILY partitions on received_at, retention orgs.settings.raw_retention_days (default 30). ⚠️ No API reads this table yet, so the rejection feed this table exists to serve does not exist and the rows age out unseen — tracked in ADR 0024 Consequences.';
-- +goose StatementEnd
