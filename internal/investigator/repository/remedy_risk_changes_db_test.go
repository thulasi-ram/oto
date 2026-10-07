package repository_test

// A PENDING RULE CHANGE AGAINST A REAL POSTGRES (migration 00111; ADR 0054 §3, owner ruling O3).
// Every claim here is a claim about SQL as much as Go: one pending change per org, the proposer
// who cannot also be the confirmer — as a CHECK, so a bug in the Go cannot write the row —, the
// trigger that freezes what a confirmer read, and the tenant predicate.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/harness"
)

func oneRule(t *testing.T) domain.RiskRules {
	t.Helper()
	rules, err := domain.NewRiskRules([]domain.RiskRule{{
		Name: "restart-payments", Tool: "k8s-write__kubectl", Verbs: []string{"rollout restart"},
		Kinds: []string{"deployment"}, Namespaces: []string{"payments"}, Approvals: domain.SingleApproval,
	}})
	require.NoError(t, err)
	return rules
}

// propose runs the repository's Propose the way the service does: inside a transaction, because it
// takes the org's risk-rules advisory lock, which a lone statement would release at once.
func propose(
	t *testing.T, h *harness.H, repo *repository.RiskChangeRepository, scope db.TenantScope,
	by domain.Requester, rules domain.RiskRules, model uuid.UUID,
) domain.RiskChange {
	t.Helper()
	var out domain.RiskChange
	require.NoError(t, db.NewTxRunner(h.Pool).InTx(h.Ctx, func(ctx context.Context) error {
		var err error
		out, err = repo.Propose(ctx, scope, by, rules, model, h.Now())
		return err
	}))
	return out
}

func TestAProposalIsStoredPendingAndANewerOneSupersedesIt(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org, other := h.Org(), h.Org()
	ada := h.User(org)
	repo := repository.NewRiskChangeRepository(h.Pool)
	by := domain.Requester{UserID: ada.ID, Label: "Ada"}

	first := propose(t, h, repo, org.Scope, by, oneRule(t), uuid.Nil)
	require.Equal(t, domain.RiskChangePending, first.Status)
	require.Equal(t, ada.ID, first.ProposedBy.UserID)
	require.Len(t, first.Rules.Rules(), 1)

	got, ok, err := repo.Pending(h.Ctx, org.Scope)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first.ID, got.ID)
	require.Equal(t, first.Rules.Rules(), got.Rules.Rules(), "the rules round-trip through the JSON column")

	// ⭐ A second proposal supersedes the first: still exactly one pending.
	second := propose(t, h, repo, org.Scope, by, domain.RiskRules{}, uuid.Nil)
	pending, ok, err := repo.Pending(h.Ctx, org.Scope)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, second.ID, pending.ID)
	old, err := repo.Get(h.Ctx, org.Scope, first.ID)
	require.NoError(t, err)
	require.Equal(t, domain.RiskChangeSuperseded, old.Status)

	// Another org sees none of it.
	_, ok, err = repo.Pending(h.Ctx, other.Scope)
	require.NoError(t, err)
	require.False(t, ok)
	_, err = repo.Get(h.Ctx, other.Scope, first.ID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "got %v", err)
}

func TestADiscardIsOnceAndAnotherOrgsChangeIsNotFound(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org, other := h.Org(), h.Org()
	ada := h.User(org)
	repo := repository.NewRiskChangeRepository(h.Pool)
	by := domain.Requester{UserID: ada.ID, Label: "Ada"}

	c := propose(t, h, repo, org.Scope, by, oneRule(t), uuid.Nil)

	_, err := repo.Discard(h.Ctx, other.Scope, c.ID, by, h.Now())
	require.True(t, errs.IsKind(err, errs.KindNotFound), "got %v", err)

	done, err := repo.Discard(h.Ctx, org.Scope, c.ID, by, h.Now())
	require.NoError(t, err)
	require.Equal(t, domain.RiskChangeDiscarded, done.Status)
	require.Equal(t, "Ada", done.DecidedBy.Label)

	_, err = repo.Discard(h.Ctx, org.Scope, c.ID, by, h.Now())
	require.Equal(t, "remedy_risk_change_not_pending", errs.CodeOf(err), "got %v", err)
	_, ok, err := repo.Pending(h.Ctx, org.Scope)
	require.NoError(t, err)
	require.False(t, ok)
}

// ⛔⛔ THE TWO PEOPLE ARE THE SCHEMA'S, NOT THE GO'S: an applied row whose confirmer is its proposer
// is refused by a CHECK, written by hand here past every Go check to prove it holds on its own.
func TestTheSchemaRefusesAChangeConfirmedByItsProposer(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ada, grace := h.User(org), h.User(org)
	repo := repository.NewRiskChangeRepository(h.Pool)
	c := propose(t, h, repo, org.Scope, domain.Requester{UserID: ada.ID, Label: "Ada"}, oneRule(t), uuid.Nil)

	const apply = `UPDATE remedy_risk_changes SET status = 'applied', decided_by = $2, decided_by_label = 'x', decided_at = $3
	               WHERE id = $1`
	_, err := h.Pool.Exec(h.Ctx, apply, c.ID, ada.ID, h.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "remedy_risk_changes_two_people_ck")

	// A different member is admitted.
	_, err = h.Pool.Exec(h.Ctx, apply, c.ID, grace.ID, h.Now())
	require.NoError(t, err)
}

func TestWhatAConfirmerReadIsFrozen(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ada := h.User(org)
	repo := repository.NewRiskChangeRepository(h.Pool)
	c := propose(t, h, repo, org.Scope, domain.Requester{UserID: ada.ID, Label: "Ada"}, oneRule(t), uuid.Nil)

	// The rules of a pending change cannot be edited under a reader.
	_, err := h.Pool.Exec(h.Ctx, `UPDATE remedy_risk_changes SET rules = '[]'::jsonb WHERE id = $1`, c.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "never rewritten")

	// And a decided change cannot move again.
	_, err = repo.Discard(h.Ctx, org.Scope, c.ID, domain.Requester{UserID: ada.ID, Label: "Ada"}, h.Now())
	require.NoError(t, err)
	_, err = h.Pool.Exec(h.Ctx, `UPDATE remedy_risk_changes SET status = 'pending', decided_by = NULL,
	                              decided_by_label = NULL, decided_at = NULL WHERE id = $1`, c.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "frozen")
}

func TestAModelEndpointAPendingChangeNamesIsNotDeleted(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ada := h.User(org)
	providers := repository.NewProviderRepository(h.Pool)
	p, err := providers.Insert(h.Ctx, org.Scope, draft("risk-gw", "https://gw.example.test/v1"), uuid.Nil, h.Now())
	require.NoError(t, err)

	repo := repository.NewRiskChangeRepository(h.Pool)
	propose(t, h, repo, org.Scope, domain.Requester{UserID: ada.ID, Label: "Ada"}, oneRule(t), p.ID)

	// NO ACTION, not SET NULL: "use this model" must not quietly become "use none".
	err = providers.Delete(h.Ctx, org.Scope, p.ID)
	require.Equal(t, "model_provider_in_use", errs.CodeOf(err), "got %v", err)
}
