-- git-bug bd0fb1d. The owner withdrew the unacked reminder: oto sends nothing
-- unprompted. The sweep, its worker, its job kind, its policy column and its five
-- settings keys go in the same change; this file is the vocabulary half.
--
-- ⛔⛔ DELETED, NOT RETIRED, AND THE OWNER RULED THAT SECOND. The first pass kept
-- `unacked_reminder` readable-but-unwritable on the `00051`/`00054` bargain, because
-- `notifications.reason` has a thirteen-month horizon and rows spelling it would
-- have become undecodable. The owner then settled the premise that argument rested
-- on: **oto is UNRELEASED and this database is being reset.** There is no history to
-- keep readable. `EventType`'s own header states the test — "with no row left to
-- read back, a value kept for a reader that cannot exist is not caution, it is a
-- vocabulary entry the next person has to rule out" — and it comes out the other way
-- here. No ghost references at launch.
--
-- ⛔⛔ EXPAND/CONTRACT IS SPENT AGAIN, AND THE FACTS ARE RE-VERIFIED RATHER THAN
-- CITED. §D's rule says narrow in two releases. `00059` and `00060` each took the
-- exemption and each wrote that its header IS NOT A PRECEDENT for the next
-- narrowing, so: `git tag` lists NOTHING. There is no release N for a contracted
-- schema to be incompatible with, and nobody can have pulled a build even by
-- accident. THE MOMENT A TAGGED RELEASE EXISTS THIS EXEMPTION IS SPENT and the next
-- removal of a CHECK value goes back to expand/contract.
--
-- ⚠️ IT FAILS LOUDLY AND REWRITES NO HISTORY. This is `00018`'s rule, `00059`'s and
-- `00060`'s, verbatim: an enum narrowing with NO DOWNLEVEL MAPPING must FAIL rather
-- than invent one. There is no honest value to turn a stored `unacked_reminder`
-- notification into — it was a real fact, sent to a real channel — so there is
-- deliberately NO `UPDATE` against `notifications` below. `ADD CONSTRAINT` validates
-- the existing rows, so a database that has ever recorded one refuses this migration
-- with a 23514 naming the constraint, and the answer is the reset the owner has
-- authorised.

-- +goose Up

-- ------------------------------------------------- notifications_reason_ck
--
-- The seventeen that remain, in the order 00018 declares them, with `digest`
-- appended by 00058, `storm` removed by 00060 and `unacked_reminder` removed here.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','digest'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, seventeen values. Together with the channel verbosity it decides update-in-place versus thread reply. The storm announcement went with the damper it announced (ADR 0042). The unacked reminder went because oto sends nothing unprompted (git-bug bd0fb1d): there is no fact here that nobody asked for.';
-- +goose StatementEnd

-- --------------------------------------------------- policies_reasons_ck
--
-- The ceiling is the enum size and moves with it in both directions: 18 at 00046,
-- 19 when 00058 added `digest`, 18 when 00060 removed `storm`, 17 now. `reasons` is
-- a SET (oto_array_is_set) over a closed seventeen-value vocabulary, so an
-- eighteenth DISTINCT element is unreachable and a ceiling of 18 would be a number
-- no row could ever test. It has never been a number anybody chose: it is
-- `len(AllReasons())`.
--
-- ⚠️ THE STRIP IS NOT OPTIONAL AND 00060 SAYS SO IN THE SCHEMA ITSELF.
-- `policies_reasons_ck`'s own comment reads: "a retired Reason left in this array is
-- invisible to the database ... Every narrowing of the Reason vocabulary must strip
-- the retired value from this column by hand, the way 00060 does -- the constraint
-- will not do it for you." This is that hand. CONFIG IS NOT HISTORY: a policy naming
-- a fact oto no longer produces is a dead subscription, which is the defect
-- `0457f1f`, `35d4248`, `39e48e2` and `27a1860` each closed.
DELETE FROM notification_policies WHERE reasons = ARRAY['unacked_reminder']::text[];

UPDATE notification_policies
   SET reasons = array_remove(reasons, 'unacked_reminder'), updated_at = now()
 WHERE 'unacked_reminder' = ANY(reasons);

-- The strip above already ran, so this ALTER validates against arrays drawn from the
-- seventeen that remain and cannot fail on an eighteen-element policy: the only way
-- to reach eighteen was to name the reminder, and no row names it any more.
ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 17
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..17 SPEC H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: cardinality, NULL-freeness and set-ness are all it tests, so a removed Reason left in this array is invisible to the database and surfaces as a validator failure on the Policies page instead. Every narrowing of the Reason vocabulary must strip the value from this column by hand, the way 00060 and 00067 do -- the constraint will not do it for you.';
-- +goose StatementEnd

-- +goose Down

-- The world as 00060 left it, IN SHAPE ONLY. The widening direction always succeeds:
-- nothing on disk can violate a check that admits strictly more.
--
-- ⛔ THERE IS DELIBERATELY NO MIRROR OF THE POLICY STRIP, for the reason 00060 gave
-- in this same position. The Up removed `unacked_reminder` from every
-- `notification_policies.reasons` array and deleted the policies whose only reason it
-- was. Neither is recoverable: the array no longer records that the value was ever
-- present, so a mirroring UPDATE would have to guess WHICH policies to re-point at
-- the reminder, and adding it back to all of them -- or to none -- would both be
-- inventions. A deleted reminder-only policy is gone with its name, its matchers and
-- its channel list. This half restores only the VOCABULARY the constraints admit.

-- vocab:allow -- the Down must name the value it restores, or it cannot restore it.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','unacked_reminder','digest'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, eighteen values. Together with the channel verbosity it decides update-in-place versus thread reply. The storm announcement is gone with the damper it announced (ADR 0042): oto withholds nothing, so it has nothing to announce withholding.';
-- +goose StatementEnd

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 18
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..18 SPEC H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: cardinality, NULL-freeness and set-ness are all it tests, so a retired Reason left in this array is invisible to the database and surfaces as a validator failure on the Policies page instead. Every narrowing of the Reason vocabulary must strip the retired value from this column by hand, the way 00060 does -- the constraint will not do it for you.';
-- +goose StatementEnd
