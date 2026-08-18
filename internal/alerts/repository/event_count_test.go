package repository_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// The `flap.score` input query. What is under test is the CAP: `limit` used to
// be applied by the service while iterating a Go map, so which alerts got
// scored when it bound was decided by map iteration order — a different random
// subset every tick. The cap now lives in the statement with an ORDER BY, and
// these tests pin what that must mean: the most-changed alerts win, ties break
// on alert_id, and the same call returns the same alerts every time.

type countFixture struct {
	h    *harness.H
	repo *repository.EventRepository
	org  harness.Org
}

func newCountFixture(t *testing.T) countFixture {
	t.Helper()
	h := harness.New(t)
	return countFixture{
		h:    h,
		repo: repository.NewEventRepository(h.Pool, clock.NewFake(h.Now())),
		org:  h.Org(),
	}
}

// transitions appends n `case.opened` events for one alert at one
// instant. `case.opened` is one of the six types the statement counts;
// WHICH of the six does not matter to the cap.
func (f countFixture) transitions(t *testing.T, alertID uuid.UUID, n int, at time.Time) {
	t.Helper()

	actor, err := domain.SystemActor(domain.ActorIngest)
	require.NoError(t, err)
	observed, err := domain.NewObservationTime(at, at)
	require.NoError(t, err)

	events := make([]domain.Event, 0, n)
	for i := 0; i < n; i++ {
		e, err := domain.NewEvent(domain.EventParams{
			ID:      id.New(),
			OrgID:   f.org.ID,
			AlertID: alertID,
			CaseID:  id.New(),
			Type:    domain.EventCaseOpened,
			At:      observed,
			Actor:   actor,
			Summary: "case opened",
		})
		require.NoError(t, err)
		events = append(events, e)
	}
	written, err := f.repo.AppendBatch(f.h.Ctx, f.org.Scope, events)
	require.NoError(t, err)
	require.Equal(t, n, written)
}

// TestStateChangeCountsCapsOnTheMostChangedAlerts is the determinism contract:
// when the cap binds, the alerts that come back are the ones with the most
// transitions — the ones nearest the flap threshold, whose stale score misleads
// the most — and a tie cannot flip between ticks because alert_id breaks it.
func TestStateChangeCountsCapsOnTheMostChangedAlerts(t *testing.T) {
	t.Parallel()
	f := newCountFixture(t)

	at := f.h.Now()
	window := db.TimeWindow{From: at.Add(-time.Hour), To: at.Add(time.Hour)}

	// Two alerts TIED on two transitions each, ordered here so the assertion
	// names which of them must win: the smaller uuid, per the ORDER BY's
	// stable key.
	tieLow, tieHigh := id.New(), id.New()
	if bytes.Compare(tieHigh[:], tieLow[:]) < 0 {
		tieLow, tieHigh = tieHigh, tieLow
	}
	busy, quiet := id.New(), id.New()

	f.transitions(t, busy, 3, at)
	f.transitions(t, tieLow, 2, at)
	f.transitions(t, tieHigh, 2, at)
	f.transitions(t, quiet, 1, at)
	// Outside the window entirely: must count for nobody.
	f.transitions(t, busy, 1, at.Add(-2*time.Hour))

	got, err := f.repo.StateChangeCounts(f.h.Ctx, f.org.Scope, window, 2)
	require.NoError(t, err)
	require.Len(t, got, 2, "the cap is the statement's LIMIT, not a suggestion")
	require.Equal(t, 3, got[busy],
		"the most-changed alert must be in the capped set, and the out-of-window "+
			"event must not have inflated it")
	require.Equal(t, 2, got[tieLow],
		"the tie on 2 transitions must break on alert_id, the stable key — "+
			"two equal counts must not swap places between ticks")

	// The same call again is the SAME set: this is the property the Go-map cap
	// did not have.
	again, err := f.repo.StateChangeCounts(f.h.Ctx, f.org.Scope, window, 2)
	require.NoError(t, err)
	require.Equal(t, got, again)

	// A cap wider than the field returns everyone inside the window.
	all, err := f.repo.StateChangeCounts(f.h.Ctx, f.org.Scope, window, 10)
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]int{busy: 3, tieLow: 2, tieHigh: 2, quiet: 1}, all)
}
