package service

// git-bug 180a525's "Done when", against the scripted model and in-memory ports: a
// request runs asynchronously and off the notification path; Steps are only ever
// appended; the Finding is published as the Enrichment `investigator.<name>`; each
// per-run budget ends a run `exhausted` with its partial Finding and the reason; the
// kill switch stops new runs; tokens spent are recorded; changing the model, prompt or
// allowlist makes a new version and the Finding names it. The SQL that holds the same
// rules (00092's triggers and CHECKs) is `repository/investigations_db_test.go`'s.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/test/modelfake"
)

const testKey = "sk-live-must-never-reach-a-step"

// setup configures one endpoint, one Investigator named `firstlook` holding the three
// built-in Tools, and one Case.
func (r *rig) setup(t *testing.T, budgets domain.Budgets) (domain.Investigator, domain.CaseSubject) {
	t.Helper()
	ctx := context.Background()
	cfg, err := r.svc.CreateProvider(ctx, r.scope, domain.ProviderDraft{
		Name: "gateway", BaseURL: "https://gw.test/v1", Model: "m-1", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := domain.NewAllowlist([]string{ToolCaseTimeline, ToolRuleAtFire, ToolPriorFindings})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := domain.NewVersionSpec(cfg.ID, "You read oto's history and say what is going on.", tools)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := r.svc.CreateInvestigator(ctx, r.scope, domain.InvestigatorDraft{
		Name: "firstlook", Enabled: true, Budgets: budgets, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	c := domain.CaseSubject{CaseID: uuid.New(), Number: 412, AlertID: uuid.New(), AlertKey: "ak-1",
		Alertname: "KubePodCrashLooping", Labels: map[string]string{"namespace": "payments"},
		State: "open", StartedAt: r.clock.Now().Add(-time.Hour), RuleSnapshotID: uuid.New()}
	r.history.cases[c.CaseID] = c
	r.history.timeline = []domain.TimelineEntry{{At: c.StartedAt, Type: "case.opened", Actor: "ingest", Summary: "fired"}}
	r.history.rule = domain.RuleAtFire{Available: true, Name: "KubePodCrashLooping", Expr: "rate(x[5m]) > 0", For: 15 * time.Minute}
	return inv, c
}

func (r *rig) request(t *testing.T, inv domain.Investigator, c domain.CaseSubject) domain.Investigation {
	t.Helper()
	by, err := domain.NewRequester(uuid.New(), "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.svc.RequestCaseInvestigation(context.Background(), r.scope, c.CaseID, inv.ID, by)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (r *rig) run(t *testing.T, id uuid.UUID) (domain.Investigation, []domain.Step) {
	t.Helper()
	if err := r.svc.RunInvestigation(context.Background(), r.scope, id); err != nil {
		t.Fatalf("run: %v", err)
	}
	d, err := r.svc.GetInvestigation(context.Background(), r.scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Investigation, d.Steps
}

func call(id, name, args string) domain.ToolCall {
	return domain.ToolCall{ID: id, Name: name, Arguments: args}
}

func kinds(steps []domain.Step) string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		k := string(s.Kind)
		if s.Kind == domain.StepToolCall {
			k += ":" + s.Call.Name + ":" + string(s.Outcome)
		}
		out = append(out, k)
	}
	return strings.Join(out, " ")
}

// TestARequestRunsAsynchronouslyAndOffTheNotificationPath — the request records the
// run and enqueues `investigations.run` on its own queue; nothing calls a model until
// the job does, and a run's only outputs are its rows and one Finding.
func TestARequestRunsAsynchronouslyAndOffTheNotificationPath(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{modelfake.Text("Looks like the payments deploy at 09:00.", 300, 40)}

	run := r.request(t, inv, c)
	if run.Status != domain.StatusQueued || run.VersionNumber != 1 || run.AlertKey != "ak-1" {
		t.Fatalf("requested run = %+v", run)
	}
	if r.dial.model() != nil {
		t.Fatal("a model was dialled on the request path")
	}
	if len(r.queue.jobs) != 1 {
		t.Fatalf("%d jobs enqueued, want 1", len(r.queue.jobs))
	}
	args, ok := r.queue.jobs[0].(jobs.InvestigationsRunArgs)
	if !ok || args.InvestigationID != run.ID || args.OrgID != r.scope.OrgID() {
		t.Fatalf("enqueued %#v", r.queue.jobs[0])
	}
	if q := args.InsertOpts().Queue; q != jobs.QueueInvestigate {
		t.Fatalf("the run rides %q", q)
	}

	got, steps := r.run(t, run.ID)
	if got.Status != domain.StatusCompleted || got.Finding == "" || len(steps) != 1 {
		t.Fatalf("run = %+v, steps %s", got, kinds(steps))
	}
	// ⛔ The run enqueued nothing: no notification, no amendment.
	if len(r.queue.jobs) != 1 {
		t.Fatalf("the run enqueued %d further job(s); a Finding never causes a notification", len(r.queue.jobs)-1)
	}
}

// TestARunRecordsEveryStepAndPublishesItsFindingAsAnEnrichment — the full tool-calling
// path over the built-in Tools, with tokens summed and the Finding naming its version.
func TestARunRecordsEveryStepAndPublishesItsFindingAsAnEnrichment(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{
		modelfake.Calls(500, 30, call("c1", ToolCaseTimeline, `{"limit":10}`), call("c2", ToolRuleAtFire, ``)),
		modelfake.Calls(700, 20, call("c3", ToolPriorFindings, `{}`)),
		modelfake.Text("The rule fired once; nothing before it.", 900, 60),
	}
	run := r.request(t, inv, c)
	got, steps := r.run(t, run.ID)

	want := "model_turn tool_call:oto_case_timeline:ok tool_call:oto_rule_at_fire:ok model_turn tool_call:oto_prior_findings:ok model_turn"
	if kinds(steps) != want {
		t.Fatalf("transcript = %s\nwant       %s", kinds(steps), want)
	}
	for i, s := range steps {
		if s.Seq != i+1 {
			t.Fatalf("step %d has seq %d", i, s.Seq)
		}
	}
	if !strings.Contains(steps[1].Result, "case.opened") || !strings.Contains(steps[2].Result, "rate(x[5m])") {
		t.Fatalf("the built-in Tools answered %q and %q", steps[1].Result, steps[2].Result)
	}
	if got.Status != domain.StatusCompleted || got.Spent.InputTokens != 2100 || got.Spent.OutputTokens != 110 || got.ToolCalls != 3 {
		t.Fatalf("run = %+v", got)
	}

	// The model heard each Tool result, answering the call that asked for it.
	reqs := r.dial.model().Requests()
	if len(reqs) != 3 || len(reqs[0].Tools) != 3 {
		t.Fatalf("%d requests, first offered %d Tools", len(reqs), len(reqs[0].Tools))
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if last.Role != domain.RoleTool || last.ToolCallID != "c2" {
		t.Fatalf("the second request ended with %+v", last)
	}

	if len(r.findings.published) != 1 {
		t.Fatalf("%d Findings published, want 1", len(r.findings.published))
	}
	f := r.findings.published[0]
	if f.Enricher != "investigator.firstlook" || f.SubjectKind != domain.SubjectCase || f.SubjectID != c.CaseID || f.Version != 1 ||
		f.VersionID != inv.Current.ID || f.Partial || f.Summary != got.Finding || f.Spent != got.Spent {
		t.Fatalf("published %+v", f)
	}
}

// TestEachBudgetEndsTheRunExhaustedKeepsThePartialFindingAndSaysWhy — ADR 0053 §6.
func TestEachBudgetEndsTheRunExhaustedKeepsThePartialFindingAndSaysWhy(t *testing.T) {
	budgets := func(steps int, tokens int64, wall int) domain.Budgets {
		b, err := domain.NewBudgets(steps, tokens, wall)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("steps", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, budgets(1, 100_000, 300))
		r.dial.script = []modelfake.Step{{
			Text:      "So far: the pod restarts every minute.",
			ToolCalls: []domain.ToolCall{call("c1", ToolCaseTimeline, `{}`), call("c2", ToolRuleAtFire, `{}`)},
			Usage:     domain.Usage{InputTokens: 100, OutputTokens: 10},
		}}
		got, steps := r.run(t, r.request(t, inv, c).ID)
		if got.Status != domain.StatusExhausted || got.Ending.Reason != domain.ReasonStepBudget {
			t.Fatalf("ended %+v", got.Ending)
		}
		// The call past the budget is recorded, refused, with the reason.
		if kinds(steps) != "model_turn tool_call:oto_case_timeline:ok tool_call:oto_rule_at_fire:refused" ||
			!strings.Contains(steps[2].Result, "step budget") {
			t.Fatalf("transcript = %s (%q)", kinds(steps), steps[len(steps)-1].Result)
		}
		assertPartial(t, r, got, "So far: the pod restarts every minute.")
	})

	t.Run("tokens", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, budgets(20, 1000, 300))
		r.dial.script = []modelfake.Step{{
			Text:      "Probably the deploy.",
			ToolCalls: []domain.ToolCall{call("c1", ToolCaseTimeline, `{}`)},
			Usage:     domain.Usage{InputTokens: 900, OutputTokens: 150},
		}}
		got, steps := r.run(t, r.request(t, inv, c).ID)
		if got.Status != domain.StatusExhausted || got.Ending.Reason != domain.ReasonTokenBudget {
			t.Fatalf("ended %+v", got.Ending)
		}
		if got.Spent.Total() != 1050 || kinds(steps) != "model_turn" {
			t.Fatalf("spent %d, transcript %s", got.Spent.Total(), kinds(steps))
		}
		// Each turn is capped at what is left of the budget.
		if mo := r.dial.model().Requests()[0].MaxOutputTokens; mo != 1000 {
			t.Fatalf("the first turn's output cap was %d, want the whole budget", mo)
		}
		assertPartial(t, r, got, "Probably the deploy.")
	})

	t.Run("tokens, answer cut at the cap", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, budgets(20, 1000, 300))
		r.dial.script = []modelfake.Step{
			modelfake.Calls(400, 10, call("c1", ToolCaseTimeline, `{}`)),
			{Text: "The cause is", Usage: domain.Usage{InputTokens: 500, OutputTokens: 200}},
		}
		got, _ := r.run(t, r.request(t, inv, c).ID)
		if got.Status != domain.StatusExhausted || got.Ending.Reason != domain.ReasonTokenBudget {
			t.Fatalf("ended %+v", got.Ending)
		}
		if mo := r.dial.model().Requests()[1].MaxOutputTokens; mo != 590 {
			t.Fatalf("the second turn's output cap was %d, want 590 left", mo)
		}
		assertPartial(t, r, got, "The cause is")
	})

	t.Run("wall time", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, budgets(20, 100_000, 10))
		r.svc.tools = append(r.svc.tools, funcTool{name: ToolCaseTimeline + "_slow", fn: func(context.Context) (string, error) {
			r.clock.Advance(11 * time.Second) // the Tool took longer than the whole run may.
			return "{}", nil
		}})
		// Hold the slow Tool under its own name, by a new version.
		tools, _ := domain.NewAllowlist([]string{ToolCaseTimeline + "_slow"})
		if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Tools: &tools}); err != nil {
			t.Fatal(err)
		}
		inv, _ = r.svc.investigators.Get(context.Background(), r.scope, inv.ID)
		r.dial.script = []modelfake.Step{
			{Text: "Reading the timeline first.", ToolCalls: []domain.ToolCall{call("c1", ToolCaseTimeline+"_slow", `{}`)},
				Usage: domain.Usage{InputTokens: 100, OutputTokens: 10}},
			modelfake.Text("never asked", 1, 1),
		}
		got, steps := r.run(t, r.request(t, inv, c).ID)
		if got.Status != domain.StatusExhausted || got.Ending.Reason != domain.ReasonWallTime {
			t.Fatalf("ended %+v", got.Ending)
		}
		if kinds(steps) != "model_turn tool_call:oto_case_timeline_slow:ok" || r.dial.model().Remaining() != 1 {
			t.Fatalf("transcript %s, %d turn(s) unasked", kinds(steps), r.dial.model().Remaining())
		}
		assertPartial(t, r, got, "Reading the timeline first.")
	})
}

func assertPartial(t *testing.T, r *rig, got domain.Investigation, finding string) {
	t.Helper()
	if got.Finding != finding || !got.Partial() || got.Ending.Detail == "" {
		t.Fatalf("finding %q partial=%v detail=%q, want %q kept as partial with a reason",
			got.Finding, got.Partial(), got.Ending.Detail, finding)
	}
	if len(r.findings.published) != 1 || !r.findings.published[0].Partial || r.findings.published[0].Summary != finding {
		t.Fatalf("published %+v, want the partial Finding", r.findings.published)
	}
}

// TestATurnWithoutUsageFailsTheRun — never counted as free (ADR 0053 §3).
func TestATurnWithoutUsageFailsTheRun(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{modelfake.WithoutUsage("free tokens, apparently")}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonUsageMissing {
		t.Fatalf("ended %+v", got.Ending)
	}
	if len(steps) != 0 || got.Finding != "" || len(r.findings.published) != 0 {
		t.Fatalf("an unbudgetable turn left steps %s, finding %q, %d published", kinds(steps), got.Finding, len(r.findings.published))
	}
}

// TestAModelErrorIsRecordedAndSaysOnlyWhatIsSafe — the ending carries oto's own code
// and message, never a cause chain.
func TestAModelErrorIsRecordedAndSaysOnlyWhatIsSafe(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{modelfake.Fail(errs.UpstreamDown("model_unavailable", "the model endpoint answered 503",
		errors.New("POST https://gw.test/v1/chat/completions Authorization: Bearer "+testKey)))}
	got, _ := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonModelError {
		t.Fatalf("ended %+v", got.Ending)
	}
	if strings.Contains(got.Ending.Detail, testKey) || !strings.Contains(got.Ending.Detail, "503") {
		t.Fatalf("detail = %q", got.Ending.Detail)
	}
}

// TestTheKillSwitchStopsNewRunsAndIsRecorded — "Enabled: nothing starts", and hitting
// it is recorded, never silent (ADR 0053 §6).
func TestTheKillSwitchStopsNewRunsAndIsRecorded(t *testing.T) {
	t.Run("org off at request", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, domain.DefaultBudgets())
		r.orgControls.on = false
		run := r.request(t, inv, c)
		if run.Status != domain.StatusSkipped || run.Ending.Reason != domain.ReasonDisabled ||
			!strings.Contains(run.Ending.Detail, "investigations_enabled") {
			t.Fatalf("run = %+v", run)
		}
		if len(r.queue.jobs) != 0 {
			t.Fatal("a switched-off request was enqueued")
		}
	})
	t.Run("Investigator off at request", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, domain.DefaultBudgets())
		off := false
		if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Enabled: &off}); err != nil {
			t.Fatal(err)
		}
		run := r.request(t, inv, c)
		if run.Status != domain.StatusSkipped || !strings.Contains(run.Ending.Detail, "firstlook") || len(r.queue.jobs) != 0 {
			t.Fatalf("run = %+v, %d jobs", run, len(r.queue.jobs))
		}
	})
	t.Run("switched off before the job began", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, domain.DefaultBudgets())
		r.dial.script = []modelfake.Step{modelfake.Text("should not run", 1, 1)}
		run := r.request(t, inv, c)
		r.orgControls.on = false
		got, steps := r.run(t, run.ID)
		if got.Status != domain.StatusSkipped || got.Ending.Reason != domain.ReasonDisabled || len(steps) != 0 {
			t.Fatalf("run = %+v, steps %s", got, kinds(steps))
		}
		if r.dial.model() != nil {
			t.Fatal("a model was dialled for a run whose switch was off")
		}
		if !got.StartedAt.IsZero() {
			t.Fatal("a skipped run claims to have started")
		}
	})
}

// TestACallOutsideTheAllowlistIsRefusedRecordedAndTheRunContinues — §6.
func TestACallOutsideTheAllowlistIsRefusedRecordedAndTheRunContinues(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{
		modelfake.Calls(100, 10, call("c1", "kubectl_exec", `{"cmd":"delete pod"}`), call("c2", "oto_unknown", `{}`),
			call("c3", ToolCaseTimeline, `not json`)),
		modelfake.Text("I could not run that; here is what I know.", 200, 20),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	want := "model_turn tool_call:kubectl_exec:refused tool_call:oto_unknown:refused tool_call:oto_case_timeline:failed model_turn"
	if kinds(steps) != want || got.Status != domain.StatusCompleted {
		t.Fatalf("transcript %s, status %s", kinds(steps), got.Status)
	}
	if !strings.Contains(steps[1].Result, "allowlist") || steps[1].Call.Arguments != `{"cmd":"delete pod"}` ||
		steps[3].Call.Arguments != "not json" {
		t.Fatalf("refusal %q, arguments %q / %q", steps[1].Result, steps[1].Call.Arguments, steps[3].Call.Arguments)
	}
}

// TestTheCallTimeoutAndSizeCapAreRecordedAndTheRunContinues — the per-call controls
// never end a run.
func TestTheCallTimeoutAndSizeCapAreRecordedAndTheRunContinues(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.svc.tools = append(r.svc.tools,
		funcTool{name: "slow", fn: func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }},
		funcTool{name: "huge", fn: func(context.Context) (string, error) { return strings.Repeat("é", 5000), nil }},
	)
	tools, _ := domain.NewAllowlist([]string{"slow", "huge"})
	if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Tools: &tools}); err != nil {
		t.Fatal(err)
	}
	inv, _ = r.svc.investigators.Get(context.Background(), r.scope, inv.ID)
	r.dial.script = []modelfake.Step{
		modelfake.Calls(100, 10, call("c1", "slow", `{}`), call("c2", "huge", `{}`)),
		modelfake.Text("done", 100, 10),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if kinds(steps) != "model_turn tool_call:slow:timeout tool_call:huge:truncated model_turn" || got.Status != domain.StatusCompleted {
		t.Fatalf("transcript %s, status %s", kinds(steps), got.Status)
	}
	if !strings.Contains(steps[2].Result, "[truncated:") || len(steps[2].Result) > 4096+64 {
		t.Fatalf("truncated result is %d bytes: %q…", len(steps[2].Result), steps[2].Result[:40])
	}
}

// TestARunFoundRunningIsInterruptedNotRunAgain — a re-run would pay twice.
func TestARunFoundRunningIsInterruptedNotRunAgain(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{modelfake.Text("should not run", 1, 1)}
	run := r.request(t, inv, c)
	if got, err := r.investigations.Start(context.Background(), r.scope, run.ID, r.clock.Now(), 2); got != domain.StartBegan || err != nil {
		t.Fatal("could not start", err)
	}
	got, _ := r.run(t, run.ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonInterrupted || r.dial.model() != nil {
		t.Fatalf("run = %+v", got.Ending)
	}
	// And an ended run is frozen: running its job again changes nothing.
	if err := r.svc.RunInvestigation(context.Background(), r.scope, run.ID); err != nil {
		t.Fatal(err)
	}
}

// TestAStepThatCannotBeRecordedStopsTheRun — a transcript with a hole is not one.
func TestAStepThatCannotBeRecordedStopsTheRun(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{modelfake.Calls(10, 1, call("c1", ToolCaseTimeline, `{}`)), modelfake.Text("x", 1, 1)}
	r.investigations.failSteps = errs.Internal("disk_full", errors.New("no space"))
	got, _ := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonInternal || r.dial.model().Remaining() != 1 {
		t.Fatalf("run = %+v, %d turns left", got.Ending, r.dial.model().Remaining())
	}
}

// TestTheAPIKeyNeverReachesAStepOrAnEnding — the loop never holds the key.
func TestTheAPIKeyNeverReachesAStepOrAnEnding(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{
		modelfake.Calls(10, 1, call("c1", ToolCaseTimeline, `{}`), call("c2", "nope", `{}`)),
		modelfake.Text("fine", 1, 1),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if r.dial.gotKey != testKey {
		t.Fatal("the adapter was not handed the key")
	}
	for _, s := range steps {
		for _, field := range []string{s.Text, s.Result, s.Call.Arguments, s.Call.Name} {
			if strings.Contains(field, testKey) {
				t.Fatalf("the key reached Step %d", s.Seq)
			}
		}
	}
	if strings.Contains(got.Ending.Detail+got.Finding, testKey) {
		t.Fatal("the key reached the run")
	}
}

// TestChangingModelPromptOrAllowlistMakesANewVersionAndTheFindingNamesIt — §6.
func TestChangingModelPromptOrAllowlistMakesANewVersionAndTheFindingNamesIt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	inv, c := r.setup(t, domain.DefaultBudgets())
	version := func() int {
		t.Helper()
		got, err := r.svc.investigators.Get(ctx, r.scope, inv.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Current.Number
	}
	change := func(ch domain.InvestigatorChange) {
		t.Helper()
		if _, err := r.svc.UpdateInvestigator(ctx, r.scope, inv.ID, ch); err != nil {
			t.Fatal(err)
		}
	}

	// Not versioned: the kill switch, the budgets, re-sending what is current.
	off, b := false, domain.Budgets{MaxSteps: 5, MaxTokens: 5000, MaxWall: time.Minute}
	same := inv.Current.Prompt
	reordered, _ := domain.NewAllowlist([]string{ToolPriorFindings, ToolRuleAtFire, ToolCaseTimeline})
	change(domain.InvestigatorChange{Enabled: &off, Budgets: &b, Prompt: &same, Tools: &reordered})
	if v := version(); v != 1 {
		t.Fatalf("version %d after no versioned change", v)
	}
	on := true
	change(domain.InvestigatorChange{Enabled: &on})

	prompt := "Be brief."
	change(domain.InvestigatorChange{Prompt: &prompt})
	if v := version(); v != 2 {
		t.Fatalf("a new prompt made version %d, want 2", v)
	}
	fewer, _ := domain.NewAllowlist([]string{ToolCaseTimeline})
	change(domain.InvestigatorChange{Tools: &fewer})
	if v := version(); v != 3 {
		t.Fatalf("a new allowlist made version %d, want 3", v)
	}
	other, err := r.svc.CreateProvider(ctx, r.scope, domain.ProviderDraft{Name: "other", BaseURL: "https://gw.test/v1", Model: "m-2"})
	if err != nil {
		t.Fatal(err)
	}
	change(domain.InvestigatorChange{ProviderID: &other.ID})
	if v := version(); v != 4 {
		t.Fatalf("a new model made version %d, want 4", v)
	}

	inv, _ = r.svc.investigators.Get(ctx, r.scope, inv.ID)
	run := r.request(t, inv, c)
	// A version written after the request does not move the run it pinned.
	prompt2 := "Be briefer."
	change(domain.InvestigatorChange{Prompt: &prompt2})
	r.dial.script = []modelfake.Step{modelfake.Text("v4 says hello", 10, 1)}
	got, _ := r.run(t, run.ID)
	if got.VersionNumber != 4 || got.Model.Model != "m-2" {
		t.Fatalf("the run names version %d of %s", got.VersionNumber, got.Model)
	}
	if f := r.findings.published[0]; f.Version != 4 || f.VersionID != inv.Current.ID || f.Model.Model != "m-2" {
		t.Fatalf("the Finding names %+v", f)
	}
	if r.dial.model().Requests()[0].Messages[0].Content != "Be brief." {
		t.Fatal("the run did not use its pinned version's prompt")
	}
}

// TestAModelThatChangedUnderAVersionIsRefused — a Finding never names a model that did
// not produce it.
func TestAModelThatChangedUnderAVersionIsRefused(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	run := r.request(t, inv, c)
	p := r.store.rows[inv.Current.ProviderID]
	p.Model = "m-renamed"
	r.store.rows[p.ID] = p
	got, _ := r.run(t, run.ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonModelChanged {
		t.Fatalf("ended %+v", got.Ending)
	}
}

// TestAGoneCaseEndsTheRun — a drill disposed of its Case before the job began.
func TestAGoneCaseEndsTheRun(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	run := r.request(t, inv, c)
	delete(r.history.cases, c.CaseID)
	got, _ := r.run(t, run.ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonSubjectGone {
		t.Fatalf("ended %+v", got.Ending)
	}
}

// TestPriorFindingsAreOffered — the second run reads the first run's Finding on the
// same alert_key, and never its own.
func TestPriorFindingsAreOffered(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{modelfake.Text("first look: the deploy", 10, 1)}
	r.run(t, r.request(t, inv, c).ID)

	r.dial.script = []modelfake.Step{modelfake.Calls(10, 1, call("c1", ToolPriorFindings, `{"limit":3}`)), modelfake.Text("same again", 10, 1)}
	_, steps := r.run(t, r.request(t, inv, c).ID)
	if !strings.Contains(steps[1].Result, "first look: the deploy") || strings.Contains(steps[1].Result, "same again") {
		t.Fatalf("prior Findings answered %q", steps[1].Result)
	}
}

// TestTheJobOutlastsTheLongestWallBudget — the budget, not the job timeout, ends a run
// and gets to record how.
func TestTheJobOutlastsTheLongestWallBudget(t *testing.T) {
	if jobs.InvestigationJobTimeout <= domain.MaxWallSeconds*time.Second {
		t.Fatalf("the job times out at %s, inside the %ds wall budget an Investigator may set",
			jobs.InvestigationJobTimeout, domain.MaxWallSeconds)
	}
}

// TestCreatingAnInvestigatorPinsTheEndpointsIdentity — and another org's endpoint is
// a 404.
func TestCreatingAnInvestigatorPinsTheEndpointsIdentity(t *testing.T) {
	r := newRig(t)
	inv, _ := r.setup(t, domain.DefaultBudgets())
	if inv.Current.Model != (domain.ModelIdentity{Endpoint: "https://gw.test/v1", Model: "m-1"}) || inv.EnricherName() != "investigator.firstlook" {
		t.Fatalf("version 1 = %+v", inv.Current)
	}
	spec, _ := domain.NewVersionSpec(uuid.New(), "p", domain.Allowlist{})
	if _, err := r.svc.CreateInvestigator(context.Background(), r.scope, domain.InvestigatorDraft{
		Name: "second", Enabled: true, Budgets: domain.DefaultBudgets(), Spec: spec}); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("an unknown endpoint gave %v", err)
	}
}

// TestATurnAsksForNoMoreThanOneAnswerCanBe — review A1: an endpoint asked for the whole
// budget as one answer refuses the request, so each turn asks for the per-turn cap or
// what is left, whichever is less; a turn cut at the cap with budget left is not the
// budget speaking.
func TestATurnAsksForNoMoreThanOneAnswerCanBe(t *testing.T) {
	budgets := func(tokens int64) domain.Budgets {
		b, err := domain.NewBudgets(20, tokens, 300)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("a large budget sends the per-turn cap", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, budgets(200_000))
		r.dial.script = []modelfake.Step{modelfake.Text("The deploy.", 300, 40)}
		r.run(t, r.request(t, inv, c).ID)
		if mo := r.dial.model().Requests()[0].MaxOutputTokens; mo != domain.MaxTurnOutputTokens {
			t.Fatalf("output cap = %d, want %d", mo, domain.MaxTurnOutputTokens)
		}
	})

	t.Run("a small remainder sends the remainder", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, budgets(3000))
		r.dial.script = []modelfake.Step{
			modelfake.Calls(900, 100, call("c1", ToolCaseTimeline, `{}`)),
			modelfake.Text("The deploy.", 300, 40),
		}
		r.run(t, r.request(t, inv, c).ID)
		if mo := r.dial.model().Requests()[1].MaxOutputTokens; mo != 2000 {
			t.Fatalf("second turn's output cap = %d, want the 2000 left", mo)
		}
	})

	t.Run("cut at the per-turn cap with budget left is a model error", func(t *testing.T) {
		r := newRig(t)
		inv, c := r.setup(t, budgets(200_000))
		r.dial.script = []modelfake.Step{{Text: "The cause is", Usage: domain.Usage{InputTokens: 300, OutputTokens: 4096},
			Finish: domain.FinishLength}}
		got, steps := r.run(t, r.request(t, inv, c).ID)
		if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonModelError ||
			!strings.Contains(got.Ending.Detail, "per-turn output cap") {
			t.Fatalf("ended %+v, want model_error naming the per-turn cap", got.Ending)
		}
		if got.Finding != "" || len(r.findings.published) != 0 || steps[0].Text != "The cause is" {
			t.Fatalf("finding %q, %d published, step text %q: the cut text stays in the Step only",
				got.Finding, len(r.findings.published), steps[0].Text)
		}
	})
}

// TestAFinalTurnThatSaysNothingHasNoFinding — review A6: earlier narration is not a
// conclusion, so a run whose last turn is empty fails rather than publish "let me check".
func TestAFinalTurnThatSaysNothingHasNoFinding(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{
		{Text: "Let me check the timeline.", ToolCalls: []domain.ToolCall{call("c1", ToolCaseTimeline, `{}`)},
			Usage: domain.Usage{InputTokens: 100, OutputTokens: 10}},
		modelfake.Text("", 200, 1),
	}
	got, _ := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonModelError ||
		!strings.Contains(got.Ending.Detail, "without an answer") {
		t.Fatalf("ended %+v, want model_error: the model ended without an answer", got.Ending)
	}
	if got.Finding != "" || len(r.findings.published) != 0 {
		t.Fatalf("finding %q, %d published: stale narration became the answer", got.Finding, len(r.findings.published))
	}
}

// TestARefusedTurnIsCountedAndRecorded — review A7: an answer the endpoint billed and oto
// could not take still costs the run its tokens, on a model-turn Step.
func TestARefusedTurnIsCountedAndRecorded(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.dial.script = []modelfake.Step{modelfake.Fail(domain.RefusedTurn(domain.Usage{InputTokens: 100, OutputTokens: 20},
		errs.UpstreamDown("model_no_choice", "the model endpoint answered with no choice", nil)))}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonModelError {
		t.Fatalf("ended %+v", got.Ending)
	}
	if got.Spent.Total() != 120 || len(steps) != 1 || steps[0].Kind != domain.StepModelTurn ||
		steps[0].Usage.Total() != 120 || !strings.Contains(steps[0].Text, "model_no_choice") {
		t.Fatalf("spent %d, steps %+v: the billed turn is not on the record", got.Spent.Total(), steps)
	}
}

// TestANulFromAToolDoesNotEndTheRun — review A4: Postgres refuses U+0000, so a result
// holding one is cleaned before it is recorded, and the per-call result never ends a run.
func TestANulFromAToolDoesNotEndTheRun(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.svc.tools = append(r.svc.tools, funcTool{name: "nul", fn: func(context.Context) (string, error) {
		return "pod\x00name\xff", nil
	}})
	tools, _ := domain.NewAllowlist([]string{"nul"})
	if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Tools: &tools}); err != nil {
		t.Fatal(err)
	}
	inv, _ = r.svc.investigators.Get(context.Background(), r.scope, inv.ID)
	r.dial.script = []modelfake.Step{modelfake.Calls(100, 10, call("c1", "nul", `{}`)), modelfake.Text("done", 100, 10)}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusCompleted {
		t.Fatalf("ended %+v", got.Ending)
	}
	if strings.Contains(steps[1].Result, "\x00") {
		t.Fatalf("result %q still holds a NUL", steps[1].Result)
	}
	// The model read the same bytes the Step kept.
	sent := r.dial.model().Requests()[1].Messages
	if last := sent[len(sent)-1]; last.Content != steps[1].Result {
		t.Fatalf("the model read %q, the Step kept %q", last.Content, steps[1].Result)
	}
}

// TestAnswerShapingCallsAreFreeOnlyUpToTheCap — review A11: classifications cost no step
// up to MaxFreeCallsPerRun; past it each is refused on the record and counted.
func TestAnswerShapingCallsAreFreeOnlyUpToTheCap(t *testing.T) {
	r := newRig(t)
	r.writeClasses(t, deployRegression, capacity)
	inv, c := r.setup(t, domain.DefaultBudgets())
	calls := make([]domain.ToolCall, 0, domain.MaxFreeCallsPerRun+10)
	for i := range domain.MaxFreeCallsPerRun + 10 {
		calls = append(calls, classify(fmt.Sprintf("c%d", i), "capacity"))
	}
	r.dial.script = []modelfake.Step{modelfake.Calls(300, 200, calls...), modelfake.Text("Out of memory.", 300, 20)}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusCompleted || got.ToolCalls != 10 {
		t.Fatalf("ended %+v with %d Tool calls, want completed with the 10 past the cap counted", got.Ending, got.ToolCalls)
	}
	refused := 0
	for _, s := range steps {
		if s.Kind == domain.StepToolCall && s.Outcome == domain.OutcomeRefused {
			refused++
			if !strings.Contains(s.Result, "answer-shaping") {
				t.Fatalf("refusal says %q", s.Result)
			}
		}
	}
	if refused != 10 || got.Classification != "capacity" {
		t.Fatalf("%d refused, class %q; want calls 51-60 refused and the pick kept", refused, got.Classification)
	}
}

// TestAnInterruptedRunRecordsWhatItsStepsSpent — review A5: the run's own counters are
// written only at its end, so the ending its worker never reached sums its Steps.
func TestAnInterruptedRunRecordsWhatItsStepsSpent(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	run := r.request(t, inv, c)
	ctx := context.Background()
	if got, err := r.investigations.Start(ctx, r.scope, run.ID, r.clock.Now(), 2); got != domain.StartBegan || err != nil {
		t.Fatal("could not start", err)
	}
	now := r.clock.Now()
	for _, st := range []domain.Step{
		domain.NewModelTurnStep(1, domain.Turn{ToolCalls: []domain.ToolCall{call("c1", ToolCaseTimeline, `{}`)},
			Usage: domain.Usage{InputTokens: 100, OutputTokens: 10}}, 0, now),
		domain.NewToolStep(2, call("c1", ToolCaseTimeline, `{}`), domain.OutcomeOK, "[]", 0, now),
		domain.NewModelTurnStep(3, domain.Turn{ToolCalls: []domain.ToolCall{classify("c2", "capacity")},
			Usage: domain.Usage{InputTokens: 200, OutputTokens: 20}}, 0, now),
		domain.NewToolStep(4, classify("c2", "capacity"), domain.OutcomeOK, "recorded", 0, now),
	} {
		if err := r.investigations.AppendStep(ctx, r.scope, run.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := r.run(t, run.ID)
	if got.Ending.Reason != domain.ReasonInterrupted || got.Spent.Total() != 330 || got.ToolCalls != 1 {
		t.Fatalf("ended %s with %d tokens and %d Tool calls, want interrupted with 330 and 1",
			got.Ending.Reason, got.Spent.Total(), got.ToolCalls)
	}
}

// TestAnAbandonedRunEndsFailedAndAnEndedOneStands — review A5 / D3: a run whose job gives
// up is ended `failed/internal` rather than left `queued` (polled forever) or `running`
// (holding a concurrency slot forever); a run that already ended is not touched.
func TestAnAbandonedRunEndsFailedAndAnEndedOneStands(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	ctx := context.Background()
	cause := errs.Internal("db_down", errors.New("dial tcp: connection refused"))

	queued := r.request(t, inv, c)
	if err := r.svc.AbandonInvestigation(ctx, r.scope, queued.ID, cause); err != nil {
		t.Fatal(err)
	}
	got, _ := r.investigations.Get(ctx, r.scope, queued.ID)
	if got.Status != domain.StatusFailed || got.Ending.Reason != domain.ReasonInternal ||
		!strings.Contains(got.Ending.Detail, "stopped retrying") || strings.Contains(got.Ending.Detail, "dial tcp") {
		t.Fatalf("abandoned queued run = %+v", got.Ending)
	}

	r.dial.script = []modelfake.Step{modelfake.Text("The deploy.", 10, 1)}
	done, _ := r.run(t, r.request(t, inv, c).ID)
	if err := r.svc.AbandonInvestigation(ctx, r.scope, done.ID, cause); err != nil {
		t.Fatal(err)
	}
	if again, _ := r.investigations.Get(ctx, r.scope, done.ID); again.Status != domain.StatusCompleted || again.Ending != done.Ending {
		t.Fatalf("a completed run was rewritten: %+v", again.Ending)
	}
}
