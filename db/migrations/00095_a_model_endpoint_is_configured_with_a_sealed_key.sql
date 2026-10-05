-- ADR 0053 §3: A MODEL IS REACHED THROUGH ONE PORT AND ONE CHAT COMPLETIONS ADAPTER
-- (git-bug 8f1f071, comment #1 governs). An org configures the model endpoints its
-- Investigators may use: a base URL, a model name and an API key. One adapter reaches
-- every provider or gateway that serves the Chat Completions API, so this table has no
-- vendor column — there is nothing for one to say.
--
-- ⭐⭐ THE KEY IS SEALED IN `channel_credentials`, NOT IN A COLUMN OF ITS OWN. That table
-- is the one sealed-secret store (SPEC §D.8): `alert_sources.auth_credential_id` reuses
-- it, and so does this. A second ciphertext column would be a second key-rotation story
-- for one keyring. The key is sealed as kind `model_api_key`, which is bound into the
-- seal as additional authenticated data, so a Slack token's ciphertext moved onto this
-- row fails to open instead of being presented to a model endpoint.
--
-- ⭐ AN INVESTIGATOR PINS (base_url, model), NOT THIS ROW'S ID (ADR 0053 §6). The pair is
-- the identity a Finding names; `base_url` is stored normalised (lower-case scheme and
-- host, no trailing slash) so the same endpoint has one spelling, and is refused — not
-- stripped — if it carries credentials, because this column is shown and pinned.
--
-- ⛔ NO DEFAULT now() ON EITHER TIMESTAMP. The application stamps both from the injected
-- clock (CONTEXT.md §6, migrations 00032-00034), so `updated_at >= created_at` cannot be
-- tripped by clock skew between an app server and Postgres.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One new table that release N neither reads nor
-- writes, and a wider kind CHECK that release N never writes the new value into.

-- +goose Up

CREATE TABLE model_providers (
  id            UUID        PRIMARY KEY,
  org_id        UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name          TEXT        NOT NULL,
  base_url      TEXT        NOT NULL,
  model         TEXT        NOT NULL,
  -- NULL: the endpoint takes no key (a self-hosted model on the cluster network), and
  -- none is sent. SET NULL on delete for credential_id's usual reason: losing a key must
  -- not lose the record of which endpoint an Investigator was pinned to.
  credential_id UUID        REFERENCES channel_credentials(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL,
  CONSTRAINT model_providers_name_ck     CHECK (char_length(name) BETWEEN 1 AND 120),
  CONSTRAINT model_providers_base_url_ck CHECK (base_url ~ '^https?://' AND length(base_url) <= 2048),
  CONSTRAINT model_providers_model_ck    CHECK (char_length(model) BETWEEN 1 AND 200),
  -- ⛔ A key travels only over TLS: a keyed http endpoint would fail every run, so it is
  -- refused here as it is in domain.KeyNeedsHTTPS.
  CONSTRAINT model_providers_key_tls_ck  CHECK (credential_id IS NULL OR base_url LIKE 'https://%'),
  CONSTRAINT model_providers_time_ck     CHECK (updated_at >= created_at)
);

-- Serves: the settings list (every endpoint in one org, by name) and the uniqueness of
-- a name within an org — one index, because the name IS the list order.
CREATE UNIQUE INDEX model_providers_org_name_uniq ON model_providers (org_id, name);

-- +goose StatementBegin
COMMENT ON TABLE model_providers IS
  'The model endpoints an org''s Investigators may use (ADR 0053 §3, git-bug 8f1f071): a Chat Completions (OpenAI-compatible protocol) base URL, a model name, and a key sealed in channel_credentials. No vendor column: one adapter reaches every endpoint that serves the API. An Investigator version pins (base_url, model), never this row''s id.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN model_providers.base_url IS
  'Normalised (lower-case scheme and host, no trailing slash, no query); the Chat Completions path is appended to it. Never carries credentials: a URL with userinfo is refused, because this column is shown and pinned into every Finding.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN model_providers.credential_id IS
  'The sealed API key: ONE channel_credentials row of kind model_api_key, values {api_key}. NULL sends no key. Nothing returns it: the settings read carries only whether a key is stored.';
-- +goose StatementEnd

ALTER TABLE channel_credentials DROP CONSTRAINT channel_credentials_kind_ck;
ALTER TABLE channel_credentials ADD CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
  ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','webhook_signing_secret',
   'webhook_mapping_secrets','model_api_key','none'));

-- +goose Down

-- The keys go with the table that pointed at them: below this migration the kind CHECK
-- does not admit them, and a sealed key nothing references is nobody's.
DROP TABLE model_providers;
DELETE FROM channel_credentials WHERE kind = 'model_api_key';

-- Byte-identical to what 00090 shipped.
ALTER TABLE channel_credentials DROP CONSTRAINT channel_credentials_kind_ck;
ALTER TABLE channel_credentials ADD CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
  ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','webhook_signing_secret',
   'webhook_mapping_secrets','none'));
