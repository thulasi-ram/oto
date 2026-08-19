-- git-bug e5c060b, decided by the owner on 2026-08-19: `frozen` is DROPPED.
--
-- ⛔ THE STATE WAS NEVER ENTERABLE. `Freeze` was implemented and declared on the
-- port, and every caller was a test: the repository method had one direct exercise
-- in `clock_test.go` and one fake satisfying the interface in `digest_tick_test.go`.
-- The group-close sweep -- the one place that knows a generation closed -- never
-- called it. So `00011:197`'s comment promised a lifecycle state that nothing in oto
-- could reach, which is the same defect `00057`'s header had about live flap columns
-- and the reason `e05aaad` was filed.
--
-- ⭐ WHY REMOVED RATHER THAN WIRED, which is the fork the ticket left open. The owner
-- ruled a conversation holds exactly ONE Case, and `7570090` deletes `alert_groups`.
-- "The group generation closed" is then not a trigger that exists: there is no
-- generation to close. Wiring `frozen` would have meant inventing a trigger for a
-- concept being deleted in the same quarter.
--
-- ⛔⛔ THIS NARROWS A CHECK SHIPPED IN 00011, AND §D's EXPAND/CONTRACT RULE SAYS NOT
-- TO. 00059 and 00060 each spent the same exemption and each said, in terms, that its
-- header IS NOT A PRECEDENT FOR THE NEXT NARROWING -- so the facts are re-verified here
-- rather than cited. `git tag` lists NOTHING: there is no tagged release, so there is no
-- release N for a contracted schema to be incompatible with, and nobody can have pulled
-- a build even by accident. THE MOMENT A TAGGED RELEASE EXISTS THIS EXEMPTION IS SPENT
-- and the next removal of a CHECK value goes back to expand/contract.
--
-- This narrowing is also the mildest of the three: 00059 and 00060 removed values that
-- had been WRITTEN, and had to argue about rows that already held them. No row has ever
-- held `frozen`.
--
-- ⚠️ NARROWING A CHECK CAN FAIL, AND THAT IS THE HONEST RISK. A row already holding
-- `state = 'frozen'` makes this a 23514. No row can hold it through oto -- the only
-- writer was `freezeThreadSQL` and only tests ever ran it -- so the failure means the
-- value arrived from outside the application, which is worth stopping for rather than
-- quietly rewriting. Deliberately NO data-fixing UPDATE: repairing a row this
-- migration cannot explain would hide the one fact worth learning.
--
-- ⚠️ `threads_open_idx` and the other threads CHECKs are untouched. The index is
-- `WHERE state IN ('opening','open')` and names neither dropped value; `threads_open_ck`
-- and `threads_dead_ck` name `open` and `dead`. Only the vocabulary CHECK narrows.

-- +goose Up

ALTER TABLE channel_threads DROP CONSTRAINT threads_state_ck;
ALTER TABLE channel_threads ADD CONSTRAINT threads_state_ck
  CHECK (state IN ('opening','open','dead'));

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.state IS
  'opening | open | dead. dead means a terminal provider error, which is a STATE TRANSITION, not a retry (SPEC §H.9). ⛔ THERE IS NO `frozen`: it was removed by git-bug e5c060b because nothing ever wrote it -- `Freeze` had no production caller and the group-close sweep never called it -- and because the trigger it was waiting for is being deleted, since a conversation holds exactly ONE Case and there is no generation to close. A state a schema documents but no code can enter is worse than no state at all: it tells a reader the lifecycle has a stop it does not have.';
-- +goose StatementEnd

-- ⭐ AND THE OTHER HALF OF THE PROMISE, WHICH IS NOT ON THIS TABLE AT ALL.
-- `00008:89` told every operator running `\d+ alert_groups` that going idle past
-- `group_close_delay_s` "closes the group and freezes its thread". That sentence
-- describes the TRANSITION INTO the state this migration deletes, so correcting the
-- `channel_threads.state` comment alone would leave the lie standing one table over --
-- and a column comment is not source code: it lives IN THE DATABASE and only a
-- COMMENT ON statement can change what is already there.
--
-- The operator-facing GUIDANCE it supported is still correct and is kept: a shorter
-- close delay really does cost you a new root card. The MECHANISM was wrong. It is not
-- that the old thread is sealed -- nothing seals it -- it is that the next observation
-- opens generation N+1, and `channel_threads.subject_id` is the generation row, so a
-- new generation is a NEW thread (00011's own comment on that column says exactly this).
-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.last_activity_at IS
  'Drives group.close. Idle past orgs.settings.group_close_delay_s closes the group. ⛔ IT DOES NOT FREEZE THE THREAD -- this comment said so until git-bug e5c060b, and nothing ever froze anything: `Freeze` had no production caller and migration 00066 deleted the state. What keeps a re-fire off the closed generation''s card is that the next observation opens generation N+1 and a new generation is a NEW thread, because channel_threads.subject_id IS the generation row. The tuning rule that rests on this -- keep group_close_delay_s at or above refire_grace -- is unchanged and still right; only the reason given for it was wrong.';
-- +goose StatementEnd

-- +goose Down

ALTER TABLE channel_threads DROP CONSTRAINT threads_state_ck;
ALTER TABLE channel_threads ADD CONSTRAINT threads_state_ck
  CHECK (state IN ('opening','open','frozen','dead'));

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.state IS
  'opening | open | frozen | dead. frozen means the group closed; dead means a terminal provider error, which is a STATE TRANSITION, not a retry (SPEC §H.9).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.last_activity_at IS
  'Drives group.close. Idle past orgs.settings.group_close_delay_s closes the group and freezes its thread.';
-- +goose StatementEnd
