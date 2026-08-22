-- Selection moves onto the routing decision, and a delivery records which revision
-- produced it.
--
-- A NotificationTemplate has no `when` clause of its own (00076). A policy already
-- carries matchers, already carries reasons, and already chose the destinations --
-- so it now also names the template, and there is exactly one thing to read to
-- predict a card. The predecessor gave presentation its own predicate language with
-- its own precedence order, which meant an operator had to hold two resolution
-- rules in their head and could not tell which one had decided anything.
--
-- The delivery half is provenance. "Why did my card read like that" has to stay
-- answerable after somebody edits the template, so the row records the id AND the
-- version that rendered it. The rendered payload is already persisted beside these
-- columns, so the bytes that went out are never in doubt either way -- what these
-- add is the attribution.

-- +goose Up

-- NULL means "oto's built-in card", which is what every existing policy gets and
-- what a policy gets back when its template is deleted. ON DELETE SET NULL rather
-- than RESTRICT: deleting a template is a settings action an operator takes
-- expecting it to work, and the honest consequence is that those policies go back
-- to the default voice -- not that the delete is refused by a foreign key naming a
-- policy they were not thinking about.
ALTER TABLE notification_policies
  ADD COLUMN template_id UUID REFERENCES notification_templates(id) ON DELETE SET NULL;

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.template_id IS
  'The NotificationTemplate this policy''s deliveries are rendered with. NULL is oto''s built-in card. There is no per-channel override: a policy fans out to as many as sixteen destinations and they all get this one template, which may not suit every provider -- `card` and `text` are portable, `raw` is Slack-only and degrades elsewhere. Pairing them up is the operator''s call and oto only warns.';
-- +goose StatementEnd

-- Written in the same statement as `rendered`, so a row can never carry a payload
-- with no attribution for it. Both are NULL for a delivery oto's own card produced.
ALTER TABLE notification_deliveries
  ADD COLUMN template_id      UUID,
  ADD COLUMN template_version INTEGER;

-- No foreign key, deliberately, and it is the opposite decision from the policy
-- column above. A delivery is a HISTORICAL RECORD: it must keep saying which
-- template produced it even after that template is hard-deleted, and a foreign key
-- would either block the delete or null the evidence. The id is a pointer to look
-- up, not a constraint to enforce.
-- +goose StatementBegin
COMMENT ON COLUMN notification_deliveries.template_id IS
  'The NotificationTemplate that rendered this delivery, or NULL for oto''s built-in card. NOT a foreign key: a delivery is a historical record and must keep its attribution after the template is gone.';
-- +goose StatementEnd

ALTER TABLE notification_deliveries
  ADD CONSTRAINT notification_deliveries_template_ck
  CHECK ((template_id IS NULL) = (template_version IS NULL));

-- 00077's column, retired. It recorded a map of stanza -> the prose that wrote it,
-- which is a shape no template has: a template IS the message, so one id and one
-- version say everything the map used to. Dropped rather than left behind, because a
-- nullable column nothing writes is a column the next reader has to research.
ALTER TABLE notification_deliveries DROP COLUMN wordings;

-- +goose Down

-- No guard on either drop, and that is the honest asymmetry with 00076. These
-- columns hold a POINTER to prose, never the prose itself: dropping them loses the
-- attribution of past cards, which is a real loss and a recoverable one -- the
-- rendered payload stays on the row. 00076 refuses because dropping it would
-- destroy text that exists nowhere else.
-- +goose StatementBegin
DO $$
DECLARE n BIGINT;
BEGIN
  SELECT count(*) INTO n FROM notification_deliveries WHERE template_id IS NOT NULL;
  IF n > 0 THEN
    RAISE WARNING
      'dropping template attribution from % delivery row(s). The rendered payload on each row is unaffected; what is lost is which template and which revision produced it.', n;
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE notification_deliveries
  DROP CONSTRAINT notification_deliveries_template_ck,
  DROP COLUMN template_version,
  DROP COLUMN template_id;

-- Restored empty, for the reason 00078's Down restores `wordings`: the release below
-- this one reads the column, and a rollback that leaves it missing is a second broken
-- state rather than a return to the first.
ALTER TABLE notification_deliveries ADD COLUMN wordings JSONB;

ALTER TABLE notification_policies DROP COLUMN template_id;
