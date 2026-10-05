-- ADR 0053 §2, ADR 0052 §2 and §4: A FINDING SUGGESTS, AND A HUMAN APPLIES IT OR IT LAPSES
-- (git-bug 8327c00). "Noise reduction arrives as Suggestions (e.g. "add `count_min=3` to this
-- policy"), which a human applies and which then act deterministically. A Suggestion is
-- applied or lapses; it has no reject verb, so it is never a queue." One table:
--
--   `investigation_suggestions` — one row per Suggestion a run's Finding made, of one of two
--   kinds:
--     * `policy_count_condition`: set one notification policy's count condition (ADR 0044);
--     * `incident_membership`: add one Case to one Incident — a MOVE when the Case is in
--       another, because a Case belongs to at most one (ADR 0052 §4).
--
-- ⭐⭐ ONE STATE IS STORED; THE OTHER TWO ARE READ. A row is `applied` once `applied_at` is
-- set, `lapsed` once `lapses_at` has passed with it unset, and `open` otherwise. Lapsing is
-- read off the application's clock and never written: nothing happens when a Suggestion
-- lapses — no fact goes out, no job runs — so there is no sweeper and no `state` column for
-- one to keep in step. `lapses_at` is stamped per row (proposed_at + seven days today), so a
-- later setting changes the Suggestions made after it and none made before.
--
-- ⛔⛔ NO COLUMN RECORDS A REFUSAL, AND THAT IS THE RULING. There is no `declined_at`, no
-- `rejected_by`: an unapplied Suggestion lapses on its own. A verb that kept a Suggestion in
-- front of somebody until they acted on it would make it a queue of work routed to a person
-- (H-1).
--
-- ⭐ THE ROW IS THE PROVENANCE. Applying performs the ordinary edit — the policy PATCH's own
-- service path, or the Incident service's add or move — so the policy or the membership looks
-- exactly as a human's edit leaves it. What says "suggested by Investigation X, applied by Y
-- at T" is this row (`investigation_id`, `applied_by`, `applied_at`) and, for a membership,
-- the `incident.case_*` fact on the Case's timeline, whose payload carries
-- `suggested_by_investigation_id`. A policy edit records no actor anywhere in oto, and this
-- table does not change that for the human path.
--
-- ⭐ A PROPOSAL IS FROZEN, AN APPLICATION IS ONCE. `investigation_suggestions_once` refuses an
-- UPDATE of a row already applied, and an UPDATE that touches anything but the three
-- `applied_*` columns: what the Finding proposed can never be rewritten after the fact.
--
-- ⚠️ NO FOREIGN KEY ONTO THE TARGET. `policy_id`, `incident_id` and `case_id` name rows of
-- other modules' tables (`notification_policies`, `incidents`, `alert_cases`), and a
-- Suggestion must outlive a policy deleted after it was proposed — that is answered at apply
-- time as `suggestion_target_gone`, not by losing the record. `investigations.subject_id`
-- carries none for the same reason. The copies of names and numbers (`policy_name`,
-- `incident_number`, `case_number`) are what a human reads without a join.
--
-- ⛔ NO DEFAULT now() ON ANY TIMESTAMP (CONTEXT.md §6): every time here is the app's clock.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One new table release N-1 neither reads nor writes.

-- +goose Up

CREATE TABLE investigation_suggestions (
  id                 UUID        PRIMARY KEY,
  org_id             UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  investigation_id   UUID        NOT NULL REFERENCES investigations(id) ON DELETE CASCADE,
  kind               TEXT        NOT NULL,
  -- `policy_count_condition`: the policy, the condition proposed, and the one it carried
  -- when proposed (both NULL for none).
  policy_id          UUID,
  policy_name        TEXT,
  count_min          INT,
  count_window_s     INT,
  was_count_min      INT,
  was_count_window_s INT,
  -- `incident_membership`: this Case, into this Incident.
  incident_id        UUID,
  incident_number    BIGINT,
  case_id            UUID,
  case_number        BIGINT,
  why                TEXT        NOT NULL,
  proposed_at        TIMESTAMPTZ NOT NULL,
  lapses_at          TIMESTAMPTZ NOT NULL,
  applied_at         TIMESTAMPTZ,
  applied_by         UUID        REFERENCES users(id) ON DELETE SET NULL,
  applied_by_label   TEXT,

  CONSTRAINT investigation_suggestions_kind_ck   CHECK (kind IN ('policy_count_condition','incident_membership')),
  -- Each kind carries exactly its own columns, so a row cannot say half of two changes.
  CONSTRAINT investigation_suggestions_shape_ck  CHECK (
       (kind = 'policy_count_condition'
        AND policy_id IS NOT NULL AND policy_name IS NOT NULL AND count_min IS NOT NULL AND count_window_s IS NOT NULL
        AND incident_id IS NULL AND incident_number IS NULL AND case_id IS NULL AND case_number IS NULL)
    OR (kind = 'incident_membership'
        AND incident_id IS NOT NULL AND incident_number IS NOT NULL AND case_id IS NOT NULL AND case_number IS NOT NULL
        AND policy_id IS NULL AND policy_name IS NULL AND count_min IS NULL AND count_window_s IS NULL
        AND was_count_min IS NULL AND was_count_window_s IS NULL)
  ),
  -- ADR 0044 §4's bounds (`policies_count_min_ck`, `policies_count_window_ck`): a count
  -- condition outside them could never be applied, so it is never proposed.
  CONSTRAINT investigation_suggestions_count_ck  CHECK (
    count_min IS NULL OR (count_min BETWEEN 2 AND 10000 AND count_window_s BETWEEN 60 AND 86400)
  ),
  CONSTRAINT investigation_suggestions_was_ck    CHECK ((was_count_min IS NULL) = (was_count_window_s IS NULL)),
  CONSTRAINT investigation_suggestions_name_ck   CHECK (policy_name IS NULL OR length(btrim(policy_name)) BETWEEN 1 AND 120),
  CONSTRAINT investigation_suggestions_number_ck CHECK (
    (incident_number IS NULL OR incident_number > 0) AND (case_number IS NULL OR case_number > 0)
  ),
  CONSTRAINT investigation_suggestions_why_ck    CHECK (char_length(why) BETWEEN 1 AND 1000),
  CONSTRAINT investigation_suggestions_lapse_ck  CHECK (lapses_at > proposed_at),
  -- Applied by a named human, or not applied: the label is frozen beside the id, which is
  -- nulled when the user goes.
  CONSTRAINT investigation_suggestions_applied_ck CHECK (
    (applied_at IS NULL) = (applied_by_label IS NULL)
    AND (applied_by IS NULL OR applied_at IS NOT NULL)
    AND (applied_at IS NULL OR applied_at >= proposed_at)
  ),
  CONSTRAINT investigation_suggestions_label_ck  CHECK (
    applied_by_label IS NULL OR length(btrim(applied_by_label)) BETWEEN 1 AND 200
  )
);

-- Serves: one Investigation's Suggestions, in the order they were proposed.
CREATE INDEX investigation_suggestions_run_idx ON investigation_suggestions (org_id, investigation_id, proposed_at, id);

-- +goose StatementBegin
COMMENT ON TABLE investigation_suggestions IS
  'A change a Finding proposes and only a human can apply (ADR 0053 §2): a notification policy''s count condition (ADR 0044), or one Case into one Incident (ADR 0052 §4; a move when the Case is in another). Applied (applied_at set) or lapsed (lapses_at passed unapplied, read off the clock — nothing is written when it lapses); there is no reject verb and no column for one, so it is never a queue. Applying performs the ordinary edit with the applier as actor; this row is its provenance. The Investigator never applies one.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN investigation_suggestions.lapses_at IS
  'When an unapplied Suggestion lapses and stops being shown or applicable: proposed_at plus seven days, stamped per row. Lapsing is read off the application''s clock, never written — there is no sweeper.';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION investigation_suggestions_refuse_rewrite() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.applied_at IS NOT NULL THEN
    RAISE EXCEPTION 'suggestion % was applied at % and is frozen: it is applied once', OLD.id, OLD.applied_at;
  END IF;
  IF (NEW.id, NEW.org_id, NEW.investigation_id, NEW.kind, NEW.policy_id, NEW.policy_name, NEW.count_min,
      NEW.count_window_s, NEW.was_count_min, NEW.was_count_window_s, NEW.incident_id, NEW.incident_number,
      NEW.case_id, NEW.case_number, NEW.why, NEW.proposed_at, NEW.lapses_at)
     IS DISTINCT FROM
     (OLD.id, OLD.org_id, OLD.investigation_id, OLD.kind, OLD.policy_id, OLD.policy_name, OLD.count_min,
      OLD.count_window_s, OLD.was_count_min, OLD.was_count_window_s, OLD.incident_id, OLD.incident_number,
      OLD.case_id, OLD.case_number, OLD.why, OLD.proposed_at, OLD.lapses_at) THEN
    RAISE EXCEPTION 'suggestion % is a proposal and is never rewritten: only applying it is recorded', OLD.id;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER investigation_suggestions_once
  BEFORE UPDATE ON investigation_suggestions
  FOR EACH ROW EXECUTE FUNCTION investigation_suggestions_refuse_rewrite();

-- +goose Down

-- ⛔ THE SUGGESTIONS GO WITH THEIR TABLE. The release below has no Suggestion; an applied one's
-- edit stays where it was made — the policy keeps its count condition, the Case its
-- membership — because applying it was the ordinary edit, and only this row's provenance goes.
DROP TRIGGER investigation_suggestions_once ON investigation_suggestions;
DROP FUNCTION investigation_suggestions_refuse_rewrite();
DROP TABLE investigation_suggestions;
