-- Relax `ui_events_payload_ck` from 4096 to 16384 stored bytes.
--
-- ⭐⭐ WHY. The ui-event envelope cap was measured with TWO DIFFERENT RULERS, and
-- they diverged in BOTH directions.
--
--   * Go (`internal/streaming/domain/event.go`) bounded `len(payload)` — the
--     length of the RAW JSON TEXT.
--   * This CHECK bounds `pg_column_size(payload)` — the size of the STORED JSONB.
--
-- Those are not the same quantity and neither dominates the other:
--
--   TOO LAX. jsonb is a parsed binary form: a 4-byte varlena header, a 4-byte
--   container header, a 4-byte JEntry per key AND per value, keys stored raw, and
--   every value INTALIGNed to 4 bytes with numbers stored as full `numeric`
--   (8 bytes for `1`, against one byte of text). A compact object of many small
--   fields therefore stores far LARGER than its text. The worst 4096-byte compact
--   object — 598 pairs, distinct keys, single-digit numeric values — stores as
--   10680 bytes. It passed the Go check and then tripped THIS constraint: a 23514
--   on the SSE write path, which surfaces as a 500. Keeping that off the write
--   path is the entire stated reason the Go check exists.
--
--   TOO STRICT. jsonb discards insignificant whitespace, so a payload padded with
--   spaces was refused by Go for a size it would never have occupied here.
--
-- ⭐ THE FIX IS IN TWO HALVES, AND THIS FILE IS THE SECOND.
--
--   (a) Go now COMPACTS the payload before measuring it and STORES the compacted
--       form, which removes the whitespace divergence entirely — what was weighed
--       is what Postgres is handed.
--   (b) This migration RAISES the database bound so that Go is reliably the
--       STRICTER of the two. The two rulers still measure different things; that
--       is inherent, and pretending otherwise is what produced the bug. What is
--       enforced instead is an ORDERING: nothing Go accepts can reach this CHECK.
--
-- THE ARITHMETIC, in full, is in the doc comment on
-- `streaming/domain.MaxStoredPayloadBytes`, and it is asserted against a live
-- Postgres by TestWorstCaseJSONBOverheadStoresUnderTheDDLCap. In summary:
--
--   max pairs in 4096 compact bytes:  7N - 93 <= 4096  =>  N <= 598
--   stored:  8 headers + 8*598 JEntries + 1102 key bytes + 2 pad + 8*598 numerics
--         =  10680 bytes   (measured: pg_column_size() returns exactly 10680)
--
--   16384 - 10680 = 5704 bytes of headroom, 1.53x. It is also 4 x the Go cap.
--
-- ⛔ THE 4096 IN THE GO CODE IS NOT THIS NUMBER AND MUST NOT BE "SYNCHRONISED"
-- WITH IT. the CONTEXT.md §5b rule that a bound is identical in all three places
-- applies to a bound expressed in the SAME UNIT. This one is not: 4096 is compact
-- JSON TEXT bytes, 16384 is STORED JSONB bytes. Making the digits match is what
-- broke it.
--
-- ⭐ EXPAND/CONTRACT, N AND N+1 SIMULTANEOUS (CONTEXT.md §6). This is a pure
-- RELAXATION: the new predicate is implied by the old one, so
--
--   * every existing row already satisfies it — the validation scan cannot fail;
--   * version N (the old binary, capping raw text at 4096) keeps writing rows that
--     are legal under both constraints, because compaction only ever SHRINKS a
--     payload, so the N raw-text bound is a strictly tighter filter than the N+1 one;
--   * version N+1 writes rows that are legal under the new constraint and were
--     already legal under the old one for every payload N could have produced.
--
-- There is no window in which either binary can write a row that the schema of the other
-- rejects. Relaxing a CHECK is the safe half of expand/contract; the tightening in
-- the Down is the half that needs care, and it is handled there.
--
-- ⛔ THE CONSTRAINT NAME IS A RUNTIME CONTRACT (CONTEXT.md §6, SPEC §L.9): it is
-- returned as `errs.Error.Code`. It is therefore DROPPED AND RE-ADDED UNDER THE
-- SAME NAME rather than replaced by a new one.
--
-- PARTITIONED PARENT. `ui_events` is PARTITION BY RANGE (at) with hourly
-- partitions. Both statements below name only the PARENT and both RECURSE: a
-- CHECK on a partitioned table is copied to every partition with
-- `coninhcount = 1, conislocal = false`, DROP on the parent removes those copies,
-- and ADD on the parent installs the new one on all of them. Partitions created
-- LATER — by `oto_ensure_partitions_ahead` / `oto_partitions_manage`, which use
-- `CREATE TABLE ... PARTITION OF` — inherit whatever the parent carries at that
-- moment, so no per-partition loop is needed and none would stay correct.

-- +goose Up

ALTER TABLE ui_events DROP CONSTRAINT ui_events_payload_ck;

-- Validating (not NOT VALID): the scan is provably redundant, since the predicate
-- it checks is implied by the one just dropped, and `ui_events` is bounded by its
-- 24-hour retention rather than growing without limit. Both statements run in
-- the goose transaction, so no window exists in which the table is unconstrained.
ALTER TABLE ui_events ADD CONSTRAINT ui_events_payload_ck
  CHECK (jsonb_typeof(payload) = 'object' AND pg_column_size(payload) <= 16384);

COMMENT ON COLUMN ui_events.payload IS
  'SMALL envelope, hard-capped at 4096 bytes of COMPACT JSON TEXT by streaming/domain.NewAppend, which compacts before measuring and stores exactly what it measured. ui_events_payload_ck is the BACKSTOP, not the rule, and it bounds a DIFFERENT quantity (pg_column_size of the stored jsonb, at 16384) because jsonb adds a varlena header, a container header and a 4-byte JEntry per key AND per value: the worst-case 4096-byte compact object stores as 10680 bytes. The two numbers differ on purpose. Making them equal is what let a 23514 reach the SSE write path, where it is a 500. Just enough to update a list row without a refetch.';

-- +goose Down

-- ⛔ THE DOWN IS THE DANGEROUS DIRECTION, because re-imposing 4096 stored bytes is
-- a TIGHTENING and rows written under the relaxed bound may now be illegal. They
-- are deleted first, and that is safe HERE and almost nowhere else in this schema:
-- `ui_events` is a 24-hour replay buffer, its own table comment already tells every
-- client that `seq` is MONOTONIC BUT NOT CONTIGUOUS, and the SSE resume path is
-- specified to tolerate gaps. A deleted envelope costs a client one refetch of a
-- row it can re-read from its own endpoint; a Down that ABORTS would cost the
-- rollback.
--
-- ⛔ NOTE THE `payload::text::jsonb`, WHICH IS NOT REDUNDANT AND MUST NOT BE
-- "SIMPLIFIED" TO `pg_column_size(payload)`. pg_column_size reports the size of
-- the datum AS IT FINDS IT, and those are two different numbers for the same row:
--
--   * on INSERT the CHECK runs before TOASTing, so it measures the FULL datum
--     (the worst-case envelope: 10680 bytes);
--   * read back OUT of the heap that same payload is TOAST-compressed, and
--     pg_column_size then reports the COMPRESSED size (1518 bytes for the same
--     row, measured).
--
-- So a bare pg_column_size(payload) here would match almost nothing, and would
-- leave behind exactly the rows this DELETE exists to remove. The round trip
-- through text forces a detoast and a re-parse, which reproduces the number the
-- restored constraint will apply to the next INSERT.
--
-- (The same asymmetry is why the ADD below would have succeeded even without this
-- DELETE: a validation scan reads from the heap and sees the compressed size. That
-- is luck, not correctness. It would leave rows in the table that the constraint
-- would refuse to accept again, and the next dump/restore would fail instead.)
DELETE FROM ui_events WHERE pg_column_size(payload::text::jsonb) > 4096;

ALTER TABLE ui_events DROP CONSTRAINT ui_events_payload_ck;

ALTER TABLE ui_events ADD CONSTRAINT ui_events_payload_ck
  CHECK (jsonb_typeof(payload) = 'object' AND pg_column_size(payload) <= 4096);

COMMENT ON COLUMN ui_events.payload IS
  'SMALL envelope, hard-capped at 4096 bytes on disk by ui_events_payload_ck. Just enough to update a list row without a refetch.';
