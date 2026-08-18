-- The two dampers leave the VOCABULARY, not just the code. `ev_type_ck` (00007)
-- narrows to refuse `group.storm_started`, `group.storm_ended`,
-- `alert.flapping_started` and `alert.flapping_ended`, and
-- `notifications_reason_ck` (00018, re-issued by 00058) narrows from NINETEEN
-- admitted values to EIGHTEEN by dropping `storm`. `policies_reasons_ck` (00058)
-- follows the enum down from 19 to 18, because that ceiling has never been a
-- number anybody chose — it is `len(AllReasons())`.
--
-- ⭐⭐ 00059 SAID `notifications.reason` WAS NOT BEING TOUCHED, AND THIS FILE IS
-- THE CHANGE OF MIND. 00059's header records the asymmetry it chose: narrow
-- `suppressed_reason` because nothing could write it, leave `reason` alone
-- because "a stored row still has to say what it was about and a card still has
-- to render it". That argument is a RETIREMENT argument, and a retirement only
-- buys something when a row spelling the value can still exist. The maintainer
-- has authorised `just reset` on the only database in the world, so no such
-- row exists and no binary that could have written one survives. What the
-- retirement was protecting is a reader that cannot be constructed — and the
-- cost of keeping it is a vocabulary entry in `notification/domain`, in
-- `components.schemas.NotificationReason`, in four verbosity sets, in the Slack
-- reply headings and in the trail glyphs, every one of which the next person has
-- to read and rule out. So `storm` and the four damper event types are DELETED
-- here rather than retired, and the reasoning that argued for keeping them is
-- deleted with them rather than left standing and contradicted.
--
-- ⭐ WHY THE DAMPERS WENT IN THE FIRST PLACE, ONCE, SO THIS FILE IS READABLE ON
-- ITS OWN. Storm collapse (ADR 0042) held a whole generation to one root card and
-- dropped every per-alert reply. It was a defence against a problem WHOSE OBJECT
-- DOES NOT EXIST YET: a storm is many DIFFERENT alerts arriving together, and the
-- thing that owns many different alerts is an INCIDENT (`correlation`,
-- DEFERRED-POST-V1). Built before its object, the defence had nowhere to put what
-- it detected, so it put it in the notification layer — which is how it became
-- damping at all, and a withheld notification is indistinguishable from a signal
-- that never fired (SPEC §B.6). Flap damping went one migration earlier (00057)
-- for a different reason: the case retention window W damps a flap AT CASE
-- FORMATION, so the events `stateChangeCountsSQL` counts fall BELOW
-- `flap_threshold` exactly when an alert is flapping hardest. A detector that
-- lies is worse than no detector.
--
-- ⛔⛔ THIS IS A DESTRUCTIVE MIGRATION IN ONE RELEASE, AND `docs/design/SPEC.md`
-- §D SAYS "Expand/contract only. Never a destructive migration in one release."
-- THE EXEMPTION IS THE ONE 00059 RECORDED, SPENT A SECOND TIME AND ON THE SAME
-- FACTS, NOT A NEW ONE.
--
-- The rule exists because release N and release N+1 run at the same time: a
-- contract that lands before every writer has stopped using the value takes down
-- the release still writing it. That premise is FALSIFIABLE, and it is false
-- here. THE MAINTAINER HAS AUTHORISED A DATABASE RESET, and the repository agrees
-- on its own terms: `git tag` is EMPTY, and `.github/workflows/release.yml`
-- publishes an image only on a `v*.*.*` tag and publishes NO `latest`, so nothing
-- can have pulled a release build even by accident. There is no release N to be
-- compatible with. THE MOMENT A TAGGED RELEASE EXISTS THIS EXEMPTION IS SPENT:
-- the next removal of a CHECK value goes back to expand/contract, and this header
-- is not a precedent for it.
--
-- ⛔ THE NARROWING FAILS LOUDLY AND DOES NOT REWRITE HISTORY. This is 00018's rule
-- and 00059's, verbatim: an enum narrowing with NO DOWNLEVEL MAPPING must FAIL
-- rather than rewrite history. There is no honest value to turn a stored `storm`
-- notification or a `group.storm_started` event into, and inventing one would make
-- a timeline report a transition that never happened. So there is deliberately NO
-- `UPDATE` against `notifications` or `alert_events` below. `ADD CONSTRAINT`
-- validates the existing rows, so a database that has ever recorded one of these
-- values refuses this migration with a 23514 naming the constraint, and the person
-- holding it decides what to do. On a laptop the answer is `just reset`; there
-- is no other holder.
--
-- ⭐⭐ `notification_policies.reasons` IS THE ONE PLACE THAT IS REWRITTEN, AND THE
-- LINE IS HISTORY VERSUS CONFIGURATION. 00018's rule (00018:71-75) is a rule about
-- HISTORY, and it says so: `notifications.reason` and `alert_events.type` are an
-- immutable record of what oto SENT and of what HAPPENED, so there is no honest
-- downlevel value to turn a past fact into and the narrowing must abort rather than
-- invent one. `notification_policies.reasons` is a record of nothing. It is LIVE
-- CONFIGURATION -- a routing list an operator edits through the policies API
-- whenever they like, and which says only WHAT WOULD BE ROUTED IF IT HAPPENED.
-- Removing `storm` from such a list falsifies no claim: the policy never asserted
-- the reason occurred, and after 00059 deleted every writer of it the reason cannot
-- occur. Migrating configuration forward as the vocabulary moves is ordinary work,
-- and the distinction is not a loophole in 00018's rule -- it is the boundary
-- 00018's rule was drawn inside.
--
-- ⛔ AND THE REWRITE IS NOT OPTIONAL, BECAUSE `policies_reasons_ck` CANNOT SEE THE
-- VALUE. That constraint tests CARDINALITY, NULL-freeness and SET-ness (00046) and
-- has NEVER tested MEMBERSHIP -- there is no `reasons <@ ARRAY[...]` clause in it at
-- 00011, at 00046, at 00058 or below. So unlike the two enums above, the narrowing
-- here does not validate on `ADD CONSTRAINT` and would not abort. A policy holding
-- `{fired,storm}` would survive this migration in silence, come back on
-- `GET /api/v1/notification-policies`, and be REFUSED by `NotificationReasonSchema`
-- in the generated frontend client -- a picklist that no longer contains `storm` --
-- taking the whole Policies page down over one stale row. The only thing the
-- constraint WOULD notice is the CEILING, and it would notice it wrongly: a policy
-- naming all nineteen post-00058 values fails `cardinality BETWEEN 1 AND 18`
-- outright, so without the strip this file aborts on data it left no path to clean.
-- The strip therefore runs BEFORE the ceiling moves.
--
-- ⛔ A POLICY WHOSE ONLY REASON WAS `storm` IS DELETED, AND THE ALTERNATIVES ARE
-- WORSE RATHER THAN MERELY DIFFERENT. Stripping the value would leave an EMPTY
-- array, which `policies_reasons_ck` refuses at `cardinality >= 1`. Substituting any
-- other reason would route facts the operator never asked for. Soft-deleting the row
-- (`deleted_at`) does not help either, because a CHECK does not care whether a row is
-- soft-deleted: the array would still have to keep `storm`, in a live column, where
-- no constraint can ever see it again -- which is the exact defect this paragraph is
-- about, hidden one filter deeper. What makes the deletion honest is that the row is
-- ALREADY INERT: 00059 deleted every writer of `storm`, so a policy routing only
-- `storm` routes nothing today and can never route anything, and deleting it removes
-- a rule with no effect rather than a rule somebody is relying on. It is the same
-- judgement 00058's Down made about a `digest`-only policy, for the same reason.
-- `notifications.policy_id` is ON DELETE SET NULL (00011), so no notification is
-- destroyed with it. IT IS STILL A DELETION AND IT IS WRITTEN DOWN HERE SO THAT IT
-- IS NOT SILENT: on a database carrying such a policy the operator loses a named
-- routing rule and gets no prompt, and this header is the notice.
--
-- ⚠️ `ev_type_ck` IS A SHAPE AND STAYS ONE. 00007 wrote it as
-- `type ~ '^[a-z_]+\.[a-z_]+$'` — a shape, not a vocabulary — which is why the
-- four damper types could be kept as parseable history at no schema cost until
-- now. The narrowing adds a `NOT IN` naming exactly the four spellings and
-- changes nothing else: the regex still admits any `<subject>.<fact>` string, so
-- this constraint can refuse a value that LEFT `AllEventTypes` and still cannot
-- notice one that joined it. The live set is enumerated in exactly two places --
-- `AllEventTypes` in `internal/alerts/domain/event.go` and
-- `components.schemas.AlertEventType` -- and `TestContractEnumsMatchTheirDomainEnum`
-- in `test/contract` is what holds the two to each other.
--
-- ⛔ THE THREE REMAINING RETIREMENTS ARE NOT TOUCHED AND MUST NOT BE.
-- `group.member_joined` and `group.member_left` (retired by 00051, when the group
-- key became derived) and `case.reopened` (retired by 00054, when a Case became
-- strictly terminal) stay admitted by `ev_type_ck`, stay in `AllEventTypes` and
-- stay in the published enum. They belong to other decisions; retired means READ
-- BUT NEVER WRITTEN and that is still exactly what they are. This file is about
-- the two dampers and nothing else.
--
-- ⚠️ EIGHTEEN REASONS REMAIN. 00018 narrowed to eighteen, 00058 appended `digest`
-- for nineteen, and dropping `storm` leaves eighteen again: fired, new_alerts,
-- some_resolved, all_resolved, repeat, suppressed, unsuppressed, expired, refired,
-- acked, unacked, snoozed, unsnoozed, enriched, rule_changed, comment,
-- unacked_reminder, digest. `refired` stays: nothing has written it since ADR 0040
-- either, but its retirement was never made mechanical and making it so is that
-- ticket's change rather than this one's.

-- +goose Up

-- ------------------------------------------------------------- alert_events
--
-- ⛔ NO BACKFILL, ON PURPOSE. See the header. `ADD CONSTRAINT` validates every
-- existing row across every partition, and that validation IS the safety check:
-- it fails with 23514 on a database that has ever recorded a damper event rather
-- than rewriting the type into a transition that never happened.
ALTER TABLE alert_events DROP CONSTRAINT ev_type_ck;
ALTER TABLE alert_events ADD  CONSTRAINT ev_type_ck CHECK (
  type ~ '^[a-z_]+\.[a-z_]+$'
  AND type NOT IN ('group.storm_started','group.storm_ended',
                   'alert.flapping_started','alert.flapping_ended'));

COMMENT ON CONSTRAINT ev_type_ck ON alert_events IS
  'The SHAPE of a timeline event type, <subject>.<fact>, plus a refusal of the four damper spellings deleted with storm damping (ADR 0042) and flap detection (migration 00057). It is not a vocabulary: components.schemas.AlertEventType is the only enumeration of the live set, and a type added there and nowhere else still reaches this column unopposed. The NOT IN clause can only refuse a value that LEFT the set.';

-- ------------------------------------------------- notifications_reason_ck
--
-- The eighteen that remain, in the order 00018 declares them, with `digest`
-- appended by 00058 and `storm` removed. No backfill, for the reason above.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','unacked_reminder','digest'));

COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, eighteen values. Together with the channel verbosity it decides update-in-place versus thread reply. The storm announcement is gone with the damper it announced (ADR 0042): oto withholds nothing, so it has nothing to announce withholding.';

-- --------------------------------------------------- policies_reasons_ck
--
-- ⛔ THE ARRAYS ARE STRIPPED FIRST, AND THIS IS THE ONLY REWRITE IN THE FILE. See
-- the header for why it is allowed where the two enums above forbid one: `reasons`
-- is live configuration rather than history, and `policies_reasons_ck` constrains
-- cardinality and set-ness but never MEMBERSHIP -- so nothing validates `storm` out
-- of this TEXT[] the way `ADD CONSTRAINT` validates it out of `notifications.reason`.
--
-- ⚠️ ORDERING. Both statements run while the 00058 constraint (1..19, no NULLs, a
-- set) is still standing, and both are legal against it: the DELETE removes exactly
-- the rows the UPDATE would empty, so every array the UPDATE touches still has at
-- least one element afterwards; `array_remove` can only SHORTEN a set, so set-ness
-- and NULL-freeness are preserved; and no other policy CHECK is disturbed, because
-- `policies_digest_reason_ck` (00058) is about `digest` and this touches `storm`.
-- They must run BEFORE the ceiling drops to 18, or a policy naming all nineteen
-- post-00058 values would abort the migration on data nothing here could reach.

-- A policy whose ONLY reason was `storm` has no projection onto the eighteen that
-- remain: emptying it violates `cardinality >= 1`, and any substitute reason routes
-- facts nobody asked for. It is deleted, and the header says why that is a deletion
-- of an already-inert rule rather than of live configuration.
-- `notifications.policy_id` is ON DELETE SET NULL (00011), so nothing else moves.
DELETE FROM notification_policies WHERE reasons = ARRAY['storm']::text[];

UPDATE notification_policies
   SET reasons = array_remove(reasons, 'storm'), updated_at = now()
 WHERE 'storm' = ANY(reasons);

-- The ceiling is the ENUM SIZE and moves with it in both directions: 18 at 00046,
-- 19 when 00058 added `digest`, 18 again now. `reasons` is a SET (oto_array_is_set)
-- over a closed eighteen-value vocabulary, so a nineteenth DISTINCT element is
-- unreachable and a ceiling of 19 would be a number no row could ever test.
--
-- With the strip above already done, this ALTER validates against arrays drawn from
-- the eighteen that remain, so it cannot fail on a nineteen-element policy: the only
-- way to reach nineteen was to name `storm`, and no row names it any more.
ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 18
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..18 SPEC H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: cardinality, NULL-freeness and set-ness are all it tests, so a retired Reason left in this array is invisible to the database and surfaces as a validator failure on the Policies page instead. Every narrowing of the Reason vocabulary must strip the retired value from this column by hand, the way 00060 does -- the constraint will not do it for you.';
-- +goose StatementEnd

-- +goose Down

-- The world as 00058 and 00007 left it, IN SHAPE ONLY. The widening direction is
-- always safe -- nothing on disk can violate a check that admits strictly more, so
-- this half never fails.
--
-- ⛔ THERE IS DELIBERATELY NO MIRROR OF THE POLICY STRIP, AND THIS DOWN IS NOT A
-- CLEAN REVERSAL. The Up removed `storm` from every `notification_policies.reasons`
-- array and deleted the policies whose only reason it was. Neither is recoverable
-- here: the array no longer records that the value was ever present, so a mirroring
-- `UPDATE` would have to guess WHICH policies to re-point at `storm`, and adding it
-- back to all of them -- or to none -- would both be inventions. A deleted
-- storm-only policy is gone with its name, its matchers and its channel list. So
-- this half restores only the VOCABULARY the constraints admit: after it runs the
-- schema will once again accept a policy naming `storm`, and no policy will name
-- one. That is a downgrade an operator has to finish by hand, and saying so is
-- better than an `UPDATE` that would look like a rollback and be a fabrication.
--
-- The same limit applies to the two enums for a different reason: the Up deleted no
-- `notifications` or `alert_events` row, but a database reset between the two is not
-- a migration and this Down cannot see it.

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 19
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..19 SPEC H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses. The ceiling is the enum size and moves with it -- migration 00058 added digest.';
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
-- vocab:allow -- the Down must name the value it restores, or it cannot restore it.
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','unacked_reminder','storm','digest'));

COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum. Together with the channel verbosity it decides update-in-place versus thread reply.';

ALTER TABLE alert_events DROP CONSTRAINT ev_type_ck;
ALTER TABLE alert_events ADD  CONSTRAINT ev_type_ck CHECK (type ~ '^[a-z_]+\.[a-z_]+$');

COMMENT ON CONSTRAINT ev_type_ck ON alert_events IS
  'The SHAPE of a timeline event type, <subject>.<fact>. It is not a vocabulary: components.schemas.AlertEventType is the only enumeration of the live set.';
