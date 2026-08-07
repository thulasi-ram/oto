-- SPEC §G.9.1, SCOPE-BOUNDARY §5.2 (SPEC §P-3) -- notification_policies
-- escalate_after_s becomes unacked_reminder_after_s.
--
-- `escalation` is a SCOPE-BANNED word (SPEC §A.1, CONTEXT.md §3): AC-49 greps for
-- it across internal/, web/src/ and db/migrations/. Say UNACKED REMINDER. The
-- word matters because the concept it names is the one this codebase must never
-- acquire: an escalation is a LADDER that ends at a person, and oto has one
-- stage that ends at a CHANNEL.
--
-- The rename is deliberately a rename and not an add-then-drop pair: the column
-- has no production readers yet (SPEC §P-5 renames the Go and TS identifiers in
-- the same release), and a rename preserves the constraint, the type and the
-- ordinal position that a later expand/contract would churn for nothing.
--
-- ⛔ BINDING (SCOPE-BOUNDARY §5.3): `channel_ids` references `channels` and
-- NOTHING ELSE. `notification_policies` MUST NEVER gain `user_ids`, `team_ids`,
-- `schedule_id`, `rotation`, `time_of_day`, `days_of_week`, `timezone`, or a
-- second reminder stage (§G.9.1). A policy routes a fact to a DESTINATION. A
-- policy that routes to a PERSON is a rota.
--
-- ⛔ BINDING (SPEC §G.9.1, CONTEXT.md §1b): `unacked_reminder_after_s` STAYS
-- `INT`. ONE STAGE, FOREVER. IT MUST NEVER BECOME `INT[]`, a ladder, or acquire a
-- target other than the policy own `channel_ids`. The moment it is an array, oto
-- is an on-call product and FR-1 has been crossed. Widening it needs an ADR that
-- argues against FR-1 by name.

-- +goose Up

ALTER TABLE notification_policies RENAME COLUMN escalate_after_s TO unacked_reminder_after_s;
ALTER TABLE notification_policies RENAME CONSTRAINT policies_esc_ck TO policies_reminder_ck;

COMMENT ON COLUMN notification_policies.unacked_reminder_after_s IS
  'Unacked-for seconds before the notify.unacked_reminder job fires ONE Reason=unacked_reminder notification, 60..86400. SCALAR. ONE STAGE, FOREVER; never an array, never a ladder, never a target other than this policy channel_ids (SPEC §G.9.1). oto runs its own clock because Alertmanager repeat_interval defaults to FOUR HOURS (SPEC §G.9). NULL disables the reminder.';

-- +goose Down

ALTER TABLE notification_policies RENAME CONSTRAINT policies_reminder_ck TO policies_esc_ck;
ALTER TABLE notification_policies RENAME COLUMN unacked_reminder_after_s TO escalate_after_s;

COMMENT ON COLUMN notification_policies.escalate_after_s IS
  'Unacked-for seconds before escalation.check fires one Reason=escalation notification, 60..86400. oto runs its own clock because Alertmanager repeat_interval defaults to FOUR HOURS (SPEC §G.9). NULL disables escalation.';
