-- A rule snapshot is deduplicated PER RULE KEY, not per source.
--
-- ⭐⭐ WHY. 00009 wrote `rule_snapshots_content_uniq UNIQUE (org_id, source_id,
-- rule_fingerprint)` and SPEC §C.6 defines `rule_fingerprint` over the rule
-- DEFINITION alone: expr, for, keep_firing_for and the canonical labels and
-- annotations. The RuleKey (source, file, group, name) is deliberately NOT in the
-- digest, because the digest has to answer "is this the same TEXT" across a rule
-- that was renamed or moved. That is right, and it means the digest cannot also
-- be asked "is this the same RULE" — which is exactly what the uniqueness
-- constraint was asking it.
--
-- ⛔ WHAT IT COST. An `unavailable` capture is a well-formed row with an empty
-- expr, zero durations and empty label and annotation maps (rule_snapshots_expr_ck
-- requires precisely that). Every unavailable capture therefore hashes to the SAME
-- content address, so ONE Prometheus behind a firewall produced ONE row for EVERY
-- rule in that source, carrying the rule_name of whichever alert failed first.
-- The upsert's UNION arm hands the incumbent back, so every alert after the first
-- got a Snapshot naming somebody else's rule, a `rule.lookup_failed` event whose
-- `rule_name` was wrong, and `NewVersion = false` for a rule that had never been
-- captured. It was self-concealing: every read path filters on rule_name, so the
-- misattributed row could not be retrieved again and the product could not show
-- anybody the evidence.
--
-- The same collapse is reachable without a firewall, just more rarely: two
-- genuinely different rules in one source whose definitions are byte-identical
-- shared a row on the same mechanism.
--
-- ⭐ THE FIX. Put the whole RuleKey in the tuple. Dedup then means what the table
-- has always claimed it means: the same rule captured on every fire costs one row
-- until its TEXT changes, and two different rules are two rows however alike their
-- text. The column order leads with rule_name because that is the component every
-- read path supplies, so the constraint's index is usable as a prefix by the same
-- queries rule_snapshots_key_idx serves.
--
-- ⛔ THE CONSTRAINT IS A WIDENING, so no existing row can violate it and no
-- backfill is needed. Rows collapsed by the old tuple STAY collapsed: their
-- history was never recoverable (nothing recorded which rules were folded into
-- them) and inventing rows to represent captures oto did not make would be worse
-- than an honest gap. New captures separate correctly from this migration on.
--
-- ⭐ THE PRICE, stated plainly. A rule first seen through generatorURL (which
-- knows the expression but not the file it is written in, so rule_file and
-- rule_group are '') and later through /api/v1/rules (which supplies both) is now
-- two rows when the two recoveries happen to be byte-identical: same content,
-- different key completeness. That is one extra row for a rule with no `for` and
-- no rule-level labels, and it is honest provenance rather than a bug: the two
-- captures observed genuinely different amounts about the same rule. The read
-- side reunites them, because keyPredicate matches a stored key component that is
-- equal OR unknown (see internal/rules/repository/snapshots.go).

-- +goose Up

ALTER TABLE rule_snapshots
  DROP CONSTRAINT rule_snapshots_content_uniq;

ALTER TABLE rule_snapshots
  ADD CONSTRAINT rule_snapshots_content_uniq
  UNIQUE (org_id, source_id, rule_name, rule_group, rule_file, rule_fingerprint);

COMMENT ON CONSTRAINT rule_snapshots_content_uniq ON rule_snapshots IS
  'One row per (rule key, content address). The RuleKey is in the tuple because rule_fingerprint is over the DEFINITION only (SPEC §C.6), so without it every unavailable capture in a source (empty expr, zero durations, empty maps) collapsed into one shared row named after whichever alert failed first.';

-- +goose Down

-- ⛔ THE DOWN LOSES ROWS, and it loses precisely the rows this migration exists to
-- let the table hold. Restoring (org_id, source_id, rule_fingerprint) means at most
-- one row per content address per source can survive, so every capture that was
-- correctly separated by its rule key has to be folded back into the oldest one.
-- The survivor is chosen by (captured_at, id) so the fold is deterministic and the
-- row that lives is the one the old code would itself have kept as the incumbent.
--
-- alert_occurrences.rule_snapshot_id is ON DELETE SET NULL (occ_rule_fk), so an
-- occurrence bound to a deleted snapshot CAN unbind rather than disappear: the
-- alert history would survive and the rule text behind some of it would not.
--
-- ⭐ SO THE OCCURRENCES ARE REMAPPED FIRST, AND ALMOST NOTHING IS ACTUALLY LOST.
-- Every folded row shares its `rule_fingerprint` with the survivor it folds into
-- — that is the definition of the fold — and `rule_fingerprint` is the content
-- address of the DEFINITION (SPEC §C.6), so the survivor's expr, for_seconds,
-- keep_firing_for_seconds, rule_labels and rule_annotations are BYTE FOR BYTE the
-- doomed row's. Letting the FK null the pointer would throw away "what the rule
-- said when this fired" for an occurrence whose answer is sitting unchanged one
-- row away, and that answer is the whole product. The UPDATE runs before the
-- DELETE because after it there is nothing left to read the mapping from.
--
-- ⛔ WHAT IS STILL LOST IS THE RULE KEY, and it cannot be otherwise. The survivor
-- may carry a different rule_name, rule_group or rule_file — that is exactly the
-- distinction the narrow tuple cannot represent — so an occurrence remapped onto
-- it keeps the right rule TEXT under somebody else's rule NAME. That is 00009's
-- behaviour, faithfully restored, and it is why this direction is a one-way door
-- in practice even though it runs cleanly.

WITH survivors AS (
  SELECT DISTINCT ON (org_id, source_id, rule_fingerprint)
         org_id, source_id, rule_fingerprint, id
    FROM rule_snapshots
   ORDER BY org_id, source_id, rule_fingerprint, captured_at, id
), folded AS (
  SELECT r.id AS doomed_id, s.id AS survivor_id
    FROM rule_snapshots r
    JOIN survivors s
      ON s.org_id = r.org_id
     AND s.source_id = r.source_id
     AND s.rule_fingerprint = r.rule_fingerprint
   WHERE r.id <> s.id
)
UPDATE alert_occurrences o
   SET rule_snapshot_id = f.survivor_id
  FROM folded f
 WHERE o.rule_snapshot_id = f.doomed_id;

-- The DELETE's predicate is the complement of `survivors` above, expressed the
-- same way: a row dies exactly when some row with its content address in its
-- source is older by (captured_at, id). The two must agree, or occurrences would
-- be remapped onto rows that are themselves about to be deleted.
DELETE FROM rule_snapshots r
 WHERE EXISTS (
   SELECT 1 FROM rule_snapshots k
    WHERE k.org_id = r.org_id
      AND k.source_id = r.source_id
      AND k.rule_fingerprint = r.rule_fingerprint
      AND (k.captured_at, k.id) < (r.captured_at, r.id));

ALTER TABLE rule_snapshots
  DROP CONSTRAINT rule_snapshots_content_uniq;

ALTER TABLE rule_snapshots
  ADD CONSTRAINT rule_snapshots_content_uniq
  UNIQUE (org_id, source_id, rule_fingerprint);

COMMENT ON CONSTRAINT rule_snapshots_content_uniq ON rule_snapshots IS
  'One row per content address per source (SPEC §D.6 as written in 00009).';
