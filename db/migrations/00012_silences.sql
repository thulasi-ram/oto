-- SPEC §D.9 -- Silences (read-only mirror).
--
-- A Silence is a READ-ONLY MIRROR of an Alertmanager silence. oto has NO WRITE
-- PATH into your cluster (SPEC R3, CONTEXT.md §4): this table is populated
-- exclusively by the silences.sync job and is never the source of truth.
--
-- Note that a silenced alert is INVISIBLE to webhooks -- Alertmanager MuteStage
-- drops muted alerts before the webhook fires -- so this mirror plus the API v2
-- reconciler is the ONLY way oto can know an alert is suppressed (SPEC §B.2).

-- +goose Up

CREATE TABLE silences (
  id            UUID        PRIMARY KEY,
  org_id        UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  source_id     UUID        NOT NULL REFERENCES alert_sources(id) ON DELETE CASCADE,
  source_silence_id TEXT    NOT NULL,              -- Alertmanager's silence UUID
  matchers      JSONB       NOT NULL,              -- [{name,value,isRegex,isEqual}]
  starts_at     TIMESTAMPTZ NOT NULL,
  ends_at       TIMESTAMPTZ NOT NULL,
  created_by    TEXT        NOT NULL,
  comment       TEXT        NOT NULL,
  annotations   JSONB       NOT NULL DEFAULT '{}'::jsonb,   -- AM >= 0.32.0
  state         TEXT        NOT NULL,
  source_updated_at TIMESTAMPTZ,
  mirrored_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT silences_source_uniq UNIQUE (source_id, source_silence_id),
  -- Inline and unnamed in SPEC §D.9; named here.
  CONSTRAINT silences_state_ck    CHECK (state IN ('active','pending','expired')),
  CONSTRAINT silences_srcid_ck    CHECK (length(btrim(source_silence_id)) BETWEEN 1 AND 128),
  CONSTRAINT silences_match_ck    CHECK (jsonb_typeof(matchers) = 'array' AND jsonb_array_length(matchers) >= 1),
  CONSTRAINT silences_annot_ck    CHECK (jsonb_typeof(annotations) = 'object'),
  CONSTRAINT silences_order_ck    CHECK (ends_at > starts_at),
  CONSTRAINT silences_by_ck       CHECK (length(btrim(created_by)) > 0)
);

-- Serves: "what is currently silencing this alert" -- the live and upcoming
-- silences, ordered by when they lapse. Partial because the expired tail is
-- large and only ever read for history.
CREATE INDEX silences_active_idx ON silences (org_id, state, ends_at) WHERE state IN ('active','pending');

COMMENT ON TABLE  silences IS
  'READ-ONLY MIRROR of Alertmanager silences, refreshed by the silences.sync job every 60s. oto has NO WRITE PATH into your cluster (SPEC R3) -- creating or expiring a silence here would not affect Alertmanager and is deliberately impossible.';
COMMENT ON COLUMN silences.source_silence_id IS 'Alertmanager own silence id. Natural key together with source_id (silences_source_uniq).';
COMMENT ON COLUMN silences.matchers IS 'Alertmanager matcher array [{name,value,isRegex,isEqual}], at least one. Mirrored verbatim.';
COMMENT ON COLUMN silences.starts_at IS 'Upstream start. ends_at is strictly after it (silences_order_ck).';
COMMENT ON COLUMN silences.created_by IS 'Whoever created the silence in Alertmanager. Non-blank, mirrored verbatim, not an oto user.';
COMMENT ON COLUMN silences.comment IS 'The silence justification from Alertmanager. Shown wherever oto explains why something is suppressed.';
COMMENT ON COLUMN silences.annotations IS 'Alertmanager >= 0.32.0 silence annotations. Empty object on older versions.';
COMMENT ON COLUMN silences.state IS 'active | pending | expired, as reported upstream. oto never computes it from the clock -- the mirror mirrors.';
COMMENT ON COLUMN silences.source_updated_at IS 'Upstream updatedAt, used to detect a changed silence between syncs.';
COMMENT ON COLUMN silences.mirrored_at IS 'oto clock at the last successful sync of this row. Staleness indicator for the UI.';

-- +goose Down

DROP TABLE IF EXISTS silences;
