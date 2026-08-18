-- SPEC §D.10 (first half) -- Streaming.
--
-- ui_events is the SSE spine. A writer inserts here and issues
-- NOTIFY oto_events, '<org_id>:<seq>' IN THE SAME TRANSACTION; each API pod holds
-- one dedicated connection on LISTEN oto_events and re-reads the seq range
-- (SPEC §E.4). The NOTIFY payload is deliberately tiny -- the 8 kB NOTIFY limit
-- is a trap, so the envelope travels through the table, not the notification.
--
-- HOURLY RANGE partitions on `at`, 24-hour retention. That retention is itself
-- part of the API contract: §E.4 replays only `at >= now() - interval '24 hours'`
-- and sends a `resync` frame rather than a partial replay beyond it.

-- +goose Up

CREATE TABLE ui_events (
  seq         BIGSERIAL   NOT NULL,                -- monotonic; the SSE Last-Event-ID
  org_id      UUID        NOT NULL,
  kind        TEXT        NOT NULL,                -- §E.4
  resource    TEXT        NOT NULL,
  resource_id UUID        NOT NULL,
  payload     JSONB       NOT NULL,                -- SMALL envelope; client re-reads for detail
  at          TIMESTAMPTZ NOT NULL DEFAULT now(),  -- PARTITION KEY
  PRIMARY KEY (seq, at),
  CONSTRAINT ui_events_kind_ck    CHECK (kind IN
    ('alert.upserted','occurrence.upserted','group.upserted','event.appended',  -- vocab:allow -- migration history: this file states the schema as it was at its own version, and a shipped migration is not editable. ADR 0036 renamed the entity and 00052 carries the rename.
     'delivery.updated','source.health')),
  CONSTRAINT ui_events_res_ck     CHECK (resource IN
    ('alert','occurrence','group','alert_event','delivery','source')),  -- vocab:allow -- migration history: this file states the schema as it was at its own version, and a shipped migration is not editable. ADR 0036 renamed the entity and 00052 carries the rename.
  CONSTRAINT ui_events_payload_ck CHECK (jsonb_typeof(payload) = 'object'
                                         AND pg_column_size(payload) <= 4096)
) PARTITION BY RANGE (at);

-- Serves: the SSE RESUME READ (SPEC §E.4) --
--   SELECT ... FROM ui_events
--    WHERE org_id = $1 AND seq > $2 AND at >= now() - interval '24 hours'
--    ORDER BY seq ASC
-- org_id leads because a client can never widen past its own tenant, and seq
-- follows so the replay is an index-ordered range scan from the client cursor.
CREATE INDEX ui_ev_org_idx ON ui_events (org_id, seq);

COMMENT ON TABLE  ui_events IS
  'The SSE spine: a small, ordered envelope per user-visible change. HOURLY partitions on `at`, 24-HOUR RETENTION -- which is exactly the replay window §E.4 promises; a gap older than that gets a `resync` frame, never a partial replay. Clients MUST NOT assume seq is contiguous.';
COMMENT ON COLUMN ui_events.seq IS
  'Monotonic BIGSERIAL, surfaced as the SSE id: and Last-Event-ID header. Strictly increasing, NOT contiguous -- a rolled-back transaction consumes a value. Note the PRIMARY KEY must include the partition key `at`, so its uniqueness is per-partition; global monotonicity comes from the SEQUENCE, not the index.';
COMMENT ON COLUMN ui_events.kind IS 'Closed enum from §E.4: alert.upserted | occurrence.upserted | group.upserted | event.appended | delivery.updated | source.health. The SSE event: name.';  -- vocab:allow -- migration history: this file states the schema as it was at its own version, and a shipped migration is not editable. ADR 0036 renamed the entity and 00052 carries the rename.
COMMENT ON COLUMN ui_events.resource IS 'Which resource the id refers to, so a client knows which endpoint to re-read.';
COMMENT ON COLUMN ui_events.resource_id IS 'The changed row. The client re-reads for detail -- this table carries the SIGNAL, not the payload.';
COMMENT ON COLUMN ui_events.payload IS 'SMALL envelope, hard-capped at 4096 bytes on disk by ui_events_payload_ck. Just enough to update a list row without a refetch.';
COMMENT ON COLUMN ui_events.at IS 'oto clock. PARTITION KEY, and the lower bound of the §E.4 replay window.';

-- Initial partitions: the current hour plus the next 6, per SPEC §D.11.
SELECT oto_ensure_partitions_ahead('ui_events', 'hour', 6);

-- +goose Down

DROP TABLE IF EXISTS ui_events;   -- drops its partitions and the BIGSERIAL sequence
