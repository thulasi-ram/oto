package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAnAllowlistIsExactSortedAndOrderBlind(t *testing.T) {
	for _, bad := range []string{"*", "oto_*", "a.b", "", "x y", strings.Repeat("a", 65)} {
		if _, err := NewAllowlist([]string{bad}); err == nil {
			t.Fatalf("%q accepted as a Tool name", bad)
		}
	}
	if _, err := NewAllowlist([]string{"a", "a"}); err == nil {
		t.Fatal("a repeated name accepted")
	}
	a, _ := NewAllowlist([]string{"b", "a"})
	b, _ := NewAllowlist([]string{"a", "b"})
	if !a.Equal(b) || !a.Allows("a") || a.Allows("c") || a.Names()[0] != "a" {
		t.Fatalf("allowlist %v", a.Names())
	}
}

func TestANameTakesTheEnricherAlphabet(t *testing.T) {
	for _, ok := range []string{"a", "firstlook", "v2"} {
		if _, err := NewInvestigatorName(ok); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "First", "first-look", "first_look", "1st", strings.Repeat("a", 64)} {
		if _, err := NewInvestigatorName(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestBudgetsMirrorTheDDL(t *testing.T) {
	if _, err := NewBudgets(MinStepBudget, MinTokenBudget, MinWallSeconds); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBudgets(MaxStepBudget, MaxTokenBudget, MaxWallSeconds); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][3]int64{{0, 1000, 10}, {101, 1000, 10}, {1, 999, 10}, {1, 2_000_001, 10}, {1, 1000, 9}, {1, 1000, 1801}} {
		if _, err := NewBudgets(int(b[0]), b[1], int(b[2])); err == nil {
			t.Fatalf("%v accepted", b)
		}
	}
	if DefaultBudgets().WallSeconds() != DefaultWallSeconds {
		t.Fatal("default wall")
	}
}

func TestAnEndingsReasonBelongsToItsStatus(t *testing.T) {
	cases := map[Reason]Status{
		ReasonStepBudget: StatusExhausted, ReasonTokenBudget: StatusExhausted, ReasonWallTime: StatusExhausted,
		ReasonUsageMissing: StatusFailed, ReasonModelError: StatusFailed, ReasonModelChanged: StatusFailed,
		ReasonSubjectGone: StatusFailed, ReasonInterrupted: StatusFailed, ReasonInternal: StatusFailed,
		ReasonDisabled: StatusSkipped,
	}
	for r, st := range cases {
		e := EndedBy(r, "")
		if e.Status != st || e.Detail == "" {
			t.Fatalf("%s ended %+v", r, e)
		}
		if _, err := RestoreEnding(st, string(r), "d"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RestoreEnding(StatusExhausted, string(ReasonDisabled), "d"); err == nil {
		t.Fatal("exhausted/disabled restored")
	}
	if e := EndedBy("bogus", "x"); e.Reason != ReasonInternal {
		t.Fatalf("an unknown reason ended %+v", e)
	}
	if long := EndedBy(ReasonModelError, strings.Repeat("x", 3000)); len(long.Detail) != MaxReasonDetail {
		t.Fatalf("detail is %d long", len(long.Detail))
	}
}

func TestAVersionChangesOnlyWithModelPromptOrAllowlist(t *testing.T) {
	tools, _ := NewAllowlist([]string{"a"})
	p := uuid.New()
	m := ModelIdentity{Endpoint: "https://x", Model: "m"}
	v := Version{ProviderID: p, Model: m, Prompt: "hi", Tools: tools}
	spec, _ := NewVersionSpec(p, " hi ", tools)
	if v.NeedsNewVersion(spec, m) {
		t.Fatal("the same spec is a new version")
	}
	if !v.NeedsNewVersion(spec, ModelIdentity{Endpoint: "https://x", Model: "m2"}) {
		t.Fatal("a renamed model is not a new version")
	}
	other, _ := NewAllowlist([]string{"b"})
	if !v.NeedsNewVersion(VersionSpec{ProviderID: p, Prompt: "hi", Tools: other}, m) {
		t.Fatal("a new allowlist is not a new version")
	}
}

func TestStepsAreClippedToTheirColumns(t *testing.T) {
	s := NewToolStep(1, ToolCall{ID: "c", Name: "t", Arguments: strings.Repeat("a", MaxStepText+5)},
		OutcomeOK, strings.Repeat("é", MaxStepText+5), -time.Second, time.Now())
	if len([]rune(s.Result)) != MaxStepText || len(s.Call.Arguments) != MaxStepText || s.Duration != 0 {
		t.Fatalf("step not clipped: %d %d %s", len([]rune(s.Result)), len(s.Call.Arguments), s.Duration)
	}
	if NewFinding("   ") != "" || len([]rune(NewFinding(strings.Repeat("x", MaxFindingLength+1)))) != MaxFindingLength {
		t.Fatal("finding not normalised")
	}
}
