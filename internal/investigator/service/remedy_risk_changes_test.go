package service

// A RULE CHANGE TAKES A SECOND PERSON (ADR 0054 §3, owner ruling O3): these are the service's own
// rules against in-memory ports. The row's CHECKs, the lock and the trigger are
// `repository/remedy_risk_changes_db_test.go`'s and `internal/app`'s.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

type memRiskChanges struct {
	mu   sync.Mutex
	rows []domain.RiskChange
}

func (m *memRiskChanges) Propose(_ context.Context, s db.TenantScope, by domain.Requester, rules domain.RiskRules, model uuid.UUID, at time.Time) (domain.RiskChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].Pending() {
			m.rows[i].Status = domain.RiskChangeSuperseded
		}
	}
	c := domain.RiskChange{ID: uuid.New(), OrgID: s.OrgID(), Rules: rules, RiskModelProviderID: model,
		Status: domain.RiskChangePending, ProposedBy: by, ProposedAt: at}
	m.rows = append(m.rows, c)
	return c, nil
}

func (m *memRiskChanges) Pending(context.Context, db.TenantScope) (domain.RiskChange, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.rows {
		if c.Pending() {
			return c, true, nil
		}
	}
	return domain.RiskChange{}, false, nil
}

func (m *memRiskChanges) Get(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.RiskChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.rows {
		if c.ID == id {
			return c, nil
		}
	}
	return domain.RiskChange{}, domain.RiskChangeNotFound()
}

func (m *memRiskChanges) Discard(_ context.Context, _ db.TenantScope, id uuid.UUID, by domain.Requester, _ time.Time) (domain.RiskChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].ID != id {
			continue
		}
		if !m.rows[i].Pending() {
			return domain.RiskChange{}, domain.RiskChangeNotPending(m.rows[i].Status)
		}
		m.rows[i].Status, m.rows[i].DecidedBy = domain.RiskChangeDiscarded, by
		return m.rows[i], nil
	}
	return domain.RiskChange{}, domain.RiskChangeNotFound()
}

// memRiskApplier stands for internal/app's applier: it writes the rules into the fake settings the
// service reads, and counts what it was asked, so a test can say a refusal never reached it.
type memRiskApplier struct {
	changes *memRiskChanges
	risk    *memRemedyRisk
	called  int
}

func (a *memRiskApplier) Confirm(_ context.Context, _ db.TenantScope, id uuid.UUID, by domain.Requester, at time.Time) (domain.RemedyRiskSettings, error) {
	a.called++
	a.changes.mu.Lock()
	defer a.changes.mu.Unlock()
	for i := range a.changes.rows {
		c := &a.changes.rows[i]
		if c.ID != id {
			continue
		}
		if err := c.ConfirmableBy(by); err != nil {
			return domain.RemedyRiskSettings{}, err
		}
		c.Status, c.DecidedBy = domain.RiskChangeApplied, by
		a.risk.mu.Lock()
		a.risk.set = domain.RemedyRiskSettings{Rules: c.Rules, RiskModelProviderID: c.RiskModelProviderID,
			WrittenByLabel: by.Label, WrittenAt: at}
		a.risk.mu.Unlock()
		return a.risk.set, nil
	}
	return domain.RemedyRiskSettings{}, domain.RiskChangeNotFound()
}

func human(label string) domain.Requester { return domain.Requester{UserID: uuid.New(), Label: label} }

func oneApprovalDraft() domain.RiskChangeDraft {
	return domain.RiskChangeDraft{Rules: []domain.RiskRule{{
		Name: "restart-payments", Tool: "k8s-write__kubectl", Verbs: []string{"rollout restart"},
		Kinds: []string{"deployment"}, Namespaces: []string{"payments"}, Approvals: domain.SingleApproval,
	}}}
}

func TestAProposalWritesNoRule(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	c, err := r.svc.ProposeRemedyRiskChange(ctx, r.scope, human("Ada"), oneApprovalDraft())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Pending() {
		t.Fatalf("status = %s", c.Status)
	}
	// ⛔ The rules a Remedy is tiered by are exactly what they were.
	got, err := r.svc.RemedyRisk(ctx, r.scope)
	if err != nil || len(got.Rules.Rules()) != 0 {
		t.Fatalf("a proposal changed the rules: %+v %v", got, err)
	}
	if r.riskApplier.called != 0 {
		t.Fatal("a proposal reached the applier")
	}
}

func TestOnlyADifferentMemberConfirmsAndThenTheRulesStand(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ada, grace := human("Ada"), human("Grace")
	c, _ := r.svc.ProposeRemedyRiskChange(ctx, r.scope, ada, oneApprovalDraft())

	// ⛔ The proposer is refused before the applier is reached.
	_, err := r.svc.ConfirmRemedyRiskChange(ctx, r.scope, c.ID, ada)
	if errs.CodeOf(err) != "remedy_risk_change_needs_a_second_person" || !errs.IsKind(err, errs.KindForbidden) {
		t.Fatalf("err = %v", err)
	}
	if r.riskApplier.called != 0 {
		t.Fatal("the proposer's confirmation reached the applier")
	}

	set, err := r.svc.ConfirmRemedyRiskChange(ctx, r.scope, c.ID, grace)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Rules.Rules()) != 1 || set.WrittenByLabel != "Grace" {
		t.Fatalf("settings = %+v", set)
	}
	// Applied once: a second confirmation is refused, not applied again.
	if _, err := r.svc.ConfirmRemedyRiskChange(ctx, r.scope, c.ID, human("Linus")); errs.CodeOf(err) != "remedy_risk_change_not_pending" {
		t.Fatalf("err = %v", err)
	}
}

func TestANewerProposalSupersedesAndTheOldOneCannotBeConfirmed(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ada, grace := human("Ada"), human("Grace")
	first, _ := r.svc.ProposeRemedyRiskChange(ctx, r.scope, ada, oneApprovalDraft())
	second, _ := r.svc.ProposeRemedyRiskChange(ctx, r.scope, ada, domain.RiskChangeDraft{})

	// ⭐ What Grace read was the first; confirming it must not apply the second in its place.
	if _, err := r.svc.ConfirmRemedyRiskChange(ctx, r.scope, first.ID, grace); errs.CodeOf(err) != "remedy_risk_change_not_pending" {
		t.Fatalf("err = %v", err)
	}
	pending, ok, _ := r.svc.PendingRemedyRiskChange(ctx, r.scope)
	if !ok || pending.ID != second.ID {
		t.Fatalf("pending = %+v, want the newer one", pending)
	}
}

func TestAnyoneMayDiscardAndNothingIsWritten(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ada := human("Ada")
	c, _ := r.svc.ProposeRemedyRiskChange(ctx, r.scope, ada, oneApprovalDraft())
	if err := r.svc.DiscardRemedyRiskChange(ctx, r.scope, c.ID, ada); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.svc.PendingRemedyRiskChange(ctx, r.scope); ok {
		t.Fatal("a discarded change is still pending")
	}
	if err := r.svc.DiscardRemedyRiskChange(ctx, r.scope, c.ID, ada); errs.CodeOf(err) != "remedy_risk_change_not_pending" {
		t.Fatalf("err = %v", err)
	}
	if r.riskApplier.called != 0 {
		t.Fatal("a discard reached the applier")
	}
}

func TestAProposalIsValidatedAsTheCLIValidatesIt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// ⛔ A rule that says one approval and names no Tool is refused, as `oto remedy-rules apply` does.
	loose := domain.RiskChangeDraft{Rules: []domain.RiskRule{{Name: "loose", Verbs: []string{"delete"}, Approvals: domain.SingleApproval}}}
	if _, err := r.svc.ProposeRemedyRiskChange(ctx, r.scope, human("Ada"), loose); !errs.IsKind(err, errs.KindValidation) {
		t.Fatalf("err = %v, want a validation refusal", err)
	}
	// A risk model that is not this org's endpoint is refused, naming the field.
	d := oneApprovalDraft()
	d.RiskModelProviderID = uuid.New()
	_, err := r.svc.ProposeRemedyRiskChange(ctx, r.scope, human("Ada"), d)
	if errs.CodeOf(err) != "remedy_risk_model_not_found" {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := r.svc.PendingRemedyRiskChange(ctx, r.scope); ok {
		t.Fatal("a refused proposal left a pending change")
	}
}
