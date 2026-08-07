-- SPEC §D.7 -- Enrichment.
--
-- An Enrichment is ONE TYPED, PROVENANCED RESULT from ONE NAMED, VERSIONED
-- Enricher. Provenance is the point: a result that cannot say who computed it,
-- at which version, from cache or not, and whether it succeeded, is a rumour.
--
-- Enrichment is NEVER on the ingest critical path (CONTEXT.md §2, commitment 2).

-- +goose Up

-- --------------------------------------------------------------- enrichments

CREATE TABLE enrichments (
  id               UUID        PRIMARY KEY,
  org_id           UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  subject_kind     TEXT        NOT NULL,
  subject_id       UUID        NOT NULL,
  enricher         TEXT        NOT NULL,
  enricher_version INT         NOT NULL,
  phase            SMALLINT    NOT NULL,
  status           TEXT        NOT NULL,
  payload          JSONB       NOT NULL DEFAULT '{}'::jsonb,
  warnings         TEXT[]      NOT NULL DEFAULT '{}',
  error            TEXT,
  duration_ms      INT         NOT NULL DEFAULT 0,
  from_cache       BOOLEAN     NOT NULL DEFAULT false,
  computed_at      TIMESTAMPTZ NOT NULL,
  expires_at       TIMESTAMPTZ,
  CONSTRAINT enrichments_subject_uniq UNIQUE (subject_kind, subject_id, enricher),
  -- Inline and unnamed in SPEC §D.7; named here.
  CONSTRAINT enrichments_subjkind_ck CHECK (subject_kind IN ('alert','occurrence','group')),
  CONSTRAINT enrichments_phase_ck    CHECK (phase IN (1,2)),
  CONSTRAINT enrichments_status_ck   CHECK (status IN ('ok','partial','skipped','failed','timeout')),
  CONSTRAINT enrichments_name_ck    CHECK (enricher ~ '^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$'),
  CONSTRAINT enrichments_ver_ck     CHECK (enricher_version >= 1),
  CONSTRAINT enrichments_dur_ck     CHECK (duration_ms >= 0),
  CONSTRAINT enrichments_payload_ck CHECK (jsonb_typeof(payload) = 'object'),
  CONSTRAINT enrichments_err_ck     CHECK (status NOT IN ('failed','timeout') OR error IS NOT NULL),
  CONSTRAINT enrichments_exp_ck     CHECK (expires_at IS NULL OR expires_at > computed_at)
);

-- Serves: "everything enriched about this subject", read when rendering an
-- alert detail page or building a NotificationView.
CREATE INDEX enr_subject_idx ON enrichments (org_id, subject_kind, subject_id);

COMMENT ON TABLE  enrichments IS
  'One typed, provenanced result from one named, versioned Enricher (SPEC §F.3). A failed or timed-out enrichment is RECORDED, not discarded -- a missing enrichment and a failed one must be distinguishable in the UI. Never on the ingest critical path.';
COMMENT ON COLUMN enrichments.subject_kind IS 'What was enriched: alert | occurrence | group.';
COMMENT ON COLUMN enrichments.subject_id IS 'The subject row id. Polymorphic, so no FK -- subject_kind names the table.';
COMMENT ON COLUMN enrichments.enricher IS 'Dotted registry name of the Enricher, e.g. rules.drift. One result per (subject, enricher) by enrichments_subject_uniq.';
COMMENT ON COLUMN enrichments.enricher_version IS 'Version of the Enricher that produced this. Bumping it is how a result is invalidated.';
COMMENT ON COLUMN enrichments.phase IS 'Budgeted pipeline phase: 1 = inline/fast (blocks the first notification), 2 = background (arrives as a thread update).';
COMMENT ON COLUMN enrichments.status IS 'ok | partial | skipped | failed | timeout. failed and timeout REQUIRE an error string (enrichments_err_ck).';
COMMENT ON COLUMN enrichments.warnings IS 'Non-fatal notes from the Enricher, surfaced in the UI alongside the result.';
COMMENT ON COLUMN enrichments.duration_ms IS 'Wall time spent. Feeds the pipeline budget, which is why it is recorded even on failure.';
COMMENT ON COLUMN enrichments.from_cache IS 'Whether this result was served from enrichment_cache rather than recomputed. Part of provenance.';
COMMENT ON COLUMN enrichments.expires_at IS 'When the result should be considered stale. NULL means it never goes stale on its own.';

-- ---------------------------------------------------------- enrichment_cache

CREATE TABLE enrichment_cache (
  cache_key   TEXT        PRIMARY KEY,
  org_id      UUID        NOT NULL,
  payload     JSONB       NOT NULL,
  computed_at TIMESTAMPTZ NOT NULL,
  expires_at  TIMESTAMPTZ NOT NULL,
  CONSTRAINT enrichment_cache_key_ck CHECK (length(cache_key) BETWEEN 1 AND 512),
  CONSTRAINT enrichment_cache_exp_ck CHECK (expires_at > computed_at)
);

-- Serves: the cache.expire maintenance job (SPEC §G.3, every 600s). Lookups ride
-- the PRIMARY KEY on cache_key.
CREATE INDEX enr_cache_exp_idx ON enrichment_cache (expires_at);

COMMENT ON TABLE  enrichment_cache IS
  'Shared, expiring result cache for Enrichers, keyed by an Enricher-computed cache_key. Distinct from enrichments: this table is disposable, that one is the provenanced record. Swept by the cache.expire job.';
COMMENT ON COLUMN enrichment_cache.cache_key IS 'Enricher-defined cache identity, e.g. enricher name plus a hash of its inputs. Globally unique, hence the PRIMARY KEY.';
COMMENT ON COLUMN enrichment_cache.org_id IS 'Tenant that populated the entry. No FK: this is a disposable cache and must not participate in a delete cascade.';
COMMENT ON COLUMN enrichment_cache.expires_at IS 'Hard expiry, strictly after computed_at. Rows past it are deleted, never served.';

-- +goose Down

DROP TABLE IF EXISTS enrichment_cache;
DROP TABLE IF EXISTS enrichments;
