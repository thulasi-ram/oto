-- SPEC §B.1, §D.4 -- `alerts.ack_state` is dropped. An acknowledgement is a
-- receipt for ONE FIRING EPISODE and it belongs to that episode.
--
-- ⭐⭐ WHY. An ack is a statement about one firing. 00007 projected the current
-- occurrence's ack onto `alerts`, an entity that OUTLIVES the firing, so the
-- column kept asserting a claim whose subject had ended:
--
--     10:00  fires    -> occurrence 7 opens, a human acks -> ack_state='acked'
--     10:05  resolves -> occurrence 7 closes
--     10:05..14:00    alerts.state     = 'resolved'   honest, that IS its state
--                     alerts.ack_state = 'acked'      a claim about a closed episode
--     14:00  fires again -> occurrence 8 opens unacked -> the claim flips back
--
-- `occ_ackorder_ck CHECK (acked_at IS NULL OR acked_at >= started_at)` already
-- states that an ack cannot exist without an episode to belong to. This column
-- was the only place in the schema that contradicted it. It is the mechanism by
-- which a September firing could arrive pre-acknowledged because somebody acked
-- in March -- never entering anyone's queue, never looked at -- and the list of
-- unacknowledged things is the surface people work from, so its trustworthiness
-- is the whole value of ack.
--
-- ⛔ WHY `state` STAYS PROJECTED AND THIS DOES NOT. `state` is the one axis that
-- still has an honest answer when nothing is firing: a resolved alert really is
-- resolved, and four indexes on the hot list path lead with it. Ack has no
-- answer at all once the episode ends -- neither `acked` nor `unacked` is true
-- of a thing that is not happening -- so there is nothing to project.
--
-- ⭐ NOTHING LOSES THE ABILITY TO ASK. `alert_occurrences.ack_state` is and was
-- the authority, and it is indexed for exactly this question:
-- `occ_ack_idx (org_id, ack_state, started_at DESC) WHERE ended_at IS NULL`,
-- created in 00007 for "the unacked and still open queue". The dropped column,
-- by contrast, was covered by NO index on `alerts` -- the ten there lead with
-- `state`, `last_seen_at`, `alertname`, `namespace`, `severity`,
-- `source_fingerprint` and `snoozed_until` -- so `?ack=acked` scanned
-- `(org_id, last_seen_at DESC, id DESC)` and discarded off the heap, degrading
-- as the table grew because acked alerts are the minority.
--
-- ⚠️ THE READERS WENT FIRST, IN THE SAME CHANGE AND BEFORE THIS FILE. Two of
-- them were CROSS-MODULE SQL: `notification`'s member and focus reads, which
-- have no Go import and no compiler error to break, and `stats`'s overview
-- counters. Both now reach the case -- the member row's own occurrence, and the
-- alert's `current_occurrence_id` -- by primary key. The `?ack=` filter, the
-- roll-up's `acked` counter and the ack field on every alert-shaped DTO are gone
-- from the contract in the same commit. `test/arch/alertackcolumn_test.go`
-- refuses the column's return.
--
-- ⛔ IT IS NOT REVERSIBLE, AND THE DOWN SAYS SO IN SQL RATHER THAN IN PROSE.
-- Dropping a column destroys its rows. The Down below restores the column, its
-- CHECK and its comment -- the shape 00007 shipped -- and REBUILDS the values
-- from `alert_occurrences`, which is where they were always authoritative. A
-- rolled-back database therefore holds the same projection it would have held if
-- this migration had never run, because the projection was never the record.

-- +goose Up

ALTER TABLE alerts DROP CONSTRAINT alerts_ackstate_ck;
ALTER TABLE alerts DROP COLUMN ack_state;

-- ⛔ StatementBegin/End IS LOAD-BEARING, NOT DECORATION. goose ends a statement
-- at a line whose last word before the first `--` token ends in `;`, so the
-- literal ` -- ` INSIDE this comment string hides the terminating semicolon from
-- the parser and the statement never closes. Being the last statement in its
-- block, there is no later semicolon to accidentally close it either, so goose
-- fails the whole migration with "unexpected unfinished SQL query". The
-- delimiters end it explicitly. Same reason 00048 wraps its Down.
-- +goose StatementBegin
COMMENT ON TABLE alerts IS
  'The IDENTITY of a label set within (org, cluster_key) -- oto answer to Sentry Issue. Created on first sight and never deleted; resolution ends an Occurrence, not an Alert. Everything below first_seen_at is a PROJECTION of alert_events, kept for query speed, never the only record. It carries NO ack: an acknowledgement is a receipt for one firing episode and lives on alert_occurrences, because a claim projected onto this row would outlive the firing it was about.';  -- vocab:allow -- SEQUENCE ORDER, not history: 00049 runs BEFORE 00052, so `alert_occurrences` is the table's actual name at this point in the sequence and `alert_cases` would not resolve. The rename is deliberately its own migration (00052's Up is the rename and nothing else), which is what makes it reviewable; the price is that every migration numbered below it must spell the old name. 00052 rewrites this COMMENT along with the table.
-- +goose StatementEnd

-- +goose Down

ALTER TABLE alerts ADD COLUMN ack_state TEXT NOT NULL DEFAULT 'unacked';

-- Rebuilt from the authority rather than defaulted, so a rolled-back database
-- matches the one this migration was applied to instead of quietly unacking
-- everything.
UPDATE alerts a
   SET ack_state = o.ack_state
  FROM alert_occurrences o
 WHERE o.id = a.current_occurrence_id
   AND o.ack_state <> a.ack_state;

ALTER TABLE alerts ADD CONSTRAINT alerts_ackstate_ck CHECK (ack_state IN ('unacked','acked'));

COMMENT ON COLUMN alerts.ack_state IS 'ORTHOGONAL to state (SPEC §B.1). An acked alert is still firing.';

-- Wrapped for the same reason the Up's last statement is: the ` -- ` inside the
-- string hides the terminating `;` from goose's line scanner.
-- +goose StatementBegin
COMMENT ON TABLE alerts IS
  'The IDENTITY of a label set within (org, cluster_key) -- oto answer to Sentry Issue. Created on first sight and never deleted; resolution ends an Occurrence, not an Alert. Everything below first_seen_at is a PROJECTION of alert_events, kept for query speed, never the only record.';
-- +goose StatementEnd
