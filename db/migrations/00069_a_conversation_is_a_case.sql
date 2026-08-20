-- git-bug 7570090, stage 4 of 7. THE ENTITY GOES.
--
-- ⭐ A CONVERSATION IS A CASE. 00064 made the delivery target a pair
-- `(conversation_kind, conversation_id)` and said, in its own header, what the next
-- kind would be: *"when stage 4 deletes `alert_groups`, `alert_group` leaves this
-- vocabulary and `case` replaces it"*. This is that migration. `alert_groups` is
-- dropped, `alert_cases.group_id` and `notifications.group_id` go with it, and every
-- closed set that admitted the group vocabulary narrows to the sets that are left.
--
-- The ruling this rests on was made on 2026-08-19 and is comment #3 on the ticket:
-- ONE CASE PER CONVERSATION, ALWAYS. `alert_cases` already has that cardinality and
-- is already the product's first destination (`2c26520` redirects `/` to `/cases`),
-- so nothing new is invented here -- the conversation re-points at a row that exists.
--
-- ⛔⛔ THIS MIGRATION DESTROYS DATA AND THE DOWN CANNOT BRING IT BACK. Read this
-- before `goose up`, not after. What is destroyed:
--
--   * EVERY `alert_groups` ROW. Twenty-six columns per generation: the derived
--     `group_key`, the generation number, `source_group_key`, `receiver`, the split
--     axes in `group_labels`, the rendered `title`, the six counts, `severity`, and
--     the three timestamps. The table is dropped, not emptied.
--   * EVERY `alert_cases.group_id`. The membership -- which generation each episode
--     belonged to, and since 00051 the ONLY record of it, because
--     `alert_group_members` was dropped in favour of this column.
--   * EVERY `notifications.group_id`. Which thread each fact landed on.
--   * Every `notifications` row whose `reason` is `new_alerts` or `some_resolved`,
--     and every `notification_deliveries` row whose `mode` is `broadcast_reply` --
--     not deleted by this file, but made UNWRITABLE AND UNVALIDATABLE by it, so a
--     database holding one refuses the migration (see the ⚠️ on narrowing below).
--   * Every `notification_policies.reasons` element naming the two dropped reasons,
--     and every policy whose reasons were ONLY those two, which is deleted outright.
--
-- WHAT THE DOWN DOES RESTORE: the STRUCTURE, so the schema is walkable and every
-- migration below this one still resolves. `alert_groups` comes back with all
-- twenty-six columns, its twelve CHECKs, its unique key, its five indexes, its three
-- foreign keys and its comments; `alert_cases.group_id` and `notifications.group_id`
-- come back with their foreign keys and their indexes; every narrowed CHECK goes back
-- to the predicate 00068 shipped. It comes back EMPTY and every `group_id` comes back
-- NULL. That is not a defect in the Down, it is the shape of the loss: no migration
-- can invent which generation an episode belonged to once the generation is gone.
-- The Down exists so that `goose down` past this point works at all -- 00066's Down
-- runs `COMMENT ON COLUMN alert_groups.last_activity_at`, 00059's re-adds two columns
-- to the table, 00051's rebuilds `alert_group_members` with a foreign key into it, and
-- every one of those is a 42P01 against a schema where the table never came back.
--
-- ⛔ AND THE ANSWER TO THE LOSS IS A RESET, WHICH IS AUTHORISED. `git tag` lists
-- NOTHING -- zero tags -- and `.github/workflows/release.yml` publishes an image only
-- on a `v*` tag push, so no build of oto has ever left this repository and there is no
-- deployment holding rows anybody wants. The reset was authorised on this basis for
-- 00067 and 00068 and the basis is re-verified here rather than cited. THE MOMENT A
-- TAGGED RELEASE EXISTS THIS STOPS BEING TRUE and a deletion of this size needs a
-- backfill plan instead of a paragraph.
--
-- ⚠️ SEVEN CLOSED SETS NARROW, AND A NARROWING IS THE HALF THAT CAN FAIL. This is
-- 00018's rule, restated by 00059, 00060, 00066 and 00067: an enum narrowing with NO
-- HONEST DOWNLEVEL MAPPING must FAIL rather than invent one. There is deliberately no
-- `UPDATE` turning a stored `alert_group` conversation into a `case` one, and that is
-- not laziness -- it is the cardinality. A generation held MANY cases, so "which case
-- was this group's conversation about" has no answer, and picking one would fabricate
-- a fact about where a message landed. `ADD CONSTRAINT` validates the rows on disk, so
-- a database that recorded a group conversation refuses this migration with a 23514
-- naming the constraint, and the answer is the reset above. Same for a
-- `broadcast_reply` delivery and a `new_alerts` notification: both were real facts
-- sent to real channels, and there is nothing honest to rewrite them as.
--
-- ⭐ WHY `new_alerts` AND `some_resolved` GO AND `all_resolved` STAYS. Both dropped
-- reasons assert a PLURALITY inside one conversation: `new_alerts` means "more alerts
-- joined the thing this thread is about" and `some_resolved` means "part of it
-- cleared". A conversation holds one Case, so there is no part and no rest -- neither
-- reason has anything left to be about. `all_resolved` survives because it collapses
-- into the Case's own resolve: a Case resolving is a fact ABOUT the Case and lands in
-- the Case's conversation. That is the second of the two answers ticket comment #6
-- lays out, and it is chosen deliberately over the first (both reasons die and
-- broadcast dies with them).
--
-- ⚠️ `refired` STAYS DECLARED AND STILL HAS NO PRODUCER. `broadcast.go:71` says so
-- itself -- *"⛔ RETAINED FOR HISTORY; NOTHING PRODUCES IT"* -- and ADR 0040 retired
-- the T8 transition that used to mint it. It is NOT dropped here. That is a separate
-- decision about a separate vocabulary entry, and bundling it into a migration about
-- the group would make this file the place a future reader looks for a ruling that was
-- never made here.
--
-- ⛔ `broadcast_reply` IS REMOVED AS A DELIVERY MODE, BY RULING, AND THAT IS NOT THE
-- SAME QUESTION AS `all_resolved`. Slack thread-broadcast goes entirely: the mode
-- leaves `deliveries_mode_ck`, so `all_resolved` survives as an ordinary thread reply
-- in the Case's conversation rather than as a message echoed to the channel. Keeping
-- the mode after the ruling would leave a value the schema admits and no dispatcher
-- can choose, which is the defect `0457f1f`, `35d4248`, `39e48e2` and `27a1860` were
-- each filed and closed for.
--
-- ⚠️ `notifications_target_ck` IS ALREADY GONE AND IS NOT DROPPED HERE. It was added
-- by 00058 (`subject_kind = 'digest' OR group_id IS NOT NULL`) and RETIRED BY 00064,
-- which replaced it with the pair. A `DROP CONSTRAINT` for it in this file would be a
-- 42704. Its restoration lives in 00064's Down and stays there. Said explicitly
-- because the ticket body still lists retiring it as stage 4's work, and it was done
-- one stage early.
--
-- ⛔⛔ WHAT IS DELIBERATELY LEFT, so the next reader does not "finish the job":
--
--   * `alert_events.group_id` AND `ev_subject_ck`. The column is 00007:245 -- it
--     PREDATES 00008 and is therefore NOT a foreign key to `alert_groups`, so dropping
--     the table does not break it -- and `ev_subject_ck` (00007:249) reads
--     `alert_id IS NOT NULL OR case_id IS NOT NULL OR group_id IS NOT NULL`.
--     `alert_events` is append-only history with a thirteen-month retention, and those
--     ids are real facts about what happened. This is the 00051/00054 bargain:
--     READABLE, UNWRITABLE. The column and the CHECK stay exactly as they are, so old
--     rows keep validating and keep rendering, and nothing new writes a group id
--     because there is no group to name. Narrowing `ev_subject_ck` would make existing
--     rows unvalidatable; dropping the column would delete history the product retains
--     on purpose. It is the same treatment the ticket gives `group.opened` and
--     `group.closed`.
--   * `delivery_drills.group_id`. Also unFK'd, also uncommented. Ticket comment #6
--     records that what a drill's evidence becomes is an OPEN QUESTION the ticket never
--     answered, and answering it inside a migration about the group would be deciding
--     it by accident.
--   * ~~`enrichments_subjkind_ck`~~ -- ⭐ NOW DONE IN THIS MIGRATION, below. It was
--     deferred because narrowing it needs the enrichment domain's `SubjectGroup`
--     (`internal/enrichment/domain/enrichment.go:328`) to go in the SAME change, or
--     the schema starts refusing a value the validator still accepts. That Go change
--     has landed: `SubjectGroup` had exactly four references in the whole tree -- its
--     declaration, the `Valid` switch and one test asserting acceptance -- with no
--     producer and no reader, so it was deleted and the test inverted to assert
--     REJECTION. Nothing ever wrote `'group'`, so no UPDATE is owed in either
--     direction and no row can fail validation.
--
-- ⚠️ THE THREE `notifications.group_id` READERS THIS FORCES. 00064's Down-note kept
-- the column because three reads used it for SUBJECT-shaped questions. Ticket comment
-- #6 settled it: the rollup's second leg (`rollup.go:103-109`) is not a mis-typed
-- subject lookup, it is a FAN-OUT RULE -- *a notification about the group counts as a
-- notification about each member alert* -- and with no container there is nothing for
-- the rule to be about, so it is DELETED rather than re-pointed. Same for the audit
-- filter and the drill artifact read. The Go half of that is not in this file; the
-- column going is what makes it impossible to defer again.

-- +goose Up

-- ------------------------------------------------- notifications_reason_ck
--
-- The strip runs FIRST, and it is not optional: `policies_reasons_ck` constrains
-- cardinality, NULL-freeness and set-ness and NOT membership, so a dropped Reason left
-- in this array is invisible to the database and surfaces as a validator failure on
-- the Policies page. 00060 and 00067 each did this by hand and each wrote that the
-- constraint will not do it for you. This is that hand, twice.
--
-- The DELETE precedes the UPDATE for the reason 00067's did: `cardinality >= 1`, so a
-- policy whose ONLY reasons are the two being dropped cannot be stripped down to `{}`
-- -- it has to go. `<@` is the generalisation of 00067's equality test to two values:
-- it holds when every element of `reasons` is one of the two, which covers
-- `{new_alerts}`, `{some_resolved}` and `{new_alerts,some_resolved}`.
DELETE FROM notification_policies
 WHERE reasons <@ ARRAY['new_alerts','some_resolved']::text[];

UPDATE notification_policies
   SET reasons = array_remove(array_remove(reasons, 'new_alerts'), 'some_resolved'),
       updated_at = now()
 WHERE reasons && ARRAY['new_alerts','some_resolved']::text[];

-- The fifteen that remain, in the order 00018 declares them, with `digest` appended by
-- 00058, `storm` removed by 00060, `unacked_reminder` removed by 00067 and the two
-- plurality reasons removed here.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','all_resolved','repeat','suppressed','unsuppressed','expired','refired',
   'acked','unacked','snoozed','unsnoozed','enriched','rule_changed','comment','digest'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, fifteen values. Together with the channel verbosity it decides update-in-place versus thread reply. The storm announcement went with the damper it announced (ADR 0042). The unacked reminder went because oto sends nothing unprompted (git-bug bd0fb1d). `new_alerts` and `some_resolved` went with the container they counted (git-bug 7570090): both assert a plurality inside one conversation, and a conversation holds exactly one Case, so there is no part and no rest for either to name. `all_resolved` stays -- a Case resolving is a fact about the Case and lands in the Case''s own conversation. `refired` stays DECLARED and still has no producer (ADR 0040 retired T8); that is a separate decision, not an oversight here.';
-- +goose StatementEnd

-- The ceiling is the enum size and moves with it in both directions: 18 at 00046, 19
-- when 00058 added `digest`, 18 when 00060 removed `storm`, 17 at 00067, 15 now. A
-- sixteenth DISTINCT element is unreachable over a fifteen-value vocabulary, so a
-- higher ceiling would be a number no row could ever test. It has never been a number
-- anybody chose: it is `len(AllReasons())`.
--
-- The strip above already ran, so this validates against arrays drawn from the fifteen
-- that remain and cannot fail on an over-long policy.
ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 15
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..15 SPEC H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder, 00069 removed new_alerts and some_resolved. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: cardinality, NULL-freeness and set-ness are all it tests, so a removed Reason left in this array is invisible to the database and surfaces as a validator failure on the Policies page instead. Every narrowing of the Reason vocabulary must strip the value from this column by hand, the way 00060, 00067 and 00069 do -- the constraint will not do it for you.';
-- +goose StatementEnd

-- ------------------------------------------------- deliveries_mode_ck
--
-- Slack thread-broadcast is removed entirely, by ruling. `MsgOptionBroadcast` was the
-- only thing this mode selected, and with no mode there is nothing for it to select.
--
-- ⚠️ DELIBERATELY NO DATA-FIXING UPDATE. A delivery recorded as `broadcast_reply`
-- really was broadcast to the channel, and rewriting it to `thread_reply` would make
-- the receipt lie about what the operator saw. A database holding one refuses this
-- statement with a 23514, which is the honest outcome and the reset is the answer.
ALTER TABLE notification_deliveries DROP CONSTRAINT deliveries_mode_ck;
ALTER TABLE notification_deliveries ADD CONSTRAINT deliveries_mode_ck
  CHECK (mode IN ('post_root','update_root','thread_reply'));

-- +goose StatementBegin
COMMENT ON COLUMN notification_deliveries.mode IS
  'post_root | update_root | thread_reply. chat.update in place is PRIMARY; thread replies are the exception. repeat interval elapsed means UPDATE ONLY, NEVER POST (CONTEXT.md §6). ⛔ THERE IS NO `broadcast_reply`: Slack thread-broadcast was removed outright by ruling in git-bug 7570090, so a reply lands in the conversation and nowhere else. `all_resolved` -- the one Reason broadcast could still have been reached by -- survives as an ordinary reply in the Case''s conversation.';
-- +goose StatementEnd

-- ------------------------------------------------- the subject vocabulary
--
-- `alert_group` leaves both subject sets. What a fact can be ABOUT, and what a
-- conversation can be KEYED BY, are `alert`, `case` and `digest`.
ALTER TABLE notifications DROP CONSTRAINT notifications_subjkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subjkind_ck
  CHECK (subject_kind IN ('alert','case','digest'));

ALTER TABLE channel_threads DROP CONSTRAINT threads_subjkind_ck;
ALTER TABLE channel_threads ADD  CONSTRAINT threads_subjkind_ck
  CHECK (subject_kind IN ('alert','case','digest'));

-- `notifications_subject_ck` NAMES `group_id` in its third arm, so it is rewritten
-- BEFORE the column is dropped, for the reason 00065 and 00068 gave: leaving it to
-- DROP COLUMN's cascade would make this migration depend on drop order rather than on
-- being right. Three arms now, one per surviving subject kind.
ALTER TABLE notifications DROP CONSTRAINT notifications_subject_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subject_ck CHECK (
     (subject_kind = 'alert'  AND alert_id IS NOT NULL AND subject_id = alert_id)
  OR (subject_kind = 'case'   AND case_id  IS NOT NULL AND subject_id = case_id)
  OR (subject_kind = 'digest' AND digest_window_start IS NOT NULL
      AND (policy_id IS NULL OR subject_id = policy_id)));

-- ⚠️ A CONSTRAINT COMMENT DIES WITH THE CONSTRAINT IT IS ON, so every DROP/ADD pair
-- in this file that carried one has to re-state it. This is the only one that did.
-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_subject_ck ON notifications IS
  'subject_id agrees with the id column subject_kind names, and that column is present. It stands in for the foreign key a three-table reference cannot have -- four until git-bug 7570090 dropped the alert_group arm with the table it named. The digest arm tolerates a NULL policy_id because policy_id is ON DELETE SET NULL: enforcing the tie unconditionally would make the first digest ever sent turn its own policy undeletable.';
-- +goose StatementEnd

-- ------------------------------------------------- notifications_convkind_ck
--
-- ⭐ A REPLACEMENT, NOT A RENAME, AND THE DIFFERENCE IS THE CARDINALITY. 00064 stored
-- today's identity unchanged so that this swap would be a value change rather than a
-- schema change, and it is -- but `alert_group` and `case` are not two names for one
-- thing. A generation held MANY cases; a conversation holds ONE. So there is no
-- `UPDATE ... SET conversation_kind = 'case'` here: the id in `conversation_id` is an
-- `alert_groups.id`, the new kind promises an `alert_cases.id`, and no mapping between
-- them exists that is not a guess about where a message landed. It fails loudly.
ALTER TABLE notifications DROP CONSTRAINT notifications_convkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_convkind_ck
  CHECK (conversation_kind IN ('case','digest'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.conversation_kind IS
  'WHERE this fact lands: which kind of conversation owns the thread. Mirrors subject_kind, and is deliberately a SEPARATE vocabulary -- a subject is what a fact is ABOUT, a conversation is where it is DELIVERED, and `alert` is a subject no conversation is keyed by. Bounded by notifications_convkind_ck. `alert_group` left this set in git-bug 7570090 and `case` REPLACED it rather than renaming it: a generation held many Cases and a conversation holds exactly one, which is the ruling the whole stage rests on.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.conversation_id IS
  'The conversation this fact lands in, in the table conversation_kind names: alert_cases.id for `case`, notification_policies.id for `digest`. No FK, because one column cannot reference two tables -- the same reason subject_id has none. This is now the ONLY delivery-target column: `group_id` was dropped with alert_groups in git-bug 7570090, and the three readers that used it for SUBJECT-shaped questions were answered rather than re-pointed -- the rollup''s second leg was a fan-out rule (a notification about the group counted as one about each member alert) and there is no container left for it to be about.';
-- +goose StatementEnd

-- ------------------------------------------------- the indexes on group_id
--
-- Each is dropped by NAME rather than left to the cascade, same rule as the
-- constraints above. `grp_*` and `alert_groups_synthetic_idx` are not listed: they are
-- indexes ON the dropped table and go with `DROP TABLE`.
--
-- ⭐ AND THE THROTTLE GETS ITS INDEX BACK. `notif_group_idx`'s own comment says it
-- serves "the policy throttle (windowed) and the group notification receipt", and
-- `countRecentSQL` now keys on `conversation_id` instead of `group_id`. Dropping the
-- index without the replacement would leave the throttle -- which runs on every
-- delivery decision -- on a scan. Same three columns, same order, one column swapped:
-- the equality the query supplies, then `created_at DESC` as the whole range, so the
-- window stops the scan and nothing is sorted.
DROP INDEX notif_group_idx;
CREATE INDEX notif_conversation_idx ON notifications (org_id, conversation_id, created_at DESC);

-- +goose StatementBegin
COMMENT ON INDEX notif_conversation_idx IS
  'Delivery target, not subject. Serves the policy throttle''s windowed count and the per-conversation notification receipt. Replaces notif_group_idx column-for-column (git-bug 7570090): the target used to be group_id and is now conversation_id, which is the only spelling left once alert_groups is gone. Two equalities and the whole range on created_at DESC, so a windowed count stops at the window rather than scanning the org.';
-- +goose StatementEnd

DROP INDEX case_group_idx;
DROP INDEX case_group_live_idx;

-- ------------------------------------------------- the columns and the table
--
-- Foreign keys first, by name, then the columns, then the table. `notifications`'
-- FK is 00011:207 (`ON DELETE CASCADE`) and `alert_cases`' is 00008:119
-- (`case_group_fk`, `ON DELETE SET NULL`, renamed from `occ_group_fk` by 00052).
-- `alert_group_members` -- 00008:94 and 00051:108 -- carried the third, and 00051
-- dropped that table, so there is nothing left there to name.
ALTER TABLE notifications DROP CONSTRAINT notifications_group_id_fkey;
ALTER TABLE notifications DROP COLUMN group_id;

ALTER TABLE alert_cases DROP CONSTRAINT case_group_fk;
ALTER TABLE alert_cases DROP COLUMN group_id;

DROP TABLE alert_groups;

-- ------------------------------------------------- the comments that survived it
--
-- ⛔ A COLUMN COMMENT IS NOT SOURCE CODE: it lives IN THE DATABASE, and only a
-- COMMENT ON statement changes what an operator's `\d+` prints. Four of them describe
-- the dropped table in the present tense. 00066 made exactly this argument one table
-- over -- correcting the source file changes nothing that is already deployed -- and a
-- live comment describing a table that no longer exists is a defect this project has
-- filed tickets about.
-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_kind IS
  'WHAT this conversation is keyed by: alert | case | digest. A signal thread is keyed by the CASE, which is the conversation: one Case, one thread, always (git-bug 7570090). A DIGEST thread is keyed by the POLICY, so a policy''s digests are one ongoing conversation with one reply per tick rather than a new thread every window -- which is the noise a digest exists to replace.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_id IS
  'The row this conversation is about, in the table subject_kind names: alert_cases.id for `case`, notification_policies.id for `digest`. It named an alert_groups GENERATION until git-bug 7570090 deleted the entity; a Case is the conversation now, and a re-fire that opens a new Case therefore opens a new thread -- the same property the generation used to provide, resting on a row that means something to an operator.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.subject_kind IS
  'WHAT this fact is about: alert | case | digest. It selects which id column subject_id must agree with (notifications_subject_ck), and it is hashed into idempotency_key. `alert_group` left this set when git-bug 7570090 deleted the entity. `digest` is the one value whose subject is not a row in the signal graph at all: it is a WINDOW OVER A NAMESPACE, spelled as the pair (policy_id, digest_window_start). Which Reason declares which subject is the domain allocation (internal/notification/domain/reason.go), deliberately NOT a CHECK here.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.subject_id IS
  'The row this fact is about, in the table subject_kind names: alerts.id, alert_cases.id, or -- for a digest -- notification_policies.id, which is the policy half of the (policy, window) pair. It could also be an alert_groups.id until git-bug 7570090 deleted that table. No FK, because one column cannot reference three tables; notifications_subject_ck ties it to alert_id, case_id or policy_id instead.';
-- +goose StatementEnd

-- ⛔ THE THIRD SPELLING OF THE SUBJECT AXIS, AND THE LAST ONE. `enrichments.subject_kind`
-- says `group` where `notifications`/`channel_threads` said `alert_group`; 00052 widened
-- it and 00056 records why the two vocabularies differ by a word. The value had NO
-- producer and NO reader in Go, so it is the dead-enum shape this project has closed
-- five tickets about, and it goes with the entity.
--
-- No UPDATE precedes it: nothing ever wrote `'group'`, so no stored row can fail the
-- narrower CHECK. That is asserted rather than assumed -- if any row did carry it, this
-- statement would fail loudly at migrate time rather than silently admitting a value the
-- validator now rejects.
-- +goose StatementBegin
ALTER TABLE enrichments DROP CONSTRAINT enrichments_subjkind_ck;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE enrichments ADD CONSTRAINT enrichments_subjkind_ck
  CHECK (subject_kind IN ('alert','case'));
-- +goose StatementEnd

-- +goose Down

-- ⭐ THE SHAPE, NOT THE ROWS. Everything below restores the schema 00068 shipped so
-- that a rollback past this migration resolves -- 00066's Down comments on
-- `alert_groups.last_activity_at`, 00059's re-adds `storm_mode` and `storm_since` to
-- it, 00051's rebuilds `alert_group_members` with a foreign key into it. The table
-- comes back EMPTY and every restored `group_id` comes back NULL. See the ⛔ block in
-- the header: the Up dropped the rows and no statement here records what they held.
--
-- ⚠️ TWO OF THESE RE-ADDS ARE THEMSELVES NARROWINGS AND CAN FAIL, which is the honest
-- risk of a rollback past a replacement. `notifications_convkind_ck` goes back to
-- `('alert_group','digest')`, so a row written under 00069 holding `case` refuses it;
-- and `notifications_subject_ck`'s restored `alert_group` arm demands
-- `group_id IS NOT NULL`, which the restored column never is. The second is safe
-- because 00069 also narrowed `notifications_subjkind_ck`, so no row can spell
-- `alert_group` any more -- the arm comes back unsatisfiable but unviolated. The first
-- is not safe and is not made safe: rewriting a Case conversation into a group one
-- would invent the generation the header says cannot be invented.

-- ------------------------------------------------- alert_groups
--
-- Byte-equivalent to the state 00068 left: 00008's twenty-seven columns, minus
-- `storm_mode` and `storm_since` (00059) and their `groups_storm_ck`, plus `synthetic`
-- (00039). The comments are 00050's rewrite with 00066's correction to
-- `last_activity_at`, which is the text that was live at 00068.
--
-- ⛔ AND `groups_axes_ck` IS DELIBERATELY ABSENT, in this direction as in every other.
-- 00050 argued it out: a CHECK requiring `alertname` in `group_labels` cannot be added
-- over pre-00050 rows without making them permanently un-UPDATE-able, so the presence
-- of the axis is an invariant of the writer and is not enforced in SQL. Adding it here
-- would restore a constraint that has never existed.
CREATE TABLE alert_groups (
  id                  UUID        PRIMARY KEY,
  org_id              UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  source_id           UUID        NOT NULL REFERENCES alert_sources(id) ON DELETE CASCADE,
  cluster_id          UUID        NOT NULL REFERENCES clusters(id),
  group_key           TEXT        NOT NULL,
  generation          INT         NOT NULL DEFAULT 1,
  source_group_key    TEXT,
  receiver            TEXT        NOT NULL DEFAULT '',
  group_labels        JSONB       NOT NULL DEFAULT '{}'::jsonb,
  title               TEXT        NOT NULL,

  state               TEXT        NOT NULL,
  severity            TEXT,
  state_version       INT         NOT NULL DEFAULT 1,

  firing_count        INT         NOT NULL DEFAULT 0,
  suppressed_count    INT         NOT NULL DEFAULT 0,
  resolved_count      INT         NOT NULL DEFAULT 0,
  expired_count       INT         NOT NULL DEFAULT 0,
  total_count         INT         NOT NULL DEFAULT 0,
  acked_count         INT         NOT NULL DEFAULT 0,

  last_notification_reason TEXT,

  first_seen_at       TIMESTAMPTZ NOT NULL,
  last_activity_at    TIMESTAMPTZ NOT NULL,
  closed_at           TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  synthetic           BOOLEAN     NOT NULL DEFAULT false,
  CONSTRAINT groups_key_gen_uniq UNIQUE (org_id, group_key, generation),
  CONSTRAINT groups_state_ck  CHECK (state IN ('open','closed')),
  CONSTRAINT groups_key_ck    CHECK (group_key ~ '^gk_[0-9a-v]{26}$'),
  CONSTRAINT groups_gen_ck    CHECK (generation >= 1),
  CONSTRAINT groups_title_ck  CHECK (length(btrim(title)) BETWEEN 1 AND 500),
  CONSTRAINT groups_labels_ck CHECK (jsonb_typeof(group_labels) = 'object'),
  CONSTRAINT groups_sver_ck   CHECK (state_version >= 1),
  CONSTRAINT groups_counts_ck CHECK (firing_count >= 0 AND suppressed_count >= 0 AND resolved_count >= 0
                                     AND expired_count >= 0 AND total_count >= 0 AND acked_count >= 0),
  CONSTRAINT groups_acked_ck  CHECK (acked_count <= total_count),
  CONSTRAINT groups_closed_ck CHECK ((state = 'closed') = (closed_at IS NOT NULL)),
  CONSTRAINT groups_corder_ck CHECK (closed_at IS NULL OR closed_at >= first_seen_at),
  CONSTRAINT groups_act_ck    CHECK (last_activity_at >= first_seen_at),
  CONSTRAINT groups_time_ck   CHECK (updated_at >= created_at)
);

CREATE INDEX grp_list_idx ON alert_groups (org_id, state, last_activity_at DESC, id DESC);
CREATE INDEX grp_open_idx ON alert_groups (org_id, group_key) WHERE state = 'open';
CREATE INDEX grp_close_idx ON alert_groups (org_id, last_activity_at) WHERE state = 'open';
CREATE INDEX grp_first_seen_idx ON alert_groups (org_id, first_seen_at DESC, id DESC);
CREATE INDEX alert_groups_synthetic_idx ON alert_groups (org_id, id) WHERE synthetic;

COMMENT ON INDEX grp_first_seen_idx IS 'Keyset sort of alert groups by first_seen_at descending.';

-- +goose StatementBegin
COMMENT ON TABLE alert_groups IS
  'ONE GENERATION of one MACHINE-DERIVED grouping of alerts, keyed by (org, cluster, alertname, namespace-or-empty) — the alert own labels, never Alertmanager grouping (ADR 0038). OWNS EXACTLY ONE Slack thread. A closed group that re-opens gets a new generation and therefore a new thread. Never means a UI grouping -- that is a view (SPEC §A.1).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.group_key IS
  'gk_ + 26 base32hex chars over sha256(org_id, cluster_key, canon(alertname, namespace-or-empty)) (SPEC §C.4, ADR 0038). FIXED, NOT CONFIGURABLE: a tunable split key reinvents group_by inside oto and re-inherits the problem it was built to escape. Computed identically on the webhook path and the reconciler path, which is what makes the two agree about which thread an alert belongs to.';
-- +goose StatementEnd

COMMENT ON COLUMN alert_groups.generation IS 'Increments when a closed group re-opens. Part of the unique key, and the reason a re-opened group starts a fresh Slack thread.';

-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.source_group_key IS
  'Alertmanager raw groupKey, kept verbatim for observability. OPAQUE -- MUST NOT BE PARSED: it is unescaped and unbounded (SPEC §C.4). Since ADR 0038 it is an input to nothing at all; the full envelope it came from is in ingest_batches.payload.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.receiver IS
  'PROVENANCE ONLY since ADR 0038: the Alertmanager receiver that first delivered into this generation, empty for a reconciler-sourced one. It is NOT part of group_key. While it was, one alert routed to two receivers by continue:true occupied two groups and two threads at once. Removing it MERGES routes that deliberately separated the same alerts; cluster_key is what must distinguish them, which it should be anyway since alert identity is already (org, cluster).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.group_labels IS
  'oto OWN split axes for this generation: alertname, plus namespace when the alert has one. An ABSENT namespace is its own partition and is recorded as the ABSENCE of the key, never as an empty string. THE PRESENCE OF alertname IS AN INVARIANT OF THE WRITER AND IS NOT ENFORCED IN SQL: kernel.SplitLabels is total over a LabelSet that already refuses an empty alertname, and a CHECK cannot be added over pre-00050 rows without making them permanently un-UPDATE-able -- see this migration header. A row with no alertname is a generation opened before ADR 0038. EVERY NOTIFICATION MATCHER IS FED THIS MAP, which is why it stopped being Alertmanager groupLabels: a policy matching namespace used to match nothing unless the operator had put namespace in group_by, and it failed quietly as a no_policy suppression rather than as an error.';
-- +goose StatementEnd

COMMENT ON COLUMN alert_groups.title IS 'Pre-rendered group title for the Slack card and the UI.';
COMMENT ON COLUMN alert_groups.severity IS 'Highest member severity, recomputed on membership change. Drives the card emoji.';

-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.state_version IS
  'Increments on every MATERIAL group change. Hashed into notification.idempotency_key (SPEC §C.7), which is what makes "all_resolved at state_version 7" exist exactly once.';
-- +goose StatementEnd

COMMENT ON COLUMN alert_groups.acked_count IS 'Members acknowledged. Constrained <= total_count by groups_acked_ck.';
COMMENT ON COLUMN alert_groups.last_notification_reason IS 'The most recent Alertmanager notification_reason seen for this group. Feeds the §H.6 reason-to-mode decision table.';

-- The 00066 text, not 00008's: `last_activity_at` stopped promising a freeze when
-- e5c060b deleted the state, and 00068 is the version this Down rolls back TO.
-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.last_activity_at IS
  'Drives group.close. Idle past orgs.settings.group_close_delay_s closes the group. ⛔ IT DOES NOT FREEZE THE THREAD -- this comment said so until git-bug e5c060b, and nothing ever froze anything: `Freeze` had no production caller and migration 00066 deleted the state. What keeps a re-fire off the closed generation''s card is that the next observation opens generation N+1 and a new generation is a NEW thread, because channel_threads.subject_id IS the generation row. The tuning rule that rests on this -- keep group_close_delay_s at or above refire_grace -- is unchanged and still right; only the reason given for it was wrong.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alert_groups.synthetic IS
  'Denormalised from the observations that opened this generation, so the dashboard group counts can exclude drills with an indexed predicate instead of a nested loop through alert_group_members. Written once, at generation-open time; never read back onto the domain entity, because it is a reporting fact and not a group invariant.';
-- +goose StatementEnd

-- ------------------------------------------------- alert_cases.group_id
--
-- Nullable, as 00007 declared it: NULL means the group key could not be computed and
-- the signal was recorded groupless, which is the deliberate degradation in the ingest
-- orchestrator. Every restored row is NULL, which the release this rolls back to reads
-- as exactly that -- see the ⛔ block in the header.
ALTER TABLE alert_cases ADD COLUMN group_id UUID;

ALTER TABLE alert_cases ADD CONSTRAINT case_group_fk
  FOREIGN KEY (group_id) REFERENCES alert_groups(id) ON DELETE SET NULL;

CREATE INDEX case_group_idx ON alert_cases (org_id, group_id, started_at DESC);
CREATE INDEX case_group_live_idx ON alert_cases (org_id, group_id, started_at DESC, id DESC)
  WHERE ended_at IS NULL;

-- +goose StatementBegin
COMMENT ON COLUMN alert_cases.group_id IS
  'THE MEMBERSHIP. Which AlertGroup generation this episode belongs to, and the only record of it since alert_group_members was dropped. Written once, when the episode opens, and never moved: the group key is derived from the alert own labels (ADR 0038) and alert identity IS its label set, so an episode split key is fixed for its whole life. NULL means the group key could not be computed and the signal was recorded groupless, which is the deliberate degradation in the ingest orchestrator. started_at is when it joined and ended_at is when it left; membership is still history, it is just the episode own history now.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON INDEX case_group_live_idx IS
  'Serves listCurrentMembersSQL -- the keyset page behind GET /alert-groups/{id}/alerts, the twenty-row top_alerts preview behind GET /alert-groups/{id} which the ack, snooze and unsnooze replies also render, the fan-out candidate read and the current-member count. It carries both equalities, the partial predicate and the whole sort key, so the LIMIT stops the scan and no Sort node appears. PARTIAL on ended_at IS NULL so it stays the size of the LIVE membership rather than of the generation history: the replay reads (allMembersSQL, membersAtSQL) and the rollup want the ended episodes back and are meant to ride case_group_idx instead.';
-- +goose StatementEnd

-- ------------------------------------------------- notifications.group_id
--
-- NULLABLE, which is the state 00058 left it in: it was `NOT NULL` from 00011 until
-- 00058:276 dropped that so a digest could omit a thread. Restoring it `NOT NULL`
-- would be restoring a schema no release ever shipped.
ALTER TABLE notifications ADD COLUMN group_id UUID;

ALTER TABLE notifications ADD CONSTRAINT notifications_group_id_fkey
  FOREIGN KEY (group_id) REFERENCES alert_groups(id) ON DELETE CASCADE;

DROP INDEX notif_conversation_idx;
CREATE INDEX notif_group_idx ON notifications (org_id, group_id, created_at DESC);

-- +goose StatementBegin
COMMENT ON INDEX notif_group_idx IS
  'Delivery target, not subject. Serves the policy throttle (windowed) and the group notification receipt (unbounded in time). Both were subject-keyed until subject_kind admitted more than one value; keep this index if either query survives.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.group_id IS
  'THE DELIVERY TARGET, not the subject: which AlertGroup generation thread this fact lands on. MANDATORY for every subject_kind except `digest` (notifications_target_ck) -- a fact that could name a destination is not allowed to omit one. It is NULL exactly for a digest, which spans many generations and therefore has no single thread to land in; a digest opens its own conversation, keyed by its policy. Ask subject_kind and subject_id what the fact is ABOUT.';
-- +goose StatementEnd

-- ------------------------------------------------- the vocabularies, re-widened
--
-- ⚠️ `notifications_target_ck` IS NOT RE-ADDED HERE. 00064 owns it in both
-- directions: its Up dropped it and its Down restores it. Adding it in this file too
-- would leave a duplicate name after a rollback past 00064.
ALTER TABLE notifications DROP CONSTRAINT notifications_subjkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subjkind_ck  -- vocab:allow -- the Down must name the value it restores, or it cannot restore it.
  CHECK (subject_kind IN ('alert','case','alert_group','digest'));

ALTER TABLE channel_threads DROP CONSTRAINT threads_subjkind_ck;
ALTER TABLE channel_threads ADD  CONSTRAINT threads_subjkind_ck  -- vocab:allow -- the Down must name the value it restores, or it cannot restore it.
  CHECK (subject_kind IN ('alert','case','alert_group','digest'));

ALTER TABLE notifications DROP CONSTRAINT notifications_subject_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subject_ck CHECK (
     (subject_kind = 'alert'       AND alert_id IS NOT NULL AND subject_id = alert_id)
  OR (subject_kind = 'case'        AND case_id  IS NOT NULL AND subject_id = case_id)
  OR (subject_kind = 'alert_group' AND group_id IS NOT NULL AND subject_id = group_id)
  OR (subject_kind = 'digest'      AND digest_window_start IS NOT NULL
      AND (policy_id IS NULL OR subject_id = policy_id)));

-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_subject_ck ON notifications IS
  'subject_id agrees with the id column subject_kind names, and that column is present. It stands in for the foreign key a four-table reference cannot have. The digest arm tolerates a NULL policy_id because policy_id is ON DELETE SET NULL: enforcing the tie unconditionally would make the first digest ever sent turn its own policy undeletable.';
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_convkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_convkind_ck  -- vocab:allow -- the Down must name the value it restores, or it cannot restore it.
  CHECK (conversation_kind IN ('alert_group','digest'));

ALTER TABLE notification_deliveries DROP CONSTRAINT deliveries_mode_ck;
ALTER TABLE notification_deliveries ADD CONSTRAINT deliveries_mode_ck
  CHECK (mode IN ('post_root','update_root','thread_reply','broadcast_reply'));

-- +goose StatementBegin
COMMENT ON COLUMN notification_deliveries.mode IS
  'post_root | update_root | thread_reply | broadcast_reply. chat.update in place is PRIMARY; thread replies are the exception. repeat interval elapsed means UPDATE ONLY, NEVER POST (CONTEXT.md §6).';
-- +goose StatementEnd

-- ⛔ THERE IS DELIBERATELY NO MIRROR OF THE POLICY STRIP, the position 00060 and 00067
-- each took in this same place. The Up removed `new_alerts` and `some_resolved` from
-- every `notification_policies.reasons` array and deleted the policies whose only
-- reasons were those two. Neither is recoverable: the array no longer records that the
-- values were present, so a mirroring UPDATE would have to guess WHICH policies to
-- re-subscribe, and adding them back to all of them -- or to none -- would both be
-- inventions. A deleted policy is gone with its name, its matchers and its channel
-- list. This half restores only the VOCABULARY the constraints admit.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','new_alerts','some_resolved','all_resolved','repeat','suppressed','unsuppressed',
   'expired','refired','acked','unacked','snoozed','unsnoozed','enriched','rule_changed',
   'comment','digest'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, seventeen values. Together with the channel verbosity it decides update-in-place versus thread reply. The storm announcement went with the damper it announced (ADR 0042). The unacked reminder went because oto sends nothing unprompted (git-bug bd0fb1d): there is no fact here that nobody asked for.';
-- +goose StatementEnd

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 17
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..17 SPEC H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: cardinality, NULL-freeness and set-ness are all it tests, so a removed Reason left in this array is invisible to the database and surfaces as a validator failure on the Policies page instead. Every narrowing of the Reason vocabulary must strip the value from this column by hand, the way 00060 and 00067 do -- the constraint will not do it for you.';
-- +goose StatementEnd

-- ------------------------------------------------- the comments, as 00068 had them
-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_kind IS
  'WHAT this conversation is keyed by: alert | case | alert_group | digest. A signal thread is keyed by the AlertGroup GENERATION, so forty alerts still produce one thread. A DIGEST thread is keyed by the POLICY, so a policy''s digests are one ongoing conversation with one reply per tick rather than a new thread every window -- which is the noise a digest exists to replace.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN channel_threads.subject_id IS
  'The row this conversation is about, in the table subject_kind names. For alert_group it is one GENERATION: a re-opened group is a new generation and therefore a new thread.';
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
COMMENT ON COLUMN notifications.conversation_kind IS
  'WHERE this fact lands: which kind of conversation owns the thread. Mirrors subject_kind, and is deliberately a SEPARATE vocabulary -- a subject is what a fact is ABOUT, a conversation is where it is DELIVERED, and `alert` and `case` are subjects no conversation is keyed by. Bounded by notifications_convkind_ck. This pair replaced `group_id` plus notifications_target_ck, whose shape made a digest the one exception in a CHECK and could not have absorbed a third kind (git-bug 7570090).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notifications.conversation_id IS
  'The conversation this fact lands in, in the table conversation_kind names: alert_groups.id for `alert_group`, notification_policies.id for `digest`. No FK, because one column cannot reference two tables -- the same reason subject_id has none. ⛔ NOT the same question as group_id, which several readers use to answer SUBJECT-shaped questions (the per-alert rollup, the drill artifact read, the audit filter) and which survives until stage 4 answers each of them deliberately.';
-- +goose StatementEnd


-- ⛔ RE-WIDEN `enrichments_subjkind_ck` to 00052's three values. `'group'` becomes
-- admissible again so a rollback past this migration leaves the CHECK exactly as
-- 00068 shipped it. No row carries it, so nothing is restored by admitting it -- the
-- constraint text is the whole of what is being put back.
-- +goose StatementBegin
ALTER TABLE enrichments DROP CONSTRAINT enrichments_subjkind_ck;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE enrichments ADD CONSTRAINT enrichments_subjkind_ck
  CHECK (subject_kind IN ('alert','case','group'));
-- +goose StatementEnd
