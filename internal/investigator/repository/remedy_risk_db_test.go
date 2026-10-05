package repository_test

// THE REMEDY RISK RULES AND A REMEDY'S RISK RECORD AGAINST A REAL POSTGRES (migration 00103,
// git-bug eb4f21b). What the SQL holds on its own: a fresh org has no rules; the rules are
// replaced whole in the operator's order, with who wrote them; a rule with no condition cannot
// be a row; a Remedy's risk record round-trips, is frozen with its proposal, and ONE approval
// stands only on a rule the model, if asked, kept.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

func TestTheRiskRulesAreReplacedWholeInOrderWithTheirWriter(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewRemedyRiskRepository(w.h.Pool)
	ada := w.user(t, "Ada Lovelace")
	replace := func(rules ...domain.RiskRule) {
		t.Helper()
		rs, err := domain.NewRiskRules(rules)
		require.NoError(t, err)
		require.NoError(t, db.NewTxRunner(w.h.Pool).InTx(w.h.Ctx, func(ctx context.Context) error {
			return repo.ReplaceRemedyRisk(ctx, w.scope, domain.RemedyRiskSettings{Rules: rs}, ada, w.h.Now())
		}))
	}

	// ⭐ oto ships no rule.
	got, err := repo.RemedyRisk(w.h.Ctx, w.scope)
	require.NoError(t, err)
	require.Empty(t, got.Rules.Rules())
	require.Equal(t, "", got.WrittenByLabel)

	replace(
		domain.RiskRule{Name: "restart-payments", Verbs: []string{"rollout restart"}, Kinds: []string{"deploy"},
			Namespaces: []string{"payments"}, Approvals: 1},
		domain.RiskRule{Name: "secrets-need-two", Tool: "k8s-write__kubectl", Kinds: []string{"secret"},
			Reversibility: domain.Irreversible, Approvals: 2},
	)
	got, err = repo.RemedyRisk(w.h.Ctx, w.scope)
	require.NoError(t, err)
	rules := got.Rules.Rules()
	require.Len(t, rules, 2)
	require.Equal(t, "restart-payments", rules[0].Name)
	require.Equal(t, []string{"deployment"}, rules[0].Kinds)
	require.Equal(t, "k8s-write__kubectl", rules[1].Tool)
	require.Equal(t, domain.Irreversible, rules[1].Reversibility)
	require.Equal(t, "Ada Lovelace", got.WrittenByLabel)

	other, err := repo.RemedyRisk(w.h.Ctx, w.h.Org().Scope)
	require.NoError(t, err)
	require.Empty(t, other.Rules.Rules(), "another org reads none of them")

	replace()
	got, err = repo.RemedyRisk(w.h.Ctx, w.scope)
	require.NoError(t, err)
	require.Empty(t, got.Rules.Rules())

	// ⛔ A rule with no condition is not a row.
	_, err = w.h.Pool.Exec(w.h.Ctx, `
INSERT INTO remedy_risk_rules (org_id, name, position, tool, verbs, kinds, namespaces, reversibility, approvals, created_at)
VALUES ($1, 'everything', 0, NULL, '{}', '{}', '{}', NULL, 1, $2)`, w.scope.OrgID(), w.h.Now())
	require.Equal(t, "remedy_risk_rules_condition_ck", errs.CodeOf(mapPG(err)))
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
	} {
		run := w.queued(t, uuid.NewString(), w.h.Now())
		bad := r
		bad.ID, bad.InvestigationID, bad.SubjectID, bad.Risk = uuid.New(), run.ID, run.SubjectID, risk
		err := repo.InsertRemedy(w.h.Ctx, w.scope, bad, domain.RemedyTransition{ID: uuid.New(), To: domain.RemedyProposed,
			Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: r.ProposedBy}, At: at})
		require.Equal(t, "remedies_risk_tier_ck", errs.CodeOf(err), "%+v", risk)
	}
}
