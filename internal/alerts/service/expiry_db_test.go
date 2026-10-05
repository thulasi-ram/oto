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
// vouches for exactly `healthy` — and `source_health` rows that say the same, so
// the scans' §B.4 pre-filter and the guard agree. A test about the two
// disagreeing writes the rows itself and builds the service with portOnly.
func (f *fixture) expiryService(healthy ...uuid.UUID) *Service {
	f.t.Helper()
	f.markHealth("healthy", healthy...)
	return f.portOnly(healthy...)
}

// portOnly is expiryService without touching `source_health`.
func (f *fixture) portOnly(healthy ...uuid.UUID) *Service {
	f.t.Helper()
	result := make(map[uuid.UUID]bool, len(healthy))
	for _, src := range healthy {
		result[src] = true
	}
	return f.sweepService(repository.NewCaseRepository(f.pool), &fakeHealth{result: result})
}

// pastGrace moves the clock past the resolve grace, the wait `source_removed`
// keeps after a cluster's last removal.
func (f *fixture) pastGrace() {
	f.t.Helper()
	f.clk.Advance(domain.DefaultResolveGrace + time.Minute)
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
// Case silent for two days is held exactly where it is. Both halves of the guard
// are pinned: the scan's pre-filter over `source_health` never offers the Case,
// and when the row and the port disagree the port's "no" is the verdict.
func TestReapHoldsASilentCaseUnderASourceThatIsNotHealthy(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	c := f.openWebhookCase()
	f.markHealth("degraded", src.ID)

	f.clk.Advance(48 * time.Hour)
	res, err := f.portOnly(src.ID).Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Considered, "a degraded source's Cases are not even candidates")
	assert.Zero(t, res.Expired)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))

	f.markHealth("healthy", src.ID)
	res, err = f.portOnly().Reap(ctx, f.scope, 10) // the port vouches for nothing
	require.NoError(t, err)
	assert.Zero(t, res.Expired, "the pre-filter is never the verdict: the guard's no holds")
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
// Case ended. It expires on the first tick a resolve grace after the deletion
// (B5: a source deleted and re-created in between ends nothing), with no health to
// ask — there is no source to be healthy — and with no wait for any silence.
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
	assert.Zero(t, res.Expired, "a removal younger than a resolve grace ends nothing yet")
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))

	f.pastGrace()
	res, err = svc.Reap(ctx, f.scope, 10)
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
	f.pastGrace()
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
	f.pastGrace()

	candidates, err := f.cases.SourceRemovedCandidates(ctx, f.scope,
		f.clk.Now().Add(-domain.DefaultResolveGrace), 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	f.h.Source(f.org, f.cluster) // registered between the scan and the write
	svc := f.sweepService(repository.NewCaseRepository(f.pool), nil)
	expired, err := svc.expire(ctx, f.scope, candidates[0], f.clk.Now(),
		svc.lifecycleSettings(ctx, f.scope), domain.ResolveSourceRemoved, nil)
	require.NoError(t, err)
	assert.False(t, expired, "the scan's verdict must not reach the database")
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))
}

// TestReapEndsNothingWhenASourceIsDeletedAndRegisteredAgainInsideTheGrace — B5.
// An operator deletes a source and registers it again (a fixed URL, a rotated
// token): for a moment the cluster has no live source. Nothing may expire in that
// moment — the scan waits a resolve grace past the last removal — and once the new
// source is live the cluster is simply not orphaned.
func TestReapEndsNothingWhenASourceIsDeletedAndRegisteredAgainInsideTheGrace(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	src := f.h.Source(f.org, f.cluster)
	c := f.openWebhookCase()
	svc := f.sweepService(repository.NewCaseRepository(f.pool), nil)

	f.removeSource(src.ID)
	f.clk.Advance(time.Minute)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired, "the cluster has had no live source for a minute, not a grace")

	f.h.Source(f.org, f.cluster) // registered again, inside the grace
	f.pastGrace()
	res, err = svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.SourceRemoved)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))
	assert.Empty(t, f.resolveReasonOf(c.ID()))
}

// ------------------------------------------------- HA clusters (owner ruling R1)

// ⭐⭐ TestReapExpiresAnHAPairsSilentCaseWhenBothReplicasAreHealthy — R1. An HA pair
// is two witnesses to one cluster's alerts. When BOTH are healthy and neither has
// said anything about a Case for longer than either would let pass, the Case
// expires as `silent` — it used to be held forever, because the reaper acted only
// under exactly one live source. The threshold is the LONGER of the two.
func TestReapExpiresAnHAPairsSilentCaseWhenBothReplicasAreHealthy(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	replicaA := f.h.Source(f.org, f.cluster)
	replicaB := f.h.Source(f.org, f.cluster)
	f.h.Exec(`UPDATE alert_sources SET max_silence_s = 172800 WHERE id = $1`, replicaB.ID)
	c := f.openWebhookCase()
	svc := f.expiryService(replicaA.ID, replicaB.ID)

	f.clk.Advance(25 * time.Hour)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired, "replica B allows two days of silence, and the cluster waits for it")

	f.clk.Advance(24 * time.Hour)
	res, err = svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Silent)
	assert.Zero(t, res.Held)
	assert.Equal(t, "silent", f.resolveReasonOf(c.ID()))
}

// TestReapExpiresAnHAPairsTimedOutCaseWhenBothReplicasAreHealthy — R1 for
// `timeout`: past `source_ends_at + resolve_grace` under a pair whose replicas are
// both healthy, the Case expires.
func TestReapExpiresAnHAPairsTimedOutCaseWhenBothReplicasAreHealthy(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()
	replicaA, replicaB := f.healthySource(), f.healthySource()
	c := f.openFiring(now.Add(-2*time.Hour), now.Add(-30*time.Minute))

	health := &fakeHealth{result: map[uuid.UUID]bool{replicaA.ID: true, replicaB.ID: true}}
	res, err := f.sweepService(repository.NewCaseRepository(f.pool), health).Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Expired)
	assert.Zero(t, res.Silent)
	assert.Equal(t, "timeout", f.resolveReasonOf(c.ID()))
	require.Len(t, health.asked, 1)
	assert.ElementsMatch(t, []uuid.UUID{replicaA.ID, replicaB.ID}, health.asked[0],
		"the guard asks EVERY live source on the cluster")
}

// TestReapHoldsAnHAPairsCaseWhileOneReplicaIsNotHealthy — R1's other half. Either
// replica oto cannot see might be the one still carrying the alert, so one
// degraded replica holds every Case the pair shares, `timeout` and `silent` alike.
func TestReapHoldsAnHAPairsCaseWhileOneReplicaIsNotHealthy(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	ctx := t.Context()
	replicaA, replicaB := f.healthySource(), f.healthySource()
	f.markHealth("degraded", replicaB.ID)
	timedOut := f.openFiring(now.Add(-2*time.Hour), now.Add(-30*time.Minute))
	silent := f.openSecondFiring(now.Add(-2*time.Hour), time.Time{})
	svc := f.portOnly(replicaA.ID, replicaB.ID)

	f.clk.Advance(48 * time.Hour)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(timedOut.ID()))
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(silent.ID()))

	f.markHealth("healthy", replicaB.ID)
	res, err = f.portOnly(replicaA.ID).Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired, "the port's no about replica B holds both Cases")
	assert.Equal(t, 2, res.Held)
	assert.Equal(t, []uuid.UUID{replicaB.ID}, res.HeldSources,
		"the healthy replica is never named: it would raise a false source.unreachable")
}

// TestReapLeavesAnHAPairsSilentCaseOpenWhenOneReplicaTurnedMaxSilenceOff — R1's
// threshold rule: a NULL `max_silence_s` on ANY live source turns `silent` off for
// the cluster, because that replica's operator said its silence proves nothing.
func TestReapLeavesAnHAPairsSilentCaseOpenWhenOneReplicaTurnedMaxSilenceOff(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()
	replicaA := f.h.Source(f.org, f.cluster)
	replicaB := f.h.Source(f.org, f.cluster)
	f.h.Exec(`UPDATE alert_sources SET max_silence_s = NULL WHERE id = $1`, replicaB.ID)
	c := f.openWebhookCase()
	svc := f.expiryService(replicaA.ID, replicaB.ID)

	f.clk.Advance(30 * 24 * time.Hour)
	res, err := svc.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Considered)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(c.ID()))
}

// ------------------------------------------- the rollout flag (judged fix B2)

// TestReapWritesNeitherNewReasonWhileTheFlagIsOff — `jobs.expire_silent_and_removed`
// is off by default, and off, the reaper is the `timeout` sweep it was before
// 00094: a Case silent for two days under a healthy source, and a Case whose only
// source was deleted long ago, both stay open.
func TestReapWritesNeitherNewReasonWhileTheFlagIsOff(t *testing.T) {
	f := newFixture(t, harness.Epoch)
	ctx := t.Context()

	src := f.healthySource()
	silent := f.openWebhookCase()

	gone := f.h.ClusterNamed(f.org, "gone")
	goneSrc := f.h.Source(f.org, gone)
	orphan := f.openOnCluster(gone)
	f.removeSource(goneSrc.ID)

	off := f.sweepServiceWith(repository.NewCaseRepository(f.pool),
		&fakeHealth{result: map[uuid.UUID]bool{src.ID: true}}, false)
	f.clk.Advance(48 * time.Hour)
	res, err := off.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Zero(t, res.Expired)
	assert.Zero(t, res.Silent)
	assert.Zero(t, res.SourceRemoved)
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(silent.ID()))
	assert.Equal(t, domain.StateFiring.String(), f.alertStateOf(orphan.ID()))

	on := f.sweepServiceWith(repository.NewCaseRepository(f.pool),
		&fakeHealth{result: map[uuid.UUID]bool{src.ID: true}}, true)
	res, err = on.Reap(ctx, f.scope, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Silent, "the same tick with the flag on ends the silent Case")
	assert.Equal(t, 1, res.SourceRemoved, "and the orphaned one")
}

// openOnCluster opens a webhook-only Case (no `endsAt`) for a distinct alert on
// another cluster of the fixture's org.
func (f *fixture) openOnCluster(cl harness.Cluster) domain.Case {
	f.t.Helper()
	labels := harness.Labels(f.t, map[string]string{
		"alertname": "OrphanedAlert",
		"severity":  "warning",
	})
	obs := domain.Observation{
		Source:            domain.ObservedByIngest,
		ClusterID:         cl.ID,
		ClusterKey:        cl.Key,
		AlertKey:          harness.AlertKey(f.orgID, cl.Key, labels),
		SourceFingerprint: domain.ComputeSourceFingerprint(labels),
		Labels:            labels,
		Annotations:       map[string]string{},
		Status:            "firing",
		SourceStartsAt:    f.clk.Now().Add(-time.Hour),
		SourceUpdatedAt:   f.clk.Now(),
		ObservedAt:        f.clk.Now(),
	}
	if _, err := f.svc.ObserveBatch(f.t.Context(), f.scope, []domain.Observation{obs},
		ObserveOptions{}); err != nil {
		f.t.Fatalf("open case on cluster %s: %v", cl.ID, err)
	}
	ac, err := f.cases.GetByID(f.t.Context(), f.scope, f.caseIDOf(obs.AlertKey))
	if err != nil {
		f.t.Fatalf("read case on cluster %s: %v", cl.ID, err)
	}
	return ac
}

// ---------------------------------------------------- the re-read, as a function

// TestSilentStandsDownUnlessTheRowAndTheClusterStillSayItIsSilent pins
// `unexpirable`'s `silent` arm as a pure function, for `unreapable`'s reason (see
// reap_guard_test.go): through the sweep, domain.Apply's own refusal would make
// the test pass with the guard deleted.
func TestSilentStandsDownUnlessTheRowAndTheClusterStillSayItIsSilent(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	row := reapRow(t, started, started, time.Time{}, time.Time{}) // last heard at `started`
	proven := uuid.MustParse("018f3a4b-0000-7000-8000-0000000003c1")
	replica := uuid.MustParse("018f3a4b-0000-7000-8000-0000000003c2")
	day := 24 * time.Hour
	grace := 5 * time.Minute
	later := started.Add(2 * day)
	alone := domain.CaseSources{Live: 1, LiveIDs: []uuid.UUID{proven}, MaxSilence: day}
	pair := domain.CaseSources{Live: 2, LiveIDs: []uuid.UUID{replica, proven}, MaxSilence: day}
	const moved = "the sources proven healthy are no longer the cluster's live sources"

	assert.Empty(t, unexpirable(row, later, grace, domain.ResolveSilent, alone, []uuid.UUID{proven}))
	assert.Empty(t, unexpirable(row, later, grace, domain.ResolveSilent, pair,
		[]uuid.UUID{proven, replica}), "an HA pair proven healthy in any order stands (R1)")
	assert.Equal(t, "heard about within max_silence_s",
		unexpirable(row, started.Add(day), grace, domain.ResolveSilent, alone, []uuid.UUID{proven}))
	assert.Equal(t, "a live source turned max_silence_s off",
		unexpirable(row, later, grace, domain.ResolveSilent,
			domain.CaseSources{Live: 1, LiveIDs: []uuid.UUID{proven}}, []uuid.UUID{proven}))
	assert.Equal(t, moved,
		unexpirable(row, later, grace, domain.ResolveSilent, pair, []uuid.UUID{proven}),
		"a replica joined: the health that was proven no longer covers the cluster")
	assert.Equal(t, moved,
		unexpirable(row, later, grace, domain.ResolveSilent, alone, []uuid.UUID{proven, replica}),
		"a replica left: its health verdict went with it")
	assert.Equal(t, moved,
		unexpirable(row, later, grace, domain.ResolveSilent,
			domain.CaseSources{Live: 1, LiveIDs: []uuid.UUID{uuid.New()}, MaxSilence: day},
			[]uuid.UUID{proven}))
	assert.Equal(t, moved,
		unexpirable(row, later, grace, domain.ResolveSilent, domain.CaseSources{}, nil),
		"no live source is nobody to vouch for the Case")
}

// TestTimeoutStandsDownUnlessTheProvenSourcesAreStillTheLiveSet — R1 applied to
// `timeout`: the guard's verdict covers the sources it asked, and the expiry
// stands only while they are still exactly the cluster's live set.
func TestTimeoutStandsDownUnlessTheProvenSourcesAreStillTheLiveSet(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	row := reapRow(t, started, started, time.Time{}, time.Time{})
	a := uuid.MustParse("018f3a4b-0000-7000-8000-0000000003d1")
	b := uuid.MustParse("018f3a4b-0000-7000-8000-0000000003d2")
	now := started.Add(time.Hour)
	pair := domain.CaseSources{Live: 2, LiveIDs: []uuid.UUID{a, b}}

	assert.Empty(t, unexpirable(row, now, time.Minute, domain.ResolveTimeout, pair, []uuid.UUID{b, a}))
	assert.Equal(t, "the sources proven healthy are no longer the cluster's live sources",
		unexpirable(row, now, time.Minute, domain.ResolveTimeout, pair, []uuid.UUID{a}))
	assert.Equal(t, "the sources proven healthy are no longer the cluster's live sources",
		unexpirable(row, now, time.Minute, domain.ResolveTimeout, pair, []uuid.UUID{a, a}),
		"a duplicated proof is not a proof of the second replica")
}

// TestSourceRemovedStandsDownUntilTheGraceHasPassedSinceTheLastRemoval pins B5 as a
// pure function: the scan's cutoff, re-asked of the cluster inside the transaction.
func TestSourceRemovedStandsDownUntilTheGraceHasPassedSinceTheLastRemoval(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	row := reapRow(t, started, started, time.Time{}, time.Time{})
	grace := 5 * time.Minute
	removedAt := started.Add(time.Hour)
	orphan := domain.CaseSources{Removed: 1, LastRemovedAt: removedAt}

	assert.Equal(t, "a source was removed from the cluster within resolve_grace",
		unexpirable(row, removedAt.Add(time.Minute), grace, domain.ResolveSourceRemoved, orphan, nil))
	assert.Equal(t, "a source was removed from the cluster within resolve_grace",
		unexpirable(row, removedAt.Add(grace), grace, domain.ResolveSourceRemoved, orphan, nil),
		"the cutoff is strict, as the scan's `max(deleted_at) < now - grace` is")
	assert.Empty(t, unexpirable(row, removedAt.Add(grace+time.Second), grace,
		domain.ResolveSourceRemoved, orphan, nil))

	assert.Equal(t, "a live source feeds the cluster again",
		unexpirable(row, removedAt.Add(time.Hour), grace, domain.ResolveSourceRemoved,
			domain.CaseSources{Live: 1, Removed: 1, LiveIDs: []uuid.UUID{uuid.New()}, LastRemovedAt: removedAt},
			nil))
	assert.Equal(t, "no source was ever removed from the cluster",
		unexpirable(row, removedAt.Add(time.Hour), grace, domain.ResolveSourceRemoved,
			domain.CaseSources{}, nil))
}
