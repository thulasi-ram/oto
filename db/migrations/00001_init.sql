-- +goose Up
-- +goose StatementBegin

-- Extensions only. The real oto schema is owned by a separate agent and lands in
-- the migrations that follow this one (SPEC §D):
--
--   00002_tenancy.sql       -- SPEC §D.1  orgs, users, sessions, PATs, ingest tokens
--   00003_sources.sql       -- SPEC §D.2  clusters, alert sources, source_health
--   00004_ingestion.sql     -- SPEC §D.3  ingest_batches (partitioned), ingest_dedup,
--                           --            ingest_rejections
--   00005_alerts.sql        -- SPEC §D.4  alerts, alert_occurrences, alert_events
--                           --            (partitioned), alert_event_keys
--   00006_groups.sql        -- SPEC §D.5  alert_groups, generations, membership
--   00007_rules.sql         -- SPEC §D.6  rule_snapshots, rule_matches
--   00008_enrichment.sql    -- SPEC §D.7  enrichments
--   00009_notification.sql  -- SPEC §D.8  channels, policies, notifications,
--                           --            notification_deliveries, channel_threads
--   00010_silences.sql      -- SPEC §D.9  silences (read-only mirror, R3)
--   00011_platform.sql      -- SPEC §D.10 ui_events, rate_limit_buckets, stat rollups
--
-- Binding constraints for whoever writes those (CONTEXT.md §5, SPEC §D):
--   * expand/contract only -- assume release N and N+1 run simultaneously
--   * every composite index starts with org_id
--   * enums are TEXT + CHECK, never a Postgres ENUM type
--   * all timestamps are TIMESTAMPTZ in UTC
--   * ids are UUIDv7 minted in Go by platform/id.New(); never gen_random_uuid()
--   * partition management per SPEC §D.11; retention is DROP PARTITION, never DELETE

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- pgcrypto is deliberately NOT dropped: other databases in the cluster may rely
-- on it, and dropping an extension is exactly the kind of destructive migration
-- the expand/contract rule forbids.
SELECT 1;

-- +goose StatementEnd
