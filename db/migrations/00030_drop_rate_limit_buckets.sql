-- Drop `rate_limit_buckets`: a table nothing has ever read or written.
--
-- ⭐⭐ WHY. 00014_ops.sql created it, gave it four CHECK constraints and six
-- COMMENTs, and SPEC §G.6 described the mechanism in the present tense:
--
--   "Slack rate limiting is additionally gated BEFORE the call by a Postgres
--    token bucket in `rate_limit_buckets`, keyed slack:{team_id}:{channel_id},
--    capacity 3, refill 1/s."
--
-- There is no such gate. Not one Go file in the tree names the table. The only
-- limiter oto has is `internal/platform/ratelimit`, it is IN-PROCESS, and its own
-- doc comment says plainly that across N replicas a client gets N times the
-- budget. So a maintainer reading either the DDL or the SPEC concluded the Slack
-- write budget was shared across pods, and it is per-pod — which is exactly the
-- belief that gets a workspace rate-limited by an autoscaler.
--
-- A schema is a claim about what the system does. An empty table with authoritative
-- comments is the most expensive kind of documentation debt: it survives every
-- grep for dead code, because there is no code.
--
-- ⛔ WHY DROPPING RATHER THAN IMPLEMENTING. A shared bucket would put a write on
-- the outbound delivery path, in the same database the alert pipeline needs, to
-- protect a third party's rate limit — and `platform/ratelimit` already argues
-- (for the login path, where the stakes are higher) that a Postgres-backed bucket
-- trades one denial of service for a better one. Slack's own 429 with a
-- `Retry-After` is honoured exactly by the delivery retry policy above, which is
-- a real, tested, distributed answer to the same question. If oto ever wants a
-- pre-emptive shared budget it belongs behind a dedicated store, and it will
-- arrive with the code that reads it.
--
-- NO DATA IS LOST: the table has never had a row. `IF EXISTS` keeps this
-- idempotent for a database restored from a dump taken before 00014.

-- +goose Up

DROP TABLE IF EXISTS rate_limit_buckets;

-- +goose Down

-- The Down recreates the table exactly as 00014_ops.sql left it, so that a
-- `goose down` lands on a schema byte-identical to the one 00014 produced. It is
-- restored EMPTY, which is also how it was.
CREATE TABLE IF NOT EXISTS rate_limit_buckets (
  bucket_key  TEXT        PRIMARY KEY,             -- "slack:{team_id}:{channel_id}"
  tokens      DOUBLE PRECISION NOT NULL,
  capacity    DOUBLE PRECISION NOT NULL,
  refill_per_s DOUBLE PRECISION NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT rate_limit_key_ck    CHECK (length(bucket_key) BETWEEN 1 AND 256),
  CONSTRAINT rate_limit_tokens_ck CHECK (tokens >= 0 AND tokens <= capacity),
  CONSTRAINT rate_limit_cap_ck    CHECK (capacity > 0),
  CONSTRAINT rate_limit_refill_ck CHECK (refill_per_s > 0)
);

COMMENT ON TABLE  rate_limit_buckets IS
  'Postgres token buckets that gate outbound provider calls BEFORE they are made (SPEC §G.6). Shared across worker pods, which is why it lives in the database rather than in process memory. Keyed, not tenant-scoped: the limit belongs to the provider conversation, not to the org.';
COMMENT ON COLUMN rate_limit_buckets.bucket_key IS
  'Bucket identity. slack:{team_id}:{channel_id} at capacity 3, refill 1/s for chat.postMessage. update_root bypasses that and uses slack:{team_id}:update at 45/min -- Tier 3 with headroom below 50.';
COMMENT ON COLUMN rate_limit_buckets.tokens IS 'Current balance, never negative and never above capacity (rate_limit_tokens_ck).';
COMMENT ON COLUMN rate_limit_buckets.capacity IS 'Burst size, strictly positive.';
COMMENT ON COLUMN rate_limit_buckets.refill_per_s IS 'Steady-state rate, strictly positive.';
COMMENT ON COLUMN rate_limit_buckets.updated_at IS 'Last refill instant. Tokens are computed lazily from the elapsed time since this.';
