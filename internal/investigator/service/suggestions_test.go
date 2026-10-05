package service

// git-bug 8327c00's "Done when", against the scripted model and in-memory ports (ADR 0053
// §2, ADR 0052 §2 and §4): a Finding proposes Suggestions through two built-in Tools, an
// invalid proposal is refused on the record, the Investigator applies nothing; applying a
// count-condition Suggestion makes the ordinary policy edit; applying a membership one adds
// the Case — or moves it, said before it is applied — with the applier as actor and the
// Investigation as provenance; an unapplied one lapses and stops showing, and a lapsed or
// applied one cannot be applied. The ordinary edits themselves are internal/app's and the
// notification and incidents modules' tests.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/modelfake"
)

// suggestingInvestigator is `firstlook` holding the case timeline and both proposing Tools.
func (r *rig) suggestingInvestigator(t *testing.T) (domain.Investigator, domain.CaseSubject) {
	t.Helper()
	inv, c := r.setup(t, domain.DefaultBudgets())
	tools, err := domain.NewAllowlist([]string{ToolCaseTimeline, ToolSuggestCountCondition, ToolSuggestMembership})
	if err != nil {
		t.Fatal(err)
	}
	inv, err = r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Tools: &tools})
	if err != nil {
		t.Fatal(err)
	}
	return inv, c
}

var crashPolicy = domain.PolicyTarget{ID: uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001"),
	Name: "crashloops → #platform", SubjectKinds: []string{"case"}}

func (r *rig) applier(t *testing.T) domain.Requester {
	t.Helper()
	by, err := domain.NewRequester(uuid.New(), "Grace Hopper")
	if err != nil {
		t.Fatal(err)
	}
	return by
}

func (r *rig) shown(t *testing.T, investigationID uuid.UUID) []domain.Suggestion {
	t.Helper()
	list, err := r.svc.ListSuggestions(context.Background(), r.scope, investigationID)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// TestAFindingProposesAndAnInvalidProposalIsRefusedOnTheRecord — a proposal that holds is
// an `ok` Step kept with the Finding; one naming no policy is refused, and the refusal
// names the policies a count condition can sit on; neither costs a step; nothing is
// applied by the run.
func TestAFindingProposesAndAnInvalidProposalIsRefusedOnTheRecord(t *testing.T) {
	r := newRig(t)
	r.policies.policies = []domain.PolicyTarget{crashPolicy,
		{ID: uuid.New(), Name: "everything → #firehose"}}
	inv, c := r.suggestingInvestigator(t)
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20,
			call("c1", ToolSuggestCountCondition, `{"policy":"no such policy","count_min":3,"count_window_seconds":600,"why":"noise"}`),
			call("c2", ToolSuggestCountCondition, `{"policy":"everything → #firehose","count_min":3,"count_window_seconds":600,"why":"noise"}`),
			call("c3", ToolSuggestCountCondition, `{"policy":"crashloops → #platform","count_min":3,"count_window_seconds":600,"why":"It flaps every deploy."}`)),
		modelfake.Text("It flaps on every deploy; a count condition would quiet it.", 500, 40),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)

	want := "model_turn tool_call:oto_suggest_count_condition:refused tool_call:oto_suggest_count_condition:refused " +
		"tool_call:oto_suggest_count_condition:ok model_turn"
	if kinds(steps) != want {
		t.Fatalf("transcript = %s\nwant       %s", kinds(steps), want)
	}
	if !strings.Contains(steps[1].Result, "crashloops → #platform") || strings.Contains(steps[1].Result, "firehose") {
		t.Fatalf("the refusal must name the countable policies, and only them: %q", steps[1].Result)
	}
	if !strings.Contains(steps[2].Result, "bound to") {
		t.Fatalf("a policy not bound to exactly case is refused with why: %q", steps[2].Result)
	}
	if got.ToolCalls != 0 {
		t.Fatalf("a proposal cost %d Tool calls; it is the shape of the answer, not a look", got.ToolCalls)
	}
	// ⛔ The run applied nothing.
	if len(r.policies.edits) != 0 || len(r.memberships.edits) != 0 {
		t.Fatalf("the Investigator applied its own Suggestion: %+v %+v", r.policies.edits, r.memberships.edits)
	}
	list := r.shown(t, got.ID)
	if len(list) != 1 {
		t.Fatalf("%d Suggestions kept, want 1", len(list))
	}
	s := list[0]
	if s.Kind != domain.SuggestCountCondition || s.Count.PolicyID != crashPolicy.ID || s.Count.CountMin != 3 ||
		s.Count.CountWindow != 10*time.Minute || s.Count.WasMin != 0 || s.StateAt(r.clock.Now()) != domain.SuggestionOpen {
		t.Fatalf("Suggestion = %+v", s)
	}
	if !s.LapsesAt.Equal(got.EndedAt.Add(domain.SuggestionLapse)) {
		t.Fatalf("lapses at %s, want seven days after the Finding at %s", s.LapsesAt, got.EndedAt)
	}
}

// TestNoFindingNoSuggestion — a proposal belongs to what was concluded: a run that ends
// without a Finding keeps none, though its Steps show what it proposed.
func TestNoFindingNoSuggestion(t *testing.T) {
	r := newRig(t)
	r.policies.policies = []domain.PolicyTarget{crashPolicy}
	inv, c := r.suggestingInvestigator(t)
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, call("c1", ToolSuggestCountCondition,
			`{"policy":"crashloops → #platform","count_min":3,"count_window_seconds":600,"why":"flaps"}`)),
		modelfake.WithoutUsage("never budgeted"),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusFailed || len(steps) != 2 || steps[1].Outcome != domain.OutcomeOK {
		t.Fatalf("run = %+v, steps %s", got, kinds(steps))
	}
	if n := len(r.suggestions.rows); n != 0 {
		t.Fatalf("%d Suggestions kept without a Finding", n)
	}
}

// TestAProposingToolOffTheAllowlistIsRefused — being built in is not being allowed.
func TestAProposingToolOffTheAllowlistIsRefused(t *testing.T) {
	r := newRig(t)
	r.policies.policies = []domain.PolicyTarget{crashPolicy}
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, call("c1", ToolSuggestCountCondition,
			`{"policy":"crashloops → #platform","count_min":3,"count_window_seconds":600,"why":"flaps"}`)),
		modelfake.Text("done", 300, 10),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if steps[1].Outcome != domain.OutcomeRefused || !strings.Contains(steps[1].Result, "allowlist") {
		t.Fatalf("step = %+v", steps[1])
	}
	if len(r.shown(t, got.ID)) != 0 {
		t.Fatal("a Suggestion was kept from a Tool the Investigator does not hold")
	}
}

// proposeCount runs one Investigation that proposes crashPolicy's count condition, and
// returns the Suggestion it kept.
func (r *rig) proposeCount(t *testing.T) domain.Suggestion {
	t.Helper()
	r.policies.policies = []domain.PolicyTarget{crashPolicy}
	inv, c := r.suggestingInvestigator(t)
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, call("c1", ToolSuggestCountCondition,
			`{"policy":"crashloops → #platform","count_min":3,"count_window_seconds":600,"why":"It flaps every deploy."}`)),
		modelfake.Text("It flaps.", 300, 10),
	}
	got, _ := r.run(t, r.request(t, inv, c).ID)
	list := r.shown(t, got.ID)
	if len(list) != 1 {
		t.Fatalf("%d Suggestions, want 1", len(list))
	}
	return list[0]
}

// TestApplyingACountSuggestionMakesTheOrdinaryEditOnce — the edit is the policy editor's,
// with exactly the proposed numbers; the applier is recorded; a second apply is refused.
func TestApplyingACountSuggestionMakesTheOrdinaryEditOnce(t *testing.T) {
	r := newRig(t)
	s := r.proposeCount(t)
	by := r.applier(t)

	applied, err := r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, by, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.policies.edits) != 1 || r.policies.edits[0] != (countEdit{crashPolicy.ID, 3, 10 * time.Minute}) {
		t.Fatalf("edits = %+v", r.policies.edits)
	}
	if applied.StateAt(r.clock.Now()) != domain.SuggestionApplied || applied.AppliedBy != by {
		t.Fatalf("applied = %+v", applied)
	}
	list := r.shown(t, s.InvestigationID)
	if len(list) != 1 || list[0].AppliedBy.Label != "Grace Hopper" {
		t.Fatalf("the applied Suggestion stays shown with who applied it: %+v", list)
	}

	_, err = r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, by, 0)
	if errs.CodeOf(err) != "suggestion_already_applied" || !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("second apply = %v", err)
	}
	if len(r.policies.edits) != 1 {
		t.Fatal("a second apply edited the policy again")
	}
}

// TestAnUnappliedSuggestionLapsesStopsShowingAndCannotBeApplied — the lapse is read off
// the clock: nothing is written when it passes, and from then on it is neither listed nor
// applicable.
func TestAnUnappliedSuggestionLapsesStopsShowingAndCannotBeApplied(t *testing.T) {
	r := newRig(t)
	s := r.proposeCount(t)

	r.clock.Advance(domain.SuggestionLapse - time.Second)
	if len(r.shown(t, s.InvestigationID)) != 1 {
		t.Fatal("a Suggestion stopped showing before it lapsed")
	}
	r.clock.Advance(time.Second)
	if n := len(r.shown(t, s.InvestigationID)); n != 0 {
		t.Fatalf("a lapsed Suggestion is still shown (%d)", n)
	}
	_, err := r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, r.applier(t), 0)
	if errs.CodeOf(err) != "suggestion_lapsed" || !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("applying a lapsed Suggestion = %v", err)
	}
	if len(r.policies.edits) != 0 {
		t.Fatal("a lapsed Suggestion edited the policy")
	}
}

// TestASuggestionWhosePolicyIsGoneIsRefusedAsSuch — deleted since it was proposed.
func TestASuggestionWhosePolicyIsGoneIsRefusedAsSuch(t *testing.T) {
	r := newRig(t)
	s := r.proposeCount(t)
	r.policies.policies = nil

	_, err := r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, r.applier(t), 0)
	if errs.CodeOf(err) != "suggestion_target_gone" || !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("apply = %v", err)
	}
	if len(r.policies.edits) != 0 {
		t.Fatal("a gone policy was edited")
	}
}

// proposeMembership runs one Investigation of an Incident that proposes `other` for it.
func (r *rig) proposeMembership(t *testing.T, other domain.CaseSubject) (domain.IncidentSubject, domain.Suggestion) {
	t.Helper()
	member := domain.CaseSubject{CaseID: uuid.New(), Number: 7, AlertKey: "ak-7", Alertname: "PaymentsDown", State: "open"}
	r.history.cases[member.CaseID] = member
	r.history.cases[other.CaseID] = other
	incident := r.drawIncident(member)
	inv := r.incidentInvestigator(t, "storyteller", true, false, ToolSuggestMembership)
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, call("c1", ToolSuggestMembership,
			`{"case_id":"`+other.CaseID.String()+`","why":"Same namespace, same minute."}`)),
		modelfake.Text("One story: the payments deploy.", 300, 10),
	}
	got, steps := r.run(t, r.requestIncident(t, inv, incident).ID)
	if steps[1].Outcome != domain.OutcomeOK {
		t.Fatalf("proposal step = %+v", steps[1])
	}
	list := r.shown(t, got.ID)
	if len(list) != 1 || list[0].Membership.CaseID != other.CaseID || list[0].Membership.IncidentNumber != incident.Number {
		t.Fatalf("Suggestions = %+v", list)
	}
	return incident, list[0]
}

// TestApplyingAMembershipSuggestionAddsTheCaseWithTheApplierAsActor.
func TestApplyingAMembershipSuggestionAddsTheCaseWithTheApplierAsActor(t *testing.T) {
	r := newRig(t)
	other := domain.CaseSubject{CaseID: uuid.New(), Number: 9, AlertKey: "ak-9", Alertname: "CheckoutSlow", State: "open"}
	incident, s := r.proposeMembership(t, other)
	if !s.MovesFrom.IsZero() {
		t.Fatalf("a free Case reads as a move from %+v", s.MovesFrom)
	}
	by := r.applier(t)
	if _, err := r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, by, 0); err != nil {
		t.Fatal(err)
	}
	want := domain.AppliedMembership{IncidentNumber: incident.Number, CaseID: other.CaseID, By: by, SuggestedBy: s.InvestigationID}
	if len(r.memberships.edits) != 1 || r.memberships.edits[0] != want {
		t.Fatalf("edits = %+v, want %+v", r.memberships.edits, want)
	}
}

// TestAMembershipSuggestionForAHeldCaseSaysItMovesBeforeItIsApplied — the read names the
// Incident it would move the Case from; an apply that did not confirm it is refused with
// that sentence and writes nothing; one that did is the move.
func TestAMembershipSuggestionForAHeldCaseSaysItMovesBeforeItIsApplied(t *testing.T) {
	r := newRig(t)
	other := domain.CaseSubject{CaseID: uuid.New(), Number: 9, AlertKey: "ak-9", Alertname: "CheckoutSlow", State: "open"}
	elsewhere := domain.IncidentSubject{IncidentID: uuid.New(), Number: 2, Active: true,
		Members: []domain.IncidentMember{{CaseID: other.CaseID, CaseNumber: other.Number}}}
	r.incidents.incidents[elsewhere.IncidentID] = elsewhere
	r.incidents.holding[other.CaseID] = elsewhere.IncidentID

	incident, s := r.proposeMembership(t, other)
	if s.MovesFrom.Number != elsewhere.Number {
		t.Fatalf("the read says it moves from %+v, want Incident #%d", s.MovesFrom, elsewhere.Number)
	}

	by := r.applier(t)
	_, err := r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, by, 0)
	if errs.CodeOf(err) != "suggestion_moves_case" || !strings.Contains(err.Error(), "move Case #9 from Incident #2") {
		t.Fatalf("an unconfirmed move = %v", err)
	}
	if len(r.memberships.edits) != 0 {
		t.Fatal("an unconfirmed move was made")
	}

	if _, err := r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, by, elsewhere.Number); err != nil {
		t.Fatal(err)
	}
	want := domain.AppliedMembership{IncidentNumber: incident.Number, CaseID: other.CaseID, FromNumber: elsewhere.Number,
		By: by, SuggestedBy: s.InvestigationID}
	if len(r.memberships.edits) != 1 || r.memberships.edits[0] != want {
		t.Fatalf("edits = %+v, want %+v", r.memberships.edits, want)
	}
}

// TestAMembershipProposalForACaseAlreadyInTheIncidentIsRefused — nothing would change.
func TestAMembershipProposalForACaseAlreadyInTheIncidentIsRefused(t *testing.T) {
	r := newRig(t)
	member := domain.CaseSubject{CaseID: uuid.New(), Number: 7, AlertKey: "ak-7", Alertname: "PaymentsDown", State: "open"}
	r.history.cases[member.CaseID] = member
	incident := r.drawIncident(member)
	inv := r.incidentInvestigator(t, "storyteller", true, false, ToolSuggestMembership)
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, call("c1", ToolSuggestMembership,
			`{"case_id":"`+member.CaseID.String()+`","why":"It is in the story."}`)),
		modelfake.Text("One story.", 300, 10),
	}
	got, steps := r.run(t, r.requestIncident(t, inv, incident).ID)
	if steps[1].Outcome != domain.OutcomeRefused || !strings.Contains(steps[1].Result, "already in Incident #4") {
		t.Fatalf("step = %+v", steps[1])
	}
	if len(r.shown(t, got.ID)) != 0 {
		t.Fatal("a no-op membership was kept")
	}
}

// TestAStaleCountSuggestionIsRefusedAndWritesNothing — review B3: a hand edit since the
// proposal makes "from X to Y" a lie, and applying it would overwrite that edit; it is
// refused as stale, the policy is not touched, and the Suggestion stays unapplied.
func TestAStaleCountSuggestionIsRefusedAndWritesNothing(t *testing.T) {
	r := newRig(t)
	s := r.proposeCount(t)
	by := r.applier(t)
	// Someone edits the policy by hand after the proposal.
	if err := r.policies.ApplyCountCondition(context.Background(), r.scope, crashPolicy.ID, 5, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	edits := len(r.policies.edits)

	_, err := r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, by, 0)
	if errs.CodeOf(err) != "suggestion_stale" || !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("apply = %v, want suggestion_stale", err)
	}
	if e, _ := errs.As(err); e == nil || !strings.Contains(e.Message, "5 in 30m0s") {
		t.Fatalf("the refusal does not say what the policy says now: %v", err)
	}
	if len(r.policies.edits) != edits {
		t.Fatal("a stale Suggestion edited the policy")
	}
	if list := r.shown(t, s.InvestigationID); len(list) != 1 || list[0].StateAt(r.clock.Now()) == domain.SuggestionApplied {
		t.Fatalf("the stale Suggestion was marked applied: %+v", list)
	}
}

// TestACountSuggestionComparesThePolicyUnderItsLock — judgment 2, E6: the stale check reads the
// policy through its row lock, so a hand edit that committed just before the lock was taken is
// SEEN, and the apply is refused stale instead of overwriting it.
func TestACountSuggestionComparesThePolicyUnderItsLock(t *testing.T) {
	r := newRig(t)
	s := r.proposeCount(t)
	by := r.applier(t)
	r.policies.beforeLock = func() {
		if err := r.policies.ApplyCountCondition(context.Background(), r.scope, crashPolicy.ID, 5, 30*time.Minute); err != nil {
			t.Error(err)
		}
	}
	edits := len(r.policies.edits) + 1 // the hand edit's

	_, err := r.svc.ApplySuggestion(context.Background(), r.scope, s.ID, by, 0)
	if errs.CodeOf(err) != "suggestion_stale" {
		t.Fatalf("apply = %v, want suggestion_stale", err)
	}
	if r.policies.locks != 1 || len(r.policies.edits) != edits {
		t.Fatalf("%d locked reads, %d edits (want 1 and %d)", r.policies.locks, len(r.policies.edits), edits)
	}
}
