-- SPEC §D.6 -- Rule snapshots (the differentiator).
--
-- A RuleSnapshot is a CONTENT-ADDRESSED capture of a Prometheus alerting rule at
-- a point in time. It is what lets oto answer the question no other tool in this
-- space answers: WHAT DID THE RULE SAY WHEN THIS FIRED?
--
-- Rows are immutable and deduplicated by content: the same rule text captured a
-- thousand times is one row (rule_snapshots_content_uniq). "Drift" is defined as
-- the newest snapshot for a rule_key having a different rule_fingerprint than
-- the one bound to the previous occurrence (SPEC §C.6).

-- +goose Up

CREATE TABLE rule_snapshots (
  id                UUID        PRIMARY KEY,
  org_id            UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  source_id         UUID        NOT NULL REFERENCES alert_sources(id) ON DELETE CASCADE,
  rule_fingerprint  TEXT        NOT NULL,          -- C.6, content address
  -- RuleKey
  rule_file         TEXT        NOT NULL DEFAULT '',
  rule_group        TEXT        NOT NULL DEFAULT '',
  rule_name         TEXT        NOT NULL,          -- == alertname
  -- definition
  expr              TEXT        NOT NULL,
  for_seconds       DOUBLE PRECISION NOT NULL DEFAULT 0,
  keep_firing_for_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
  rule_labels       JSONB       NOT NULL DEFAULT '{}'::jsonb,
  rule_annotations  JSONB       NOT NULL DEFAULT '{}'::jsonb,
  -- provenance
  origin            TEXT        NOT NULL,
  prometheus_url    TEXT,
  match_confidence  TEXT        NOT NULL DEFAULT 'exact',
  candidate_count   INT         NOT NULL DEFAULT 1,
  captured_at       TIMESTAMPTZ NOT NULL,
  CONSTRAINT rule_snapshots_content_uniq UNIQUE (org_id, source_id, rule_fingerprint),
  -- Inline and unnamed in SPEC §D.6; named here.
  CONSTRAINT rule_snapshots_origin_ck  CHECK (origin IN ('prometheus_api','generator_url','unavailable')),
  CONSTRAINT rule_snapshots_mconf_ck   CHECK (match_confidence IN ('exact','probable','ambiguous','none')),
  CONSTRAINT rule_snapshots_fp_ck     CHECK (rule_fingerprint ~ '^[0-9a-f]{64}$'),
  CONSTRAINT rule_snapshots_name_ck   CHECK (length(btrim(rule_name)) BETWEEN 1 AND 1024),
  CONSTRAINT rule_snapshots_expr_ck   CHECK ((origin = 'unavailable') = (length(btrim(expr)) = 0)),
  CONSTRAINT rule_snapshots_exprlen_ck CHECK (length(expr) <= 65536),
  CONSTRAINT rule_snapshots_for_ck    CHECK (for_seconds >= 0 AND keep_firing_for_seconds >= 0),
  CONSTRAINT rule_snapshots_labels_ck CHECK (jsonb_typeof(rule_labels) = 'object'
                                             AND jsonb_typeof(rule_annotations) = 'object'),
  CONSTRAINT rule_snapshots_cand_ck   CHECK (candidate_count >= 0),
  -- match_confidence and candidate_count must agree
  CONSTRAINT rule_snapshots_conf_ck   CHECK (
      (match_confidence = 'none'      AND candidate_count = 0) OR
      (match_confidence = 'exact'     AND candidate_count = 1) OR
      (match_confidence = 'probable'  AND candidate_count >= 1) OR
      (match_confidence = 'ambiguous' AND candidate_count >= 2)),
  CONSTRAINT rule_snapshots_promurl_ck CHECK (origin <> 'prometheus_api' OR prometheus_url IS NOT NULL)
);

-- Serves: the drift check -- "newest snapshot for this rule_key" (SPEC §C.6).
-- The column order is the RuleKey (source_id, rule_file, rule_group, rule_name)
-- rearranged so the most selective component leads, then captured_at DESC so the
-- newest row is the first tuple read.
CREATE INDEX rule_snapshots_key_idx ON rule_snapshots
  (org_id, source_id, rule_name, rule_group, rule_file, captured_at DESC);

COMMENT ON TABLE  rule_snapshots IS
  'A content-addressed capture of one Prometheus alerting rule at a point in time -- what the rule SAID at that moment. THE DIFFERENTIATOR. Immutable and deduplicated by rule_fingerprint, so a rule captured on every fire costs one row until its text changes.';
COMMENT ON COLUMN rule_snapshots.rule_fingerprint IS
  'sha256 over expr, for, keep_firing_for, canon(rule_labels) and canon(rule_annotations) (SPEC §C.6). The content address; equality means the rule is byte-identical.';
COMMENT ON COLUMN rule_snapshots.rule_file IS 'RuleKey component. Empty string when the origin could not supply it.';
COMMENT ON COLUMN rule_snapshots.rule_group IS 'RuleKey component. Empty string when the origin could not supply it.';
COMMENT ON COLUMN rule_snapshots.rule_name IS 'RuleKey component. Equals the alertname.';
COMMENT ON COLUMN rule_snapshots.expr IS 'The PromQL. Empty exactly when origin=unavailable, and non-empty otherwise (rule_snapshots_expr_ck).';
COMMENT ON COLUMN rule_snapshots.for_seconds IS 'The rule for: duration. Only origin=prometheus_api can supply it.';
COMMENT ON COLUMN rule_snapshots.keep_firing_for_seconds IS 'The rule keep_firing_for: duration. Only origin=prometheus_api can supply it.';
COMMENT ON COLUMN rule_snapshots.origin IS
  'How the definition was obtained. generator_url decodes g0.expr out of generatorURL with ZERO API calls and is the robust primary path. prometheus_api additionally yields for, keep_firing_for and raw labels/annotations. unavailable means neither worked.';
COMMENT ON COLUMN rule_snapshots.prometheus_url IS 'The Prometheus queried. Mandatory when origin=prometheus_api (rule_snapshots_promurl_ck).';
COMMENT ON COLUMN rule_snapshots.match_confidence IS
  'exact | probable | ambiguous | none, locked to candidate_count by rule_snapshots_conf_ck. ambiguous MUST be surfaced in the UI and in Slack, NEVER hidden (SPEC §D.6).';
COMMENT ON COLUMN rule_snapshots.candidate_count IS 'How many rules matched the lookup. 0 for none, 1 for exact, >=2 for ambiguous.';
COMMENT ON COLUMN rule_snapshots.captured_at IS 'When oto captured it. Newest-per-rule_key is what drift compares against.';

-- alert_occurrences.rule_snapshot_id was declared in 00007 without a FK because
-- rule_snapshots did not exist yet. Close the loop (SPEC §D.6).
ALTER TABLE alert_occurrences ADD CONSTRAINT occ_rule_fk  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
  FOREIGN KEY (rule_snapshot_id) REFERENCES rule_snapshots(id) ON DELETE SET NULL;

-- +goose Down

ALTER TABLE alert_occurrences DROP CONSTRAINT IF EXISTS occ_rule_fk;
DROP TABLE IF EXISTS rule_snapshots;
