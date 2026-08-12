-- A CLAIMED IDEMPOTENCY KEY. It records THAT a key was used and WHAT it made.
-- It holds no response, and it holds no secret, and both of those are the design.
--
-- ⭐⭐ WHY. `api/openapi/openapi.yaml` declares `parameters/IdempotencyKeyHeader`
-- on 28 operations, and SPEC §E.1 promises it flatly: "Every mutating endpoint
-- accepts `Idempotency-Key`". The server read it on ZERO of them. The only
-- mention of the header anywhere in the process was the CORS allow-list — it was
-- let through the door and then dropped on the floor.
--
-- ⛔ WHAT IT COST. `createApiToken` is the one endpoint in oto whose 201 carries a
-- plaintext credential, and it is `POST` with no natural key. A retry — a dropped
-- response, a proxy timeout, a double-clicked button — MINTED A SECOND LIVE
-- TOKEN. The secret of that second token was written into a response that may
-- never have arrived, so the outcome was a working credential nobody holds, in
-- nobody's inventory, that nobody knows to revoke. `rotateSourceIngestToken` is
-- the same defect on the same shape: it mints an ingest secret for a source, and
-- a retried rotation leaves a live ingest credential behind the same way.
-- `createSource` is the third, and it was the most deceptive of them: it declared
-- the header, read nothing, and was safe only because
-- `alert_sources_name_uniq (org_id, name)` happens to refuse a second create
-- under the same name — an accident that protects nothing the moment the name is
-- generated or the retry uses a different one. The contract advertising the
-- protection made all three worse, because a careful client retries BELIEVING it
-- is safe.
--
-- ⭐⭐ WHAT THIS TABLE DELIBERATELY DOES NOT DO, AND WHY IT IS NOT A SHORTCUT.
-- The obvious implementation of the header's own wording — "returns the original
-- result rather than acting twice" — is a response cache: store the 201 body,
-- replay it. For `createApiToken` that body IS THE PLAINTEXT SECRET, so a table
-- built to prevent ONE orphaned credential would become a table holding EVERY
-- credential, in the clear, addressable by a string the client chose. That is a
-- strictly worse security posture than the bug it fixes, and it would undo the
-- property `identity/domain.APIToken` is built around: there is no field on that
-- type that could hold plaintext, so "the secret is never stored" is a fact about
-- the type and not a habit of its callers. It stays that way. There is no column
-- here for a response body and there must never be one.
--
-- ⭐ SO THE REPLAY IS ANSWERED HONESTLY INSTEAD. A second call with the same key
-- and the same body does not mint anything; it is told, with a refining code and
-- the id of what the FIRST call created, that the first attempt succeeded and
-- that oto genuinely cannot show the secret twice. A caller who never received it
-- revokes that id and issues again. That is one extra step for a caller in a rare
-- failure, in exchange for it being IMPOSSIBLE to end up with a live credential
-- outside anyone's inventory. Same key, different body is the `409` the contract
-- already promised (SPEC §L.1).
--
-- ⛔ IT IS UNPARTITIONED, AND IT JOINS THE SIBLINGS THAT ALREADY ARE. A UNIQUE
-- index on a partitioned table must include the partition key, so it can only
-- enforce uniqueness WITHIN a partition — see `ingest_dedup` in
-- 00006_ingestion.sql, which says exactly this and for exactly this reason
-- (conflict ruling 14). A client-supplied idempotency key has no partition key it
-- could carry, and its uniqueness must hold across all of time, or a retry that
-- straddled midnight would mint the second credential this table exists to
-- prevent. So it is a small, aggressively-pruned side table, and it joins
-- `ingest_dedup`, `alert_event_keys`, `sessions` and `enrichment_cache` as a row
-- that `retention.prune` deletes by hand rather than by dropping a partition
-- (ADR 0024).
--
-- ⭐ THE RETENTION WINDOW IS 24 HOURS, and the contract states no number, so here
-- is the argument for this one.
--
-- The window's FLOOR is how long one logical request can keep being retried by
-- somebody who still thinks it might not have happened: an HTTP client's own
-- retry budget, a proxy that gave up and was re-driven, a queued mobile client
-- that drains when the network comes back, a person who closed the laptop and
-- came back after lunch to press the button again. Minutes are not enough for the
-- last two. Its CEILING is that a claim which outlives the caller's memory of
-- making it protects nobody, and every row is rent paid on a table that must stay
-- small enough to prune cheaply. 24 hours is also the window every mainstream
-- payments API publishes, so a client library's default assumption is already
-- right against oto.
--
-- ⛔ IT MUST NOT BE READ AS A SECOND `DedupTTL`, AND IT DOES NOT CONTRADICT IT.
-- `ingestion/domain.DedupTTL` is FIVE MINUTES and is bounded from ABOVE by
-- `refire_grace`: it suppresses SIGNAL, so a window as wide as the grace period
-- makes the T8 re-fire path unreachable — an alert firing again inside the window
-- is dropped at ingest and the state machine never sees it. That ceiling is the
-- whole of its reasoning and it applies to nothing here. This window suppresses no
-- signal. It suppresses a second MINTING of a credential by an authenticated
-- operator, an act that is never a repeat of a real-world event the way a re-fire
-- is, and no state machine is waiting to see it. The two numbers are 288 times
-- apart precisely so that nobody ever reads one as the other, and neither one
-- constrains the other in either direction.
--
-- ⭐ THE PRICE, stated plainly. THE KEY IS SCOPED PER PRINCIPAL, so two members of
-- one org who happen to generate the same key both succeed and both get a token.
-- That is deliberate: a key is a CLIENT's private handle on ITS OWN retry, and a
-- shared-per-org namespace would let one member's key silently block another
-- member's create — a denial of service, and an oracle telling them a key they
-- guessed was in use. It also means a caller who rotates credentials cannot reuse
-- a key across two principals, which nobody wants to do.
--
-- ⭐ AND IT IS SCOPED PER OPERATION, so one key cannot be replayed across two
-- DIFFERENT endpoints. Without `operation` in the tuple, a client that reuses one
-- key per user gesture — which is exactly what oto's own frontend does — would
-- find its `revokeApiToken` refused because the same gesture's `createApiToken`
-- had already claimed the key, and the refusal would name a resource from the
-- wrong endpoint. The key answers "is this the same request", and two different
-- operations are never the same request.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One new table, nothing altered, nothing to
-- backfill. A release-N process that has never heard of this table simply does
-- not claim keys, which is the behaviour it has today.

-- +goose Up

CREATE TABLE idempotency_claims (
  -- The tenant. First column of the primary key, like every other composite index
  -- in this schema, and CASCADE because a claim is a transport fact about a
  -- request made INTO one tenant and means nothing once the tenant is gone.
  org_id          UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,

  -- The acting principal (`authn.Principal.UserID` for a session or a PAT).
  principal_id    UUID        NOT NULL,

  -- The contract operationId the key was claimed for.
  operation       TEXT        NOT NULL,

  -- The client's own string, stored verbatim.
  idempotency_key TEXT        NOT NULL,

  -- sha256 of the request body, which is how "the same body" is decided.
  request_hash    BYTEA       NOT NULL,

  -- What the first call created, if it created anything.
  created_ref     UUID,

  -- The prune horizon.
  claimed_at      TIMESTAMPTZ NOT NULL,

  CONSTRAINT idempotency_claims_pk PRIMARY KEY (org_id, principal_id, operation, idempotency_key),

  -- The contract's own bound on the header (`minLength: 1, maxLength: 200`) and
  -- nothing more. ⛔ THIS IS NOT `ingest_dedup_key_ck`. That key is sha256 hex
  -- because OTO computes it; this one is an opaque string a CLIENT chose, so a
  -- shape CHECK here would reject keys the contract promises to accept — and a
  -- CHECK stricter than layer 1 is a 23514 surfacing as a 500 where nothing at all
  -- belongs (SPEC §L.1). It is not trimmed either: leading whitespace is part of
  -- the caller's key, and normalising would make two different keys collide.
  CONSTRAINT idempotency_claims_key_ck  CHECK (length(idempotency_key) BETWEEN 1 AND 200),

  -- An operationId, which oto supplies and a caller never does: lowerCamelCase,
  -- bounded so a handler that passed a URL path or a whole method+path string
  -- fails at the write rather than quietly splitting one operation's key space in
  -- two. Mirrored by `idempotency.PatternOperation`.
  CONSTRAINT idempotency_claims_op_ck   CHECK (operation ~ '^[a-z][a-zA-Z0-9]{0,63}$'),

  -- A sha256 digest, exactly 32 bytes, like every other digest column in this
  -- schema. A shorter value would silently be a different hash function.
  CONSTRAINT idempotency_claims_hash_ck CHECK (octet_length(request_hash) = 32)
);

-- Serves: the `retention.prune` sweep, and only that. The claim itself rides the
-- PRIMARY KEY as an INSERT ... ON CONFLICT DO NOTHING, never a read-then-write
-- race. Deliberately NOT led by org_id: the pruner sweeps the whole table by age
-- and has no tenant to lead with, which is the same shape as
-- `ingest_dedup_prune_idx`.
CREATE INDEX idempotency_claims_prune_idx ON idempotency_claims (claimed_at);

COMMENT ON TABLE idempotency_claims IS
  'One claimed client-supplied `Idempotency-Key` (SPEC §E.1, api/openapi/openapi.yaml parameters/IdempotencyKeyHeader). It records THAT a key was used, the hash of the body it was used with, and the id of what that call created — NEVER a response body and NEVER a secret, because the one endpoint whose response carries a plaintext credential is the endpoint this table exists to protect. DELIBERATELY UNPARTITIONED for the reason ingest_dedup is (conflict ruling 14): a UNIQUE index on a partitioned table cannot enforce uniqueness across partitions, and a client-supplied key has no partition key to carry. Pruned by row in retention.prune at claimed_at < now() - 24h (ADR 0024), joining ingest_dedup, sessions and enrichment_cache.';

COMMENT ON COLUMN idempotency_claims.org_id IS
  'The tenant, and the first column of the key like every composite index in this schema (CONTEXT.md §5.7). CASCADE because a claim is a transport fact about a request made INTO one tenant: it protects a retry that is already over once the tenant is gone.';

COMMENT ON COLUMN idempotency_claims.principal_id IS
  'The acting principal — authn.Principal.UserID for a session or a PAT. IN THE KEY ON PURPOSE: a key is a client''s private handle on its own retry, so one org member''s key must not be able to block another''s create, which would be both a denial of service and an oracle for guessed keys. Deliberately NOT a FK to users: a Principal is not always a user row (SPEC §E.1 also has ingest, slack and system kinds), and a short-lived transport fact must neither keep a users row alive nor disappear when one goes. The org FK already bounds its lifetime.';

COMMENT ON COLUMN idempotency_claims.operation IS
  'The contract operationId this key was claimed for, e.g. createApiToken. IN THE KEY ON PURPOSE: one key must not be replayable across two DIFFERENT operations. A client that mints one key per user gesture — which oto''s own frontend does — would otherwise have its second call refused by its first, and told about a resource from the wrong endpoint.';

COMMENT ON COLUMN idempotency_claims.idempotency_key IS
  'The caller''s string, verbatim and untrimmed (SPEC §E.1: 1..200 characters). Opaque to oto: it is compared, never parsed.';

COMMENT ON COLUMN idempotency_claims.request_hash IS
  'sha256 of the request body as received, and the ONLY thing that decides "the same body". A replay whose hash matches is the caller''s own retry; a replay whose hash differs is a 409 (SPEC §L.1), because the two calls cannot both be the request this key names. The BODY ITSELF IS NOT STORED — the hash answers the whole question, and a stored body would be a copy of every token name and every request an operator ever sent.';

COMMENT ON COLUMN idempotency_claims.created_ref IS
  'The id of the row the first call created — an api_tokens.id or an alert_sources.id today — so a replay can name what already exists instead of leaving the caller to guess. NULL when the operation creates nothing (a revoke) or when nothing was created. ⛔ IT IS AN ID AND NEVER A SECRET, and there is no column here that could hold one. Deliberately not a FK: this table is transport machinery shared by every module, and a FK would bind it to one module''s table.';

COMMENT ON COLUMN idempotency_claims.claimed_at IS
  'When the key was claimed, and the prune horizon. 24 hours, which is wide enough for a queued client draining after a network outage and an operator returning to a half-finished page, and is NOT a second DedupTTL: that window is five minutes because it suppresses SIGNAL and must stay strictly inside refire_grace (ingestion/domain.DedupTTL). This one suppresses a second credential mint, which no state machine is waiting to observe, so that ceiling does not apply.';

COMMENT ON CONSTRAINT idempotency_claims_pk ON idempotency_claims IS
  'One claim per (tenant, principal, operation, key). The tuple is the whole security property: two tenants cannot collide, two principals cannot block each other, and one key cannot be replayed across two different operations.';

COMMENT ON CONSTRAINT idempotency_claims_key_ck ON idempotency_claims IS
  'The contract''s bound on Idempotency-Key (minLength 1, maxLength 200) and nothing more. NOT ingest_dedup_key_ck''s sha256-hex shape: that key is oto-computed and this one is whatever the client chose, so any shape CHECK here would reject a key the contract promises to accept and turn it into a 500.';

COMMENT ON CONSTRAINT idempotency_claims_op_ck ON idempotency_claims IS
  'An operationId, which oto supplies and a caller never does. Bounded so a handler that passed a URL path or a method+path string fails at the write rather than quietly splitting one operation''s key space in two. Mirrored byte for byte by idempotency.PatternOperation.';

COMMENT ON CONSTRAINT idempotency_claims_hash_ck ON idempotency_claims IS
  'A sha256 digest, exactly 32 bytes, like every other digest column in this schema (SPEC §L.9). A shorter value would silently be a different hash function, and "the same body" would stop meaning anything.';

-- +goose Down

-- ⭐ THE DOWN IS TOTAL AND LOSES NOTHING THAT MATTERS. A claim is a short-lived
-- transport fact with a 24-hour horizon and no reader outside the request that
-- made it; there is no history here to preserve and nothing references these rows.
-- ⛔ WHAT IT DOES LOSE is the protection itself: while the table is absent, a
-- retried create mints a second live credential again. That is a property of
-- rolling back to a release that never claimed keys, not of this statement.

DROP TABLE idempotency_claims;
