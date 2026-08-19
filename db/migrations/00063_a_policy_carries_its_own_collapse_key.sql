-- git-bug 7570090, stage 2 of 7.
--
-- ⭐ THE GROUPING DECISION MOVES ONTO THE POLICY. `alert_groups` stores a decision
-- that is a pure function of four fixed labels and that no operator can configure;
-- `digest` already proved the decision belongs to a notification policy, and does
-- it with no group row at all. This column is where the configurable half lands.
--
-- ⛔ WHAT THIS MIGRATION DOES NOT DO, so the next reader is not misled. It adds the
-- column and its bound. It does NOT yet key a conversation by the collapse key --
-- a conversation is still identified through `alert_groups.id` inside
-- `notifications.subject_id`, and moving it is stage 3's
-- `(conversation_kind, conversation_id)` pair. Until then `group_by` is READ and
-- HASHED but nothing routes on the result. An empty array is today's behaviour and
-- is the default, so this migration changes no delivery at all.
--
-- ⚠️ THE PREREQUISITE ALREADY LANDED (stage 1, `d76ee0d`). A policy cannot group by
-- `node` unless the matcher is handed `node`, and until stage 1 it was handed only
-- `alertname` and `namespace` -- the group's two axes. Adding this column before
-- that would have shipped a knob over labels nothing could see.

-- +goose Up

ALTER TABLE notification_policies
  ADD COLUMN group_by TEXT[] NOT NULL DEFAULT '{}'::text[];

-- ⛔ CARDINALITY ONLY, AND THE PER-ELEMENT GRAMMAR IS DELIBERATELY NOT HERE.
-- Eight is well above any real collapse -- Alertmanager's own `group_by` lists run
-- to one to three labels in practice -- and a bound is what stops a policy naming
-- so many labels that every alert lands in its own conversation.
--
-- "every element matches Prometheus's label-name grammar" cannot be written as a
-- CHECK: a CHECK may not contain a subquery, so `unnest`/`array_agg` are out, and
-- the alternatives are both worse than the rule they buy. Joining the array and
-- regexing the result accepts a single element that CONTAINS the separator, which
-- is exactly the malformed input the rule is for. An IMMUTABLE helper function
-- would work, but it is a schema object this migration would then have to create
-- and its Down drop, for one regex.
--
-- So the grammar is enforced on the WRITE PATH, in `Policy.Validate`, where a bad
-- label name comes back as a field-level violation the settings form can point at
-- rather than as a 23514 an operator has to decode -- which is the same division
-- `validateDigest` already makes, and the reason that function exists.
--
-- ⚠️ THE COST IS STATED RATHER THAN HIDDEN: a row written around the service, by
-- hand or by a future migration, can hold a label name no label set can contain.
-- It collapses nothing and matches nothing; it does not corrupt anything.
ALTER TABLE notification_policies ADD CONSTRAINT policies_group_by_ck
  CHECK (cardinality(group_by) <= 8);

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.group_by IS
  'The labels this policy collapses its deliveries by, computed at delivery from the alert''s OWN label set rather than from a stored group row (git-bug 7570090). EMPTY IS THE DEFAULT AND IS TODAY''S BEHAVIOUR: no policy-level collapse, and the conversation is identified the way it always was. ⛔ It is deliberately NOT `alert_groups.group_key` under another name -- that key is oto''s fixed identity axes (alertname, namespace) and is IMMUTABLE for an alert''s life, whereas this is a DELIVERY-TIME collapse an operator owns and may change without re-parenting anything. Editing it changes which future deliveries share a conversation; it moves nothing that already landed. Bounded at eight labels by policies_group_by_ck, each matching Prometheus''s label-name grammar.';
-- +goose StatementEnd

-- +goose Down

ALTER TABLE notification_policies DROP CONSTRAINT policies_group_by_ck;
ALTER TABLE notification_policies DROP COLUMN group_by;
