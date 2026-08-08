package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/migrate"
)

// These are the concurrency regression tests for §B.3 — the read-decide-write
// races that let the reaper fabricate a resolution over a live alert. They need a
// REAL POSTGRES: every one of them turns on READ COMMITTED semantics, on row
// locks, and on how many rows an UPDATE's WHERE clause matches after it unblocks.
// A fake would agree with whatever the code does, which is precisely the failure
// mode being tested.
//
// One container is started for the whole package and each test seeds its own org,
// so nothing is shared but the schema.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("oto"),
		tcpostgres.WithUsername("oto"),
		tcpostgres.WithPassword("oto"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres container: %v\n", err)
		os.Exit(1)
	}
	code := run(ctx, container, m)
	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "terminate container: %v\n", err)
	}
	os.Exit(code)
}

func run(ctx context.Context, container *tcpostgres.PostgresContainer, m *testing.M) int {
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		return 1
	}
	if err := migrate.Up(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool: %v\n", err)
		return 1
	}
	defer pool.Close()

	// alert_events is monthly-partitioned; without partitions every timeline
	// append fails and every one of these tests would fail for the wrong reason.
	if _, err := pool.Exec(ctx, `SELECT oto_partitions_manage()`); err != nil {
		fmt.Fprintf(os.Stderr, "partitions: %v\n", err)
		return 1
	}

	testPool = pool
	return m.Run()
}

// ------------------------------------------------------------------- fixtures

// fixture is one isolated tenant with one alert and one open, firing occurrence.
type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	scope db.TenantScope
	clk   *clock.Fake

	svc *Service

	occurrences *repository.OccurrenceRepository

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
	ctx := t.Context()

	orgID, clusterID := id.New(), id.New()
	slug := fmt.Sprintf("t%s", uuid.NewString()[:8])
	if _, err := testPool.Exec(ctx,
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		orgID, slug, "test org"); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO clusters (id, org_id, cluster_key, display_name) VALUES ($1, $2, $3, $4)`,
		clusterID, orgID, "prod", "prod"); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}

	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	clusterKey, err := domain.NewClusterKey("prod")
	if err != nil {
		t.Fatalf("cluster key: %v", err)
	}
	labels, err := domain.NewLabelSet(map[string]string{
		"alertname": "HighErrorRate",
		"severity":  "critical",
		"service":   "checkout",
	})
	if err != nil {
		t.Fatalf("labels: %v", err)
	}

	clk := clock.NewFake(now)
	f := &fixture{
		t:           t,
		pool:        testPool,
		scope:       scope,
		clk:         clk,
		occurrences: repository.NewOccurrenceRepository(testPool),
		orgID:       orgID,
		clusterID:   clusterID,
		clusterKey:  clusterKey,
		alertKey:    domain.ComputeAlertKey(orgID, clusterKey, labels, nil),
		labels:      labels,
	}
	f.svc = f.newService(clk)
	return f
}

// newService builds an independent service instance over the SAME pool. The race
// tests need two, because two callers racing one row is the whole subject.
func (f *fixture) newService(clk clock.Clock) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:      repository.NewAlertRepository(f.pool, clk),
		Occurrences: repository.NewOccurrenceRepository(f.pool),
		Events:      repository.NewEventRepository(f.pool, clk),
		Snoozes:     repository.NewSnoozeRepository(f.pool, clk),
		Tx:          repository.NewTxRunner(f.pool),
		AlertBatch:  repository.NewAlertRepository(f.pool, clk),
		OccBatch:    repository.NewOccurrenceRepository(f.pool),
		Clock:       clk,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
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

// openFiring drives the real ingest path once, so the occurrence under test was
// created exactly the way production creates one.
func (f *fixture) openFiring(startsAt, endsAt time.Time) domain.Occurrence {
	f.t.Helper()
	obs := f.observation(domain.ObservedByIngest, "firing", f.clk.Now(), startsAt, endsAt)
	if _, err := f.svc.ObserveBatch(f.t.Context(), f.scope, []domain.Observation{obs},
		ObserveOptions{}); err != nil {
		f.t.Fatalf("open firing occurrence: %v", err)
	}
	return f.currentOccurrence()
}

// currentOccurrence re-reads the one occurrence of this fixture's alert.
func (f *fixture) currentOccurrence() domain.Occurrence {
	f.t.Helper()
	ctx := f.t.Context()

	var alertID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		f.orgID, f.alertKey.String()).Scan(&alertID); err != nil {
		f.t.Fatalf("read alert: %v", err)
	}
	occ, ok, err := f.occurrences.GetLatestByAlert(ctx, f.scope, alertID)
	if err != nil || !ok {
		f.t.Fatalf("read occurrence: ok=%v err=%v", ok, err)
	}
	return occ
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
