-- git-bug 7570090, stage 3 of 7.
--
-- ⭐ THE DELIVERY TARGET BECOMES A PAIR, MIRRORING THE SUBJECT PAIR. `notifications`
-- already answers "what is this fact ABOUT" with `(subject_kind, subject_id)`
-- (00056). It answered "WHERE does it land" with `group_id` plus one exception, and
-- the exception was written into a CHECK: `notifications_target_ck`
-- (00058:278) reads `subject_kind = 'digest' OR group_id IS NOT NULL`, i.e. every
-- fact names a group EXCEPT a digest. That shape does not extend. A policy-collapsed
-- conversation is neither a group nor a digest, and adding it would mean a third arm
-- in the same CHECK and a third branch in every reader.
--
-- `(conversation_kind, conversation_id)` makes a digest ONE KIND AMONG SEVERAL
-- instead of the exception, and lets the next kind arrive without a migration.
--
-- ⛔ AND THE NEXT KIND IS ALREADY DECIDED: `case`. The owner ruled on 2026-08-19
-- that a conversation holds exactly ONE Case, never a collapse of several. So when
-- stage 4 deletes `alert_groups`, `alert_group` leaves this vocabulary and `case`
-- replaces it -- `alert_cases` already has the one-per-conversation cardinality the
-- ruling requires, and is already the product's first destination. This migration
-- stores TODAY's identity unchanged; it is a refactor of shape, not of behaviour,
-- and it is the pair that makes that later swap a value change rather than a
-- schema change.
--
-- ⛔ THIS IS EXACTLY WHAT `threadSubjectOf` ALREADY COMPUTES, and that is the
-- evidence the pair is the right shape rather than a guess. `notify.go` has:
--
--     if n.Digest() { return SubjectDigest, n.SubjectID }
--     return SubjectAlertGroup, n.GroupID
--
-- — a kind and an id, derived on every fan-out and then thrown away. The column is
-- that function's return value, stored. `dispatch.go:739` already branches on the
-- THREAD's kind rather than on `n.Digest()`, so one reader is in the target shape
-- today.
--
-- ⛔ `group_id` IS KEPT AND IS NOT REDUNDANT. Several readers use it to answer
-- SUBJECT-shaped questions -- the rollup's per-alert coverage
-- (`notification/repository/rollup.go:103-109`), the drill artifact read, the audit
-- filter -- and those are not the same question as "which conversation". Dropping it
-- belongs with stage 4, where `alert_groups` goes and each of those readers has to
-- be answered deliberately rather than by deleting a column they happen to use.
--
-- ⚠️ AND THE GO HALF OF THE EXCEPTION SURVIVES THIS MIGRATION. `Notify` rejects a
-- nil `GroupID` unconditionally (`notify.go:244`), and the digest path only escapes
-- it by bypassing `Notify` entirely and building its row in `digest.go:335`. So the
-- digest exception lived in TWO places, and retiring the CHECK closes one. The other
-- closes in stage 4, when `group_id` stops being required at all. Said here so the
-- next reader does not conclude from a green migration that the exception is gone.

-- +goose Up

-- EXPAND: nullable, backfilled, then tightened. The table is written on the ingest
-- hot path, so a NOT NULL with no default would fail every in-flight insert from a
-- release-N pod during the rollout.
ALTER TABLE notifications ADD COLUMN conversation_kind TEXT;
ALTER TABLE notifications ADD COLUMN conversation_id   UUID;

-- The backfill IS `threadSubjectOf`, in SQL. A digest converses with its policy; a
-- non-digest converses with the generation's thread, which is its `group_id`. The
-- old CHECK guaranteed `group_id IS NOT NULL` for every non-digest row, so this is
-- total and no row is left behind -- which is why the tightening below is safe.
UPDATE notifications
   SET conversation_kind = 'digest',
       conversation_id   = subject_id
 WHERE subject_kind = 'digest';

UPDATE notifications
   SET conversation_kind = 'alert_group',
       conversation_id   = group_id
 WHERE subject_kind <> 'digest';

ALTER TABLE notifications ALTER COLUMN conversation_kind SET NOT NULL;
ALTER TABLE notifications ALTER COLUMN conversation_id   SET NOT NULL;

-- ⭐ THE VOCABULARY IS ITS OWN CHECK, AND IT IS DELIBERATELY NOT `subject_kind`'s.
-- A subject is what a fact is about (`alert | case | alert_group | digest`); a
-- conversation is where it lands, and the two sets are not the same and will
-- diverge further -- `alert` and `case` are subjects that no conversation is ever
-- keyed by, and the policy collapse stage 4 adds is a conversation that is no
-- subject at all. Sharing one CHECK between them would tie two vocabularies that
-- have different reasons to change.
ALTER TABLE notifications ADD CONSTRAINT notifications_convkind_ck
  CHECK (conversation_kind IN ('alert_group', 'digest'));

-- The exception is retired. Every row now names a conversation, unconditionally,
-- and `notifications_convkind_ck` above says which kinds exist without singling one
-- out.
ALTER TABLE notifications DROP CONSTRAINT notifications_target_ck;

-- +goose StatementBegin
COMMENT ON COLUMN notifications.conversation_kind IS
  'WHERE this fact lands: which kind of conversation owns the thread. Mirrors subject_kind, and is deliberately a SEPARATE vocabulary -- a subject is what a fact is ABOUT, a conversation is where it is DELIVERED, and `alert` and `case` are subjects no conversation is keyed by. Bounded by notifications_convkind_ck. This pair replaced `group_id` plus notifications_target_ck, whose shape made a digest the one exception in a CHECK and could not have absorbed a third kind (git-bug 7570090).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.conversation_id IS
  'The conversation this fact lands in, in the table conversation_kind names: alert_groups.id for `alert_group`, notification_policies.id for `digest`. No FK, because one column cannot reference two tables -- the same reason subject_id has none. ⛔ NOT the same question as group_id, which several readers use to answer SUBJECT-shaped questions (the per-alert rollup, the drill artifact read, the audit filter) and which survives until stage 4 answers each of them deliberately.';
-- +goose StatementEnd

-- +goose Down

-- Byte-identical to what 00058 shipped, so a rolled-back database is in the state
-- its migration history describes.
ALTER TABLE notifications ADD CONSTRAINT notifications_target_ck
  CHECK (subject_kind = 'digest' OR group_id IS NOT NULL);

ALTER TABLE notifications DROP CONSTRAINT notifications_convkind_ck;
ALTER TABLE notifications DROP COLUMN conversation_id;
ALTER TABLE notifications DROP COLUMN conversation_kind;
