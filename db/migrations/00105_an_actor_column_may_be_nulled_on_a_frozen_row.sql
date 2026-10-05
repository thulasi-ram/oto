-- A FROZEN INVESTIGATION, OR AN APPLIED SUGGESTION, LETS ITS ACTOR GO WHEN THE USER DOES
-- (review B5). Two rows in ADR 0053's record are frozen by a BEFORE UPDATE trigger:
--
--   investigations_frozen            an Investigation that ended (00096) is never rewritten
--   investigation_suggestions_once   an applied Suggestion (00101) is applied once
--
-- and each carries an ACTOR column in the acked_by mould — `investigations.requested_by`
-- and `investigation_suggestions.applied_by`, both `REFERENCES users(id) ON DELETE SET
-- NULL`, with the `_label` frozen beside them. That SET NULL is an UPDATE, so it reached
-- the freeze and was refused: hard-deleting a user who had asked for a run that has ended,
-- or applied a Suggestion, aborted the DELETE. Nothing hard-deletes a user today; a purge
-- or an org cascade would, and it would fail on exactly the people who used the feature.
--
-- ⭐ THE FREEZE NOW LETS THROUGH THAT ONE UPDATE AND NOTHING ELSE. Both functions admit an
-- UPDATE only when (a) it arrives nested inside another trigger — `pg_trigger_depth() > 1`,
-- which is what a foreign key's own action is, and what 00104's `remedy_record_refuse_change`
-- already reads the same way; (b) the actor column goes NULL; and (c) every OTHER column is
-- unchanged. A statement issued against the table directly is depth 1 and is refused as
-- before, whatever it changes — including a hand-written `SET requested_by = NULL`.
--
-- ⭐ (c) COMPARES THE WHOLE ROW AS jsonb WITH THE ACTOR KEY REMOVED, rather than spelling
-- the columns out as 00101's proposal guard does. A column a later migration adds is then
-- covered without this function being touched again — a spelled-out list is a freeze with
-- a hole the day somebody forgets it. The label column is in the comparison, so it stays
-- exactly as it was: the name is the record, the id is only who it was.
--
-- 00101's second guard — a proposal is never rewritten, applied or not — is kept verbatim
-- below the first, so an UNapplied Suggestion is as frozen in its proposal as it was.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Two function bodies replaced in place; no table, column
-- or constraint changes, so release N-1 runs against it unchanged. The Down restores both
-- bodies exactly.

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION investigations_refuse_change_once_ended() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status IN ('completed','exhausted','failed','skipped') THEN
    -- ⭐ THE ACTOR LEAVING: `ON DELETE SET NULL` from users, nested in the FK's trigger.
    IF pg_trigger_depth() > 1 AND NEW.requested_by IS NULL
       AND (to_jsonb(NEW) - 'requested_by') = (to_jsonb(OLD) - 'requested_by') THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'investigation % has ended (%) and is frozen: an Investigation is never rewritten', OLD.id, OLD.status;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION investigation_suggestions_refuse_rewrite() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.applied_at IS NOT NULL THEN
    -- ⭐ THE ACTOR LEAVING: `ON DELETE SET NULL` from users, nested in the FK's trigger.
    IF pg_trigger_depth() > 1 AND NEW.applied_by IS NULL
       AND (to_jsonb(NEW) - 'applied_by') = (to_jsonb(OLD) - 'applied_by') THEN
      RETURN NEW;
    END IF;
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

-- +goose Down

-- The two bodies exactly as 00096 and 00101 wrote them. A user deletion that would null an
-- actor on a frozen row aborts again below this migration, which is what release N-1 did.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION investigations_refuse_change_once_ended() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status IN ('completed','exhausted','failed','skipped') THEN
    RAISE EXCEPTION 'investigation % has ended (%) and is frozen: an Investigation is never rewritten', OLD.id, OLD.status;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION investigation_suggestions_refuse_rewrite() RETURNS trigger
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
