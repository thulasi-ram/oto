-- ADR 0053 §5: A FINDING IS CLASSIFIED ONLY IN THE OPERATOR'S WORDS (git-bug 4298aa0).
-- "A Finding's class is chosen from a closed set the operator wrote, or `unclassified`,
-- which is always admissible and is the right answer under doubt. oto ships no classes —
-- no `noise`, which would be oto's opinion of someone else's signal. A Finding keeps its
-- class if the set later changes." Two changes:
--
--   1. `investigation_classes`: the org's class set, a name and a description per row,
--      in the order the operator wrote them. EMPTY BY DEFAULT AND NOTHING HERE SEEDS IT —
--      oto ships no class, so an org that wrote none has Findings with no classification
--      at all. `unclassified` is never a row: it is always admissible, so it is not the
--      operator's to add or to take away, and `investigation_classes_name_ck` refuses it.
--   2. `investigations.classification`: the class the run's Finding was given, as TEXT.
--
-- ⛔⛔ THE CLASS IS A COPY OF A NAME, NOT A FOREIGN KEY, ON PURPOSE. "A Finding keeps its
-- class if the set later changes": renaming or removing a class rewrites the SET and never
-- a Finding, and a frozen Investigation (`investigations_frozen`) could not be rewritten
-- anyway. An FK would have to CASCADE (deleting history), SET NULL (rewriting it) or
-- RESTRICT (making the operator's vocabulary impossible to change) — and all three are
-- wrong. So `classification` names what the model chose from the set AS IT STOOD when the
-- run read it, and stays that word.
--
-- ⭐ NULL MEANS "NO SET WAS OFFERED", AND IS NEVER A SYNONYM FOR `unclassified`. A run that
-- reached a Finding while the org had no classes carries NULL; a run that was offered a
-- set and found nothing in it fitting — or never said, or picked a word outside it —
-- carries `unclassified`. A classification without a Finding cannot be stored: the class
-- belongs to what was concluded.
--
-- ⛔ A CLASSIFICATION IS A MODEL'S JUDGEMENT, AND IT IS NEVER AN INPUT TO WHETHER ANYONE IS
-- TOLD (ADR 0053 §2). It travels outbound with the Finding (ADR 0052 §5) and a receiver may
-- page on it; the docs say plainly that that is paging on a model's judgement. Nothing on
-- the notification path reads this column.
--
-- ⛔ NO DEFAULT now() ON ANY TIMESTAMP (CONTEXT.md §6): `created_at` is the app's clock.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One new table release N-1 neither reads nor writes, and
-- one nullable column it never names.

-- +goose Up

CREATE TABLE investigation_classes (
  org_id      UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name        TEXT        NOT NULL,
  description TEXT        NOT NULL,
  -- The operator's order: the order the model is told the set in, and the settings
  -- screen shows it in. Dense from 0 — the whole set is replaced at once.
  position    INT         NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL,
  CONSTRAINT investigation_classes_pk          PRIMARY KEY (org_id, name),
  -- ⭐ A WORD A MODEL CAN SAY BACK EXACTLY, a card can quote and a receiver can match on:
  -- lower-case letters, digits, `_` and `-`, starting with a letter, at most 63 characters.
  -- ⛔ `unclassified` is reserved: it is always admissible and is not the operator's.
  CONSTRAINT investigation_classes_name_ck     CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$' AND name <> 'unclassified'),
  CONSTRAINT investigation_classes_desc_ck     CHECK (char_length(description) <= 500),
  CONSTRAINT investigation_classes_position_ck CHECK (position BETWEEN 0 AND 49)
);

-- Serves: the set in the operator's order, and one class per position.
CREATE UNIQUE INDEX investigation_classes_position_uniq ON investigation_classes (org_id, position);

-- +goose StatementBegin
COMMENT ON TABLE investigation_classes IS
  'The org''s Classification set (ADR 0053 §5): the closed vocabulary an Investigation must classify its Finding in, written by the operator, at most 50, in their order. oto ships NO class and seeds none; with no row here a Finding carries no classification. unclassified is never a row: it is always admissible and is the right answer under doubt. Replaced whole by the settings API. A Finding copies the NAME it was given (investigations.classification), so changing this set never rewrites a Finding.';
-- +goose StatementEnd

ALTER TABLE investigations
  ADD COLUMN classification TEXT,
  ADD CONSTRAINT investigations_class_ck CHECK (
    classification IS NULL
    OR (finding IS NOT NULL AND classification ~ '^[a-z][a-z0-9_-]{0,62}$')
  );

-- +goose StatementBegin
COMMENT ON COLUMN investigations.classification IS
  'ADR 0053 §5: the class the model gave this run''s Finding — one of the org''s investigation_classes as the set stood when the run read it, or unclassified (offered a set, it named nothing in it: doubt, silence, or a word outside the set). NULL: the org had no classes, so none was offered. A COPY OF THE NAME, never a foreign key: renaming or removing a class leaves every Finding with the class it was given. A model''s judgement — never an input to whether anyone is told (ADR 0053 §2).';
-- +goose StatementEnd

-- +goose Down

-- ⛔ THE CLASSIFICATIONS GO WITH THE COLUMN, AND THE SET WITH ITS TABLE. The release below
-- has neither, and an Investigation is frozen (`investigations_frozen` guards UPDATE, not
-- ALTER), so dropping the column is the only way back; the Findings themselves stay.
ALTER TABLE investigations DROP CONSTRAINT investigations_class_ck;
ALTER TABLE investigations DROP COLUMN classification;

DROP TABLE investigation_classes;
