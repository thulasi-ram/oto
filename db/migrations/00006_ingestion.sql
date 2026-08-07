-- SPEC §D.3 -- Ingestion (raw, partitioned, short retention).
--
-- The webhook transaction touches exactly these tables plus the River queue
-- (SPEC §G.1). Nothing here makes an outbound network call, and a 2xx is a
-- promise that the payload is on disk.
--
-- ingest_batches and ingest_rejections are DAILY RANGE partitions on
-- received_at. ingest_dedup is deliberately UNPARTITIONED -- see 00005 and
-- SPEC conflict ruling 14.

-- +goose Up

-- ------------------------------------------------------------ ingest_batches

CREATE TABLE ingest_batches (
  id                  UUID        NOT NULL,
  org_id              UUID        NOT NULL,
  source_id           UUID        NOT NULL,
  mode                TEXT        NOT NULL,
  received_at         TIMESTAMPTZ NOT NULL,
  body_bytes          INT         NOT NULL,
  checksum            BYTEA       NOT NULL,        -- sha256 of the raw body
  dedup_key           TEXT        NOT NULL,        -- C.5
  am_version          TEXT,                        -- payload "version" field, literal "4"
  group_key           TEXT,                        -- AM's raw groupKey, opaque
  receiver            TEXT,
  notification_reason TEXT,                        -- AM >= 0.32.0; "" when absent
  status_top          TEXT,                        -- firing | resolved
  alert_count         INT         NOT NULL,
  truncated_alerts    INT         NOT NULL DEFAULT 0,
  payload             JSONB       NOT NULL,        -- REDACTED per source config
  status              TEXT        NOT NULL DEFAULT 'pending',
  processed_at        TIMESTAMPTZ,
  error               TEXT,
  PRIMARY KEY (id, received_at),
  -- Inline and unnamed in SPEC §D.3; named here.
  CONSTRAINT ingest_batches_mode_ck     CHECK (mode IN ('push','reconcile')),
  CONSTRAINT ingest_batches_state_ck    CHECK (status IN ('pending','processed','partial','failed')),
  CONSTRAINT ingest_batches_bytes_ck    CHECK (body_bytes > 0 AND body_bytes <= 8388608),
  CONSTRAINT ingest_batches_checksum_ck CHECK (octet_length(checksum) = 32),
  CONSTRAINT ingest_batches_dedup_ck    CHECK (dedup_key ~ '^[0-9a-f]{64}$'),
  CONSTRAINT ingest_batches_count_ck    CHECK (alert_count >= 0 AND alert_count <= 10000),
  CONSTRAINT ingest_batches_trunc_ck    CHECK (truncated_alerts >= 0),
  CONSTRAINT ingest_batches_status_ck   CHECK (status_top IS NULL OR status_top IN ('firing','resolved')),
  CONSTRAINT ingest_batches_payload_ck  CHECK (jsonb_typeof(payload) = 'object'),
  CONSTRAINT ingest_batches_proc_ck     CHECK ((status IN ('processed','partial','failed')) = (processed_at IS NOT NULL)),
  CONSTRAINT ingest_batches_procts_ck   CHECK (processed_at IS NULL OR processed_at >= received_at),
  CONSTRAINT ingest_batches_err_ck      CHECK (status <> 'failed' OR error IS NOT NULL)
) PARTITION BY RANGE (received_at);

-- Serves: the ingest.process_batch worker picking up unfinished work, and the
-- operator's "what is stuck" view. Partial so the index stays tiny -- the
-- overwhelming majority of rows are 'processed'.
CREATE INDEX ingest_batches_status_idx ON ingest_batches (status, received_at)
  WHERE status IN ('pending','failed','partial');
-- Serves: the per-source ingest history screen, newest first.
CREATE INDEX ingest_batches_source_idx ON ingest_batches (org_id, source_id, received_at DESC);

COMMENT ON TABLE  ingest_batches IS
  'One raw, durably-recorded webhook body or reconciler sweep. Written inside the short ingest transaction (SPEC §G.1) and processed asynchronously; this table is the reason a 2xx is a promise. DAILY partitions on received_at, retention orgs.settings.raw_retention_days (default 14).';
COMMENT ON COLUMN ingest_batches.mode IS 'push = an Alertmanager webhook. reconcile = a GET /api/v2/alerts sweep. Both produce Observations for the same state machine (SPEC C18).';
COMMENT ON COLUMN ingest_batches.received_at IS 'oto clock at accept time. PARTITION KEY.';
COMMENT ON COLUMN ingest_batches.checksum IS 'sha256 of the raw request body, exactly 32 bytes. Distinct from dedup_key: the checksum identifies bytes, dedup_key identifies meaning.';
COMMENT ON COLUMN ingest_batches.dedup_key IS 'batch_dedup_key from SPEC §C.5. The replay-suppression identity; its uniqueness is enforced in ingest_dedup, not here.';
COMMENT ON COLUMN ingest_batches.group_key IS 'Alertmanager raw groupKey, stored verbatim for observability. OPAQUE -- MUST NOT be parsed (SPEC §C.4). oto computes its own group_key.';
COMMENT ON COLUMN ingest_batches.notification_reason IS 'Alertmanager >= 0.32.0 notification_reason. Empty string when the field is absent.';
COMMENT ON COLUMN ingest_batches.status_top IS 'The batch-level status field of the webhook payload, firing or resolved. Per-alert status is what actually drives the state machine.';
COMMENT ON COLUMN ingest_batches.truncated_alerts IS 'How many alerts the source said it dropped from this payload.';
COMMENT ON COLUMN ingest_batches.payload IS 'The raw body AFTER redaction (SPEC §C.9.2). Redaction happens before persistence so sensitive label values never reach disk.';
COMMENT ON COLUMN ingest_batches.status IS 'pending | processed | partial | failed. partial means a >2000-alert batch is mid-chunking (SPEC §G.4).';

-- --------------------------------------------------------- ingest_rejections

CREATE TABLE ingest_rejections (
  id           UUID        NOT NULL,
  org_id       UUID        NOT NULL,
  source_id    UUID        NOT NULL,
  batch_id     UUID,
  received_at  TIMESTAMPTZ NOT NULL,
  reason       TEXT        NOT NULL,
  detail       TEXT,
  raw          JSONB       NOT NULL,
  PRIMARY KEY (id, received_at),
  CONSTRAINT ingest_rejections_reason_ck CHECK (reason IN
    ('too_many_labels','label_value_too_large','label_name_too_large','labelset_too_large',
     'too_many_annotations','annotation_too_large','missing_alertname','invalid_label_name',
     'timestamp_out_of_window','too_many_alerts','body_too_large','undecodable','unknown_source'))
) PARTITION BY RANGE (received_at);

-- Serves: the per-source rejection feed, newest first. This screen is the whole
-- point of the table -- oto never silently drops (SPEC §C.9.1).
CREATE INDEX ingest_rejections_source_idx ON ingest_rejections (org_id, source_id, received_at DESC);

COMMENT ON TABLE  ingest_rejections IS
  'Every observation oto refused to normalise, with the offending element kept. Writing a row here is what makes it legitimate to return 202 for a partially bad payload -- a 4xx would make Alertmanager delete the alert forever (SPEC §G.2). We never silently drop.';
COMMENT ON COLUMN ingest_rejections.batch_id IS 'The batch this element came from. NULL when the whole body was undecodable or the source was unknown, i.e. when no batch row exists.';
COMMENT ON COLUMN ingest_rejections.reason IS 'Closed enum matching the hard caps in SPEC §C.9.1 and the bounds checks B1-B17 in §L.3. Also the oto_ingest_rejected_total{reason} label.';
COMMENT ON COLUMN ingest_rejections.detail IS 'Human-readable specifics, e.g. which label exceeded which cap.';
COMMENT ON COLUMN ingest_rejections.raw IS 'The rejected element itself, post-redaction, so an operator can see exactly what arrived.';
COMMENT ON COLUMN ingest_rejections.received_at IS 'oto clock at accept time. PARTITION KEY.';

-- -------------------------------------------------------------- ingest_dedup

-- UNPARTITIONED. This is where webhook replay suppression actually works (C14).
-- A UNIQUE index on a partitioned table can only enforce uniqueness within a
-- partition; Alertmanager HA is at-least-once BY DESIGN and a network partition
-- guarantees duplicates, so this constraint has to be global. Keeping it in a
-- small, aggressively-pruned side table is the price of that guarantee.
CREATE TABLE ingest_dedup (
  source_id  UUID        NOT NULL,
  dedup_key  TEXT        NOT NULL,
  batch_id   UUID        NOT NULL,
  seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (source_id, dedup_key),
  CONSTRAINT ingest_dedup_key_ck CHECK (dedup_key ~ '^[0-9a-f]{64}$')
);

-- Serves: the 10-minute pruner. The dedupe lookup itself rides the PRIMARY KEY
-- (source_id, dedup_key) -- an INSERT ... ON CONFLICT DO NOTHING, never a
-- read-then-write race (SPEC §G.1).
CREATE INDEX ingest_dedup_prune_idx ON ingest_dedup (seen_at);

COMMENT ON TABLE  ingest_dedup IS
  'Webhook replay suppression (SPEC §C.5). DELIBERATELY UNPARTITIONED: a unique index on a partitioned table cannot enforce global uniqueness (conflict ruling 14), and this uniqueness must be global or an HA Alertmanager will double-record a batch across a partition boundary. On conflict the handler returns 202 with the ORIGINAL batch_id and does nothing else. Pruned at seen_at < now() - 10 minutes.';
COMMENT ON COLUMN ingest_dedup.dedup_key IS 'batch_dedup_key, 64 lowercase hex chars (SPEC §C.5).';
COMMENT ON COLUMN ingest_dedup.batch_id IS 'The batch that won the race. Returned verbatim to the duplicate caller so the response is stable across retries.';
COMMENT ON COLUMN ingest_dedup.seen_at IS 'Prune horizon. 10 minutes comfortably exceeds n_peers x cluster.peer-timeout (45s for a 3-node cluster).';

-- Initial partitions: today plus the next 7 days, per SPEC §D.11. From here on
-- the hourly `partitions.manage` job owns this (SELECT * FROM oto_partitions_manage()).
SELECT oto_ensure_partitions_ahead('ingest_batches',    'day', 7);
SELECT oto_ensure_partitions_ahead('ingest_rejections', 'day', 7);

-- +goose Down

DROP TABLE IF EXISTS ingest_dedup;
DROP TABLE IF EXISTS ingest_rejections;   -- drops its partitions with it
DROP TABLE IF EXISTS ingest_batches;      -- drops its partitions with it
