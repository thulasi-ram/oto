-- ADR 0040 -- an AlertCase is OPEN or CLOSED, and nothing else. The four §B.2
-- values `firing | suppressed | resolved | expired` describe THE ALERT: they are
-- what the world is doing to a label set, and the Alert is the identity that
-- label set has. A Case is one ephemeral firing episode of one Alert, and the
-- only question it can answer about itself is whether it is still running.
--
-- ⭐⭐ WHY THE COLUMN WAS WRONG RATHER THAN MERELY WIDE. `alert_cases.state` held
-- all four values, which made every one of them SAY something about the episode
-- that only the Alert could mean:
--
--   `suppressed`  a silence is an ALERT-scoped fact. It mutes a label set, not an
--                 episode of it, and 00017 already said `state` MUST NEVER carry
--                 a snooze. The episode's part in it -- WHICH silence, and why --
--                 is `suppression_reason` and `suppressed_by`, and those stay
--                 exactly where they are (see the ⚠️ below).
--   `resolved`    is a claim about WHY the episode ended. It survives, in
--     /`expired`  `resolve_reason`, which is the column that has always held the
--                 distinction and is now the only place it lives.
--
-- What is left once those move out is a two-valued fact that `ended_at` was
-- already telling the truth about, and `case_terminal_ended` has tied the two
-- together since 00007. This file makes the column say the same thing as the
-- timestamp beside it instead of four things, three of which belonged elsewhere.
--
-- ⭐ NOTHING IS LOST, AND THAT IS CHECKABLE. The four-way value is DERIVED, not
-- deleted, and the derivation is total:
--
--     state='open'   AND suppression_reason IS NULL      ->  firing
--     state='open'   AND suppression_reason IS NOT NULL  ->  suppressed
--     state='closed' AND resolve_reason = 'upstream'     ->  resolved
--     state='closed' AND resolve_reason = 'timeout'      ->  expired
--
-- `case_resolve_ck` makes `resolve_reason` present exactly when closed and
-- `case_resreason_ck` bounds it to those two values, so the bottom two arms are
-- exhaustive; the new `case_suppress_ck` keeps `suppression_reason` off closed
-- rows, so the top two are. That is also what makes this migration REVERSIBLE:
-- the Down below rebuilds all four values from the rows, not from a default.
--
-- ⛔ THE SQL READERS DO NOT USE THAT DERIVATION FOR THE OPEN HALF, AND THE ADR
-- SAYS WHY. `suppression_reason`'s presence is no longer a CHECKED biconditional
-- (it cannot be -- there is no state to bind it to), so a query that read it as
-- "this is suppressed" would be trusting an invariant the schema stopped
-- enforcing. Every aggregate that needs `firing` apart from `suppressed` asks
-- `alerts.state`, which is a first-class CHECKed enum, and joins by primary key
-- to do it. An OPEN case is always its Alert's `current_case_id`
-- (`case_one_open_idx` is UNIQUE (alert_id) WHERE ended_at IS NULL), so the
-- Alert's state IS this episode's state whenever this episode is running.
--
-- ⚠️ `suppression_reason` AND `suppressed_by` DO NOT MOVE, DELIBERATELY. Whether
-- they belong on the Alert has not been decided, so this file makes the minimal
-- change: `case_suppress_ck` was `(state = 'suppressed') = (suppression_reason IS
-- NOT NULL)` and could not survive the narrowing, so it becomes the half of that
-- biconditional the schema can still state -- a CLOSED case cannot be suppressed.
-- The other half is GONE rather than moved, and it had to be: "suppressed" is now
-- the READING OF having a reason (see the derivation above), so "suppressed
-- implies a reason" became a tautology rather than a check. `Case.check()`
-- re-proves exactly the surviving direction, on every construction and every
-- transition. The columns keep their names, their types and their rows, and ADR
-- 0040 section 8 lists precisely what is left enforcing them.
--
-- ⭐ AND THE RE-FIRE ALWAYS OPENS A NEW EPISODE NOW, so `reopen_count` and
-- `reopen_of` go. A Case is strictly terminal: `open -> closed`, once. The old
-- §B.5 T8 edge reopened a CLOSED episode when the re-fire landed inside
-- `refire_grace`, which meant a closed episode could become open again and carry
-- its acknowledgement across a gap in the firing. ADR 0040 reverses that: a
-- re-fire opens the next `seq`, UNACKED, always. `case.reopened` is RETIRED
-- rather than deleted -- thirteen months of `alert_events` rows spell it and must
-- stay decodable -- exactly as 00051 retired `group.member_joined`.
--
-- ⛔ THIS FILE REWRITES EVERY ROW OF `alert_cases`, AND 00052 REFUSED TO DO THAT
-- TO `alert_events` FOR A REASON THAT DOES NOT APPLY HERE. `alert_events` is
-- monthly-partitioned, append-only and retained thirteen months, so an UPDATE
-- over it rewrites cold partitions nobody was going to read. `alert_cases` is
-- bounded by the same retention but is orders of magnitude smaller -- one row per
-- firing episode, not one per transition -- and there is no way to change the
-- meaning of a value in place. The UPDATE takes ROW EXCLUSIVE; the four ADD
-- CONSTRAINTs take ACCESS EXCLUSIVE and each validates with a full scan.
--
-- ⛔ EVERY `COMMENT ON` BELOW IS WRAPPED IN StatementBegin/End, AND IT IS NOT
-- DECORATION. goose ends a statement at a line whose last word before the first
-- `--` token ends in `;`, so a ` -- ` INSIDE a comment string hides the
-- terminating semicolon and the statement never closes -- "unexpected unfinished
-- SQL query", the whole migration refused. 00048 and 00049 wrap theirs for the
-- same reason and this has already broken the stack once.

-- +goose Up

-- ------------------------------------------------------------- the constraints
--
-- Dropped BEFORE the UPDATE, not after: `case_state_ck` forbids 'open' and
-- 'closed', and `case_resolve_map_ck` pairs `resolve_reason` with a state literal
-- that is about to stop existing. Either one would abort the rewrite.

ALTER TABLE alert_cases DROP CONSTRAINT case_state_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_terminal_ended;
ALTER TABLE alert_cases DROP CONSTRAINT case_resolve_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_suppress_ck;

-- ⭐ `case_resolve_map_ck` IS NOT REPLACED, AND THAT IS THE POINT OF THE FILE. It
-- said `(state='resolved' AND resolve_reason='upstream') OR (state='expired' AND
-- resolve_reason='timeout')` -- a lock between two columns that carried the SAME
-- fact. One of them is now the only one that carries it, so there is nothing left
-- to lock together. `case_resreason_ck` already bounds the value to exactly those
-- two spellings and needs no widening: `upstream` IS resolved and `timeout` IS
-- expired, so the distinction 00007 called "the one oto must never fabricate"
-- survives this file with its enforcement intact and its subject reduced by one.
ALTER TABLE alert_cases DROP CONSTRAINT case_resolve_map_ck;

-- --------------------------------------------------------------------- the rows

UPDATE alert_cases
   SET state = CASE WHEN state IN ('firing', 'suppressed') THEN 'open' ELSE 'closed' END;

-- ---------------------------------------------------------- the new constraints

ALTER TABLE alert_cases ADD CONSTRAINT case_state_ck
  CHECK (state IN ('open', 'closed'));

-- Unchanged in meaning, restated in the surviving vocabulary: the episode has an
-- `ended_at` exactly when it is closed.
ALTER TABLE alert_cases ADD CONSTRAINT case_terminal_ended
  CHECK ((state = 'closed') = (ended_at IS NOT NULL));

-- `resolve_reason` is now the SOLE record of resolved-versus-expired, so the
-- constraint that guarantees a closed episode has one is load-bearing in a way it
-- was not before: without it a closed Case could say nothing about why it ended.
ALTER TABLE alert_cases ADD CONSTRAINT case_resolve_ck
  CHECK ((state = 'closed') = (resolve_reason IS NOT NULL));

-- The surviving half of the old biconditional. See the ⚠️ in the header: the
-- other half -- that an OPEN, suppressed episode HAS a reason -- is a domain
-- invariant now, because the schema no longer has a `suppressed` state to bind it
-- to and inventing one on another column would pre-empt a decision not yet made.
ALTER TABLE alert_cases ADD CONSTRAINT case_suppress_ck
  CHECK (suppression_reason IS NULL OR state = 'open');

-- ------------------------------------------------------------------ the columns

ALTER TABLE alert_cases DROP CONSTRAINT case_reopen_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_reopenof_ck;
ALTER TABLE alert_cases DROP COLUMN reopen_count;
ALTER TABLE alert_cases DROP COLUMN reopen_of;

-- ----------------------------------------------------------------- the comments

-- +goose StatementBegin
COMMENT ON TABLE alert_cases IS
  'One CONTIGUOUS FIRING EPISODE of an Alert, identified by (alert_id, seq). This is what you acknowledge and whose firing duration is measured -- the duration belongs to the SIGNAL and no per-person timing is stored anywhere (SPEC A.1, R8). It is STRICTLY TERMINAL: open -> closed, once, and a re-fire opens the next seq rather than reviving this one (ADR 0040). At most one may be open per Alert, enforced by case_one_open_idx.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.state IS
  'open | closed, and nothing else (ADR 0040). The four SPEC B.2 values describe the ALERT, not one episode of it: firing-versus-suppressed is alerts.state, and resolved-versus-expired is this row own resolve_reason. Both are recoverable from a Case at any time, which is why narrowing this column lost nothing.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.ended_at IS
  'OTO clock. NULL exactly while the episode is open (case_terminal_ended), which since ADR 0040 makes it the SAME fact as state and the only spelling of it a partial index can be proved against.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.resolve_reason IS
  'upstream (an explicit status=resolved arrived) or timeout (we stopped hearing about it). Since ADR 0040 this is the SOLE record of resolved-versus-expired on a Case: state says only that the episode closed, and case_resolve_ck guarantees a closed episode says why. oto can still never claim resolved when it means expired, because it can no longer claim resolved at all without this column saying upstream.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.suppression_reason IS
  'Why the episode was suppressed: silence | inhibition | mute_time_interval | active_time_interval. Present only on an OPEN episode (case_suppress_ck), and cleared when it closes. It stays on the Case as a per-firing record of WHICH upstream object muted this episode; whether suppression belongs to the Alert instead is undecided (ADR 0040), so nothing about these two columns moved.';
-- +goose StatementEnd

-- +goose Down

-- ⭐⭐ THIS DOWN IS LOSSLESS FOR `state`, AND `suppression_reason` IS WHY. All four
-- values are rebuilt from the rows rather than defaulted: `resolve_reason` names
-- resolved apart from expired on the closed half, and `suppression_reason` --
-- which this migration deliberately left on the Case -- names suppressed apart
-- from firing on the open half. Had those columns moved to the Alert, the
-- rollback would have had to guess, and a guessed `firing` on a silenced alert is
-- exactly the visible lie SPEC B.4 exists to prevent.
--
-- `reopen_count` is the one value that does NOT come back: it counted T8 reopens,
-- the edge ADR 0040 retired, so there is nothing left in the schema that
-- witnesses them and it restores as its DEFAULT. `reopen_of` DOES come back, and
-- from the authority rather than from a default: `seq` is 1-based and gapless, and
-- every episode above the first was opened by a re-fire of the one below it, so
-- the predecessor is the row at `seq - 1`.

ALTER TABLE alert_cases ADD COLUMN reopen_count INT NOT NULL DEFAULT 0;
ALTER TABLE alert_cases ADD COLUMN reopen_of UUID;

UPDATE alert_cases c
   SET reopen_of = p.id
  FROM alert_cases p
 WHERE p.org_id = c.org_id
   AND p.alert_id = c.alert_id
   AND p.seq = c.seq - 1;

ALTER TABLE alert_cases ADD CONSTRAINT case_reopen_ck   CHECK (reopen_count >= 0);
ALTER TABLE alert_cases ADD CONSTRAINT case_reopenof_ck CHECK (reopen_of IS NULL OR reopen_of <> id);

ALTER TABLE alert_cases DROP CONSTRAINT case_suppress_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_resolve_ck;
ALTER TABLE alert_cases DROP CONSTRAINT case_terminal_ended;
ALTER TABLE alert_cases DROP CONSTRAINT case_state_ck;

UPDATE alert_cases
   SET state = CASE
                 WHEN state = 'open' AND suppression_reason IS NOT NULL THEN 'suppressed'
                 WHEN state = 'open'                                    THEN 'firing'
                 WHEN resolve_reason = 'timeout'                        THEN 'expired'
                 ELSE 'resolved'
               END;

ALTER TABLE alert_cases ADD CONSTRAINT case_state_ck
  CHECK (state IN ('firing', 'suppressed', 'resolved', 'expired'));
ALTER TABLE alert_cases ADD CONSTRAINT case_terminal_ended
  CHECK ((state IN ('resolved', 'expired')) = (ended_at IS NOT NULL));
ALTER TABLE alert_cases ADD CONSTRAINT case_suppress_ck
  CHECK ((state = 'suppressed') = (suppression_reason IS NOT NULL));
ALTER TABLE alert_cases ADD CONSTRAINT case_resolve_ck
  CHECK ((state IN ('resolved', 'expired')) = (resolve_reason IS NOT NULL));
ALTER TABLE alert_cases ADD CONSTRAINT case_resolve_map_ck
  CHECK (resolve_reason IS NULL
         OR (state = 'resolved' AND resolve_reason = 'upstream')
         OR (state = 'expired'  AND resolve_reason = 'timeout'));

-- +goose StatementBegin
COMMENT ON TABLE alert_cases IS
  'One CONTIGUOUS FIRING EPISODE of an Alert, identified by (alert_id, seq). This is what you acknowledge and whose firing duration is measured -- the duration belongs to the SIGNAL and no per-person timing is stored anywhere (SPEC A.1, R8). At most one may be open per Alert, enforced by case_one_open_idx.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.state IS
  'firing | suppressed | resolved | expired. suppressed is INVISIBLE to webhooks -- Alertmanager MuteStage drops muted alerts before the webhook fires, so only the API v2 reconciler can ever set it (SPEC B.2).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.ended_at IS
  'OTO clock. NULL exactly while the episode is open (case_terminal_ended).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.resolve_reason IS
  'upstream (an explicit status=resolved arrived) or timeout (we stopped hearing about it). The pair state/resolve_reason is locked together by case_resolve_map_ck so oto can never claim resolved when it means expired.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.suppression_reason IS
  'Why it is suppressed: silence | inhibition | mute_time_interval | active_time_interval. Present exactly when state=suppressed (case_suppress_ck).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.reopen_of IS
  'The previous Case when this episode re-fired within the grace window (transition T7, SPEC B.5).';
-- +goose StatementEnd
