-- Widen `ingest_rejections_reason_ck` with two storability reasons:
-- `invalid_label_value` (B18) and `annotation_unstorable` (B19).
--
-- ⭐⭐ WHY. `ingest_rejections` is THE ONLY PLACE A REJECTED ALERT SURVIVES. The
-- whole justification for rejecting per-alert and still answering 202 (SPEC
-- §L.3.2, §C.9.1, ADR 0007) is that the rejection is RECORDED rather than lost:
-- a 4xx would make Alertmanager delete the notification forever, so this row is
-- the entire audit trail. An operator debugging "why did oto drop this alert"
-- reads exactly one column -- `reason` -- and it has to be TRUE.
--
-- It was not. `alerts/domain.NewLabels` refuses a label value carrying U+0000,
-- because Postgres `text` cannot hold that byte at all and such a value fails at
-- layer 6, the INSERT, no matter what oto does with it. The bound is correct. But
-- the code it minted, `invalid_label_value`, had no member in this enum, so
-- `ingestion/domain.ReasonFromError` fell through to its default and recorded
-- `undecodable`.
--
-- `undecodable` means "these bytes are not an Alertmanager webhook payload at
-- all" -- the custom `payload:` template case (B16, §L.3.1). It sent an operator
-- hunting for malformed JSON that was never there. The payload decoded
-- perfectly; ONE LABEL VALUE was unwritable, and oto knew which label and why.
-- Honest about the outcome, wrong about the cause, and wrong in the one direction
-- that costs debugging hours.
--
-- ⭐ TWO REASONS, BECAUSE THE SAME BOUND HAS TWO OPPOSITE VERDICTS.
--
-- The storability rule is one predicate -- no U+0000, valid UTF-8 -- and it lives
-- in one function, `alerts/domain.UnstorableReason`. What oto DOES about a
-- violation depends on what the string is FOR:
--
--   `invalid_label_value` (B18) REJECTS THE ALERT. A label value is part of alert
--     IDENTITY: `alert_key` hashes the label set (§C.1, §C.2). Replacing a byte
--     would change WHICH ALERT this is and file an observation under a key the
--     upstream never sent -- a corrupted timeline that nothing downstream could
--     detect. Losing one observation is recoverable; silently merging two Alerts
--     is not.
--
--   `annotation_unstorable` (B19) KEEPS THE ALERT. Annotations are descriptive,
--     never identity (§C.9.3), and their ingest policy is already
--     truncate-and-keep (B7 drops excess annotations, B8 truncates a long value).
--     Rejecting an alert over a bad byte in its `description` would contradict
--     that policy and throw away the signal underneath the prose. So the
--     unstorable code points of a VALUE are replaced with U+FFFD, and an
--     annotation whose NAME is unstorable is dropped -- a name is a jsonb KEY, and
--     sanitising two different bad names into one string would silently overwrite
--     one annotation with another. Either way this row records that oto edited
--     what the upstream sent, which is the only thing that makes editing it
--     acceptable at all.
--
-- ⛔ DO NOT "SIMPLIFY" THESE TO ONE MEMBER. They are distinguished precisely
-- because one means AN ALERT IS MISSING FROM THE TIMELINE and the other means AN
-- ALERT IS PRESENT WITH ONE ALTERED SENTENCE. An operator triages those
-- differently, and the `oto_ingest_rejected_total{reason}` series that alerts on
-- the first must not be polluted by the second.
--
-- ⭐ EXPAND/CONTRACT, N AND N+1 SIMULTANEOUS (CONTEXT.md §6). This is a pure
-- RELAXATION -- the old predicate implies the new one -- and it MUST LAND BEFORE
-- ANY BINARY WRITES THE NEW VALUES:
--
--   * every existing row already satisfies the new constraint, so the validation
--     scan cannot fail;
--   * version N (which knows only the thirteen old members) keeps writing rows
--     that are legal under both constraints;
--   * version N+1 writes the two new members, which are legal only under the new
--     one -- hence the ordering. Deploying N+1 first would turn a rejected alert
--     into a 23514 on the rejection write, i.e. an alert lost while recording
--     that an alert was lost.
--
-- There is no window in which N can write a row the new schema refuses. Relaxing
-- a CHECK is the safe half of expand/contract; the tightening in the Down is the
-- half that needs care, and it is handled there.
--
-- ⛔ THE CONSTRAINT NAME IS A RUNTIME CONTRACT (CONTEXT.md §6, SPEC §L.9): it is
-- returned as `errs.Error.Code` on a 23514. It is therefore DROPPED AND RE-ADDED
-- UNDER THE SAME NAME rather than replaced by a differently-named one.
--
-- PARTITIONED PARENT. `ingest_rejections` is PARTITION BY RANGE (received_at)
-- with DAILY partitions. Both statements below name only the PARENT and both
-- RECURSE: a CHECK on a partitioned table is copied to every partition with
-- `coninhcount = 1, conislocal = false`, DROP on the parent removes those copies,
-- and ADD on the parent installs the new one on all of them. Partitions created
-- LATER -- by `oto_ensure_partitions_ahead` / `oto_partitions_manage`, which use
-- `CREATE TABLE ... PARTITION OF` -- inherit whatever the parent carries at that
-- moment, so no per-partition loop is needed and none would stay correct.

-- +goose Up

ALTER TABLE ingest_rejections DROP CONSTRAINT ingest_rejections_reason_ck;

-- Validating (not NOT VALID): the scan is provably redundant, since the new
-- predicate is implied by the one just dropped, and `ingest_rejections` is bounded
-- by `orgs.settings.raw_retention_days` (default 14) rather than growing without
-- limit. Both statements run in the goose transaction, so there is no window in
-- which the table is unconstrained.
--
-- The member order matches `ingestion/domain`'s const block exactly, so the two
-- lists stay diffable by eye. That is the only thing keeping them in step -- there
-- is no generator.
ALTER TABLE ingest_rejections ADD CONSTRAINT ingest_rejections_reason_ck CHECK (reason IN
  ('too_many_labels','label_value_too_large','label_name_too_large','labelset_too_large',
   'too_many_annotations','annotation_too_large','annotation_unstorable','missing_alertname',
   'invalid_label_name','invalid_label_value','timestamp_out_of_window','too_many_alerts',
   'body_too_large','undecodable','unknown_source'));

COMMENT ON COLUMN ingest_rejections.reason IS
  'Closed enum matching the hard caps in SPEC §C.9.1 and the bounds checks B1-B19 in §L.3. Also the oto_ingest_rejected_total{reason} label. invalid_label_value (B18) means a label value Postgres cannot store (U+0000 or invalid UTF-8) and THAT ALERT was rejected, because a label value is alert identity and oto will not rewrite an identity in order to store it. annotation_unstorable (B19) is the same bytes in an annotation, where the alert is KEPT: the value''s unstorable code points are replaced with U+FFFD, or the annotation is dropped when its NAME is unstorable. Neither is undecodable, which means the body was not a webhook payload at all.';

-- +goose Down

-- ⛔ THE DOWN IS THE DANGEROUS DIRECTION, because narrowing the enum makes rows
-- written under the widened one illegal. They are NOT deleted.
--
-- `ingest_rejections` is evidence. Its entire reason for existing is that a
-- rejected alert leaves a trace an operator can find, and its retention is already
-- the shortest in the schema (14 days). Deleting the rows would destroy exactly
-- what the table is for, and would do it during a rollback -- the moment an
-- operator is most likely to be reading it.
--
-- So the rows are REWRITTEN to what version N would itself have recorded for the
-- same input: `undecodable`, which is precisely the lossy mapping this migration
-- exists to remove. That is not a regression introduced here; it is the old
-- binary's own behaviour, restored along with the old binary. The true reason is
-- preserved verbatim at the FRONT of `detail`, so nothing is actually lost and a
-- re-application of the Up can be reconciled by eye.
--
-- `detail` is nullable, hence the coalesce. Only `reason` moves, never
-- `received_at`, so no row changes partition.
UPDATE ingest_rejections
   SET detail = '[' || reason || '] ' || coalesce(detail, ''),
       reason = 'undecodable'
 WHERE reason IN ('invalid_label_value','annotation_unstorable');

ALTER TABLE ingest_rejections DROP CONSTRAINT ingest_rejections_reason_ck;

ALTER TABLE ingest_rejections ADD CONSTRAINT ingest_rejections_reason_ck CHECK (reason IN
  ('too_many_labels','label_value_too_large','label_name_too_large','labelset_too_large',
   'too_many_annotations','annotation_too_large','missing_alertname','invalid_label_name',
   'timestamp_out_of_window','too_many_alerts','body_too_large','undecodable','unknown_source'));

COMMENT ON COLUMN ingest_rejections.reason IS
  'Closed enum matching the hard caps in SPEC §C.9.1 and the bounds checks B1-B17 in §L.3. Also the oto_ingest_rejected_total{reason} label.';
