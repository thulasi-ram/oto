-- `notification_policies.reasons` becomes a SET in the database, and its ceiling
-- follows the wire down to 18.
--
-- ⭐⭐ WHY. CONTEXT.md §5b binds a bound to THREE places — the DTO `validate` tag,
-- the domain constructor and the DDL CHECK — and requires them to be identical.
-- Uniqueness on `reasons` lived in exactly one of the three. The request DTOs
-- carried `unique`; `Policy.Validate` checked length and membership and never
-- compared an element to any other element; and `policies_reasons_ck` was a
-- length test and a NULL scan. So the guarantee rested entirely on layer 1, which
-- is the precise shape §5b exists to forbid.
--
-- ⛔ AND THE CONTRACT PUBLISHES IT ON THE RESPONSE, NOT ONLY ON THE REQUEST.
-- `PolicyDTO.reasons` declares `uniqueItems: true`, and the generated frontend
-- validator turns that into a runtime assertion. A duplicate that ever reached
-- this column would be served back on GET /notification-policies and REJECTED BY
-- OTO'S OWN CLIENT — the server failing the client the server generated. That is
-- §5b's row 6 exactly: a 500-class symptom produced by a row the database was
-- happy to store, meaning layers 1 to 3 have a hole.
--
-- ⚠️ NO IN-TREE WRITER CAN REACH IT TODAY, and this migration should not pretend
-- otherwise. The HTTP create and patch bodies are the only writers of the column,
-- both carry the `unique` tag, and it has been on both since the DTOs were first
-- written. There is no seeder, no importer and no drill fixture that builds a
-- `domain.PolicyDraft`. What is being fixed is not a live bug; it is that
-- `ConfigRepository.CreatePolicy` and `UpdatePolicy` take a domain type and write
-- it through untouched, on the documented understanding that the domain has
-- already proved it and the CHECKs are the backstop. Neither of those two was
-- proving this one. The next writer of the column — a bulk import, a policy
-- template, a backfill — would have inherited a bag.
--
-- ⭐ WHY THE CEILING MOVES 32 -> 18 IN THE SAME BREATH. The two numbers were only
-- ever allowed to differ because duplicates were storable: with repetition legal,
-- 32 genuinely described what the column could hold, while `uniqueItems: true`
-- over the 18-value Reason enum made a 19th element unreachable on the wire. Take
-- repetition away and that argument evaporates — a set of DISTINCT values drawn
-- from a closed 18-value vocabulary cannot reach 19 — so 32 becomes a number no
-- row can ever test, of the kind the next enum edit will not move. 18 is now the
-- tag, the constant and the CHECK. Nothing can violate the new ceiling that does
-- not also violate uniqueness, so the two tightenings share one backfill.
--
-- ⭐ WHY A FUNCTION AND NOT AN EXPRESSION. Postgres forbids a subquery in a CHECK,
-- and every direct way to write "this array has no repeated element" needs one:
-- `count(DISTINCT ...)`, `array_agg(DISTINCT ...)` and `ARRAY(SELECT DISTINCT
-- unnest(...))` are all aggregates over `unnest`. A per-value form without a
-- subquery does exist — `cardinality(array_positions(reasons, 'fired')) <= 1`,
-- eighteen times — and it was rejected: it would spell the Reason vocabulary out
-- a third time, in SQL, where migration 00018 already owns it once and
-- `domain/reason.go` owns it again, and it would silently stop checking any value
-- outside that list. An IMMUTABLE one-liner says the rule once and says it about
-- arrays rather than about reasons.
--
-- ⛔ `cardinality`, NOT `array_length(reasons, 1)`, AND THAT IS A SECOND
-- TIGHTENING. `array_length` returns NULL for an empty array, a CHECK yields NULL
-- rather than false, and a NULL CHECK PASSES — so `reasons = '{}'` satisfied the
-- old constraint despite it reading `BETWEEN 1 AND 32`, and a policy that reacts
-- to nothing was storable by the same non-DTO path this migration is about.
-- `cardinality` returns 0 and the row is refused. Nothing can have written such a
-- row either: both DTOs carry `min=1` and `Policy.Validate` has always rejected
-- an empty list. If one exists anyway, this migration FAILS LOUDLY on the
-- validation scan rather than admitting a row the contract calls impossible, and
-- that is the right failure for a contract-phase migration to have.
--
-- ⭐ EXPAND/CONTRACT, AND THIS IS THE CONTRACT HALF (CONTEXT.md §6). Tightening a
-- CHECK on a live table fails if any stored row already violates it, so the fold
-- runs FIRST and in the same transaction: any row holding a repeated reason is
-- rewritten to its distinct values before the constraint is added. The fold
-- preserves FIRST-OCCURRENCE ORDER rather than sorting, because the column is
-- read back verbatim into `PolicyDTO.reasons` and an operator who wrote
-- [fired, acked] should not find [acked, fired] in the editor after a deploy.
-- It cannot change behaviour: `Policy.Handles` is a membership test with an early
-- return, so a repeated reason routed nothing twice and dropping it routes
-- nothing less.
--
-- Version N (the binary before this migration) writes only DTO-validated rows,
-- which are already sets of at most 18 distinct values, so it cannot violate the
-- new constraint during the deploy window. Version N+1 writes rows legal under
-- both. There is no window in which either binary writes a row the other's schema
-- rejects.
--
-- `updated_at` IS DELIBERATELY NOT TOUCHED BY THE FOLD. Migrations 00032 to 00034
-- took the database's clock off this table on purpose: every timestamp on
-- `notification_policies` comes from the application, and a migration has no pod
-- clock to speak with. Stamping `now()` here would reintroduce exactly the
-- authority those three migrations removed, to record a rewrite that changes what
-- is stored and not what the policy does.
--
-- ⛔ THE CONSTRAINT NAME IS A RUNTIME CONTRACT (CONTEXT.md §6, SPEC §L.9): it is
-- returned as `errs.Error.Code`. It is therefore DROPPED AND RE-ADDED UNDER THE
-- SAME NAME rather than replaced by a new one.
--
-- THE DOWN is a pure relaxation and cannot fail: every row legal under the tight
-- constraint is legal under the loose one. What it does not do is put the
-- duplicates back, and it must not — they are unrecoverable by definition (a
-- repeated element carries no information beyond the element) and the rolled-back
-- release refuses them at layer 1 anyway.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION oto_array_is_set(a TEXT[]) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
  SELECT count(*) = count(DISTINCT t.e) FROM unnest(a) AS t(e);
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION oto_array_is_set(TEXT[]) IS
  'True when the array has no repeated element. Exists because a CHECK may not contain a subquery and every direct spelling of set-ness is an aggregate over unnest. NULL elements: count(DISTINCT) ignores them while count(*) does not, so an array carrying one is reported as not a set; every caller also forbids NULL elements outright, so the two answers agree. NULL input yields true, since unnest of NULL is no rows at all.';

-- The fold. Runs before the constraint is added, in the same goose transaction,
-- so no window exists in which the table is unconstrained or half folded. The
-- WHERE clause makes it a no-op on every database whose policies were written
-- through the API, which today is all of them.
WITH deduplicated AS (
  SELECT p.id,
         array_agg(u.reason ORDER BY u.first_position) AS reasons
    FROM notification_policies p
    CROSS JOIN LATERAL (
      SELECT e.reason, min(e.ord) AS first_position
        FROM unnest(p.reasons) WITH ORDINALITY AS e(reason, ord)
       GROUP BY e.reason
    ) u
   GROUP BY p.id
)
UPDATE notification_policies p
   SET reasons = d.reasons
  FROM deduplicated d
 WHERE p.id = d.id
   AND cardinality(p.reasons) <> cardinality(d.reasons);

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;

-- Validating rather than NOT VALID: the fold above has just made the scan
-- provably clean, and this table is one row per routing rule per tenant, not a
-- table anybody has to worry about locking.
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 18
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

COMMENT ON COLUMN notification_policies.reasons IS
  'Which §H.6 Reason values this policy reacts to. A SET: 1..18 entries, no NULLs, no duplicates. 18 is the size of the Reason enum, which is the most a set drawn from it can hold, and it is the same number the DTO tag and domain.MaxPolicyReasons carry.';

COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..18 §H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses.';

-- +goose Down

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;

-- Byte-identical to the predicate 00011 shipped, so a rolled-back database is in
-- the state its migration history describes.
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (array_length(reasons, 1) BETWEEN 1 AND 32
         AND array_position(reasons, NULL) IS NULL);

COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS NULL;

COMMENT ON COLUMN notification_policies.reasons IS
  'Which §H.6 Reason values this policy reacts to. 1..32 entries, no NULLs.';

DROP FUNCTION oto_array_is_set(TEXT[]);
