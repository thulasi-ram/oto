-- ADR 0054 §3: AN OPERATOR'S RULES SAY HOW MANY APPROVALS A REMEDY NEEDS, AND A MODEL MAY ONLY
-- ASK FOR MORE (git-bug eb4f21b). "Operator-written rules over the command (verb, resource
-- kind, namespace, reversibility) set the baseline. A model may then move a Remedy from single
-- to double approval, never back. Anything the rules cannot parse — `sh -c`, pipes — is double
-- approval." Four changes:
--
--   1. `remedy_risk_rules` — the org's rules, in the operator's order, replaced whole.
--   2. `remedy_risk_settings` — one row per org that has written them: the model endpoint asked
--      to raise a single-approval Remedy (NULL: none, and the rules' tier stands), and who last
--      wrote the rules, when.
--   3. `remedies` gains the record of how its `required_approvals` was set: the basis (a rule,
--      no rule, or an unparseable command), the rule's NAME, why, what the risk model did, which
--      model, and what asking it cost. `remedies_risk_tier_ck` is the rule in the schema: ONE
--      approval only when a rule said so and the model, if asked, kept it.
--   4. `remedies_refuse_rewrite` freezes the new columns with the rest of the proposal.
--
-- ⭐⭐ THE MOST SEVERE MATCHING RULE WINS, AND NO MATCH IS TWO. The order of the rules decides
-- only which rule is NAMED on a Remedy, never its tier: a rule saying two cannot be outvoted by
-- one above it. That is Go's (domain.RiskRules.Evaluate); the schema holds what can be held
-- without the command — a single-approval Remedy names its rule, and an unparseable or
-- unmatched one needs two.
--
-- ⚠️ A RULE IS COPIED ONTO A REMEDY BY NAME, NEVER BY A FOREIGN KEY. Rules are replaced whole;
-- a Remedy keeps the name of the rule that set its tier as the rules stood at its proposal, and
-- changing the rules re-tiers no Remedy already proposed.
--
-- ⛔ NO DEFAULT now() ON ANY TIMESTAMP (CONTEXT.md §6): every time here is the app's clock.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Two new tables release N-1 never reads; six nullable columns
-- on `remedies` whose CHECKs every existing row satisfies (each is NULL there, and a Remedy
-- proposed before this migration says so on the screen); one trigger function whose new body
-- compares more columns; two comments.

-- +goose Up

CREATE TABLE remedy_risk_rules (
  org_id        UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name          TEXT        NOT NULL,
  -- The operator's order: which rule is NAMED when several match. Dense from 0.
  position      INT         NOT NULL,
  -- A qualified write Tool, `<toolserver>__<tool>`; NULL for any.
  tool          TEXT,
  -- Each list holds when the command's verb / kind / namespace is in it; empty for any.
  verbs         TEXT[]      NOT NULL,
  kinds         TEXT[]      NOT NULL,
  namespaces    TEXT[]      NOT NULL,
  -- NULL for either.
  reversibility TEXT,
  approvals     INT         NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL,
  CONSTRAINT remedy_risk_rules_pk            PRIMARY KEY (org_id, name),
  CONSTRAINT remedy_risk_rules_name_ck       CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$'),
  CONSTRAINT remedy_risk_rules_position_ck   CHECK (position BETWEEN 0 AND 99),
  CONSTRAINT remedy_risk_rules_tool_ck       CHECK (tool IS NULL OR tool ~ '^[a-z]([a-z0-9-]{0,22}[a-z0-9])?__.{1,128}$'),
  CONSTRAINT remedy_risk_rules_lists_ck      CHECK (
    cardinality(verbs) <= 20 AND cardinality(kinds) <= 20 AND cardinality(namespaces) <= 20
    AND array_position(verbs, NULL) IS NULL AND array_position(kinds, NULL) IS NULL
    AND array_position(namespaces, NULL) IS NULL
  ),
  CONSTRAINT remedy_risk_rules_rev_ck        CHECK (reversibility IS NULL OR reversibility IN ('reversible','irreversible')),
  CONSTRAINT remedy_risk_rules_approvals_ck  CHECK (approvals IN (1, 2)),
  -- ⭐ A rule with no condition would match every command the rules can read: the default,
  -- lowered by accident. Refused here as in Go.
  CONSTRAINT remedy_risk_rules_condition_ck  CHECK (
    tool IS NOT NULL OR cardinality(verbs) > 0 OR cardinality(kinds) > 0
    OR cardinality(namespaces) > 0 OR reversibility IS NOT NULL
  )
);

-- Serves: the rules in the operator's order, and one rule per position.
CREATE UNIQUE INDEX remedy_risk_rules_position_uniq ON remedy_risk_rules (org_id, position);

-- +goose StatementBegin
COMMENT ON TABLE remedy_risk_rules IS
  'ADR 0054 §3 (00107, git-bug eb4f21b): the operator''s rules over a Remedy''s command — its write Tool, verb, resource kind, namespace and whether its verb is known reversible — each saying 1 or 2 approvals. The MOST SEVERE matching rule wins and is named after the first such in position order; no match is 2; a command the rules cannot parse (sh -c, a pipe, a redirect, a quote, an unknown flag…) is 2 whatever they say. A risk model may then raise 1 to 2, never lower. Replaced whole by the settings API. A Remedy copies the NAME of the rule that set its tier, so replacing the rules re-tiers no Remedy already proposed. oto ships no rule.';
-- +goose StatementEnd

CREATE TABLE remedy_risk_settings (
  org_id                 UUID        PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  -- The model endpoint asked to raise a single-approval Remedy; NULL for none. An endpoint
  -- that goes takes the setting with it: the rules' tier then stands, recorded as such.
  risk_model_provider_id UUID        REFERENCES model_providers(id) ON DELETE SET NULL,
  -- ACTOR metadata in the acked_by mould: nulled when the user goes, the label frozen.
  written_by             UUID        REFERENCES users(id) ON DELETE SET NULL,
  written_by_label       TEXT        NOT NULL,
  written_at             TIMESTAMPTZ NOT NULL,
  CONSTRAINT remedy_risk_settings_label_ck CHECK (length(btrim(written_by_label)) BETWEEN 1 AND 200)
);

CREATE INDEX remedy_risk_settings_provider_idx ON remedy_risk_settings (risk_model_provider_id)
  WHERE risk_model_provider_id IS NOT NULL;
CREATE INDEX remedy_risk_settings_user_idx ON remedy_risk_settings (written_by) WHERE written_by IS NOT NULL;

-- +goose StatementBegin
COMMENT ON TABLE remedy_risk_settings IS
  'ADR 0054 §3 (00107, git-bug eb4f21b): per org, the model endpoint asked whether a Remedy the rules said needs ONE approval should need two (NULL: no model is asked, and the rules'' tier stands), and who last replaced the risk rules and when. The model sees only the command, its target and the rules'' verdict — never an Investigation, Step, Finding, log line or Tool answer — and may only raise. Absent row: no rules were ever written.';
-- +goose StatementEnd

ALTER TABLE remedies
  ADD COLUMN risk_basis        TEXT,
  ADD COLUMN risk_rule         TEXT,
  ADD COLUMN risk_detail       TEXT,
  ADD COLUMN risk_model_check  TEXT,
  ADD COLUMN risk_model        TEXT,
  ADD COLUMN risk_model_tokens BIGINT,
  ADD CONSTRAINT remedies_risk_basis_ck  CHECK (risk_basis IS NULL OR risk_basis IN ('rule','no_rule','unparseable')),
  ADD CONSTRAINT remedies_risk_model_ck  CHECK (
    risk_model_check IS NULL OR risk_model_check IN ('unset','not_asked','kept','raised','failed')
  ),
  -- All six are set together or not at all; a Remedy with no Tool has none.
  ADD CONSTRAINT remedies_risk_set_ck    CHECK (
    (risk_basis IS NULL) = (risk_model_check IS NULL)
    AND (risk_basis IS NULL OR tool_server_id IS NOT NULL)
    AND (risk_basis IS NOT NULL OR (risk_rule IS NULL AND risk_detail IS NULL AND risk_model IS NULL AND risk_model_tokens IS NULL))
  ),
  ADD CONSTRAINT remedies_risk_rule_ck   CHECK (
    (risk_basis = 'rule') = (risk_rule IS NOT NULL)
    AND (risk_rule IS NULL OR risk_rule ~ '^[a-z][a-z0-9_-]{0,62}$')
  ),
  ADD CONSTRAINT remedies_risk_detail_ck CHECK (risk_detail IS NULL OR char_length(risk_detail) BETWEEN 1 AND 1000),
  ADD CONSTRAINT remedies_risk_model_used_ck CHECK (
    (risk_model_check IN ('kept','raised','failed') OR (risk_model IS NULL AND risk_model_tokens IS NULL))
    AND (risk_model IS NULL OR char_length(risk_model) BETWEEN 1 AND 2300)
    AND (risk_model_tokens IS NULL OR risk_model_tokens >= 0)
  ),
  -- ⭐⭐ ONE APPROVAL ONLY WHEN A RULE SAID SO AND NO MODEL RAISED IT. No match and unparseable
  -- are two and ask no model; a raise or a failure is two and stands on a rule that said one.
  ADD CONSTRAINT remedies_risk_tier_ck   CHECK (
    risk_basis IS NULL
    OR (required_approvals = 1 AND risk_basis = 'rule' AND risk_model_check IN ('unset','kept'))
    OR (required_approvals = 2 AND (
         (risk_basis IN ('no_rule','unparseable') AND risk_model_check IN ('unset','not_asked'))
      OR (risk_basis = 'rule' AND risk_model_check IN ('unset','not_asked','raised','failed'))))
  );

-- +goose StatementBegin
COMMENT ON COLUMN remedies.required_approvals IS
  'How many DIFFERENT grant holders must approve before it runs, set at proposal from the org''s risk rules (00107, git-bug eb4f21b): 1 only when a rule said so and the risk model, if one is configured, kept it; 2 when a rule said 2, no rule matched, the command could not be parsed, or the model raised it or failed. risk_basis, risk_rule and risk_model_check say which. A Remedy with no Tool, and one proposed before 00107, needs 2 and has no risk record. A model may only ever raise it (ADR 0054 §3).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN remedies.risk_basis IS
  'What set the baseline tier (00107): rule (risk_rule names it, as the rules stood at proposal — a copy of the NAME, never a foreign key), no_rule (2), or unparseable (2 whatever the rules say; risk_detail says why). NULL for a Remedy with no Tool or one proposed before 00107. risk_model_check says what the risk model then did: unset (none configured), not_asked (the baseline was already 2), kept, raised (risk_detail is its reason) or failed (2; risk_detail says why).';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION remedies_refuse_rewrite() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'proposed' THEN
      RAISE EXCEPTION 'remedy % is inserted %: a Remedy begins proposed, and every later state is a transition', NEW.id, NEW.state;
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.state IN ('executed','failed','declined','expired') THEN
    RAISE EXCEPTION 'remedy % is % and is frozen: a Remedy that ended is never rewritten, and a failed one is never retried', OLD.id, OLD.state;
  END IF;
  -- ⭐ 00107: how the tier was set is part of the proposal, frozen with it.
  IF (NEW.id, NEW.org_id, NEW.investigation_id, NEW.subject_kind, NEW.subject_id, NEW.proposed_by_label,
      NEW.tool_server_id, NEW.tool_server_name, NEW.tool_name, NEW.arguments, NEW.arguments_sha256,
      NEW.target, NEW.description, NEW.required_approvals, NEW.proposed_at,
      NEW.risk_basis, NEW.risk_rule, NEW.risk_detail, NEW.risk_model_check, NEW.risk_model, NEW.risk_model_tokens)
     IS DISTINCT FROM
     (OLD.id, OLD.org_id, OLD.investigation_id, OLD.subject_kind, OLD.subject_id, OLD.proposed_by_label,
      OLD.tool_server_id, OLD.tool_server_name, OLD.tool_name, OLD.arguments, OLD.arguments_sha256,
      OLD.target, OLD.description, OLD.required_approvals, OLD.proposed_at,
      OLD.risk_basis, OLD.risk_rule, OLD.risk_detail, OLD.risk_model_check, OLD.risk_model, OLD.risk_model_tokens) THEN
    RAISE EXCEPTION 'remedy % is a proposal and is never rewritten: what was approved is what is executed', OLD.id;
  END IF;
  IF NOT ((OLD.state = 'proposed'  AND NEW.state IN ('approved','declined','expired'))
       OR (OLD.state = 'approved'  AND NEW.state IN ('executing','declined','expired','failed'))
       OR (OLD.state = 'executing' AND NEW.state IN ('executed','failed'))) THEN
    RAISE EXCEPTION 'remedy % cannot move from % to %: every change to a Remedy is one of its transitions', OLD.id, OLD.state, NEW.state;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- Byte-identical to 00104's body.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION remedies_refuse_rewrite() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'proposed' THEN
      RAISE EXCEPTION 'remedy % is inserted %: a Remedy begins proposed, and every later state is a transition', NEW.id, NEW.state;
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.state IN ('executed','failed','declined','expired') THEN
    RAISE EXCEPTION 'remedy % is % and is frozen: a Remedy that ended is never rewritten, and a failed one is never retried', OLD.id, OLD.state;
  END IF;
  IF (NEW.id, NEW.org_id, NEW.investigation_id, NEW.subject_kind, NEW.subject_id, NEW.proposed_by_label,
      NEW.tool_server_id, NEW.tool_server_name, NEW.tool_name, NEW.arguments, NEW.arguments_sha256,
      NEW.target, NEW.description, NEW.required_approvals, NEW.proposed_at)
     IS DISTINCT FROM
     (OLD.id, OLD.org_id, OLD.investigation_id, OLD.subject_kind, OLD.subject_id, OLD.proposed_by_label,
      OLD.tool_server_id, OLD.tool_server_name, OLD.tool_name, OLD.arguments, OLD.arguments_sha256,
      OLD.target, OLD.description, OLD.required_approvals, OLD.proposed_at) THEN
    RAISE EXCEPTION 'remedy % is a proposal and is never rewritten: what was approved is what is executed', OLD.id;
  END IF;
  IF NOT ((OLD.state = 'proposed'  AND NEW.state IN ('approved','declined','expired'))
       OR (OLD.state = 'approved'  AND NEW.state IN ('executing','declined','expired','failed'))
       OR (OLD.state = 'executing' AND NEW.state IN ('executed','failed'))) THEN
    RAISE EXCEPTION 'remedy % cannot move from % to %: every change to a Remedy is one of its transitions', OLD.id, OLD.state, NEW.state;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Byte-identical to what 00104 shipped.
-- +goose StatementBegin
COMMENT ON COLUMN remedies.required_approvals IS
  'How many DIFFERENT grant holders must approve before it runs, set at proposal. 2 for every Remedy until operator-written risk rules exist (git-bug eb4f21b), which may lower it per Remedy; a model may only ever raise it (ADR 0054 §3).';
-- +goose StatementEnd

-- ⛔ A REMEDY'S RISK RECORD GOES WITH ITS COLUMNS. The release below reads no tier but 2's
-- column; a Remedy a rule lowered to 1 keeps required_approvals = 1, which 00104's CHECK
-- admits and its executor honours — the rule that set it is what is lost.
ALTER TABLE remedies
  DROP CONSTRAINT remedies_risk_tier_ck,
  DROP CONSTRAINT remedies_risk_model_used_ck,
  DROP CONSTRAINT remedies_risk_detail_ck,
  DROP CONSTRAINT remedies_risk_rule_ck,
  DROP CONSTRAINT remedies_risk_set_ck,
  DROP CONSTRAINT remedies_risk_model_ck,
  DROP CONSTRAINT remedies_risk_basis_ck,
  DROP COLUMN risk_model_tokens,
  DROP COLUMN risk_model,
  DROP COLUMN risk_model_check,
  DROP COLUMN risk_detail,
  DROP COLUMN risk_rule,
  DROP COLUMN risk_basis;

DROP TABLE remedy_risk_settings;
DROP TABLE remedy_risk_rules;
