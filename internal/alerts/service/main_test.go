package service

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// These are the concurrency regression tests for §B.3 — the read-decide-write
// races that let the reaper fabricate a resolution over a live alert. They need a
// REAL POSTGRES: every one of them turns on READ COMMITTED semantics, on row
// locks, and on how many rows an UPDATE's WHERE clause matches after it unblocks.
// A fake would agree with whatever the code does, which is precisely the failure
// mode being tested.
//
// The container, the migrations, the partition bootstrap and the between-tests
// reset all live in `test/harness` (ADR 0021 §1): one container for the whole
// binary, one truncated database per test.

func TestMain(m *testing.M) { harness.Main(m) }

// ------------------------------------------------------------------- fixtures

// fixture is one isolated tenant with one alert and one open, firing case.
type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	scope db.TenantScope
	clk   *clock.Fake

	svc *Service

	cases *repository.CaseRepository

	// h, org and cluster are kept so a test can seed the rows a §C.4 group needs
	// without standing up a second harness over the same database.
	h       *harness.H
	org     harness.Org
	cluster harness.Cluster

	orgID      uuid.UUID
	clusterID  uuid.UUID
	clusterKey domain.ClusterKey
	alertKey   domain.AlertKey
	labels     domain.LabelSet
}

// newFixture seeds a fresh org and cluster and builds a real alerts service over
// the real repositories. Nothing is mocked below the service boundary: the
// compare-and-set under test is SQL.
func newFixture(t *testing.T, now time.Time) *fixture {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	labels := harness.Labels(t, harness.DefaultLabels())

	clk := clock.NewFake(now)
	f := &fixture{
		t:          t,
		pool:       h.Pool,
		scope:      org.Scope,
		clk:        clk,
		cases:      repository.NewCaseRepository(h.Pool),
		h:          h,
		org:        org,
		cluster:    cluster,
		orgID:      org.ID,
		clusterID:  cluster.ID,
		clusterKey: cluster.Key,
		alertKey:   harness.AlertKey(org.ID, cluster.Key, labels),
		labels:     labels,
	}
	f.svc = f.newService(clk)
	return f
}

// newService builds an independent service instance over the SAME pool. The race
// tests need two, because two callers racing one row is the whole subject.
func (f *fixture) newService(clk clock.Clock) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:     repository.NewAlertRepository(f.pool, clk, false),
		Cases:      repository.NewCaseRepository(f.pool),
		Events:     repository.NewEventRepository(f.pool, clk),
		Snoozes:    repository.NewSnoozeRepository(f.pool, clk),
		Tx:         repository.NewTxRunner(f.pool),
		AlertBatch: repository.NewAlertRepository(f.pool, clk, false),
		OccBatch:   repository.NewCaseRepository(f.pool),
		Clock:      clk,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		f.t.Fatalf("build service: %v", err)
	}
	return svc
}

// observation builds one normalised Observation for this fixture's alert.
func (f *fixture) observation(
	src domain.ObservationSource, status string, at, startsAt, endsAt time.Time,
) domain.Observation {
	f.t.Helper()
	return domain.Observation{
		Source:            src,
		ClusterID:         f.clusterID,
		ClusterKey:        f.clusterKey,
		AlertKey:          f.alertKey,
		SourceFingerprint: domain.ComputeSourceFingerprint(f.labels),
		Labels:            f.labels,
		Annotations:       map[string]string{},
		Status:            status,
		SourceStartsAt:    startsAt,
		SourceEndsAt:      endsAt,
		SourceUpdatedAt:   at,
		ObservedAt:        at,
	}
}

// openFiring drives the real ingest path once, so the case under test was
// created exactly the way production creates one.
func (f *fixture) openFiring(startsAt, endsAt time.Time) domain.Case {
	f.t.Helper()
	obs := f.observation(domain.ObservedByIngest, "firing", f.clk.Now(), startsAt, endsAt)
	if _, err := f.svc.ObserveBatch(f.t.Context(), f.scope, []domain.Observation{obs},
		ObserveOptions{}); err != nil {
		f.t.Fatalf("open firing case: %v", err)
	}
	return f.currentCase()
}

// currentCase re-reads the one case of this fixture's alert.
func (f *fixture) currentCase() domain.Case {
	f.t.Helper()
	ctx := f.t.Context()

	var alertID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		f.orgID, f.alertKey.String()).Scan(&alertID); err != nil {
		f.t.Fatalf("read alert: %v", err)
	}
	ac, ok, err := f.cases.GetLatestByAlert(ctx, f.scope, alertID)
	if err != nil || !ok {
		f.t.Fatalf("read case: ok=%v err=%v", ok, err)
	}
	return ac
}

// countEvents counts appended timeline entries of one type for this org.
func (f *fixture) countEvents(eventType string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM alert_events WHERE org_id = $1 AND type = $2`,
		f.orgID, eventType).Scan(&n); err != nil {
		f.t.Fatalf("count events: %v", err)
	}
	return n
}

// ------------------------------------------------------------ race scheduling

// waitForBlockedWriter blocks until at least one backend is waiting on a lock, so
// a test can prove the second writer really has reached its UPDATE and parked on
// the first writer's row lock.
//
// This is what makes these tests deterministic rather than sleep-and-hope: the
// bug only manifests when the loser READ before the winner COMMITTED, and that
// ordering is exactly "the loser is blocked while the winner is still open".
func waitForBlockedWriter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no backend ever blocked on a row lock; the race was not staged")
}

// holdOpen runs fn inside a transaction, signals that its writes are done but
// UNCOMMITTED, and waits for release before committing. It is how a test pins the
// window in which a competing reader still sees the old row.
func holdOpen(
	t *testing.T, pool *pgxpool.Pool, fn func(ctx context.Context) error,
) (staged chan struct{}, release chan struct{}, done chan error) {
	t.Helper()
	staged, release, done = make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- db.Tx(context.Background(), pool, func(ctx context.Context) error {
			if err := fn(ctx); err != nil {
				return err
			}
			close(staged)
			<-release
			return nil
		})
	}()
	return staged, release, done
}
