-- ADR 0054 §3: A SINGLE APPROVAL NAMES ITS TOOL, AND ITS RULE (judgment 2 on the Remedy review,
-- C1+C3 and C11). Two CHECKs, and the rows the first would refuse:
--
--   1. `remedy_risk_rules_single_names_tool_ck` — a rule that says ONE names the write Tool it is
--      about. Whether a Tool's arguments are kubectl's command line is something only the operator
--      knows; a one-approval rule about `payments` with no Tool read a helm-shaped `args` array, or
--      a delete Tool's `{"verb":"rollout restart"}`, as the restart it names. A rule saying TWO may
--      still omit it: a broader two only ever raises. Go refuses the same rule
--      (domain.NewRiskRules, `single_needs_tool`), so `oto remedy-rules apply` names the line.
--   2. `remedies_one_needs_a_rule_ck` — a Remedy that names a Tool needs ONE approval only with a
--      risk record behind it. 00107's `remedies_risk_tier_ck` admits any tier while `risk_basis` is
--      NULL (its first arm), and `remedies_approvals_ck` admits 1, so the schema alone let a
--      Tool-naming Remedy stand at one with nothing saying why.
--
-- ⭐⭐ EVERY STORED ONE-APPROVAL RULE WITH NO TOOL IS RAISED TO TWO, AND RECORDED. Deleting it would
-- lose the operator's conditions; guessing its Tool is guessing; leaving it at one is the bypass. Two
-- is what such a rule's commands now get anyway — Go skips it (RiskRules.Evaluate) — so raising it
-- changes no tier oto would give, keeps the rule readable in `oto remedy-rules show`, and leaves the
-- operator to re-apply it with a `tool:`. Each raised rule is a row in
-- `remedy_risk_rules_raised_by_00110`, which is what lets the Down put it back exactly.
--
-- ⚠️ NO REMEDY IS RE-TIERED (00107: a Remedy's tier is frozen at its proposal). A Remedy proposed at
-- one under such a rule keeps its one; decline it if the rule should not have lowered it.
--
-- ⛔ NO DEFAULT now() (CONTEXT.md §6). Nothing here writes a time.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One new table release N-1 never reads; two CHECKs. Every existing
-- Remedy satisfies the second (a Tool-naming Remedy proposed before 00107 needed two — every Remedy
-- did until eb4f21b, and since 00107 every one records its basis). A release N-1 `remedy-rules apply`
-- of a Tool-less one-approval rule fails on the first, naming it: the safe failure.

-- +goose Up

CREATE TABLE remedy_risk_rules_raised_by_00110 (
  org_id UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name   TEXT NOT NULL,
  CONSTRAINT remedy_risk_rules_raised_by_00110_pk PRIMARY KEY (org_id, name)
);

-- +goose StatementBegin
COMMENT ON TABLE remedy_risk_rules_raised_by_00110 IS
  'ADR 0054 §3 (00110; judgment 2, C1+C3): the risk rules 00110 raised from 1 to 2 approvals because they named no write Tool — a rule that says one must name the Tool it is about. Read-only history: re-apply such a rule with a `tool:` to lower it again. 00110''s Down reads it to restore exactly what it raised.';
-- +goose StatementEnd

INSERT INTO remedy_risk_rules_raised_by_00110 (org_id, name)
SELECT org_id, name FROM remedy_risk_rules WHERE approvals = 1 AND tool IS NULL;

UPDATE remedy_risk_rules SET approvals = 2 WHERE approvals = 1 AND tool IS NULL;

ALTER TABLE remedy_risk_rules
  ADD CONSTRAINT remedy_risk_rules_single_names_tool_ck CHECK (approvals = 2 OR tool IS NOT NULL);

ALTER TABLE remedies
  ADD CONSTRAINT remedies_one_needs_a_rule_ck CHECK (
    tool_server_id IS NULL OR risk_basis IS NOT NULL OR required_approvals = 2
  );

-- +goose Down

ALTER TABLE remedies DROP CONSTRAINT remedies_one_needs_a_rule_ck;

ALTER TABLE remedy_risk_rules DROP CONSTRAINT remedy_risk_rules_single_names_tool_ck;

-- ⭐ Exactly the rules Up raised, each while it is still the Tool-less two Up left it. (A rule
-- re-applied since under the same name, Tool-less at two, cannot be told apart and is lowered with
-- them: the state release N-1 ran with.)
UPDATE remedy_risk_rules r SET approvals = 1
  FROM remedy_risk_rules_raised_by_00110 x
 WHERE r.org_id = x.org_id AND r.name = x.name AND r.tool IS NULL AND r.approvals = 2;

DROP TABLE remedy_risk_rules_raised_by_00110;
