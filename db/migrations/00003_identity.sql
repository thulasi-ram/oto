-- SPEC §D.1 -- Tenancy and identity.
--
-- Every other table in the schema hangs off orgs. `org_id` is the tenancy axis
-- (CONTEXT.md §5 rule 6: every repository method takes a db.TenantScope, and
-- rule 7: every composite index starts with org_id).
--
-- Constraint and index names are taken VERBATIM from SPEC §D. They are a public
-- contract, not decoration: §L.9 maps a 23505 to errs.KindConflict with the
-- constraint name as the error `Code`, and a 23514 increments
-- oto_check_violation_total{constraint}. Renaming one silently changes an API
-- response and a metric label.

-- +goose Up

-- ---------------------------------------------------------------------- orgs

CREATE TABLE orgs (
  id           UUID        PRIMARY KEY,
  slug         CITEXT      NOT NULL UNIQUE,
  name         TEXT        NOT NULL,
  settings     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    -- keys: refire_grace_s(600), resolve_grace_s(300), group_close_delay_s(300),
    --       flap_threshold(5), flap_window_s(1800), flap_digest_interval_s(900),
    --       storm_threshold(25), storm_window_s(60), storm_cooldown_s(600),
    --       raw_retention_days(14), event_retention_months(13)
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at   TIMESTAMPTZ,
  CONSTRAINT orgs_slug_ck     CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
  CONSTRAINT orgs_name_ck     CHECK (length(btrim(name)) BETWEEN 1 AND 200),
  CONSTRAINT orgs_settings_ck CHECK (jsonb_typeof(settings) = 'object'),
  CONSTRAINT orgs_time_ck     CHECK (updated_at >= created_at)
);

COMMENT ON TABLE  orgs IS
  'Tenant root. Every table in oto is scoped by org_id, directly or transitively (SPEC §D.1).';
COMMENT ON COLUMN orgs.id IS
  'UUIDv7 minted by platform/id.New(). Never gen_random_uuid() -- oto needs time-ordered index locality.';
COMMENT ON COLUMN orgs.slug IS
  'URL-safe tenant handle, case-insensitive-unique via CITEXT.';
COMMENT ON COLUMN orgs.settings IS
  'Per-org tuning for the lifecycle machine and retention: refire_grace_s, resolve_grace_s, group_close_delay_s, flap_threshold, flap_window_s, flap_digest_interval_s, storm_threshold, storm_window_s, storm_cooldown_s, raw_retention_days, event_retention_months.';
COMMENT ON COLUMN orgs.deleted_at IS
  'Soft delete. Expand/contract only -- rows are never removed by a migration.';

-- --------------------------------------------------------------------- users

CREATE TABLE users (
  id             UUID        PRIMARY KEY,
  org_id         UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  email          CITEXT      NOT NULL,
  display_name   TEXT        NOT NULL,
  password_hash  TEXT,                            -- argon2id; NULL disables password login
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  disabled_at    TIMESTAMPTZ,
  CONSTRAINT users_email_uniq UNIQUE (org_id, email),
  CONSTRAINT users_email_ck   CHECK (email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$' AND length(email) <= 254),
  CONSTRAINT users_name_ck    CHECK (length(btrim(display_name)) BETWEEN 1 AND 120),
  CONSTRAINT users_pw_ck      CHECK (password_hash IS NULL OR password_hash LIKE '$argon2id$%'),
  CONSTRAINT users_time_ck    CHECK (updated_at >= created_at)
);

-- Serves: the members list and every "who acked this" lookup, both of which
-- only ever want live users.
CREATE INDEX users_org_idx ON users (org_id) WHERE disabled_at IS NULL;

COMMENT ON TABLE  users IS
  'A human principal within one org. Acknowledgement identity is stored because it is operationally necessary; per-person response-time metrics are deliberately unrepresentable anywhere in this schema (SPEC R8).';
COMMENT ON COLUMN users.password_hash IS
  'argon2id encoded hash. NULL means password login is disabled for this user (SSO or Slack-only).';
COMMENT ON COLUMN users.disabled_at IS
  'Soft disable. A disabled user keeps their acked_by rows so the timeline stays honest.';

-- ---------------------------------------------------------------- api_tokens

CREATE TABLE api_tokens (
  id           UUID        PRIMARY KEY,
  org_id       UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id      UUID        REFERENCES users(id) ON DELETE CASCADE,  -- NULL for ingest tokens
  kind         TEXT        NOT NULL,
  name         TEXT        NOT NULL,
  token_hash   BYTEA       NOT NULL,               -- sha256 of the presented secret
  prefix       TEXT        NOT NULL,               -- first 12 chars, for display: "oto_pat_AbCd"
  source_id    UUID,                               -- REQUIRED for kind='ingest'; FK added in 00004
  last_used_at TIMESTAMPTZ,
  expires_at   TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at   TIMESTAMPTZ,
  -- SPEC §D.1 writes this one inline and therefore unnamed. Named here so that a
  -- 23514 carries a stable label into oto_check_violation_total{constraint}.
  CONSTRAINT api_tokens_kind_ck       CHECK (kind IN ('pat','ingest')),
  CONSTRAINT api_tokens_ingest_scope CHECK (kind <> 'ingest' OR source_id IS NOT NULL),
  CONSTRAINT api_tokens_pat_user     CHECK (kind <> 'pat'    OR user_id  IS NOT NULL),
  CONSTRAINT api_tokens_name_ck      CHECK (length(btrim(name)) BETWEEN 1 AND 120),
  CONSTRAINT api_tokens_hash_ck      CHECK (octet_length(token_hash) = 32),
  CONSTRAINT api_tokens_prefix_ck    CHECK (prefix ~ '^oto_(pat|ingest)_[A-Za-z0-9]{4}$'),
  CONSTRAINT api_tokens_expiry_ck    CHECK (expires_at IS NULL OR expires_at > created_at),
  CONSTRAINT api_tokens_revoke_ck    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

-- Serves: the ingest hot path -- one indexed lookup on the presented token's
-- sha256, LRU-cached for 60s (SPEC §G.1). Unique because a hash collision would
-- be a cross-tenant auth bypass.
CREATE UNIQUE INDEX api_tokens_hash_idx ON api_tokens (token_hash);
-- Serves: the settings screen listing live tokens of one kind for one org.
CREATE INDEX api_tokens_org_idx ON api_tokens (org_id, kind) WHERE revoked_at IS NULL;

COMMENT ON TABLE  api_tokens IS
  'Bearer credentials: kind=pat is a user personal access token, kind=ingest is a per-AlertSource webhook token scoped to exactly one source_id (SPEC §G.2 -- a token not scoped to the target source is a 401).';
COMMENT ON COLUMN api_tokens.token_hash IS
  'sha256 of the presented secret, exactly 32 bytes. The secret itself is shown once at creation and never stored.';
COMMENT ON COLUMN api_tokens.prefix IS
  'Leading identifiable chars of the secret, for display in the UI so an operator can tell two tokens apart without revealing either.';
COMMENT ON COLUMN api_tokens.source_id IS
  'The AlertSource this ingest token may post to. Mandatory for kind=ingest; a token presented against another source is rejected 401.';

-- ------------------------------------------------------------------ sessions

CREATE TABLE sessions (
  id          UUID        PRIMARY KEY,
  org_id      UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash  BYTEA       NOT NULL,
  user_agent  TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NOT NULL,
  revoked_at  TIMESTAMPTZ,
  CONSTRAINT sessions_hash_ck   CHECK (octet_length(token_hash) = 32),
  CONSTRAINT sessions_expiry_ck CHECK (expires_at > created_at),
  CONSTRAINT sessions_revoke_ck CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

-- Serves: cookie -> session resolution on every authenticated request.
CREATE UNIQUE INDEX sessions_hash_idx ON sessions (token_hash);
-- Serves: the session reaper sweeping expired-but-not-revoked rows.
CREATE INDEX sessions_expiry_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

COMMENT ON TABLE  sessions IS
  'Browser session for the SolidJS UI. Cookie-backed; the cookie carries the secret, the table carries its sha256.';
COMMENT ON COLUMN sessions.token_hash IS 'sha256 of the session cookie value, exactly 32 bytes.';
COMMENT ON COLUMN sessions.user_agent IS 'Captured at creation for the "your sessions" screen. Never used for authorisation.';

-- ---------------------------------------------------------- slack_identities

CREATE TABLE slack_identities (
  id           UUID        PRIMARY KEY,
  org_id       UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  team_id      TEXT        NOT NULL,               -- Slack workspace T...
  slack_user_id TEXT       NOT NULL,               -- U...
  slack_handle TEXT,
  user_id      UUID        REFERENCES users(id) ON DELETE SET NULL,  -- NULL = unlinked
  linked_at    TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT slack_identities_uniq UNIQUE (org_id, team_id, slack_user_id),
  CONSTRAINT slack_identities_team_ck CHECK (team_id ~ '^T[A-Z0-9]{2,}$'),
  CONSTRAINT slack_identities_user_ck CHECK (slack_user_id ~ '^[UW][A-Z0-9]{2,}$'),
  CONSTRAINT slack_identities_link_ck CHECK ((user_id IS NULL) = (linked_at IS NULL))
);

COMMENT ON TABLE  slack_identities IS
  'Maps a Slack workspace member onto an oto user so that an ack pressed in Slack is attributable. An unlinked identity (user_id NULL) still acks -- the timeline records the Slack handle as actor_label.';
COMMENT ON COLUMN slack_identities.team_id IS 'Slack workspace id (T...). Part of the natural key because one org may span workspaces.';
COMMENT ON COLUMN slack_identities.slack_user_id IS 'Slack member id. U... for humans, W... for Enterprise Grid.';
COMMENT ON COLUMN slack_identities.slack_handle IS 'Display handle at link time. Denormalised and allowed to go stale -- oto never reads Slack back (SPEC C9).';
COMMENT ON COLUMN slack_identities.linked_at IS 'Set exactly when user_id is set; the pair is all-or-nothing (slack_identities_link_ck).';

-- +goose Down

DROP TABLE IF EXISTS slack_identities;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS orgs;
