-- A rejection is identified by WHAT IT IS, and its primary key stays a uuidv7.
--
-- ⭐⭐ WHY. `oto replay` re-runs a stored batch through the same normalisation the
-- webhook took, and normalisation writes `ingest_rejections` BEFORE it applies any
-- observation (internal/ingestion/service/process.go). Every write under §G.5 is
-- idempotent by construction — the alert upsert is ON CONFLICT, event appends
-- dedupe through `alert_event_keys` — and this table was the one exception: it had
-- no uniqueness of any kind and minted a fresh id per row per attempt. A batch
-- with forty rejections replayed twice showed a hundred and twenty in the
-- per-source feed, and the §G.6 retry budget could do the same thing without a
-- replay, because the rejections are deliberately written outside the observation
-- transactions and a retried chunk rewrites them.
--
-- That is not a cosmetic duplicate. The feed exists so an operator can answer
-- "what did oto refuse, and why" during the incident (§C.9.1); a feed that
-- triples its own rows answers it with a number that is three times wrong.
--
-- ⛔ WHY THE ID IS NOT THE PLACE TO PUT THIS, which was the first attempt and was
-- wrong. Deriving the primary key from (batch, ordinal, reason) — a uuidv5 — does
-- make the insert idempotent, and it also DESTROYS THE KEYSET. `List` pages the
-- feed on `(received_at, id) < (cursor)` and every rejection of one batch shares
-- that batch's `received_at` to the microsecond, so `id` is not a decoration on
-- the sort: it is the only thing making the order TOTAL, and it works because a
-- uuidv7 is time-ordered by minting. A content-derived id is a hash with no
-- ordering at all, so pages started skipping and repeating rows — which on this
-- screen reads as "your alert was never rejected". The invariant was written down
-- in `List`'s comment and the right response to a change that falsified it was to
-- change the approach, not the comment.
--
-- ⭐ THE FIX. Keep the uuidv7 primary key, put the natural key in its own
-- constraint, and let the INSERT arbitrate on that instead.
--
--   ingest_rejections_natural_uniq (received_at, batch_id, ordinal, reason)
--
-- `ordinal` is the position of the rejection within the call that produced it —
-- the slice index in RecordBatch, which is the position in the payload, because
-- rejections are produced by walking the stored envelope in order. The same bytes
-- normalised again yield the same rejections at the same positions with the same
-- reasons, so a replayed row conflicts with itself exactly and DOES NOTHING. It is
-- what separates two elements of one batch that failed the same bound, and there
-- is nothing else that could: the evidence is not unique (two alerts can carry the
-- identical oversized label) and hashing 2 KiB of user data into a key is not a
-- key.
--
-- ⚠️ `received_at` LEADS BECAUSE IT MUST. `ingest_rejections` is PARTITION BY
-- RANGE (received_at) and a unique constraint on a partitioned table has to
-- contain every partition key column — uniqueness that Postgres can only enforce
-- within one partition is not uniqueness. It costs nothing here: `received_at` is
-- the batch's own receipt, so it is constant across every attempt at that batch
-- and adds no discrimination the natural key did not already have.
--
-- ⭐ NULLS ARE DISTINCT, AND BOTH NULLS ARE LOAD-BEARING.
--
--   * `batch_id IS NULL` is the undecodable body and the unknown source (§C.9.1):
--     no batch row exists, so there is nothing to derive an identity from and
--     nothing will ever replay them — a replay starts FROM a stored batch, and
--     these rows are the record that there is none. They must never dedupe, and
--     under the default NULLS DISTINCT they cannot.
--   * `ordinal IS NULL` is a writer that predates this migration. That is the
--     expand/contract half (CONTEXT.md §6): the column is NULLABLE with no
--     default precisely so a release-N binary still inserting seven columns keeps
--     working, unchanged, duplicates and all. NOT NULL would break that writer
--     outright, and `DEFAULT 0` would be worse — every row of an old writer's
--     batch would collide on ordinal 0 and the INSERT would FAIL, turning a
--     recording mechanism into a 500 on the ingest path.
--
-- ⚠️ WHAT THE CONSTRAINT WOULD REJECT THAT IS LEGITIMATE, stated plainly. Two
-- writers put rows against the same batch_id: the accept path's batch-level notes
-- (RecordBatch inside the accept transaction) and the process path's per-alert
-- rejections. Both start their ordinals at zero, so `reason` is the only thing
-- keeping them apart. They cannot collide TODAY, because the accept path emits
-- only batch-level reasons (too_many_alerts, body_too_large) and the process path
-- only per-element ones, and `ingest_rejections_reason_ck` is a closed enum that
-- makes the two sets inspectable. If a future change ever teaches the accept path
-- a per-element reason, a genuine rejection would be silently swallowed by DO
-- NOTHING. The cheap guard, should that day come, is to give the accept path its
-- own ordinal base rather than to widen this key.
--
-- ⛔ THE BACKFILL IS NOT COSMETIC. Existing rows are numbered per (received_at,
-- batch_id) in id order, which — ids being uuidv7 — is insertion order, and is
-- therefore the same ordinal the code would mint for every batch whose rejections
-- came from a single call. Without it every pre-migration batch would replay into
-- one extra copy of its own rejections before settling down. It is safe for the
-- constraint by construction: row_number() is unique within each partition-by
-- group, so no two rows can share (received_at, batch_id, ordinal) at all, let
-- alone with the same reason.
--
-- ⚠️ The one shape the backfill cannot reproduce is a batch that has BOTH accept
-- notes and process rejections: the code numbers each call from zero, and a single
-- ordering over the batch numbers the process rows from where the notes left off.
-- Those batches replay into one extra copy, once, and dedupe from then on. It is
-- rare (accept notes mean the upstream truncated the payload), it is bounded, and
-- it is confined to rows written before this migration, all of which age out
-- within `orgs.settings.raw_retention_days`.

-- +goose Up

ALTER TABLE ingest_rejections ADD COLUMN ordinal INT;

UPDATE ingest_rejections r
   SET ordinal = n.ord
  FROM (
    SELECT id, received_at,
           row_number() OVER (PARTITION BY received_at, batch_id ORDER BY id) - 1 AS ord
      FROM ingest_rejections
  ) n
 WHERE r.id = n.id AND r.received_at = n.received_at;

ALTER TABLE ingest_rejections
  ADD CONSTRAINT ingest_rejections_natural_uniq
  UNIQUE (received_at, batch_id, ordinal, reason);

COMMENT ON COLUMN ingest_rejections.ordinal IS
  'Position of this rejection within the call that produced it, which is its position in the stored payload — rejections are produced by walking the envelope in order. It exists so a replayed or retried batch conflicts with its own rows instead of appending a second copy of them (ingest_rejections_natural_uniq). NULL means a writer older than migration 00047; those rows never dedupe, which is exactly release-N behaviour.';

COMMENT ON CONSTRAINT ingest_rejections_natural_uniq ON ingest_rejections IS
  'The natural key: one row per (batch, position, reason). It is what makes ingest_rejections replay-safe, so that oto replay and the SPEC G.6 retry budget re-run a batch without tripling the per-source rejection feed. received_at leads because a unique constraint on a partitioned table must carry the partition key. NULLS ARE DISTINCT on purpose: batch_id IS NULL is the undecodable body and the unknown source, which have no batch to be identified against and can never be replayed; ordinal IS NULL is a pre-00047 writer, kept working unchanged by expand/contract. The primary key stays a uuidv7 because the rejection feed keysets on (received_at, id) and every rejection of one batch shares its received_at — a content-derived id would make that ordering arbitrary and the paging would skip rows.';

-- +goose Down

-- Losing the constraint loses only idempotence: every row survives, and the feed
-- goes back to being able to double-count a replayed batch. The column goes with
-- it because it has no other reader.

ALTER TABLE ingest_rejections
  DROP CONSTRAINT IF EXISTS ingest_rejections_natural_uniq;

ALTER TABLE ingest_rejections DROP COLUMN IF EXISTS ordinal;
