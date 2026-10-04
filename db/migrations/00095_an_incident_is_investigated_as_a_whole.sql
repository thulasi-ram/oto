-- ADR 0053 §4, ADR 0052 §5: AN INCIDENT IS INVESTIGATED AS A WHOLE (git-bug 74ea849).
-- "Subjects are case | incident | digest | policy … Triggers: an Incident is drawn; its
-- membership changes (with a minimum interval on the Investigator); a human asks …
-- Not on quiet. A Case already in an Incident gets no Investigation of its own
-- automatically — the Incident's covers it — so a forty-Case storm costs a handful of
-- runs, not forty." Five changes, each the other half of one already on the record:
--
--   1. `investigations_subjkind_ck` admits `incident`. 00092 shaped the column for four
--      subjects and admitted one; this is the widening its header promised.
--   2. `enrichments_subjkind_ck` admits `incident`. ⚠️ THE TICKET SAID THIS CHECK STILL
--      ADMITTED `group` (00069:640). That line is 00069's DOWN; its Up narrowed the CHECK
--      to `('alert','case')` at 00069:342, and that is what this migration widens — and
--      what its Down restores, byte for byte. An Incident's Finding is stored as the
--      Enrichment `investigator.<name>` on the Incident, exactly as a Case's is on the Case.
--   3. `investigators.investigates_incidents`. WHICH Investigator an Incident starts is an
--      operator's word, not oto's: a flag on the mutable half (beside `enabled` and
--      `min_interval_s`), default FALSE, so an org whose Investigators were written for
--      "a human asks" starts paying for automatic runs only when somebody says so. It is
--      not versioned — it decides when a run starts, not what produced a Finding.
--   4. `notifications_reason_ck` gains `finding`, appended after `active_again`: the
--      sixth Incident fact, "a new Finding" (ADR 0052 §5). Like the other five it is a
--      FACT, never a command — a Finding never decides whether anyone is told (ADR 0053
--      §2); it is declared to whatever the org routes Incident facts to.
--   5. `policies_reasons_ck`'s ceiling follows the enum to 21, as it has since 00046.
--
-- ⛔ NOTHING HERE PUTS AN INVESTIGATION ON THE NOTIFICATION PATH OR ON THE INCIDENT'S
-- WRITE. The trigger is a job (`investigations.incident`, on `lifecycle`) enqueued in the
-- membership change's own transaction; the run is `investigations.run` on `investigate`.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Three widened CHECKs, one widened ceiling, one column
-- with a constant default release N-1 does not name, and comments. A release-N-1 writer
-- produces none of the new values.

-- +goose Up

ALTER TABLE investigations DROP CONSTRAINT investigations_subjkind_ck;
ALTER TABLE investigations ADD  CONSTRAINT investigations_subjkind_ck
  CHECK (subject_kind IN ('case','incident'));

ALTER TABLE enrichments DROP CONSTRAINT enrichments_subjkind_ck;
ALTER TABLE enrichments ADD  CONSTRAINT enrichments_subjkind_ck
  CHECK (subject_kind IN ('alert','case','incident'));

ALTER TABLE investigators
  ADD COLUMN investigates_incidents BOOLEAN NOT NULL DEFAULT false;

-- +goose StatementBegin
COMMENT ON COLUMN investigators.investigates_incidents IS
  'ADR 0053 §4: when true, an Incident being drawn starts one Investigation by this Investigator, and its membership changing starts another under min_interval_s (coalesced). Going quiet starts nothing. Default false: automatic runs cost money, so an operator opts an Investigator in. A human may ask any Investigator about any Incident regardless. Not versioned: it decides when a run starts, not what produced a Finding.';
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','all_resolved','repeat','suppressed','unsuppressed','expired','refired',
   'acked','unacked','snoozed','unsnoozed','enriched','rule_changed','comment','digest',
   'drawn','case_added','case_removed','quiet','active_again','finding'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, twenty-one values. Fifteen are facts about one signal or one window; the last six are facts about an Incident -- drawn, case_added, case_removed, quiet, active_again (00084) and finding (00095: an Investigation of the Incident reached a new Finding) -- and none of them is a resolve, a close or a status: oto declares facts to an incident tool and never commands it (ADR 0052 §5). `quiet` is every member Case closed, never "the incident is over". A Finding never decides whether anyone is told (ADR 0053 §2); `finding` declares it to wherever the org routes Incident facts.';
-- +goose StatementEnd

ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD  CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 21
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..21 SPEC H.6 Reason values. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder, 00069 removed new_alerts and some_resolved, 00084 added the five Incident facts, 00095 added finding. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: every narrowing of the Reason vocabulary must strip the value from this column by hand.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ THE `finding` NOTIFICATIONS GO, BECAUSE NO ROLLED-BACK CHECK CAN HOLD THEM; their
-- deliveries go with them by notification_deliveries' ON DELETE CASCADE. A policy that
-- reacted to `finding` alone goes rather than being emptied (an empty reason set is not
-- storable, and widening it to anything would route more than it did); one that also
-- named another reason keeps the rest. 00084's shape.
DELETE FROM notifications WHERE reason = 'finding';
DELETE FROM notification_policies WHERE reasons = ARRAY['finding']::text[];
UPDATE notification_policies
   SET reasons = array_remove(reasons, 'finding')
 WHERE 'finding' = ANY(reasons);

-- Byte-identical to 00084's Up.
ALTER TABLE notification_policies DROP CONSTRAINT policies_reasons_ck;
ALTER TABLE notification_policies ADD  CONSTRAINT policies_reasons_ck
  CHECK (cardinality(reasons) BETWEEN 1 AND 20
         AND array_position(reasons, NULL) IS NULL
         AND oto_array_is_set(reasons));

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_reasons_ck ON notification_policies IS
  'reasons is a set of 1..20 SPEC H.6 Reason values. The ceiling is the enum size and moves with it -- 00058 added digest, 00060 removed storm, 00067 removed unacked_reminder, 00069 removed new_alerts and some_resolved, 00084 added the five Incident facts. ⛔ IT DOES NOT CONSTRAIN MEMBERSHIP: every narrowing of the Reason vocabulary must strip the value from this column by hand.';
-- +goose StatementEnd

ALTER TABLE notifications DROP CONSTRAINT notifications_reason_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_reason_ck CHECK (reason IN
  ('fired','all_resolved','repeat','suppressed','unsuppressed','expired','refired',
   'acked','unacked','snoozed','unsnoozed','enriched','rule_changed','comment','digest',
   'drawn','case_added','case_removed','quiet','active_again'));

-- +goose StatementBegin
COMMENT ON COLUMN notifications.reason IS
  'The SPEC H.6 Reason enum, twenty values. Fifteen are facts about one signal or one window; the last five (00084) are facts about an Incident -- drawn, case_added, case_removed, quiet, active_again -- and none of them is a resolve, a close or a status: oto declares facts to an incident tool and never commands it (ADR 0052 §5). `quiet` is every member Case closed, never "the incident is over".';
-- +goose StatementEnd

ALTER TABLE investigators DROP COLUMN investigates_incidents;

-- ⛔ AN INCIDENT'S FINDING HAS NO HOME BELOW THIS MIGRATION, AND IT IS DELETED RATHER
-- THAN RELABELLED: an Enrichment re-pointed at a Case would claim a Finding about the
-- story was a Finding about one of its signals. Every run's own Finding is on its
-- Investigation row, which goes next.
DELETE FROM enrichments WHERE subject_kind = 'incident';

-- Byte-identical to 00069's Up.
ALTER TABLE enrichments DROP CONSTRAINT enrichments_subjkind_ck;
ALTER TABLE enrichments ADD  CONSTRAINT enrichments_subjkind_ck
  CHECK (subject_kind IN ('alert','case'));

-- ⛔ THE INCIDENT'S INVESTIGATIONS GO WITH THE SUBJECT THE RELEASE BELOW CANNOT NAME.
-- `investigations_frozen` guards UPDATE, not DELETE, and their Steps go by the cascade
-- `investigation_steps_append_only` admits — the record going with its owner.
DELETE FROM investigations WHERE subject_kind = 'incident';

-- Byte-identical to 00092's Up.
ALTER TABLE investigations DROP CONSTRAINT investigations_subjkind_ck;
ALTER TABLE investigations ADD  CONSTRAINT investigations_subjkind_ck
  CHECK (subject_kind IN ('case'));
