package repository_test

// THE REMEDY RISK RULES AND A REMEDY'S RISK RECORD AGAINST A REAL POSTGRES (migration 00107,
// git-bug eb4f21b). What the SQL holds on its own: a fresh org has no rules; the rules read in
// the operator's order, with who wrote them (`oto remedy-rules apply` — the repository only
// reads); a rule with no condition cannot be a row; a Remedy's risk record round-trips, is
// frozen with its proposal, and ONE approval stands only on a rule the model, if asked, kept —
// never on one whose question the day's budget could not pay for (00108), whose tokens count in
// the day's spend.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/errs"
)

func TestTheRiskRulesAreReadInOrderWithTheirWriter(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewRemedyRiskRepository(w.h.Pool)

	// ⭐ oto ships no rule.
	got, err := repo.RemedyRisk(w.h.Ctx, w.scope)
	require.NoError(t, err)
	require.Empty(t, got.Rules.Rules())
	require.Equal(t, "", got.WrittenByLabel)

	// ⛔ The repository only reads (owner ruling 2026-10-05): `oto remedy-rules apply` writes,
	// in internal/app. These rows are what it writes.
	_, err = w.h.Pool.Exec(w.h.Ctx, `
INSERT INTO remedy_risk_settings (org_id, risk_model_provider_id, written_by, written_by_label, written_at)
VALUES ($1, NULL, NULL, 'oto remedy-rules apply', $2)`, w.scope.OrgID(), w.h.Now())
	require.NoError(t, err)
	_, err = w.h.Pool.Exec(w.h.Ctx, `
INSERT INTO remedy_risk_rules (org_id, name, position, tool, verbs, kinds, namespaces, reversibility, approvals, created_at)
VALUES ($1, 'secrets-need-two', 1, 'k8s-write__kubectl', '{}', '{secret}', '{}', 'irreversible', 2, $2),
       ($1, 'restart-payments', 0, 'k8s-write__kubectl', '{rollout restart}', '{deployment}', '{payments}', NULL, 1, $2)`,
		w.scope.OrgID(), w.h.Now())
	require.NoError(t, err)

	got, err = repo.RemedyRisk(w.h.Ctx, w.scope)
	require.NoError(t, err)
	rules := got.Rules.Rules()
	require.Len(t, rules, 2)
	require.Equal(t, "restart-payments", rules[0].Name, "in the operator's order")
	require.Equal(t, "k8s-write__kubectl", rules[1].Tool)
	require.Equal(t, domain.Irreversible, rules[1].Reversibility)
	require.Equal(t, "oto remedy-rules apply", got.WrittenByLabel)

	other, err := repo.RemedyRisk(w.h.Ctx, w.h.Org().Scope)
	require.NoError(t, err)
	require.Empty(t, other.Rules.Rules(), "another org reads none of them")

	// ⛔ A rule with no condition is not a row.
	_, err = w.h.Pool.Exec(w.h.Ctx, `
INSERT INTO remedy_risk_rules (org_id, name, position, tool, verbs, kinds, namespaces, reversibility, approvals, created_at)
VALUES ($1, 'everything', 2, NULL, '{}', '{}', '{}', NULL, 1, $2)`, w.scope.OrgID(), w.h.Now())
	require.Equal(t, "remedy_risk_rules_condition_ck", errs.CodeOf(mapPG(err)))

	// ⛔ Nor is a rule that lowers to one and names no Tool (00110; judgment 2, C1+C3).
	_, err = w.h.Pool.Exec(w.h.Ctx, `
INSERT INTO remedy_risk_rules (org_id, name, position, tool, verbs, kinds, namespaces, reversibility, approvals, created_at)
VALUES ($1, 'payments-one', 2, NULL, '{}', '{}', '{payments}', NULL, 1, $2)`, w.scope.OrgID(), w.h.Now())
	require.Equal(t, "remedy_risk_rules_single_names_tool_ck", errs.CodeOf(mapPG(err)))
}

func TestARemedysRiskRecordRoundTripsIsFrozenAndOneApprovalStandsOnlyOnARule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewRemedyRepository(w.h.Pool)
	run := w.queued(t, "k", w.h.Now())
	at := w.h.Now()
	r := domain.Remedy{ID: uuid.New(), OrgID: w.scope.OrgID(), InvestigationID: run.ID,
		SubjectKind: domain.SubjectCase, SubjectID: run.SubjectID, ProposedBy: "Investigator firstlook v1",
		Target: "Deployment payments/api", Description: "Restart it.", RequiredApprovals: 1,
		Risk: domain.RemedyRisk{Approvals: 1, Basis: domain.BasisRule, Rule: "restart-payments", Model: domain.ModelKept,
			ModelIdentity: "https://risk.test/v1#risk-m", ModelTokens: 135},
		Tool:  domain.RemedyTool{ToolServerID: uuid.New(), ToolServerName: "k8s-write", Tool: "kubectl"},
		State: domain.RemedyProposed, ProposedAt: at, ExpiresAt: at.Add(time.Hour)}
	r.Arguments, r.ArgumentsSHA256 = dbRemedyArgs, domain.HashArguments(dbRemedyArgs)
	proposal := domain.RemedyTransition{ID: uuid.New(), To: domain.RemedyProposed,
		Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: r.ProposedBy}, At: at}
	require.NoError(t, repo.InsertRemedy(w.h.Ctx, w.scope, r, proposal))

	got, err := repo.GetRemedy(w.h.Ctx, w.scope, r.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.RequiredApprovals)
	require.Equal(t, r.Risk, got.Risk)

	// ⛔ The risk record is part of the proposal, frozen with it.
	_, err = w.h.Pool.Exec(w.h.Ctx,
		`UPDATE remedies SET risk_rule = 'anything-else', state = 'declined', ended_at = proposed_at WHERE id = $1`, r.ID)
	require.Error(t, err)

	// ⛔⛔ ONE APPROVAL ONLY ON A RULE THE MODEL KEPT: no match at one, unparseable at one, and a
	// raise left at one are each refused at the row.
	for _, risk := range []domain.RemedyRisk{
		{Approvals: 1, Basis: domain.BasisNoRule, Model: domain.ModelUnset},
		{Approvals: 1, Basis: domain.BasisUnparseable, Detail: "sh", Model: domain.ModelUnset},
		{Approvals: 1, Basis: domain.BasisRule, Rule: "x", Detail: "raised", Model: domain.ModelRaised},
		{Approvals: 1, Basis: domain.BasisRule, Rule: "x", Detail: "failed", Model: domain.ModelFailed},
		{Approvals: 1, Basis: domain.BasisRule, Rule: "x", Detail: "budget", Model: domain.ModelBudget},
	} {
		run := w.queued(t, uuid.NewString(), w.h.Now())
		bad := r
		bad.ID, bad.InvestigationID, bad.SubjectID, bad.Risk = uuid.New(), run.ID, run.SubjectID, risk
		err := repo.InsertRemedy(w.h.Ctx, w.scope, bad, domain.RemedyTransition{ID: uuid.New(), To: domain.RemedyProposed,
			Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: r.ProposedBy}, At: at})
		require.Equal(t, "remedies_risk_tier_ck", errs.CodeOf(err), "%+v", risk)
	}
}

// TestASpentBudgetsRemedyRoundTripsAtTwo — 00108 (owner ruling 2026-10-05 on git-bug eb4f21b):
// a Remedy whose risk question the day's budget could not pay for is a row at TWO approvals,
// recorded `budget`, asking no model and costing nothing.
func TestASpentBudgetsRemedyRoundTripsAtTwo(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewRemedyRepository(w.h.Pool)
	run := w.queued(t, "k", w.h.Now())
	at := w.h.Now()
	verdict := domain.RiskVerdict{Approvals: 1, Basis: domain.BasisRule, Rule: "restart-payments"}
	r := domain.Remedy{ID: uuid.New(), OrgID: w.scope.OrgID(), InvestigationID: run.ID,
		SubjectKind: domain.SubjectCase, SubjectID: run.SubjectID, ProposedBy: "Investigator firstlook v1",
		Target: "Deployment payments/api", Description: "Restart it.", RequiredApprovals: 2,
		Risk:  verdict.BudgetSpent("this org has spent 1000 of its 1000 daily Investigation tokens"),
		Tool:  domain.RemedyTool{ToolServerID: uuid.New(), ToolServerName: "k8s-write", Tool: "kubectl"},
		State: domain.RemedyProposed, ProposedAt: at, ExpiresAt: at.Add(time.Hour)}
	r.Arguments, r.ArgumentsSHA256 = dbRemedyArgs, domain.HashArguments(dbRemedyArgs)
	require.NoError(t, repo.InsertRemedy(w.h.Ctx, w.scope, r, domain.RemedyTransition{ID: uuid.New(),
		To: domain.RemedyProposed, Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: r.ProposedBy}, At: at}))
	got, err := repo.GetRemedy(w.h.Ctx, w.scope, r.ID)
	require.NoError(t, err)
	require.Equal(t, 2, got.RequiredApprovals)
	require.Equal(t, r.Risk, got.Risk)
	require.Equal(t, "risk_model_budget", got.Risk.SetBy())
}

// TestTheRiskModelsTokensAreSummedIntoTheDaysSpend — owner ruling 2026-10-05 on git-bug
// eb4f21b: what a Remedy's risk question cost counts against the org's daily budget beside every
// model turn, from the day it was proposed; yesterday's and another org's do not.
func TestTheRiskModelsTokensAreSummedIntoTheDaysSpend(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	remedies := repository.NewRemedyRepository(w.h.Pool)
	runs := repository.NewInvestigationRepository(w.h.Pool)
	midnight := domain.DayStart(w.h.Now())
	insert := func(at time.Time, tokens int64) {
		t.Helper()
		run := w.queued(t, uuid.NewString(), at)
		r := domain.Remedy{ID: uuid.New(), OrgID: w.scope.OrgID(), InvestigationID: run.ID,
			SubjectKind: domain.SubjectCase, SubjectID: run.SubjectID, ProposedBy: "Investigator firstlook v1",
			Target: "Deployment payments/api", Description: "Restart it.", RequiredApprovals: 1,
			Risk: domain.RemedyRisk{Approvals: 1, Basis: domain.BasisRule, Rule: "restart-payments", Model: domain.ModelKept,
				ModelIdentity: "https://risk.test/v1#risk-m", ModelTokens: tokens},
			Tool:  domain.RemedyTool{ToolServerID: uuid.New(), ToolServerName: "k8s-write", Tool: "kubectl"},
			State: domain.RemedyProposed, ProposedAt: at, ExpiresAt: at.Add(time.Hour)}
		r.Arguments, r.ArgumentsSHA256 = dbRemedyArgs, domain.HashArguments(dbRemedyArgs)
		require.NoError(t, remedies.InsertRemedy(w.h.Ctx, w.scope, r, domain.RemedyTransition{ID: uuid.New(),
			To: domain.RemedyProposed, Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: r.ProposedBy}, At: at}))
	}
	insert(midnight.Add(-time.Minute), 1000) // yesterday's
	insert(midnight.Add(time.Hour), 135)
	insert(midnight.Add(2*time.Hour), 40)

	spent, err := runs.SpentSince(w.h.Ctx, w.scope, midnight)
	require.NoError(t, err)
	require.Equal(t, int64(175), spent, "today's two risk questions, and not yesterday's")

	other, err := runs.SpentSince(w.h.Ctx, w.h.Org().Scope, midnight)
	require.NoError(t, err)
	require.Zero(t, other, "another org's spend is not this org's")
}
