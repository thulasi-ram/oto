-- SPEC §D.11 -- Partition management (binding).
--
-- oto declaratively RANGE-partitions its three high-volume time series:
--
--   ingest_batches      DAILY   on received_at   retention orgs.settings.raw_retention_days   (14)
--   ingest_rejections   DAILY   on received_at   retention orgs.settings.raw_retention_days   (14)
--   alert_events        MONTHLY on recorded_at   retention orgs.settings.event_retention_months (13)
--   ui_events           HOURLY  on at            retention 24 hours
--
-- Naming is binding: <parent>_p<YYYYMMDDHH|YYYYMMDD|YYYYMM>, e.g. alert_events_p202608.
--
-- WHY THIS MATTERS FOR CORRECTNESS, NOT JUST SIZE
-- -----------------------------------------------
-- A UNIQUE index on a partitioned table must contain the partition key, which
-- means it can only ever enforce uniqueness WITHIN one partition -- never
-- globally (SPEC §L, conflict ruling 14). Two idempotency tables therefore
-- exist as deliberately UNPARTITIONED siblings of partitioned tables:
--
--   ingest_dedup      UNIQUE (source_id, dedup_key)  guards webhook replay   (§C.5)
--   alert_event_keys  UNIQUE (org_id, dedupe_key)    guards event append     (§C.8)
--
-- They are small because they are pruned aggressively (10 minutes and 30 days
-- respectively). Do not "optimise" them into their partitioned parents: the
-- uniqueness would silently stop being global and both idempotency mechanisms
-- would quietly fail at a partition boundary, which is precisely the moment a
-- retry storm is most likely.
--
-- RETENTION IS DROP PARTITION, NEVER DELETE. A DELETE on a 13-month event table
-- is an outage.
--
-- The `partitions.manage` maintenance job (SPEC §G.3, hourly) calls
-- oto_partitions_manage(). Nothing else should create partitions by hand.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION oto_partition_name(p_parent text, p_grain text, p_ts timestamptz)
RETURNS text
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT p_parent || '_p' || to_char(
    date_trunc(p_grain, p_ts AT TIME ZONE 'UTC'),
    CASE p_grain
      WHEN 'hour'  THEN 'YYYYMMDDHH24'
      WHEN 'day'   THEN 'YYYYMMDD'
      WHEN 'month' THEN 'YYYYMM'
    END);
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION oto_partition_name(text, text, timestamptz) IS
  'Binding partition naming from SPEC §D.11: <parent>_p<YYYYMMDDHH|YYYYMMDD|YYYYMM>, always computed in UTC.';

-- +goose StatementBegin
CREATE FUNCTION oto_ensure_partition(p_parent text, p_grain text, p_ts timestamptz)
RETURNS text
LANGUAGE plpgsql
AS $$
DECLARE
  v_lo   timestamptz;
  v_hi   timestamptz;
  v_name text;
BEGIN
  IF p_grain NOT IN ('hour','day','month') THEN
    RAISE EXCEPTION 'oto_ensure_partition: unsupported grain %, want hour|day|month', p_grain;
  END IF;

  -- Bounds are computed in UTC so the result does not depend on the session
  -- TimeZone. Every timestamp in oto is UTC (SPEC §D conventions).
  v_lo   := date_trunc(p_grain, p_ts AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
  v_hi   := v_lo + ('1 ' || p_grain)::interval;
  v_name := oto_partition_name(p_parent, p_grain, p_ts);

  IF to_regclass(format('public.%I', v_name)) IS NOT NULL THEN
    RETURN v_name;
  END IF;

  EXECUTE format(
    'CREATE TABLE IF NOT EXISTS public.%I PARTITION OF public.%I FOR VALUES FROM (%L) TO (%L)',
    v_name, p_parent, v_lo, v_hi);

  RETURN v_name;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION oto_ensure_partition(text, text, timestamptz) IS
  'Idempotently create the partition of p_parent covering p_ts at the given grain. Returns the partition name. Safe to call concurrently-ish: the to_regclass fast path plus CREATE TABLE IF NOT EXISTS means a lost race is a no-op, and partitions.manage is a single-worker maintenance queue anyway (SPEC §G.3).';

-- +goose StatementBegin
CREATE FUNCTION oto_ensure_partitions_ahead(p_parent text, p_grain text, p_ahead int,
                                            p_from timestamptz DEFAULT now())
RETURNS int
LANGUAGE plpgsql
AS $$
DECLARE
  i         int;
  v_created int := 0;
BEGIN
  FOR i IN 0..p_ahead LOOP
    PERFORM oto_ensure_partition(p_parent, p_grain, p_from + (i || ' ' || p_grain)::interval);
    v_created := v_created + 1;
  END LOOP;
  RETURN v_created;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION oto_ensure_partitions_ahead(text, text, int, timestamptz) IS
  'Ensure the partition covering p_from plus the next p_ahead periods all exist. Returns how many were considered.';

-- +goose StatementBegin
CREATE FUNCTION oto_drop_partitions_before(p_parent text, p_grain text, p_cutoff timestamptz)
RETURNS int
LANGUAGE plpgsql
AS $$
DECLARE
  v_child   text;
  v_suffix  text;
  v_lo      timestamptz;
  v_hi      timestamptz;
  v_dropped int := 0;
BEGIN
  FOR v_child IN
    SELECT c.relname
      FROM pg_inherits i
      JOIN pg_class c ON c.oid = i.inhrelid
      JOIN pg_class p ON p.oid = i.inhparent
     WHERE p.relname = p_parent
     ORDER BY c.relname
  LOOP
    v_suffix := substring(v_child from '_p([0-9]+)$');
    CONTINUE WHEN v_suffix IS NULL OR length(v_suffix) NOT IN (6, 8, 10);

    -- Reconstruct the lower bound from the binding name format rather than by
    -- parsing pg_get_expr(relpartbound), which is not a stable text contract.
    v_lo := make_timestamptz(
      substr(v_suffix, 1, 4)::int,
      substr(v_suffix, 5, 2)::int,
      coalesce(nullif(substr(v_suffix, 7, 2), ''), '1')::int,
      coalesce(nullif(substr(v_suffix, 9, 2), ''), '0')::int,
      0, 0, 'UTC');
    v_hi := v_lo + ('1 ' || p_grain)::interval;

    -- Only drop a partition whose whole range is already past the cutoff.
    CONTINUE WHEN v_hi > p_cutoff;

    -- DETACH first so a long-running reader on the parent does not take an
    -- ACCESS EXCLUSIVE lock on the parent for the duration of the DROP.
    EXECUTE format('ALTER TABLE public.%I DETACH PARTITION public.%I', p_parent, v_child);
    EXECUTE format('DROP TABLE public.%I', v_child);
    v_dropped := v_dropped + 1;
  END LOOP;

  RETURN v_dropped;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION oto_drop_partitions_before(text, text, timestamptz) IS
  'DETACH + DROP every partition of p_parent whose range ends at or before p_cutoff. This is oto retention: SPEC §D.11 forbids DELETE FROM on a partitioned event table.';

-- +goose StatementBegin
CREATE FUNCTION oto_partitions_manage(p_raw_retention_days      int DEFAULT 14,
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
  'The entire contract of the hourly `partitions.manage` job (SPEC §G.3, §D.11). Call it as SELECT * FROM oto_partitions_manage(raw_days, event_months, ui_hours), passing the orgs.settings values. Idempotent, safe to run every hour forever. There are deliberately NO DEFAULT partitions: a row that falls outside every range must fail loudly rather than silently accumulate in a bucket that can never be dropped.';

-- +goose Down

DROP FUNCTION IF EXISTS oto_partitions_manage(int, int, int);
DROP FUNCTION IF EXISTS oto_drop_partitions_before(text, text, timestamptz);
DROP FUNCTION IF EXISTS oto_ensure_partitions_ahead(text, text, int, timestamptz);
DROP FUNCTION IF EXISTS oto_ensure_partition(text, text, timestamptz);
DROP FUNCTION IF EXISTS oto_partition_name(text, text, timestamptz);
