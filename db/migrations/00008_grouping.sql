-- SPEC §D.5 -- Groups.
--
-- An AlertGroup is ONE GENERATION of one Alertmanager notification group,
-- derived from (source, receiver, groupLabels). It OWNS EXACTLY ONE Slack
-- thread. When a closed group re-opens, the generation increments and a new
-- thread is born -- which is why the unique key is
-- (org_id, group_key, generation) and not (org_id, group_key).
--
-- "group" here NEVER means a UI grouping. That is a view (SPEC §A.1).

-- +goose Up

-- -------------------------------------------------------------- alert_groups

CREATE TABLE alert_groups (
  id                  UUID        PRIMARY KEY,
  org_id              UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  source_id           UUID        NOT NULL REFERENCES alert_sources(id) ON DELETE CASCADE,
  cluster_id          UUID        NOT NULL REFERENCES clusters(id),
  group_key           TEXT        NOT NULL,        -- C.4, stable across AM config edits
  generation          INT         NOT NULL DEFAULT 1,
  source_group_key    TEXT,                        -- AM's raw groupKey. OPAQUE. NEVER PARSED.
  receiver            TEXT        NOT NULL DEFAULT '',
  group_labels        JSONB       NOT NULL DEFAULT '{}'::jsonb,
  title               TEXT        NOT NULL,

  state               TEXT        NOT NULL,
  severity            TEXT,                        -- max member severity
  state_version       INT         NOT NULL DEFAULT 1,

  firing_count        INT         NOT NULL DEFAULT 0,
  suppressed_count    INT         NOT NULL DEFAULT 0,
  resolved_count      INT         NOT NULL DEFAULT 0,
  expired_count       INT         NOT NULL DEFAULT 0,
  total_count         INT         NOT NULL DEFAULT 0,
  acked_count         INT         NOT NULL DEFAULT 0,

  storm_mode          BOOLEAN     NOT NULL DEFAULT false,
  storm_since         TIMESTAMPTZ,
  last_notification_reason TEXT,                   -- AM's notification_reason, last seen

  first_seen_at       TIMESTAMPTZ NOT NULL,
  last_activity_at    TIMESTAMPTZ NOT NULL,
  closed_at           TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT groups_key_gen_uniq UNIQUE (org_id, group_key, generation),
  -- Inline and unnamed in SPEC §D.5; named here.
  CONSTRAINT groups_state_ck  CHECK (state IN ('open','closed')),
  CONSTRAINT groups_key_ck    CHECK (group_key ~ '^gk_[0-9a-v]{26}$'),
  CONSTRAINT groups_gen_ck    CHECK (generation >= 1),
  CONSTRAINT groups_title_ck  CHECK (length(btrim(title)) BETWEEN 1 AND 500),
  CONSTRAINT groups_labels_ck CHECK (jsonb_typeof(group_labels) = 'object'),
  CONSTRAINT groups_sver_ck   CHECK (state_version >= 1),
  CONSTRAINT groups_counts_ck CHECK (firing_count >= 0 AND suppressed_count >= 0 AND resolved_count >= 0
                                     AND expired_count >= 0 AND total_count >= 0 AND acked_count >= 0),
  CONSTRAINT groups_acked_ck  CHECK (acked_count <= total_count),
  CONSTRAINT groups_closed_ck CHECK ((state = 'closed') = (closed_at IS NOT NULL)),
  CONSTRAINT groups_corder_ck CHECK (closed_at IS NULL OR closed_at >= first_seen_at),
  CONSTRAINT groups_act_ck    CHECK (last_activity_at >= first_seen_at),
  CONSTRAINT groups_storm_ck  CHECK (storm_mode = (storm_since IS NOT NULL)),
  CONSTRAINT groups_time_ck   CHECK (updated_at >= created_at)
);

-- Serves: the groups list, keyset-paginated by (last_activity_at DESC, id DESC).
CREATE INDEX grp_list_idx ON alert_groups (org_id, state, last_activity_at DESC, id DESC);
-- Serves: the ingest hot path -- resolve the CURRENT generation for a group_key
-- (SPEC §G.4 step 4). Partial on state='open' because only one generation of a
-- given group_key is ever open, so this is effectively a unique lookup.
CREATE INDEX grp_open_idx ON alert_groups (org_id, group_key) WHERE state = 'open';
-- Serves: the group.close sweep -- open groups idle past group_close_delay_s.
CREATE INDEX grp_close_idx ON alert_groups (org_id, last_activity_at)
  WHERE state = 'open';

COMMENT ON TABLE  alert_groups IS
  'ONE GENERATION of one Alertmanager notification group, from (source, receiver, groupLabels). OWNS EXACTLY ONE Slack thread. A closed group that re-opens gets a new generation and therefore a new thread. Never means a UI grouping -- that is a view (SPEC §A.1).';
COMMENT ON COLUMN alert_groups.group_key IS
  'gk_ + 26 base32hex chars over sha256(org_id, source_id, receiver, canon(groupLabels)) (SPEC §C.4). STABLE ACROSS alertmanager.yml ROUTE EDITS, which is exactly what Alertmanager own groupKey is not.';
COMMENT ON COLUMN alert_groups.generation IS 'Increments when a closed group re-opens. Part of the unique key, and the reason a re-opened group starts a fresh Slack thread.';
COMMENT ON COLUMN alert_groups.source_group_key IS 'Alertmanager raw groupKey, kept verbatim for observability. OPAQUE -- MUST NOT BE PARSED: it is unescaped and unbounded (SPEC §C.4).';
COMMENT ON COLUMN alert_groups.receiver IS 'Alertmanager receiver name. Empty string for reconciler-sourced groups with no groupLabels (SPEC §C.4).';
COMMENT ON COLUMN alert_groups.title IS 'Pre-rendered group title for the Slack card and the UI.';
COMMENT ON COLUMN alert_groups.severity IS 'Highest member severity, recomputed on membership change. Drives the card emoji.';
COMMENT ON COLUMN alert_groups.state_version IS
  'Increments on every MATERIAL group change. Hashed into notification.idempotency_key (SPEC §C.7), which is what makes "all_resolved at state_version 7" exist exactly once.';
COMMENT ON COLUMN alert_groups.acked_count IS 'Members acknowledged. Constrained <= total_count by groups_acked_ck.';
COMMENT ON COLUMN alert_groups.storm_mode IS 'Storm collapse is ON by default and is a VISIBLE state, never silent suppression (SPEC §B.6). Paired all-or-nothing with storm_since.';
COMMENT ON COLUMN alert_groups.last_notification_reason IS 'The most recent Alertmanager notification_reason seen for this group. Feeds the §H.6 reason-to-mode decision table.';
COMMENT ON COLUMN alert_groups.last_activity_at IS 'Drives group.close. Idle past orgs.settings.group_close_delay_s closes the group and freezes its thread.';

-- ------------------------------------------------------- alert_group_members

CREATE TABLE alert_group_members (
  group_id      UUID        NOT NULL REFERENCES alert_groups(id) ON DELETE CASCADE,
  occurrence_id UUID        NOT NULL REFERENCES alert_occurrences(id) ON DELETE CASCADE,
  org_id        UUID        NOT NULL,
  alert_id      UUID        NOT NULL,
  joined_at     TIMESTAMPTZ NOT NULL,
  left_at       TIMESTAMPTZ,
  PRIMARY KEY (group_id, occurrence_id),
  CONSTRAINT gm_order_ck CHECK (left_at IS NULL OR left_at >= joined_at)
);

-- Serves: "which groups has this alert been part of", newest first.
CREATE INDEX gm_alert_idx ON alert_group_members (org_id, alert_id, joined_at DESC);
-- Serves: the reverse lookup when an occurrence changes state and every group
-- holding it needs a notify.evaluate.
CREATE INDEX gm_occ_idx   ON alert_group_members (occurrence_id);

COMMENT ON TABLE  alert_group_members IS
  'Membership of one AlertOccurrence in one AlertGroup generation, with join/leave times so the group card can be replayed at any past instant.';
COMMENT ON COLUMN alert_group_members.org_id IS 'Denormalised so gm_alert_idx can lead with org_id (CONTEXT.md §5 rule 7).';
COMMENT ON COLUMN alert_group_members.alert_id IS 'Denormalised from the occurrence to answer "groups for this alert" without a join.';
COMMENT ON COLUMN alert_group_members.left_at IS 'NULL while still a member. Membership is history, not a boolean.';

-- alert_occurrences.group_id was declared in 00007 without a FK because
-- alert_groups did not exist yet. Close the loop (SPEC §D.5).
ALTER TABLE alert_occurrences ADD CONSTRAINT occ_group_fk
  FOREIGN KEY (group_id) REFERENCES alert_groups(id) ON DELETE SET NULL;

-- +goose Down

ALTER TABLE alert_occurrences DROP CONSTRAINT IF EXISTS occ_group_fk;
DROP TABLE IF EXISTS alert_group_members;
DROP TABLE IF EXISTS alert_groups;
