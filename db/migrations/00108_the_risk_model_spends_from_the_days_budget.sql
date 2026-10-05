-- ADR 0054 §3, OWNER RULING 2026-10-05 ON git-bug eb4f21b: THE RISK MODEL SPENDS FROM THE DAY'S
-- BUDGET, AND THE RULES ARE WRITTEN FROM THE HOST SHELL. Three changes:
--
--   1. `remedies.risk_model_check` admits `budget`: the rules said ONE, a risk model is
--      configured, and the org's daily token budget (ADR 0053 §6) was already spent, so the model
--      was NOT asked and the Remedy needs TWO. `remedies_risk_model_ck` admits the word and
--      `remedies_risk_tier_ck` admits it on the two-approval arm ONLY — a question nobody asked
--      never lets one approval stand (fail closed). `remedies_risk_model_used_ck` is unchanged
--      and already holds that a `budget` row names no model and cost nothing.
--   2. `remedies_risk_spend_idx` serves the risk model's half of "what has this org spent since
--      00:00 UTC" (InvestigationRepository.SpentSince), beside `investigation_steps_spend_idx`.
--   3. The comments on `remedy_risk_rules`, `remedy_risk_settings` and `remedies.risk_basis` say
--      who writes the rules now — `oto remedy-rules apply`, from the host shell; no route does —
--      and that the risk model's tokens are budgeted.
--
-- ⛔ NO DEFAULT now() (CONTEXT.md §6). Nothing here writes a time.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Two CHECKs widened by one word every existing row already
-- satisfies (no row says `budget`), one partial index on a table of proposals, three comments.
-- Release N-1 never writes `budget`; it reads the column as text.
--
-- ⚠️ THE INDEX IS BUILT IN THE MIGRATION'S TRANSACTION, not CONCURRENTLY: `remedies` holds one row
-- per proposed cluster change (00104), a table measured in hundreds, and the lock is momentary.

-- +goose Up

ALTER TABLE remedies
  DROP CONSTRAINT remedies_risk_model_ck,
  ADD CONSTRAINT remedies_risk_model_ck CHECK (
    risk_model_check IS NULL OR risk_model_check IN ('unset','not_asked','kept','raised','failed','budget')
  ),
  DROP CONSTRAINT remedies_risk_tier_ck,
  -- ⭐⭐ ONE APPROVAL ONLY WHEN A RULE SAID SO AND NO MODEL RAISED IT — and `budget` only at two.
  ADD CONSTRAINT remedies_risk_tier_ck CHECK (
    risk_basis IS NULL
    OR (required_approvals = 1 AND risk_basis = 'rule' AND risk_model_check IN ('unset','kept'))
    OR (required_approvals = 2 AND (
         (risk_basis IN ('no_rule','unparseable') AND risk_model_check IN ('unset','not_asked'))
      OR (risk_basis = 'rule' AND risk_model_check IN ('unset','not_asked','raised','failed','budget'))))
  );

-- Serves: the tokens this org's risk questions cost since 00:00 UTC — one index range, and only
-- the rows that asked a model.
CREATE INDEX remedies_risk_spend_idx ON remedies (org_id, proposed_at)
  WHERE risk_model_tokens IS NOT NULL;

-- +goose StatementBegin
COMMENT ON TABLE remedy_risk_rules IS
  'ADR 0054 §3 (00107, git-bug eb4f21b): the operator''s rules over a Remedy''s command — its write Tool, verb, resource kind, namespace and whether its verb is known reversible — each saying 1 or 2 approvals. The MOST SEVERE matching rule wins and is named after the first such in position order; no match is 2; a command the rules cannot parse (sh -c, a pipe, a redirect, a quote, an unknown flag…) is 2 whatever they say. A risk model may then raise 1 to 2, never lower. Replaced whole by `oto remedy-rules apply` from the host shell ONLY (00108, the ruling of 2026-10-05 on git-bug eb4f21b): a rule saying 1 lets one grant holder approve alone, so no HTTP route writes one. A Remedy copies the NAME of the rule that set its tier, so replacing the rules re-tiers no Remedy already proposed. oto ships no rule.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON TABLE remedy_risk_settings IS
  'ADR 0054 §3 (00107, git-bug eb4f21b): per org, the model endpoint asked whether a Remedy the rules said needs ONE approval should need two (NULL: no model is asked, and the rules'' tier stands), and who last replaced the risk rules and when — `oto remedy-rules apply`, from the host shell (00108), with written_by NULL. The model sees only the command, its target and the rules'' verdict — never an Investigation, Step, Finding, log line or Tool answer — and may only raise. Its tokens count against the org''s daily token budget; when that is spent it is not asked and the Remedy needs two (risk_model_check = budget). Absent row: no rules were ever written.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN remedies.risk_basis IS
  'What set the baseline tier (00107): rule (risk_rule names it, as the rules stood at proposal — a copy of the NAME, never a foreign key), no_rule (2), or unparseable (2 whatever the rules say; risk_detail says why). NULL for a Remedy with no Tool or one proposed before 00107. risk_model_check says what the risk model then did: unset (none configured), not_asked (the baseline was already 2), kept, raised (risk_detail is its reason), failed (2; risk_detail says why) or budget (00108: the org''s daily token budget was spent, so it was not asked: 2; risk_detail says so). risk_model_tokens count against that budget from proposed_at.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ A `budget` row cannot survive a CHECK that does not know the word. It needed two approvals
-- because nothing was asked; the release below records that as `failed` — two, the model gave
-- no answer oto could take — which is true of it and keeps its tier, so no approval it holds is
-- re-read. risk_model_check is frozen with the proposal, so `remedies_frozen` (00104) is stepped
-- around for this one rewrite, inside the Down's own transaction: nothing else can write a
-- Remedy between the two ALTERs.
ALTER TABLE remedies DISABLE TRIGGER remedies_frozen;
UPDATE remedies SET risk_model_check = 'failed' WHERE risk_model_check = 'budget';
ALTER TABLE remedies ENABLE TRIGGER remedies_frozen;

-- Byte-identical to what 00107 shipped.
-- +goose StatementBegin
COMMENT ON COLUMN remedies.risk_basis IS
  'What set the baseline tier (00107): rule (risk_rule names it, as the rules stood at proposal — a copy of the NAME, never a foreign key), no_rule (2), or unparseable (2 whatever the rules say; risk_detail says why). NULL for a Remedy with no Tool or one proposed before 00107. risk_model_check says what the risk model then did: unset (none configured), not_asked (the baseline was already 2), kept, raised (risk_detail is its reason) or failed (2; risk_detail says why).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON TABLE remedy_risk_settings IS
  'ADR 0054 §3 (00107, git-bug eb4f21b): per org, the model endpoint asked whether a Remedy the rules said needs ONE approval should need two (NULL: no model is asked, and the rules'' tier stands), and who last replaced the risk rules and when. The model sees only the command, its target and the rules'' verdict — never an Investigation, Step, Finding, log line or Tool answer — and may only raise. Absent row: no rules were ever written.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON TABLE remedy_risk_rules IS
  'ADR 0054 §3 (00107, git-bug eb4f21b): the operator''s rules over a Remedy''s command — its write Tool, verb, resource kind, namespace and whether its verb is known reversible — each saying 1 or 2 approvals. The MOST SEVERE matching rule wins and is named after the first such in position order; no match is 2; a command the rules cannot parse (sh -c, a pipe, a redirect, a quote, an unknown flag…) is 2 whatever they say. A risk model may then raise 1 to 2, never lower. Replaced whole by the settings API. A Remedy copies the NAME of the rule that set its tier, so replacing the rules re-tiers no Remedy already proposed. oto ships no rule.';
-- +goose StatementEnd

DROP INDEX remedies_risk_spend_idx;

ALTER TABLE remedies
  DROP CONSTRAINT remedies_risk_tier_ck,
  ADD CONSTRAINT remedies_risk_tier_ck CHECK (
    risk_basis IS NULL
    OR (required_approvals = 1 AND risk_basis = 'rule' AND risk_model_check IN ('unset','kept'))
    OR (required_approvals = 2 AND (
         (risk_basis IN ('no_rule','unparseable') AND risk_model_check IN ('unset','not_asked'))
      OR (risk_basis = 'rule' AND risk_model_check IN ('unset','not_asked','raised','failed'))))
  ),
  DROP CONSTRAINT remedies_risk_model_ck,
  ADD CONSTRAINT remedies_risk_model_ck CHECK (
    risk_model_check IS NULL OR risk_model_check IN ('unset','not_asked','kept','raised','failed')
  );
