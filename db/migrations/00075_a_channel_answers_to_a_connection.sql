-- A channel row used to be two things at once: "a Slack bot token for workspace T123" and
-- "the specific #sre-alerts it posts to". Every destination carried its own sealed
-- credential, so five channels in one workspace meant the same bot token pasted five
-- times -- and there was nowhere to set the workspace up once and point several channels
-- at it.
--
-- This migration splits that into `channel_connections` (org-wide: one Slack workspace's
-- bot token, or one webhook receiver family's shared auth/signing secret, set up once) and
-- `channels` (one destination -- a specific #channel or URL -- that references a
-- connection). Several channels now share one connection instead of duplicating its
-- credential.
--
-- Breaking, no backfill: this is pre-release, and there is no way to group existing
-- `channels` rows into connections without guessing which shared a workspace. Every
-- existing channel and credential is dropped rather than migrated.

-- +goose Up

-- ------------------------------------------------------------- channel_connections

CREATE TABLE channel_connections (
  id            UUID        PRIMARY KEY,
  org_id        UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  type          TEXT        NOT NULL,
  name          CITEXT      NOT NULL,
  config        JSONB       NOT NULL,          -- validated against Provider.ConnectionConfigSchema
  credential_id UUID        REFERENCES channel_credentials(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL,
  deleted_at    TIMESTAMPTZ,
  CONSTRAINT channel_connections_name_uniq UNIQUE (org_id, name),
  CONSTRAINT channel_connections_type_ck   CHECK (type IN ('slack','webhook')),
  CONSTRAINT channel_connections_name_ck   CHECK (length(btrim(name::text)) BETWEEN 1 AND 120),
  CONSTRAINT channel_connections_config_ck CHECK (jsonb_typeof(config) = 'object'),
  -- A slack connection MUST carry a bot token; a webhook connection may be unauthenticated
  -- (config-only, no shared secret at all) so credential_id stays nullable for it.
  CONSTRAINT channel_connections_cred_ck   CHECK (type <> 'slack' OR credential_id IS NOT NULL),
  CONSTRAINT channel_connections_time_ck   CHECK (updated_at >= created_at)
);

-- +goose StatementBegin
COMMENT ON TABLE channel_connections IS
  'Org-wide provider setup: a Slack workspace''s bot token, or a webhook receiver family''s shared auth/signing secret. Several channels reference one connection.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_connections.config IS
  'Connection-level, non-secret settings -- e.g. a Slack workspace''s team_id. Destination-specific settings (conversation_id, a webhook URL) live on channels.config, not here.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_connections.credential_id IS
  'The shared secret every channel under this connection uses: a Slack bot token, or a webhook basic/bearer credential or signing secret. ON DELETE SET NULL rather than CASCADE for the same reason channels.credential_id was: losing the secret must not lose the record of the connection.';
-- +goose StatementEnd

CREATE INDEX channel_connections_enabled_idx ON channel_connections (org_id, type) WHERE deleted_at IS NULL;

-- ------------------------------------------------------------------------ channels

-- Every existing channel embedded its own credential; there is no connection yet for any
-- of them to point at, and no way to reconstruct one. Drop them rather than guess.
DELETE FROM channels;
DELETE FROM channel_credentials;

ALTER TABLE channels
  DROP CONSTRAINT channels_cred_ck,
  DROP COLUMN credential_id,
  -- ON DELETE RESTRICT, not CASCADE or SET NULL: a channel with no connection cannot open a
  -- provider at all, which is a worse failure than the 409 the API layer raises when an
  -- admin tries to delete a connection still in use (the same shape as channels_in_use for
  -- a channel still named by an enabled policy).
  ADD COLUMN connection_id UUID NOT NULL REFERENCES channel_connections(id) ON DELETE RESTRICT;

-- +goose StatementBegin
COMMENT ON COLUMN channels.connection_id IS
  'The org-wide connection this destination opens through. A channel''s type must equal its connection''s type -- no CHECK can see across the two tables, so channels/api enforces it (the same way checkRenderer catches a cross-provider renderer channels_rend_ck cannot see).';
-- +goose StatementEnd

CREATE INDEX channels_connection_idx ON channels (connection_id);

-- ------------------------------------------------------------------- channel_credentials

-- `webhook_signing_secret` is new: an HMAC secret the webhook provider signs its outbound
-- JSON body with, so a receiver can verify a payload actually came from oto. It is a
-- connection-level credential like the others, never a channel-level one.
ALTER TABLE channel_credentials DROP CONSTRAINT channel_credentials_kind_ck;
ALTER TABLE channel_credentials ADD CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
  ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','webhook_signing_secret','none'));

-- +goose StatementBegin
COMMENT ON COLUMN channel_credentials.kind IS 'What the sealed blob is. slack_signing_secret matters especially: slack-go below v0.23.1 accepts an EMPTY signing secret and therefore forged requests (CONTEXT.md §6). webhook_signing_secret signs the OUTBOUND payload for the receiver to verify; it is not an inbound signature check.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ INTEGRITY CHECK: NO CHANNEL OR CONNECTION MAY EXIST. The Up direction already deleted
-- every pre-migration `channels`/`channel_credentials` row -- that loss happened the moment
-- Up ran and this rollback cannot undo it. What THIS guard protects is different: any
-- `channels` or `channel_connections` row created AFTER Up, under the new schema, has no
-- honest reverse shape. Dropping `connection_id` would leave such a channel with no
-- credential at all, and there is no default choice of "which channel keeps the shared
-- token" when several reference one connection -- an operator has to decide that, not a
-- migration.
-- +goose StatementBegin
DO $$
DECLARE live_channels BIGINT;
DECLARE live_connections BIGINT;
BEGIN
  SELECT count(*) INTO live_channels FROM channels;
  SELECT count(*) INTO live_connections FROM channel_connections;

  IF live_channels > 0 OR live_connections > 0 THEN
    RAISE EXCEPTION
      'migration 00075 down: % channel(s) and % connection(s) exist. Rolling back would strand every channel without a credential -- there is no honest choice of which channel keeps a connection''s shared token when several reference it. Decide what happens to them deliberately (e.g. export their config, then TRUNCATE channels, channel_connections CASCADE) before rolling back.',
      live_channels, live_connections;
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX channels_connection_idx;

ALTER TABLE channels
  DROP COLUMN connection_id,
  ADD COLUMN credential_id UUID REFERENCES channel_credentials(id) ON DELETE SET NULL,
  ADD CONSTRAINT channels_cred_ck CHECK (type <> 'slack' OR credential_id IS NOT NULL);

-- 00011 never commented this column; a rolled-back database matches that.
-- +goose StatementBegin
COMMENT ON COLUMN channels.credential_id IS NULL;
-- +goose StatementEnd

DROP INDEX channel_connections_enabled_idx;
DROP TABLE channel_connections;

ALTER TABLE channel_credentials DROP CONSTRAINT channel_credentials_kind_ck;
ALTER TABLE channel_credentials ADD CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
  ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','none'));

-- Byte-identical to what 00011 shipped, so a rolled-back database matches its own history.
-- +goose StatementBegin
COMMENT ON COLUMN channel_credentials.kind IS 'What the sealed blob is. slack_signing_secret matters especially: slack-go below v0.23.1 accepts an EMPTY signing secret and therefore forged requests (CONTEXT.md §6).';
-- +goose StatementEnd
