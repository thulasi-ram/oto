-- SPEC §D.10 (second half) -- Rate limiting and alert-hygiene stats.

-- +goose Up

-- -------------------------------------------------------- rate_limit_buckets

CREATE TABLE rate_limit_buckets (
  bucket_key  TEXT        PRIMARY KEY,             -- "slack:{team_id}:{channel_id}"
  tokens      DOUBLE PRECISION NOT NULL,
  capacity    DOUBLE PRECISION NOT NULL,
  refill_per_s DOUBLE PRECISION NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT rate_limit_key_ck    CHECK (length(bucket_key) BETWEEN 1 AND 256),
  CONSTRAINT rate_limit_tokens_ck CHECK (tokens >= 0 AND tokens <= capacity),
  CONSTRAINT rate_limit_cap_ck    CHECK (capacity > 0),
  CONSTRAINT rate_limit_refill_ck CHECK (refill_per_s > 0)
);

COMMENT ON TABLE  rate_limit_buckets IS
  'Postgres token buckets that gate outbound provider calls BEFORE they are made (SPEC §G.6). Shared across worker pods, which is why it lives in the database rather than in process memory. Keyed, not tenant-scoped: the limit belongs to the provider conversation, not to the org.';
COMMENT ON COLUMN rate_limit_buckets.bucket_key IS
  'Bucket identity. slack:{team_id}:{channel_id} at capacity 3, refill 1/s for chat.postMessage. update_root bypasses that and uses slack:{team_id}:update at 45/min -- Tier 3 with headroom below 50.';
COMMENT ON COLUMN rate_limit_buckets.tokens IS 'Current balance, never negative and never above capacity (rate_limit_tokens_ck).';
COMMENT ON COLUMN rate_limit_buckets.capacity IS 'Burst size, strictly positive.';
COMMENT ON COLUMN rate_limit_buckets.refill_per_s IS 'Steady-state rate, strictly positive.';
COMMENT ON COLUMN rate_limit_buckets.updated_at IS 'Last refill instant. Tokens are computed lazily from the elapsed time since this.';

-- ------------------------------------------------------- alert_quality_daily

-- Alert-hygiene accounting. TEAM/ALERT SCOPED ONLY. NEVER PER-PERSON (R8).
CREATE TABLE alert_quality_daily (
  org_id            UUID        NOT NULL,
  day               DATE        NOT NULL,
  cluster_key       TEXT        NOT NULL,
  alertname         TEXT        NOT NULL,
  occurrences       INT         NOT NULL DEFAULT 0,
  notifications     INT         NOT NULL DEFAULT 0,
  deliveries        INT         NOT NULL DEFAULT 0,
  acked_occurrences INT         NOT NULL DEFAULT 0,
  auto_resolved     INT         NOT NULL DEFAULT 0,
  expired           INT         NOT NULL DEFAULT 0,
  total_firing_seconds BIGINT   NOT NULL DEFAULT 0,
  flap_transitions  INT         NOT NULL DEFAULT 0,
  PRIMARY KEY (org_id, day, cluster_key, alertname),
  CONSTRAINT alert_quality_nonneg_ck CHECK (
      occurrences >= 0 AND notifications >= 0 AND deliveries >= 0 AND acked_occurrences >= 0
      AND auto_resolved >= 0 AND expired >= 0 AND total_firing_seconds >= 0 AND flap_transitions >= 0),
  CONSTRAINT alert_quality_acked_ck  CHECK (acked_occurrences <= occurrences),
  CONSTRAINT alert_quality_name_ck   CHECK (length(alertname) BETWEEN 1 AND 1024)
);

-- No secondary index: the PRIMARY KEY (org_id, day, cluster_key, alertname) is
-- already the access path for every rollup read, which is always a date range
-- for one org. Totals are served from HERE, never COUNT(*) on alerts or
-- alert_events (SPEC §D.12).

COMMENT ON TABLE  alert_quality_daily IS
  'Daily alert-hygiene rollup, written by the stats.rollup job. DELIBERATELY KEYED BY (org, day, cluster, alertname) AND CARRYING NO USER COLUMN: per-person response metrics, leaderboards and aggregates are UNREPRESENTABLE IN THIS SCHEMA BY CONSTRUCTION (SPEC R8). A feature you do not build cannot be misused. Surfacing "this rule fired 200 times and was never acknowledged" does more good than any enrichment -- the best alert is the one that no longer exists.';
COMMENT ON COLUMN alert_quality_daily.day IS 'UTC calendar day of the rollup.';
COMMENT ON COLUMN alert_quality_daily.occurrences IS 'Episodes that STARTED on this day for this (cluster, alertname).';
COMMENT ON COLUMN alert_quality_daily.notifications IS 'Notification intents minted. Compare against deliveries to see suppression at work.';
COMMENT ON COLUMN alert_quality_daily.deliveries IS 'Delivery materialisations attempted across all channels.';
COMMENT ON COLUMN alert_quality_daily.acked_occurrences IS 'Episodes acknowledged by SOMEBODY. Never by whom -- that is the whole point (R8). Capped at occurrences.';
COMMENT ON COLUMN alert_quality_daily.auto_resolved IS 'Episodes that ended with an explicit upstream status=resolved.';
COMMENT ON COLUMN alert_quality_daily.expired IS 'Episodes that ended because we STOPPED HEARING about them. Counted separately from auto_resolved because expired is NOT resolved and oto never conflates the two.';
COMMENT ON COLUMN alert_quality_daily.total_firing_seconds IS 'Summed firing duration, the raw material for MTTR without storing a per-person timing anywhere.';
COMMENT ON COLUMN alert_quality_daily.flap_transitions IS 'State flips observed, the noisiness signal for the hygiene report.';

-- +goose Down

DROP TABLE IF EXISTS alert_quality_daily;
DROP TABLE IF EXISTS rate_limit_buckets;
