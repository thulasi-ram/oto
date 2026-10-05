-- ADR 0055 §1: THE EVENT CONTRACT IS THE PLUGIN API, AND ITS SIGNATURE HAS TO BE USABLE
-- BY SOMEONE WHO DID NOT WRITE IT (git-bug 2765f74). 00075 let a webhook Connection carry
-- a `webhook_signing_secret`, and the provider has signed every body with it since. Two
-- things kept that signature from being one a stranger's receiver could rely on, and both
-- are storage:
--
--   1. A Connection held ONE credential (`channel_connections.credential_id`, ADR 0047 "a
--      webhook connection's shared credential is one row, one kind"). A receiver that
--      needs a bearer token -- incident.io's HTTP alert source does -- could not also be
--      signed, so the signature was unavailable for exactly the incident tools 0055 is
--      for. ADR 0047 deferred the second slot "for a case that does not exist yet"; this
--      is that case.
--   2. Rotating the secret re-sealed it in place, so the next delivery was signed with the
--      new secret only and every receiver still holding the old one rejected it. A
--      signature that cannot rotate is a signature that gets turned off the first time a
--      secret leaks.
--
-- ⭐⭐ THE SIGNING SECRET GETS ITS OWN SLOT ON THE CONNECTION, NOT A SECOND KIND IN THE
-- FIRST ONE. `signing_credential_id` is a second reference into the same sealed store,
-- with the same ON DELETE SET NULL, rather than -- the alternative the ticket offered -- a
-- signing secret folded into the `credential_id` row's sealed values beside a token. The
-- two answer opposite questions (the credential gets oto INTO the receiver; the signing
-- secret lets the receiver PROVE a body came from oto), they rotate on different
-- schedules, and the second one has an overlap the first must never have: a leaked bearer
-- token is revoked on the spot, while a signing secret has to keep signing beside its
-- successor until every receiver has the new one. One row per secret keeps each
-- rotation one UPDATE of one row, and keeps `channel_credentials.kind` a true statement
-- about what is sealed in it.
--
-- ⭐⭐ THE OVERLAP LIVES ON THE CREDENTIAL ROW, AS CIPHERTEXT, AND IS MOVED BY SQL. A
-- rotation of a `webhook_signing_secret` copies the row's CURRENT `sealed` and
-- `key_version` into `previous_sealed` / `previous_key_version` in the same UPDATE that
-- seals the new value, and stamps `previous_until` (the rotation instant plus
-- `domain.SigningSecretOverlap`, 24 hours). Postgres evaluates every SET expression
-- against the OLD row, so the outgoing secret is never unsealed to be carried forward --
-- the bytes move, the plaintext does not exist. Until `previous_until`, the provider signs
-- with both secrets and sends both signatures; after it, the old one simply stops being
-- used. A second rotation inside the overlap replaces the previous secret with the one
-- it is retiring: at most two signatures, ever.
--
-- ⛔ ONLY A SIGNING SECRET MAY HAVE A PREVIOUS ONE. `channel_credentials_previous_ck`
-- says so: an overlap on a bearer token or a Slack bot token would be a revoked secret
-- that still works, which is the opposite of what rotating one is for.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Every column is nullable with no default, so a
-- release-N pod's INSERTs and UPDATEs never name them and stay valid. The one data move
-- -- an existing signing secret leaves `credential_id` for `signing_credential_id` -- is
-- read by release N+1 only; a release-N pod reading a moved row sees an unsigned,
-- unauthenticated connection for the length of the rollout, which is the honest
-- reading of a row it no longer understands rather than a wrong one.

-- +goose Up

ALTER TABLE channel_connections
  ADD COLUMN signing_credential_id UUID REFERENCES channel_credentials(id) ON DELETE SET NULL;

-- Only a webhook signs: Slack's signing secret is INBOUND (it verifies Slack to oto) and
-- is a `slack_signing_secret`, a different thing entirely. And the two slots never name
-- one row: rotating the auth credential would otherwise rotate the signature with it, and
-- the overlap would be granted to a bearer token.
ALTER TABLE channel_connections
  ADD CONSTRAINT channel_connections_signing_ck
    CHECK (signing_credential_id IS NULL OR type = 'webhook'),
  ADD CONSTRAINT channel_connections_signing_distinct_ck
    CHECK (signing_credential_id IS NULL OR credential_id IS NULL OR signing_credential_id <> credential_id);

-- A signing secret 00075 stored in the ONE slot moves to its own. Nothing else could have
-- been in the slot beside it, so `credential_id` becomes NULL: the connection was
-- unauthenticated and signed before, and it is unauthenticated and signed after.
UPDATE channel_connections cx
   SET signing_credential_id = cx.credential_id,
       credential_id         = NULL
  FROM channel_credentials cc
 WHERE cc.id = cx.credential_id
   AND cc.org_id = cx.org_id
   AND cc.kind = 'webhook_signing_secret';

-- +goose StatementBegin
COMMENT ON COLUMN channel_connections.credential_id IS
  'The shared secret every channel under this connection AUTHENTICATES with: a Slack bot token, or a webhook basic/bearer credential. Never a webhook_signing_secret since 00088 -- that is signing_credential_id. ON DELETE SET NULL rather than CASCADE for the same reason channels.credential_id was: losing the secret must not lose the record of the connection.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_connections.signing_credential_id IS
  'A webhook connection''s OUTBOUND signing secret (kind webhook_signing_secret, enforced by channels/api since no CHECK can see across the two tables), beside -- never instead of -- credential_id, so one receiver can require a bearer token AND verify X-Oto-Signature (ADR 0055 §1, 00088). NULL means the connection does not sign. ON DELETE SET NULL for the reason credential_id gives.';
-- +goose StatementEnd

ALTER TABLE channel_credentials
  ADD COLUMN previous_sealed      BYTEA,
  ADD COLUMN previous_key_version INT,
  ADD COLUMN previous_until       TIMESTAMPTZ;

ALTER TABLE channel_credentials
  ADD CONSTRAINT channel_credentials_previous_ck CHECK (
       (previous_sealed IS NULL AND previous_key_version IS NULL AND previous_until IS NULL)
    OR (previous_sealed IS NOT NULL AND previous_key_version >= 1 AND previous_until IS NOT NULL
        AND kind = 'webhook_signing_secret'));

-- +goose StatementBegin
COMMENT ON COLUMN channel_credentials.previous_sealed IS
  'The signing secret this row held before its last rotation, still SEALED, under previous_key_version. Moved here by the rotating UPDATE itself (SET expressions read the old row), so the outgoing secret is never unsealed to be carried forward. Only a webhook_signing_secret has one (channel_credentials_previous_ck): a revoked bearer token that keeps working is not an overlap, it is a leak.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_credentials.previous_until IS
  'When previous_sealed stops signing: the rotation instant plus domain.SigningSecretOverlap (24 hours). Until then every outbound webhook body carries both signatures and a receiver holding either secret verifies it; after it the column is simply ignored, not cleared -- the next rotation overwrites it.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ INTEGRITY CHECK: NO CONNECTION MAY HOLD BOTH SLOTS. Below this migration a connection
-- has one credential, and there is no honest choice of which of a bearer token and a
-- signing secret it keeps: dropping the token stops the receiver accepting oto's requests,
-- dropping the secret stops it trusting them. An operator decides that, not a migration.
-- +goose StatementBegin
DO $$
DECLARE both_slots BIGINT;
BEGIN
  SELECT count(*) INTO both_slots FROM channel_connections
   WHERE credential_id IS NOT NULL AND signing_credential_id IS NOT NULL;

  IF both_slots > 0 THEN
    RAISE EXCEPTION
      'migration 00088 down: % connection(s) carry both an authentication credential and a signing secret. The release below 00088 has one credential slot per connection, and there is no honest choice of which to keep. Detach one (PATCH /api/v1/channel-connections/{id} with credential or signing_credential of kind none) before rolling back.',
      both_slots;
  END IF;
END $$;
-- +goose StatementEnd

-- The guard above makes credential_id NULL wherever signing_credential_id is set, so the
-- secret goes back to the one slot 00075 kept it in.
UPDATE channel_connections
   SET credential_id = signing_credential_id
 WHERE signing_credential_id IS NOT NULL;

ALTER TABLE channel_connections
  DROP CONSTRAINT channel_connections_signing_distinct_ck,
  DROP CONSTRAINT channel_connections_signing_ck,
  DROP COLUMN signing_credential_id;

-- Byte-identical to what 00075 shipped, so a rolled-back database matches its own history.
-- +goose StatementBegin
COMMENT ON COLUMN channel_connections.credential_id IS
  'The shared secret every channel under this connection uses: a Slack bot token, or a webhook basic/bearer credential or signing secret. ON DELETE SET NULL rather than CASCADE for the same reason channels.credential_id was: losing the secret must not lose the record of the connection.';
-- +goose StatementEnd

-- A secret mid-overlap loses its predecessor here: the release below signs with one
-- secret, and receivers still on the old one start rejecting -- which is exactly the
-- pre-00088 behaviour this rollback asks for.
ALTER TABLE channel_credentials
  DROP CONSTRAINT channel_credentials_previous_ck,
  DROP COLUMN previous_until,
  DROP COLUMN previous_key_version,
  DROP COLUMN previous_sealed;
