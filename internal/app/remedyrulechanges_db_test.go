package app

// THE SECOND WRITER OF THE REMEDY RISK RULES, AGAINST A REAL POSTGRES (ADR 0054 §3, owner ruling O3;
// migration 00111): a change a DIFFERENT member confirmed. What it must NOT do is as much the claim
// as what it does: a proposer's own confirmation writes nothing, a superseded change is not applied
// over the newer word, and `oto remedy-rules apply` from the host shell overtakes a pending change.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	investigatorrepo "github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/harness"
)

const oneRuleYAML = `
rules:
  - name: restart-payments
    tool: k8s-write__kubectl
    verbs: [rollout restart]
    kinds: [deployment]
    namespaces: [payments]
    approvals: 1
`

func proposeOneRule(
	t *testing.T, h *harness.H, scope db.TenantScope, by investigatordomain.Requester,
) investigatordomain.RiskChange {
	t.Helper()
	rules, err := investigatordomain.NewRiskRules([]investigatordomain.RiskRule{{
		Name: "restart-payments", Tool: "k8s-write__kubectl", Verbs: []string{"rollout restart"},
		Kinds: []string{"deployment"}, Namespaces: []string{"payments"}, Approvals: investigatordomain.SingleApproval,
	}})
	require.NoError(t, err)
	repo := investigatorrepo.NewRiskChangeRepository(h.Pool)
	var out investigatordomain.RiskChange
	require.NoError(t, db.NewTxRunner(h.Pool).InTx(h.Ctx, func(ctx context.Context) error {
		out, err = repo.Propose(ctx, scope, by, rules, uuid.Nil, h.Now())
		return err
	}))
	return out
}

func currentRules(t *testing.T, h *harness.H, scope db.TenantScope) investigatordomain.RemedyRiskSettings {
	t.Helper()
	set, err := investigatorrepo.NewRemedyRiskRepository(h.Pool).RemedyRisk(h.Ctx, scope)
	require.NoError(t, err)
	return set
}

func TestADifferentMembersConfirmationWritesTheRules(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ada, grace := h.User(org), h.User(org)
	applier := NewRemedyRiskApplier(h.Pool)

	change := proposeOneRule(t, h, org.Scope, investigatordomain.Requester{UserID: ada.ID, Label: "Ada"})
	require.Empty(t, currentRules(t, h, org.Scope).Rules.Rules(), "a proposal writes no rule")

	set, err := applier.Confirm(h.Ctx, org.Scope, change.ID,
		investigatordomain.Requester{UserID: grace.ID, Label: "Grace"}, h.Now())
	require.NoError(t, err)
	require.Len(t, set.Rules.Rules(), 1)
	require.Equal(t, "restart-payments", set.Rules.Rules()[0].Name)
	require.Contains(t, set.WrittenByLabel, "Grace")
	require.Contains(t, set.WrittenByLabel, "Ada", "the label says whose change it was")

	// The change is applied, by Grace, and the settings row names her as the writer.
	var status string
	var decidedBy, writtenBy *uuid.UUID
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT status, decided_by FROM remedy_risk_changes WHERE id = $1`, change.ID).Scan(&status, &decidedBy))
	require.Equal(t, "applied", status)
	require.NotNil(t, decidedBy)
	require.Equal(t, grace.ID, *decidedBy)
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT written_by FROM remedy_risk_settings WHERE org_id = $1`, org.ID).Scan(&writtenBy))
	require.NotNil(t, writtenBy)
	require.Equal(t, grace.ID, *writtenBy)
}

// ⛔⛔ THE POINT: the member who proposed a change cannot be the one to apply it, and a refused
// attempt writes nothing — not the rules, and not an "applied" mark.
func TestTheProposerCannotConfirmTheirOwnChange(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ada := h.User(org)
	by := investigatordomain.Requester{UserID: ada.ID, Label: "Ada"}
	change := proposeOneRule(t, h, org.Scope, by)

	_, err := NewRemedyRiskApplier(h.Pool).Confirm(h.Ctx, org.Scope, change.ID, by, h.Now())
	require.Equal(t, "remedy_risk_change_needs_a_second_person", errs.CodeOf(err), "got %v", err)

	require.Empty(t, currentRules(t, h, org.Scope).Rules.Rules())
	var status string
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT status FROM remedy_risk_changes WHERE id = $1`, change.ID).Scan(&status))
	require.Equal(t, "pending", status)
}

func TestAConfirmationNamesTheChangeItReadAndASupersededOneIsRefused(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ada, grace := h.User(org), h.User(org)
	proposer := investigatordomain.Requester{UserID: ada.ID, Label: "Ada"}
	first := proposeOneRule(t, h, org.Scope, proposer)
	_ = proposeOneRule(t, h, org.Scope, proposer) // supersedes the first

	_, err := NewRemedyRiskApplier(h.Pool).Confirm(h.Ctx, org.Scope, first.ID,
		investigatordomain.Requester{UserID: grace.ID, Label: "Grace"}, h.Now())
	require.Equal(t, "remedy_risk_change_not_pending", errs.CodeOf(err), "got %v", err)
	require.Empty(t, currentRules(t, h, org.Scope).Rules.Rules())
}

func TestAnotherOrgsChangeIsNotFound(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org, other := h.Org(), h.Org()
	ada, grace := h.User(org), h.User(other)
	change := proposeOneRule(t, h, org.Scope, investigatordomain.Requester{UserID: ada.ID, Label: "Ada"})

	_, err := NewRemedyRiskApplier(h.Pool).Confirm(h.Ctx, other.Scope, change.ID,
		investigatordomain.Requester{UserID: grace.ID, Label: "Grace"}, h.Now())
	require.True(t, errs.IsKind(err, errs.KindNotFound), "got %v", err)
}

// ⭐ The shell's word is the later one: a host-shell apply supersedes a pending change, so a stale
// proposal cannot be confirmed over it.
func TestAHostShellApplySupersedesAPendingChange(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ada, grace := h.User(org), h.User(org)
	change := proposeOneRule(t, h, org.Scope, investigatordomain.Requester{UserID: ada.ID, Label: "Ada"})

	_, err := ApplyRemedyRules(h.Ctx, h.Pool, org.Slug, []byte("rules: []\n"), h.Now())
	require.NoError(t, err)

	_, err = NewRemedyRiskApplier(h.Pool).Confirm(h.Ctx, org.Scope, change.ID,
		investigatordomain.Requester{UserID: grace.ID, Label: "Grace"}, h.Now())
	require.Equal(t, "remedy_risk_change_not_pending", errs.CodeOf(err), "got %v", err)
	require.Empty(t, currentRules(t, h, org.Scope).Rules.Rules(), "the shell's empty set stands")
}

func TestTheShellStillAppliesAFileDirectly(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	res, err := ApplyRemedyRules(h.Ctx, h.Pool, org.Slug, []byte(oneRuleYAML), h.Now())
	require.NoError(t, err)
	require.Len(t, res.Rules, 1)
	require.Equal(t, RemedyRulesWriter, res.WrittenByLabel)
	var writtenBy *uuid.UUID
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT written_by FROM remedy_risk_settings WHERE org_id = $1`, org.ID).Scan(&writtenBy))
	require.Nil(t, writtenBy, "the host's shell is not a member")
}
