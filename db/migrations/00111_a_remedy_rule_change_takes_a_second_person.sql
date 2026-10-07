-- ADR 0054 §3, amended 2026-10-06 (owner ruling O3, reversing the 2026-10-05 "host shell only"
-- ruling for the Settings screen): A CHANGE TO THE REMEDY RISK RULES MADE FROM THE APP IS A
-- PENDING CHANGE, AND IT TAKES A DIFFERENT PERSON TO CONFIRM IT.
--
-- WHY A TABLE AND NOT A ROUTE THAT WRITES. 5ace8f3 let any member replace the rules over HTTP, and
-- that was the loophole: a rule saying ONE lets one grant holder approve a Remedy alone, so writing
-- one is the authority of granting a second approver. Rules only ever LOOSEN (oto ships none, and
-- with none every Remedy needs two), so no change is exempt. A proposal is therefore a row here and
-- nothing else: it changes no tier. Only a confirmation by a second member writes
-- `remedy_risk_rules` / `remedy_risk_settings`, and the one writer of those stays in `internal/app`.
--
-- ⛔⛔ THE SECOND PERSON IS A CHECK, NOT A CONVENTION. `remedy_risk_changes_two_people_ck` refuses an
-- applied row whose confirmer is its proposer, so a bug in the Go that decides it cannot write the
-- row that would let one person do both. A user who is later deleted nulls both ids, and the CHECK
-- admits that (NULL is not equal to NULL), because the labels are frozen beside them.
--
-- ⭐ WHAT A CONFIRMER SEES IS WHAT APPLIES. The rules and the risk model are frozen on the row by a
-- trigger, so a pending change cannot be edited after someone has read it; a different change is a
-- NEW row that supersedes it, and a confirmation names the row's id, so confirming a change that
-- was superseded meanwhile is refused rather than applying the newer one.
--
-- ⭐ ONE PENDING CHANGE PER ORG (partial unique index). Proposing again supersedes the pending one,
-- recorded as such; an `oto remedy-rules apply` from the host shell supersedes it too, since the
-- shell's word is the later one and a stale proposal must not overwrite it when someone confirms.
--
-- `risk_model_provider_id` is NO ACTION: an endpoint a pending change names cannot be deleted out
-- from under it. SET NULL would turn "use this model" into "use none" without anyone saying so.
--
-- NO DEFAULT now() (CONTEXT.md §6): every timestamp is the application's.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One new table release N-1 never reads or writes.

-- +goose Up

CREATE TABLE remedy_risk_changes (
  id                     UUID        PRIMARY KEY,
  org_id                 UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  -- The rules as proposed, in order, each in `RiskRuleJSON`'s shape. Re-validated by the domain
  -- when confirmed, so a rule the domain has since tightened is refused then, not written.
  rules                  JSONB       NOT NULL,
  risk_model_provider_id UUID        REFERENCES model_providers(id),
  status                 TEXT        NOT NULL,
  -- ACTOR metadata in the acked_by mould: nulled when the user goes, the label frozen.
  proposed_by            UUID        REFERENCES users(id) ON DELETE SET NULL,
  proposed_by_label      TEXT        NOT NULL,
  proposed_at            TIMESTAMPTZ NOT NULL,
  decided_by             UUID        REFERENCES users(id) ON DELETE SET NULL,
  decided_by_label       TEXT,
  decided_at             TIMESTAMPTZ,
  CONSTRAINT remedy_risk_changes_status_ck CHECK (status IN ('pending','applied','discarded','superseded')),
  CONSTRAINT remedy_risk_changes_rules_ck  CHECK (jsonb_typeof(rules) = 'array' AND jsonb_array_length(rules) <= 100),
  CONSTRAINT remedy_risk_changes_labels_ck CHECK (
    length(btrim(proposed_by_label)) BETWEEN 1 AND 200
    AND (decided_by_label IS NULL OR length(btrim(decided_by_label)) BETWEEN 1 AND 200)
  ),
  -- Decided exactly when it is not pending, and then with a label and a time.
  CONSTRAINT remedy_risk_changes_decided_ck CHECK (
    (status = 'pending') = (decided_at IS NULL)
    AND (status = 'pending') = (decided_by_label IS NULL)
    AND (decided_at IS NULL OR decided_at >= proposed_at)
  ),
  -- ⛔⛔ The whole point: an applied change was confirmed by someone other than its proposer.
  CONSTRAINT remedy_risk_changes_two_people_ck CHECK (
    status <> 'applied' OR decided_by IS DISTINCT FROM proposed_by OR decided_by IS NULL
  )
);

-- Serves: "the org's pending change", and at most one.
CREATE UNIQUE INDEX remedy_risk_changes_one_pending_uniq ON remedy_risk_changes (org_id) WHERE status = 'pending';
-- Serves: the recent history under the screen, newest first.
CREATE INDEX remedy_risk_changes_org_time_idx ON remedy_risk_changes (org_id, proposed_at DESC);
CREATE INDEX remedy_risk_changes_provider_idx ON remedy_risk_changes (risk_model_provider_id)
  WHERE risk_model_provider_id IS NOT NULL;
CREATE INDEX remedy_risk_changes_proposed_by_idx ON remedy_risk_changes (proposed_by) WHERE proposed_by IS NOT NULL;
CREATE INDEX remedy_risk_changes_decided_by_idx ON remedy_risk_changes (decided_by) WHERE decided_by IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION remedy_risk_changes_refuse_rewrite() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'pending' THEN
    RAISE EXCEPTION 'remedy risk change % was % at % and is frozen', OLD.id, OLD.status, OLD.decided_at;
  END IF;
  IF (NEW.id, NEW.org_id, NEW.rules, NEW.risk_model_provider_id, NEW.proposed_by_label, NEW.proposed_at)
     IS DISTINCT FROM
     (OLD.id, OLD.org_id, OLD.rules, OLD.risk_model_provider_id, OLD.proposed_by_label, OLD.proposed_at) THEN
    RAISE EXCEPTION 'remedy risk change % is a proposal and is never rewritten: what a confirmer read is what applies', OLD.id;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER remedy_risk_changes_once
  BEFORE UPDATE ON remedy_risk_changes
  FOR EACH ROW EXECUTE FUNCTION remedy_risk_changes_refuse_rewrite();

-- +goose StatementBegin
COMMENT ON TABLE remedy_risk_changes IS
  'ADR 0054 §3 (00111, ruling O3): a proposed replacement of the org''s Remedy risk rules and risk model, made from Settings. It changes no Remedy''s tier. It applies only when a DIFFERENT member confirms it (remedy_risk_changes_two_people_ck), which writes remedy_risk_rules and remedy_risk_settings from internal/app and marks this row applied. One pending per org; a newer proposal or an `oto remedy-rules apply` supersedes it. The rules are frozen once proposed.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ PENDING CHANGES GO WITH THEIR TABLE. Applied ones already wrote `remedy_risk_rules`, which stays:
-- the release below reads rules, and only this record of who proposed and confirmed them goes.
DROP TRIGGER remedy_risk_changes_once ON remedy_risk_changes;
DROP FUNCTION remedy_risk_changes_refuse_rewrite();
DROP TABLE remedy_risk_changes;
