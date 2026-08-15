-- ADR 0024 — the raw-payload retention default moves 14 -> 30 days, and the
-- schema's own comments stop disagreeing with the code.
--
-- ⭐⭐ WHY. `raw_retention_days` was 14 on no stated requirement. The one thing a
-- stored raw payload exists to serve is SPEC acceptance criterion 36 — "replaying
-- a stored `ingest_batch` after a parser fix reproduces the same state without
-- duplicate Slack messages" — and that replay is idempotent only while the
-- batch's event dedupe keys are still in `alert_event_keys`, which SPEC §D.4
-- prunes at 30 DAYS. So 30 is the exact width of the window in which a raw
-- payload is still USEFUL; 14 was shorter than that for no reason, and anything
-- longer keeps bytes nothing can safely act on.
--
-- ⛔ THE TWO NUMBERS ARE COUPLED. If the `alert_event_keys` pruner ever moves off
-- 30 days, this moves with it — the same relationship `MinRefireGraceSeconds`
-- has with `ingestion/domain.DedupTTL`.
--
-- WHAT THIS MIGRATION ACTUALLY CHANGES is documentation and one dead parameter
-- default. `oto_partitions_manage`'s DEFAULTs are never used — `partitions.manage`
-- always passes all three arguments explicitly — but a function signature that
-- says 14 while the code says 30 is precisely the third-copy-that-disagrees this
-- schema keeps warning about (CONTEXT.md R9). The COMMENT ON TABLE strings are
-- user-visible through `\d+` and were saying 14 too.
--
-- The live retention behaviour is NOT set here and never was: the effective
-- window is `Config.Retention` folded with every org's `orgs.settings`, computed
-- in `internal/app.Container.effectiveRetention` and passed in as arguments.
--
-- ⚠️ The comments below also record what a DROP actually costs, because the
-- boundary is irreversible and the schema is where somebody reads about a table
-- before they read an ADR.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). CREATE OR REPLACE on a function body that is
-- byte-identical apart from one default, plus comment rewrites. Nothing to
-- backfill, no constraint a release-N writer can violate, and the Down restores
-- the previous text exactly.

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION oto_partitions_manage(p_raw_retention_days      int DEFAULT 30,
                                                 p_event_retention_months  int DEFAULT 13,
                                                 p_ui_retention_hours      int DEFAULT 24)
RETURNS TABLE (parent text, grain text, created int, dropped int)
LANGUAGE plpgsql
AS $$
DECLARE
  v_now timestamptz := now();
BEGIN
  -- SPEC §D.11, in the order it is written there.

  -- ui_events: current hour + next 6, 24h retention.
  IF to_regclass('public.ui_events') IS NOT NULL THEN
    parent  := 'ui_events'; grain := 'hour';
    created := oto_ensure_partitions_ahead('ui_events', 'hour', 6, v_now);
    dropped := oto_drop_partitions_before('ui_events', 'hour',
                                          v_now - make_interval(hours => p_ui_retention_hours));
    RETURN NEXT;
  END IF;

  -- ingest_batches / ingest_rejections: today + next 7 days.
  IF to_regclass('public.ingest_batches') IS NOT NULL THEN
    parent  := 'ingest_batches'; grain := 'day';
    created := oto_ensure_partitions_ahead('ingest_batches', 'day', 7, v_now);
    dropped := oto_drop_partitions_before('ingest_batches', 'day',
                                          v_now - make_interval(days => p_raw_retention_days));
    RETURN NEXT;
  END IF;

  IF to_regclass('public.ingest_rejections') IS NOT NULL THEN
    parent  := 'ingest_rejections'; grain := 'day';
    created := oto_ensure_partitions_ahead('ingest_rejections', 'day', 7, v_now);
    dropped := oto_drop_partitions_before('ingest_rejections', 'day',
                                          v_now - make_interval(days => p_raw_retention_days));
    RETURN NEXT;
  END IF;

  -- alert_events: this month + next 3.
  IF to_regclass('public.alert_events') IS NOT NULL THEN
    parent  := 'alert_events'; grain := 'month';
    created := oto_ensure_partitions_ahead('alert_events', 'month', 3, v_now);
    dropped := oto_drop_partitions_before('alert_events', 'month',
                                          v_now - make_interval(months => p_event_retention_months));
    RETURN NEXT;
  END IF;

  RETURN;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION oto_partitions_manage(int, int, int) IS
  'The entire contract of the hourly `partitions.manage` job (SPEC §G.3, §D.11). Call it as SELECT * FROM oto_partitions_manage(raw_days, event_months, ui_hours). The caller ALWAYS passes all three: the effective window is the deployment config folded with every org''s orgs.settings, computed in internal/app.Container.effectiveRetention, because retention is a FLOOR and never a ceiling (ADR 0024) — a partition holds every tenant''s rows, so the longest configured window wins. The DEFAULTs here are documentation of the shipped values, not the live policy. Idempotent, safe to run every hour forever. There are deliberately NO DEFAULT partitions: a row that falls outside every range must fail loudly rather than silently accumulate in a bucket that can never be dropped.';

COMMENT ON TABLE ingest_batches IS
  'One raw, durably-recorded webhook body or reconciler sweep. Written inside the short ingest transaction (SPEC §G.1) and processed asynchronously; this table is the reason a 2xx is a promise. DAILY partitions on received_at, retention orgs.settings.raw_retention_days (default 30). THE 30 IS DERIVED, NOT CHOSEN (ADR 0024): it is the alert_event_keys idempotency horizon, past which SPEC acceptance criterion 36''s replay-after-a-parser-fix would append the timeline a second time — so a payload kept longer cannot be used for the thing it is kept for. Move one number and you move both. Nothing a product surface renders is served from here; there is no API that reads this table.';

COMMENT ON TABLE ingest_rejections IS
  'Every observation oto refused to normalise, with the offending element kept. Writing a row here is what makes it legitimate to return 202 for a partially bad payload — a 4xx would make Alertmanager delete the alert forever (SPEC §G.2). We never silently drop. DAILY partitions on received_at, retention orgs.settings.raw_retention_days (default 30). ⚠️ No API reads this table yet, so the rejection feed this table exists to serve does not exist and the rows age out unseen — tracked in ADR 0024 Consequences.';

COMMENT ON TABLE alert_events IS
  'The append-only timeline: one immutable thing that happened at one instant. Current state everywhere else in this schema is a projection of these rows, never the only record. If you would ever want to UPDATE a row here, it is not an Event. MONTHLY partitions on recorded_at, retention orgs.settings.event_retention_months (default 13 — the longest default that keeps one org inside ADR 0014''s scale envelope, at ~752 bytes per row all-in). ⛔ DROPPING A PARTITION HERE DESTROYS comment.added AND THE NOTE ON occurrence.unacknowledged, WHICH EXIST NOWHERE ELSE IN THIS SCHEMA AND CANNOT BE REBUILT. Everything else oto promises — the alert, every occurrence with its ack and outcome, the rule snapshot, the notification and delivery record, the thread handle — lives in tables with no reaper. Retention deletes the narrative, never the record (ADR 0024). There is no cold-storage export; it is scoped and unbuilt.';

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION oto_partitions_manage(p_raw_retention_days      int DEFAULT 14,
                                                 p_event_retention_months  int DEFAULT 13,
                                                 p_ui_retention_hours      int DEFAULT 24)
RETURNS TABLE (parent text, grain text, created int, dropped int)
LANGUAGE plpgsql
AS $$
DECLARE
  v_now timestamptz := now();
BEGIN
  IF to_regclass('public.ui_events') IS NOT NULL THEN
    parent  := 'ui_events'; grain := 'hour';
    created := oto_ensure_partitions_ahead('ui_events', 'hour', 6, v_now);
    dropped := oto_drop_partitions_before('ui_events', 'hour',
                                          v_now - make_interval(hours => p_ui_retention_hours));
    RETURN NEXT;
  END IF;

  IF to_regclass('public.ingest_batches') IS NOT NULL THEN
    parent  := 'ingest_batches'; grain := 'day';
    created := oto_ensure_partitions_ahead('ingest_batches', 'day', 7, v_now);
    dropped := oto_drop_partitions_before('ingest_batches', 'day',
                                          v_now - make_interval(days => p_raw_retention_days));
    RETURN NEXT;
  END IF;

  IF to_regclass('public.ingest_rejections') IS NOT NULL THEN
    parent  := 'ingest_rejections'; grain := 'day';
    created := oto_ensure_partitions_ahead('ingest_rejections', 'day', 7, v_now);
    dropped := oto_drop_partitions_before('ingest_rejections', 'day',
                                          v_now - make_interval(days => p_raw_retention_days));
    RETURN NEXT;
  END IF;

  IF to_regclass('public.alert_events') IS NOT NULL THEN
    parent  := 'alert_events'; grain := 'month';
    created := oto_ensure_partitions_ahead('alert_events', 'month', 3, v_now);
    dropped := oto_drop_partitions_before('alert_events', 'month',
                                          v_now - make_interval(months => p_event_retention_months));
    RETURN NEXT;
  END IF;

  RETURN;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION oto_partitions_manage(int, int, int) IS
  'The entire contract of the hourly `partitions.manage` job (SPEC §G.3, §D.11). Call it as SELECT * FROM oto_partitions_manage(raw_days, event_months, ui_hours), passing the orgs.settings values. Idempotent, safe to run every hour forever. There are deliberately NO DEFAULT partitions: a row that falls outside every range must fail loudly rather than silently accumulate in a bucket that can never be dropped.';

COMMENT ON TABLE ingest_batches IS
  'One raw, durably-recorded webhook body or reconciler sweep. Written inside the short ingest transaction (SPEC §G.1) and processed asynchronously; this table is the reason a 2xx is a promise. DAILY partitions on received_at, retention orgs.settings.raw_retention_days (default 14).';

COMMENT ON TABLE ingest_rejections IS
  'Every observation oto refused to normalise, with the offending element kept. Writing a row here is what makes it legitimate to return 202 for a partially bad payload — a 4xx would make Alertmanager delete the alert forever (SPEC §G.2). We never silently drop.';

COMMENT ON TABLE alert_events IS
  'The append-only timeline: one immutable thing that happened at one instant. Current state everywhere else in this schema is a projection of these rows, never the only record. If you would ever want to UPDATE a row here, it is not an Event. MONTHLY partitions on recorded_at, retention orgs.settings.event_retention_months (default 13).';
