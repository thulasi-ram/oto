-- SPEC §D.8 -- Channels, policies, notifications, deliveries, threads.
--
-- The distinction that this whole module exists to preserve (SPEC §A):
--
--   Channel               a CONFIGURED DESTINATION INSTANCE ("Slack workspace
--                         T123, #sre-alerts"). NOT a channel type.
--   Notification          the channel-agnostic INTENT to communicate ONE FACT.
--                         Idempotent by (org_id, idempotency_key).
--   NotificationDelivery  ONE MATERIALISATION of that intent on ONE Channel.
--                         Owns retry state and provider ids.
--   ChannelThread         the binding of an AlertGroup to (channel_id, root_ts).
--
-- "notification" NEVER means a Slack message. That is a Delivery (SPEC §A.1).

-- +goose Up

-- ------------------------------------------------------- channel_credentials

CREATE TABLE channel_credentials (
  id          UUID        PRIMARY KEY,
  org_id      UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  kind        TEXT        NOT NULL,                -- slack_bot_token | slack_app_token |
                                                   -- slack_signing_secret | basic | bearer | none
  sealed      BYTEA       NOT NULL,                -- AES-256-GCM, key from platform/secrets keyring
  key_version INT         NOT NULL DEFAULT 1,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  rotated_at  TIMESTAMPTZ,
  CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
    ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','none')),
  CONSTRAINT channel_credentials_seal_ck CHECK (octet_length(sealed) BETWEEN 29 AND 65536),
  CONSTRAINT channel_credentials_ver_ck  CHECK (key_version >= 1),
  CONSTRAINT channel_credentials_rot_ck  CHECK (rotated_at IS NULL OR rotated_at >= created_at)
);

COMMENT ON TABLE  channel_credentials IS
  'Sealed secrets for Channels and for AlertSource auth. The generic secret store: alert_sources.auth_credential_id reuses it rather than growing a second one.';
COMMENT ON COLUMN channel_credentials.kind IS 'What the sealed blob is. slack_signing_secret matters especially: slack-go below v0.23.1 accepts an EMPTY signing secret and therefore forged requests (CONTEXT.md §6).';
COMMENT ON COLUMN channel_credentials.sealed IS 'AES-256-GCM ciphertext, key from the platform/secrets keyring. The lower bound of 29 bytes is a 12-byte nonce plus a 16-byte tag plus at least one byte of plaintext.';
COMMENT ON COLUMN channel_credentials.key_version IS 'Keyring generation that sealed this blob, so rotation can decrypt old rows.';
COMMENT ON COLUMN channel_credentials.rotated_at IS 'When the secret was last re-sealed. NULL means never rotated.';

-- alert_sources.auth_credential_id was declared in 00004 without a FK because
-- channel_credentials did not exist yet. Close the loop (SPEC §D.8).
ALTER TABLE alert_sources ADD CONSTRAINT alert_sources_cred_fk
  FOREIGN KEY (auth_credential_id) REFERENCES channel_credentials(id) ON DELETE SET NULL;

-- ------------------------------------------------------------------ channels

CREATE TABLE channels (
  id                UUID        PRIMARY KEY,
  org_id            UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  type              TEXT        NOT NULL,
  name              CITEXT      NOT NULL,
  config            JSONB       NOT NULL,          -- validated against Provider.ConfigSchema
  credential_id     UUID        REFERENCES channel_credentials(id) ON DELETE SET NULL,
  capabilities      BIGINT      NOT NULL DEFAULT 0,
  renderer          TEXT        NOT NULL DEFAULT 'default',
  verbosity         TEXT        NOT NULL DEFAULT 'status_changes',
  thread_updates    BOOLEAN     NOT NULL DEFAULT true,   -- false => update-in-place only
  show_field_emoji  BOOLEAN     NOT NULL DEFAULT true,
  enabled           BOOLEAN     NOT NULL DEFAULT true,
  health_status     TEXT        NOT NULL DEFAULT 'unknown',
  health_error      TEXT,
  health_checked_at TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at        TIMESTAMPTZ,
  CONSTRAINT channels_name_uniq UNIQUE (org_id, name),
  -- Inline and unnamed in SPEC §D.8; named here.
  CONSTRAINT channels_type_ck      CHECK (type IN ('slack','webhook')),
  CONSTRAINT channels_verbosity_ck CHECK (verbosity IN ('all','status_changes','firing_and_resolved','firing_only')),
  CONSTRAINT channels_hstatus_ck   CHECK (health_status IN ('healthy','degraded','auth_failed','config_invalid','unknown')),
  CONSTRAINT channels_name_ck   CHECK (length(btrim(name::text)) BETWEEN 1 AND 120),
  CONSTRAINT channels_config_ck CHECK (jsonb_typeof(config) = 'object'),
  CONSTRAINT channels_caps_ck   CHECK (capabilities >= 0),
  CONSTRAINT channels_rend_ck   CHECK (renderer IN ('default','slack.default','webhook.json')),
  -- a slack channel MUST carry a credential; the webhook provider may not
  CONSTRAINT channels_cred_ck   CHECK (type <> 'slack' OR credential_id IS NOT NULL),
  CONSTRAINT channels_health_ck CHECK (health_status IN ('healthy','unknown') OR health_error IS NOT NULL),
  CONSTRAINT channels_time_ck   CHECK (updated_at >= created_at)
);

-- Serves: policy fan-out -- resolving a policy channel_ids list down to the
-- live, enabled channels of one provider type.
CREATE INDEX channels_enabled_idx ON channels (org_id, type) WHERE enabled AND deleted_at IS NULL;

COMMENT ON TABLE  channels IS
  'A CONFIGURED DESTINATION INSTANCE -- "Slack workspace T123, #sre-alerts" -- not a channel TYPE. Slack and the generic webhook are deliberately built as one-of-N implementations of the Channel port (SPEC §F.1).';
COMMENT ON COLUMN channels.type IS 'Provider type: slack | webhook. Selects the Provider implementation and its ConfigSchema.';
COMMENT ON COLUMN channels.config IS 'Provider-specific settings, validated against the provider JSON Schema (SPEC §L.5). ONE schema serves both server validation and the settings form.';
COMMENT ON COLUMN channels.capabilities IS 'Bitmask of Provider capabilities (threading, edit-in-place, buttons). Lets the renderer degrade instead of failing.';
COMMENT ON COLUMN channels.renderer IS 'Which Renderer to use. default resolves to the provider default.';
COMMENT ON COLUMN channels.verbosity IS 'How much this destination wants: all | status_changes | firing_and_resolved | firing_only. Filtering here produces a suppressed Notification with reason=verbosity, never a silent drop.';
COMMENT ON COLUMN channels.thread_updates IS 'false means update-in-place only -- never post thread replies to this channel.';
COMMENT ON COLUMN channels.show_field_emoji IS 'Renderer preference for severity emoji in card fields.';
COMMENT ON COLUMN channels.health_status IS
  'healthy | degraded | auth_failed | config_invalid | unknown. auth_failed and config_invalid are set by terminal delivery errors and raise a UI banner -- the difference between "Slack is flaky" and "your token was revoked three days ago and nobody noticed" (SPEC §G.6).';
COMMENT ON COLUMN channels.health_error IS 'Mandatory whenever health_status is not healthy or unknown (channels_health_ck).';

-- ------------------------------------------------------ notification_policies

CREATE TABLE notification_policies (
  id            UUID        PRIMARY KEY,
  org_id        UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name          CITEXT      NOT NULL,
  priority      INT         NOT NULL DEFAULT 100,  -- lower = evaluated first
  enabled       BOOLEAN     NOT NULL DEFAULT true,
  matchers      JSONB       NOT NULL DEFAULT '[]'::jsonb,
                -- [{"name":"severity","op":"=","value":"critical"}, ...]  op in = != =~ !~
  reasons       TEXT[]      NOT NULL,              -- subset of §H.6 Reason values
  channel_ids   UUID[]      NOT NULL,
  throttle      JSONB       NOT NULL DEFAULT '{}'::jsonb,   -- {"max":N,"window_s":S} per subject
  -- vocab:allow — shipped schema as of this migration; the rename is 00019_unacked_reminder.sql. A historical migration is not edited, it is superseded.
  escalate_after_s INT,                            -- NULL = no escalation; else unacked-for seconds
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at    TIMESTAMPTZ,
  CONSTRAINT policies_name_uniq UNIQUE (org_id, name),
  CONSTRAINT policies_name_ck     CHECK (length(btrim(name::text)) BETWEEN 1 AND 120),
  CONSTRAINT policies_prio_ck     CHECK (priority BETWEEN 0 AND 10000),
  CONSTRAINT policies_matchers_ck CHECK (jsonb_typeof(matchers) = 'array' AND jsonb_array_length(matchers) <= 32),
  CONSTRAINT policies_reasons_ck  CHECK (array_length(reasons, 1) BETWEEN 1 AND 32
                                         AND array_position(reasons, NULL) IS NULL),
  CONSTRAINT policies_chan_ck     CHECK (array_length(channel_ids, 1) BETWEEN 1 AND 16
                                         AND array_position(channel_ids, NULL) IS NULL),
  CONSTRAINT policies_throttle_ck CHECK (jsonb_typeof(throttle) = 'object'),
  -- vocab:allow — shipped schema as of this migration; the rename is 00019_unacked_reminder.sql. A historical migration is not edited, it is superseded.
  CONSTRAINT policies_esc_ck      CHECK (escalate_after_s IS NULL OR escalate_after_s BETWEEN 60 AND 86400),
  CONSTRAINT policies_time_ck     CHECK (updated_at >= created_at)
);

-- Serves: policy matching on every lifecycle transition -- walk live policies in
-- priority order and stop at the first match.
CREATE INDEX policies_eval_idx ON notification_policies (org_id, priority)
  WHERE enabled AND deleted_at IS NULL;

COMMENT ON TABLE  notification_policies IS
  'Which facts go to which Channels. Evaluated in priority order (lower first) on every lifecycle transition. A subject matching no policy produces a Notification with status=suppressed and suppressed_reason=no_policy -- recorded, never silently dropped.';
COMMENT ON COLUMN notification_policies.priority IS 'Evaluation order, 0..10000, LOWER IS FIRST.';
COMMENT ON COLUMN notification_policies.matchers IS 'Label matchers, at most 32: [{"name","op","value"}] with op in = != =~ !~.';
COMMENT ON COLUMN notification_policies.reasons IS 'Which §H.6 Reason values this policy reacts to. 1..32 entries, no NULLs.';
COMMENT ON COLUMN notification_policies.channel_ids IS 'Fan-out targets, 1..16 Channel ids. A plain UUID[] rather than a join table because it is read whole, on every evaluation, and never queried by member.';
COMMENT ON COLUMN notification_policies.throttle IS 'Per-subject rate cap: {"max":N,"window_s":S}. Throttling yields suppressed_reason=throttled, which is a VISIBLE UI state.';
-- vocab:allow — shipped schema as of this migration; the rename is 00019_unacked_reminder.sql. A historical migration is not edited, it is superseded.
COMMENT ON COLUMN notification_policies.escalate_after_s IS
  'Unacked-for seconds before escalation.check fires one Reason=escalation notification, 60..86400. oto runs its own clock because Alertmanager repeat_interval defaults to FOUR HOURS (SPEC §G.9). NULL disables escalation.';

-- ------------------------------------------------------------ channel_threads

CREATE TABLE channel_threads (
  id                       UUID        PRIMARY KEY,
  org_id                   UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  channel_id               UUID        NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  subject_kind             TEXT        NOT NULL,
  subject_id               UUID        NOT NULL,   -- alert_groups.id (one generation)
  provider_conversation_id TEXT,                   -- Slack channel id C..., from the API RESPONSE
  provider_thread_id       TEXT,                   -- Slack root ts. STRING. NEVER A FLOAT.
  root_delivery_id         UUID,
  reply_count              INT         NOT NULL DEFAULT 0,
  last_sent_seq            INT         NOT NULL DEFAULT 0,   -- ordering gate
  next_seq                 INT         NOT NULL DEFAULT 1,   -- allocator
  state                    TEXT        NOT NULL DEFAULT 'opening',
  dead_reason              TEXT,                   -- channel_not_found | is_archived | message_not_found |
                                                   -- not_in_channel | token_revoked | account_inactive |
                                                   -- edit_window_closed | cannot_reply_to_message
  created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT threads_subject_uniq UNIQUE (channel_id, subject_kind, subject_id),
  -- Inline and unnamed in SPEC §D.8; named here.
  CONSTRAINT threads_subjkind_ck CHECK (subject_kind IN ('alert_group')),
  CONSTRAINT threads_state_ck  CHECK (state IN ('opening','open','frozen','dead')),
  CONSTRAINT threads_seq_ck    CHECK (next_seq >= 1 AND last_sent_seq >= 0 AND last_sent_seq < next_seq),
  CONSTRAINT threads_reply_ck  CHECK (reply_count >= 0),
  -- an OPEN thread must have both halves of the provider handle; ts is TEXT, never a float (S7)
  CONSTRAINT threads_open_ck   CHECK (state <> 'open' OR
                                      (provider_conversation_id IS NOT NULL AND provider_thread_id IS NOT NULL)),
  CONSTRAINT threads_ts_ck     CHECK (provider_thread_id IS NULL OR provider_thread_id ~ '^[0-9]{10}\.[0-9]{6}$'),
  CONSTRAINT threads_dead_ck   CHECK ((state = 'dead') = (dead_reason IS NOT NULL)),
  CONSTRAINT threads_deadmap_ck CHECK (dead_reason IS NULL OR dead_reason IN
    ('channel_not_found','is_archived','message_not_found','not_in_channel','token_revoked',
     'account_inactive','edit_window_closed','cannot_reply_to_message','restricted_action_thread_locked')),
  CONSTRAINT threads_time_ck   CHECK (updated_at >= created_at)
);

-- Serves: the dispatch worker resolving the live thread for a group, and the
-- operator view of threads still in flight.
CREATE INDEX threads_open_idx ON channel_threads (org_id, state) WHERE state IN ('opening','open');

COMMENT ON TABLE  channel_threads IS
  'The binding of one AlertGroup generation to one provider conversation. THE DURABLE HANDLE IS (provider_conversation_id, provider_thread_id) -- and the conversation id comes from the API RESPONSE, never from config (SPEC §H.1). oto never reads Slack back: this table IS oto memory of Slack (C9).';
COMMENT ON COLUMN channel_threads.subject_id IS 'The alert_groups row -- one GENERATION. A re-opened group is a new generation and therefore a new thread.';
COMMENT ON COLUMN channel_threads.provider_conversation_id IS 'Slack channel id (C...) taken from the chat.postMessage RESPONSE, because the configured name can be stale or ambiguous.';
COMMENT ON COLUMN channel_threads.provider_thread_id IS 'Slack root message ts. TEXT, NEVER A FLOAT -- float rounding silently breaks threading (SPEC S7). Shape enforced by threads_ts_ck.';
COMMENT ON COLUMN channel_threads.root_delivery_id IS 'The delivery that created the root message. No FK: notification_deliveries also points here, and one direction is enough to avoid a circular constraint.';
COMMENT ON COLUMN channel_threads.last_sent_seq IS 'Ordering gate: the highest thread_seq actually sent. A delivery whose thread_seq is not last_sent_seq+1 snoozes (SPEC §G.7).';
COMMENT ON COLUMN channel_threads.next_seq IS 'Allocator: a delivery takes next_seq++ inside the CREATING transaction, so sequence order is the causal order of domain events.';
COMMENT ON COLUMN channel_threads.state IS 'opening | open | frozen | dead. frozen means the group closed; dead means a terminal provider error, which is a STATE TRANSITION, not a retry (SPEC §H.9).';
COMMENT ON COLUMN channel_threads.dead_reason IS 'Terminal Slack error that killed the thread. Present exactly when state=dead (threads_dead_ck).';

-- ------------------------------------------------------------- notifications

CREATE TABLE notifications (
  id              UUID        PRIMARY KEY,
  org_id          UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  subject_kind    TEXT        NOT NULL,
  subject_id      UUID        NOT NULL,
  group_id        UUID        NOT NULL REFERENCES alert_groups(id) ON DELETE CASCADE,
  alert_id        UUID,                            -- set when the fact is about one alert
  occurrence_id   UUID,  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
  reason          TEXT        NOT NULL,            -- §H.6 Reason enum
  policy_id       UUID        REFERENCES notification_policies(id) ON DELETE SET NULL,
  state_version   INT         NOT NULL,
  idempotency_key TEXT        NOT NULL,            -- C.7
  status          TEXT        NOT NULL DEFAULT 'pending',
  suppressed_reason TEXT,                          -- no_policy | throttled | storm | flapping |
                                                   -- verbosity | channel_disabled | duplicate_render
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT notifications_idem_uniq UNIQUE (org_id, idempotency_key),
  -- Inline and unnamed in SPEC §D.8; named here.
  CONSTRAINT notifications_subjkind_ck CHECK (subject_kind IN ('alert_group')),
  CONSTRAINT notifications_status_ck   CHECK (status IN ('pending','dispatched','partial','delivered','failed','suppressed')),
  CONSTRAINT notifications_reason_ck CHECK (reason IN
    ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
     -- vocab:allow — the original §H.6 enum; 00018_notification_reasons.sql narrows it. History, not intent.
     'expired','refired','acked','unacked','enriched','rule_changed','comment','escalation','storm')),
  CONSTRAINT notifications_sver_ck   CHECK (state_version >= 1),
  CONSTRAINT notifications_idem_ck   CHECK (idempotency_key ~ '^[0-9a-f]{64}$'),
  CONSTRAINT notifications_supp_ck   CHECK ((status = 'suppressed') = (suppressed_reason IS NOT NULL)),
  CONSTRAINT notifications_suppmap_ck CHECK (suppressed_reason IS NULL OR suppressed_reason IN
    ('no_policy','throttled','storm','flapping','verbosity','channel_disabled','duplicate_render')),
  -- alert-scoped reasons must name the alert they are about
  CONSTRAINT notifications_focus_ck  CHECK (reason NOT IN ('acked','unacked','refired','rule_changed')
                                            OR alert_id IS NOT NULL),
  CONSTRAINT notifications_time_ck   CHECK (updated_at >= created_at)
);

-- Serves: the notification history for one group generation, newest first.
CREATE INDEX notif_subject_idx ON notifications (org_id, subject_kind, subject_id, created_at DESC);
-- Serves: "was anybody told about this alert" on the alert detail page.
-- Delivery failure must be VISIBLE PER ALERT -- oto silence must never be
-- indistinguishable from "no alert" (CONTEXT.md §6).
CREATE INDEX notif_alert_idx   ON notifications (org_id, alert_id, created_at DESC);

COMMENT ON TABLE  notifications IS
  'The channel-agnostic INTENT to communicate ONE FACT. Idempotent by (org_id, idempotency_key), so "all_resolved at state_version 7" can exist exactly once (SPEC §C.7). A Notification is NOT a Slack message -- that is a NotificationDelivery.';
COMMENT ON COLUMN notifications.subject_id IS 'The alert_groups generation this fact is about. Mirrors group_id; kept because subject_kind is designed to grow.';
COMMENT ON COLUMN notifications.alert_id IS 'The specific Alert, when the fact is about one. MANDATORY for acked, unacked, refired and rule_changed (notifications_focus_ck).';
COMMENT ON COLUMN notifications.reason IS 'The §H.6 Reason enum. Together with the channel verbosity it decides update-in-place versus thread reply.';
COMMENT ON COLUMN notifications.state_version IS 'The alert_groups.state_version this intent was minted against. Hashed into idempotency_key.';
COMMENT ON COLUMN notifications.idempotency_key IS 'sha256 over org, subject, reason and state_version (SPEC §C.7), 64 hex chars. A 23505 here is the idempotency mechanism WORKING, not an error (SPEC §L.9).';
COMMENT ON COLUMN notifications.status IS 'pending | dispatched | partial | delivered | failed | suppressed. Aggregated from its deliveries.';
COMMENT ON COLUMN notifications.suppressed_reason IS 'Why nothing was sent. Present exactly when status=suppressed. Suppression is RECORDED and shown in the UI -- silent suppression destroys trust (SPEC §B.6).';

-- ---------------------------------------------------- notification_deliveries

CREATE TABLE notification_deliveries (
  id                  UUID        PRIMARY KEY,
  org_id              UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  notification_id     UUID        NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
  channel_id          UUID        NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  thread_id           UUID        REFERENCES channel_threads(id) ON DELETE SET NULL,
  thread_seq          INT,                         -- FIFO position within the thread
  mode                TEXT        NOT NULL,
  status              TEXT        NOT NULL DEFAULT 'pending',
  attempts            INT         NOT NULL DEFAULT 0,
  next_attempt_at     TIMESTAMPTZ,
  rendered            JSONB,                       -- persisted at CLAIM time, attempts==0 (C11)
  rendered_hash       TEXT,                        -- sha256 of rendered; skips no-op updates
  rendered_fallback   TEXT,                        -- top-level `text`
  provider_message_id TEXT,                        -- Slack ts of THIS message
  provider_conversation_id TEXT,                   -- Slack channel id from the RESPONSE
  provider_response   JSONB,
  error               TEXT,
  error_class         TEXT,
  ambiguous           BOOLEAN     NOT NULL DEFAULT false,   -- §G.5
  sent_at             TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT deliveries_fanout_uniq UNIQUE (notification_id, channel_id),
  -- Inline and unnamed in SPEC §D.8; named here.
  CONSTRAINT deliveries_mode_ck     CHECK (mode IN ('post_root','update_root','thread_reply','broadcast_reply')),
  CONSTRAINT deliveries_status_ck   CHECK (status IN ('pending','sending','sent','failed','dead','skipped')),
  CONSTRAINT deliveries_errclass_ck CHECK (error_class IS NULL OR error_class IN
                                    ('retryable','rate_limited','permanent','config_invalid','auth_expired')),
  CONSTRAINT deliveries_attempts_ck CHECK (attempts >= 0 AND attempts <= 32),
  CONSTRAINT deliveries_seq_ck      CHECK (thread_seq IS NULL OR thread_seq >= 1),
  -- every mode except post_root needs a thread to attach to
  CONSTRAINT deliveries_thread_ck   CHECK (mode = 'post_root' OR thread_id IS NOT NULL),
  CONSTRAINT deliveries_render_ck   CHECK ((rendered IS NULL) = (rendered_hash IS NULL)),
  CONSTRAINT deliveries_hash_ck     CHECK (rendered_hash IS NULL OR rendered_hash ~ '^[0-9a-f]{64}$'),
  CONSTRAINT deliveries_fb_ck       CHECK (rendered IS NULL OR length(coalesce(rendered_fallback,'')) > 0),
  -- a SENT delivery must carry the provider handle and a timestamp
  CONSTRAINT deliveries_sent_ck     CHECK (status <> 'sent' OR
                                           (provider_message_id IS NOT NULL AND sent_at IS NOT NULL)),
  CONSTRAINT deliveries_err_ck      CHECK (status NOT IN ('failed','dead') OR
                                           (error IS NOT NULL AND error_class IS NOT NULL)),
  CONSTRAINT deliveries_retry_ck    CHECK (status <> 'pending' OR attempts = 0 OR next_attempt_at IS NOT NULL),
  CONSTRAINT deliveries_time_ck     CHECK (updated_at >= created_at)
);

-- Serves: the per-thread ORDERING GATE in deliver.dispatch (SPEC §G.7.2) --
-- under the thread advisory lock, find the in-flight deliveries for this thread
-- in seq order to decide whether this one is next.
CREATE INDEX del_thread_seq_idx ON notification_deliveries (thread_id, thread_seq)
  WHERE status IN ('pending','sending');
-- Serves: the retry sweep that re-enqueues due deliveries. The CLAIM itself is
-- the optimistic-lock UPDATE ... WHERE id=$1 AND status IN ('pending','failed')
-- (SPEC §G.5), which rides the PRIMARY KEY; this index finds WHICH ids are due.
CREATE INDEX del_retry_idx  ON notification_deliveries (next_attempt_at)
  WHERE status IN ('pending','failed');
-- Serves: the dead-letter screen. Delivery failure must be visible.
CREATE INDEX del_dead_idx   ON notification_deliveries (org_id, created_at DESC) WHERE status = 'dead';
-- Serves: aggregating a notification status from its fan-out.
CREATE INDEX del_notif_idx  ON notification_deliveries (notification_id);

COMMENT ON TABLE  notification_deliveries IS
  'ONE MATERIALISATION of a Notification on ONE Channel. Owns retry state, provider ids and the rendered payload. Unique per (notification_id, channel_id), which is the fan-out idempotency guard (SPEC §G.5).';
COMMENT ON COLUMN notification_deliveries.thread_seq IS 'FIFO position within the thread, allocated from channel_threads.next_seq inside the creating transaction. Ordering is a property of the WRITE (SPEC §G.7).';
COMMENT ON COLUMN notification_deliveries.mode IS 'post_root | update_root | thread_reply | broadcast_reply. chat.update in place is PRIMARY; thread replies are the exception. repeat interval elapsed means UPDATE ONLY, NEVER POST (CONTEXT.md §6).';
COMMENT ON COLUMN notification_deliveries.status IS 'pending | sending | sent | failed | dead | skipped. skipped covers a coalesced no-op update whose rendered_hash matched the last one.';
COMMENT ON COLUMN notification_deliveries.attempts IS 'Incremented by the claim UPDATE, capped at 32. Terminal after 12 retryable or 20 rate_limited attempts (SPEC §G.6).';
COMMENT ON COLUMN notification_deliveries.rendered IS 'The exact provider payload, persisted BEFORE the network call and at CLAIM time so the card reflects state at claim, not at enqueue (SPEC C11). A payload failing render/slack.Validate is persisted here and the delivery goes straight to dead -- never silently truncated (SPEC §L.6).';
COMMENT ON COLUMN notification_deliveries.rendered_hash IS 'sha256 of rendered. An update_root whose hash equals the thread last hash is skipped, which stops a flapping alert producing 40 identical updates (SPEC §G.7.4).';
COMMENT ON COLUMN notification_deliveries.rendered_fallback IS 'The Slack top-level text: a COMPLETE SENTENCE. It is the push notification, the search snippet, and THE ONLY THING SCREEN READERS READ (CONTEXT.md §6). Mandatory whenever rendered is set.';
COMMENT ON COLUMN notification_deliveries.provider_message_id IS 'Slack ts of THIS message. Mandatory once status=sent (deliveries_sent_ck).';
COMMENT ON COLUMN notification_deliveries.provider_conversation_id IS 'Slack channel id from the API RESPONSE, not from config.';
COMMENT ON COLUMN notification_deliveries.error_class IS 'retryable | rate_limited | permanent | config_invalid | auth_expired. Decides the retry policy; config_invalid and auth_expired NEVER retry (SPEC §G.6).';
COMMENT ON COLUMN notification_deliveries.ambiguous IS
  'The honest flag: oto crashed after the provider may have accepted a post_root and re-sent it (SPEC §G.5). The card carries a visible "possible duplicate after oto recovery" line. Under-delivering a firing alert is worse than a labelled duplicate; exactly-once does not exist and the schema says so.';

-- +goose Down

DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS channel_threads;
DROP TABLE IF EXISTS notification_policies;
DROP TABLE IF EXISTS channels;
ALTER TABLE alert_sources DROP CONSTRAINT IF EXISTS alert_sources_cred_fk;
DROP TABLE IF EXISTS channel_credentials;
