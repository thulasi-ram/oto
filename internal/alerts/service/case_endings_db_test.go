package service

import (
	"context"
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

// ADR 0052 §3, §5 (git-bug aa6d18b): A CASE ENDING IS TOLD TO WHOEVER LISTENS, IN
// THE TRANSACTION THAT ENDED IT.
//
// The listener is `incidents`, which decides from it whether an Incident just went
// quiet; this module knows only the port. What is pinned here is the half this
// module owns: that a real close — driven through the real ingest path — reaches the
// port with the Case's id, and that an observation which closes nothing does not.

type endedCases struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (e *endedCases) CasesEnded(_ context.Context, _ db.TenantScope, caseIDs []uuid.UUID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, caseIDs...)
	return nil
}

func (f *fixture) endingService(listener CaseEndings) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:      repository.NewAlertRepository(f.pool, f.clk, false),
		Cases:       repository.NewCaseRepository(f.pool),
		Events:      repository.NewEventRepository(f.pool, f.clk),
		Snoozes:     repository.NewSnoozeRepository(f.pool, f.clk),
		Tx:          repository.NewTxRunner(f.pool),
		AlertBatch:  repository.NewAlertRepository(f.pool, f.clk, false),
		OccBatch:    repository.NewCaseRepository(f.pool),
		Enqueuer:    &recordingEnqueuer{},
		CaseEndings: listener,
		Clock:       f.clk,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		f.t.Fatalf("build service: %v", err)
	}
	return svc
}

func TestACaseClosingIsToldToTheEndingsPort(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	listener := &endedCases{}
	f.svc = f.endingService(listener)
	ctx := t.Context()

	opened := f.openFiring(now.Add(-time.Minute), time.Time{})
	if len(listener.ids) != 0 {
		t.Fatalf("opening a Case reported %d ending(s); only a close is an ending", len(listener.ids))
	}

	// A second firing observation of the same episode changes nothing terminal.
	f.clk.Advance(time.Minute)
	if _, err := f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{
		f.observation(domain.ObservedByIngest, "firing", f.clk.Now(), now.Add(-time.Minute), time.Time{}),
	}, ObserveOptions{}); err != nil {
		t.Fatalf("repeat firing: %v", err)
	}
	if len(listener.ids) != 0 {
		t.Fatalf("a repeat observation reported an ending")
	}

	// The upstream resolve closes the episode (W=0: no retention window is wired).
	f.clk.Advance(time.Minute)
	if _, err := f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{
		f.observation(domain.ObservedByIngest, "resolved", f.clk.Now(), now.Add(-time.Minute), f.clk.Now()),
	}, ObserveOptions{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if f.currentCase().State() != domain.CaseClosed {
		t.Fatalf("the resolve did not close the Case; this test is asserting nothing")
	}
	if len(listener.ids) != 1 || listener.ids[0] != opened.ID() {
		t.Fatalf("the endings port saw %v, want exactly the closed Case %s", listener.ids, opened.ID())
	}
}
