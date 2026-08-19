-- git-bug 7570090. This REVERTS stage 2 (migration 00063), which landed and is
-- already pushed, so the undo is a forward migration rather than a rollback.
--
-- ⛔ THE OWNER RULED: ONE CASE PER CONVERSATION, ALWAYS. `group_by` and the
-- `CollapseKey` it fed existed for exactly one job -- to collapse SEVERAL cases into
-- ONE conversation, so that an operator could choose which deliveries share a
-- message. Under one-case-per-conversation there is nothing left to choose: the Case
-- IS the conversation, and `alert_cases` already has that cardinality. The column
-- does not decide anything any more.
--
-- ⛔ THIS MIGRATION DESTROYS DATA AND THE DOWN CANNOT BRING IT BACK. Every stored
-- collapse list goes; the Down restores the COLUMN and its bound, and every policy
-- comes back at the `{}` default. Stated here, above the Up, rather than only beside
-- the Down, because the header is what gets read before `goose up` and the loss is not
-- reversible by running `goose down`. It is also the whole loss: nothing ever read the
-- values, so there is no behaviour to restore alongside them.
--
-- ⭐ WHY IT IS DELETED RATHER THAN LEFT DORMANT. A configurable knob that no code
-- reads is the precise defect four tickets closed in the run that added this one --
-- `0457f1f`, `35d4248`, `39e48e2`, `27a1860` -- each a setting an operator could
-- write and nothing would honour. Keeping `group_by` "in case it comes back" would
-- make it the fifth. The exposure was the API CONTRACT, not a screen: no settings form
-- ever rendered this field, but `POST /api/v1/notification-policies` accepted
-- `group_by` and `PATCH` honoured it, and `GET` returned it as a REQUIRED property --
-- so a client could set a grouping choice, read it back, and watch it govern nothing.
--
-- ⚠️ NOTHING EVER ROUTED ON IT, SO NO DELIVERY CHANGES. 00063's own header says so:
-- "`group_by` is READ and HASHED but nothing routes on the result". Verified against
-- the code and not taken from the header -- `Policy.CollapseKey` has zero production
-- callers, every reference is its own definition or its own test. Dropping the column
-- therefore cannot change which conversation anything lands in.
--
-- ⚠️ THIS IS NOT THE ALERTMANAGER `group_by` AND NOT THE ROLLUP AXIS. Both are
-- unrelated columns of the same name that stay exactly as they are: the route tree's
-- inherited `group_by` (00037, `am_route_timings`) and the `group_by=alertname`
-- rollup axis. Only `notification_policies.group_by` goes.

-- +goose Up

-- The constraint names the column, so it is dropped first for the reason 00063's Down
-- gave: leaving the order to DROP COLUMN's cascade would make this migration depend on
-- drop order rather than on being right.
ALTER TABLE notification_policies DROP CONSTRAINT policies_group_by_ck;
ALTER TABLE notification_policies DROP COLUMN group_by;

-- +goose Down

ALTER TABLE notification_policies
  ADD COLUMN group_by TEXT[] NOT NULL DEFAULT '{}'::text[];

ALTER TABLE notification_policies ADD CONSTRAINT policies_group_by_ck
  CHECK (cardinality(group_by) <= 8);

-- ⚠️ THE SHAPE, NOT THE VALUES -- see the ⛔ block in the header. Every policy comes
-- back at the `{}` default because the Up dropped the arrays and no migration can
-- invent them again.

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.group_by IS
  'The labels this policy collapses its deliveries by, computed at delivery from the alert''s OWN label set rather than from a stored group row (git-bug 7570090). EMPTY IS THE DEFAULT AND IS TODAY''S BEHAVIOUR: no policy-level collapse, and the conversation is identified the way it always was. ⛔ It is deliberately NOT `alert_groups.group_key` under another name -- that key is oto''s fixed identity axes (alertname, namespace) and is IMMUTABLE for an alert''s life, whereas this is a DELIVERY-TIME collapse an operator owns and may change without re-parenting anything. Editing it changes which future deliveries share a conversation; it moves nothing that already landed. Bounded at eight labels by policies_group_by_ck, each matching Prometheus''s label-name grammar.';
-- +goose StatementEnd
