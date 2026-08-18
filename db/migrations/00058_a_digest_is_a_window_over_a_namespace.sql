-- SPEC §D.8, §H.6 -- a Notification may be about a WINDOW OVER A NAMESPACE
-- rather than about one alert, one case or one generation. `subject_kind` gains
-- a fourth value, `digest`; `notification_policies` gains the window and the
-- floor; and `notifications.group_id` stops being mandatory, because a digest
-- has no single group to be delivered to.
--
-- ⭐⭐ WHAT WAS WRONG. Every notification oto could send was TRIGGERED BY ONE
-- OBJECT. All eighteen Reasons named a change to a single thing -- an alert
-- fired, a case was acked, a generation's membership moved -- so the question
-- "what happened in the observability namespace in the last ten minutes, and
-- don't bother me unless it was more than a trickle" had NO SUBJECT to hang on.
-- It is not about an alert, a case or a group. It is about a SET SELECTED BY
-- PROPERTIES OVER A WINDOW, and the only way to approximate it was to receive
-- every event and read them as a batch by eye.
--
-- `notification_policies` could say WHICH object's changes reach a channel and
-- could cap the RATE (`throttle`), but it could not express a SCHEDULE, a WINDOW
-- or a FLOOR. `repeat` is the closest thing in the Reason vocabulary and it still
-- hangs off one group.
--
-- ⭐⭐ THE STRUCTURAL PROBLEM WAS `group_id NOT NULL`, AND THIS IS WHERE IT IS
-- SOLVED. Migration 00056 separated the SUBJECT (what the fact is about) from the
-- DELIVERY TARGET (`group_id`, which thread it lands on) and deliberately kept
-- `group_id` NOT NULL, with a written argument: "a Notification with no delivery
-- target is a Notification with nowhere to go, and every writer already supplies
-- one because every producer of a per-alert fact reaches the group through the
-- Case it moved". That argument was exactly right for the eighteen Reasons it was
-- written about, and it named its own limit in the next sentence -- "relaxing NOT
-- NULL is the expand-SAFE direction ... but there is no row that needs it."
--
-- THERE IS NOW A ROW THAT NEEDS IT. A digest spans a namespace over a window and
-- therefore has no single group: not "a group we have not resolved yet" but no
-- group at all, by construction, because the set it reports on is selected by
-- LABELS and TIME and typically spans many generations. So the column is relaxed
-- and the invariant 00056 relied on is RESTATED AS A CONSTRAINT rather than
-- surrendered:
--
--     notifications_target_ck  CHECK (subject_kind = 'digest' OR group_id IS NOT NULL)
--
-- Nothing that could name a group is allowed to stop naming one. The NOT NULL was
-- carrying two meanings at once -- "this column is populated" and "a fact must
-- have a destination" -- and only the second is true of every Notification. A
-- digest's destination is the POLICY's channel list, which is what it always was;
-- what it lacks is a GROUP THREAD to land in, and it opens its own conversation
-- instead (see `channel_threads` below).
--
-- ⭐ WIDENING NULLABILITY IS THE EXPAND-SAFE DIRECTION (SPEC §D "expand/contract
-- only", CONTEXT.md §6). It admits rows the previous release could not write and
-- rejects none it could. Release N supplies `group_id` on every write --
-- `alerts/service` applyEdge, `actions.go` Acknowledge/Comment/notifySnoozeChange
-- and `sweep.go` expiry all pass groupID, alertID and caseID together -- so the
-- new constraint is satisfied by every row release N produces, and the two
-- releases can run side by side.
--
-- ⚠️ THE ONE CASCADE THAT DEPENDED ON IT, AND WHY NOTHING LEAKS.
-- `internal/drill/repository/dispose.go` deletes a drill's `alert_groups` row and
-- relies on the `group_id` FK to CASCADE that drill's `notifications` and, through
-- them, its `notification_deliveries`. A NULL `group_id` is not reachable by that
-- cascade, so the question is whether a digest row could be left behind by a
-- drill disposal. It cannot, for two independent reasons:
--
--   1. A DRILL NEVER PRODUCES A DIGEST. `Dispose` is scoped BY ID -- every
--      statement names an id the drill itself recorded -- and a digest is minted
--      by a policy tick, not by a drill's manifest. There is no digest row in a
--      drill's manifest to leak.
--   2. A DIGEST NEVER COUNTS A DRILL. The window count filters
--      `NOT alert_groups.synthetic` (`notification/repository/digest.go`), so a
--      drill's manufactured firing is invisible to every digest. A digest's
--      content therefore cannot outlive the drill it reported on, because it never
--      reported on one.
--
-- What DOES reap a digest is what reaps every other notification: nothing, by
-- design (ADR 0024 -- `notifications` is never reaped) until the tenant is deleted,
-- at which point `notifications.org_id REFERENCES orgs(id) ON DELETE CASCADE`
-- takes it. A digest is retained evidence on exactly the same terms as every other
-- Notification, and this migration introduces no new class of orphan.
--
-- ⭐⭐ THE SUBJECT OF A DIGEST IS THE PAIR (policy_id, digest_window_start), AND
-- `subject_id` CARRIES THE POLICY HALF.
--
-- One UUID column cannot hold a pair, and the two ways to force it to are both
-- worse. Hashing the pair into a synthetic UUID would make `subject_id`
-- unjoinable -- an id that resolves against no table, which is the precise defect
-- 00056 spent a migration removing. Storing the window alone would lose which
-- policy asked. So the pair is spelled as it is everywhere else in this table: the
-- typed column (`policy_id`) plus its agreement with `subject_id`, and the second
-- coordinate on a column of its own (`digest_window_start`). `notifications`
-- ALREADY has this shape -- `group_id`, `alert_id` and `case_id` are the three
-- typed halves 00056 tied `subject_id` to, and this adds a fourth.
--
-- ⚠️ THE DIGEST ARM TOLERATES A NULL `policy_id`, AND IT MUST.
-- `policy_id REFERENCES notification_policies(id) ON DELETE SET NULL` (00011).
-- Requiring `subject_id = policy_id` unconditionally would mean that deleting a
-- policy fires the SET NULL, which then fails this CHECK with a 23514 -- so the
-- first digest ever sent would make its policy undeletable. The arm therefore
-- reads `(policy_id IS NULL OR subject_id = policy_id)`: while the policy exists
-- the tie is enforced, and when it is gone `subject_id` keeps the id it named.
-- That is the same rule 00011 and 00029 already state for `alert_id` and
-- `case_id` -- "a notification is retained evidence and must outlive a purge of
-- what it was about" -- applied to the third typed column.
--
-- ⭐ THE IDEMPOTENCY KEY IS THAT PAIR, DECLARED TWICE, ON PURPOSE.
--
--   - `notif_digest_uniq UNIQUE (org_id, policy_id, digest_window_start)
--     WHERE subject_kind = 'digest'` -- the key a human can read, enforced by an
--     index rather than by trusting a hash. A retried tick, a second pod and a
--     leader election that fires twice all collide here.
--   - `notifications_idem_uniq (org_id, idempotency_key)` -- the §C.7 key, which
--     already covers this without changing shape, because `subject_kind` and
--     `subject_id` are in its pre-image and `state_version` carries the WINDOW
--     ORDINAL (`floor(window_start / window_s)`). §C.7 defines `state_version` as
--     "the version of the subject this intent was minted against", and for a
--     subject that IS a window the ordinal is exactly that version. It satisfies
--     `notifications_sver_ck (>= 1)` for every window after 1970.
--
-- Two declarations of one key is not redundancy here: the hash is what makes the
-- §C.7 insert path converge without a special case, and the index is what makes
-- the rule true even if the hash pre-image is ever changed.
--
-- ⭐⭐ `channel_threads` IS WIDENED, AND THE THREAD IS KEYED BY THE POLICY, NOT BY
-- THE WINDOW. A digest is ONE ONGOING CONVERSATION PER POLICY PER CHANNEL, one
-- reply per tick -- not a fresh thread every ten minutes, which would be a channel
-- full of one-message threads and exactly the noise a digest exists to replace. So
-- a digest thread is `(channel_id, 'digest', policy_id)` and
-- `threads_subject_uniq UNIQUE (channel_id, subject_kind, subject_id)` needs no
-- change to mean it: it already says "exactly one conversation per subject", and
-- the digest's conversation subject is the policy. The per-window identity lives on
-- the NOTIFICATION, where the idempotency key needs it; the per-policy identity
-- lives on the THREAD, where the conversation needs it. Note that the two
-- `subject_id`s therefore differ in what they resolve against for a digest --
-- `notification_policies` in both cases here, but the thread's is the CONVERSATION
-- key and the notification's is the FACT key, and 00056's separation of the two is
-- what makes that sayable at all.
--
-- ⚠️ THE THROTTLE AND `notif_group_idx` ARE UNAFFECTED, AND THAT WAS CHECKED.
-- `countRecentSQL` (notification/repository/notifications.go) counts
-- `WHERE org_id = $1 AND group_id = $2`; `$2` is never NULL, and `NULL = $2`
-- evaluates to NULL, so a digest row is EXCLUDED from every group's throttle
-- numerator rather than silently inflating it. That is the correct answer: the
-- throttle asks "how much has oto already said into THIS GROUP'S THREAD", and a
-- digest was said into a different thread entirely. A digest cannot escape a rate
-- limit by that exclusion either, because its rate limit is the WINDOW: at most one
-- digest per policy per window, enforced by `notif_digest_uniq` above.
-- `notif_group_idx (org_id, group_id, created_at DESC)` indexes NULLs like any
-- b-tree and never matches an equality probe against them, so it neither grows a
-- correctness hole nor stops serving the query it was added for in 00056.
--
-- ⚠️ THERE IS DELIBERATELY NO reason -> subject_kind CHECK, STILL. 00056 refused
-- one and gave the reason: which subject each Reason declares is the ALLOCATION,
-- and it lives in `internal/notification/domain/reason.go` where a test proves it
-- total. The argument that made it unsafe there -- release N writes `alert_group`
-- for every reason -- does not apply to `digest`, since release N cannot write that
-- value at all; but the argument that makes it UNNECESSARY does apply, and adding
-- the CHECK for one arm of a rule the domain owns entirely would split the
-- allocation across two files. The database constrains the SHAPE of a subject; the
-- domain decides WHICH subject a Reason has.
--
-- ⭐ THE WINDOW IS ALIGNED TO THE WALL CLOCK, NOT TO THE POLICY, AND THE CHECK
-- ENFORCES IT. `86400 % digest_window_s = 0` means every admissible window length
-- divides the UTC day, so every boundary is a wall-clock boundary, no window
-- straddles midnight, and `window_start = floor(unix / window_s) * window_s` is
-- derivable from the clock alone in every pod with no stored state. Aligning to
-- `created_at` instead would make the boundary a function of a configuration row:
-- two ten-minute policies would report on two different ten minutes, digests would
-- arrive at ragged times, and recreating a policy would shift every future boundary
-- and could re-open a window already covered. The evaluator's whole claim -- that
-- it needs "no durable state beyond which window was last covered" -- rests on
-- `window_start` being computable rather than remembered.
--
-- ⭐ THE FLOOR COUNTS CASES OPENED IN THE WINDOW. Not alerts and not
-- notifications, and both alternatives are wrong rather than merely coarser. An
-- Alert is an IDENTITY that outlives its firings, so counting alerts answers "how
-- many distinct things are broken", which says nothing about a ten-minute window --
-- something that has been firing all week would be counted forever. Counting
-- NOTIFICATIONS is circular: it counts oto's own chatter, so a throttled or quiet
-- channel would lower the count, and a digest exists precisely for the case where
-- the individual notifications were NOT sent. A Case is ONE FIRING EPISODE with a
-- `started_at`, so "what happened in this namespace in this window" is exactly the
-- episodes that OPENED inside it -- a count of real upstream events, independent of
-- every delivery decision oto made. `case_started_idx (org_id, started_at, id)`
-- (00053) serves it.
--
-- ⭐ A POLICY WITHOUT A WINDOW BEHAVES EXACTLY AS TODAY. Both columns are NULL by
-- default and NULL means "no digest". Alert-based and case-based policies gain no
-- schedule and no floor and fire immediately, every time: their noise is a signal
-- to fix the Prometheus rule, and oto does not decide to be quiet about a firing.
--
-- ⭐ `policies_digest_reason_ck` BINDS THE WINDOW TO THE REASON IN ONE DIRECTION.
-- A policy that carries a window MUST list `digest` in `reasons`, because a
-- schedule whose facts nothing routes is a schedule that produces suppressed rows
-- forever (`no_policy`) -- the same coherence `reminder.go` checks by hand for
-- `unacked_reminder`. The converse is NOT required: `digest` may appear in
-- `reasons` with no window, which is inert exactly like `refired`, a value nothing
-- produces (ADR 0040). Requiring both directions would outlaw the whole-vocabulary
-- policy that `policies_reasons_ck` exists to keep legal.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). This is an EXPAND step. Two CHECKs are
-- WIDENED (`notifications_subjkind_ck`, `notifications_reason_ck`,
-- `threads_subjkind_ck`, `policies_reasons_ck`), one NOT NULL is DROPPED, four
-- columns are ADDED nullable, and every new constraint is satisfied by every row
-- release N can write: no row has a `digest` subject, every row has a `group_id`,
-- and no policy has a window. It is safe to deploy under release N.
--
-- ⛔⛔ THE DOWN DELETES DIGEST ROWS, AND IT IS THE ONLY HONEST OPTION. Restoring
-- `group_id NOT NULL` requires that no NULL remain, and a digest CANNOT be
-- normalised the way 00056 normalised a case-subject row back onto its group: there
-- is no group to normalise it to. The argument for the deletion is narrow and must
-- stay narrow -- a digest is a DERIVED SUMMARY. Every case it counted still exists,
-- untouched, in `alert_cases`; the count is recomputable by re-running the tick; and
-- unlike an alert notification the row is not the only record of anything. The
-- deletes are scoped by `subject_kind = 'digest'` and `'digest' = ANY(reasons)`,
-- values only this migration can create, so a rollback cannot reach a row that
-- predates it. A digest-only POLICY goes too, for the same reason and with more
-- regret: it exists solely to express a thing the pre-00058 schema has no
-- vocabulary for, and projecting it onto the old one would mean inventing a reason
-- the operator never asked for.

-- +goose Up

-- ------------------------------------------------------ notification_policies

-- The window and the floor. NULL for both is "no digest", which is what every
-- existing row says and what every policy created by release N will say.
ALTER TABLE notification_policies ADD COLUMN digest_window_s INT;
ALTER TABLE notification_policies ADD COLUMN digest_floor    INT;

-- 300..86400 AND a divisor of the day. The lower bound is five minutes because a
-- digest shorter than that is the per-event stream it replaces, wearing a delay;
-- the divisor rule is what makes every boundary a wall-clock boundary (see the
-- header) and is therefore load-bearing rather than tidy.
ALTER TABLE notification_policies ADD CONSTRAINT policies_digest_window_ck
  CHECK (digest_window_s IS NULL
         OR (digest_window_s BETWEEN 300 AND 86400 AND 86400 % digest_window_s = 0));

-- A floor of 1 means "send if anything at all happened", which is the weakest
-- useful floor and therefore the bottom of the range. Zero is not admitted: it
-- would mean "send for an empty window", and an empty window sends nothing by
-- construction, so the value would describe a behaviour that does not exist.
ALTER TABLE notification_policies ADD CONSTRAINT policies_digest_floor_ck
  CHECK (digest_floor IS NULL OR digest_floor BETWEEN 1 AND 10000);

-- A floor with no window is a threshold nothing evaluates. The reverse is fine:
-- a window with no floor digests whenever the window was not empty.
ALTER TABLE notification_policies ADD CONSTRAINT policies_digest_pair_ck
  CHECK (digest_floor IS NULL OR digest_window_s IS NOT NULL);

-- A schedule whose facts the policy does not route would mint one suppressed
-- `no_policy` row per window, forever. One direction only -- see the header.
ALTER TABLE notification_policies ADD CONSTRAINT policies_digest_reason_ck
  CHECK (digest_window_s IS NULL OR 'digest' = ANY(reasons));

-- The ceiling is the size of the Reason enum, which is now 19. 00046 set it to 18
-- and said why: `reasons` is a SET over a closed vocabulary, so the most a policy
-- can carry is that vocabulary once, and any other number is either unreachable or
-- outlaws a legal policy. The number moves with the enum; the rule does not.
ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 19
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- Serves: the digest tick, which walks the policies that carry a window. It is
-- partial so that it is the size of the digest configuration rather than of the
-- policy table -- most installs will have none.
CREATE INDEX policies_digest_idx ON notification_policies (org_id, priority)
  WHERE digest_window_s IS NOT NULL AND enabled AND deleted_at IS NULL;

-- --------------------------------------------------------------- notifications

-- ⭐ THE RELAXATION, AND THE INVARIANT IT DOES NOT SURRENDER. See the header: the
-- NOT NULL was carrying "a fact must have a destination" as well as "this column
-- is populated", and only the first is true of every Notification.
ALTER TABLE notifications ALTER COLUMN group_id DROP NOT NULL;

ALTER TABLE notifications ADD CONSTRAINT notifications_target_ck
  CHECK (subject_kind = 'digest' OR group_id IS NOT NULL);

-- The window half of the digest's subject, and the count the digest ASSERTS.
--
-- ⭐ THE COUNT IS STORED RATHER THAN RECOMPUTED AT CLAIM TIME, and that is a
-- deliberate exception to C11. C11 renders from the world as it is at claim time
-- because a card describing a state the alert left twenty minutes ago is worse
-- than no card. A digest's window is CLOSED and its content is therefore immutable,
-- so there is no newer truth to render -- but `alert_cases` is reapable (ADR 0024,
-- `case.reap`), so recomputing would silently shrink the number as the episodes it
-- counted aged out. The row would then say a different thing every time it was
-- read, and the message already in Slack would be the only correct copy. Storing it
-- makes the notification self-describing evidence: "oto said N things happened in
-- this window", true forever, on the same terms `alert_id` and `case_id` are kept
-- without an FK.
ALTER TABLE notifications ADD COLUMN digest_window_start TIMESTAMPTZ;
ALTER TABLE notifications ADD COLUMN digest_count        INT;

-- ⚠️ THE FIRST CLAUSE IS AN `IS NOT NULL` COMPARISON AND THE SECOND IS SEPARATE,
-- ON PURPOSE. Folding `digest_count >= 1` into the equality would make the whole
-- predicate NULL for a digest row with a NULL count -- and a CHECK passes on NULL,
-- which is the exact silent hole 00056's `alert_id IS NOT NULL` guards exist to
-- close. Split, the equality can only be true or false, and the range clause is
-- vacuously true when there is nothing to range over.
ALTER TABLE notifications ADD CONSTRAINT notifications_digest_ck
  CHECK ((subject_kind = 'digest') = (digest_window_start IS NOT NULL AND digest_count IS NOT NULL)
         AND (digest_count IS NULL OR digest_count >= 1));

ALTER TABLE notifications DROP CONSTRAINT notifications_subjkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subjkind_ck
  CHECK (subject_kind IN ('alert', 'case', 'alert_group', 'digest'));

-- ⚠️ THE GROUP ARM GAINS `group_id IS NOT NULL`, AND IT IS NOT DECORATION. 00056
-- wrote that arm as a bare `subject_id = group_id` with a comment saying "`group_id`
-- needs no NULL guard: it is NOT NULL and stays that way". That sentence is what
-- this migration falsifies, and without the guard the arm would evaluate to NULL
-- for a NULL `group_id` -- so a row could claim `subject_kind = 'alert_group'` while
-- naming no group at all, which is the same hole 00056 closed for the other two.
ALTER TABLE notifications DROP CONSTRAINT notifications_subject_ck;
ALTER TABLE notifications ADD CONSTRAINT notifications_subject_ck CHECK (
     (subject_kind = 'alert'       AND alert_id IS NOT NULL AND subject_id = alert_id)
  OR (subject_kind = 'case'        AND case_id  IS NOT NULL AND subject_id = case_id)
  OR (subject_kind = 'alert_group' AND group_id IS NOT NULL AND subject_id = group_id)
  OR (subject_kind = 'digest'      AND digest_window_start IS NOT NULL
                                   AND (policy_id IS NULL OR subject_id = policy_id)));

-- The nineteenth Reason. Pure widening: the eighteen 00018 left are all still
-- admitted, in the order 00018 declares them, and `digest` is appended.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','unacked_reminder','storm','digest'));

-- The readable idempotency key: one digest per policy per window per tenant. A
-- retried tick, a second pod and a leader election that fires twice all land here.
-- It is also the index the "last window covered" cursor rides -- `max(window_start)`
-- for a policy is a backwards walk of one entry.
CREATE UNIQUE INDEX notif_digest_uniq
  ON notifications (org_id, policy_id, digest_window_start)
  WHERE subject_kind = 'digest';

-- ------------------------------------------------------------- channel_threads

-- A digest opens its OWN conversation, keyed by the POLICY, so that a policy's
-- digests are one ongoing thread with one reply per tick rather than a new thread
-- per window. See the header for why the thread's subject and the notification's
-- subject differ here.
ALTER TABLE channel_threads DROP CONSTRAINT threads_subjkind_ck;
ALTER TABLE channel_threads ADD  CONSTRAINT threads_subjkind_ck
  CHECK (subject_kind IN ('alert', 'case', 'alert_group', 'digest'));

-- ------------------------------------------------------------------- the words

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.digest_window_s IS
  'The DIGEST WINDOW in seconds: 300..86400 and a divisor of 86400, so every boundary is a wall-clock boundary in UTC and no window straddles midnight. NULL -- the default -- means this policy sends no digest and behaves exactly as it did before migration 00058. Windows are aligned to the epoch (floor(unix / digest_window_s)), never to the policy''s created_at: the boundary must be computable from the clock alone, or the tick would need durable state it deliberately does not have.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.digest_floor IS
  'The FLOOR: a digest is sent only if at least this many Cases OPENED inside the window. 1..10000, NULL means no floor (send whenever the window was not empty). It counts CASES -- one firing episode each -- not alerts, which are identities that outlive their firings, and not notifications, which would be a count of oto''s own chatter and would fall when a channel was throttled. Requires digest_window_s (policies_digest_pair_ck).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_digest_window_ck ON notification_policies IS
  'The window is 300..86400 seconds AND divides the UTC day. The divisor rule is what makes epoch alignment and wall-clock alignment the same thing, which is what lets the tick derive window_start from the clock instead of remembering it.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_digest_reason_ck ON notification_policies IS
  'A policy carrying a window must list `digest` in `reasons`, or its digests would be minted and immediately suppressed as no_policy, once per window, forever. One direction only: `digest` without a window is inert, like `refired`.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.subject_kind IS
  'WHAT this fact is about: alert | case | alert_group | digest. It selects which id column subject_id must agree with (notifications_subject_ck), and it is hashed into idempotency_key. `digest` is the one value whose subject is not a row in the signal graph at all: it is a WINDOW OVER A NAMESPACE, spelled as the pair (policy_id, digest_window_start). Which Reason declares which subject is the domain allocation (internal/notification/domain/reason.go), deliberately NOT a CHECK here.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.subject_id IS
  'The row this fact is about, in the table subject_kind names: alerts.id, alert_cases.id, alert_groups.id, or -- for a digest -- notification_policies.id, which is the policy half of the (policy, window) pair. No FK, because one column cannot reference four tables; notifications_subject_ck ties it to alert_id, case_id, group_id or policy_id instead.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.group_id IS
  'THE DELIVERY TARGET, not the subject: which AlertGroup generation thread this fact lands on. MANDATORY for every subject_kind except `digest` (notifications_target_ck) -- a fact that could name a destination is not allowed to omit one. It is NULL exactly for a digest, which spans many generations and therefore has no single thread to land in; a digest opens its own conversation, keyed by its policy. Ask subject_kind and subject_id what the fact is ABOUT.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.digest_window_start IS
  'The window half of a digest''s subject: the INCLUSIVE start of the closed window this digest reports on, aligned to floor(unix / policy.digest_window_s) in UTC. Present exactly when subject_kind = digest (notifications_digest_ck). With policy_id it is the idempotency key (notif_digest_uniq) and the "last window covered" cursor.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.digest_count IS
  'How many Cases OPENED inside the window -- the number this digest asserts, and the number the floor was compared against. Present exactly when subject_kind = digest. STORED rather than recomputed at claim time, which is a deliberate exception to C11: the window is closed so there is no newer truth, but alert_cases is reapable, so a recomputed count would shrink over time and the row would say a different thing every time it was read.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.state_version IS
  'The version of the SUBJECT this intent was minted against, hashed into idempotency_key (§C.7). For the three signal subjects that is alert_groups.state_version. For a digest the subject is a window, and its version is the WINDOW ORDINAL -- floor(window_start / digest_window_s) -- which is what makes the §C.7 key distinguish one window from the next without changing its shape.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_target_ck ON notifications IS
  'Everything except a digest must name its delivery target. It is the half of the old group_id NOT NULL that is true of every Notification, kept as a constraint when the column was relaxed so that relaxing it could not quietly let an ordinary fact lose its destination.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_digest_ck ON notifications IS
  'digest_window_start and digest_count are present exactly for a digest and absent for everything else, and a stored count is at least 1. The range test is a separate conjunct because folding it into the equality would make the predicate NULL for a missing count, and a CHECK passes on NULL.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_subject_ck ON notifications IS
  'subject_id agrees with the id column subject_kind names, and that column is present. It stands in for the foreign key a four-table reference cannot have. The digest arm tolerates a NULL policy_id because policy_id is ON DELETE SET NULL: enforcing the tie unconditionally would make the first digest ever sent turn its own policy undeletable.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON INDEX notif_digest_uniq IS
  'One digest per (tenant, policy, window). The readable spelling of the idempotency key notifications_idem_uniq also enforces through the §C.7 hash, and the index the last-window-covered cursor reads backwards.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.reasons IS
  'Which §H.6 Reason values this policy reacts to. A SET: 1..19 entries, no NULLs, no duplicates. 19 is the size of the Reason enum, which is the most a set drawn from it can hold, and it is the same number the DTO tag and domain.MaxPolicyReasons carry.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..19 §H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses. The ceiling is the enum size and moves with it -- migration 00058 added `digest`.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON INDEX policies_digest_idx IS
  'Serves the digest tick: the live policies that carry a window, in priority order. Partial, so it is the size of the digest configuration rather than of the policy table.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_kind IS
  'WHAT this conversation is keyed by: alert | case | alert_group | digest. A signal thread is keyed by the AlertGroup GENERATION, so forty alerts still produce one thread. A DIGEST thread is keyed by the POLICY, so a policy''s digests are one ongoing conversation with one reply per tick rather than a new thread every window -- which is the noise a digest exists to replace.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ THE DIGEST ROWS GO FIRST, AND THEY ARE DELETED RATHER THAN NORMALISED. There
-- is no group to normalise a digest onto -- that absence is the whole feature --
-- and `group_id` cannot be made NOT NULL again while one exists. The deletes are
-- scoped by a value only the Up can create. See the header for the argument.
DROP INDEX IF EXISTS notif_digest_uniq;

-- CASCADEs to notification_deliveries (00011, ON DELETE CASCADE).
DELETE FROM notifications WHERE subject_kind = 'digest';

-- No FK reaches channel_threads from a subject, so the digest conversations are
-- deleted by hand. Unlike 00056 -- which refused to delete a thread because that
-- would destroy oto's memory of Slack (C9) -- there is nothing here to remember:
-- every delivery that used these threads has just been removed with its
-- notification, so the handle points at a conversation oto will never write to
-- again.
DELETE FROM channel_threads WHERE subject_kind = 'digest';

ALTER TABLE notifications DROP CONSTRAINT notifications_digest_ck;
ALTER TABLE notifications DROP CONSTRAINT notifications_target_ck;
ALTER TABLE notifications DROP COLUMN digest_count;
ALTER TABLE notifications DROP COLUMN digest_window_start;
ALTER TABLE notifications ALTER COLUMN group_id SET NOT NULL;

ALTER TABLE notifications DROP CONSTRAINT notifications_subjkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subjkind_ck
  CHECK (subject_kind IN ('alert', 'case', 'alert_group'));

-- Byte-identical to the predicate 00056 shipped, so a rolled-back database is in
-- the state its migration history describes.
ALTER TABLE notifications DROP CONSTRAINT notifications_subject_ck;
ALTER TABLE notifications ADD CONSTRAINT notifications_subject_ck CHECK (
     (subject_kind = 'alert'       AND alert_id IS NOT NULL AND subject_id = alert_id)
  OR (subject_kind = 'case'        AND case_id  IS NOT NULL AND subject_id = case_id)
  OR (subject_kind = 'alert_group' AND subject_id = group_id));

-- The eighteen 00018 left.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','unacked_reminder','storm'));

ALTER TABLE channel_threads DROP CONSTRAINT threads_subjkind_ck;
ALTER TABLE channel_threads ADD  CONSTRAINT threads_subjkind_ck
  CHECK (subject_kind IN ('alert', 'case', 'alert_group'));

-- ⛔ THE POLICY CONSTRAINTS COME OFF BEFORE THE REASON ARRAYS ARE TOUCHED.
-- `policies_digest_reason_ck` requires a windowed policy to list `digest`, so
-- removing the value while the window is still set would fail it.
ALTER TABLE notification_policies DROP CONSTRAINT policies_digest_reason_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_digest_pair_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_digest_floor_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_digest_window_ck;
DROP INDEX IF EXISTS policies_digest_idx;

-- A policy whose ONLY reason is `digest` has no projection onto the pre-00058
-- vocabulary: stripping the value would leave an empty array, which
-- `policies_reasons_ck` refuses, and substituting any other reason would route
-- facts the operator never asked for. It is deleted, with regret, and its
-- notifications are already gone above. `notifications.policy_id` is
-- ON DELETE SET NULL, so nothing else is disturbed.
DELETE FROM notification_policies WHERE reasons = ARRAY['digest']::text[];

UPDATE notification_policies
   SET reasons = array_remove(reasons, 'digest'), updated_at = now()
 WHERE 'digest' = ANY(reasons);

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 18
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

ALTER TABLE notification_policies DROP COLUMN digest_floor;
ALTER TABLE notification_policies DROP COLUMN digest_window_s;

-- The 00056 wording, restored verbatim.
-- +goose StatementBegin
COMMENT ON COLUMN notifications.subject_kind IS
  'WHAT this fact is about: alert | case | alert_group. It selects which id column subject_id must agree with (notifications_subject_ck), and it is hashed into idempotency_key, so the same reason at the same state_version about a Case and about its group are two intents and not a collision. Which Reason declares which subject is the domain allocation (internal/notification/domain/reason.go), deliberately NOT a CHECK here: release N writes alert_group for every reason and both releases run at once.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.subject_id IS
  'The row this fact is about, in the table subject_kind names: alerts.id, alert_cases.id or alert_groups.id. It no longer mirrors group_id. No FK, because one column cannot reference three tables -- notifications_subject_ck ties it to alert_id, case_id or group_id instead, and those columns are the typed halves that carry the integrity.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.group_id IS
  'THE DELIVERY TARGET, not the subject. Which AlertGroup generation thread this fact lands on, which is why it is NOT NULL for every reason including the per-alert and per-case ones: a fact with no destination has nowhere to go. It is the group half of the pair whose fusion this migration undid -- ask subject_kind and subject_id what the fact is ABOUT.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.state_version IS
  'The alert_groups.state_version this intent was minted against. Hashed into idempotency_key.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_subject_ck ON notifications IS
  'subject_id agrees with the id column subject_kind names, and that column is present. It is the CHECK that stands in for the foreign key a three-table reference cannot have, and it is what makes subject_id a usable join key: a reader can resolve it from the row alone instead of knowing by convention that it means the group.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_kind IS
  'WHAT this conversation is keyed by: alert | case | alert_group. v1 opens every thread on the AlertGroup generation, so forty alerts still produce one thread; the column is widened because thread identity is a POLICY decision and was welded to the alert grouping by a one-line CHECK. Widening it is what makes threads_subject_uniq say something -- a kind that could hold one value made its first column a constant.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..18 §H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.reasons IS
  'Which §H.6 Reason values this policy reacts to. A SET: 1..18 entries, no NULLs, no duplicates. 18 is the size of the Reason enum, which is the most a set drawn from it can hold, and it is the same number the DTO tag and domain.MaxPolicyReasons carry.';
-- +goose StatementEnd

-- The digest columns need no comment reset: DROP COLUMN above took their
-- catalogue entries with them.
