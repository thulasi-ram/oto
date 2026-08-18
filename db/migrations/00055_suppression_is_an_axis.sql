-- ADR 0041 -- suppression is an AXIS, not a state. `alerts.state` narrows to
-- `firing | resolved | expired` and `suppression_reason` / `suppressed_by` join
-- it as columns of their own, independent of it.
--
-- ⭐⭐ WHY THE COLUMN WAS WRONG. `suppressed` occupied the slot `firing` needed.
-- Suppression is not a phase of a signal's life -- it is a statement about
-- whether ALERTMANAGER IS DELIVERING the signal, and the signal goes on firing
-- underneath it. So `state = 'firing'` silently excluded every alert somebody had
-- silenced, and a silence is the single most common thing an operator does to a
-- firing alert. A dashboard counting firing alerts under-reported during exactly
-- the window somebody had silenced something, and "is anything still on fire?"
-- could not be answered from the column whose whole job is to answer it.
--
-- ⭐ THE PRODUCT ALREADY KNEW THIS AND GAVE THE SAME FACT THE OPPOSITE TREATMENT.
-- `internal/alerts/domain/snooze.go:25-32` refuses to let snooze become a State,
-- because an alert can be firing AND acked AND snoozed at once and all three are
-- displayed. Suppression is the same kind of fact -- "you will not be told" --
-- observed about another system rather than decided by oto, and it got a State
-- value. After this file there are four independent axes and none of them is
-- spelled inside another:
--
--     alerts.state                    the signal's phase        (oto's reading)
--     alerts.suppression_reason       Alertmanager's delivery   (observed)
--     alert_snoozes                   oto's own quiet           (decided)
--     alert_cases.ack_state           a human's attention       (per firing)
--
-- ⭐ THE SCHEMA HAS BEEN CARRYING THE WORKAROUND SINCE 00007. `alerts_open_idx`
-- could not spell "live" as `state = 'firing'` and spelled it
-- `state IN ('firing','suppressed')` instead. That disjunction is the defect
-- written down in DDL, and this file is what lets it go away.
--
-- ⚠️ `alert_cases.suppression_reason` AND `suppressed_by` DO NOT MOVE, and ADR
-- 0041 section 5 is the ruling. They are not a duplicate of the new columns: the
-- Alert's pair is the LIVE AXIS -- is this signal being delivered right now --
-- and is written by the same projection statement that writes `state` and
-- `current_case_id`; the Case's pair is THIS FIRING'S RECORD of having been
-- muted while it ran, which `suppress_count` makes explicit by counting
-- suppressions of one episode. A silence has a time window, so it can mute
-- episode 3 and not episode 4.
--
-- The alternative was checked before it was rejected: the case timeline DOES
-- carry both edges as first-class events (`case.suppressed`, `case.unsuppressed`,
-- SPEC §D.4.1), so dropping the Case's columns would not have destroyed history.
-- It would have turned "was this episode suppressed" from a column read into a
-- fold over an append-only, thirteen-month, monthly-partitioned table, for four
-- live readers that are asking the per-episode question. The columns stay on
-- their merits.
--
-- ⛔ NOTHING HERE MAKES `snoozed` A SUPPRESSION REASON. `alerts_supreason_ck`
-- mirrors ALERTMANAGER'S FOUR REASONS and nothing else, exactly as
-- `case_supreason_ck` has since 00007. Adding `snoozed` would make oto report
-- "Alertmanager is suppressing this" when the truth is "a human asked oto to be
-- quiet" -- a lie about the world, in the columns whose only job is to mirror it.
--
-- ⭐ THE Go `State` ENUM KEEPS ALL FOUR VALUES, AND THAT IS NOT AN OVERSIGHT.
-- `suppressed` remains the DISPLAY reading and the lifecycle machine's phase; it
-- simply stops being a value the COLUMN can hold. The API composes it back from
-- `state` + `suppression_reason` at the DTO boundary, so `?state=suppressed`, the
-- state chip and the roll-up counters are unchanged for every client. What
-- changes is that every AGGREGATE now reads a column that no longer hides firing
-- alerts inside another word.

-- +goose Up

-- ------------------------------------------------------------------ the columns
--
-- Added BEFORE the state rewrite, because the rewrite is what reads them for its
-- own correctness check below.

ALTER TABLE alerts
  ADD COLUMN suppression_reason TEXT,
  ADD COLUMN suppressed_by      JSONB NOT NULL DEFAULT '{}'::jsonb;

-- --------------------------------------------------------------------- the rows
--
-- The projection is the authority and the join is exact by construction:
-- `setProjectionBatchSQL` writes `state` and `current_case_id` in ONE statement
-- from ONE case, and it wrote `suppressed` exactly when that case was open and
-- carried a `suppression_reason`. So every `state = 'suppressed'` row has a
-- current case that knows why.

UPDATE alerts a
   SET suppression_reason = o.suppression_reason,
       suppressed_by      = o.suppressed_by
  FROM alert_cases o
 WHERE o.id = a.current_case_id
   AND a.state = 'suppressed'
   AND o.suppression_reason IS NOT NULL;

-- ⛔ A ROW THE JOIN DID NOT REACH IS A HARD STOP, NOT A DEFAULT. Migration 00054
-- put the rule in writing: "a guessed `firing` on a silenced alert is exactly the
-- visible lie SPEC §B.4 exists to prevent." Narrowing the enum over such a row
-- would erase the suppression fact entirely and leave oto claiming an alert is
-- being delivered when the last thing it knew was that it was not. Choosing a
-- reason instead would invent an Alertmanager object that may not exist. Both are
-- worse than refusing, so this refuses -- and names the repair.
-- +goose StatementBegin
DO $$
DECLARE orphans BIGINT;
BEGIN
  SELECT count(*) INTO orphans
    FROM alerts
   WHERE state = 'suppressed' AND suppression_reason IS NULL;

  IF orphans > 0 THEN
    RAISE EXCEPTION
      'migration 00055: % alert(s) read state=suppressed with no current case to say why. The projection that writes both columns cannot produce this, so the database has been changed by something other than oto. Inspect them with:  SELECT id, org_id, alert_key, current_case_id FROM alerts WHERE state = ''suppressed'' AND suppression_reason IS NULL;  then either repair current_case_id or set the reason by hand from the Alertmanager object that is muting each one. Do not guess a reason to get past this.',
      orphans;
  END IF;
END $$;
-- +goose StatementEnd

-- The narrowing itself. Every one of these rows now carries its suppression on
-- the axis beside it, so the word it loses is not a fact it stops holding.
UPDATE alerts SET state = 'firing' WHERE state = 'suppressed';

-- ---------------------------------------------------------- the new constraints

ALTER TABLE alerts DROP CONSTRAINT alerts_state_ck;

ALTER TABLE alerts ADD CONSTRAINT alerts_state_ck
  CHECK (state IN ('firing','resolved','expired'));

-- Alertmanager's four reasons and nothing else, byte-identical to
-- `case_supreason_ck`. `snoozed` is not here and never will be.
ALTER TABLE alerts ADD CONSTRAINT alerts_supreason_ck
  CHECK (suppression_reason IS NULL
         OR suppression_reason IN ('silence','inhibition','mute_time_interval','active_time_interval'));

ALTER TABLE alerts ADD CONSTRAINT alerts_suppby_ck
  CHECK (jsonb_typeof(suppressed_by) = 'object');

-- ⭐ THE AXES ARE INDEPENDENT, AND THIS IS THE ONE PLACE THEY TOUCH. A resolved
-- or expired signal is not being delivered because there is nothing to deliver,
-- which is a different fact from Alertmanager declining to route it; leaving a
-- witness on a terminal row would make oto go on saying "silenced by <id>" about
-- an alert that has ended. This is the exact shape of `case_suppress_ck`, one
-- table up.
ALTER TABLE alerts ADD CONSTRAINT alerts_suppress_ck
  CHECK (suppression_reason IS NULL OR state = 'firing');

-- ---------------------------------------------------------------- the index
--
-- ⭐ LIVENESS WITHOUT A DISJUNCTION, which is the observable point of the whole
-- file. The predicate is now a single equality, so the planner matches it against
-- `state = 'firing'` in the query without having to prove a set membership, and
-- a reader of the DDL can no longer mistake "open" for two states.

DROP INDEX alerts_open_idx;

CREATE INDEX alerts_open_idx ON alerts (org_id, last_seen_at DESC, id DESC)
                             WHERE state = 'firing';

-- ----------------------------------------------------------------- the comments

-- +goose StatementBegin
COMMENT ON COLUMN alerts.state IS
  'Projection of the current case: firing | resolved | expired, and NOTHING ELSE (ADR 0041). Suppression is an orthogonal axis on suppression_reason, so a firing alert reads firing whether or not Alertmanager is delivering it -- which is what makes this column able to answer "is anything still on fire?". expired is NOT resolved: resolved requires an explicit upstream status=resolved, expired means we stopped hearing about it. Never fabricate a resolution.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alerts.suppression_reason IS
  'Whether ALERTMANAGER is delivering this signal right now, and why not: silence | inhibition | mute_time_interval | active_time_interval. INDEPENDENT OF state (ADR 0041) -- a firing, silenced alert reads state=firing with a reason here. NULL means oto has no reason to think delivery is blocked. This is Alertmanager suppressing; oto being quiet about a signal on purpose is a snooze and lives in alert_snoozes, and putting snoozed here would report another system saying something it never said.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alerts.suppressed_by IS
  'Raw upstream attribution for suppression_reason: silencedBy / inhibitedBy / mutedBy, all three, as Alertmanager reported them. Empty object when nothing is suppressing. Projection of the current case suppressed_by.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON INDEX alerts_open_idx IS
  'Serves the default landing view -- live alerts only, same keyset as alerts_list_idx. Partial on a single equality since ADR 0041: before it, liveness could not be spelled state = firing and was spelled state IN (firing, suppressed), which is the defect that ADR written in DDL.';
-- +goose StatementEnd

-- +goose Down

-- ⭐⭐ THIS DOWN IS LOSSLESS, AND THE AXIS IS WHY. `suppressed` is rebuilt from the
-- rows rather than defaulted: an alert is suppressed exactly when it is firing and
-- carries a reason, which is the same derivation the Up ran backwards. Nothing has
-- to be guessed, because nothing was thrown away -- the Up moved a fact from
-- inside one column to beside it.

ALTER TABLE alerts DROP CONSTRAINT alerts_suppress_ck;
ALTER TABLE alerts DROP CONSTRAINT alerts_state_ck;

UPDATE alerts
   SET state = 'suppressed'
 WHERE state = 'firing' AND suppression_reason IS NOT NULL;

ALTER TABLE alerts ADD CONSTRAINT alerts_state_ck
  CHECK (state IN ('firing','suppressed','resolved','expired'));

DROP INDEX alerts_open_idx;

CREATE INDEX alerts_open_idx ON alerts (org_id, last_seen_at DESC, id DESC)
                             WHERE state IN ('firing','suppressed');

ALTER TABLE alerts DROP CONSTRAINT alerts_suppby_ck;
ALTER TABLE alerts DROP CONSTRAINT alerts_supreason_ck;
ALTER TABLE alerts DROP COLUMN suppressed_by;
ALTER TABLE alerts DROP COLUMN suppression_reason;

-- +goose StatementBegin
COMMENT ON COLUMN alerts.state IS
  'Projection of the current case: firing | suppressed | resolved | expired. expired is NOT resolved -- resolved requires an explicit upstream status=resolved; expired means we stopped hearing about it. Never fabricate a resolution.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON INDEX alerts_open_idx IS
  'Serves the default landing view -- open alerts only, same keyset. Partial so the resolved/expired long tail never enters the index.';
-- +goose StatementEnd
