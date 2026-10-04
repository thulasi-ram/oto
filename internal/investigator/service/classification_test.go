package service

// git-bug 4298aa0's "Done when", against the scripted model and in-memory ports (ADR
// 0053 §5): with no classes configured a Finding carries no classification and nothing
// is offered; with classes the model must pick one or `unclassified`; a word outside
// the set is refused on the record and never stored; changing the set rewrites no
// earlier Finding; and the class travels with the Finding — published, and on the
// Incident's latest Finding that goes outbound.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/test/modelfake"
)

func (r *rig) writeClasses(t *testing.T, classes ...domain.Class) {
	t.Helper()
	set, err := domain.NewClassSet(classes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ReplaceClassSet(context.Background(), r.scope, set); err != nil {
		t.Fatal(err)
	}
}

func classify(id, class string) domain.ToolCall {
	return call(id, domain.ClassifyTool, `{"class":"`+class+`"}`)
}

var (
	deployRegression = domain.Class{Name: "deploy-regression", Description: "A change we shipped broke it."}
	capacity         = domain.Class{Name: "capacity", Description: "Something ran out."}
)

// TestWithNoClassesAFindingCarriesNoClassificationAndNothingIsOffered — oto ships no
// classes, so an org that wrote none is never asked, and NULL is not `unclassified`.
func TestWithNoClassesAFindingCarriesNoClassificationAndNothingIsOffered(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{modelfake.Text("The deploy at 09:00.", 300, 40)}

	got, _ := r.run(t, r.request(t, inv, c).ID)
	if got.Finding == "" || got.Classification != "" {
		t.Fatalf("run = %+v, want a Finding with no classification", got)
	}
	if f := r.findings.published[0]; f.Classification != "" {
		t.Fatalf("published classification %q, want none", f.Classification)
	}
	req := r.dial.model().Requests()[0]
	for _, tool := range req.Tools {
		if tool.Name == domain.ClassifyTool {
			t.Fatal("oto_classify was offered to an org with no classes")
		}
	}
	if p := req.Messages[0].Content; strings.Contains(p, "Classification") || strings.Contains(p, domain.Unclassified) {
		t.Fatalf("the prompt asked for a classification nobody configured:\n%s", p)
	}
}

// TestWithClassesTheModelIsToldTheSetAndItsPickIsStored — the set reaches the model as
// an enum of exactly the operator's words and `unclassified`, and the pick is the
// Finding's class, everywhere the Finding goes.
func TestWithClassesTheModelIsToldTheSetAndItsPickIsStored(t *testing.T) {
	r := newRig(t)
	r.writeClasses(t, deployRegression, capacity)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, call("c1", ToolCaseTimeline, `{}`), classify("c2", "deploy-regression")),
		modelfake.Text("The deploy at 09:00 doubled the error rate.", 500, 40),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)

	want := "model_turn tool_call:oto_case_timeline:ok tool_call:oto_classify:ok model_turn"
	if kinds(steps) != want {
		t.Fatalf("transcript = %s\nwant       %s", kinds(steps), want)
	}
	if got.Status != domain.StatusCompleted || got.Classification != "deploy-regression" {
		t.Fatalf("run = %+v", got)
	}
	// ⭐ The classification is the answer's shape, not a look at anything.
	if got.ToolCalls != 1 {
		t.Fatalf("tool_calls = %d, want 1 — oto_classify is not one of them", got.ToolCalls)
	}
	if f := r.findings.published[0]; f.Classification != "deploy-regression" {
		t.Fatalf("published classification %q", f.Classification)
	}

	req := r.dial.model().Requests()[0]
	var schema domain.ToolSchema
	for _, tool := range req.Tools {
		if tool.Name == domain.ClassifyTool {
			schema = tool
		}
	}
	var params struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.Parameters, &params); err != nil {
		t.Fatalf("oto_classify was not offered with a schema: %v", err)
	}
	if got := strings.Join(params.Properties["class"].Enum, ","); got != "deploy-regression,capacity,unclassified" {
		t.Fatalf("the enum is %q, want exactly the operator's classes and unclassified", got)
	}
	prompt := req.Messages[0].Content
	for _, w := range []string{"You read oto's history", "- deploy-regression: A change we shipped broke it.",
		"- capacity: Something ran out.", "unclassified", "right answer under doubt"} {
		if !strings.Contains(prompt, w) {
			t.Errorf("the prompt does not carry %q:\n%s", w, prompt)
		}
	}
	if strings.Contains(strings.ToLower(prompt), "noise") {
		t.Error("the prompt names a class the operator never wrote")
	}
}

// TestAWordOutsideTheSetIsRefusedOnTheRecordAndTheModelIsAskedAgain — an invalid pick is
// a refused Step whose answer is the set, so the model's next turn is the re-ask; the
// valid word it then says is the class, and the invalid one is never stored.
func TestAWordOutsideTheSetIsRefusedOnTheRecordAndTheModelIsAskedAgain(t *testing.T) {
	r := newRig(t)
	r.writeClasses(t, deployRegression, capacity)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, classify("c1", "noise")),
		modelfake.Calls(400, 20, classify("c2", "capacity")),
		modelfake.Text("The node pool ran out of memory.", 500, 40),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)

	want := "model_turn tool_call:oto_classify:refused model_turn tool_call:oto_classify:ok model_turn"
	if kinds(steps) != want {
		t.Fatalf("transcript = %s\nwant       %s", kinds(steps), want)
	}
	refusal := steps[1]
	if refusal.Call.Arguments != `{"class":"noise"}` ||
		!strings.Contains(refusal.Result, `"noise" is not one of this organisation's classes`) ||
		!strings.Contains(refusal.Result, "deploy-regression, capacity, unclassified") {
		t.Fatalf("the refusal recorded %q / %q", refusal.Call.Arguments, refusal.Result)
	}
	// The model heard the refusal before it answered again.
	second := r.dial.model().Requests()[1]
	if last := second.Messages[len(second.Messages)-1]; last.ToolCallID != "c1" || !strings.Contains(last.Content, "refused") {
		t.Fatalf("the re-ask did not carry the refusal: %+v", last)
	}
	if got.Classification != "capacity" {
		t.Fatalf("classification = %q, want the valid word the model said after the refusal", got.Classification)
	}
}

// TestARunThatNamesNothingInTheSetIsUnclassified — doubt, silence and a word outside
// the set all end `unclassified`, which is always admissible; a value outside the set is
// never what is stored.
func TestARunThatNamesNothingInTheSetIsUnclassified(t *testing.T) {
	for name, script := range map[string][]modelfake.Step{
		"it never classified": {modelfake.Text("Could be anything.", 300, 20)},
		"it only said a word outside the set": {
			modelfake.Calls(300, 20, classify("c1", "noise")),
			modelfake.Text("Probably noise.", 300, 20),
		},
		"its arguments named no class": {
			modelfake.Calls(300, 20, call("c1", domain.ClassifyTool, `{"kind":"capacity"}`)),
			modelfake.Text("Out of memory.", 300, 20),
		},
		"it chose unclassified": {
			modelfake.Calls(300, 20, classify("c1", domain.Unclassified)),
			modelfake.Text("Not sure.", 300, 20),
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.writeClasses(t, deployRegression, capacity)
			inv, c := r.setup(t, domain.DefaultBudgets())
			r.dial.script = script
			got, _ := r.run(t, r.request(t, inv, c).ID)
			if got.Status != domain.StatusCompleted || got.Classification != domain.Unclassified {
				t.Fatalf("run = %+v, want completed and unclassified", got)
			}
			if f := r.findings.published[0]; f.Classification != domain.Unclassified {
				t.Fatalf("published %q", f.Classification)
			}
		})
	}
}

// TestAClassificationCostsNoStepAndAPartialFindingKeepsIt — the step budget counts
// looks, not answers: a classify call past the budget is still taken, and the partial
// Finding is classified by it.
func TestAClassificationCostsNoStepAndAPartialFindingKeepsIt(t *testing.T) {
	r := newRig(t)
	r.writeClasses(t, deployRegression)
	b, err := domain.NewBudgets(1, domain.DefaultBudgets().MaxTokens, 300)
	if err != nil {
		t.Fatal(err)
	}
	inv, c := r.setup(t, b)
	// One turn that says where it got to, looks twice and classifies: the second look
	// is past the budget of one and is refused; the classification is not a look.
	r.dial.script = []modelfake.Step{
		{Text: "So far, a deploy.", Usage: domain.Usage{InputTokens: 300, OutputTokens: 20},
			ToolCalls: []domain.ToolCall{call("c1", ToolCaseTimeline, `{}`), call("c2", ToolRuleAtFire, ``),
				classify("c3", "deploy-regression")}},
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	want := "model_turn tool_call:oto_case_timeline:ok tool_call:oto_rule_at_fire:refused tool_call:oto_classify:ok"
	if kinds(steps) != want {
		t.Fatalf("transcript = %s\nwant       %s", kinds(steps), want)
	}
	if got.Status != domain.StatusExhausted || got.Ending.Reason != domain.ReasonStepBudget ||
		got.Finding != "So far, a deploy." || got.Classification != "deploy-regression" {
		t.Fatalf("run = %+v, want a partial Finding classified deploy-regression", got)
	}
}

// TestChangingTheSetRewritesNoEarlierFinding — a Finding keeps the class it was given:
// the set is renamed under it, and the old run still reads its old word, while the next
// run is held to the new set and a word only the old one had is refused.
func TestChangingTheSetRewritesNoEarlierFinding(t *testing.T) {
	r := newRig(t)
	r.writeClasses(t, deployRegression)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, classify("c1", "deploy-regression")),
		modelfake.Text("The deploy.", 300, 20),
	}
	first, _ := r.run(t, r.request(t, inv, c).ID)
	if first.Classification != "deploy-regression" {
		t.Fatalf("first run classified %q", first.Classification)
	}

	// The operator renames it.
	r.writeClasses(t, domain.Class{Name: "change-failure", Description: "A change we shipped broke it."})
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, classify("c1", "deploy-regression")),
		modelfake.Text("The deploy again.", 300, 20),
	}
	second, steps := r.run(t, r.request(t, inv, c).ID)

	old, err := r.svc.GetInvestigation(context.Background(), r.scope, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Investigation.Classification != "deploy-regression" {
		t.Fatalf("the earlier Finding now reads %q — changing the set rewrote it", old.Investigation.Classification)
	}
	if steps[1].Outcome != domain.OutcomeRefused || second.Classification != domain.Unclassified {
		t.Fatalf("a word the set no longer holds was taken: %s, %q", kinds(steps), second.Classification)
	}
	prior, err := r.investigations.PriorFindings(context.Background(), r.scope, c.AlertKey, second.ID, 5)
	if err != nil || len(prior) != 1 || prior[0].Classification != "deploy-regression" {
		t.Fatalf("prior Findings = %+v, %v — memory reads the class as it was given", prior, err)
	}
}

// TestAnIncidentsFindingCarriesItsClassificationOutbound — the class is on the
// Incident's latest Finding, which is what the `finding` fact and its card carry.
func TestAnIncidentsFindingCarriesItsClassificationOutbound(t *testing.T) {
	r := newRig(t)
	r.writeClasses(t, capacity)
	inv := r.incidentInvestigator(t, "storyline", true, false, ToolMemberFindings)
	incident := r.drawIncident()
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, classify("c1", "capacity")),
		modelfake.Text("One node pool ran out of memory.", 300, 20),
	}
	run := r.requestIncident(t, inv, incident)
	r.run(t, run.ID)

	latest, ok, err := r.svc.LatestIncidentFinding(context.Background(), r.scope, incident.IncidentID)
	if err != nil || !ok || latest.Classification != "capacity" {
		t.Fatalf("latest = %+v (%v, %v), want the class the Finding was given", latest, ok, err)
	}
	if len(r.declarer.declared) != 1 {
		t.Fatalf("%d Findings declared outbound, want 1", len(r.declarer.declared))
	}
}
