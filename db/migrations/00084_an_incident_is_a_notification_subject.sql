-- ADR 0052 §5: DECLARING AN INCIDENT IS A NOTIFICATION (git-bug aa6d18b).
--
-- `subject_kinds` gains `incident`, so "every Incident goes to incident.io" is ONE
-- catch-all policy routed to a webhook — and retries, idempotency, the delivery
-- audit and drills come with it, because an Incident fact becomes an ordinary
-- `notifications` row with ordinary `notification_deliveries`. An org with no such
-- policy has Incidents in oto and sends nothing.
--
-- ⭐ FIVE FACTS, AND NOT ONE OF THEM IS A COMMAND. `drawn`, `case_added`,
-- `case_removed`, `quiet` and `active_again` are things oto OBSERVED about a grouping
-- of signals. There is no `resolved`, no `closed`, no `mitigated`: oto never sends
-- an incident tool a resolve or a status change (§5) — the incident tool does not
-- resolve oto's alert, and oto does not resolve the incident tool's incident. `quiet`
-- says every member Case has closed; whether the response is over is the other
-- tool's fact, and an org that wants auto-resolve writes it there, keyed on this.
--
-- ⭐ AN INCIDENT FACT NAMES NO ALERT AND NO CASE. Its `subject_id` is the Incident,
-- and the subject arm below holds it to exactly that: no `alert_id`, no `case_id`,
-- no digest window. There is no `incident_id` column on `notifications` and there
-- must never be one (SCOPE-BOUNDARY §5.6) — `subject_kind = 'incident'` IS the
-- reference, the same way `subject_kind = 'digest'` names a policy without a
-- policy-shaped column of its own. With neither an alert nor a case on the row, the
-- per-signal timeline (`alert_events`) carries no `notification.*`/`delivery.*`
-- line for it, exactly as for a digest; the member Cases already carry the
-- `incident.case_*` facts from 00083's verbs.
--
-- ⚠️ THE CONVERSATION IS THE INCIDENT, AND `threads_subjkind_ck` IS DELIBERATELY NOT
-- TOUCHED. `notifications.conversation_kind` is NOT NULL (00064), so an Incident fact
-- has to say where it lands, and the honest answer is "in the Incident's own
-- conversation". On a destination that keeps no thread — the generic webhook — that
-- is simply the destination. A THREADED destination would need a `channel_threads`
-- row keyed by the Incident, and an Incident becomes a thread only when a Correlator
-- says its Incidents are conversations (§6), which is its own ticket; so the fan-out
-- records such a delivery as SKIPPED with that sentence instead of opening a thread
-- the schema refuses.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Every change is a WIDENING: a release-N row
-- satisfies each new CHECK, and a release-N writer cannot produce `incident`.

-- +goose Up

ALTER TABLE notification_policies DROP CONSTRAINT policies_subjkinds_ck;
ALTER TABLE notification_policies ADD  CONSTRAINT policies_subjkinds_ck
  CHECK (subject_kinds <@ ARRAY['alert','case','digest','incident']::text[]
         AND array_position(subject_kinds, NULL) IS NULL
         AND oto_array_is_set(subject_kinds));

ALTER TABLE notifications DROP CONSTRAINT notifications_subjkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subjkind_ck
  CHECK (subject_kind IN ('alert','case','digest','incident'));

-- The fourth arm. An Incident fact names the Incident and nothing else: the typed id
-- columns are NULL, so no reader that counts notifications per alert or per case —
-- the delivery roll-up, the case card's own notification list, stats — can mistake a
-- fact about a story for a fact about one of its signals.
ALTER TABLE notifications DROP CONSTRAINT notifications_subject_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subject_ck CHECK (
     (subject_kind = 'alert'    AND alert_id IS NOT NULL AND subject_id = alert_id)
  OR (subject_kind = 'case'     AND case_id  IS NOT NULL AND subject_id = case_id)
  OR (subject_kind = 'digest'   AND digest_window_start IS NOT NULL
      AND (policy_id IS NULL OR subject_id = policy_id))
  OR (subject_kind = 'incident' AND alert_id IS NULL AND case_id IS NULL
      AND digest_window_start IS NULL));

-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_subject_ck ON notifications IS
  'subject_id agrees with the id column subject_kind names, and that column is present. It stands in for the foreign key a multi-table reference cannot have. The digest arm tolerates a NULL policy_id because policy_id is ON DELETE SET NULL. The incident arm (00084) names NO typed column at all: subject_id is the incidents.id, and alert_id, case_id and the digest window are NULL, because an Incident fact is about the story and never about one of its signals -- and this table has no column naming an Incident, by SCOPE-BOUNDARY §5.6.';
-- +goose StatementEnd

-- An Incident fact lands in the Incident's own conversation. See the header for why
-- `threads_subjkind_ck` is left exactly as it is.
ALTER TABLE notifications DROP CONSTRAINT notifications_convkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_convkind_ck
  CHECK (conversation_kind IN ('case','digest','incident'));

-- The five Incident Reasons, APPENDED after `digest` in the order
-- `notification/domain.allReasons` declares them, which the contract enum mirrors.
ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','all_resolved','repeat','suppressed','unsuppressed','expired','refired',
   'acked','unacked','snoozed','unsnoozed','enriched','rule_changed','comment','digest',
   'drawn','case_added','case_removed','quiet','active_again'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, twenty values. Fifteen are facts about one signal or one window; the last five (00084) are facts about an Incident -- drawn, case_added, case_removed, quiet, active_again -- and none of them is a resolve, a close or a status: oto declares facts to an incident tool and never commands it (ADR 0052 §5). `quiet` is every member Case closed, never "the incident is over".';
-- +goose StatementEnd

-- The ceiling is the enum size, as it has been since 00046: 15 at 00069, 20 now.
ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD  CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 20
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..20 SPEC H.6 Reason values. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder, 00069 removed new_alerts and some_resolved, 00084 added the five Incident facts. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: every narrowing of the Reason vocabulary must strip the value from this column by hand.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ THE INCIDENT NOTIFICATIONS GO, BECAUSE NO ROLLED-BACK CHECK CAN HOLD THEM. Each
-- is a fact about an Incident that release N-1 has no word for; its deliveries are
-- removed with it by notification_deliveries' ON DELETE CASCADE.
DELETE FROM notifications WHERE subject_kind = 'incident';

-- A policy bound to `incident` ALONE goes, rather than being widened: stripping its
-- only kind would leave `{}`, which means EVERY kind, and a rollback must never make
-- a policy route more than it did. A policy that also named another kind keeps it.
DELETE FROM notification_policies WHERE subject_kinds = ARRAY['incident']::text[];
UPDATE notification_policies
   SET subject_kinds = array_remove(subject_kinds, 'incident')
 WHERE 'incident' = ANY(subject_kinds);

-- The same rule for the five Reasons, 00069's shape: a policy that reacted to
-- nothing else goes, and one that reacted to more keeps the rest.
DELETE FROM notification_policies
 WHERE reasons <@ ARRAY['drawn','case_added','case_removed','quiet','active_again']::text[];
UPDATE notification_policies
   SET reasons = array_remove(array_remove(array_remove(array_remove(array_remove(
                   reasons, 'drawn'), 'case_added'), 'case_removed'), 'quiet'), 'active_again')
 WHERE reasons && ARRAY['drawn','case_added','case_removed','quiet','active_again']::text[];

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD  CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 15
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..15 SPEC H.6 Reason values. Uniqueness is enforced here as well as in the DTO tag and the domain constructor because the contract publishes uniqueItems on the RESPONSE: a duplicate reaching this column comes back on a read as a row the generated frontend client refuses. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder, 00069 removed new_alerts and some_resolved. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: cardinality, NULL-freeness and set-ness are all it tests, so a removed Reason left in this array is invisible to the database and surfaces as a validator failure on the Policies page instead. Every narrowing of the Reason vocabulary must strip the value from this column by hand, the way 00060, 00067 and 00069 do -- the constraint will not do it for you.';
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','all_resolved','repeat','suppressed','unsuppressed','expired','refired',
   'acked','unacked','snoozed','unsnoozed','enriched','rule_changed','comment','digest'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, fifteen values. Together with the channel verbosity it decides update-in-place versus thread reply. The storm announcement went with the damper it announced (ADR 0042). The unacked reminder went because oto sends nothing unprompted (git-bug bd0fb1d). `new_alerts` and `some_resolved` went with the container they counted (git-bug 7570090): both assert a plurality inside one conversation, and a conversation holds exactly one Case, so there is no part and no rest for either to name. `all_resolved` stays -- a Case resolving is a fact about the Case and lands in the Case''s own conversation. `refired` stays DECLARED and still has no producer (ADR 0040 retired T8); that is a separate decision, not an oversight here.';
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_convkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_convkind_ck
  CHECK (conversation_kind IN ('case','digest'));

ALTER TABLE notifications DROP CONSTRAINT notifications_subject_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subject_ck CHECK (
     (subject_kind = 'alert'  AND alert_id IS NOT NULL AND subject_id = alert_id)
  OR (subject_kind = 'case'   AND case_id  IS NOT NULL AND subject_id = case_id)
  OR (subject_kind = 'digest' AND digest_window_start IS NOT NULL
      AND (policy_id IS NULL OR subject_id = policy_id)));

-- +goose StatementBegin
COMMENT ON CONSTRAINT notifications_subject_ck ON notifications IS
  'subject_id agrees with the id column subject_kind names, and that column is present. It stands in for the foreign key a three-table reference cannot have -- four until git-bug 7570090 dropped the alert_group arm with the table it named. The digest arm tolerates a NULL policy_id because policy_id is ON DELETE SET NULL: enforcing the tie unconditionally would make the first digest ever sent turn its own policy undeletable.';
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_subjkind_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_subjkind_ck
  CHECK (subject_kind IN ('alert','case','digest'));

ALTER TABLE notification_policies DROP CONSTRAINT policies_subjkinds_ck;
ALTER TABLE notification_policies ADD  CONSTRAINT policies_subjkinds_ck
  CHECK (subject_kinds <@ ARRAY['alert','case','digest']::text[]
         AND array_position(subject_kinds, NULL) IS NULL
         AND oto_array_is_set(subject_kinds));
