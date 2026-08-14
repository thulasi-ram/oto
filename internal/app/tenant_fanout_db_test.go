package app

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/test/harness"
)

// These tests are about the fan-out that replaced the tenant loop inside every
// periodic sweep (05e6fb1). They run against a real Postgres because the two
// things that can go wrong live in SQL — the page that drives the fan-out and the
// lookup that authorises one tenant's job — and a fake pool would agree with
// whatever the query happened to say.
//
// ⚠️ NOTHING HERE WRITES A TIMELINE EVENT, AND NOTHING HERE NEEDS A DERIVED
// `now()` ANY MORE. `alert_events` is partitioned with no default partition and
// the partition manager builds its window around the DATABASE's clock, so an
// append stamped at the harness clock used to fail a nameless 23514 and five
// tests each derived a `now` of their own to dodge it. `test/harness`'s
// `epochPartitionsSQL` now builds the same window around `harness.Epoch` at
// template bootstrap (git-bug 6547228), so a test that grows into driving a real
// per-tenant sweep can write at `h.Now()` like everything else — the answer is
// there, not in a sixth local helper.

// TestTheFanOutReachesEveryTenantAcrossContinuations is the property the ceiling
// exists to preserve, and the one a naive bound destroys.
//
// ⭐ A CEILING THAT DROPS IS WORSE THAN NO CEILING AT ALL. Truncating at the first
// TenantFanOutLimit tenants sorted by id would starve everybody past the boundary
// FOREVER, because a periodic tick restarts the same walk every minute and nobody
// is holding the request to press again. So the truncated page queues a
// continuation carrying the cursor, and what this test asserts is the whole of
// that contract: no execution exceeds the ceiling, the chain terminates, and
// every live tenant is enqueued EXACTLY once across it — missed and duplicated
// are both failures, and a cursor that failed to advance would produce the second
// while looking like progress.
func TestTheFanOutReachesEveryTenantAcrossContinuations(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	// One more than the ceiling, so the first page is FULL and a second is
	// unavoidable. Anything less and a fan-out that ignored its cursor entirely
	// would still pass.
	want := seedOrgs(t, h, jobs.TenantFanOutLimit+1)

	// A departed tenant is seeded INSIDE the first page's range so the filter is
	// proved to be part of the query rather than a post-filter over the page.
	gone := want[0]
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), gone)
	want = want[1:]

	lister := orgLister{pool: h.Pool}
	enq := &recordingEnqueuer{}

	seen := map[uuid.UUID]int{}
	after := uuid.Nil
	executions := 0
	for {
		executions++
		require.LessOrEqual(t, executions, 10,
			"the continuation chain did not terminate; a full page that does not advance the cursor is an infinite fan-out")

		enq.reset()
		out, err := jobs.FanOutTenants(h.Ctx, jobs.KindOccurrenceReap, enq, lister, nil, after,
			func(f jobs.TenantFanOut) db.JobArgs { return jobs.OccurrenceReapArgs{TenantFanOut: f} })
		require.NoError(t, err)

		orgs, cont := enq.split(t)
		require.LessOrEqual(t, len(orgs), jobs.TenantFanOutLimit,
			"one execution enqueued more than the ceiling; the bound is not being applied")
		require.Equal(t, out.Enqueued, len(orgs),
			"the reported count and the inserted jobs disagree, so an operator's log line is fiction")
		for _, orgID := range orgs {
			seen[orgID]++
		}

		if !out.Deferred {
			require.Empty(t, cont, "a completed fan-out queued a continuation nobody will terminate")
			break
		}
		require.Len(t, cont, 1, "a deferred fan-out must queue exactly one continuation")
		require.NotEqual(t, after, cont[0], "the continuation did not advance the cursor")
		after = cont[0]
	}

	require.Greater(t, executions, 1,
		"the seed did not cross the ceiling, so the continuation was never exercised")
	require.Len(t, seen, len(want), "the fan-out reached a different set of tenants than the live one")
	for _, orgID := range want {
		require.Equal(t, 1, seen[orgID], "tenant %s was missed or enqueued twice", orgID)
	}
	require.Zero(t, seen[gone], "a departed tenant was given a job")
}

// TestAShortPageQueuesNoContinuation is the terminating half. It is stated
// separately because the failure it guards is silent in the opposite direction: a
// chain that never ends enqueues a job per tick forever against a tenant list
// that has already been exhausted.
func TestAShortPageQueuesNoContinuation(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()

	enq := &recordingEnqueuer{}
	out, err := jobs.FanOutTenants(h.Ctx, jobs.KindGroupClose, enq, orgLister{pool: h.Pool}, nil, uuid.Nil,
		func(f jobs.TenantFanOut) db.JobArgs { return jobs.GroupCloseArgs{TenantFanOut: f} })
	require.NoError(t, err)

	require.False(t, out.Deferred, "a page well under the ceiling reported itself truncated")
	orgs, cont := enq.split(t)
	require.Empty(t, cont, "an untruncated fan-out queued a continuation")
	require.Contains(t, orgs, org.ID, "the tenant that exists was not enqueued")
}

// TestAPerTenantJobResolvesItsTenantAgainstTheTable is the authorisation half,
// and it is the reason `LiveScope` exists at all.
//
// ⛔ A JOB PAYLOAD IS DATA, NOT AUTHORITY. If a per-tenant handler built its scope
// straight from the org id in its payload, the tenancy boundary would be decided
// by a row in `river_job` — and, more prosaically, a tenant that departed in the
// seconds between the fan-out and the pass would still be swept, undoing exactly
// what be3d314 fixed on the list side. So the id is resolved against `orgs` with
// the same `deleted_at IS NULL` filter, and a tenant that is gone is NOT a job
// failure: there is nothing to do and nothing to retry.
func TestAPerTenantJobResolvesItsTenantAgainstTheTable(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	live := h.Org()
	gone := h.Org()
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), gone.ID)

	lister := orgLister{pool: h.Pool}

	swept := []uuid.UUID{}
	record := func(_ context.Context, scope db.TenantScope) error {
		swept = append(swept, scope.OrgID())
		return nil
	}

	require.NoError(t, jobs.ForTenant(h.Ctx, jobs.KindOccurrenceReap, lister, live.ID, record))
	require.Equal(t, []uuid.UUID{live.ID}, swept, "a live tenant's job did not run under its own scope")

	require.NoError(t, jobs.ForTenant(h.Ctx, jobs.KindOccurrenceReap, lister, gone.ID, record),
		"a departed tenant must not fail the job; there is nothing to retry")
	require.NoError(t, jobs.ForTenant(h.Ctx, jobs.KindOccurrenceReap, lister, uuid.New(), record),
		"an org id naming nothing at all must not fail the job either")
	require.Equal(t, []uuid.UUID{live.ID}, swept,
		"a soft-deleted or unknown tenant was swept from a job payload")

	// The scope the handler receives comes from the table, not from the payload.
	scope, err := lister.LiveScope(h.Ctx, gone.ID)
	require.True(t, errs.IsKind(err, errs.KindNotFound),
		"a departed tenant must resolve to NotFound, not to a usable scope")
	require.Zero(t, scope.OrgID())
}

// TestScopePageIsBoundedAndOrdered pins the query the fan-out walks with. The
// bound is what stops one execution materialising the whole customer base, and
// the ascending order is what makes the cursor a cursor rather than a guess.
func TestScopePageIsBoundedAndOrdered(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	seeded := seedOrgs(t, h, 5)

	lister := orgLister{pool: h.Pool}
	page, err := lister.ScopePage(h.Ctx, uuid.Nil, 3)
	require.NoError(t, err)
	require.Len(t, page, 3, "the limit was not applied")

	// seedOrgs mints UUIDv7s in ascending order, so the first page is the first
	// three ids it minted — in that order.
	require.Equal(t, seeded[:3], page, "the page is not the keyset walk's next three tenants, in order")

	rest, err := lister.ScopePage(h.Ctx, page[len(page)-1], 3)
	require.NoError(t, err)
	require.Equal(t, seeded[3:], rest, "the cursor did not resume strictly after the id it was given")
}

// recordingEnqueuer is `db.Enqueuer` that keeps what it was handed. The fan-out's
// whole output is an insert, so this is the seam the assertions read.
type recordingEnqueuer struct {
	mu   sync.Mutex
	reqs []db.JobRequest
}

func (e *recordingEnqueuer) Enqueue(_ context.Context, args db.JobArgs, _ ...db.JobOption) (db.EnqueueResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = append(e.reqs, db.JobRequest{Args: args})
	return db.EnqueueResult{Kind: args.Kind()}, nil
}

func (e *recordingEnqueuer) EnqueueMany(_ context.Context, reqs []db.JobRequest) ([]db.EnqueueResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = append(e.reqs, reqs...)
	out := make([]db.EnqueueResult, len(reqs))
	for i, r := range reqs {
		out[i] = db.EnqueueResult{Kind: r.Args.Kind()}
	}
	return out, nil
}

func (e *recordingEnqueuer) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = nil
}

// split separates the per-tenant jobs from the continuation, which is the
// distinction the whole design turns on: one names an org and does work, the
// other names a cursor and does none.
func (e *recordingEnqueuer) split(t *testing.T) (orgIDs, continuations []uuid.UUID) {
	t.Helper()

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.reqs {
		fo, ok := tenantFanOutOf(r.Args)
		require.True(t, ok, "the fan-out enqueued a payload with no tenant half: %T", r.Args)
		if fo.IsFanOut() {
			require.NotEqual(t, uuid.Nil, fo.After,
				"a continuation with no cursor would restart the walk from the beginning, forever")
			continuations = append(continuations, fo.After)
			continue
		}
		require.Equal(t, uuid.Nil, fo.After, "a per-tenant job must carry no cursor")
		orgIDs = append(orgIDs, fo.OrgID)
	}
	return orgIDs, continuations
}

// tenantFanOutOf reads the embedded payload half back out of an args struct.
func tenantFanOutOf(args db.JobArgs) (jobs.TenantFanOut, bool) {
	switch a := args.(type) {
	case jobs.OccurrenceReapArgs:
		return a.TenantFanOut, true
	case jobs.GroupCloseArgs:
		return a.TenantFanOut, true
	case jobs.FlapScoreArgs:
		return a.TenantFanOut, true
	case jobs.RetentionPruneArgs:
		return a.TenantFanOut, true
	case jobs.StatsRollupArgs:
		return a.TenantFanOut, true
	default:
		return jobs.TenantFanOut{}, false
	}
}
