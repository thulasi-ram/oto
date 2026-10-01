-- ADR 0052 §6: AN INCIDENT MAY BE A CONVERSATION (git-bug bf5fc7e). Per
-- Correlator, an operator may say its Incidents are conversations; later facts about
-- member Cases then post into the Incident's Slack thread instead of each opening
-- its own. Until this migration forty Cases one Correlator matched posted forty root
-- cards (ADR 0045), and an Incident fact bound for a threaded channel was recorded as
-- a SKIPPED delivery because no thread could be keyed by it (00084's header).
--
-- ⭐⭐ IT NARROWS ADR 0045 AND DOES NOT RETURN `AlertGroup`. 0045's ruling is "N
-- alerts, N conversations"; this is its one exception — "unless an operator-written
-- Correlator says they are one story". It passes ADR 0042 §3 where the deleted
-- group did not: the decision about what shares a message is a column an operator
-- wrote, can read back and can clear with one PATCH (ADR 0044's test), and the
-- default is OFF, so nothing changes for an org until somebody asks by name.
--
-- ⭐ THE SETTING IS READ AT DELIVERY, AND NOTHING IS STAMPED ON THE INCIDENT. The
-- notification layer asks, for each fact as it EVALUATES it, whether the Case is a
-- current member of an Incident whose Correlator says so, and records the answer as
-- that fact's `(conversation_kind, conversation_id)` — the pair 00064 made for
-- exactly this. So the boundary is the membership row's commit: a fact evaluated
-- before it lands in the Case's own thread, one evaluated after lands in the
-- Incident's. Nothing is held back to wait for a Correlator, and nothing already
-- posted moves. A human-drawn Incident has no Correlator and is never one; a Case a
-- human adds to, or moves into, a Correlator's conversation follows its membership.
--
-- ⚠️ `notifications_convkind_ck` IS NOT TOUCHED HERE, CONTRARY TO THE TICKET. 00084
-- already widened it to `('case','digest','incident')`, because an Incident fact must
-- name the conversation it lands in whether or not any channel threads it. What 00084
-- deliberately left alone, and this migration widens, is `threads_subjkind_ck`: until
-- now no thread could be KEYED by an Incident.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One NOT NULL column with a DEFAULT every existing
-- row takes (false — today's behaviour exactly), one widened CHECK and one index. A
-- release-N pod never writes `incident` into `channel_threads` and never reads the
-- column.

-- +goose Up

ALTER TABLE correlators ADD COLUMN incidents_are_conversations BOOLEAN NOT NULL DEFAULT false;

-- +goose StatementBegin
COMMENT ON COLUMN correlators.incidents_are_conversations IS
  'The operator says the Incidents this Correlator draws are CONVERSATIONS (ADR 0052 §6): a fact about a member Case evaluated after its membership committed posts into the Incident''s thread (channel_threads keyed by the Incident) rather than the Case''s own, and each member Case that already had a thread gets one "now part of Incident #N" reply there. Read at delivery, never stamped on the Incident: turning it on or off redirects later facts only, and nothing already posted moves. Default false, which is ADR 0045''s one conversation per Case. A human-drawn Incident has no Correlator and is never a conversation. Not cleared by deleted_at or enabled: those stop a Correlator drawing, not the stories it drew.';
-- +goose StatementEnd

ALTER TABLE channel_threads DROP CONSTRAINT threads_subjkind_ck;
ALTER TABLE channel_threads ADD  CONSTRAINT threads_subjkind_ck
  CHECK (subject_kind IN ('alert','case','digest','incident'));

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_kind IS
  'WHAT this conversation is keyed by: case (one Case, ADR 0045), digest (one policy) or incident (one Incident whose Correlator says its Incidents are conversations, ADR 0052 §6, 00087). alert is admitted and nothing keys a thread by it. An Incident thread''s ROOT is always the Incident''s own card -- members, active or quiet -- whichever fact opened it; Case facts and Incident facts reply beneath it.';
-- +goose StatementEnd

-- Serves: the "now part of Incident #N" sweep — every thread, on every channel, a
-- member Case already has. `threads_subject_uniq` leads with `channel_id`, which the
-- sweep does not know: the threads are where the Case's messages actually landed,
-- and the policy that put them there may have changed since.
CREATE INDEX threads_subject_idx ON channel_threads (org_id, subject_kind, subject_id);

-- +goose StatementBegin
COMMENT ON COLUMN notifications.conversation_id IS
  'The conversation this fact lands in, in the table conversation_kind names: alert_cases.id for `case`, notification_policies.id for `digest`, incidents.id for `incident`. No FK, because one column cannot reference three tables -- the same reason subject_id has none. A CASE fact may name an `incident` conversation (00087): it was evaluated while its Case was a member of an Incident whose Correlator says its Incidents are conversations, and that answer is frozen here so the fact is never re-placed. An INCIDENT fact may name a `case` conversation: the one "now part of Incident #N" reply posted into a member Case''s own thread.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ THE DOWN DELETES WHAT RELEASE N-1 CANNOT PLACE, AND IT DELETES RATHER THAN
-- RE-POINTS — 00058's choice for the digest conversations, for its reason: rewriting
-- `conversation_kind` back to `case` would make the receipt lie about where a message
-- landed.
--
-- 1. Deliveries threaded on an Incident conversation go first, by thread, because
--    `notification_deliveries.thread_id` is ON DELETE SET NULL and
--    `deliveries_thread_ck` refuses a NULL thread on anything but `post_root` — so
--    deleting the threads first would fail on the first reply.
DROP INDEX IF EXISTS threads_subject_idx;

DELETE FROM notification_deliveries
 WHERE thread_id IN (SELECT id FROM channel_threads WHERE subject_kind = 'incident');

DELETE FROM channel_threads WHERE subject_kind = 'incident';

-- 2. A Case fact placed in an Incident conversation. Release N-1 reads a Case fact's
--    Case out of `conversation_id`, which here is an Incident, so a queued delivery
--    of one would build its card from no Case. Its remaining deliveries go with it
--    (ON DELETE CASCADE).
DELETE FROM notifications WHERE conversation_kind = 'incident' AND subject_kind <> 'incident';

-- 3. The "now part of Incident #N" pointers: an Incident fact in a Case's own
--    conversation, which release N-1 has no renderer for.
DELETE FROM notifications WHERE subject_kind = 'incident' AND conversation_kind = 'case';

ALTER TABLE channel_threads DROP CONSTRAINT threads_subjkind_ck;
ALTER TABLE channel_threads ADD  CONSTRAINT threads_subjkind_ck
  CHECK (subject_kind IN ('alert','case','digest'));

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_kind IS
  'WHAT this conversation is keyed by: alert | case | alert_group. v1 opens every thread on the AlertGroup generation, so forty alerts still produce one thread; the column is widened because thread identity is a POLICY decision and was welded to the alert grouping by a one-line CHECK. Widening it is what makes threads_subject_uniq say something -- a kind that could hold one value made its first column a constant.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.conversation_id IS
  'The conversation this fact lands in, in the table conversation_kind names: alert_cases.id for `case`, notification_policies.id for `digest`. No FK, because one column cannot reference two tables -- the same reason subject_id has none. This is now the ONLY delivery-target column: `group_id` was dropped with alert_groups in git-bug 7570090, and the three readers that used it for SUBJECT-shaped questions were answered rather than re-pointed -- the rollup''s second leg was a fan-out rule (a notification about the group counted as one about each member alert) and there is no container left for it to be about.';
-- +goose StatementEnd

-- ⚠️ THE DOWN DROPS CONFIGURATION: every Correlator falls back to one conversation
-- per Case, and the value is recoverable from nowhere else.
ALTER TABLE correlators DROP COLUMN IF EXISTS incidents_are_conversations;
