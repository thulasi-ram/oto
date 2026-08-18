-- SPEC §D.8, §H.6 -- `notifications.subject_kind` and `channel_threads.subject_kind`
-- admit `alert | case | alert_group`, and `notifications.subject_id` is tied by a
-- CHECK to the id column its kind names.
--
-- ⭐⭐ WHAT WAS WRONG. Both tables model a subject as a (kind, id) PAIR and then
-- constrained the kind to a single value:
--
--     threads_subjkind_ck        CHECK (subject_kind IN ('alert_group'))
--     notifications_subjkind_ck  CHECK (subject_kind IN ('alert_group'))
--
-- So every Notification claimed the AlertGroup generation as its subject even when
-- the fact it carried was about ONE FIRING. An acknowledgement is a receipt against
-- a Case -- `alert_cases.ack_state` is where 00049 put it, and ADR 0041 lists it as
-- one of four independent axes, "a human attention, per firing" -- and the row
-- recording that receipt said it was about the group of forty. `subject_id` was
-- therefore unusable as a join key: its meaning was fixed by convention rather than
-- declared by the row, and the 00011 comment admitted as much ("Mirrors group_id").
--
-- The schema already knew the subject varies. `notifications` carries `alert_id`
-- (00011) and `case_id` (00011 as `occurrence_id`, renamed in 00052) next to
-- `group_id`, `notifications_focus_ck` already makes `alert_id` mandatory for four
-- reasons, and `enrichments_subjkind_ck` has admitted three kinds since 00010. Only
-- these two CHECKs pretended otherwise.
--
-- ⭐ THE SUBJECT IS WHAT THE FACT IS ABOUT. `group_id` IS WHERE IT IS DELIVERED.
-- Fusing them is what made an ack unaddressable. They are separated here, and
-- `group_id` STAYS NOT NULL: a Notification with no delivery target is a Notification
-- with nowhere to go, and every writer already supplies one because every producer
-- of a per-alert fact reaches the group through the Case it moved
-- (`alerts/service/lifecycle.go` applyEdge, `actions.go` Acknowledge, Comment and
-- notifySnoozeChange, `sweep.go` expiry -- all four pass groupID, alertID and
-- caseID together). Relaxing NOT NULL is the expand-SAFE direction and dropping it
-- would still be reversible, but there is no row that needs it: the delivery target
-- is orthogonal to the subject, not an alternative to it.
--
-- ⭐ WHY A CHECK AND NOT A FOREIGN KEY. `subject_id` addresses three tables, and one
-- column cannot have one FK to three of them. The alternative -- three nullable
-- typed columns plus a CHECK that exactly one is set -- is the shape this table
-- ALREADY HAS, because `group_id`, `alert_id` and `case_id` are those columns and two
-- of them predate this file. So the tie is stated as agreement between `subject_id`
-- and the typed column its kind names, and referential integrity keeps coming from
-- `group_id` FK (00011) plus `notif_alert_idx` and `notif_case_idx` on the other two.
-- `alert_id` and `case_id` deliberately have no FK of their own, which 00011 and
-- 00029 both record: a notification is retained evidence and must outlive a purge of
-- what it was about.
--
-- ⚠️ THERE IS DELIBERATELY NO reason -> subject_kind CHECK, and the omission is the
-- expand/contract rule, not an oversight. Which subject each Reason declares is the
-- ALLOCATION, and it lives in `internal/notification/domain/reason.go` where a test
-- proves it total. A CHECK saying `reason = 'acked' IMPLIES subject_kind = 'case'`
-- would reject every row the release-N binary writes -- release N writes
-- `alert_group` for all eighteen reasons -- from the instant this file lands, and
-- release N and N+1 run at the same time. It would also fail validation against
-- history: every `acked` row already in the table says `alert_group`. The database
-- constrains the SHAPE of a subject; the domain decides WHICH subject a Reason has.
--
-- ⭐ THREADING IS UNCHANGED. FORTY ALERTS STILL PRODUCE ONE THREAD.
-- `threads_subject_uniq UNIQUE (channel_id, subject_kind, subject_id)` continues to
-- mean exactly one conversation per subject; widening the kind is what finally makes
-- that sentence say something, because a subject that could only be a group made the
-- `subject_kind` column of that index a constant. v1 keys every thread by the
-- AlertGroup generation and this file writes no thread rows, so the widening buys the
-- CONVERSATION KEY the design was blocked on rather than spending it.
--
-- ⚠️ ONE GO CALL SITE MUST BE PINNED BEFORE A NON-GROUP SUBJECT IS EVER WRITTEN.
-- `internal/notification/service/notify.go` resolves the thread with
-- `s.threads.Ensure(ctx, scope, channelID, n.SubjectKind, n.SubjectID, now)`. That
-- passes the NOTIFICATION subject as the THREAD subject, which is correct only while
-- the two are the same value. Once a Notification says `case`, that call would open a
-- thread per Case and the forty-alert card would shatter. The thread subject is
-- `(SubjectAlertGroup, GroupID)` and must be spelled that way.
--
-- ⭐ THE GROUP KIND KEEPS THE SPELLING `alert_group`, and `enrichments` keeps `group`.
-- The two vocabularies differ by one word and this is the file that records why: rows
-- already carry `alert_group` under an index (`notif_subject_idx`) and a unique
-- constraint (`threads_subject_uniq`) that both lead with it, and re-spelling a
-- persisted enum value to match a neighbouring table buys nothing and re-keys
-- everything. `alert` and `case` are spelled identically in both.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). This is an EXPAND step, entirely additive.
-- Widening a CHECK cannot reject a row the previous release could write, no column
-- changes nullability, no column is dropped, and the one new constraint is satisfied
-- by every row release N produces (`subject_kind = 'alert_group'` with
-- `subject_id = group_id`, which is what `notify.go` has always written and what
-- every fixture in the tree spells `$3,$3`). It is safe to deploy under release N.
--
-- ⭐ THE DOWN IS LOSSLESS AND IT WORKS. Narrowing the notifications CHECK back would
-- abort on any row that had declared a non-group subject, so the Down NORMALISES
-- first: `subject_kind` returns to `alert_group` and `subject_id` to `group_id`, which
-- is exactly the pair release N wrote. Nothing is destroyed -- the alert or case the
-- row was about is still on `alert_id` / `case_id`, which is where the Down leaves the
-- information the subject was carrying. `channel_threads` needs no normalisation and
-- must not be given one: a thread keyed by an alert or a case cannot be re-keyed to
-- its group without colliding with the group own thread under `threads_subject_uniq`,
-- and deleting a thread would destroy oto memory of Slack (C9). v1 writes no such
-- row, so the narrowing is a no-op; if one ever exists the Down ABORTS, which is the
-- honest outcome for a handle a rollback cannot reconstruct.

-- +goose Up

-- ------------------------------------------------------------- channel_threads

ALTER TABLE channel_threads DROP CONSTRAINT threads_subjkind_ck;
ALTER TABLE channel_threads ADD  CONSTRAINT threads_subjkind_ck
  CHECK (subject_kind IN ('alert', 'case', 'alert_group'));

-- --------------------------------------------------------------- notifications

ALTER TABLE notifications DROP CONSTRAINT notifications_subjkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subjkind_ck
  CHECK (subject_kind IN ('alert', 'case', 'alert_group'));

-- The tie. Each arm names the id column its kind declares AND requires that column
-- to be present, because `subject_id = alert_id` against a NULL `alert_id` evaluates
-- to NULL and a CHECK passes on NULL -- which would have let an alert-subject row
-- name no alert at all, the same silent hole this migration exists to close.
--
-- `group_id` needs no NULL guard: it is NOT NULL and stays that way.
ALTER TABLE notifications ADD CONSTRAINT notifications_subject_ck CHECK (
     (subject_kind = 'alert'       AND alert_id IS NOT NULL AND subject_id = alert_id)
  OR (subject_kind = 'case'        AND case_id  IS NOT NULL AND subject_id = case_id)
  OR (subject_kind = 'alert_group' AND subject_id = group_id));

-- ------------------------------------------------------------------- the words

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
COMMENT ON COLUMN notifications.alert_id IS
  'The specific Alert. MANDATORY for acked, unacked, refired and rule_changed (notifications_focus_ck), and MANDATORY as the subject whenever subject_kind = alert (notifications_subject_ck). The two are different questions: acked names an alert AND is a fact about the Case, because ack lives on the firing episode (00049, ADR 0041).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.case_id IS
  'The AlertCase -- the ONE FIRING EPISODE this fact is about, when it is about one. MANDATORY as the subject whenever subject_kind = case (notifications_subject_ck). No FK: a notification is retained evidence about a delivery and must outlive a purge of the episode it reported.';
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
COMMENT ON COLUMN channel_threads.subject_id IS
  'The row this conversation is about, in the table subject_kind names. For alert_group it is one GENERATION: a re-opened group is a new generation and therefore a new thread.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT threads_subject_uniq ON channel_threads IS
  'Exactly one conversation per (channel, subject). Not per channel and group -- per channel and SUBJECT, which is the same sentence it always was and is now a constraint on three kinds rather than a restatement of one.';
-- +goose StatementEnd

-- The two counting readers -- the policy throttle and the "how loud was this?" receipt --
-- ask what was DELIVERED to a group's thread, not what a row is about, so they key on
-- `group_id`. Before this migration they filtered `subject_kind = 'alert_group'` and rode
-- `notif_subject_idx`, whose leading column was a constant while only one kind existed.
-- Widening the kind takes that index away from them: nothing indexed `group_id`, because
-- the 00011 FK to `alert_groups` creates no index on the referencing side.
CREATE INDEX notif_group_idx ON notifications (org_id, group_id, created_at DESC);

-- +goose StatementBegin
COMMENT ON INDEX notif_group_idx IS
  'Delivery target, not subject. Serves the policy throttle (windowed) and the group notification receipt (unbounded in time). Both were subject-keyed until subject_kind admitted more than one value; keep this index if either query survives.';
-- +goose StatementEnd

-- +goose Down

DROP INDEX IF EXISTS notif_group_idx;

-- The subject returns to the mirror release N wrote. `alert_id` and `case_id` keep
-- what the subject was carrying, so the rollback loses no fact -- only the ability to
-- state which of the three the row was about.
UPDATE notifications
   SET subject_kind = 'alert_group',
       subject_id   = group_id
 WHERE subject_kind <> 'alert_group';

ALTER TABLE notifications DROP CONSTRAINT notifications_subject_ck;

ALTER TABLE notifications DROP CONSTRAINT notifications_subjkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subjkind_ck
  CHECK (subject_kind IN ('alert_group'));

-- No normalisation, deliberately: see the header. A non-group thread cannot be
-- re-keyed without colliding under threads_subject_uniq and must not be deleted, so
-- this narrowing aborts rather than corrupting or forgetting a provider handle.
ALTER TABLE channel_threads DROP CONSTRAINT threads_subjkind_ck;
ALTER TABLE channel_threads ADD  CONSTRAINT threads_subjkind_ck
  CHECK (subject_kind IN ('alert_group'));

-- The 00011 wording, restored verbatim where 00011 had one and dropped where it did
-- not: `subject_kind`, `group_id` and both constraint comments were uncommented
-- before this migration, and a rolled-back catalogue should say what it said.
-- +goose StatementBegin
COMMENT ON CONSTRAINT threads_subject_uniq ON channel_threads IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_kind IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_id IS 'The alert_groups row -- one GENERATION. A re-opened group is a new generation and therefore a new thread.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.subject_kind IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.group_id IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.case_id IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.subject_id IS 'The alert_groups generation this fact is about. Mirrors group_id; kept because subject_kind is designed to grow.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.alert_id IS 'The specific Alert, when the fact is about one. MANDATORY for acked, unacked, refired and rule_changed (notifications_focus_ck).';
-- +goose StatementEnd
