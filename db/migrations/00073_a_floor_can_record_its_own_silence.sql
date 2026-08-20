-- git-bug 7570090, stage 6's REMAINING HALF. `count_min` now decides something,
-- and the silence it produces has a name an operator can read.
--
-- ⭐⭐ THIS IS THE MIGRATION 00072 NAMED, AND IT LANDS THE VALUE AND ITS PRODUCER IN
-- ONE CHANGE. 00072's header promised, in one sentence with the file in it: *"the
-- evaluator is one comparison beside the throttle's at
-- `internal/notification/service/notify.go`, where the count query and a
-- `suppressed_reason` for 'below the floor' both belong; that is a separate change
-- with a separate migration widening `notifications_suppmap_ck`."* This is that
-- change. The comparison is in `NotificationService.suppressors`, directly under
-- the throttle's, the count is `NotificationRepository.CountRecentSubjects`, and
-- the reason is the value this file admits.
--
-- ⛔ THE ORDER OF THE TWO MIGRATIONS WAS DELIBERATE AND IS WORTH KEEPING ON THE
-- RECORD, BECAUSE IT IS THE OPPOSITE OF THIS REPOSITORY'S USUAL MISTAKE. 00072
-- refused to widen this CHECK, on the argument that a vocabulary value NOTHING CAN
-- WRITE is a dead enum entry — the next person has to rule it out, the UI has to
-- explain it, and the contract publishes a state the product cannot reach. So the
-- column shipped first and the vocabulary waits for its writer. That is the mirror
-- of the defect the same header warns about (`0457f1f`, `35d4248`, `39e48e2`,
-- `27a1860`, and `refire_grace_s` in 00071): a knob with no reader. Both halves are
-- now present, in the order that never leaves an unreachable state behind.
--
-- ------------------------------------------------------------------ the value
--
-- SIX BECOMES SEVEN. 00011 declared this domain, 00018 widened it to eight, 00059
-- narrowed it to six by deleting `storm` and `flapping`, and this adds
-- `below_threshold`: the policy's count condition is not met yet, so oto has
-- nothing to say about this fact FOR NOW.
--
-- ⛔⛔ IT IS NOT `flapping` COMING BACK UNDER A NEW SPELLING, AND THAT IS THE TEST
-- THIS VALUE HAD TO PASS BEFORE IT COULD BE ADDED. 00059's whole argument is that
-- `storm` and `flapping` were the only two values in this domain that were OTO'S
-- OWN OPINION that a real firing was not worth mentioning — the one suppression an
-- operator cannot tell apart from a signal that never fired. Every value that
-- survived is the absence of a destination, a human asking for less, or nothing to
-- say. `below_threshold` produces a silence with the same SHAPE as `flapping`'s
-- ("this keeps happening, say nothing yet") and it is admissible anyway, for one
-- reason: THE NUMBER HAS A DIFFERENT AUTHOR. `flapping` compared against
-- `DefaultFlapThreshold = 5` over `DefaultFlapWindow = 7200 s`, two constants
-- welded into Go that no operator could see, change or switch off.
-- `below_threshold` compares against `notification_policies.count_min` over
-- `count_window_s` — two columns an operator wrote, can read back through the API,
-- and can clear with one PATCH. A damper whose threshold is the operator's is their
-- request; a damper whose threshold is ours is our judgement. git-bug 7570090 names
-- exactly this substitution: the count condition is *"the correct replacement --
-- operator-configurable per alertname instead of a hardcoded threshold of 5 over
-- 7200 s"*.
--
-- ⭐ AND IT IS RECORDED RATHER THAN SILENT, WHICH IS THE OTHER HALF OF WHY IT MAY
-- EXIST AT ALL. A fact refused by the floor gets a `notifications` row, a reason, a
-- timeline event and a place in the audit list, exactly as a throttled one does
-- (SPEC §B.6). The alternative implementation — return early, write nothing, and
-- let the fact vanish until the count happens to clear — is the silent suppression
-- this table's whole design refuses.
--
-- ------------------------------------------------------------- the precedence
--
-- THIS MIGRATION PROPOSES THAT IT SIT DIRECTLY BELOW `throttled` AND ABOVE
-- `verbosity`, which would make the chain read: channel_disabled, no_policy, snoozed,
-- throttled, below_threshold, verbosity, duplicate_render.
--
-- ✅ THAT WAS A PROPOSAL AND NOT A CITATION WHEN THIS FILE SHIPPED, AND IT IS NOW
-- RATIFIED (owner, 2026-08-20, ADR 0044 §5). The difference is recorded because THIS
-- FILE FIRST CLAIMED THE SECOND. SPEC §B.8.2 as first written fixed SIX ranks --
-- channel_disabled, no_policy, snoozed, throttled, verbosity, duplicate_render -- and
-- does not contain `below_threshold`, because the value did not exist when it was
-- written. The sentence that used to stand here said "so §B.8.2 reads" and then called
-- its own two neighbours "the ruling", which was this migration citing itself as the
-- authority for its own choice -- a seventh rank nothing had ratified. ADR 0044 §5 is
-- now that authority, and it is the one to read: the rank below is a RULING, but it
-- became one by being ruled on, not by this file asserting it. The other six ranks are
-- unchanged and are the spec's.
--
-- The argument for the proposed rank, which is all it rests on: `throttled` and
-- `below_threshold` are the SAME
-- TWO POLICY COLUMNS READ WITH OPPOSITE SENSES — a ceiling and a floor over a
-- sliding window — so they belong adjacent, and both belong above `verbosity`,
-- which is a property of a DESTINATION rather than of the policy that routed the
-- fact. The ceiling is ranked first of the two because a spent cap is the ACTIVE
-- fact: oto has been speaking about this conversation and stopped, against a number
-- the operator has already been hit by. An unmet floor is the ordinary RESTING
-- state of every policy that carries one — for most of any window a count condition
-- is unmet by design — and a resting state that outranked an active damper would
-- mask it on every policy carrying both. That is the same argument that already
-- puts `verbosity` below `throttled`.
--
-- --------------------------------------------------------- what is counted
--
-- Written into the column comment as well as here, because an operator reading
-- `\d+ notifications` at 03:00 gets the schema and not this file. The evaluator
-- counts DISTINCT `subject_id`s of the ONE `subject_kind` the policy binds
-- (`policies_count_subject_ck`), scoped to that POLICY, over the sliding window
-- `[now - count_window_s, now]`, INCLUDING rows this very floor suppressed, plus
-- the subject of the fact being evaluated.
--
-- Three of those clauses are choices and each has a failure mode on the other side:
--
--   * DISTINCT SUBJECTS RATHER THAN ROWS, because the unit is what `subject_kinds`
--     exists to declare. Counting rows would clear a threshold of five on ONE Case
--     that was acked, enriched, expired and refired — five facts about one episode,
--     which is not "five of these happened". `digest_floor` counts Cases for the
--     same reason and 00058 carries that argument.
--   * PER POLICY RATHER THAN PER CONVERSATION, because the conversation is usually
--     the thing being counted. "PodRestart opened 5 Cases in an hour" is five Cases
--     and therefore five conversations; a per-conversation count reads 1 on each of
--     them and the condition could never be met by the facts it describes.
--   * SUPPRESSED ROWS INCLUDED, which is the exact opposite of the throttle's
--     numerator and is forced rather than chosen. The throttle excludes them so a
--     cap cannot count its own suppressions and become a permanent mute. Exclude
--     them from a FLOOR and the same failure arrives from the other side: every
--     fact below the threshold is suppressed BY the threshold, so the count would
--     sit at zero forever and the policy would never speak at all. A suppressed row
--     is oto's record that the fact HAPPENED, which is what this count asks;
--     whether oto spoke is the throttle's question.
--
-- ⚠️ THE DIGEST PATH IS NOT GATED BY IT, and that is stated because a reader will
-- otherwise assume the binding's third value behaves like its first two. A digest
-- is minted by the tick against `digest_window_s`/`digest_floor` — its own floor
-- over its own tiled window — so a policy declaring `subject_kinds = {digest}`
-- together with a count condition has that condition read by nothing. Policies
-- bound to `alert` or to `case`, which is what the ticket's example asks for, are
-- decided on the evaluation path this migration serves.
--
-- ------------------------------------------------------------------ the index
--
-- ⚠️ AN INDEX IS ADDED HERE AND 00072 DECLINED ONE, AND THE DIFFERENCE IS NOT A
-- CHANGE OF MIND. 00072's header says: *"Nothing searches for a count condition or
-- for a binding: both are read off the policy row that `policies_eval_idx` already
-- returned."* That was true of the COLUMNS and it stops being true the moment there
-- is a QUERY. This migration adds one, on the evaluation path, once per fact for
-- every policy that carries a condition — and `notifications.policy_id` has had no
-- index since 00011, only an FK, which creates none on the referencing side. Two
-- equalities and the whole range on `created_at DESC`, so the window stops the scan
-- rather than the org's day being read and filtered — the shape 00069 gave the
-- throttle when it replaced `notif_group_idx` with `notif_conversation_idx`.
--
-- `subject_kind` is deliberately NOT in the index. A policy carrying a count
-- condition binds exactly one kind, so the predicate is a constant for every row
-- the two equalities return, and a column that never narrows anything is the dead
-- weight 00072 refused an index for in the first place.
--
-- ------------------------------------------------------------------- the rules
--
-- EXPAND/CONTRACT (CONTEXT.md §6). This is an EXPAND step in the direction that
-- CANNOT FAIL: `ADD CONSTRAINT` validates every existing row against a predicate
-- admitting strictly MORE than the one it replaces, so no row on disk can violate
-- it and no backfill exists to be needed. It is safe under release N, which never
-- writes the new value — and the reverse is not, which is what the Down's guard
-- below is for.
--
-- R9 / three copies. The vocabulary is written in exactly three places and this is
-- one: the CHECK here, `SuppressedReason` plus `suppressorOrder` in
-- `internal/notification/domain/suppression.go` (the precedence chain lives THERE,
-- and `alerts/domain.SuppressorPrecedence` mirrors it because snooze evaluates the
-- chain without importing the notification domain), and
-- `NotificationSuppressedReason` in `api/openapi/openapi.yaml`. The `maxItems: 6`
-- on the `suppressed_reason` query filter is a fourth spelling of the vocabulary's
-- SIZE and moves with it by rule, exactly as `MaxPolicyReasons` does: it becomes 7
-- here.

-- +goose Up

-- -------------------------------------------------- notifications_suppmap_ck
--
-- The constraint KEEPS ITS NAME, as it has across 00018 and 00059. Every reader
-- that names it — `test/integration`'s reversibility walk, this repository's own
-- error messages — keeps working, and the name is the only handle an operator has
-- on a 23514.
ALTER TABLE notifications DROP CONSTRAINT notifications_suppmap_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_suppmap_ck CHECK (suppressed_reason IS NULL OR
  suppressed_reason IN ('channel_disabled','no_policy','snoozed','throttled','below_threshold',
                        'verbosity','duplicate_render'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.suppressed_reason IS
  'Why nothing was sent. Present exactly when status=suppressed. Suppression is RECORDED and shown in the UI; silent suppression destroys trust (SPEC §B.6). When several apply the FIRST MATCH in this fixed order wins: channel_disabled, no_policy, snoozed, throttled, below_threshold, verbosity, duplicate_render. The first four and the last two are SPEC §B.8.2''s; the rank of below_threshold between them is 00073''s own proposal and is NOT YET RATIFIED, because §B.8.2 as written lists six values and not this one. Snooze outranks the automatic dampers because it is a deliberate human act and therefore the most actionable explanation. ⭐ below_threshold (00073) is the policy count condition not being met YET: the evaluator counted the DISTINCT subject_ids of the one subject_kind the policy binds, for that policy, over the sliding window [now - count_window_s, now], counting the fact being evaluated and counting rows this same floor suppressed, and the total was under count_min. It is throttle''s dual -- the same two columns read as a floor instead of a ceiling -- which is why it ranks directly below throttled: a spent cap is an active fact, an unmet floor is the resting state of every policy that carries one. ⛔ It is NOT the flapping damper returning: that compared against constants welded into Go, and this compares against a number the operator wrote and can clear. Every value here is still either the absence of a destination, a human asking for less, or nothing to say -- none is oto own judgement that a real firing was not worth mentioning.';
-- +goose StatementEnd

-- ------------------------------------------------------------ notif_policy_idx
--
-- The count condition's numerator. See the header for why 00072 needed no index
-- and this does.
CREATE INDEX notif_policy_idx ON notifications (org_id, policy_id, created_at DESC);

-- +goose StatementBegin
COMMENT ON INDEX notif_policy_idx IS
  'Serves the policy count condition''s windowed count of distinct subjects (count_min over count_window_s, 00072/00073), which runs on the evaluation path once per fact for every policy carrying a condition. Two equalities -- org_id, policy_id -- then the whole range on created_at DESC, so the window stops the scan instead of the org day being read and filtered; the same shape notif_conversation_idx has for the throttle. notifications.policy_id has carried an FK since 00011 and no index: a foreign key creates none on the referencing side. subject_kind is deliberately absent -- policies_count_subject_ck makes it a constant for every row this index returns, and a column that narrows nothing is dead weight.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ INTEGRITY CHECK: NO ROW MAY STILL SPELL THE VALUE THIS ROLLBACK UNADMITS.
-- Narrowing is the direction that CAN fail, and it must fail LOUDLY rather than
-- rewrite history — 00018's rule, restated by 00059 and 00067. There is no honest
-- value to turn a stored `below_threshold` into: it was a real decision about a
-- real fact, taken against a threshold an operator wrote, and mapping it onto
-- `throttled` would report a ceiling that was never hit while mapping it onto
-- `verbosity` would blame a channel setting that had no say. So nothing is
-- rewritten and nothing is deleted.
--
-- The bare `ADD CONSTRAINT` below would already refuse the rollback with a 23514,
-- which is the correct OUTCOME arriving as an unreadable sentence in the middle of
-- an operator's rollback. This block is the same refusal with the count, the
-- explanation and the query in it.
-- +goose StatementBegin
DO $$
DECLARE bad BIGINT;
BEGIN
  SELECT count(*) INTO bad
    FROM notifications
   WHERE suppressed_reason = 'below_threshold';

  IF bad > 0 THEN
    RAISE EXCEPTION
      'migration 00073 down: % notification row(s) record suppressed_reason = below_threshold, which the six-value domain this rollback restores does not admit. Every one of them is a fact oto deliberately said nothing about because a policy count condition was not met, and there is no other reason that describes it -- throttled would name a ceiling that was never hit, verbosity would blame a channel that had no say. Inspect them with:  SELECT id, org_id, policy_id, subject_kind, subject_id, reason, created_at FROM notifications WHERE suppressed_reason = ''below_threshold'' ORDER BY created_at DESC;  A rollback past this point needs those rows gone, and deleting a recorded suppression deletes the only evidence that oto chose silence -- so decide it deliberately rather than to get the migration to run.',
      bad;
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX notif_policy_idx;

-- The world as 00059 left it: the same six values, in the same order, under the
-- same constraint name.
ALTER TABLE notifications DROP CONSTRAINT notifications_suppmap_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_suppmap_ck CHECK (suppressed_reason IS NULL OR
  suppressed_reason IN ('channel_disabled','no_policy','snoozed','throttled','verbosity',
                        'duplicate_render'));

-- 00059's text, verbatim. A Down that improved the comment on its way past would
-- leave the schema in a state no Up ever produced.
-- +goose StatementBegin
COMMENT ON COLUMN notifications.suppressed_reason IS
  'Why nothing was sent. Present exactly when status=suppressed. Suppression is RECORDED and shown in the UI; silent suppression destroys trust (SPEC §B.6). When several apply the FIRST MATCH in this fixed order wins (SPEC §B.8.2): channel_disabled, no_policy, snoozed, throttled, verbosity, duplicate_render. Snooze outranks the automatic dampers because it is a deliberate human act and therefore the most actionable explanation. The two dampers that WERE oto own opinion about a signal are gone: every value here is either the absence of a destination, a human asking for less, the world rate limit, or nothing to say.';
-- +goose StatementEnd
