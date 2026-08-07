-- SPEC §D.4 -- Alerts, occurrences, events. The heart of oto.
--
-- Three nouns, and conflating any two of them is the single most expensive
-- mistake available in this codebase (SPEC §A):
--
--   Alert            the IDENTITY of a label set within (org, cluster). Created
--                    on first sight, survives resolution forever. Sentry's Issue.
--   AlertOccurrence  one CONTIGUOUS FIRING EPISODE of an Alert, (alert_id, seq).
--                    What you acknowledge, what you time for MTTR.
--   AlertEvent       one IMMUTABLE thing that happened at one instant. The
--                    timeline. Append-only. If you would ever want to UPDATE it,
--                    it is not an Event.
--
-- alerts and alert_occurrences are PROJECTIONS. alert_events is the truth.

-- +goose Up

-- -------------------------------------------------------------------- alerts

CREATE TABLE alerts (
  id                    UUID        PRIMARY KEY,
  org_id                UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  cluster_id            UUID        NOT NULL REFERENCES clusters(id),
  alert_key             TEXT        NOT NULL,      -- C.2, the identity
  source_fingerprint    TEXT        NOT NULL,      -- C.3, recomputed FNV-1a, 16 hex chars

  -- promoted labels (hot filters)
  alertname             TEXT        NOT NULL,
  severity              TEXT,
  namespace             TEXT,
  service               TEXT,
  cluster_key           TEXT        NOT NULL,

  -- full data
  labels                JSONB       NOT NULL,
  annotations           JSONB       NOT NULL DEFAULT '{}'::jsonb,
  generator_url         TEXT,

  -- projection of the current/latest occurrence
  state                 TEXT        NOT NULL,
  current_occurrence_id UUID,
  ack_state             TEXT        NOT NULL DEFAULT 'unacked',

  -- history
  first_seen_at         TIMESTAMPTZ NOT NULL,
  last_seen_at          TIMESTAMPTZ NOT NULL,
  last_state_change_at  TIMESTAMPTZ NOT NULL,
  total_occurrences     INT         NOT NULL DEFAULT 0,
  flap_score            REAL        NOT NULL DEFAULT 0,
  is_flapping           BOOLEAN     NOT NULL DEFAULT false,

  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT alerts_key_uniq    UNIQUE (org_id, alert_key),
  -- Inline and unnamed in SPEC §D.4; named here.
  CONSTRAINT alerts_state_ck    CHECK (state IN ('firing','suppressed','resolved','expired')),
  CONSTRAINT alerts_ackstate_ck CHECK (ack_state IN ('unacked','acked')),
  CONSTRAINT alerts_key_ck      CHECK (alert_key ~ '^ak_[0-9a-v]{26}$'),
  CONSTRAINT alerts_srcfp_ck    CHECK (source_fingerprint ~ '^[0-9a-f]{16}$'),
  CONSTRAINT alerts_name_ck     CHECK (length(alertname) BETWEEN 1 AND 1024),
  CONSTRAINT alerts_clusterk_ck CHECK (length(btrim(cluster_key)) > 0),
  CONSTRAINT alerts_labels_ck   CHECK (jsonb_typeof(labels) = 'object'),
  CONSTRAINT alerts_annot_ck    CHECK (jsonb_typeof(annotations) = 'object'),
  CONSTRAINT alerts_occ_ck      CHECK (total_occurrences >= 0),
  CONSTRAINT alerts_flap_ck     CHECK (flap_score >= 0),
  CONSTRAINT alerts_seen_ck     CHECK (last_seen_at >= first_seen_at),
  CONSTRAINT alerts_change_ck   CHECK (last_state_change_at >= first_seen_at),
  CONSTRAINT alerts_time_ck     CHECK (updated_at >= created_at)
);

-- Serves: §D.12(a), the single hottest query -- the alert list, keyset-paginated
-- by (last_seen_at DESC, id DESC), filtered by state. There is no OFFSET in
-- this codebase (CONTEXT.md §5 rule 8).
CREATE INDEX alerts_list_idx   ON alerts (org_id, state, last_seen_at DESC, id DESC);
-- Serves: the default landing view -- open alerts only, same keyset. Partial so
-- the resolved/expired long tail never enters the index.
CREATE INDEX alerts_open_idx   ON alerts (org_id, last_seen_at DESC, id DESC)
                                WHERE state IN ('firing','suppressed');
-- Serves: §D.12(a) filter $6, alertname equality, plus the alert-hygiene drill-in
-- ("this rule fired 200 times and was never acknowledged").
CREATE INDEX alerts_name_idx   ON alerts (org_id, alertname, last_seen_at DESC);
-- Serves: §D.12(a) filters $4/$5 -- the cluster + namespace + state facet.
CREATE INDEX alerts_ns_idx     ON alerts (org_id, cluster_key, namespace, state);
-- Serves: §D.12(a) filter $3 -- the severity facet, keyset-ordered.
CREATE INDEX alerts_sev_idx    ON alerts (org_id, severity, state, last_seen_at DESC);
-- Serves: the reconciler join key -- matching a GET /api/v2/alerts result back
-- onto an oto Alert by Alertmanager fingerprint (SPEC §C.3, §G.8.3).
CREATE INDEX alerts_srcfp_idx  ON alerts (org_id, cluster_key, source_fingerprint);
-- Serves: §D.12(a) filter $7 -- the label selector, `labels @> $7`. jsonb_path_ops
-- is roughly a third the size of the default opclass and is the right choice
-- because oto only ever does containment, never key-existence (SPEC §C.9.3).
CREATE INDEX alerts_labels_gin ON alerts USING GIN (labels jsonb_path_ops);
-- Serves: the free-text search box over alertname + summary + description.
CREATE INDEX alerts_text_idx   ON alerts USING GIN (
    to_tsvector('simple', alertname || ' ' || coalesce(annotations->>'summary','')
                                    || ' ' || coalesce(annotations->>'description','')));

COMMENT ON TABLE  alerts IS
  'The IDENTITY of a label set within (org, cluster_key) -- oto answer to Sentry Issue. Created on first sight and never deleted; resolution ends an Occurrence, not an Alert. Everything below first_seen_at is a PROJECTION of alert_events, kept for query speed, never the only record.';
COMMENT ON COLUMN alerts.alert_key IS
  'The dedup identity: ak_ + 26 base32hex chars over sha256(org_id, cluster_key, canon(labels minus source.ignore_labels)) (SPEC §C.2). Dedup is enforced by alerts_key_uniq, NEVER by a read-then-write check.';
COMMENT ON COLUMN alerts.source_fingerprint IS
  'Alertmanager fingerprint RECOMPUTED by oto, never trusted from the wire (SPEC §C.3). 16 hex chars, FNV-1a over the FULL label set. The reconciliation join key -- never the product identity.';
COMMENT ON COLUMN alerts.severity IS 'Promoted label. Drives the Slack EMOJI, never the Slack colour (SPEC §H.1).';
COMMENT ON COLUMN alerts.namespace IS 'Promoted label, btree-indexed. Nullable -- not every alert is Kubernetes.';
COMMENT ON COLUMN alerts.service IS 'Promoted label, for the service facet. Nullable.';
COMMENT ON COLUMN alerts.cluster_key IS 'Denormalised from clusters so the hot list query filters without a join. Part of alert_key.';
COMMENT ON COLUMN alerts.labels IS 'The FULL label set including labels excluded from the hash. Searched via alerts_labels_gin containment (SPEC §C.9.3).';
COMMENT ON COLUMN alerts.generator_url IS 'Prometheus link from the payload. Decoding g0.expr out of it is the zero-API-call path to a RuleSnapshot (SPEC §D.6).';
COMMENT ON COLUMN alerts.state IS 'Projection of the current occurrence: firing | suppressed | resolved | expired. expired is NOT resolved -- resolved requires an explicit upstream status=resolved; expired means we stopped hearing about it. Never fabricate a resolution.';
COMMENT ON COLUMN alerts.current_occurrence_id IS 'The latest AlertOccurrence. FK added below, after alert_occurrences exists.';
COMMENT ON COLUMN alerts.ack_state IS 'ORTHOGONAL to state (SPEC §B.1). An acked alert is still firing.';
COMMENT ON COLUMN alerts.last_state_change_at IS 'When state last changed, as opposed to last_seen_at which moves on every observation.';
COMMENT ON COLUMN alerts.flap_score IS 'Rolling flap metric from the flap.score job. Never negative.';
COMMENT ON COLUMN alerts.is_flapping IS 'A VISIBLE UI state, never silent suppression -- silence destroys trust (SPEC §B.6).';

-- --------------------------------------------------------- alert_occurrences

CREATE TABLE alert_occurrences (
  id                 UUID        PRIMARY KEY,
  org_id             UUID        NOT NULL,
  alert_id           UUID        NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  group_id           UUID,                          -- FK added in 00008
  seq                INT         NOT NULL,          -- 1,2,3... per alert

  state              TEXT        NOT NULL,
  suppression_reason TEXT,
  suppressed_by      JSONB       NOT NULL DEFAULT '{}'::jsonb,  -- {silencedBy:[],inhibitedBy:[],mutedBy:[]}

  -- oto clock
  started_at         TIMESTAMPTZ NOT NULL,
  ended_at           TIMESTAMPTZ,
  last_observed_at   TIMESTAMPTZ NOT NULL,
  -- upstream clock
  source_starts_at   TIMESTAMPTZ NOT NULL,
  source_ends_at     TIMESTAMPTZ,
  source_updated_at  TIMESTAMPTZ,

  resolve_reason     TEXT,
  reopen_count       INT         NOT NULL DEFAULT 0,
  reopen_of          UUID,                          -- previous occurrence when T7 followed a close

  ack_state          TEXT        NOT NULL DEFAULT 'unacked',
  acked_by           UUID        REFERENCES users(id) ON DELETE SET NULL,
  acked_by_label     TEXT,                          -- denormalised, immutable display name
  acked_at           TIMESTAMPTZ,
  ack_note           TEXT,

  rule_snapshot_id   UUID,                          -- FK added in 00009
  value              DOUBLE PRECISION,
  observed_skew_ms   BIGINT      NOT NULL DEFAULT 0,

  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT occ_seq_uniq       UNIQUE (alert_id, seq),
  -- Inline and unnamed in SPEC §D.4; named here.
  CONSTRAINT occ_state_ck       CHECK (state IN ('firing','suppressed','resolved','expired')),
  CONSTRAINT occ_supreason_ck   CHECK (suppression_reason IS NULL OR suppression_reason IN
                                  ('silence','inhibition','mute_time_interval','active_time_interval')),
  CONSTRAINT occ_resreason_ck   CHECK (resolve_reason IS NULL OR resolve_reason IN ('upstream','timeout')),
  CONSTRAINT occ_ackstate_ck    CHECK (ack_state IN ('unacked','acked')),
  CONSTRAINT occ_terminal_ended CHECK ((state IN ('resolved','expired')) = (ended_at IS NOT NULL)),
  CONSTRAINT occ_seq_ck         CHECK (seq >= 1),
  CONSTRAINT occ_reopen_ck      CHECK (reopen_count >= 0),
  CONSTRAINT occ_order_ck       CHECK (ended_at IS NULL OR ended_at >= started_at),
  CONSTRAINT occ_obs_ck         CHECK (last_observed_at >= started_at),
  CONSTRAINT occ_src_order_ck   CHECK (source_ends_at IS NULL OR source_ends_at >= source_starts_at),
  -- suppression_reason and suppressed_by exist ONLY while suppressed (C1: reconciler-only)
  CONSTRAINT occ_suppress_ck    CHECK ((state = 'suppressed') = (suppression_reason IS NOT NULL)),
  CONSTRAINT occ_suppby_ck      CHECK (jsonb_typeof(suppressed_by) = 'object'),
  -- resolve_reason exists ONLY on a terminal state, and matches it
  CONSTRAINT occ_resolve_ck     CHECK ((state IN ('resolved','expired')) = (resolve_reason IS NOT NULL)),
  CONSTRAINT occ_resolve_map_ck CHECK (resolve_reason IS NULL
                                       OR (state = 'resolved' AND resolve_reason = 'upstream')
                                       OR (state = 'expired'  AND resolve_reason = 'timeout')),
  -- ack fields are all-or-nothing
  CONSTRAINT occ_ack_ck         CHECK ((ack_state = 'acked') = (acked_at IS NOT NULL)),
  CONSTRAINT occ_acklabel_ck    CHECK ((acked_at IS NULL) = (acked_by_label IS NULL)),
  CONSTRAINT occ_ackorder_ck    CHECK (acked_at IS NULL OR acked_at >= started_at),
  CONSTRAINT occ_acknote_ck     CHECK (ack_note IS NULL OR length(ack_note) <= 2000),
  CONSTRAINT occ_reopenof_ck    CHECK (reopen_of IS NULL OR reopen_of <> id),
  CONSTRAINT occ_time_ck        CHECK (updated_at >= created_at)
);

-- INVARIANT: at most one open occurrence per alert. Enforced in the DB, not in Go.
-- An Occurrence is a CONTIGUOUS episode; two open at once is definitionally a bug.
CREATE UNIQUE INDEX occ_one_open_idx ON alert_occurrences (alert_id) WHERE ended_at IS NULL;
-- Serves: the per-alert episode history, newest episode first.
CREATE INDEX occ_alert_idx  ON alert_occurrences (org_id, alert_id, seq DESC);
-- Serves: group membership expansion when rendering a group card.
CREATE INDEX occ_group_idx  ON alert_occurrences (org_id, group_id, started_at DESC);
-- Serves: the occurrence.reap sweep -- open episodes whose upstream endsAt has
-- passed. Partial and non-org-scoped on purpose: the reaper is a global
-- background sweep, not a tenant query.
CREATE INDEX occ_reap_idx   ON alert_occurrences (source_ends_at)
                             WHERE ended_at IS NULL AND source_ends_at IS NOT NULL;
-- Serves: the "unacked and still open" queue, and escalation.check (SPEC §G.9).
CREATE INDEX occ_ack_idx    ON alert_occurrences (org_id, ack_state, started_at DESC)
                             WHERE ended_at IS NULL;

ALTER TABLE alerts ADD CONSTRAINT alerts_current_occ_fk
  FOREIGN KEY (current_occurrence_id) REFERENCES alert_occurrences(id) ON DELETE SET NULL;

COMMENT ON TABLE  alert_occurrences IS
  'One CONTIGUOUS FIRING EPISODE of an Alert, identified by (alert_id, seq). This is what you acknowledge and what you time for MTTR. At most one may be open per Alert, enforced by occ_one_open_idx.';
COMMENT ON COLUMN alert_occurrences.org_id IS 'Denormalised from alerts so every composite index can lead with org_id (CONTEXT.md §5 rule 7).';
COMMENT ON COLUMN alert_occurrences.seq IS 'Episode number within the Alert, 1-based and gapless.';
COMMENT ON COLUMN alert_occurrences.state IS 'firing | suppressed | resolved | expired. suppressed is INVISIBLE to webhooks -- Alertmanager MuteStage drops muted alerts before the webhook fires, so only the API v2 reconciler can ever set it (SPEC §B.2).';
COMMENT ON COLUMN alert_occurrences.suppression_reason IS 'Why it is suppressed: silence | inhibition | mute_time_interval | active_time_interval. Present exactly when state=suppressed (occ_suppress_ck).';
COMMENT ON COLUMN alert_occurrences.suppressed_by IS 'Raw upstream attribution: {silencedBy:[], inhibitedBy:[], mutedBy:[]} from GET /api/v2/alerts.';
COMMENT ON COLUMN alert_occurrences.started_at IS 'OTO clock. Ordering and durations use this.';
COMMENT ON COLUMN alert_occurrences.ended_at IS 'OTO clock. NULL exactly while the episode is open (occ_terminal_ended).';
COMMENT ON COLUMN alert_occurrences.source_starts_at IS 'UPSTREAM claim (Alertmanager startsAt). Displayed, not ordered by.';
COMMENT ON COLUMN alert_occurrences.source_ends_at IS 'UPSTREAM claim (Alertmanager endsAt). What the reaper watches -- but only while source_health.status = healthy (SPEC §B.4).';
COMMENT ON COLUMN alert_occurrences.resolve_reason IS 'upstream (an explicit status=resolved arrived) or timeout (we stopped hearing about it). The pair state/resolve_reason is locked together by occ_resolve_map_ck so oto can never claim resolved when it means expired.';
COMMENT ON COLUMN alert_occurrences.reopen_of IS 'The previous occurrence when this episode re-fired within the grace window (transition T7, SPEC §B.5).';
COMMENT ON COLUMN alert_occurrences.acked_by IS 'The user who acknowledged. Stored because it is operationally necessary; ON DELETE SET NULL because the acked_by_label keeps the timeline readable afterwards. NO per-person metrics are derived from this (SPEC R8).';
COMMENT ON COLUMN alert_occurrences.acked_by_label IS 'Display name frozen at ack time. Immutable -- the timeline must read the same in a year.';
COMMENT ON COLUMN alert_occurrences.rule_snapshot_id IS 'The RuleSnapshot bound at fire time: what the rule SAID at that moment. The differentiator. FK added in 00009.';
COMMENT ON COLUMN alert_occurrences.value IS 'The evaluated sample value, when the source supplied one.';
COMMENT ON COLUMN alert_occurrences.observed_skew_ms IS 'received_at minus upstream startsAt. Clock skew is MEASURED, not rejected (SPEC C12).';

-- -------------------------------------------------------------- alert_events

CREATE TABLE alert_events (
  id            UUID        NOT NULL,               -- uuidv7 => time-sortable tiebreak
  org_id        UUID        NOT NULL,
  alert_id      UUID,
  occurrence_id UUID,
  group_id      UUID,
  type          TEXT        NOT NULL,               -- SPEC §D.4.1, closed enum
  occurred_at   TIMESTAMPTZ NOT NULL,               -- UPSTREAM clock (display)
  recorded_at   TIMESTAMPTZ NOT NULL,               -- OTO clock (ordering)  -- PARTITION KEY
  actor_kind    TEXT        NOT NULL,
  actor_id      TEXT,
  actor_label   TEXT,                               -- denormalised, immutable
  summary       TEXT        NOT NULL,               -- pre-rendered one-liner for the timeline
  payload       JSONB       NOT NULL DEFAULT '{}'::jsonb,
  dedupe_key    TEXT,
  PRIMARY KEY (id, recorded_at),
  -- Inline and unnamed in SPEC §D.4; named here.
  CONSTRAINT ev_actorkind_ck CHECK (actor_kind IN
                            ('system','ingest','reconciler','reaper','enricher','notifier','user','slack')),
  CONSTRAINT ev_type_ck    CHECK (type ~ '^[a-z_]+\.[a-z_]+$'),
  CONSTRAINT ev_summary_ck CHECK (length(btrim(summary)) BETWEEN 1 AND 500),
  CONSTRAINT ev_payload_ck CHECK (jsonb_typeof(payload) = 'object'),
  CONSTRAINT ev_actor_ck   CHECK (actor_kind <> 'user' OR (actor_id IS NOT NULL AND actor_label IS NOT NULL)),
  CONSTRAINT ev_subject_ck CHECK (alert_id IS NOT NULL OR occurrence_id IS NOT NULL OR group_id IS NOT NULL),
  -- NOTE: there is deliberately NO (recorded_at >= occurred_at) check. Upstream clock skew is
  -- real and is measured, not rejected (C12). Ordering uses recorded_at; display uses occurred_at.
  -- Do not "fix" this: adding the check would make oto reject the truth about a
  -- skewed cluster instead of reporting it in source_health.clock_skew_ms.
  CONSTRAINT ev_dedupe_ck  CHECK (dedupe_key IS NULL OR length(dedupe_key) BETWEEN 1 AND 200)
) PARTITION BY RANGE (recorded_at);

-- Serves: the per-alert timeline, keyset-paginated by (recorded_at DESC, id DESC).
CREATE INDEX ev_alert_idx ON alert_events (org_id, alert_id,      recorded_at DESC, id DESC);
-- Serves: the per-occurrence timeline.
CREATE INDEX ev_occ_idx   ON alert_events (org_id, occurrence_id, recorded_at DESC, id DESC);
-- Serves: §D.12(b), the GROUP TIMELINE -- the signature UI view. Always
-- time-bounded by the caller so the planner can prune partitions; the API
-- defaults the lower bound to group.first_seen_at.
CREATE INDEX ev_group_idx ON alert_events (org_id, group_id,      recorded_at DESC, id DESC);
-- Serves: filtering a timeline to one event type, e.g. "show me only deliveries".
CREATE INDEX ev_type_idx  ON alert_events (org_id, type,          recorded_at DESC);

COMMENT ON TABLE  alert_events IS
  'The append-only timeline: one immutable thing that happened at one instant. Current state everywhere else in this schema is a projection of these rows, never the only record. If you would ever want to UPDATE a row here, it is not an Event. MONTHLY partitions on recorded_at, retention orgs.settings.event_retention_months (default 13).';
COMMENT ON COLUMN alert_events.id IS 'UUIDv7, so id is itself a time-ordered tiebreak for the (recorded_at, id) keyset.';
COMMENT ON COLUMN alert_events.type IS 'Closed enum, enumerated in SPEC §D.4.1. The regex only enforces the shape; adding a type requires a SPEC amendment and implementers MUST NOT invent types.';
COMMENT ON COLUMN alert_events.occurred_at IS 'UPSTREAM claim of when it happened. DISPLAYED. Never used for ordering.';
COMMENT ON COLUMN alert_events.recorded_at IS 'OTO clock. ORDERED BY. PARTITION KEY. There is deliberately no constraint tying it to occurred_at -- upstream clock skew is measured, not rejected (SPEC C12).';
COMMENT ON COLUMN alert_events.actor_kind IS 'Who caused it: system | ingest | reconciler | reaper | enricher | notifier | user | slack.';
COMMENT ON COLUMN alert_events.actor_id IS 'Opaque actor identifier, mandatory when actor_kind=user.';
COMMENT ON COLUMN alert_events.actor_label IS 'Display name frozen at write time. Immutable, so a renamed or deleted user does not rewrite history.';
COMMENT ON COLUMN alert_events.summary IS 'Pre-rendered one-line timeline string. Rendered once at write time so reading a timeline never needs the renderer.';
COMMENT ON COLUMN alert_events.dedupe_key IS 'Optional idempotency token, e.g. occ:{id}:opened. Its uniqueness is enforced in alert_event_keys, NOT here -- see that table.';

-- ---------------------------------------------------------- alert_event_keys

-- UNPARTITIONED. This is where event idempotency actually works (C14).
-- The writer inserts here FIRST, in the same transaction; zero rows affected
-- means the event already exists and the alert_events insert is skipped
-- (SPEC §C.8). A unique index on the partitioned parent could not do this
-- because it would have to include recorded_at, and the whole point is to
-- suppress a SECOND write at a DIFFERENT time.
CREATE TABLE alert_event_keys (
  org_id     UUID        NOT NULL,
  dedupe_key TEXT        NOT NULL,
  event_id   UUID        NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, dedupe_key),
  CONSTRAINT alert_event_keys_ck CHECK (length(dedupe_key) BETWEEN 1 AND 200)
);

-- Serves: the 30-day pruner. The idempotency probe itself rides the PRIMARY KEY.
CREATE INDEX alert_event_keys_prune_idx ON alert_event_keys (created_at);

COMMENT ON TABLE  alert_event_keys IS
  'Global idempotency index for alert_events (SPEC §C.8). DELIBERATELY UNPARTITIONED: a unique index on a partitioned table must include the partition key and therefore cannot suppress a duplicate written in a later partition (conflict ruling 14). INSERT ... ON CONFLICT DO NOTHING here, in the same transaction as the event; zero rows means skip the event. Pruned at created_at < now() - 30 days.';
COMMENT ON COLUMN alert_event_keys.event_id IS 'The alert_events row that won. No FK: alert_events is partitioned and this table outlives no partition boundary guarantee.';

-- Initial partitions: this month plus the next 3, per SPEC §D.11.
SELECT oto_ensure_partitions_ahead('alert_events', 'month', 3);

-- +goose Down

DROP TABLE IF EXISTS alert_event_keys;
DROP TABLE IF EXISTS alert_events;   -- drops its partitions with it
ALTER TABLE alerts DROP CONSTRAINT IF EXISTS alerts_current_occ_fk;
DROP TABLE IF EXISTS alert_occurrences;
DROP TABLE IF EXISTS alerts;
