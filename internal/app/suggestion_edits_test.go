package app

// git-bug 8327c00: "Applying a count-condition Suggestion edits the policy exactly as a
// human edit would." These tests hold the adapter to the literal reading — it calls
// `notification/service.PolicyWriter.UpdatePolicy`, the method `PATCH
// /notification-policies/{id}` calls, so the merged validation, the refusal of a deleted
// policy and the write are the hand edit's own — and that the patch it sends is the one a
// `{"count_min": n, "count_window_seconds": w}` body binds to and nothing more.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	notifdomain "github.com/thulasiram/oto/internal/notification/domain"
	notifservice "github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// policyRows is a PolicyWriteStore and the adapter's read half over one map, recording
// every patch that reached the write.
type policyRows struct {
	rows    map[uuid.UUID]notifdomain.Policy
	patched []notifdomain.PolicyPatch
}

func (p *policyRows) CreatePolicy(context.Context, db.TenantScope, notifdomain.PolicyDraft) (notifdomain.Policy, error) {
	return notifdomain.Policy{}, errs.New(errs.KindInternal, "unused", "not in this test")
}

func (p *policyRows) GetPolicy(_ context.Context, _ db.TenantScope, id uuid.UUID) (notifdomain.Policy, error) {
	pol, ok := p.rows[id]
	if !ok {
		return notifdomain.Policy{}, errs.NotFound("policy_not_found", "no such notification policy")
	}
	return pol, nil
}

func (p *policyRows) LockPolicy(ctx context.Context, s db.TenantScope, id uuid.UUID) (notifdomain.Policy, error) {
	return p.GetPolicy(ctx, s, id)
}

func (p *policyRows) ListPolicies(context.Context, db.TenantScope, db.Keyset) ([]notifdomain.Policy, db.Cursor, error) {
	out := make([]notifdomain.Policy, 0, len(p.rows))
	for _, pol := range p.rows {
		if pol.DeletedAt == nil {
			out = append(out, pol)
		}
	}
	return out, db.Cursor{}, nil
}

func (p *policyRows) UpdatePolicy(_ context.Context, _ db.TenantScope, id uuid.UUID, patch notifdomain.PolicyPatch) (notifdomain.Policy, error) {
	p.patched = append(p.patched, patch)
	pol := p.rows[id]
	if patch.CountMin != nil && *patch.CountMin != nil {
		pol.Count.Min = **patch.CountMin
	}
	if patch.CountWindow != nil && *patch.CountWindow != nil {
		pol.Count.Window = **patch.CountWindow
	}
	p.rows[id] = pol
	return pol, nil
}

func basePolicy(id uuid.UUID, subjects notifdomain.SubjectBinding) notifdomain.Policy {
	return notifdomain.Policy{
		ID: id, Name: "crashloops → #platform", Priority: 100, Enabled: true,
		Reasons:    []notifdomain.Reason{notifdomain.ReasonFired},
		ChannelIDs: []uuid.UUID{uuid.New()},
		Subjects:   subjects,
	}
}

func suggestionEditRig(t *testing.T, pols ...notifdomain.Policy) (*policyRows, suggestionPolicies, db.TenantScope) {
	t.Helper()
	rows := &policyRows{rows: map[uuid.UUID]notifdomain.Policy{}}
	for _, p := range pols {
		rows.rows[p.ID] = p
	}
	writer, err := notifservice.NewPolicyWriter(notifservice.PolicyWriterOptions{Store: rows})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return rows, suggestionPolicies{reads: rows, writes: writer}, scope
}

// TestACountSuggestionIsAppliedThroughThePolicyEditAHumanUses.
func TestACountSuggestionIsAppliedThroughThePolicyEditAHumanUses(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	rows, adapter, scope := suggestionEditRig(t, basePolicy(id, notifdomain.SubjectBinding{notifdomain.SubjectCase}))

	if err := adapter.ApplyCountCondition(context.Background(), scope, id, 3, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(rows.patched) != 1 {
		t.Fatalf("%d writes, want 1", len(rows.patched))
	}
	p := rows.patched[0]
	if p.CountMin == nil || *p.CountMin == nil || **p.CountMin != 3 ||
		p.CountWindow == nil || *p.CountWindow == nil || **p.CountWindow != 10*time.Minute {
		t.Fatalf("the patch does not carry the condition: %+v", p)
	}
	// ⛔ Nothing else is touched: the patch is the two fields a hand PATCH of the
	// condition binds to.
	p.CountMin, p.CountWindow = nil, nil
	if !p.IsEmpty() {
		t.Fatalf("the Suggestion's patch changes more than the count condition: %+v", p)
	}
	target, err := adapter.SuggestionPolicy(context.Background(), scope, id)
	if err != nil || target.CountMin != 3 || target.CountWindow != 10*time.Minute || !target.Countable() {
		t.Fatalf("read back %+v, %v", target, err)
	}
}

// TestACountSuggestionIsRefusedWhereAHandEditIsRefused — the merged validation is the
// PATCH's: a policy not bound to exactly `case` refuses the condition with the same code
// a hand edit gets, and nothing is written; a deleted policy is `policy_deleted`.
func TestACountSuggestionIsRefusedWhereAHandEditIsRefused(t *testing.T) {
	t.Parallel()
	unbound := basePolicy(uuid.New(), nil)
	gone := basePolicy(uuid.New(), notifdomain.SubjectBinding{notifdomain.SubjectCase})
	deleted := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	gone.DeletedAt = &deleted
	rows, adapter, scope := suggestionEditRig(t, unbound, gone)

	n, w := new(int), new(time.Duration)
	*n, *w = 3, 10*time.Minute
	byHand := notifdomain.PolicyPatch{CountMin: &n, CountWindow: &w}.ValidateAgainst(unbound)
	if byHand == nil {
		t.Fatal("the fixture should be refused by hand too")
	}
	err := adapter.ApplyCountCondition(context.Background(), scope, unbound.ID, 3, 10*time.Minute)
	if errs.CodeOf(err) != errs.CodeOf(byHand) || errs.KindOf(err) != errs.KindValidation {
		t.Fatalf("Suggestion refusal %v, hand refusal %v", err, byHand)
	}

	err = adapter.ApplyCountCondition(context.Background(), scope, gone.ID, 3, 10*time.Minute)
	if errs.CodeOf(err) != "policy_deleted" || !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("a deleted policy = %v", err)
	}
	if _, err := adapter.SuggestionPolicy(context.Background(), scope, gone.ID); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("a deleted policy is read as live: %v", err)
	}
	if len(rows.patched) != 0 {
		t.Fatalf("a refused edit reached the store: %+v", rows.patched)
	}
}
