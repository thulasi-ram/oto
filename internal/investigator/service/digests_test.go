package service

// git-bug 3e96f5a's "Done when", the investigator half, against the scripted model and
// in-memory ports (ADR 0053 §2, §4): a summarised digest window's run is armed ahead of
// its close, once; the Finding a digest may carry is only an ENDED run's — a run still
// queued or running, or one that failed or was skipped, is the built-in body; and a
// digest window's Finding is neither published as an Enrichment nor declared. The send
// half — the digest sent on time with the run still running, and never amended — is
// `notification/service/digest_finding_test.go`'s.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/test/modelfake"
)

// digestInvestigator writes an Investigator with its own endpoint and the given budgets.
func (r *rig) digestInvestigator(t *testing.T, name string, enabled bool, b domain.Budgets, tools ...string) domain.Investigator {
	t.Helper()
	ctx := context.Background()
	cfg, err := r.svc.CreateProvider(ctx, r.scope, domain.ProviderDraft{
		Name: "gw-" + name, BaseURL: "https://gw.test/v1", Model: "m-1", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	allow, err := domain.NewAllowlist(tools)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := domain.NewVersionSpec(cfg.ID, "You summarise a window of a notification policy's Cases.", allow)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := r.svc.CreateInvestigator(ctx, r.scope, domain.InvestigatorDraft{
		Name: name, Enabled: enabled, Budgets: b, MinInterval: domain.DefaultMinInterval(), Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// hourWindow is the hour-long window the rig's clock (09:00 UTC) opens.
func (r *rig) hourWindow(t *testing.T) domain.DigestWindow {
	t.Helper()
	start := r.clock.Now().UTC().Truncate(time.Hour)
	w, err := domain.NewDigestWindow(start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// summarise has one policy ask `inv` to summarise its hourly digest, and returns it.
func (r *rig) summarise(t *testing.T, inv domain.Investigator) domain.SummarisedDigest {
	t.Helper()
	d := domain.SummarisedDigest{PolicyID: uuid.New(), PolicyName: "payments digest",
		InvestigatorID: inv.ID, Window: r.hourWindow(t)}
	r.digests.policies = append(r.digests.policies, d)
	r.digests.cases[d.PolicyID] = []domain.DigestCase{{CaseID: uuid.New(), Alertname: "KubePodCrashLooping",
		Labels:    map[string]string{"alertname": "KubePodCrashLooping", "namespace": "payments"},
		StartedAt: d.Window.Start.Add(5 * time.Minute)}}
	return d
}

func (r *rig) arm(t *testing.T) int {
	t.Helper()
	n, err := r.svc.ArmDigestInvestigations(context.Background(), r.scope)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *rig) carried(t *testing.T, d domain.SummarisedDigest) (domain.Investigation, bool) {
	t.Helper()
	run, ok, err := r.svc.DigestFinding(context.Background(), r.scope, d.PolicyID, d.Window)
	if err != nil {
		t.Fatal(err)
	}
	return run, ok
}

func digestBudgets(t *testing.T, steps int, tokens int64, wall int) domain.Budgets {
	t.Helper()
	b, err := domain.NewBudgets(steps, tokens, wall)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestADigestWindowIsArmedOnceItsLeadHasBegunAndOnlyOnce — the run starts AHEAD of the
// close, by the Investigator's wall-time budget plus the slack, and one window is one run.
func TestADigestWindowIsArmedOnceItsLeadHasBegunAndOnlyOnce(t *testing.T) {
	r := newRig(t)
	inv := r.digestInvestigator(t, "digest", true, digestBudgets(t, 10, 100_000, 300), ToolDigestCases)
	d := r.summarise(t, inv)

	// 09:00: the window has just opened; its lead (5m + 2m) begins at 09:53.
	if n := r.arm(t); n != 0 {
		t.Fatalf("armed %d runs an hour before the close", n)
	}
	r.clock.Set(d.Window.End.Add(-8 * time.Minute))
	if n := r.arm(t); n != 0 {
		t.Fatalf("armed %d runs before the lead began", n)
	}
	r.clock.Set(d.Window.End.Add(-7 * time.Minute))
	if n := r.arm(t); n != 1 {
		t.Fatalf("armed %d runs once the lead began, want 1", n)
	}
	run, err := r.investigations.DigestRun(context.Background(), r.scope, d.PolicyID, d.Window)
	if err != nil || run == nil {
		t.Fatalf("no run recorded for the window: %v", err)
	}
	if run.SubjectKind != domain.SubjectDigest || run.SubjectID != d.PolicyID || run.DigestWindow != d.Window ||
		run.Status != domain.StatusQueued || !strings.Contains(run.RequestedBy.Label, "payments digest") {
		t.Fatalf("the window's run = %+v", run)
	}
	if len(r.queue.jobs) != 1 {
		t.Fatalf("%d jobs for one run", len(r.queue.jobs))
	}
	if args, ok := r.queue.jobs[0].(jobs.InvestigationsRunArgs); !ok || args.InvestigationID != run.ID {
		t.Fatalf("enqueued %#v", r.queue.jobs[0])
	}

	// Every later tick inside the lead finds it armed.
	r.clock.Advance(time.Minute)
	if n := r.arm(t); n != 0 || len(r.queue.jobs) != 1 {
		t.Fatalf("a second tick armed %d more (jobs %d)", n, len(r.queue.jobs))
	}
}

// TestASwitchedOffInvestigatorIsNotArmedForADigest — as for an Incident: an
// Investigator that is off is not subscribed, so nothing is recorded per window.
func TestASwitchedOffInvestigatorIsNotArmedForADigest(t *testing.T) {
	r := newRig(t)
	inv := r.digestInvestigator(t, "dormant", false, digestBudgets(t, 10, 100_000, 300), ToolDigestCases)
	d := r.summarise(t, inv)
	r.clock.Set(d.Window.End.Add(-time.Minute))
	if n := r.arm(t); n != 0 || len(r.investigations.rows) != 0 {
		t.Fatalf("armed %d runs (%d rows) for a switched-off Investigator", n, len(r.investigations.rows))
	}
}

// TestADigestCarriesOnlyAnEndedRunsFinding — the binding rule's investigator half: at
// the send, a queued or running run is the built-in body, and a completed one is carried.
// The Finding is neither published as an Enrichment nor declared: the digest reads it.
func TestADigestCarriesOnlyAnEndedRunsFinding(t *testing.T) {
	r := newRig(t)
	inv := r.digestInvestigator(t, "digest", true, digestBudgets(t, 10, 100_000, 300), ToolDigestCases)
	d := r.summarise(t, inv)
	r.clock.Set(d.Window.End.Add(-5 * time.Minute))
	r.arm(t)
	run, _ := r.investigations.DigestRun(context.Background(), r.scope, d.PolicyID, d.Window)

	if _, ok := r.carried(t, d); ok {
		t.Fatal("a QUEUED run's window carries a Finding; the digest must send the built-in body")
	}

	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, call("c1", ToolDigestCases, `{}`)),
		modelfake.Text("One crash-looping pod in payments, all hour.", 400, 30),
	}
	got, steps := r.run(t, run.ID)
	if got.Status != domain.StatusCompleted || got.Finding != "One crash-looping pod in payments, all hour." {
		t.Fatalf("the run ended %+v", got)
	}
	if kinds(steps) != "model_turn tool_call:oto_digest_cases:ok model_turn" {
		t.Fatalf("transcript = %s", kinds(steps))
	}
	if req := r.dial.model().Requests()[0]; !strings.Contains(req.Messages[len(req.Messages)-1].Content, "KubePodCrashLooping") {
		t.Fatal("the run was not told the window's Cases")
	}
	carried, ok := r.carried(t, d)
	if !ok || carried.ID != run.ID || carried.Finding != got.Finding || carried.Partial() {
		t.Fatalf("a completed run's Finding is not carried: %+v, %v", carried, ok)
	}
	if len(r.findings.published) != 0 || len(r.declarer.declared) != 0 {
		t.Fatalf("a digest window's Finding was published (%d) or declared (%d)",
			len(r.findings.published), len(r.declarer.declared))
	}
}

// TestAnExhaustedDigestRunIsCarriedAsPartial — a budget's partial Finding is kept (ADR
// 0053 §6), so the digest carries it, marked partial.
func TestAnExhaustedDigestRunIsCarriedAsPartial(t *testing.T) {
	r := newRig(t)
	inv := r.digestInvestigator(t, "digest", true, digestBudgets(t, 20, 1000, 300), ToolDigestCases)
	d := r.summarise(t, inv)
	r.clock.Set(d.Window.End.Add(-5 * time.Minute))
	r.arm(t)
	run, _ := r.investigations.DigestRun(context.Background(), r.scope, d.PolicyID, d.Window)
	r.dial.script = []modelfake.Step{{
		Text:      "So far: payments is crash-looping.",
		ToolCalls: []domain.ToolCall{call("c1", ToolDigestCases, `{}`)},
		Usage:     domain.Usage{InputTokens: 900, OutputTokens: 150},
	}}
	if got, _ := r.run(t, run.ID); got.Status != domain.StatusExhausted {
		t.Fatalf("ended %+v", got.Ending)
	}
	carried, ok := r.carried(t, d)
	if !ok || !carried.Partial() || carried.Finding != "So far: payments is crash-looping." {
		t.Fatalf("an exhausted run's partial Finding = %+v, %v", carried, ok)
	}
}

// TestAFailedOrSkippedDigestRunIsTheBuiltInBody — neither will ever have a Finding, and
// neither is waited for.
func TestAFailedOrSkippedDigestRunIsTheBuiltInBody(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		r := newRig(t)
		inv := r.digestInvestigator(t, "digest", true, digestBudgets(t, 10, 100_000, 300), ToolDigestCases)
		d := r.summarise(t, inv)
		r.clock.Set(d.Window.End.Add(-5 * time.Minute))
		r.arm(t)
		run, _ := r.investigations.DigestRun(context.Background(), r.scope, d.PolicyID, d.Window)
		r.dial.script = []modelfake.Step{modelfake.WithoutUsage("a summary nobody can budget")}
		if got, _ := r.run(t, run.ID); got.Status != domain.StatusFailed {
			t.Fatalf("ended %+v", got.Ending)
		}
		if _, ok := r.carried(t, d); ok {
			t.Fatal("a failed run's window carries a Finding")
		}
	})
	t.Run("skipped", func(t *testing.T) {
		r := newRig(t)
		inv := r.digestInvestigator(t, "digest", true, digestBudgets(t, 10, 100_000, 300), ToolDigestCases)
		d := r.summarise(t, inv)
		r.orgControls.on = false
		r.clock.Set(d.Window.End.Add(-5 * time.Minute))
		r.arm(t)
		run, _ := r.investigations.DigestRun(context.Background(), r.scope, d.PolicyID, d.Window)
		if run == nil || run.Status != domain.StatusSkipped || run.Ending.Reason != domain.ReasonDisabled {
			t.Fatalf("the org's switch was not recorded on the window's run: %+v", run)
		}
		if len(r.queue.jobs) != 0 {
			t.Fatal("a skipped run was enqueued")
		}
		if _, ok := r.carried(t, d); ok {
			t.Fatal("a skipped run's window carries a Finding")
		}
		// And the window is not re-armed: the skip is the record.
		r.orgControls.on = true
		if n := r.arm(t); n != 0 {
			t.Fatalf("a skipped window was armed again (%d)", n)
		}
	})
}

// TestADigestRunIsNotOfferedACaseOrMembershipTool — the subject is a window, so the
// Tools that read one Case or propose a membership are refused with the reason.
func TestADigestRunIsNotOfferedACaseOrMembershipTool(t *testing.T) {
	r := newRig(t)
	inv := r.digestInvestigator(t, "digest", true, digestBudgets(t, 10, 100_000, 300),
		ToolDigestCases, ToolCaseTimeline, ToolSuggestMembership)
	d := r.summarise(t, inv)
	r.clock.Set(d.Window.End.Add(-5 * time.Minute))
	r.arm(t)
	run, _ := r.investigations.DigestRun(context.Background(), r.scope, d.PolicyID, d.Window)
	r.dial.script = []modelfake.Step{
		modelfake.Calls(100, 10, call("c1", ToolCaseTimeline, `{}`),
			call("c2", ToolSuggestMembership, `{"incident_number":1,"why":"x"}`)),
		modelfake.Text("Quiet hour.", 100, 10),
	}
	_, steps := r.run(t, run.ID)
	if kinds(steps) != "model_turn tool_call:oto_case_timeline:refused tool_call:oto_suggest_membership:refused model_turn" {
		t.Fatalf("transcript = %s", kinds(steps))
	}
	if !strings.Contains(steps[1].Result, "a digest window") {
		t.Fatalf("the refusal does not say why: %q", steps[1].Result)
	}
}
