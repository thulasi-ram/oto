package service

// git-bug bf172fe's "Done when", against the scripted model and in-memory ports: past
// the daily budget, new Investigations exist as `skipped`/`budget` rows and the budget
// resets at UTC midnight; concurrency excess waits; triggers within the interval produce
// one run. The SQL that holds the same rules — the advisory lock, the day's sum over
// Steps, the widened reason CHECK — is `repository/investigations_db_test.go`'s.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/modelfake"
)

// isSnooze reports whether a job's error is a jobs.Snooze — a wait, not a failure.
func isSnooze(err error) bool { return err != nil && strings.HasPrefix(err.Error(), "snooze ") }

// runToEnd requests and runs one Investigation that spends in+out tokens.
func (r *rig) runToEnd(t *testing.T, inv domain.Investigator, c domain.CaseSubject, in, out int64) domain.Investigation {
	t.Helper()
	r.dial.script = []modelfake.Step{modelfake.Text("Looks like the deploy.", in, out)}
	got, _ := r.run(t, r.request(t, inv, c).ID)
	if got.Status != domain.StatusCompleted {
		t.Fatalf("setup run ended %+v", got.Ending)
	}
	return got
}

// TestPastTheDailyBudgetARequestIsSkippedOnTheRecordAndNeverQueued — §6: "New
// Investigations are recorded as skipped with reason budget, not queued. Resets at UTC
// midnight."
func TestPastTheDailyBudgetARequestIsSkippedOnTheRecordAndNeverQueued(t *testing.T) {
	r := newRig(t)
	r.orgControls.dailyTokens = 1000
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.runToEnd(t, inv, c, 900, 100) // the day's 1000, spent
	jobsBefore := len(r.queue.jobs)
	r.clock.Advance(time.Minute)

	run := r.request(t, inv, c)
	if run.Status != domain.StatusSkipped || run.Ending.Reason != domain.ReasonBudget {
		t.Fatalf("a request past the budget = %+v", run)
	}
	for _, want := range []string{"1000 of its 1000", "investigation_daily_tokens", "2026-10-03T00:00:00Z"} {
		if !strings.Contains(run.Ending.Detail, want) {
			t.Fatalf("detail %q does not say %q", run.Ending.Detail, want)
		}
	}
	if len(r.queue.jobs) != jobsBefore {
		t.Fatal("a budget-skipped request was enqueued")
	}
	if !run.StartedAt.IsZero() || run.EndedAt.IsZero() {
		t.Fatalf("a skipped run started at %v, ended at %v", run.StartedAt, run.EndedAt)
	}
	// It is visible where every run is: the Case's list.
	listed, _, err := r.svc.ListCaseInvestigations(context.Background(), r.scope, c.CaseID, db.Keyset{Limit: 25})
	if err != nil || len(listed) != 2 || listed[0].ID != run.ID {
		t.Fatalf("the skipped run is not the Case's latest: %v %+v", err, listed)
	}

	// UTC midnight: the day's spend is yesterday's, and a request queues again.
	r.clock.Set(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	if next := r.request(t, inv, c); next.Status != domain.StatusQueued {
		t.Fatalf("after midnight UTC the request is %s, want queued", next.Status)
	}
}

// TestAQueuedRunThatFindsTheDaySpentIsSkippedNotRun — read twice, like the kill
// switch: a run queued while the day had budget, whose job begins after it is spent,
// never calls a model.
func TestAQueuedRunThatFindsTheDaySpentIsSkippedNotRun(t *testing.T) {
	r := newRig(t)
	r.orgControls.dailyTokens = 1000
	inv, c := r.setup(t, domain.DefaultBudgets())
	waiting := r.request(t, inv, c)
	r.runToEnd(t, inv, c, 1500, 0) // one run overruns the day; it is bounded by its own budget, not cut off

	got, steps := r.run(t, waiting.ID)
	if got.Status != domain.StatusSkipped || got.Ending.Reason != domain.ReasonBudget || len(steps) != 0 {
		t.Fatalf("run = %+v, steps %s", got, kinds(steps))
	}
	if !got.StartedAt.IsZero() {
		t.Fatal("a budget-skipped run claims to have started")
	}
}

// TestPastTheConcurrencyARunWaitsAndIsNeverDropped — §6: "Waits in the job queue;
// never dropped." The job snoozes (no attempt spent) and the run stays queued, then
// runs once a slot frees.
func TestPastTheConcurrencyARunWaitsAndIsNeverDropped(t *testing.T) {
	r := newRig(t)
	r.orgControls.concurrency = 1
	inv, c := r.setup(t, domain.DefaultBudgets())
	busy := r.request(t, inv, c)
	if got, err := r.investigations.Start(context.Background(), r.scope, busy.ID, r.clock.Now(), 1); got != domain.StartBegan || err != nil {
		t.Fatalf("could not occupy the slot: %v %v", got, err)
	}

	waiting := r.request(t, inv, c)
	if waiting.Status != domain.StatusQueued {
		t.Fatalf("concurrency decides when a run starts, not whether it is asked for: %s", waiting.Status)
	}
	r.dial.script = []modelfake.Step{modelfake.Text("done", 100, 10)}
	for i := 0; i < 3; i++ {
		err := r.svc.RunInvestigation(context.Background(), r.scope, waiting.ID)
		if !isSnooze(err) || !strings.Contains(err.Error(), "investigation_concurrency") {
			t.Fatalf("attempt %d: err = %v, want a concurrency snooze", i, err)
		}
	}
	still, _ := r.svc.GetInvestigation(context.Background(), r.scope, waiting.ID)
	if still.Investigation.Status != domain.StatusQueued || r.dial.model() != nil {
		t.Fatalf("a waiting run is %s, model dialled: %v", still.Investigation.Status, r.dial.model() != nil)
	}

	// The slot frees; the waiting run takes it.
	if err := r.investigations.Finish(context.Background(), r.scope, busy.ID, domain.Completed(), domain.Usage{}, 0, "", r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.run(t, waiting.ID); got.Status != domain.StatusCompleted {
		t.Fatalf("the waiting run ended %+v", got.Ending)
	}
}

// (TestMembershipChangesInsideTheIntervalProduceOneRun moved to incidents_test.go when
// git-bug 74ea849 gave the membership trigger a subject that has members: an
// Incident. It drives the same coalescing through IncidentChanged.)

// TestAHumanAskingIsNotCoalesced — ADR 0053 §6 scopes the interval to
// membership-change triggers; "a human asks" (§4) gets a run, under the kill switch,
// the budget and the concurrency like every other.
func TestAHumanAskingIsNotCoalesced(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	every := time.Hour
	if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{MinInterval: &every}); err != nil {
		t.Fatal(err)
	}
	inv, _ = r.svc.investigators.Get(context.Background(), r.scope, inv.ID)
	a, b := r.request(t, inv, c), r.request(t, inv, c)
	if a.ID == b.ID || !b.NotBefore.IsZero() || len(r.queue.jobs) != 2 || !r.queue.opts[1].ScheduledAt.IsZero() {
		t.Fatalf("two asks = %s, %s (not before %v), %d jobs", a.ID, b.ID, b.NotBefore, len(r.queue.jobs))
	}
}
