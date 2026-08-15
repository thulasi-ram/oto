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
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// These are the sweep tests: the §B.4 batch health guard of `occurrence.reap`
// and the steady-state write elision of `flap.score`. They run against the same
// real Postgres as the race tests above them, because the properties under test
// end in rows — what expired, what was held, and above all what was NOT written.

// ------------------------------------------------------------------ reap fakes

// fakeOccSources maps occurrences onto sources from a fixed table, standing in
// for the group-membership join the production resolver walks. An occurrence
// absent from the table is absent from the answer, which the sweep must read as
// "cannot prove healthy".
type fakeOccSources struct {
	bySource map[uuid.UUID]uuid.UUID
}

func (f fakeOccSources) SourceIDs(
	_ context.Context, _ db.TenantScope, occurrenceIDs []uuid.UUID,
) (map[uuid.UUID]uuid.UUID, error) {
	out := make(map[uuid.UUID]uuid.UUID, len(occurrenceIDs))
	for _, occID := range occurrenceIDs {
		if src, ok := f.bySource[occID]; ok {
			out[occID] = src
		}
	}
	return out, nil
}

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
// sweeps use: the reaper's resolver and health guard, and the flap scorer's
// event counter. The nil-port degradations are covered by using nil here too.
func (f *fixture) sweepService(occSources OccurrenceSourceResolver, health SourceHealth) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:      repository.NewAlertRepository(f.pool, f.clk),
		Occurrences: repository.NewOccurrenceRepository(f.pool),
		Events:      repository.NewEventRepository(f.pool, f.clk),
		Snoozes:     repository.NewSnoozeRepository(f.pool, f.clk),
		Tx:          repository.NewTxRunner(f.pool),
		AlertBatch:  repository.NewAlertRepository(f.pool, f.clk),
		OccBatch:    repository.NewOccurrenceRepository(f.pool),
		OccSources:  occSources,
		Health:      health,
		EventCounts: repository.NewEventRepository(f.pool, f.clk),
		Clock:       f.clk,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		f.t.Fatalf("build sweep service: %v", err)
	}
	return svc
}

// openSecondFiring opens an occurrence on a SECOND alert in the same tenant, so
// a reap tick has two candidates to consider.
func (f *fixture) openSecondFiring(startsAt, endsAt time.Time) domain.Occurrence {
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
		f.t.Fatalf("open second firing occurrence: %v", err)
	}
	occ, err := f.occurrences.GetByID(f.t.Context(), f.scope, f.occurrenceIDOf(obs.AlertKey))
	if err != nil {
		f.t.Fatalf("read second occurrence: %v", err)
	}
	return occ
}

// occurrenceIDOf resolves an alert key to its current occurrence id.
func (f *fixture) occurrenceIDOf(key domain.AlertKey) uuid.UUID {
	f.t.Helper()
	var occID uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT current_occurrence_id FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		f.orgID, key.String()).Scan(&occID); err != nil {
		f.t.Fatalf("resolve occurrence of %s: %v", key, err)
	}
	return occID
}

// stateOf reads one occurrence's state letter straight from the row.
func (f *fixture) stateOf(occID uuid.UUID) string {
	f.t.Helper()
	var state string
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT state FROM alert_occurrences WHERE org_id = $1 AND id = $2`,
		f.orgID, occID).Scan(&state); err != nil {
		f.t.Fatalf("read occurrence state: %v", err)
	}
	return state
}

// -------------------------------------------------------------------- reaping

// TestReapAsksForHealthOncePerTickNotPerOccurrence pins the round-trip shape of
// the §B.4 guard: the guard is per SOURCE, so a tick with two candidates over
// one source asks the health port exactly once, with exactly that source.
func TestReapAsksForHealthOncePerTickNotPerOccurrence(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()

	startsAt := now.Add(-2 * time.Hour)
	occ1 := f.openFiring(startsAt, now.Add(-30*time.Minute))
	occ2 := f.openSecondFiring(startsAt, now.Add(-30*time.Minute))

	srcID := id.New()
	health := &fakeHealth{result: map[uuid.UUID]bool{srcID: true}}
	svc := f.sweepService(fakeOccSources{bySource: map[uuid.UUID]uuid.UUID{
		occ1.ID(): srcID,
		occ2.ID(): srcID,
	}}, health)

	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Considered)
	assert.Equal(t, 2, res.Expired, "a healthy source's candidates must expire")
	assert.Zero(t, res.Held)

	require.Equal(t, 1, health.calls,
		"health is resolved ONCE per tick: per-occurrence lookups made a source outage cost "+
			"two queries per candidate per minute, worst exactly when the source was down")
	require.Equal(t, []uuid.UUID{srcID}, health.asked[0],
		"two candidates over one source must ask about that one source, deduped")

	assert.Equal(t, domain.StateExpired.String(), f.stateOf(occ1.ID()))
	assert.Equal(t, domain.StateExpired.String(), f.stateOf(occ2.ID()))
}

// TestReapHoldsWhatTheBatchCannotVouchFor is §B.4 over the batch result: a
// source ABSENT from the returned map is not proven healthy, and every
// occurrence it owns is held in place — absence and false are the same verdict.
func TestReapHoldsWhatTheBatchCannotVouchFor(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()

	startsAt := now.Add(-2 * time.Hour)
	occ1 := f.openFiring(startsAt, now.Add(-30*time.Minute))
	occ2 := f.openSecondFiring(startsAt, now.Add(-30*time.Minute))

	srcHealthy, srcUnknown := id.New(), id.New()
	health := &fakeHealth{result: map[uuid.UUID]bool{srcHealthy: true}}
	svc := f.sweepService(fakeOccSources{bySource: map[uuid.UUID]uuid.UUID{
		occ1.ID(): srcHealthy,
		occ2.ID(): srcUnknown, // resolved, but the health batch says nothing about it
	}}, health)

	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Considered)
	assert.Equal(t, 1, res.Expired)
	assert.Equal(t, 1, res.Held,
		"a source the batch did not return is a source oto cannot prove healthy")
	assert.Equal(t, []uuid.UUID{srcUnknown}, res.HeldSources,
		"the held source is named, so one source.unreachable banner can be raised for it")

	assert.Equal(t, domain.StateExpired.String(), f.stateOf(occ1.ID()))
	assert.Equal(t, domain.StateFiring.String(), f.stateOf(occ2.ID()),
		"the held occurrence must be left exactly as it was")
}

// TestReapHoldsEveryCandidateWhenTheHealthLookupFails: a failed batch lookup is
// "oto does not know" for the WHOLE tick. The sweep neither aborts nor expires —
// it holds everything and reports the holds, exactly as the per-candidate
// version did when a single lookup failed.
func TestReapHoldsEveryCandidateWhenTheHealthLookupFails(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()

	startsAt := now.Add(-2 * time.Hour)
	occ1 := f.openFiring(startsAt, now.Add(-30*time.Minute))
	occ2 := f.openSecondFiring(startsAt, now.Add(-30*time.Minute))

	srcA, srcB := id.New(), id.New()
	health := &fakeHealth{err: errors.New("sources service unreachable")}
	svc := f.sweepService(fakeOccSources{bySource: map[uuid.UUID]uuid.UUID{
		occ1.ID(): srcA,
		occ2.ID(): srcB,
	}}, health)

	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err, "not knowing holds candidates; it must never abort the sweep")
	assert.Equal(t, 2, res.Considered)
	assert.Zero(t, res.Expired)
	assert.Equal(t, 2, res.Held)
	assert.ElementsMatch(t, []uuid.UUID{srcA, srcB}, res.HeldSources)

	assert.Equal(t, domain.StateFiring.String(), f.stateOf(occ1.ID()))
	assert.Equal(t, domain.StateFiring.String(), f.stateOf(occ2.ID()))
}

// ---------------------------------------------------------------- flap scoring

// alertRowVersion reads the alert row's xmin, which moves on every UPDATE and
// only on an UPDATE. It is the direct witness of "nothing wrote", which no
// value column can be: an unconditional UPDATE writes the same values back and
// leaves every readable column looking untouched.
func (f *fixture) alertRowVersion() string {
	f.t.Helper()
	var v string
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT xmin::text FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		f.orgID, f.alertKey.String()).Scan(&v); err != nil {
		f.t.Fatalf("read alert row version: %v", err)
	}
	return v
}

// flapScore reads the projected score straight from the row.
func (f *fixture) flapScore() float32 {
	f.t.Helper()
	var v float32
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT flap_score FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		f.orgID, f.alertKey.String()).Scan(&v); err != nil {
		f.t.Fatalf("read flap score: %v", err)
	}
	return v
}

// TestScoreFlapsSkipsTheSteadyStateWrite is the WAL-churn guard: a tick that
// recomputes the same score for the same alert writes NOTHING, and a tick that
// computes a different one still writes. `flap_score` is REAL, so the skip's
// float32 comparison is exact round-trip equality, which is what makes "same
// score" decidable at all.
func TestScoreFlapsSkipsTheSteadyStateWrite(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()

	// One opened transition, recorded at Epoch. The scorer's window is
	// [now-2h, now) with an EXCLUSIVE upper bound, so the clock must move past
	// the append before the event is countable.
	f.openFiring(now.Add(-2*time.Hour), now.Add(30*time.Minute))
	f.clk.Advance(time.Minute)

	svc := f.sweepService(nil, nil)

	// Tick 1: the score moves off its default, so the row is written.
	res1, err := svc.ScoreFlaps(ctx, f.scope, 10)
	require.NoError(t, err)
	require.Equal(t, 1, res1.Scored)
	score1 := f.flapScore()
	require.Greater(t, score1, float32(0), "the first tick must have written a real score")
	version1 := f.alertRowVersion()

	// Tick 2: same window, same count, same flag — the steady state.
	res2, err := svc.ScoreFlaps(ctx, f.scope, 10)
	require.NoError(t, err)
	require.Equal(t, 1, res2.Scored, "a skipped write is still a scored alert")
	require.Equal(t, version1, f.alertRowVersion(),
		"the steady state must not touch the row: an UPDATE that changes neither column is "+
			"pure WAL churn on the hottest table in the system")
	require.Equal(t, score1, f.flapScore())

	// The occurrence resolves, adding a second transition to the window: the
	// score CHANGES, so the elision must not suppress this write.
	resolved := f.observation(domain.ObservedByIngest, "resolved",
		f.clk.Now(), now.Add(-2*time.Hour), f.clk.Now())
	_, err = f.svc.ObserveBatch(ctx, f.scope, []domain.Observation{resolved}, ObserveOptions{})
	require.NoError(t, err)
	f.clk.Advance(time.Minute)

	res3, err := svc.ScoreFlaps(ctx, f.scope, 10)
	require.NoError(t, err)
	require.Equal(t, 1, res3.Scored)
	require.NotEqual(t, version1, f.alertRowVersion(), "a changed score must still be written")
	require.Greater(t, f.flapScore(), score1)
}
