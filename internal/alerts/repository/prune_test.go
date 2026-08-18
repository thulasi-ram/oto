package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// The `alert_event_keys` sweep — the one `retention.prune` runs, and the one
// 00007 promised in a table comment and an index thirty-six migrations before
// anything performed it.
//
// ⭐ WHY THESE RUN AGAINST REAL POSTGRES. Every property here is a property of a
// STATEMENT: which rows a horizon selects, that a LIMIT in a subquery bounds a
// DELETE, and that a sweep with no tenant predicate reaches every tenant. A fake
// store would assert the shape of its own map, and the one thing worth knowing
// about this query — that it deletes some rows and not others — is the thing a
// fake cannot be wrong about.
//
// ⛔ THE ASSERTION THAT MATTERS IS THE SURVIVOR, not the count deleted. A key
// deleted early does not fail loudly: `AppendBatch` simply finds the key
// unclaimed, writes the event a second time, and reports success. That is SPEC
// acceptance criterion 36 breaking silently — a replayed `ingest_batch`
// appending the timeline twice — which is why the horizon is a floor of
// `domain.DedupeKeyRetention` widened to the longest `raw_retention_days` any
// tenant configured, and why every test below asserts what stayed.

type keyFixture struct {
	h    *harness.H
	repo *repository.EventRepository
	orgA harness.Org
	orgB harness.Org
}

func newKeyFixture(t *testing.T) keyFixture {
	t.Helper()
	h := harness.New(t)
	return keyFixture{
		h:    h,
		repo: repository.NewEventRepository(h.Pool, clock.NewFake(h.Now())),
		orgA: h.Org(),
		orgB: h.Org(),
	}
}

// claim writes one key directly, because the horizon is the subject and
// `created_at` has to be a value the test chose. The production writer lets the
// column DEFAULT to the database clock (00034), which is exactly the freedom a
// test of a time horizon cannot have.
func (f keyFixture) claim(org uuid.UUID, key string, at time.Time) {
	f.h.T.Helper()
	f.h.Exec(`INSERT INTO alert_event_keys (org_id, dedupe_key, event_id, created_at)
	          VALUES ($1, $2, $3, $4)`, org, key, id.New(), at)
}

// keysOf reads back one tenant's surviving keys, sorted, so a failure names the
// rows rather than a count.
func (f keyFixture) keysOf(org uuid.UUID) []string {
	f.h.T.Helper()
	rows, err := f.h.Pool.Query(f.h.Ctx,
		`SELECT dedupe_key FROM alert_event_keys WHERE org_id = $1 ORDER BY dedupe_key`, org)
	require.NoError(f.h.T, err)
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var k string
		require.NoError(f.h.T, rows.Scan(&k))
		out = append(out, k)
	}
	require.NoError(f.h.T, rows.Err())
	return out
}

// TestThePrunerDeletesOnlyKeysPastTheHorizon is the sweep's whole contract.
//
// The boundary is asserted in both directions AND ON THE INSTANT ITSELF, because
// `created_at < $1` is a strict inequality and a key stamped exactly at the
// horizon is one the sweep must leave alone: the horizon is the last instant the
// key is still needed, not the first instant it is not.
func TestThePrunerDeletesOnlyKeysPastTheHorizon(t *testing.T) {
	t.Parallel()
	f := newKeyFixture(t)

	horizon := f.h.Now().Add(-domain.DedupeKeyRetention)
	f.claim(f.orgA.ID, "case:stale:opened", horizon.Add(-time.Minute))
	f.claim(f.orgA.ID, "case:exactly:opened", horizon)
	f.claim(f.orgA.ID, "case:fresh:opened", horizon.Add(time.Minute))

	deleted, err := f.repo.PruneDedupeKeys(f.h.Ctx, horizon, 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	require.Equal(t, []string{"case:exactly:opened", "case:fresh:opened"}, f.keysOf(f.orgA.ID),
		"a key inside the horizon must survive the sweep: unclaiming one early does not fail, it "+
			"lets the next replay of that batch append the timeline a second time and report success")
}

// TestOneTickIsBoundedByItsLimit is why the sweep is a subquery with a LIMIT
// rather than a bare `DELETE ... WHERE created_at < $1`.
//
// ⛔ THE FIRST TICK ON A REAL DEPLOYMENT MEETS EVERY KEY EVER WRITTEN. Nothing
// has ever pruned this table, so an install that has been running since oto
// shipped has tens of millions of rows past the horizon, and an unbounded DELETE
// over them is one long transaction against the table the ingest path claims
// keys in. The bound is what makes convergence take several ticks instead of
// taking the database.
func TestOneTickIsBoundedByItsLimit(t *testing.T) {
	t.Parallel()
	f := newKeyFixture(t)

	horizon := f.h.Now().Add(-domain.DedupeKeyRetention)
	for _, key := range []string{"k1", "k2", "k3", "k4", "k5"} {
		f.claim(f.orgA.ID, key, horizon.Add(-time.Hour))
	}
	f.claim(f.orgA.ID, "live", horizon.Add(time.Hour))

	for _, want := range []int64{2, 2, 1, 0} {
		deleted, err := f.repo.PruneDedupeKeys(f.h.Ctx, horizon, 2)
		require.NoError(t, err)
		require.Equal(t, want, deleted,
			"a tick must delete at most its limit, and the sweep must CONVERGE: a tick that "+
				"keeps returning rows has stopped being a prune and started being a loop")
	}

	require.Equal(t, []string{"live"}, f.keysOf(f.orgA.ID))
}

// TestTheSweepReachesEveryTenant pins the one thing that looks like a bug and is
// not: this repository method takes no `db.TenantScope`.
//
// ⚠️ UNSCOPED BY DESIGN, like `SessionRepository.DeleteExpired` and
// `DedupRepository.Prune`. It is maintenance, run by a job and reachable from no
// request, and it is deliberately NOT a per-tenant fan-out: the horizon is the
// widest window any tenant configured (ADR 0024's "retention is a floor, never a
// ceiling"), so every tenant's keys age out on the same instant. A per-tenant
// sweep would be a per-tenant horizon, which is the version that deletes one
// org's keys while its payloads are still replayable.
func TestTheSweepReachesEveryTenant(t *testing.T) {
	t.Parallel()
	f := newKeyFixture(t)

	horizon := f.h.Now().Add(-domain.DedupeKeyRetention)
	for _, org := range []uuid.UUID{f.orgA.ID, f.orgB.ID} {
		f.claim(org, "case:stale:opened", horizon.Add(-time.Hour))
		f.claim(org, "case:fresh:opened", horizon.Add(time.Hour))
	}

	deleted, err := f.repo.PruneDedupeKeys(f.h.Ctx, horizon, 100)
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted,
		"one tick must sweep BOTH tenants: a sweep that stopped at the first org would leave "+
			"every other tenant's keys growing forever, which is the defect this job fixes")

	// The same dedupe key text exists in both tenants, so this also proves the
	// DELETE removes exactly the rows the bounded inner SELECT chose — it matches
	// them by `ctid`, one physical tuple each — and not every row sharing a
	// `dedupe_key`. A delete keyed on `dedupe_key` alone would have taken the other
	// tenant's live key with it.
	for _, org := range []uuid.UUID{f.orgA.ID, f.orgB.ID} {
		require.Equal(t, []string{"case:fresh:opened"}, f.keysOf(org))
	}
}

// TestASweptKeyIsFreeAgainAndALiveOneStillSuppresses runs the real writer against
// the real sweep, which is the only way to assert what pruning MEANS.
//
// ⭐ A KEY IS NOT A ROW, IT IS A PROMISE THAT AN EVENT WAS ALREADY RECORDED. So
// the property is behavioural on both sides: while the key is claimed a second
// `AppendBatch` of the same event writes NOTHING, and once the sweep has taken it
// the same append writes again. The second half is not a feature — it is the
// hazard the horizon exists to keep away from any batch still worth replaying —
// and it is asserted so that a sweep deleting too early can never look like a
// sweep working.
func TestASweptKeyIsFreeAgainAndALiveOneStillSuppresses(t *testing.T) {
	t.Parallel()
	f := newKeyFixture(t)

	// `alert_events` is partitioned MONTHLY on recorded_at and the schema
	// pre-creates this month plus three, so a real append has to happen at a real
	// now — unlike the key rows above, which live in an unpartitioned table and can
	// be dated freely.
	now := time.Now().UTC()
	event := newKeyedEvent(t, f.orgA.ID, now, "case:"+id.New().String()+":opened")

	written, err := f.repo.AppendBatch(f.h.Ctx, f.orgA.Scope, []domain.Event{event})
	require.NoError(t, err)
	require.Equal(t, 1, written)

	// The horizon a healthy deployment sweeps on. The key was claimed seconds ago,
	// so nothing may touch it.
	deleted, err := f.repo.PruneDedupeKeys(f.h.Ctx, now.Add(-domain.DedupeKeyRetention), 100)
	require.NoError(t, err)
	require.Zero(t, deleted)

	replay := newKeyedEvent(t, f.orgA.ID, now, event.DedupeKey())
	written, err = f.repo.AppendBatch(f.h.Ctx, f.orgA.Scope, []domain.Event{replay})
	require.NoError(t, err)
	require.Zero(t, written,
		"a claimed key must still suppress the append: this is SPEC acceptance criterion 36 — "+
			"replaying a stored batch reproduces the state without duplicating the timeline")

	// Now sweep it, with a horizon past the claim, and the promise is gone.
	deleted, err = f.repo.PruneDedupeKeys(f.h.Ctx, now.Add(time.Hour), 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	written, err = f.repo.AppendBatch(f.h.Ctx, f.orgA.Scope, []domain.Event{replay})
	require.NoError(t, err)
	require.Equal(t, 1, written,
		"once the key is swept the same event appends AGAIN — which is why the horizon must "+
			"outlive every batch that can still be replayed, and why the pruner takes the WIDER "+
			"of 30 days and the longest raw_retention_days any tenant configured")
}

func newKeyedEvent(t *testing.T, orgID uuid.UUID, at time.Time, dedupeKey string) domain.Event {
	t.Helper()

	actor, err := domain.SystemActor(domain.ActorIngest)
	require.NoError(t, err)
	observed, err := domain.NewObservationTime(at, at)
	require.NoError(t, err)

	e, err := domain.NewEvent(domain.EventParams{
		ID:        id.New(),
		OrgID:     orgID,
		CaseID:    id.New(),
		Type:      domain.EventCaseOpened,
		At:        observed,
		Actor:     actor,
		Summary:   "case opened",
		DedupeKey: dedupeKey,
	})
	require.NoError(t, err)
	return e
}
