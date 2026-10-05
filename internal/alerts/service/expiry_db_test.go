package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/test/harness"
)

// These are the DB tests for ADR 0056's two expiries: `silent` (§3) and
// `source_removed` (§2). Unlike the sweep tests above them, the source resolver
// here is the REAL one — the cluster join the production reaper walks — over real
// `alert_sources` rows, because both expiries are questions about which sources
// feed a Case's cluster and a fake would answer them by fiat.
//
// ⚠️ EVERY CASE IS OPENED BY WEBHOOK WITH NO `endsAt`, which is the shape §3 exists
// for: Alertmanager zeroes `endsAt` on a firing webhook, so these Cases carry no
// `source_ends_at` and are never `timeout` candidates. Anything that ends them is
// one of the two new passes.

// expiryService is the reaper over the real resolver, with a health port that
// vouches for exactly `healthy`.
func (f *fixture) expiryService(healthy ...uuid.UUID) *Service {
	f.t.Helper()
	result := make(map[uuid.UUID]bool, len(healthy))
	for _, src := range healthy {
		result[src] = true
	}
	return f.sweepService(repository.NewCaseRepository(f.pool), &fakeHealth{result: result})
}

// openWebhookCase opens the fixture's alert by webhook, with no upstream end time.
func (f *fixture) openWebhookCase() domain.Case {
	f.t.Helper()
	c := f.openFiring(f.clk.Now().Add(-time.Hour), time.Time{})
	require.True(f.t, c.SourceEndsAt().IsZero(),
		"the case under test must carry no source_ends_at, or `timeout` could end it")
	return c
}

// removeSource soft-deletes a source the way `SourceRepository.SoftDelete` does.
func (f *fixture) removeSource(sourceID uuid.UUID) {
	f.t.Helper()
	f.h.Exec(`UPDATE alert_sources
	             SET deleted_at = $3, push_enabled = false, updated_at = GREATEST(updated_at, $3)
	           WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`,
		f.orgID, sourceID, f.clk.Now())
}

// resolveReasonOf reads the row's own `resolve_reason`, "" while open.
func (f *fixture) resolveReasonOf(caseID uuid.UUID) string {
	f.t.Helper()
	var reason *string
	require.NoError(f.t, f.pool.QueryRow(f.t.Context(),
		`SELECT resolve_reason FROM alert_cases WHERE org_id = $1 AND id = $2`,
		f.orgID, caseID).Scan(&reason))
	if reason == nil {
		return ""
	}
	return *reason
}

// expiredEventOf reads the `case.expired` the reaper appended for one Case.
func (f *fixture) expiredEventOf(caseID uuid.UUID) (summary, reason string) {
	f.t.Helper()
	require.NoError(f.t, f.pool.QueryRow(f.t.Context(),
		`SELECT summary, coalesce(payload->>'resolve_reason', '')
		   FROM alert_events WHERE org_id = $1 AND case_id = $2 AND type = 'case.expired'`,
		f.orgID, caseID).Scan(&summary, &reason))
	return summary, reason
}

// ------------------------------------------------------------------- silent

// TestReapExpiresACaseItsHealthySourceWentSilentAbout — ADR 0056 §3. A Case
// known only from webhooks, under a healthy source, ends as `silent` once the
// source has said nothing about it for longer than its `max_silence_s` (the DDL
// default, a day) — and not one tick before.
func TestReapExpiresACaseItsHealthySourceWentSilentAbout(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	c := f.openWebhookCase()
	svc := f.expiryService(src.ID)

	f.clk.Advance(23 * time.Hour)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired, "a day has not passed since the source last spoke about it")
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))

	f.clk.Advance(2 * time.Hour)
	res, err = svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Expired)
	assert.Equal(t, 1, res.Silent)
	assert.Zero(t, res.SourceRemoved)
	assert.Zero(t, res.Held)

	assert.Equal(t, "silent", f.resolveReasonOf(c.ID()))
	assert.Equal(t, domain.StateExpired.String(), f.alertStateOf(c.ID()),
		"silent reads as expired: it is NOT a resolution")
	summary, reason := f.expiredEventOf(c.ID())
	assert.Equal(t, "silent", reason, "the timeline says which expiry it was")
	assert.Equal(t, "Case expired: its source went silent about it", summary)
}

// TestReapHoldsASilentCaseUnderASourceThatIsNotHealthy — §B.4, unchanged by ADR
// 0056: under an unhealthy source oto cannot tell silence from an outage, so a
// Case silent for two days is held exactly where it is.
func TestReapHoldsASilentCaseUnderASourceThatIsNotHealthy(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	c := f.openWebhookCase()
	svc := f.expiryService() // vouches for nothing

	f.clk.Advance(48 * time.Hour)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired)
	assert.Equal(t, 1, res.Held)
	assert.Equal(t, []uuid.UUID{src.ID}, res.HeldSources)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))
}

// TestReapLeavesASilentCaseOpenWhenItsSourceTurnedMaxSilenceOff — a NULL
// `max_silence_s` is "off", and the scan never even considers the Case.
func TestReapLeavesASilentCaseOpenWhenItsSourceTurnedMaxSilenceOff(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	f.h.Exec(`UPDATE alert_sources SET max_silence_s = NULL WHERE id = $1`, src.ID)
	c := f.openWebhookCase()
	svc := f.expiryService(src.ID)

	f.clk.Advance(30 * 24 * time.Hour)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Considered)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))
}

// TestReapHonoursEachSourcesOwnMaxSilence — the threshold is the source's, not a
// constant: raised to two days, a day and a bit of silence ends nothing.
func TestReapHonoursEachSourcesOwnMaxSilence(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	f.h.Exec(`UPDATE alert_sources SET max_silence_s = 172800 WHERE id = $1`, src.ID)
	c := f.openWebhookCase()
	svc := f.expiryService(src.ID)

	f.clk.Advance(25 * time.Hour)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))

	f.clk.Advance(24 * time.Hour)
	res, err = svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Silent)
	assert.Equal(t, "silent", f.resolveReasonOf(c.ID()))
}

// ----------------------------------------------------------- source_removed

// TestReapExpiresTheCasesOfADeletedSourceAsSourceRemoved — ADR 0056 §2. The only
// source feeding the cluster is deleted, so nothing is left that could say the
// Case ended. It expires on the next tick, with no health to ask — there is no
// source to be healthy — and with no wait for any silence.
func TestReapExpiresTheCasesOfADeletedSourceAsSourceRemoved(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	c := f.openWebhookCase()
	f.removeSource(src.ID)

	// No health port at all: `source_removed` must not depend on one.
	svc := f.sweepService(repository.NewCaseRepository(f.pool), nil)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Expired)
	assert.Equal(t, 1, res.SourceRemoved)
	assert.Zero(t, res.Held,
		"a removed source's Case is not HELD: §B.4 protects a source oto cannot see, and there is none")

	assert.Equal(t, "source_removed", f.resolveReasonOf(c.ID()))
	assert.Equal(t, domain.StateExpired.String(), f.alertStateOf(c.ID()))
	summary, reason := f.expiredEventOf(c.ID())
	assert.Equal(t, "source_removed", reason)
	assert.Equal(t, "Case expired: its source was removed", summary)
}

// ⭐⭐ TestReapKeepsACaseOpenWhileAnotherLiveSourceFeedsItsCluster — the HA pair.
// Alertmanager replicas are several sources on one cluster, and deleting one
// leaves the other able to say when every Case they shared ends. So the Case
// stays open — and only when the LAST live source goes does it expire.
func TestReapKeepsACaseOpenWhileAnotherLiveSourceFeedsItsCluster(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	replicaA := f.h.Source(f.org, f.cluster)
	replicaB := f.h.Source(f.org, f.cluster)
	c := f.openWebhookCase()
	svc := f.expiryService(replicaA.ID, replicaB.ID)

	f.removeSource(replicaA.ID)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired, "replica B still feeds the cluster and can say when this ends")
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))
	assert.Empty(t, f.resolveReasonOf(c.ID()))

	f.removeSource(replicaB.ID)
	res, err = svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SourceRemoved, "with the last live source gone, nothing can")
	assert.Equal(t, "source_removed", f.resolveReasonOf(c.ID()))
}

// TestReapDoesNotNameARemovalNobodyMade — a cluster no source was ever removed
// from is not one whose source was removed. Its Cases are held as they always
// were, rather than expired under a cause that did not happen.
func TestReapDoesNotNameARemovalNobodyMade(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	c := f.openWebhookCase() // the fixture's cluster has no sources at all
	svc := f.sweepService(repository.NewCaseRepository(f.pool), nil)

	f.clk.Advance(48 * time.Hour)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))
}

// TestReapStandsDownWhenASourceIsRegisteredAfterTheScan — the in-transaction
// re-read. A candidate for `source_removed` whose cluster has gained a live
// source by the time the write happens is left open: that source can now say
// when it ends.
func TestReapStandsDownWhenASourceIsRegisteredAfterTheScan(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	c := f.openWebhookCase()
	f.removeSource(src.ID)

	candidates, err := f.cases.SourceRemovedCandidates(ctx, f.scope, 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	f.h.Source(f.org, f.cluster) // registered between the scan and the write
	svc := f.sweepService(repository.NewCaseRepository(f.pool), nil)
	expired, err := svc.expire(ctx, f.scope, candidates[0], f.clk.Now(),
		svc.lifecycleSettings(ctx, f.scope), domain.ResolveSourceRemoved, uuid.Nil)
	require.NoError(t, err)
	assert.False(t, expired, "the scan's verdict must not reach the database")
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))
}

// TestSilentStandsDownUnlessTheRowAndTheClusterStillSayItIsSilent pins
// `unexpirable`'s `silent` arm as a pure function, for `unreapable`'s reason (see
// reap_guard_test.go): through the sweep, domain.Apply's own refusal would make
// the test pass with the guard deleted.
func TestSilentStandsDownUnlessTheRowAndTheClusterStillSayItIsSilent(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	row := reapRow(t, started, started, time.Time{}, time.Time{}) // last heard at `started`
	proven := uuid.MustParse("018f3a4b-0000-7000-8000-0000000003c1")
	day := 24 * time.Hour
	later := started.Add(2 * day)
	alone := domain.CaseSources{Live: 1, SourceID: proven, MaxSilence: day}

	assert.Empty(t, unexpirable(row, later, domain.ResolveSilent, alone, proven))
	assert.Equal(t, "heard about within max_silence_s",
		unexpirable(row, started.Add(day), domain.ResolveSilent, alone, proven))
	assert.Equal(t, "the source turned max_silence_s off",
		unexpirable(row, later, domain.ResolveSilent,
			domain.CaseSources{Live: 1, SourceID: proven}, proven))
	assert.Equal(t, "the source proven healthy no longer speaks for this case alone",
		unexpirable(row, later, domain.ResolveSilent,
			domain.CaseSources{Live: 2, MaxSilence: day}, proven),
		"a replica joined: one source's health no longer vouches for the cluster")
	assert.Equal(t, "the source proven healthy no longer speaks for this case alone",
		unexpirable(row, later, domain.ResolveSilent,
			domain.CaseSources{Live: 1, SourceID: uuid.New(), MaxSilence: day}, proven))

	assert.Equal(t, "a live source feeds the cluster again",
		unexpirable(row, later, domain.ResolveSourceRemoved,
			domain.CaseSources{Live: 1, Removed: 1, SourceID: proven}, uuid.Nil))
	assert.Equal(t, "no source was ever removed from the cluster",
		unexpirable(row, later, domain.ResolveSourceRemoved, domain.CaseSources{}, uuid.Nil))
	assert.Empty(t, unexpirable(row, later, domain.ResolveSourceRemoved,
		domain.CaseSources{Removed: 1}, uuid.Nil))
}
