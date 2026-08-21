-- A delivery already keeps the exact bytes it sent (`rendered`, §L.6) so that a
-- card can be reconstructed. Now that a customer's own Liquid can write part of
-- that card, the bytes are no longer self-explaining: the same card is produced
-- by oto's built-in text or by a Wording, and nothing on the row says which.
--
-- "Why did my card read like that" must be answerable from ONE ROW. The
-- alternative is replaying resolution against configuration that may since have
-- changed -- a Wording edited, disabled, soft-deleted, or outranked by a new
-- higher-priority row -- which answers a question about last Tuesday with today's
-- settings. That is the same argument that put `rendered` on the row in the first
-- place, applied to the thing that produced it (ADR 0049).

-- +goose Up

ALTER TABLE notification_deliveries
  ADD COLUMN wordings JSONB;

-- +goose StatementBegin
COMMENT ON COLUMN notification_deliveries.wordings IS
  'The resolved Wording set that produced this card: a JSON object of stanza -> template source, or NULL when oto''s own text wrote every stanza. Recorded because resolution depends on rows that change: replaying it later answers with today''s configuration, not the one that rendered this card (ADR 0049). It is the SOURCE rather than a row id for the same reason -- a soft-deleted wording''s id explains nothing on its own.';
-- +goose StatementEnd

-- ⚠️ NO INDEX, DELIBERATELY. This column is read one row at a time, by a human
-- who already has a delivery id and is asking why one card said what it said.
-- Nothing queries across it, and a JSONB index on a write-once forensic column
-- would cost every delivery to serve a question nobody asks in bulk.

-- +goose Down

-- ⚠️ A WARNING RATHER THAN A REFUSAL, UNLIKE 00076's. What is lost here is the
-- EXPLANATION of a card, not the card and not the customer's authored prose: the
-- `wordings` table still holds every template, and `rendered` still holds the
-- exact bytes that were sent. A rollback that refuses to run because it would
-- drop a forensic annotation would be a rollback an operator cannot perform at
-- 02:00, and 00073-00076's guards exist for data that exists nowhere else.

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM notification_deliveries WHERE wordings IS NOT NULL) THEN
    RAISE WARNING 'wordings recorded on % delivery row(s) will be dropped by this rollback; the cards themselves are unaffected and `rendered` still holds their bytes',
      (SELECT count(*) FROM notification_deliveries WHERE wordings IS NOT NULL);
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE notification_deliveries
  DROP COLUMN wordings;
