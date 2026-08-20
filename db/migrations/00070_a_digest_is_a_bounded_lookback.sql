-- git-bug 893cee4, a8a4010, 342e071 — one design, because they are one bug seen from
-- three angles.
--
-- ⭐⭐ THE ROOT. `notifications.digest_window_start` on the newest digest row was ONE
-- FACT ASKED TO ANSWER THREE DIFFERENT QUESTIONS, and it can answer none of them:
--
--   * WAS THIS SPAN EXAMINED AT ALL? The cursor was `max(digest_window_start)`, which
--     only ever moved as a side effect of SENDING. A window examined and found below
--     its policy's floor writes no row -- deliberately, because a `suppressed` row per
--     empty window would put one row per policy per ten minutes into the audit log
--     forever -- so "examined and quiet" and "never examined" produced an IDENTICAL
--     ABSENCE and the cursor froze. The next tick re-derived the same owed span one
--     window longer, and after `MaxDigestBackfill` quiet windows every single tick ran
--     six aggregate queries and logged a data-loss WARN whose `skipped_windows` count
--     grew without bound -- on a policy that had never had anything to send. An
--     operator who alerted on that message got paged by every quiet namespace they
--     had; one who did not got a log in which the single occurrence that meant
--     something was buried under thousands that did not. (893cee4)
--
--   * HOW FAR DID THE EXAMINATION REACH IN CALENDAR TIME? A window START is a span
--     only in combination with the window LENGTH that was in force when the digest was
--     sent, and 00058 stored the start and the ordinal and never the length. So
--     `DigestWindows` re-floored the start under the CURRENT length and stepped forward
--     by the CURRENT length. Narrow a policy from an hour to ten minutes -- both
--     admissible, both divisors of 86400 -- and the hour a digest had already
--     summarised was re-tiled into six new windows that all sat AFTER the recorded
--     start and were therefore all treated as uncovered: five fresh digests about a
--     span the operator had already read, each its own thread reply, arriving at the
--     exact moment somebody was tuning the policy because it was already too noisy.
--     Neither guard catches it. `notif_digest_uniq` keys on the start and `12:10` is
--     not `12:00`; `WindowOrdinal` divides by the current length so the §C.7 key
--     differs too. Both were working exactly as designed against a repeated tick, and
--     a re-tiling is not one. (342e071)
--
--   * WAS THE EXAMINATION'S DATA COMPLETE? `alert_cases.started_at` is oto's clock
--     read at the START of a batch's processing -- `ingestion/service/process.go` takes
--     one `now` in `plan` and stamps every alert in the batch with it -- so the instant
--     on the row is BEFORE the transaction that inserted it, by decoding, the rejection
--     write, chunking, grouping, the lifecycle machine and an fsync. The digest tick
--     reads a closed window a fraction of a second after its boundary and cannot see a
--     Case whose `started_at` falls inside the window but whose transaction has not
--     committed. It then writes the digest and advances past it, and no later window's
--     predicate can reach back, because every later window starts at or after the
--     boundary that Case is on the wrong side of. The episode is uncounted, with no
--     row, no log line and no `suppressed_reason` -- which is precisely the shape §B.6
--     refuses: "a suppressed notification and a signal that never fired look identical
--     to the person who was not told". It is also worse under load, which is when it
--     matters most: the plan-to-commit gap widens with batch size and contention, so
--     the busier the incident the wider the vulnerable band at every boundary.
--     (a8a4010)
--
-- ⭐⭐ THE RULING: A DIGEST IS A BOUNDED-LOOKBACK SET OF CASES, NOT A SLICE OF CLOCK.
--
--     digest = Cases in [T, T+W)  ∪  Cases in [T-L, T)  minus those already accounted for
--
-- `L` is `notification/domain.DigestLookback` -- how late a Case may commit and still
-- be counted. Two consequences, and both are the point. Dedupe state has to span only
-- `L` and the widest window a policy may hold, never all of history, so there is no
-- permanent membership record and no write amplification proportional to the past. And
-- nothing is ever OWED: a straggler is picked up by the next digest instead of being
-- missed by a frozen cursor, so `skipped_windows` stops being a backlog and is deleted
-- rather than redefined.
--
-- ⛔ WHY NOT `now - safety_lag` AS THE CURSOR, which is the design that suggests
-- itself: it converts one unmeasurable number into PERMANENT SILENT LOSS. A Case is
-- lost exactly when `commit_delay > safety_lag`, and the effective margin is
-- `safety_lag - (reader_clock - writer_clock)`, so inter-pod skew eats the budget
-- invisibly -- git-bug b21ba93 ("Six tables can 500 when the updating pod's clock lags
-- the creating pod's") is this repo's standing record of paying that tax. Under a
-- bounded lookback, exceeding `L` produces a DUPLICATE rather than a hole, and a
-- visible duplicate beats an invisible omission.
--
-- ⛔ WHY NOT AN `xid8` VISIBILITY WATERMARK, which is strictly more correct: it needs
-- its own column defaulted from `pg_current_xact_id()`, it stalls behind any
-- long-running transaction (one idle-in-transaction psql session freezes coverage until
-- it closes and then delivers one enormous digest), and it still cannot answer "what
-- span does this digest cover" -- an xid is not a time, so the span columns below would
-- be needed alongside it regardless. Correctness that costs a second mechanism, for a
-- failure mode the lookback already downgrades to "counted twice".
--
-- ⛔ AND WHY NOT STAMP THE CASE AT COMMIT, which would be surgically exact for a8a4010
-- and does nothing for the other two: `started_at` is read across `internal/alerts` for
-- episode duration and timeline ordering, and `lifecycle.go` is explicit that
-- `recorded_at` and `occurred_at` are NEVER conflated.
--
-- ⛔⛔ AND `L` IS NOT `MaxDigestBackfill` UNDER ANOTHER NAME. THEY ARE TWO NUMBERS
-- DOING TWO JOBS AND COLLAPSING THEM WOULD DELETE A DOCUMENTED BEHAVIOUR NOBODY RULED
-- ON:
--
--     L                    how late a CASE may commit and still be counted
--                          -- straggler budget, sized against write visibility, seconds
--     MaxDigestBackfill    how many owed WINDOWS a recovered tick still emits
--                          -- outage horizon, sized against how long oto was gone
--
-- `MaxDigestBackfill` has a recorded derivation for exactly its case ("at least half an
-- hour of catch-up... covers a deploy, a leader election, a failover and a short
-- database incident") and `TestAMissedTickWritesAtMostSixDigestsAndSaysWhatItDropped`
-- pins it. Under a single-number clamp with L = 15m and W = 10m that test produces two
-- digests plus a tail and logs nothing -- so after any outage longer than `L`, oto
-- would silently stop sending the digests it owed. `L` stays the lateness term ONLY.
--
-- ---------------------------------------------------------------- WHAT THIS ADDS
--
-- THREE FACTS, AND THEY ARE THREE BECAUSE THEY ANSWER THREE QUESTIONS. The temptation
-- is to make one of them serve two, which is the mistake this migration exists to undo.
--
--   1. `notifications.digest_covered_from` / `digest_covered_to` -- WHAT THIS MESSAGE
--      COVERED. Immutable, written once, on the row. It is what lets a card state its
--      own span instead of inferring one from the policy's CURRENT `digest_window_s`,
--      and 342e071's third done-when clause -- that the meaninglessness of a stored
--      start without its length be stated where the cursor is READ -- can only become
--      truthful once the row carries its own span.
--   2. `notification_digest_coverage` -- HOW FAR THE POLICY HAS BEEN EXAMINED. Mutable,
--      monotone, one row per policy. It advances for every window the tick LOOKED at,
--      including a quiet one, which is the half of 893cee4 the arithmetic does not fix.
--      It is NOT `max(digest_covered_to)` over the digests: that is a fact about
--      messages, and a quiet policy sends none.
--   3. `notification_digest_cases` -- WHICH EPISODES A POLICY HAS ACCOUNTED FOR. The
--      dedupe state the lookback needs, and the §B.6 receipt in its STRONG form: a mark
--      with no report is oto saying "I looked at this episode and it did not clear your
--      floor", and the ABSENCE of a mark on a matched Case older than `L` is the
--      unrecoverable gap.
--
-- ⛔ THE RECONCILIATION DETECTOR IS NOT OPTIONAL AND IT IS NOT A SQL ANTI-JOIN. It is
-- the half that makes the lookback defensible rather than hopeful: `L` downgrades a
-- missed Case to a duplicate only for lateness under `L`, and past that the Case is
-- still lost. Pre-release the goal is AUDITABLE rather than provably correct -- if it
-- happens, it must be found from a number somebody can alarm on and not from a
-- customer. It cannot be written here because "appears in no digest" is ambiguous
-- between MISSED and NO POLICY SELECTS IT, and which policies select a Case is decided
-- in Go: `Policy.Matches` compiles Alertmanager-anchored regular expressions with a
-- missing-label rule (absent means empty string) that Postgres's `~` does not share. So
-- `DigestService.ReconcileOrg` is a per-policy fold reusing the tick's own matcher, and
-- the absence of a mark is the evidence it reads. That is why this table exists and why
-- its retention is bounded rather than absent.
--
-- ⭐ THERE IS NO BACKFILL, AND THAT IS THE HONEST CHOICE RATHER THAN A SHORTCUT. Every
-- digest row written before this migration genuinely does not know its own span, and
-- the only way to invent one is `digest_window_start + policies.digest_window_s AS IT
-- IS TODAY` -- which is precisely the inference 342e071 is about. Populating a coverage
-- row from it would be the same lie one layer down. So the columns are NULLABLE, the
-- coverage table starts EMPTY, and a policy whose only digests predate this migration
-- reads as "never examined" -- which `DigestWindows` answers by covering exactly ONE
-- window, the most recent closed one. First tick after the upgrade: at most one digest
-- per policy, no flood, no duplicate, and the design is fully in force from the second
-- tick onward.
--
-- ⭐ EXPAND/CONTRACT SAFE UNDER RELEASE N. Two new NULLABLE columns whose CHECK every
-- existing row satisfies vacuously, plus two new tables release N never reads. Release
-- N keeps deriving its cursor from `max(digest_window_start)` and keeps working exactly
-- as it did; release N+1 derives it from `notification_digest_coverage`. Nothing is
-- dropped, nothing is narrowed, no column changes type. The two releases disagree about
-- which windows are owed for as long as they run together, and the disagreement is
-- bounded by `notif_digest_uniq`: whichever pod gets there first owns the window and
-- the other reads its 23505 as "already covered".
--
-- ⚠️ WHAT THE DOWN DESTROYS, STATED BEFORE IT IS RUN. The span columns and the coverage
-- cursors are recoverable -- release N re-derives a window-start cursor from the digest
-- rows, which is worse but not lost. THE MARKS ARE NOT. They are the only record of
-- which episodes oto accounted for, so dropping them destroys the evidence
-- `ReconcileOrg` reads, and any gap that had not yet been noticed becomes permanently
-- unnoticeable. The Down cannot compute whether one is outstanding -- that needs
-- `Policy.Matches` in Go, which is the same reason the detector is not a query -- so it
-- reports what it is about to destroy rather than deciding for the operator, and it
-- hard-stops only on evidence that is already inconsistent with anything oto could have
-- written.

-- +goose Up

-- ⛔ THE PREMISE, CHECKED FIRST. Bounded lookback makes the QUERY overlap-tolerant; the
-- "one digest per policy per window" guarantee is still carried entirely by 00058's
-- partial unique index, and by nothing else. The coverage cursor deliberately does not
-- carry it -- a cursor that also enforced uniqueness would be the "second fact about
-- the same thing, written in a second statement" that 00058's own comment refuses. So
-- if that index has gone, this migration would ship a design whose idempotency rests on
-- an object that is not there: two pods would both send, and the coverage cursor would
-- advance over the duplicate and hide it.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE c.relkind = 'i' AND c.relname = 'notif_digest_uniq'
       AND n.nspname = current_schema()
  ) THEN
    RAISE EXCEPTION
      'migration 00070: notif_digest_uniq is missing. It is migration 00058''s partial unique index on notifications (org_id, policy_id, digest_window_start) WHERE subject_kind = ''digest'', and it is the ONLY thing that stops two ticks from both sending one window''s digest -- the coverage cursor this migration adds is a position, not a constraint, and will happily advance over a duplicate. Restore it with:  CREATE UNIQUE INDEX notif_digest_uniq ON notifications (org_id, policy_id, digest_window_start) WHERE subject_kind = ''digest'';  then run this migration again. Do not remove this guard to get past it.';
  END IF;
END $$;
-- +goose StatementEnd

-- ------------------------------------------------- notifications: the covered span

-- The SPAN THIS MESSAGE COVERED. See the header: `digest_window_start` is a span only
-- in combination with the length that was in force when it was sent, and 00058 stored
-- the start and the ordinal and never the length.
--
-- ⚠️ BOTH ARE NULLABLE AND STAY NULL ON EVERY PRE-00070 ROW. Backfilling means
-- multiplying the stored start by the policy's window AS IT IS TODAY, which is the one
-- arithmetic these two columns exist to retire. A row that does not know its span says
-- so.
--
-- ⭐ `from` MAY REACH BACK PAST THE WINDOW AND `to` MAY NOT REACH FORWARD, AND THE
-- ASYMMETRY IS THE LOOKBACK ITSELF. A digest counts stragglers out of
-- `[window_start - L, window_start)`, so its coverage genuinely begins before its
-- window when it swept one up; it can never end after `window_start + W`, because the
-- window after that is still open and covering it would burn the idempotency key for a
-- window whose real contents have not happened yet.
ALTER TABLE notifications ADD COLUMN digest_covered_from TIMESTAMPTZ;
ALTER TABLE notifications ADD COLUMN digest_covered_to   TIMESTAMPTZ;

-- ⚠️ FOUR SEPARATE CLAUSES AND NOT ONE CONJUNCTION, FOR THE REASON 00058 SPELLS OUT AT
-- `notifications_digest_ck`: a CHECK passes on NULL, so folding a range test into an
-- equality makes the whole predicate NULL -- and therefore true -- for exactly the
-- half-written row the constraint exists to refuse.
--
--   1. BOTH OR NEITHER. A `from` with no `to` is a span that never ends; a `to` with no
--      `from` is a length nothing can compute. Neither is renderable, and a renderer
--      that has to guess is back to reading the policy's current window.
--   2. ONLY A DIGEST HAS ONE. Nothing else in `notifications` is about a span of time,
--      and a non-digest row carrying one would be a fact no reader knows how to read.
--   3. ORDERED, STRICTLY. A zero-length span would assert that a digest covered no
--      time at all while carrying a count of episodes that happened inside it.
--   4. IT CONTAINS THE WINDOW'S START. This is the clause that makes the pair MEAN
--      something rather than merely be present: `from` at or before the start (the
--      lookback tail), `to` strictly after it (the window is not empty of time). A row
--      that satisfies this cannot claim a span that misses its own window.
ALTER TABLE notifications ADD CONSTRAINT notifications_digcover_ck CHECK (
      (digest_covered_from IS NULL) = (digest_covered_to IS NULL)
  AND (digest_covered_from IS NULL OR subject_kind = 'digest')
  AND (digest_covered_from IS NULL OR digest_covered_to > digest_covered_from)
  AND (digest_covered_from IS NULL OR digest_window_start IS NULL
       OR (digest_covered_from <= digest_window_start
           AND digest_covered_to  >  digest_window_start))
);

-- +goose StatementBegin
COMMENT ON COLUMN notifications.digest_covered_from IS
  'The INCLUSIVE start of the span this digest actually covered, which is at or BEFORE digest_window_start. It reaches back when the digest swept up a Case whose transaction committed too late for the previous window''s read (notification/domain.DigestLookback), so the honest sentence is "since the last digest, plus stragglers". ⛔ NULL on every digest written before migration 00070 and it stays NULL: the only way to invent one is digest_window_start + the policy''s CURRENT digest_window_s, which is exactly the inference git-bug 342e071 is about -- an operator who narrows a window would retroactively change the span every card oto has ever drawn claims to cover.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.digest_covered_to IS
  'The EXCLUSIVE end of the span this digest covered: the window''s end, and therefore the instant coverage reached. Together with digest_covered_from it is what lets a card state its own span instead of multiplying digest_window_start by the policy''s current digest_window_s. ⚠️ IT IS NOT THE TICK''S CURSOR. The cursor is notification_digest_coverage.covered_to, which advances for every window EXAMINED and not only for every window SENT -- a quiet policy sends nothing, and deriving the cursor from the messages is what froze it (git-bug 893cee4). One is evidence; the other is a position.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_digcover_ck ON notifications IS
  'The coverage span is both-or-neither, present only on a digest, strictly ordered, and CONTAINS its own window start (from <= digest_window_start < to). The four clauses are separate rather than one conjunction because a CHECK passes on NULL, so a range test folded into an equality is vacuously true for exactly the half-written row it was meant to refuse -- the hole migration 00058 closed at notifications_digest_ck. Absence is admitted because a digest written before 00070 does not know its span and inventing one from the policy''s current window is the bug this pair exists to fix.';
-- +goose StatementEnd

-- --------------------------------------------- notification_digest_coverage: the cursor

-- HOW FAR EACH POLICY HAS BEEN EXAMINED. One row per policy, mutable, monotone.
--
-- ⭐⭐ IT IS A SEPARATE FACT FROM "A DIGEST WAS SENT", AND THAT SEPARATION IS THE FIX
-- FOR 893cee4. Migration 00058's cursor was `max(digest_window_start)` over the digests
-- themselves, under a comment that argued the property was a virtue: "the digest row IS
-- the cursor, so the only state that can exist is state a digest was actually sent
-- for". The property is real -- it makes "covered exactly once even across a restart"
-- true by construction rather than by bookkeeping -- and it is also exactly why a
-- quiet policy could never move forward. A window below its floor writes no row, so it
-- is indistinguishable from a window never examined, so the cursor stays where it is,
-- so the next tick re-derives the same span one window longer, forever.
--
-- ⚠️ SO THE THING 00058 WARNED ABOUT IS ACCEPTED ON PURPOSE. It said a separate cursor
-- column would be "a second fact about the same thing, written in a second statement,
-- which can commit without its digest or vice versa". True -- and they are not the same
-- thing. A digest row says "oto told somebody about this window". This row says "oto
-- looked at everything up to here". The second can be true when the first is absent,
-- which is the whole point, and the two disagreeing costs at most a re-examination
-- whose episodes are already marked and which therefore sends nothing.
CREATE TABLE notification_digest_coverage (
  org_id     UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  policy_id  UUID        NOT NULL REFERENCES notification_policies(id) ON DELETE CASCADE,

  -- The INSTANT examination reached, exclusive. Always a window boundary in practice;
  -- read through `Digest.WindowStart` so that an instant strictly inside a window --
  -- which is what a re-tiling leaves behind -- names the window that is not yet fully
  -- covered rather than the one after it.
  covered_to TIMESTAMPTZ NOT NULL,

  -- ⚠️ NO `DEFAULT now()`, DELIBERATELY. Migration `00034_app_clock_remaining`
  -- stripped the default from every table whose timestamps the APPLICATION stamps,
  -- keeping it only on the six operator/infrastructure tables nothing in Go writes a
  -- clock into. This is an application-stamped column: `AdvanceCoverage` supplies it
  -- explicitly (`repository/digest.go`), so a default would never fire in production
  -- and would exist only to be reached by a path that FORGOT to pass the clock --
  -- exactly the bug `00034` describes, because the row would then carry the database's
  -- wall clock instead of the injected one, and a test holding its clock still would
  -- see time move anyway.
  updated_at TIMESTAMPTZ NOT NULL,

  CONSTRAINT digest_coverage_pk PRIMARY KEY (org_id, policy_id)
);

-- +goose StatementBegin
COMMENT ON TABLE notification_digest_coverage IS
  'The digest tick''s cursor: how far each policy has been EXAMINED, as an instant. One row per policy, upserted with GREATEST so it can only move forward. ⛔ IT IS DELIBERATELY NOT max(notifications.digest_covered_to). That would be a fact about MESSAGES, and a policy whose namespace has been quiet for a week sends none -- which is the whole of git-bug 893cee4: because the old cursor only advanced as a side effect of sending, a quiet policy re-derived a span that grew by one window every window, ran MaxDigestBackfill aggregate queries every sixty seconds forever, and logged a data-loss warning about a backlog nothing was ever owed. Coverage is a property of the READER, not of the message. ⚠️ It starts EMPTY at migration time and is not backfilled: deriving it from digest_window_start would need the window length that was in force when each digest was sent, which is the fact nothing stored (git-bug 342e071). An empty row set reads as "never examined", which covers exactly one window.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_digest_coverage.covered_to IS
  'The EXCLUSIVE instant this policy has been examined up to. An INSTANT and not a window start, which is the fix for git-bug 342e071: a start only means a span in combination with the length in force when it was written, so re-flooring it under a NEW digest_window_s re-tiled a span an earlier digest had already summarised and reported every episode in it again. An instant does not change meaning when the tiling changes. It advances for every window the tick examined -- sent, below the floor, or already covered by another pod -- and NOT for a window that could not be examined to a conclusion, which is why a policy whose channels are all disabled keeps owing its windows instead of losing them.';
-- +goose StatementEnd

-- ------------------------------------------- notification_digest_cases: the marks

-- WHICH EPISODES EACH POLICY HAS ACCOUNTED FOR. One narrow row per (policy, Case) the
-- policy MATCHED.
--
-- ⭐⭐ IT IS THE DEDUPE STATE THE LOOKBACK NEEDS, AND IT IS ALSO THE §B.6 RECEIPT IN
-- ITS STRONG FORM. Two readings of one row:
--
--   * `reported_in` NOT NULL -- "this episode is in that digest". Without it, a digest
--     reading a `DigestLookback` tail would re-report the whole tail in every single
--     message, which is a louder bug than the one the tail fixes.
--   * `reported_in` NULL -- "oto examined this episode and the window it fell in did
--     not clear your floor". That is the sentence 00058's below-floor `continue` could
--     never write down, and its absence is what made "quiet" and "never looked at" the
--     same absence.
--
-- And the ABSENCE OF A ROW for a Case a policy matched and that is older than
-- `DigestLookback` is the unrecoverable gap. That is what `ReconcileOrg` counts, and it
-- is why "oto's quiet must be inspectable" is answerable here in its strong form rather
-- than the downgraded one.
--
-- ⛔ MATCHED EPISODES ONLY, WHICH IS WHY THE DETECTOR CANNOT BE A QUERY. A mark means
-- "this POLICY accounted for this episode". Marking an episode a policy does not select
-- would make it impossible to tell a missed report from an episode no policy was ever
-- interested in -- and whether a policy selects an episode is decided in Go, by a
-- compiled Alertmanager-anchored regular expression whose missing-label rule Postgres's
-- `~` does not share. So the detector is a per-policy fold reusing `Policy.Matches`,
-- and this table is the half of it that SQL can hold.
CREATE TABLE notification_digest_cases (
  org_id      UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  policy_id   UUID        NOT NULL REFERENCES notification_policies(id) ON DELETE CASCADE,

  -- ⚠️ NO FK, ON THE SAME TERMS notifications.alert_id AND case_id HAVE NONE.
  -- `alert_cases` is reapable (ADR 0024, `case.reap`), and a mark whose Case has aged
  -- out is still the truthful statement that oto accounted for it. A CASCADE would
  -- delete the receipt and make the reconciler report a reaped episode as one nobody
  -- was ever told about; the retention sweep below is what bounds this table, not the
  -- parent's lifetime.
  case_id     UUID        NOT NULL,

  -- The Case's `started_at`, copied. It is what both readers range on -- the tail
  -- subtraction and the retention sweep -- and copying it is what lets them do that
  -- without joining a reapable table whose row may be gone.
  started_at  TIMESTAMPTZ NOT NULL,

  -- The digest that reported this episode, or NULL for "examined and found quiet".
  -- ⛔ NO FK HERE EITHER, AND THE REASON IS THAT NEITHER ACTION IS HONEST: ON DELETE
  -- SET NULL would silently rewrite "reported in digest X" into "examined and found
  -- quiet", which is a different claim, and CASCADE would delete the receipt and
  -- manufacture a phantom gap. The Down's integrity check is what compensates.
  reported_in UUID,

  -- ⚠️ NO `DEFAULT now()` -- see the note on `notification_digest_coverage.updated_at`
  -- above. `Mark` supplies this explicitly from the tick's own clock reading, the same
  -- instant the digest's span is computed from; letting the database stamp it instead
  -- would put a mark and the digest that produced it on two different clocks.
  marked_at   TIMESTAMPTZ NOT NULL,

  CONSTRAINT digest_case_pk PRIMARY KEY (org_id, policy_id, case_id)
);

-- +goose StatementBegin
COMMENT ON TABLE notification_digest_cases IS
  'One row per (policy, Case) a digest policy MATCHED: "this policy has accounted for this episode". It is the dedupe state that makes a bounded lookback safe -- a digest reads [window_start - DigestLookback, window_end) so that a Case whose transaction committed after the previous tick read its window is still counted (git-bug a8a4010), and this table is what stops the tail from being re-reported in every message. reported_in NOT NULL means "in that digest"; NULL means "examined, and the window did not clear the floor" -- the sentence the old below-floor path could not write, whose absence made "quiet" and "never examined" identical (git-bug 893cee4). ⭐ AND THE ABSENCE OF A ROW for a matched Case older than the lookback IS THE UNRECOVERABLE GAP: DigestService.ReconcileOrg folds Policy.Matches over the candidate span and counts exactly those, which is the alertable number that replaced the skipped_windows backlog. Write-once (ON CONFLICT DO NOTHING) and bounded by domain.DigestMarkRetention.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_digest_cases.reported_in IS
  'The digest notification that reported this episode, or NULL for "examined and found quiet" -- the window it fell in did not clear the policy''s floor. BOTH ARE MARKS AND ONLY ONE IS A REPORT, and the distinction is what the reconciler reads. It is write-once: a later, wider window must not be able to upgrade a NULL into a report, because that would make whether an episode was reported depend on when somebody edited digest_window_s, and the pre-00070 design reached the same conclusion by re-querying a closed window under a comment observing that its answer cannot have changed. ⚠️ No FK: ON DELETE SET NULL would rewrite "reported" into "quiet", which is a different claim, and CASCADE would manufacture a phantom gap.';
-- +goose StatementEnd

-- ⭐ ONE INDEX FOR BOTH READERS, AND THE COLUMN ORDER IS THE TRADE. The tail
-- subtraction asks (org_id, policy_id, started_at range) and the retention sweep asks
-- (org_id, started_at) across every policy at once. Leading with `policy_id` would
-- serve the first marginally better and would need a SECOND index for the second; this
-- order serves the sweep on its two-column prefix and serves the subtraction as an
-- index-only scan with `policy_id` filtered inside the index, no heap page touched. A
-- tenant has a handful of digest policies, not thousands, so the filter is cheaper than
-- a second index is to maintain on every mark written.
--
-- `org_id` leads, as it does on every composite index in this schema.
CREATE INDEX digest_case_span_idx
  ON notification_digest_cases (org_id, started_at, policy_id);

-- +goose StatementBegin
COMMENT ON INDEX digest_case_span_idx IS
  'Serves both readers of the mark table from one index. The tail subtraction (org_id, policy_id, started_at range) rides it as an index-only scan with policy_id filtered in-index; the retention sweep (org_id, started_at) rides its two-column prefix. Leading with policy_id would need a second index for the sweep, and a tenant has a handful of digest policies rather than thousands, so the in-index filter is cheaper than maintaining that index on every mark written.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ INTEGRITY CHECK ONE: A MARK MUST NAME A DIGEST OF ITS OWN POLICY. `reported_in`
-- carries no FK on purpose (see its COMMENT: both referential actions would rewrite the
-- claim), so this is where the missing FK is paid for. A row that names a notification
-- which is not a digest, or is a digest of a DIFFERENT policy, cannot have been written
-- by `DigestRepository.Mark` -- it passes `stored.ID` from the digest it just inserted
-- for that policy, inside the same transaction. So the table has been changed by
-- something other than oto, and this Down is about to destroy the only record of that.
-- It refuses, and names the repair.
--
-- ⚠️ A DANGLING `reported_in` -- one naming no row at all -- IS NOT AN ERROR. Digest
-- notifications are subject to retention like everything else in that table, and a mark
-- outliving the message it points at is the expected consequence of two independent
-- horizons, not corruption. The check therefore looks only at rows whose target EXISTS
-- and disagrees.
-- +goose StatementBegin
DO $$
DECLARE bad BIGINT;
BEGIN
  SELECT count(*) INTO bad
    FROM notification_digest_cases m
    JOIN notifications n ON n.id = m.reported_in AND n.org_id = m.org_id
   WHERE n.subject_kind <> 'digest'
      OR n.policy_id IS DISTINCT FROM m.policy_id;

  IF bad > 0 THEN
    RAISE EXCEPTION
      'migration 00070 down: % digest mark(s) name a notification that is not a digest of the same policy. DigestRepository.Mark writes the id of the digest it inserted for that policy in the same transaction, so it cannot produce this -- the table has been changed by something other than oto, and this rollback would drop the only record of it. Inspect them with:  SELECT m.* FROM notification_digest_cases m JOIN notifications n ON n.id = m.reported_in AND n.org_id = m.org_id WHERE n.subject_kind <> ''digest'' OR n.policy_id IS DISTINCT FROM m.policy_id;  then decide whether the marks or the notifications are wrong. Do not delete the rows to get past this without reading them first.',
      bad;
  END IF;
END $$;
-- +goose StatementEnd

-- ⛔ INTEGRITY CHECK TWO: THE COVERAGE SPAN MUST STILL SATISFY ITS OWN CHECK. The
-- constraint is about to be dropped, so this is the last moment anything can observe
-- whether it held. A violating row means `notifications_digcover_ck` was disabled,
-- dropped or bypassed -- and a digest row whose stored span does not contain its own
-- window is a card that renders a span it did not cover, which is a visible lie of
-- exactly the kind §B.6 exists to prevent. Finding one AFTER the columns are gone is
-- impossible, so it is found here.
-- +goose StatementBegin
DO $$
DECLARE bad BIGINT;
BEGIN
  SELECT count(*) INTO bad
    FROM notifications
   WHERE digest_covered_from IS NOT NULL
     AND (digest_covered_to <= digest_covered_from
       OR digest_window_start IS NULL
       OR digest_covered_from >  digest_window_start
       OR digest_covered_to   <= digest_window_start);

  IF bad > 0 THEN
    RAISE EXCEPTION
      'migration 00070 down: % digest row(s) carry a coverage span that does not contain their own window, which notifications_digcover_ck forbids. The constraint has been disabled, dropped or bypassed, and every such row is a card that would render a span it did not cover. Inspect them with:  SELECT id, org_id, policy_id, digest_window_start, digest_covered_from, digest_covered_to FROM notifications WHERE digest_covered_from IS NOT NULL AND (digest_covered_to <= digest_covered_from OR digest_window_start IS NULL OR digest_covered_from > digest_window_start OR digest_covered_to <= digest_window_start);  then repair or NULL out the spans before rolling back. Dropping the columns first makes this unfindable.',
      bad;
  END IF;
END $$;
-- +goose StatementEnd

-- ⛔⛔ AND THE DESTRUCTION RAISES RATHER THAN NOTICES, WHICH IS THE THIRD GUARD IN THIS
-- DOWN AND NOT AN EXCEPTION TO THE OTHER TWO. This block used to `RAISE NOTICE` and then
-- drop the table the notice itself calls "the only evidence ReconcileOrg reads --
-- permanently unfindable after this", under the argument that whether an unreported
-- episode is outstanding cannot be computed here (it needs `Policy.Matches` in Go, the
-- same reason the detector is not a query) and so a human must decide. THE ARGUMENT WAS
-- RIGHT AND THE MECHANISM WAS WRONG: a NOTICE is not a decision point. `goose` prints it
-- into a log nobody reads during a rollback, the DROP runs in the same breath, and the
-- operator learns what they destroyed from the past tense. Every other guard across
-- 00070..00073 stops the migration and hands back an inspection query; this one destroys
-- more than any of them and was the only one that did not.
--
-- So it stops, and it names its own escape hatch: `TRUNCATE notification_digest_cases` is
-- one statement, and typing it IS the decision. The spans and the cursors are still only
-- reported, because they are recoverable -- release N re-derives a window-start cursor
-- from the digest rows, which reinstates all three bugs but loses no message -- and an
-- empty mark table has nothing to protect, so a rollback with no evidence at risk is not
-- blocked at all.
-- +goose StatementBegin
DO $$
DECLARE marks BIGINT; quiet BIGINT; cursors BIGINT; spans BIGINT; oldest TIMESTAMPTZ;
BEGIN
  SELECT count(*), count(*) FILTER (WHERE reported_in IS NULL), min(started_at)
    INTO marks, quiet, oldest FROM notification_digest_cases;
  SELECT count(*) INTO cursors FROM notification_digest_coverage;
  SELECT count(*) INTO spans FROM notifications WHERE digest_covered_from IS NOT NULL;

  IF marks > 0 THEN
    RAISE EXCEPTION
      'migration 00070 down: % digest mark(s) are about to be destroyed (% of them "examined and found quiet", oldest episode %), and they are the ONLY evidence DigestService.ReconcileOrg reads -- any episode a digest policy matched and never reported becomes permanently unfindable once this table is dropped, and whether one is outstanding cannot be computed in SQL (it needs Policy.Matches in Go, which is why the detector is not a query). Run the reconciler once and record its output, or read the marks with:  SELECT policy_id, case_id, started_at, reported_in FROM notification_digest_cases ORDER BY started_at;  then TRUNCATE notification_digest_cases to say you have decided, and re-run the rollback. The % policy coverage cursor(s) and the stored span on % digest row(s) are NOT the reason this stopped: those are recoverable, because release N re-derives a window-start cursor from the digest rows themselves.',
      marks, quiet, oldest, cursors, spans;
  END IF;

  RAISE NOTICE 'migration 00070 down: destroying % policy coverage cursor(s) and the stored span on % digest row(s), with no digest marks outstanding. Both are recoverable -- release N re-derives a window-start cursor from the digest rows themselves -- which is why this reports rather than stops.',
    cursors, spans;
END $$;
-- +goose StatementEnd

DROP INDEX IF EXISTS digest_case_span_idx;
DROP TABLE IF EXISTS notification_digest_cases;
DROP TABLE IF EXISTS notification_digest_coverage;

ALTER TABLE notifications DROP CONSTRAINT notifications_digcover_ck;
ALTER TABLE notifications DROP COLUMN digest_covered_to;
ALTER TABLE notifications DROP COLUMN digest_covered_from;
