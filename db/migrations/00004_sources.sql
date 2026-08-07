-- SPEC §D.2 -- Clusters and sources.
--
-- A Cluster is an identity/failure domain and its cluster_key participates in
-- alert identity (SPEC §C.2): the same KubePodCrashLooping label set in prod-eu
-- and prod-us are DIFFERENT Alerts, because they have different blast radii.
--
-- An AlertSource is one configured Alertmanager (plus an optional Prometheus,
-- which is what enables RuleSnapshots). Alertmanager HA replicas are several
-- sources sharing one Cluster.

-- +goose Up

-- ------------------------------------------------------------------ clusters

CREATE TABLE clusters (
  id           UUID        PRIMARY KEY,
  org_id       UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  cluster_key  TEXT        NOT NULL,               -- participates in alert identity (C.2)
  display_name TEXT        NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at   TIMESTAMPTZ,
  CONSTRAINT clusters_key_uniq UNIQUE (org_id, cluster_key),
  CONSTRAINT clusters_key_ck   CHECK (cluster_key ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
  CONSTRAINT clusters_name_ck  CHECK (length(btrim(display_name)) BETWEEN 1 AND 120),
  CONSTRAINT clusters_time_ck  CHECK (updated_at >= created_at)
);

COMMENT ON TABLE  clusters IS
  'An identity and failure domain. Alertmanager HA replicas share one Cluster (SPEC §D.2).';
COMMENT ON COLUMN clusters.cluster_key IS
  'The stable machine name hashed into alert_key (SPEC §C.2). Changing it re-keys every future Alert, so it is immutable in practice.';
COMMENT ON COLUMN clusters.display_name IS 'Human label for the UI. Never hashed, safe to rename.';

-- ------------------------------------------------------------- alert_sources

CREATE TABLE alert_sources (
  id                 UUID        PRIMARY KEY,
  org_id             UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  cluster_id         UUID        NOT NULL REFERENCES clusters(id),
  name               CITEXT      NOT NULL,
  kind               TEXT        NOT NULL,
  base_url           TEXT        NOT NULL,         -- Alertmanager root, no trailing slash
  prometheus_url     TEXT,                         -- optional; enables rule snapshots
  auth_credential_id UUID,                         -- FK added in 00011 (channel_credentials reused)
  tls_skip_verify    BOOLEAN     NOT NULL DEFAULT false,
  inject_labels      JSONB       NOT NULL DEFAULT '{}'::jsonb,
  ignore_labels      TEXT[]      NOT NULL DEFAULT ARRAY['prometheus_replica','__replica__','monitor','replica','pod_template_hash'],
  redact_labels      TEXT[]      NOT NULL DEFAULT '{}',
  redact_annotations TEXT[]      NOT NULL DEFAULT '{}',
  push_enabled       BOOLEAN     NOT NULL DEFAULT true,
  reconcile_enabled  BOOLEAN     NOT NULL DEFAULT true,
  reconcile_interval_s INT       NOT NULL DEFAULT 30,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at         TIMESTAMPTZ,
  CONSTRAINT alert_sources_name_uniq UNIQUE (org_id, name),
  -- Inline and unnamed in SPEC §D.2; named here so 23514 carries a stable label.
  CONSTRAINT alert_sources_kind_ck    CHECK (kind IN ('alertmanager','grafana')),
  CONSTRAINT alert_sources_ivl_min_ck CHECK (reconcile_interval_s >= 10),
  CONSTRAINT alert_sources_name_ck    CHECK (length(btrim(name::text)) BETWEEN 1 AND 120),
  CONSTRAINT alert_sources_base_ck    CHECK (base_url ~ '^https?://[^[:space:]]+$' AND base_url NOT LIKE '%/'),
  CONSTRAINT alert_sources_prom_ck    CHECK (prometheus_url IS NULL OR
                                             (prometheus_url ~ '^https?://[^[:space:]]+$' AND prometheus_url NOT LIKE '%/')),
  CONSTRAINT alert_sources_inject_ck  CHECK (jsonb_typeof(inject_labels) = 'object'),
  CONSTRAINT alert_sources_ignore_ck  CHECK (array_position(ignore_labels, NULL) IS NULL
                                             AND coalesce(array_length(ignore_labels, 1), 0) <= 64),
  CONSTRAINT alert_sources_redactl_ck CHECK (coalesce(array_length(redact_labels, 1), 0) <= 64),
  CONSTRAINT alert_sources_redacta_ck CHECK (coalesce(array_length(redact_annotations, 1), 0) <= 64),
  CONSTRAINT alert_sources_ivl_ck     CHECK (reconcile_interval_s <= 3600),
  CONSTRAINT alert_sources_time_ck    CHECK (updated_at >= created_at)
);

-- Serves: the sources screen and the reconciler's per-cluster fan-out.
CREATE INDEX alert_sources_cluster_idx ON alert_sources (org_id, cluster_id) WHERE deleted_at IS NULL;

COMMENT ON TABLE  alert_sources IS
  'One configured Alertmanager (optionally paired with a Prometheus). The unit that ingest tokens are scoped to, that the reconciler polls, and that group_key is salted with (SPEC §C.4).';
COMMENT ON COLUMN alert_sources.kind IS 'alertmanager | grafana. Grafana sends an Alertmanager-compatible superset, decoded leniently (SPEC §L.3).';
COMMENT ON COLUMN alert_sources.base_url IS 'Alertmanager API root with no trailing slash, e.g. https://am.example.com.';
COMMENT ON COLUMN alert_sources.prometheus_url IS 'Optional Prometheus root. Its presence is what enables origin=prometheus_api RuleSnapshots (SPEC §D.6).';
COMMENT ON COLUMN alert_sources.auth_credential_id IS 'Reuses channel_credentials as the generic sealed-secret store. FK added in 00011 once that table exists.';
COMMENT ON COLUMN alert_sources.inject_labels IS 'Labels merged into every observation from this source before alert_key is computed. Lets one AM serve several logical clusters.';
COMMENT ON COLUMN alert_sources.ignore_labels IS 'Label names excluded from the alert_key hash (SPEC §C.1 canon ignore set). Ignored labels are still STORED on alerts.labels -- they are merely not hashed. Changing this does not re-key existing Alerts (§C.2).';
COMMENT ON COLUMN alert_sources.redact_labels IS 'Glob patterns applied BEFORE the raw batch is persisted, so sensitive values never land on disk (SPEC §C.9.2).';
COMMENT ON COLUMN alert_sources.redact_annotations IS 'As redact_labels, for annotation values.';
COMMENT ON COLUMN alert_sources.push_enabled IS 'Whether the webhook endpoint accepts pushes for this source.';
COMMENT ON COLUMN alert_sources.reconcile_enabled IS 'Whether source.reconcile polls /api/v2/alerts. The reconciler is the ONLY producer of state=suppressed (SPEC §G.8).';
COMMENT ON COLUMN alert_sources.reconcile_interval_s IS 'Reconciler period in seconds, 10..3600, default 30.';

-- api_tokens.source_id was declared in 00003 without a FK because alert_sources
-- did not exist yet. Close the loop now (SPEC §D.2).
ALTER TABLE api_tokens
  ADD CONSTRAINT api_tokens_source_fk FOREIGN KEY (source_id) REFERENCES alert_sources(id) ON DELETE CASCADE;

-- ------------------------------------------------------------- source_health

CREATE TABLE source_health (
  source_id             UUID        PRIMARY KEY REFERENCES alert_sources(id) ON DELETE CASCADE,
  org_id                UUID        NOT NULL,
  status                TEXT        NOT NULL DEFAULT 'unknown',
  last_push_at          TIMESTAMPTZ,
  last_reconcile_at     TIMESTAMPTZ,
  last_reconcile_status TEXT,
  last_error            TEXT,
  consecutive_failures  INT         NOT NULL DEFAULT 0,
  am_version            TEXT,
  send_resolved         BOOLEAN,                    -- from GET /api/v2/status; NULL = unknown
  clock_skew_ms         BIGINT      NOT NULL DEFAULT 0,   -- observed_at - source_ts, EWMA
  divergence_count      INT         NOT NULL DEFAULT 0,   -- reconciler disagreements last run
  warnings              JSONB       NOT NULL DEFAULT '[]'::jsonb,
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Inline and unnamed in SPEC §D.2; named here.
  CONSTRAINT source_health_status_ck CHECK (status IN ('healthy','degraded','unreachable','unknown')),
  CONSTRAINT source_health_fail_ck  CHECK (consecutive_failures >= 0),
  CONSTRAINT source_health_div_ck   CHECK (divergence_count >= 0),
  CONSTRAINT source_health_warn_ck  CHECK (jsonb_typeof(warnings) = 'array'),
  CONSTRAINT source_health_error_ck CHECK (status <> 'unreachable' OR last_error IS NOT NULL)
);

-- Serves: the reaper guard (SPEC §B.4) -- the reaper is BLOCKED while any
-- relevant source is not healthy, so this is read on every reap cycle.
CREATE INDEX source_health_status_idx ON source_health (org_id, status);

COMMENT ON TABLE  source_health IS
  'Liveness projection for one AlertSource. status <> healthy BLOCKS the reaper (SPEC §B.4) -- losing sight of an alert is not the alert resolving. One row per source, updated in place; it is a projection, not an event.';
COMMENT ON COLUMN source_health.org_id IS 'Denormalised from alert_sources so the reaper guard can be answered without a join. No FK by SPEC §D.2.';
COMMENT ON COLUMN source_health.status IS 'healthy | degraded | unreachable | unknown. Reaches unreachable after 3 consecutive failures (SPEC §G.8.5).';
COMMENT ON COLUMN source_health.last_reconcile_status IS 'Free-text outcome of the last source.reconcile run, for the sources screen.';
COMMENT ON COLUMN source_health.am_version IS 'Alertmanager build version from GET /api/v2/status. Gates notification_reason handling (AM >= 0.32.0).';
COMMENT ON COLUMN source_health.send_resolved IS 'Whether matching receivers have send_resolved enabled. false means oto will only ever see firing and must expire rather than resolve. NULL = not yet observed.';
COMMENT ON COLUMN source_health.clock_skew_ms IS 'EWMA of observed_at minus the upstream timestamp. Measured, never used to reject an event (SPEC C12).';
COMMENT ON COLUMN source_health.divergence_count IS 'Occurrences where oto and Alertmanager disagreed on the last reconcile. The canary for every correctness bug in the system (SPEC §G.8.4).';
COMMENT ON COLUMN source_health.warnings IS 'JSON array of structured operator warnings, e.g. send_resolved_false (SPEC C15).';

-- +goose Down

DROP TABLE IF EXISTS source_health;
ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_source_fk;
DROP TABLE IF EXISTS alert_sources;
DROP TABLE IF EXISTS clusters;
