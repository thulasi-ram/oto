-- ADR 0016, 0053 §1 §3 §6, 0054 §5: AN INVESTIGATOR READS THE CLUSTER THROUGH AN
-- OPERATOR'S TOOLSERVER (git-bug 2e9a086). A ToolServer is one MCP server the operator
-- runs — Kubernetes, VictoriaLogs, VictoriaMetrics — reached over HTTP at a URL with its
-- own access token; its Tools are what it listed the last time oto asked.
--
-- ⭐⭐ oto HOLDS NO CLUSTER CREDENTIAL (ADR 0016; 0054 §5). The trust boundary is the
-- ToolServer: its ServiceAccount and RBAC are its operator's. The one secret stored here
-- is the ToolServer's OWN access token, sealed in `channel_credentials` (the one
-- sealed-secret store, SPEC §D.8) as kind `tool_server_token`, which is bound into the
-- seal as additional authenticated data — so a model key's ciphertext moved onto this
-- row fails to open instead of being presented to a cluster-facing server.
--
-- ⭐⭐ `access` IS DECLARED, read OR write, AND IT IS THE GATE. Only a `read` ToolServer's
-- Tools may sit on an Investigator's allowlist (ADR 0053 §3: "while investigating it
-- holds only read-only Tools"); the service refuses the rest when the allowlist is
-- written and again when a run starts. A `write` ToolServer is configured and discovered
-- like any other and is never offered to a model: it is where a Remedy's write Tool will
-- be bound (ADR 0054, git-bug 4148256) and the thing its approval grant is held on.
-- MCP's `readOnlyHint` is kept on each Tool row for display and never consulted: it is
-- the server's claim about itself.
--
-- ⭐ THE PER-CALL CONTROLS LIVE HERE (ADR 0053 §6: "Per-call timeout and result size cap —
-- each Tool call"). The operator knows a ToolServer's latency and verbosity; a timeout or
-- a truncation is recorded on the Step and the run continues.
--
-- ⭐ `tool_server_tools` IS A SNAPSHOT, REPLACED WHOLE ON EACH SUCCESSFUL DISCOVERY. A
-- Step names a Tool by its qualified name (`<toolserver>__<tool>`), never by a row here,
-- so replacing the list rewrites no transcript. A failed discovery keeps the last good
-- list and records why on the ToolServer row.
--
-- ⛔ NO stdio. There is no column for a command: oto runs no subprocess, and a ToolServer
-- is a server, never a program oto starts.
--
-- ⛔ NO DEFAULT now() ON ANY TIMESTAMP. The application stamps every one from the injected
-- clock (CONTEXT.md §6, migrations 00032-00034).
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Two new tables release N neither reads nor writes,
-- and a wider kind CHECK release N never writes the new value into.

-- +goose Up

CREATE TABLE tool_servers (
  id                  UUID        PRIMARY KEY,
  org_id              UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  -- ⭐ No underscore: the first `__` of a qualified Tool name is the separator.
  name                TEXT        NOT NULL,
  url                 TEXT        NOT NULL,
  transport           TEXT        NOT NULL,
  access              TEXT        NOT NULL,
  -- NULL: the ToolServer takes no token (on the cluster network), and none is sent.
  credential_id       UUID        REFERENCES channel_credentials(id) ON DELETE SET NULL,
  call_timeout_s      INT         NOT NULL,
  max_result_bytes    INT         NOT NULL,
  discovered_at       TIMESTAMPTZ,
  discovery_failed_at TIMESTAMPTZ,
  discovery_error     TEXT,
  created_at          TIMESTAMPTZ NOT NULL,
  updated_at          TIMESTAMPTZ NOT NULL,
  CONSTRAINT tool_servers_name_ck      CHECK (name ~ '^[a-z]([a-z0-9-]{0,22}[a-z0-9])?$' AND name <> 'oto'),
  -- ⛔ No query, no fragment, no userinfo: a token in a URL is a token in a shown column.
  CONSTRAINT tool_servers_url_ck       CHECK (url ~ '^https?://[^/?#@]+(/|$)' AND url !~ '[?#]' AND length(url) <= 2048),
  CONSTRAINT tool_servers_transport_ck CHECK (transport IN ('streamable_http','sse')),
  CONSTRAINT tool_servers_access_ck    CHECK (access IN ('read','write')),
  -- ⛔ A token travels only over TLS, as domain.TokenNeedsHTTPS says.
  CONSTRAINT tool_servers_token_tls_ck CHECK (credential_id IS NULL OR url LIKE 'https://%'),
  CONSTRAINT tool_servers_timeout_ck   CHECK (call_timeout_s BETWEEN 1 AND 120),
  CONSTRAINT tool_servers_result_ck    CHECK (max_result_bytes BETWEEN 1024 AND 61440),
  CONSTRAINT tool_servers_failure_ck   CHECK (
    (discovery_failed_at IS NULL) = (discovery_error IS NULL)
    AND (discovery_error IS NULL OR char_length(discovery_error) BETWEEN 1 AND 2000)
  ),
  CONSTRAINT tool_servers_time_ck      CHECK (updated_at >= created_at)
);

-- Serves: the settings list (every ToolServer in one org, by name), a run's lookup of the
-- ToolServers its allowlist names, and the uniqueness of a name within an org.
CREATE UNIQUE INDEX tool_servers_org_name_uniq ON tool_servers (org_id, name);

-- +goose StatementBegin
COMMENT ON TABLE tool_servers IS
  'The MCP servers an org''s operator runs and oto reads the cluster through (ADR 0016, 0053, 0054 §5; git-bug 2e9a086). oto holds no cluster credential: only the ToolServer''s own access token, sealed in channel_credentials. access is DECLARED: only a read ToolServer''s Tools may be on an Investigator''s allowlist; a write ToolServer is never offered to a model and is where a Remedy''s write Tool is bound. call_timeout_s and max_result_bytes are ADR 0053 §6''s per-call controls.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN tool_servers.credential_id IS
  'The sealed access token: ONE channel_credentials row of kind tool_server_token, values {token}. NULL sends no token. Nothing returns it: the settings read carries only whether a token is stored.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN tool_servers.access IS
  'Declared by the operator, never inferred from a Tool''s readOnlyHint. read: its Tools may be held by an Investigator. write: never offered to a model; reserved for executing an approved Remedy (ADR 0054 §5).';
-- +goose StatementEnd

CREATE TABLE tool_server_tools (
  org_id         UUID    NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  tool_server_id UUID    NOT NULL REFERENCES tool_servers(id) ON DELETE CASCADE,
  name           TEXT    NOT NULL,
  description    TEXT    NOT NULL,
  -- NULL: the ToolServer listed a schema oto cannot offer a model (not an object, or past
  -- 64 KiB). The Tool is recorded and shown as unusable rather than silently dropped.
  input_schema   JSONB,
  read_only_hint BOOLEAN,
  CONSTRAINT tool_server_tools_pk        PRIMARY KEY (org_id, tool_server_id, name),
  CONSTRAINT tool_server_tools_name_ck   CHECK (char_length(name) BETWEEN 1 AND 128),
  CONSTRAINT tool_server_tools_desc_ck   CHECK (char_length(description) <= 4096),
  CONSTRAINT tool_server_tools_schema_ck CHECK (input_schema IS NULL OR jsonb_typeof(input_schema) = 'object')
);

-- +goose StatementBegin
COMMENT ON TABLE tool_server_tools IS
  'The Tools a ToolServer listed at its last successful discovery, replaced whole each time. A Step names a Tool by its qualified name <toolserver>__<tool>, never by a row here. read_only_hint is the server''s own claim, shown and never consulted.';
-- +goose StatementEnd

ALTER TABLE channel_credentials DROP CONSTRAINT channel_credentials_kind_ck;
ALTER TABLE channel_credentials ADD CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
  ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','webhook_signing_secret',
   'webhook_mapping_secrets','model_api_key','tool_server_token','none'));

-- +goose Down

-- The tokens go with the table that pointed at them: below this migration the kind CHECK
-- does not admit them, and a sealed token nothing references is nobody's.
DROP TABLE tool_server_tools;
DROP TABLE tool_servers;
DELETE FROM channel_credentials WHERE kind = 'tool_server_token';

-- Byte-identical to what 00095 shipped.
ALTER TABLE channel_credentials DROP CONSTRAINT channel_credentials_kind_ck;
ALTER TABLE channel_credentials ADD CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
  ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','webhook_signing_secret',
   'webhook_mapping_secrets','model_api_key','none'));
