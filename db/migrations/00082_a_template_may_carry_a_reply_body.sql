-- `notification_templates` gains `reply_source`: the body a THREAD REPLY is
-- rendered from, in the template's own format (ADR 0051).
--
-- ⭐ WHY A SECOND COLUMN AND NOT A SECTION OF `source`. A template already says
-- what one ROOT CARD looks like, top to bottom, and ADR 0050's argument for that —
-- the unit somebody edits is the unit that gets sent — applies to a reply as
-- well: a reply is a different message, posted at a different time, read in a
-- different place. Two bodies are two messages an author can read and preview
-- separately; one body with a reply section inside it is two messages pretending
-- to be one.
--
-- ⭐ NULL MEANS "oto's OWN REPLIES", which is every template written before this
-- column existed, so no row needs a backfill and no existing template changes
-- what it sends. A body that renders nothing for a given reason means the same
-- thing for that reason alone, which is how an author restyles `all_resolved`
-- and leaves the rest of the thread oto's.
--
-- ⛔ THE LENGTH FLOOR IS ONE, AS IT IS ON `source`. An empty body is not a body;
-- clearing the reply is writing NULL, and `''` is refused rather than given a
-- second spelling of "none".

-- +goose Up

ALTER TABLE notification_templates ADD COLUMN reply_source TEXT;

ALTER TABLE notification_templates ADD CONSTRAINT notification_templates_reply_source_ck
  CHECK (reply_source IS NULL OR length(reply_source) BETWEEN 1 AND 16384);

-- +goose StatementBegin
COMMENT ON COLUMN notification_templates.reply_source IS
  'The template body a thread reply is rendered from, in this row''s format. NULL means oto renders its own replies. A body that renders nothing for a reason leaves oto''s reply for that reason. Changing it bumps version, as source and format do.';
-- +goose StatementEnd

-- +goose Down

ALTER TABLE notification_templates DROP CONSTRAINT notification_templates_reply_source_ck;
ALTER TABLE notification_templates DROP COLUMN reply_source;
