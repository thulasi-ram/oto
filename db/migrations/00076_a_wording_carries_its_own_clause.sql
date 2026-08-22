-- Customers can change how a notification READS, and until now only Go could write
-- oto's own facts into a sentence. A rule author can already template the alert's
-- prose upstream, in their own GitOps repo, with Alertmanager's text/template -- but
-- "firing 20 minutes, 4th time this week, still unacked" is a sentence Prometheus
-- cannot write, because Prometheus does not know any of it. This table is the
-- authoring surface for that sentence (ADR 0037).
--
-- A Wording is one Liquid template producing the TEXT of one Stanza. Structure stays
-- oto's: Go builds every block, assigns every block_id, and owns the attachment,
-- colour and emoji. Four of SPEC H.7's eight stanzas take one -- title, body, rule,
-- footer, the ones that are prose -- and the other four are refused at save time
-- because a grid, two sequences and a row of buttons are structure, not wording.
--
-- IT IS NOT A COLUMN ON `notification_policies`, AND THAT WAS THE DESIGN'S OPEN
-- QUESTION (ADR 0049). Under first-match-wins routing, hanging presentation off the
-- routing table would mean an operator who wanted one channel to read differently had
-- to duplicate the routing rule -- re-declaring reasons, channel_ids and throttle --
-- creating a second thing that drifts from the first. A Wording already carries its
-- own `when` clause, so it selects itself and routing is never consulted.

-- +goose Up

CREATE TABLE wordings (
  id         UUID        PRIMARY KEY,
  org_id     UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  -- NULL means org-wide: the house voice, every card this tenant sends. A non-NULL
  -- channel_id is the exception for one destination, and it WINS over the org-wide
  -- row (ADR 0049, most-specific-first) -- a rule naming one destination is more
  -- specific than one naming a whole tenant.
  channel_id UUID        REFERENCES channels(id) ON DELETE CASCADE,
  stanza     TEXT        NOT NULL,
  template   TEXT        NOT NULL,
  -- The `when` clause, in ADR 0017's vocabulary verbatim, stored exactly as
  -- notification_policies.matchers is: a JSONB array of {name,op,value}. Reusing the
  -- shape rather than inventing a second predicate language is the whole of ADR 0017.
  matchers   JSONB       NOT NULL DEFAULT '[]',
  reasons    TEXT[]      NOT NULL DEFAULT '{}',
  -- Priority orders evaluation, LOWER FIRST, and the first match wins -- the same
  -- sentence notification_policies.priority carries. Two orderings that read the same
  -- way and behave differently is how an operator learns to distrust both.
  priority   INTEGER     NOT NULL DEFAULT 100,
  enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
  -- No DEFAULT now() on either clock column, deliberately, and migrations 00032 and
  -- 00033 are why: the database's clock and the application's clock racing against one
  -- CHECK produced a ~50%-reproducible 23514 on the first write after a create. The
  -- application owns time, unconditionally. A default here would be a trap with a
  -- delayed fuse -- a future writer that omits the column would succeed at INSERT and
  -- fail much later, somewhere unrelated.
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  -- Only the four prose stanzas. The refused four are rejected in Go with a sentence
  -- explaining which kind of structure they are; this constraint is the floor under
  -- that, so a row no code path could have written cannot arrive by any other route.
  CONSTRAINT wordings_stanza_ck   CHECK (stanza IN ('title','body','rule','footer')),
  -- A stanza is one line of prose. The ceiling is a shape limit, not a safety limit:
  -- output is bounded by the renderer's own escape-and-truncate sink regardless.
  CONSTRAINT wordings_template_ck CHECK (length(template) BETWEEN 1 AND 2048),
  CONSTRAINT wordings_matchers_ck CHECK (jsonb_typeof(matchers) = 'array' AND jsonb_array_length(matchers) <= 32),
  CONSTRAINT wordings_reasons_ck  CHECK (cardinality(reasons) <= 32),
  CONSTRAINT wordings_priority_ck CHECK (priority BETWEEN 0 AND 100000),
  CONSTRAINT wordings_time_ck     CHECK (updated_at >= created_at)
);

-- +goose StatementBegin
COMMENT ON TABLE wordings IS
  'One Liquid template producing the TEXT of one Stanza of a notification (ADR 0037). A Wording chooses words; it never chooses structure, colour, mentions, links or destination. Selected by its own `when` clause, never by a route (ADR 0049).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN wordings.channel_id IS
  'NULL is the org-wide house voice; a non-NULL destination is the exception and WINS over it (ADR 0049). ON DELETE CASCADE because a wording for a channel that no longer exists has nothing left to say.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN wordings.template IS
  'Liquid, on github.com/osteele/liquid NewBasicEngine: no tags and no filters except the ones oto registers by name, so there is no branching and no iteration. Validated at save time by RENDERING against a fixture corpus, not merely parsing -- an unknown filter is a render-time error in Liquid and a parse would miss it.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN wordings.stanza IS
  'One of SPEC H.7''s eight block names, restricted to the four that are prose. `fields` is a grid of separately-budgeted cells in a shed order, `members` and `trail` are sequences a loop-free language cannot iterate, and `actions` carries button labels bound to their action_ids.';
-- +goose StatementEnd

-- Serves: wording resolution on every delivery -- walk this org's live wordings for
-- the destination and the org-wide fallback together, most-specific first, then
-- priority. Partial, because a disabled or deleted wording is never resolved.
-- The column order matches the query exactly: `stanza` is NOT in it, because
-- resolution reads every stanza's candidates in one go and filters in Go. A
-- `stanza` column in position two would leave only the `org_id` prefix usable and
-- push the whole ORDER BY into a sort, which is the opposite of what this index's
-- name claims. `created_at, id` are here so the tie-break is served too.
CREATE INDEX wordings_resolve_idx
  ON wordings (org_id, channel_id NULLS LAST, priority, created_at, id)
  WHERE enabled AND deleted_at IS NULL;

-- +goose Down

-- +goose StatementBegin
DO $$
DECLARE n BIGINT;
BEGIN
  -- ⚠️ EVERY ROW, NOT JUST THE LIVE ONES. A soft-deleted Wording is still the
  -- customer's own prose, and it is what a delivery's recorded wording set points
  -- at when somebody asks why a card from last month read the way it did. Counting
  -- only `deleted_at IS NULL` would let the DROP take that history silently.
  SELECT count(*) INTO n FROM wordings;
  IF n > 0 THEN
    RAISE EXCEPTION
      'refusing to drop `wordings`: % row(s) would be destroyed, and a Wording is customer-authored prose that exists nowhere else. Inspect with: SELECT id, org_id, channel_id, stanza, template, deleted_at FROM wordings;', n;
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE wordings;
