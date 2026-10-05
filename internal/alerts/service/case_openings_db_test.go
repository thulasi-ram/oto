package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// ADR 0052 §2 (git-bug 61eeddf): A CASE OPENING IS TOLD TO WHOEVER LISTENS, IN THE
// TRANSACTION THAT OPENED IT — CaseEndings' twin.
//
// The listener in production enqueues `incidents.correlate` and nothing else; this
// module knows only the port. What is pinned here is the half this module owns:
// that a real open, through the real ingest path, reaches the port with the Case's
// id; that the `fired` notification is still enqueued beside it, so a Correlator is
// never a condition of a notification; and that an observation opening nothing
// reports nothing.

type openedCases struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (o *openedCases) CasesOpened(_ context.Context, _ db.TenantScope, caseIDs []uuid.UUID) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ids = append(o.ids, caseIDs...)
	return nil
}

func (f *fixture) openingService(listener CaseOpenings, enq *recordingEnqueuer) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:       repository.NewAlertRepository(f.pool, f.clk, false),
		Cases:        repository.NewCaseRepository(f.pool),
		Events:       repository.NewEventRepository(f.pool, f.clk),
		Snoozes:      repository.NewSnoozeRepository(f.pool, f.clk),
		Tx:           repository.NewTxRunner(f.pool),
		AlertBatch:   repository.NewAlertRepository(f.pool, f.clk, false),
		OccBatch:     repository.NewCaseRepository(f.pool),
		Enqueuer:     enq,
		CaseOpenings: listener,
		Clock:        f.clk,
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		f.t.Fatalf("build service: %v", err)
	}
	return svc
}

func TestACaseOpeningIsToldToTheOpeningsPortBesideItsNotification(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	listener := &openedCases{}
	enq := &recordingEnqueuer{}
	f.svc = f.openingService(listener, enq)
	ctx := t.Context()

	opened := f.openFiring(now.Add(-time.Minute), time.Time{})
	if len(listener.ids) != 1 || listener.ids[0] != opened.ID() {
		t.Fatalf("the openings port saw %v, want exactly the opened Case %s", listener.ids, opened.ID())
	}
	if len(enq.notifyEvaluations("fired")) != 1 {
		t.Fatalf("opening a Case must still enqueue its own notification; a Correlator is never a condition of one")
	}

	// A repeat firing of the same episode opens nothing.
	f.clk.Advance(time.Minute)
	if _, err := f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{
		f.observation(domain.ObservedByIngest, "firing", f.clk.Now(), now.Add(-time.Minute), time.Time{}),
	}, ObserveOptions{}); err != nil {
		t.Fatalf("repeat firing: %v", err)
	}
	if len(listener.ids) != 1 {
		t.Fatalf("a repeat observation reported an opening: %v", listener.ids)
	}
}

// failingOpenings is a port whose enqueue fails, which is the one way the opening
// path can fail on its account.
type failingOpenings struct{}

func (failingOpenings) CasesOpened(context.Context, db.TenantScope, []uuid.UUID) error {
	return errors.New("the outbox refused the job")
}

// ⭐ THE PORT RIDES THE OUTBOX, SO ITS FAILURE IS AN OUTBOX FAILURE: the batch rolls
// back whole and Alertmanager's retry re-delivers it — the Case is not opened
// without its correlate job, exactly as it is not opened without its `enrich.run`.
// What the port may never do is evaluate, which is why the production adapter only
// enqueues; a Correlator's own failure happens later, in its own job.
func TestAFailedOpeningsEnqueueRollsTheBatchBackLikeAnyEnqueue(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	f.svc = f.openingService(failingOpenings{}, &recordingEnqueuer{})

	_, err := f.svc.ObserveBatch(t.Context(), f.scope, []domain.Observation{
		f.observation(domain.ObservedByIngest, "firing", now, now.Add(-time.Minute), time.Time{}),
	}, ObserveOptions{})
	if err == nil {
		t.Fatalf("a failed enqueue must fail the batch so it is retried whole")
	}
}
