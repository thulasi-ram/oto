package service

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/test/harness"
)

// These are the DB tests for ADR 0056 §1, "staleness is shown": the two reads a
// screen uses to say whether a Case can expire and what a source is holding. They
// run over real `alert_sources` rows for the expiry tests' reason — both are
// questions about which sources feed a cluster, and a fake answers by fiat.

// coverService is the service with the real cover reader and a health port that
// vouches for exactly `healthy`.
func (f *fixture) coverService(healthy ...uuid.UUID) *Service {
	f.t.Helper()
	result := make(map[uuid.UUID]bool, len(healthy))
	for _, src := range healthy {
		result[src] = true
	}
	cases := repository.NewCaseRepository(f.pool)
	svc, err := New(Deps{
		Alerts:     repository.NewAlertRepository(f.pool, f.clk, false),
		Cases:      cases,
		Events:     repository.NewEventRepository(f.pool, f.clk),
		Snoozes:    repository.NewSnoozeRepository(f.pool, f.clk),
		Tx:         repository.NewTxRunner(f.pool),
		AlertBatch: repository.NewAlertRepository(f.pool, f.clk, false),
		OccBatch:   cases,
		OccSources: cases,
		CaseCover:  cases,
		Health:     &fakeHealth{result: result},
		Clock:      f.clk,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	require.NoError(f.t, err)
	return svc
}

// TestCaseCoverSaysWhoCanStillSpeakForACase walks one Case through the shapes its
// screen distinguishes: one healthy source with a max silence, the same source not
// proven healthy, an HA pair (owner ruling R1: it may expire only while BOTH are
// healthy, on the longer threshold, and not at all while either turned it off),
// and a cluster whose sources were all removed.
func TestCaseCoverSaysWhoCanStillSpeakForACase(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	c := f.openWebhookCase()

	cover, err := f.coverService(src.ID).CaseCover(ctx, f.scope, []uuid.UUID{c.ID()})
	require.NoError(t, err)
	got := cover[c.ID()]
	assert.Equal(t, 1, got.Live)
	assert.Zero(t, got.Removed)
	assert.Equal(t, []uuid.UUID{src.ID}, got.LiveIDs)
	require.Len(t, got.Sources, 1)
	assert.Equal(t, src.ID, got.Sources[0].ID)
	assert.Equal(t, src.Name, got.Sources[0].Name)
	assert.True(t, got.Sources[0].Healthy)
	assert.Equal(t, 24*time.Hour, got.MaxSilence, "a source registered after 00094 defaults to a day")
	assert.True(t, got.AllHealthy)

	cover, err = f.coverService().CaseCover(ctx, f.scope, []uuid.UUID{c.ID()})
	require.NoError(t, err)
	assert.False(t, cover[c.ID()].AllHealthy,
		"a source the guard cannot vouch for reads as not healthy, as the reaper reads it")
	assert.False(t, cover[c.ID()].Sources[0].Healthy)

	second := f.h.Source(f.org, f.cluster)
	f.h.Exec(`UPDATE alert_sources SET max_silence_s = 172800 WHERE id = $1`, second.ID)
	cover, err = f.coverService(src.ID, second.ID).CaseCover(ctx, f.scope, []uuid.UUID{c.ID()})
	require.NoError(t, err)
	got = cover[c.ID()]
	assert.Equal(t, 2, got.Live)
	assert.ElementsMatch(t, []uuid.UUID{src.ID, second.ID}, got.LiveIDs)
	assert.Len(t, got.Sources, 2)
	assert.True(t, got.AllHealthy, "an HA pair whose replicas are both healthy may expire")
	assert.Equal(t, 48*time.Hour, got.MaxSilence, "the cluster waits for its slowest replica")

	cover, err = f.coverService(src.ID).CaseCover(ctx, f.scope, []uuid.UUID{c.ID()})
	require.NoError(t, err)
	assert.False(t, cover[c.ID()].AllHealthy, "one replica the guard cannot vouch for holds the pair")

	f.h.Exec(`UPDATE alert_sources SET max_silence_s = NULL WHERE id = $1`, second.ID)
	cover, err = f.coverService(src.ID, second.ID).CaseCover(ctx, f.scope, []uuid.UUID{c.ID()})
	require.NoError(t, err)
	assert.Zero(t, cover[c.ID()].MaxSilence,
		"one replica with the expiry off turns it off for the whole cluster")

	f.removeSource(src.ID)
	f.removeSource(second.ID)
	cover, err = f.coverService().CaseCover(ctx, f.scope, []uuid.UUID{c.ID()})
	require.NoError(t, err)
	got = cover[c.ID()]
	assert.Zero(t, got.Live)
	assert.Equal(t, 2, got.Removed)
	assert.Empty(t, got.Sources)
	assert.False(t, got.AllHealthy, "no live source is nobody to vouch for the Case")
	assert.Equal(t, f.clk.Now().UTC(), got.LastRemovedAt.UTC())
}

// TestCaseCoverOmitsACaseItCannotSee — a Case id this org does not hold is
// absent, never a zeroed row that would read as "no live source".
func TestCaseCoverOmitsACaseItCannotSee(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	f.h.Source(f.org, f.cluster)

	cover, err := f.coverService().CaseCover(t.Context(), f.scope, []uuid.UUID{uuid.New()})
	require.NoError(t, err)
	assert.Empty(t, cover)
}

// TestOpenCasesBySourceCountsWhatTheReaperHolds — the reaper's `held` number, per
// source. Under a healthy single source nothing is held; under one the guard
// cannot vouch for, every open Case on the cluster is. In an HA pair (owner ruling
// R1) a healthy replica holds nothing — its unhealthy sibling's row is the one
// that says the pair's Cases are held.
func TestOpenCasesBySourceCountsWhatTheReaperHolds(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	f.openWebhookCase()

	counts, err := f.coverService(src.ID).OpenCasesBySource(ctx, f.scope, []uuid.UUID{src.ID})
	require.NoError(t, err)
	assert.Equal(t, SourceCaseCount{Open: 1, Held: 0}, counts[src.ID])

	counts, err = f.coverService().OpenCasesBySource(ctx, f.scope, []uuid.UUID{src.ID})
	require.NoError(t, err)
	assert.Equal(t, SourceCaseCount{Open: 1, Held: 1}, counts[src.ID])

	second := f.h.Source(f.org, f.cluster)
	counts, err = f.coverService(src.ID, second.ID).OpenCasesBySource(ctx, f.scope,
		[]uuid.UUID{src.ID, second.ID})
	require.NoError(t, err)
	assert.Equal(t, SourceCaseCount{Open: 1, Held: 0}, counts[src.ID],
		"an HA pair that is all healthy holds nothing")
	assert.Equal(t, SourceCaseCount{Open: 1, Held: 0}, counts[second.ID])

	counts, err = f.coverService(src.ID).OpenCasesBySource(ctx, f.scope,
		[]uuid.UUID{src.ID, second.ID})
	require.NoError(t, err)
	assert.Equal(t, SourceCaseCount{Open: 1, Held: 0}, counts[src.ID],
		"the healthy replica is not why anything is held")
	assert.Equal(t, SourceCaseCount{Open: 1, Held: 1}, counts[second.ID],
		"the replica the guard cannot vouch for holds the pair's Cases")

	f.removeSource(second.ID)
	counts, err = f.coverService(src.ID).OpenCasesBySource(ctx, f.scope,
		[]uuid.UUID{src.ID, second.ID})
	require.NoError(t, err)
	assert.Equal(t, SourceCaseCount{Open: 1, Held: 0}, counts[src.ID])
	_, counted := counts[second.ID]
	assert.False(t, counted, "a removed source is not counted, and is not reported as holding zero")
}
