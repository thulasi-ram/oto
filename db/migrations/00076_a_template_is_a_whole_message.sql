-- Customers can change how a notification READS, and until now only Go could write
-- oto's own facts into a sentence. A rule author can already template the alert's
-- prose upstream, in their own GitOps repo, with Alertmanager's text/template -- but
-- "firing 20 minutes, 4th time this week, still unacked" is a sentence Prometheus
-- cannot write, because Prometheus does not know any of it. This table is the
-- authoring surface for that sentence.
--
-- A NotificationTemplate is ONE WHOLE MESSAGE, and that is a deliberate reversal.
-- The design this replaced stored one row per named block and let an operator
-- override the text of four of them. It was safe, it was defensible, and nobody's
-- mental model of a template is "four holes in somebody else's card". A template a
-- person can read top to bottom is one they can predict; four independent overrides
-- are not, and the question every reviewer asked first was "so where is my
-- template?".
--
-- IT CARRIES NO `when` CLAUSE, WHICH IS THE OTHER REVERSAL. The predecessor selected
-- itself, on its own matchers, with its own precedence order -- necessary only while
-- an operator needed four different predicates for four different slots. One whole
-- template per message has no such pressure, so selection goes back where routing
-- already lives: `notification_policies.template_id`, added in 00077. One routing
-- decision, one place to read it, one precedence rule to hold in your head.

-- +goose Up

CREATE TABLE notification_templates (
  id       UUID NOT NULL PRIMARY KEY,
  org_id   UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  -- What a policy's picker shows. It is the only human handle on the row.
  name     TEXT NOT NULL,
  -- The destination kind this template was WRITTEN FOR. It is DECLARED INTENT and
  -- not an enforced constraint, and there is deliberately no foreign key or CHECK
  -- tying it to the channels a policy fans out to: a policy reaches as many as
  -- sixteen destinations and they need not share a provider. `card` and `text`
  -- render correctly anywhere; `raw` is Slack's Block Kit and degrades to oto's
  -- built-in card elsewhere. oto warns at save and does not block -- pairing them up
  -- is the operator's call, and a mismatch is a degraded message, never a dropped alert.
  provider TEXT NOT NULL,
  format   TEXT NOT NULL,
  -- The template body. Liquid, on github.com/osteele/liquid NewBasicEngine: no tags
  -- and no filters except the ones oto registers by name. The two control-flow
  -- blocks -- `for` and `if`/`unless` -- are oto's OWN implementations, not the
  -- library's, because a basic engine cannot be given the library's and the
  -- alternative brought thirteen tags including two that read from a template store.
  -- Writing them means the iteration budget is a real counter rather than a scan.
  source   TEXT NOT NULL,
  -- Bumped on every edit that changes `source` or `format`. It is the provenance
  -- half of the delivery row (00077): a card that read strangely last Tuesday can be
  -- attributed to a revision even after somebody has edited it since. Deliberately
  -- not a version HISTORY -- the rendered payload is already persisted beside the
  -- delivery, so the bytes that actually went out are never in doubt.
  version  INTEGER NOT NULL,
  enabled  BOOLEAN NOT NULL,
  -- No DEFAULT now() on either clock column, deliberately, and migrations 00032 and
  -- 00033 are why: the database's clock and the application's clock racing against one
  -- CHECK produced a ~50%-reproducible 23514 on the first write after a create. The
  -- application owns time, unconditionally. A default here would be a trap with a
  -- delayed fuse -- a future writer that omits the column would succeed at INSERT and
  -- fail much later, somewhere unrelated.
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  -- The closed set of shapes an author may write in. It is a CHECK rather than a
  -- lookup table because it is a vocabulary of the code, not of the tenant: a fourth
  -- format needs a compiler, an editor mode and a validator, so it needs a
  -- deployment either way.
  CONSTRAINT notification_templates_format_ck  CHECK (format IN ('card','text','raw')),
  CONSTRAINT notification_templates_name_ck    CHECK (length(name) BETWEEN 1 AND 120),
  -- 16 KiB. A whole card is a document, so this is far larger than the one line the
  -- slot design allowed -- but it is still a bound, and it is the first thing that
  -- stops a pathological template from ever being parsed.
  CONSTRAINT notification_templates_source_ck  CHECK (length(source) BETWEEN 1 AND 16384),
  CONSTRAINT notification_templates_version_ck CHECK (version >= 1),
  CONSTRAINT notification_templates_time_ck    CHECK (updated_at >= created_at),
  -- A name is how a policy's author picks one, so two live rows sharing one is a
  -- picker with two identical entries. Deleted rows are excluded: a name freed by a
  -- delete is available again.
  CONSTRAINT notification_templates_name_uq    UNIQUE (org_id, name, deleted_at)
);

-- +goose StatementBegin
COMMENT ON TABLE notification_templates IS
  'One whole notification message, as an operator wrote it. Selected by the policy that routes the delivery (notification_policies.template_id), never by a clause of its own. A template that fails to render at delivery falls back to oto''s built-in card, so it can never mark a delivery dead.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_templates.format IS
  '`card` is Markdown-plus, parsed to oto''s own document IR and compiled per provider -- the default, and the only PORTABLE structured format. `text` is one flat string, also portable. `raw` is literal Slack Block Kit JSON with interpolation: it is pinned to Slack, a malformed payload is REJECTED BY SLACK rather than degraded, and it is gated behind a preview that actually rendered.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_templates.source IS
  'Validated at save time by RENDERING against a fixture corpus including the hostile cases, not merely parsing -- an unknown filter is a render-time error in Liquid and a parse would miss it. Every interpolated value is markdown-escaped unconditionally in `card` format, which is why there is no raw-output syntax and no taint tracking: an alert label is attacker-influenced, and a value that cannot produce syntax cannot produce structure, a link, a mention, or a forged handle.';
-- +goose StatementEnd

-- Serves: the picker on the policy editor, and the settings list. Partial, because
-- a deleted template is never offered.
CREATE INDEX notification_templates_live_idx
  ON notification_templates (org_id, name, id)
  WHERE deleted_at IS NULL;

-- +goose Down

-- +goose StatementBegin
DO $$
DECLARE n BIGINT;
BEGIN
  -- Every row, not just the live ones. A soft-deleted template is still the
  -- customer's own prose, and it is what a delivery's recorded template_id points at
  -- when somebody asks why a card from last month read the way it did. Counting only
  -- `deleted_at IS NULL` would let the DROP take that history silently.
  SELECT count(*) INTO n FROM notification_templates;
  IF n > 0 THEN
    RAISE EXCEPTION
      'refusing to drop `notification_templates`: % row(s) would be destroyed, and a template is customer-authored prose that exists nowhere else. Inspect with: SELECT id, org_id, name, provider, format, deleted_at FROM notification_templates;', n;
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE notification_templates;
