package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// These are the sweep tests: the §B.4 batch health guard of `case.reap`. They run
// against the same real Postgres as the race tests above them, because the
// properties under test end in rows — what expired and what was held.

// ------------------------------------------------------------------ reap fakes

// fakeHealth records HOW the sweep asks, so a test can pin the shape of the
// asking — once per tick, with the distinct sources — and not just the verdict.
type fakeHealth struct {
	result map[uuid.UUID]bool
	err    error
	calls  int
	asked  [][]uuid.UUID
}

func (f *fakeHealth) HealthyFor(
	_ context.Context, _ db.TenantScope, sourceIDs []uuid.UUID,
) (map[uuid.UUID]bool, error) {
	f.calls++
	f.asked = append(f.asked, append([]uuid.UUID(nil), sourceIDs...))
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// sweepService builds a service over the fixture's pool with the ports the
// sweeps use: the reaper's resolver and health guard, and the ADR 0056 expiries
// turned ON. The nil-port degradations are covered by using nil here too.
func (f *fixture) sweepService(occSources CaseSourceResolver, health SourceHealth) *Service {
	f.t.Helper()
	return f.sweepServiceWith(occSources, health, true)
}

// sweepServiceWith is sweepService with `jobs.expire_silent_and_removed` stated.
func (f *fixture) sweepServiceWith(
	occSources CaseSourceResolver, health SourceHealth, expireUnheard bool,
) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:     repository.NewAlertRepository(f.pool, f.clk, false),
		Cases:      repository.NewCaseRepository(f.pool),
		Events:     repository.NewEventRepository(f.pool, f.clk),
		Snoozes:    repository.NewSnoozeRepository(f.pool, f.clk),
		Tx:         repository.NewTxRunner(f.pool),
		AlertBatch: repository.NewAlertRepository(f.pool, f.clk, false),
		OccBatch:   repository.NewCaseRepository(f.pool),
		OccSources: occSources,
		Health:     health,
		Clock:      f.clk,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),

		ExpireSilentAndRemoved: expireUnheard,
	})
	if err != nil {
		f.t.Fatalf("build sweep service: %v", err)
	}
	return svc
}

// markHealth writes `status` into `source_health` for each source — the table the
// candidate scans' §B.4 pre-filter reads. A source with no row reads as not
// healthy, exactly as it does to the guard. `degraded` is the unhealthy status
// that needs no `last_error`.
func (f *fixture) markHealth(status string, sourceIDs ...uuid.UUID) {
	f.t.Helper()
	for _, src := range sourceIDs {
		f.h.Exec(`INSERT INTO source_health (source_id, org_id, status, updated_at)
		          VALUES ($1, $2, $3, $4)
		          ON CONFLICT (source_id) DO UPDATE
		            SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at`,
			src, f.orgID, status, f.clk.Now())
	}
}

// healthySource registers a live source on the fixture's cluster whose
// `source_health` row says healthy, so the scans' pre-filter lets its Cases
// through to the guard.
func (f *fixture) healthySource() harness.Source {
	f.t.Helper()
	src := f.h.Source(f.org, f.cluster)
	f.markHealth("healthy", src.ID)
	return src
}

// openSecondFiring opens a case on a SECOND alert in the same tenant, so
// a reap tick has two candidates to consider.
func (f *fixture) openSecondFiring(startsAt, endsAt time.Time) domain.Case {
	f.t.Helper()
	labels := harness.Labels(f.t, map[string]string{
		"alertname": "SecondAlert",
		"severity":  "warning",
		"service":   "checkout",
	})
	obs := domain.Observation{
		Source:            domain.ObservedByIngest,
		ClusterID:         f.clusterID,
		ClusterKey:        f.clusterKey,
		AlertKey:          harness.AlertKey(f.orgID, f.clusterKey, labels),
		SourceFingerprint: domain.ComputeSourceFingerprint(labels),
		Labels:            labels,
		Annotations:       map[string]string{},
		Status:            "firing",
		SourceStartsAt:    startsAt,
		SourceEndsAt:      endsAt,
		SourceUpdatedAt:   f.clk.Now(),
		ObservedAt:        f.clk.Now(),
	}
	if _, err := f.svc.ObserveBatch(f.t.Context(), f.scope, []domain.Observation{obs},
		ObserveOptions{}); err != nil {
		f.t.Fatalf("open second firing case: %v", err)
	}
	ac, err := f.cases.GetByID(f.t.Context(), f.scope, f.caseIDOf(obs.AlertKey))
	if err != nil {
		f.t.Fatalf("read second case: %v", err)
	}
	return ac
}

// caseIDOf resolves an alert key to its current case id.
func (f *fixture) caseIDOf(key domain.AlertKey) uuid.UUID {
	f.t.Helper()
	var caseID uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT current_case_id FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		f.orgID, key.String()).Scan(&caseID); err != nil {
		f.t.Fatalf("resolve case of %s: %v", key, err)
	}
	return caseID
}

// alertStateOf reads the §B.2 state of the ALERT as one case last observed it,
// straight from the row.
//
// ⭐ IT DERIVES RATHER THAN READS, AND THAT IS ADR 0040. `alert_cases.state` holds
// `open | closed` and nothing else now; which of the four §B.2 values the episode
// stands for is that column together with `suppression_reason` (WHICH silence
// muted it) and `resolve_reason` (WHY it ended). What these tests are about is
// unchanged — the reaper must leave a held case FIRING and must stamp an expired
// one EXPIRED — and the expired half is now two columns rather than one, so a
// reaper that closed an episode without saying why would be caught here as well.
//
// ⛔ THE DERIVATION IS SPELLED OUT IN SQL rather than delegated to
// `domain.Case.AlertState`. These assertions are about what the sweep WROTE to the
// row; reading it back through the very code that computes the reading would let a
// wrong pair of columns agree with itself.
func (f *fixture) alertStateOf(caseID uuid.UUID) string {
	f.t.Helper()
	var state string
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT CASE
		          WHEN state = 'open' AND suppression_reason IS NOT NULL THEN 'suppressed'
		          WHEN state = 'open'                                    THEN 'firing'
		          WHEN resolve_reason = 'upstream'                       THEN 'resolved'
		          ELSE 'expired'
		        END
		   FROM alert_cases WHERE org_id = $1 AND id = $2`,
		f.orgID, caseID).Scan(&state); err != nil {
		f.t.Fatalf("read case state: %v", err)
	}
	return state
}

// -------------------------------------------------------------------- reaping

// TestReapAsksForHealthOncePerTickNotPerCase pins the round-trip shape of
// the §B.4 guard: the guard is per SOURCE, so a tick with two candidates over
// one source asks the health port exactly once, with exactly that source.
func TestReapAsksForHealthOncePerTickNotPerCase(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()

	src := f.healthySource()
	startsAt := now.Add(-2 * time.Hour)
	occ1 := f.openFiring(startsAt, now.Add(-30*time.Minute))
	occ2 := f.openSecondFiring(startsAt, now.Add(-30*time.Minute))

	health := &fakeHealth{result: map[uuid.UUID]bool{src.ID: true}}
	svc := f.sweepService(repository.NewCaseRepository(f.pool), health)

	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Considered)
	assert.Equal(t, 2, res.Expired, "a healthy source's candidates must expire")
	assert.Zero(t, res.Held)

	require.Equal(t, 1, health.calls,
		"health is resolved ONCE per tick: per-case lookups made a source outage cost "+
			"two queries per candidate per minute, worst exactly when the source was down")
	require.Equal(t, []uuid.UUID{src.ID}, health.asked[0],
		"two candidates over one source must ask about that one source, deduped")

	assert.Equal(t, domain.StateExpired.String(), f.alertStateOf(occ1.ID()))
	assert.Equal(t, domain.StateExpired.String(), f.alertStateOf(occ2.ID()))
}

// TestReapHoldsWhatTheBatchCannotVouchFor is §B.4 over the batch result, on an
// HA pair (owner ruling R1): a source ABSENT from the returned map is not proven
// healthy, and because the guard asks EVERY live source on the cluster, one
// replica it cannot vouch for holds every Case the pair shares — absence and false
// are the same verdict.
//
// ⭐ AND ONLY THAT REPLICA IS NAMED. The healthy one is not why anything is held,
// and naming it would raise a `source.unreachable` banner over a source that is
// fine.
func TestReapHoldsWhatTheBatchCannotVouchFor(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()

	// Both rows say healthy, so both Cases reach the guard; the port is what
	// disagrees about one of them.
	srcHealthy, srcUnknown := f.healthySource(), f.healthySource()
	startsAt := now.Add(-2 * time.Hour)
	occ1 := f.openFiring(startsAt, now.Add(-30*time.Minute))
	occ2 := f.openSecondFiring(startsAt, now.Add(-30*time.Minute))

	health := &fakeHealth{result: map[uuid.UUID]bool{srcHealthy.ID: true}}
	svc := f.sweepService(repository.NewCaseRepository(f.pool), health)

	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Considered)
	assert.Zero(t, res.Expired, "one replica oto cannot vouch for holds every Case the pair shares")
	assert.Equal(t, 2, res.Held)
	assert.Equal(t, []uuid.UUID{srcUnknown.ID}, res.HeldSources,
		"only the source the batch did not vouch for is named, so one source.unreachable "+
			"banner is raised for it and none for its healthy sibling")

	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(occ1.ID()),
		"the held case must be left exactly as it was")
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(occ2.ID()))
}

// TestReapHoldsEveryCandidateWhenTheHealthLookupFails: a failed batch lookup is
// "oto does not know" for the WHOLE tick. The sweep neither aborts nor expires —
// it holds everything and reports the holds, exactly as the per-candidate
// version did when a single lookup failed.
func TestReapHoldsEveryCandidateWhenTheHealthLookupFails(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()

	srcA, srcB := f.healthySource(), f.healthySource()
	startsAt := now.Add(-2 * time.Hour)
	occ1 := f.openFiring(startsAt, now.Add(-30*time.Minute))
	occ2 := f.openSecondFiring(startsAt, now.Add(-30*time.Minute))

	health := &fakeHealth{err: errors.New("sources service unreachable")}
	svc := f.sweepService(repository.NewCaseRepository(f.pool), health)

	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err, "not knowing holds candidates; it must never abort the sweep")
	assert.Equal(t, 2, res.Considered)
	assert.Zero(t, res.Expired)
	assert.Equal(t, 2, res.Held)
	assert.ElementsMatch(t, []uuid.UUID{srcA.ID, srcB.ID}, res.HeldSources)

	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(occ1.ID()))
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(occ2.ID()))
}

// ------------------------------------------------- flap scoring (RETIRED)

// ⛔ THE FLAP TESTS ARE GONE BECAUSE `ScoreFlaps` IS. The WAL-churn guard here
// asserted that a tick recomputing the same score wrote NOTHING and a tick
// computing a different one still wrote — a property of a detector oto no longer
// has. The case retention window W (migration 00057) damps a flap at CASE
// FORMATION, which left the score counting lifecycle events a damped flap does not
// append: `is_flapping` read false exactly when the alert was flapping. The two
// columns are retired IN PLACE — every read still works, nothing writes them — so
// there is no write to make a claim about. See `sweep.go` and ADR 0041, Amendment 1.
