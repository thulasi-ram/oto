package service

import (
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/grouping/domain"
)

// goneQuiet rebuilds the harness's generation with `last_activity_at` pushed far
// enough into the past that `group_close_delay` has certainly elapsed.
//
// ⛔ WITHOUT THIS BOTH TESTS BELOW PASS FOR THE WRONG REASON. The harness stamps
// `last_activity_at` at the fake clock's own instant, so the generation is not
// idle at all and `CanCloseAt` refuses on the DELAY — never reaching the live-case
// clause the tests exist to pin. The first version of this file asserted Held==1
// against a generation that was merely young.
func goneQuiet(t *testing.T, h *joinHarness) domain.Group {
	t.Helper()
	g := h.groups.group
	quiet, err := domain.NewGroup(domain.GroupParams{
		ID:           g.ID(),
		OrgID:        g.OrgID(),
		SourceID:     g.SourceID(),
		ClusterID:    g.ClusterID(),
		ClusterKey:   g.ClusterKey(),
		Key:          g.Key(),
		Generation:   g.Generation(),
		Receiver:     g.Receiver(),
		GroupLabels:  g.GroupLabels(),
		Title:        g.Title(),
		State:        g.State(),
		StateVersion: g.StateVersion(),
		// Both move: the generation was born before it went quiet, and
		// `last_activity_at >= first_seen_at` is enforced.
		FirstSeenAt:    h.at.Add(-48 * time.Hour),
		LastActivityAt: h.at.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	return quiet
}

// ⛔ THE PROPERTY ADR 0039 RESTS ON.
//
// git-bug `fe73f9a` asked that group close become "no live case with this split
// key" instead of `last_activity_at` plus `group_close_delay`. ADR 0039 declines:
// taken literally the clause deletes the delay, so an alert that resolves and
// re-fires inside it finds its generation gone and opens a NEW Slack thread that
// cannot be re-parented.
//
// The decline is only defensible because the safety goal the clause states is
// already held elsewhere — `CanCloseAt` refuses while `Counts.Live() > 0`, over a
// rollup re-derived INSIDE `CloseIdle`'s own transaction. Migration 00051 is what
// made that refusal true rather than nominal: the counts now derive from
// `alert_cases.ended_at`, which the state machine writes on every T5/T6, instead
// of from a membership row that could lag.
//
// That argument was asserted in an ADR and nowhere else. This is the assertion.
func TestAnIdleGenerationWithALiveCaseIsHeldOpen(t *testing.T) {
	t.Parallel()

	h := newJoinHarness(t)
	// Idle enough to be a candidate, and still firing. The whole point is that
	// those two facts are not in tension: `last_activity_at` says nothing was
	// WRITTEN recently, not that nothing is FIRING.
	quiet := goneQuiet(t, h)
	h.groups.group = quiet
	h.groups.candidates = []domain.Group{quiet}
	h.members.total = 1

	res, err := h.svc.CloseIdle(h.ctx, h.scope, 10)
	if err != nil {
		t.Fatalf("CloseIdle: %v", err)
	}

	if res.Considered != 1 {
		t.Fatalf("the sweep considered %d generations, want 1 — the fixture is not reaching "+
			"the code under test", res.Considered)
	}
	if res.Held != 1 {
		t.Errorf("Held is %d, want 1 — an idle generation with a LIVE case was not held open. "+
			"This is the property ADR 0039 cites to justify leaving `CloseIdle` "+
			"activity-driven; if it stops holding, the ADR's argument is void and the "+
			"generation's Slack thread is closed over a firing alert", res.Held)
	}
	if h.groups.closes != 0 {
		t.Errorf("%d generations were closed, want 0 — the refusal has to stop the write, not "+
			"merely be counted alongside it", h.groups.closes)
	}
}

// TestAnIdleGenerationWithNoLiveCaseIsClosed is the teeth-check for the test
// above: a refusal that never lets anything through would satisfy it for free.
func TestAnIdleGenerationWithNoLiveCaseIsClosed(t *testing.T) {
	t.Parallel()

	h := newJoinHarness(t)
	quiet := goneQuiet(t, h)
	h.groups.group = quiet
	h.groups.candidates = []domain.Group{quiet}
	h.members.total = 0

	res, err := h.svc.CloseIdle(h.ctx, h.scope, 10)
	if err != nil {
		t.Fatalf("CloseIdle: %v", err)
	}
	if res.Held != 0 {
		t.Errorf("Held is %d, want 0 — a generation with no live case is what the sweep "+
			"EXISTS to close, and holding it would leak a thread per flap", res.Held)
	}
	if h.groups.closes != 1 {
		t.Errorf("%d generations were closed, want 1", h.groups.closes)
	}
}
