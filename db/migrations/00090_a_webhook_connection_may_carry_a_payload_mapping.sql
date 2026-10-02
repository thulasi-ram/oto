-- ADR 0055 §2: A PLUGIN IS DATA -- A PAYLOAD MAPPING ON THE WEBHOOK CONNECTION (git-bug
-- 2205620). A webhook Connection may carry a document that renders the request body and
-- headers from the `oto.notification.v1` envelope for each fact, and may name where in a
-- 2xx response an incident tool's link and id are found. Without one the plain envelope
-- is sent, exactly as before; with one, an incident tool whose API wants another JSON
-- shape (incident.io wants top-level `title` and `status`) is reached without a bridge.
--
-- ⭐⭐ IT LIVES ON THE CONNECTION, NOT IN `notification_templates` (ADR 0055 §6). A
-- template is wording and falls back to oto's own card on any failure; a mapping decides
-- what an incident tool DOES, and a mapping that fails is a failed delivery -- never the
-- plain envelope, which the vendor could not parse. It is destination setup, configured
-- with the Connection, so it is a column of the Connection.
--
-- ⭐⭐ A MAPPING NEVER HOLDS A SECRET, SO THE SECRETS GET A SLOT OF THEIR OWN. A vendor key
-- that has to travel in the body (PagerDuty's `routing_key`) is sealed in ONE
-- `channel_credentials` row of kind `webhook_mapping_secrets`, values name -> secret,
-- referenced by `mapping_credential_id` -- a third slot beside 00088's two, for 00088's
-- reason: it answers a different question (what the BODY carries, not what gets oto in
-- or proves the body is oto's) and is replaced on its own schedule. The mapping refers
-- to a secret as `{{ secrets.<name> }}` and oto fills it in at the moment of sending, so
-- `payload_mapping` and the delivery row that records the body hold only the name.
--
-- ⭐ THE NAMES ARE KEPT IN THE CLEAR, BESIDE THE SLOT. Saving a mapping that references a
-- secret the Connection does not hold is refused, and the API that checks it has no way
-- to unseal anything (channels/api's CredentialWriter has no read method, on purpose).
-- `mapping_secret_names` is what it checks against, written in the same UPDATE as the
-- slot, and read as meaningless once the slot is NULL.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Three nullable columns with no default and a wider
-- kind CHECK: a release-N pod's INSERTs and UPDATEs never name them and stay valid, and
-- it reads a mapped Connection as an unmapped one for the length of the rollout -- which
-- would send the plain envelope to a vendor that cannot parse it, so the operator adds a
-- mapping AFTER the rollout, not during it (docs/setup/webhook.md says so).

-- +goose Up

ALTER TABLE channel_connections
  ADD COLUMN payload_mapping       JSONB,
  ADD COLUMN mapping_credential_id UUID REFERENCES channel_credentials(id) ON DELETE SET NULL,
  ADD COLUMN mapping_secret_names  TEXT[];

-- Only a webhook carries a mapping, and a mapping is one JSON object. Its shape beyond
-- that is checked by channels/service.ValidateMapping, which renders it against an
-- envelope for every fact before it may be saved: no CHECK can render Liquid.
ALTER TABLE channel_connections
  ADD CONSTRAINT channel_connections_mapping_ck
    CHECK (payload_mapping IS NULL OR (type = 'webhook' AND jsonb_typeof(payload_mapping) = 'object')),
  ADD CONSTRAINT channel_connections_mapping_secrets_ck
    CHECK (mapping_credential_id IS NULL OR type = 'webhook'),
  -- The third slot never names a row another slot names, for 00088's reason: replacing
  -- the mapping secrets would otherwise rotate a bearer token or a signing secret too.
  ADD CONSTRAINT channel_connections_mapping_distinct_ck
    CHECK (mapping_credential_id IS NULL
           OR ((credential_id IS NULL OR mapping_credential_id <> credential_id)
               AND (signing_credential_id IS NULL OR mapping_credential_id <> signing_credential_id))),
  ADD CONSTRAINT channel_connections_mapping_names_ck
    CHECK (mapping_secret_names IS NULL OR cardinality(mapping_secret_names) BETWEEN 1 AND 16);

-- +goose StatementBegin
COMMENT ON COLUMN channel_connections.payload_mapping IS
  'A webhook connection''s payload mapping (ADR 0055 §2): Liquid sources over the oto.notification.v1 envelope rendering the request body (a default `body` and per-fact `facts`), optional `headers`, and an optional `response` path naming where a 2xx answer carries external_url/external_id. NULL sends the plain envelope. Never holds a secret: a secret is referenced as secrets.<name> and filled from mapping_credential_id at send. Destination setup, not wording -- a mapping that fails is a dead config_invalid delivery, never a fallback to the envelope.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_connections.mapping_credential_id IS
  'The ONE sealed row (kind webhook_mapping_secrets, values name -> secret) holding the secrets a payload mapping references, e.g. PagerDuty''s routing_key (00090). Replaced whole, never merged: nothing outside the send path unseals it. ON DELETE SET NULL for the reason credential_id gives.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_connections.mapping_secret_names IS
  'The names sealed in mapping_credential_id, in the clear, so saving a mapping can refuse a reference to a secret the connection does not hold without unsealing anything. Written in the same UPDATE as the slot; meaningless when the slot is NULL.';
-- +goose StatementEnd

ALTER TABLE channel_credentials DROP CONSTRAINT channel_credentials_kind_ck;
ALTER TABLE channel_credentials ADD CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
  ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','webhook_signing_secret',
   'webhook_mapping_secrets','none'));

-- +goose Down

-- ⛔ INTEGRITY CHECK: NO CONNECTION MAY CARRY A MAPPING. Below this migration a webhook
-- Connection sends the plain envelope, and a Connection with a mapping points at a
-- vendor that cannot parse one -- so rolling back under it would turn every delivery
-- into a 4xx at best and a silently missing incident at worst (ADR 0055 §2: "never
-- falls back to the plain envelope"). An operator removes the mappings, knowing which
-- receivers that breaks, before rolling back.
-- +goose StatementBegin
DO $$
DECLARE mapped BIGINT;
BEGIN
  SELECT count(*) INTO mapped FROM channel_connections WHERE payload_mapping IS NOT NULL;

  IF mapped > 0 THEN
    RAISE EXCEPTION
      'migration 00090 down: % connection(s) carry a payload mapping. The release below 00090 sends the plain oto.notification.v1 envelope to those receivers, which they cannot parse. Remove each mapping (PATCH /api/v1/channel-connections/{id} with payload_mapping null) before rolling back.',
      mapped;
  END IF;
END $$;
-- +goose StatementEnd

-- Secrets with no mapping to read them are nobody's: the slot is cleared and the rows go,
-- because the kind CHECK below no longer admits them.
UPDATE channel_connections SET mapping_credential_id = NULL WHERE mapping_credential_id IS NOT NULL;
DELETE FROM channel_credentials WHERE kind = 'webhook_mapping_secrets';

ALTER TABLE channel_connections
  DROP CONSTRAINT channel_connections_mapping_names_ck,
  DROP CONSTRAINT channel_connections_mapping_distinct_ck,
  DROP CONSTRAINT channel_connections_mapping_secrets_ck,
  DROP CONSTRAINT channel_connections_mapping_ck,
  DROP COLUMN mapping_secret_names,
  DROP COLUMN mapping_credential_id,
  DROP COLUMN payload_mapping;

-- Byte-identical to what 00075 shipped.
ALTER TABLE channel_credentials DROP CONSTRAINT channel_credentials_kind_ck;
ALTER TABLE channel_credentials ADD CONSTRAINT channel_credentials_kind_ck CHECK (kind IN
  ('slack_bot_token','slack_app_token','slack_signing_secret','basic','bearer','webhook_signing_secret','none'));
