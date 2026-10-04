package domain_test

// ADR 0053 §6's org-wide controls and the minimum interval, as pure decisions
// (git-bug bf172fe). The service applies them; these say what each one decides.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

func TestTheDailyBudgetResetsAtUTCMidnight(t *testing.T) {
	// 23:30 in Kolkata is 18:00 UTC: the day is UTC's, whatever zone the clock says.
	ist := time.FixedZone("IST", 5*3600+1800)
	at := time.Date(2026, 10, 2, 23, 30, 0, 0, ist)
	if got, want := domain.DayStart(at), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("DayStart = %s, want %s", got, want)
	}
	if got := domain.DayStart(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("midnight itself begins its own day, got %s", got)
	}
}

func TestTheDailyBudgetIsReachedNotProjected(t *testing.T) {
	c := domain.OrgControls{Enabled: true, DailyTokens: 10_000, Concurrency: 2}
	if why := c.BudgetSpent(9_999, t0); why != "" {
		t.Fatalf("a day with budget left refuses: %q", why)
	}
	why := c.BudgetSpent(10_000, t0)
	for _, want := range []string{"10000 of its 10000", "investigation_daily_tokens", "2026-10-03T00:00:00Z"} {
		if !strings.Contains(why, want) {
			t.Fatalf("detail %q does not say %q", why, want)
		}
	}
	if len(why) > domain.MaxReasonDetail {
		t.Fatal("the detail does not fit the column")
	}
	end := domain.EndedBy(domain.ReasonBudget, why)
	if end.Status != domain.StatusSkipped || end.Reason != domain.ReasonBudget {
		t.Fatalf("a budget ending is %+v, want skipped/budget", end)
	}
	if _, err := domain.RestoreEnding(domain.StatusSkipped, "budget", why); err != nil {
		t.Fatalf("a stored skipped/budget row does not read back: %v", err)
	}
	if _, err := domain.RestoreEnding(domain.StatusFailed, "budget", why); err == nil {
		t.Fatal("budget was admitted as a failure reason")
	}
}

func TestConcurrencyIsACountOfRunning(t *testing.T) {
	c := domain.OrgControls{Concurrency: 2}
	if c.AtCapacity(1) || !c.AtCapacity(2) || !c.AtCapacity(3) {
		t.Fatal("AtCapacity is not running >= concurrency")
	}
}

func TestTheMinimumIntervalIsBounded(t *testing.T) {
	for _, ok := range []int{0, 1, 600, 86400} {
		if d, err := domain.NewMinInterval(ok); err != nil || d != time.Duration(ok)*time.Second {
			t.Fatalf("%d: %v %v", ok, d, err)
		}
	}
	for _, bad := range []int{-1, 86401} {
		_, err := domain.NewMinInterval(bad)
		if vs := errs.ViolationsOf(err); len(vs) != 1 || vs[0].Field != "min_interval_seconds" {
			t.Fatalf("%d: %v", bad, err)
		}
	}
	if domain.DefaultMinInterval() != 10*time.Minute {
		t.Fatalf("default interval %s", domain.DefaultMinInterval())
	}
}

func run(status domain.Status, requested, started time.Time) *domain.Investigation {
	return &domain.Investigation{ID: uuid.New(), Status: status, RequestedAt: requested, StartedAt: started}
}

// TestMembershipTriggersInsideTheIntervalCoalesceIntoOneRun — §6, as a sequence: a
// draw runs, a burst of changes inside the interval becomes ONE deferred follow-up
// that starts when the interval is up, and every later change in the burst lands on
// it.
func TestMembershipTriggersInsideTheIntervalCoalesceIntoOneRun(t *testing.T) {
	interval := 10 * time.Minute
	first := run(domain.StatusCompleted, t0, t0.Add(time.Minute)) // waited a minute for a slot

	// Two minutes after it began: a new run, not before ten minutes after it began.
	a := domain.Admit(domain.TriggerMembership, interval, domain.SubjectRuns{Last: first}, t0.Add(3*time.Minute))
	if a.Coalesced() || !a.NotBefore.Equal(t0.Add(11*time.Minute)) {
		t.Fatalf("first change = %+v, want a new run not before %s", a, t0.Add(11*time.Minute))
	}

	// The follow-up exists and is queued: every further change is it.
	follow := run(domain.StatusQueued, t0.Add(3*time.Minute), time.Time{})
	for _, at := range []time.Duration{4 * time.Minute, 9 * time.Minute, 10*time.Minute + 59*time.Second} {
		b := domain.Admit(domain.TriggerMembership, interval, domain.SubjectRuns{Queued: follow, Last: first}, t0.Add(at))
		if b.Onto != follow.ID {
			t.Fatalf("change at +%s = %+v, want coalesced into the queued follow-up", at, b)
		}
	}

	// Past the interval with nothing queued: a run, now.
	c := domain.Admit(domain.TriggerMembership, interval, domain.SubjectRuns{Last: first}, t0.Add(11*time.Minute))
	if c.Coalesced() || !c.NotBefore.IsZero() {
		t.Fatalf("change past the interval = %+v, want a run now", c)
	}
}

func TestAnIntervalOfZeroOrAFirstRunNeverWaits(t *testing.T) {
	last := run(domain.StatusCompleted, t0, t0)
	if a := domain.Admit(domain.TriggerMembership, 0, domain.SubjectRuns{Last: last}, t0.Add(time.Second)); a != (domain.Admission{}) {
		t.Fatalf("interval 0 = %+v", a)
	}
	if a := domain.Admit(domain.TriggerMembership, time.Hour, domain.SubjectRuns{}, t0); a != (domain.Admission{}) {
		t.Fatalf("first run = %+v", a)
	}
}

// TestAHumanIsNotHeldToTheInterval — §6 scopes coalescing to membership-change
// triggers; "a human asks" is §4's own trigger and gets a run.
func TestAHumanIsNotHeldToTheInterval(t *testing.T) {
	queued := run(domain.StatusQueued, t0, time.Time{})
	last := run(domain.StatusCompleted, t0, t0)
	a := domain.Admit(domain.TriggerHuman, time.Hour, domain.SubjectRuns{Queued: queued, Last: last}, t0.Add(time.Minute))
	if a != (domain.Admission{}) {
		t.Fatalf("a human's request = %+v, want a new run now", a)
	}
}
