package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

var countable = PolicyTarget{ID: uuid.New(), Name: "crashloops", SubjectKinds: []string{"case"}}

// TestASuggestionIsOpenThenAppliedOrLapsedAndNothingElse — the one place the rule is
// written: applied wins, lapsing is the clock reaching lapses_at, open otherwise.
func TestASuggestionIsOpenThenAppliedOrLapsedAndNothingElse(t *testing.T) {
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	s := Suggestion{ProposedAt: at, LapsesAt: at.Add(SuggestionLapse)}
	if s.StateAt(at) != SuggestionOpen || s.Applicable(at) != nil {
		t.Fatalf("a new Suggestion is %s", s.StateAt(at))
	}
	if got := s.StateAt(s.LapsesAt.Add(-time.Nanosecond)); got != SuggestionOpen {
		t.Fatalf("a nanosecond before its lapse it is %s", got)
	}
	if got := s.StateAt(s.LapsesAt); got != SuggestionLapsed {
		t.Fatalf("at its lapse it is %s", got)
	}
	if err := s.Applicable(s.LapsesAt); errs.CodeOf(err) != "suggestion_lapsed" || !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("a lapsed Suggestion is applicable: %v", err)
	}
	s.AppliedAt, s.AppliedBy = at.Add(time.Hour), Requester{Label: "Grace"}
	if got := s.StateAt(s.LapsesAt.Add(time.Hour)); got != SuggestionApplied {
		t.Fatalf("an applied Suggestion lapsed: %s", got)
	}
	if err := s.Applicable(at.Add(2 * time.Hour)); errs.CodeOf(err) != "suggestion_already_applied" {
		t.Fatalf("an applied Suggestion is applicable again: %v", err)
	}
}

// TestACountSuggestionThatCouldNeverBeAppliedIsNeverProposed — the binding, the bounds and
// a no-op are each refused while the model can still answer.
func TestACountSuggestionThatCouldNeverBeAppliedIsNeverProposed(t *testing.T) {
	cases := []struct {
		name   string
		p      PolicyTarget
		min    int
		window time.Duration
		why    string
	}{
		{"unbound policy", PolicyTarget{Name: "all"}, 3, time.Hour, "x"},
		{"alert-bound policy", PolicyTarget{Name: "a", SubjectKinds: []string{"alert"}}, 3, time.Hour, "x"},
		{"threshold of one", countable, 1, time.Hour, "x"},
		{"window under a minute", countable, 3, 30 * time.Second, "x"},
		{"window over a day", countable, 3, 25 * time.Hour, "x"},
		{"no reason", countable, 3, time.Hour, "  "},
		{"unchanged", PolicyTarget{Name: "c", SubjectKinds: []string{"case"}, CountMin: 3, CountWindow: time.Hour}, 3, time.Hour, "x"},
	}
	for _, c := range cases {
		if _, err := NewCountSuggestion(c.p, c.min, c.window, c.why); !errs.IsKind(err, errs.KindValidation) {
			t.Errorf("%s: proposed (%v)", c.name, err)
		}
	}
	d, err := NewCountSuggestion(PolicyTarget{ID: countable.ID, Name: "c", SubjectKinds: []string{"case"},
		CountMin: 2, CountWindow: time.Minute}, 5, time.Hour, " flaps ")
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != SuggestCountCondition || d.Count.WasMin != 2 || d.Count.WasWindow != time.Minute || d.Why != "flaps" {
		t.Fatalf("draft = %+v", d)
	}
}

// TestAMembershipSuggestionForACurrentMemberIsRefusedAndAFormerOneIsNot.
func TestAMembershipSuggestionForACurrentMemberIsRefusedAndAFormerOneIsNot(t *testing.T) {
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	c := CaseSubject{CaseID: uuid.New(), Number: 9}
	in := IncidentSubject{IncidentID: uuid.New(), Number: 4,
		Members: []IncidentMember{{CaseID: c.CaseID, CaseNumber: 9, AddedAt: now}}}
	if _, err := NewMembershipSuggestion(in, c, "same story"); !errs.IsKind(err, errs.KindValidation) {
		t.Fatalf("a current member was proposed: %v", err)
	}
	in.Members[0].RemovedAt = now.Add(time.Minute)
	d, err := NewMembershipSuggestion(in, c, "same story")
	if err != nil {
		t.Fatal(err)
	}
	if d.Membership.IncidentNumber != 4 || d.Membership.CaseNumber != 9 {
		t.Fatalf("draft = %+v", d)
	}
}
