package service

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ THE BATCH REASON IS THE ONE §H.6 FACT NO TRANSITION CAN PRODUCE, AND ITS
// FAN-OUT CHANGED SHAPE ENTIRELY (git-bug `7570090`).
//
// `repeat interval elapsed` is Alertmanager telling oto the same thing again:
// nothing transitioned, so no per-alert edge fires, and `ObserveOptions` carries
// the reason in from the composition root instead. What changed is the
// CARDINALITY. It was `GroupReason`, one batch was one generation, one generation
// was one conversation, and the batch produced ONE notification. A conversation
// is a Case now, so the same fact produces one notification PER OPEN CASE.
//
// ⛔ NOTHING TESTED THIS AFTER THE RENAME. `batchReasonFor` in `internal/app`
// carries a ⛔⛔ note saying the mapping was nearly deleted in the group strip
// without a single compile error, because "`ObserveOptions.GroupReason` simply
// stopped being set". The mapping now has a caller again — and this is the test
// that says what the caller is owed, on the seam where the fan-out actually
// happens.

// recordingEnqueuer captures the job requests the service hands the queue. What
// is under test is WHICH evaluations a batch mints, and a real River queue would
// answer that only by being drained.
type recordingEnqueuer struct {
	mu   sync.Mutex
	reqs []db.JobRequest
}

func (e *recordingEnqueuer) Enqueue(
	_ context.Context, args db.JobArgs, _ ...db.JobOption,
) (db.EnqueueResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = append(e.reqs, db.JobRequest{Args: args})
	return db.EnqueueResult{Kind: args.Kind()}, nil
}

func (e *recordingEnqueuer) EnqueueMany(
	_ context.Context, reqs []db.JobRequest,
) ([]db.EnqueueResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = append(e.reqs, reqs...)
	out := make([]db.EnqueueResult, len(reqs))
	for i, r := range reqs {
		out[i] = db.EnqueueResult{Kind: r.Args.Kind()}
	}
	return out, nil
}

// notifyEvaluations returns the `notify.evaluate` args the queue was handed, in
// enqueue order, filtered to one reason.
func (e *recordingEnqueuer) notifyEvaluations(reason string) []jobs.NotifyEvaluateArgs {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]jobs.NotifyEvaluateArgs, 0, len(e.reqs))
	for _, r := range e.reqs {
		args, ok := r.Args.(jobs.NotifyEvaluateArgs)
		if ok && args.Reason == reason {
			out = append(out, args)
		}
	}
	return out
}

// queuedService is the fixture's service with the job queue wired, which
// newService deliberately leaves nil.
func (f *fixture) queuedService(enq db.Enqueuer) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:     repository.NewAlertRepository(f.pool, f.clk, false),
		Cases:      repository.NewCaseRepository(f.pool),
		Events:     repository.NewEventRepository(f.pool, f.clk),
		Snoozes:    repository.NewSnoozeRepository(f.pool, f.clk),
		Tx:         repository.NewTxRunner(f.pool),
		AlertBatch: repository.NewAlertRepository(f.pool, f.clk, false),
		OccBatch:   repository.NewCaseRepository(f.pool),
		Enqueuer:   enq,
		Clock:      f.clk,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		f.t.Fatalf("build service: %v", err)
	}
	return svc
}

// TestABatchReasonFansOutToEveryOpenCaseAndNoOther is the cardinality claim.
//
// Three alerts fire and a fourth resolves, then the SAME four are observed again
// with `BatchReason` set and nothing changing. The repeat must reach the three
// live conversations and must not reach the dead one: a repeat is a root UPDATE
// of a card that is still on screen, and there is no card to refresh for an alert
// that is not firing.
//
// ⚠️ THE COUNT IS NOT THE ASSERTION — THE MEMBERSHIP IS. "Three" would be
// satisfied by any three cases, including three that all belong to the first
// alert, which is precisely the old one-per-generation shape wearing a new
// number. What is pinned is the SET of case ids, so a fan-out that reached the
// wrong episodes fails even when it reaches the right number of them.
func TestABatchReasonFansOutToEveryOpenCaseAndNoOther(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	enq := &recordingEnqueuer{}
	svc := f.queuedService(enq)
	ctx := t.Context()

	// Four alerts. The fixture's own labels are the first; three more differ only
	// by alertname, which is enough to make them four distinct Alert identities.
	names := []string{"", "BatchReasonB", "BatchReasonC", "BatchReasonD"}
	firing := make([]domain.Observation, 0, len(names))
	for _, name := range names {
		if name == "" {
			firing = append(firing, f.observation(domain.ObservedByIngest, "firing",
				now, now.Add(-time.Minute), time.Time{}))
			continue
		}
		kv := harness.DefaultLabels()
		kv["alertname"] = name
		firing = append(firing, f.observationFor(harness.Labels(t, kv), "firing",
			now, now.Add(-time.Minute), time.Time{}))
	}

	opened, err := svc.ObserveBatch(ctx, f.scope, firing, ObserveOptions{})
	require.NoError(t, err)
	require.Len(t, opened.Outcomes, len(names))

	// The fourth resolves, so exactly one of the four episodes is closed when the
	// repeat arrives. A closed episode is strictly terminal since ADR 0040, so this
	// is the only way to have a case that is not a conversation.
	f.clk.Advance(time.Minute)
	last := firing[len(firing)-1]
	last.Status = "resolved"
	last.SourceEndsAt = f.clk.Now()
	last.SourceUpdatedAt = f.clk.Now()
	last.ObservedAt = f.clk.Now()
	closed, err := svc.ObserveBatch(ctx, f.scope, []domain.Observation{last}, ObserveOptions{})
	require.NoError(t, err)
	require.Len(t, closed.Outcomes, 1)

	wantLive := map[uuid.UUID]bool{
		opened.Outcomes[0].CaseID: true,
		opened.Outcomes[1].CaseID: true,
		opened.Outcomes[2].CaseID: true,
	}
	deadCase := opened.Outcomes[3].CaseID

	// Alertmanager repeats the whole notification. Every alert is observed exactly
	// as it already stands, so no edge fires and the ONLY intents this batch can
	// mint are the fan-out's.
	f.clk.Advance(time.Minute)
	repeat := make([]domain.Observation, 0, len(firing))
	for _, o := range firing[:3] {
		o.SourceUpdatedAt = f.clk.Now()
		o.ObservedAt = f.clk.Now()
		repeat = append(repeat, o)
	}
	repeat = append(repeat, last)

	// ⛔ THE LITERAL IS DELIBERATE AND THERE IS NO CONSTANT TO USE. `alerts` has a
	// `reason*` constant for every Reason a TRANSITION produces and none for this
	// one, because this one is not derived here: mapping Alertmanager's
	// `notification_reason` onto oto's Reason enum is `notification`'s job (§H.6),
	// and `alerts` is told the answer rather than learning a second copy of the
	// table. `internal/app.batchReasonFor` is what spells it, from
	// `notifdomain.ReasonRepeat`.
	const repeatReason = "repeat"

	before := len(enq.notifyEvaluations(repeatReason))
	require.Zero(t, before, "nothing before this point may mint a repeat")

	_, err = svc.ObserveBatch(ctx, f.scope, repeat, ObserveOptions{BatchReason: repeatReason})
	require.NoError(t, err)

	got := enq.notifyEvaluations(repeatReason)

	// ⚠️ THE ENQUEUE ORDER IS NOT ASSERTED HERE, AND THAT IS A GAP THIS TEST IS
	// DECLARING RATHER THAN ACCEPTING. `lifecycle.go`'s fan-out ranges over
	// `acc.latest`, which is a Go map, so the three evaluations reach the queue in
	// a randomised order — measured at 2 runs in 10 departing from batch order.
	// Every sibling producer in the same accumulator is deterministic on purpose
	// (`projectionOrder` exists for exactly this, and `frames` is documented as
	// preserving the per-item publish order), so this one is the odd one out.
	// It is reported as a production defect rather than pinned here, because a
	// test that fails on two runs in ten is not a test.
	seen := map[uuid.UUID]int{}
	for _, args := range got {
		seen[args.CaseID]++
		assert.Nil(t, args.AlertID,
			"a repeat is about the CONVERSATION and names no focus alert: §H.6 makes "+
				"AlertID mandatory only for the alert-scoped reasons, and "+
				"notifications_focus_ck is what refuses a stray one")
	}

	for id := range wantLive {
		assert.Equal(t, 1, seen[id],
			"every OPEN case is one conversation and gets exactly one repeat; "+
				"case %s got %d", id, seen[id])
	}
	assert.Zero(t, seen[deadCase],
		"a resolved alert has no card on screen to refresh, so its closed episode "+
			"must not be reached — this is the assertion that fails if the fan-out "+
			"stops reading `ended_at`")
	assert.Len(t, seen, len(wantLive),
		"and NOTHING ELSE: the fan-out's whole population is the batch's open cases")
}
