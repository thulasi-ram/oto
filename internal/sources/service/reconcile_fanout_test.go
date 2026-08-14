package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// THE TENANT LEVEL OF THE RECONCILE FAN-OUT, TESTED WHERE IT LIVES.
//
// `source.reconcile`'s fan-out is two levels deep: tenants, then that tenant's
// due sources. The inner one was always bounded by `FanOutLimit`; the outer one
// was a loop over `Scopes()` inside one execution of a job ticked every thirty
// seconds and killed at sixty, which is the defect 2d699d6 closed for the other
// five periodics and this change closes here.
//
// ⚠️ THE PAGER HERE IS A FAKE, AND THAT IS DELIBERATE RATHER THAN A SHORTCUT. The
// SQL half of the contract — the keyset walk, `deleted_at IS NULL`, and a limit
// it will not quietly clamp — is `orgLister`'s, and it is pinned against a real
// Postgres in `internal/app/tenant_fanout_db_test.go`. What is unproven and lives
// HERE is what the reconciler does with a page: that the ceiling defers instead of
// dropping, and that the source level still gets exactly the work it used to. So
// this fake serves the pager's contract exactly — ascending, strictly after the
// cursor, never a page smaller than asked for short of the end of the list — and a
// fake that broke it would be testing a system nobody deploys.

// TestTheReconcileFanOutReachesEveryTenantAcrossTicks is the property the ceiling
// exists to preserve, and the one the old `Scopes()` loop lost the moment its
// execution was killed part-way through.
//
// ⭐ A TENANT PAST THE FIRST PAGE MUST NOT BE STARVED. A bound that stopped at the
// first `TenantFanOutLimit` tenants sorted by id would be worse than the unbounded
// loop it replaced: the loop at least visited everybody, whereas a truncating
// ceiling restarts at the same place on every tick, so the tenants past it never
// have their due sources discovered — and nothing about that is visible, because a
// source that is never reconciled looks exactly like a source with nothing to say.
// So what is asserted is the whole contract: no execution exceeds the ceiling, the
// chain terminates, every tenant is reached EXACTLY once across it, and the tenant
// level does no per-tenant work of its own.
func TestTheReconcileFanOutReachesEveryTenantAcrossTicks(t *testing.T) {
	t.Parallel()

	// Three past the ceiling, so the first page is FULL and a second execution is
	// unavoidable. Anything at or under it and a fan-out that ignored its cursor
	// entirely would still pass.
	orgs := &fanOutOrgs{ids: orgIDsInOrder(jobs.TenantFanOutLimit + 3)}
	repo := &fanOutRepo{writeRepo: &writeRepo{}}
	queue := &fanOutQueue{}
	rec := newFanOutReconciler(t, orgs, repo, queue)
	ctx := context.Background()

	seen := map[uuid.UUID]int{}
	after := uuid.Nil
	ticks := 0
	for {
		ticks++
		if ticks > 10 {
			t.Fatal("the continuation chain did not terminate; a full page that does not " +
				"advance the cursor is a fan-out that runs forever")
		}

		queue.reset()
		listedBefore := len(repo.listedDue)
		n, err := rec.FanOut(ctx, jobs.TenantFanOut{After: after})
		if err != nil {
			t.Fatalf("tenant fan-out: %v", err)
		}
		tenants, cursors := queue.split(t)

		if len(repo.listedDue) != listedBefore {
			t.Fatalf("the tenant level read %d tenants' due sources; it must only ENQUEUE, "+
				"or one execution is doing the whole customer base's work again",
				len(repo.listedDue)-listedBefore)
		}
		if len(tenants) > jobs.TenantFanOutLimit {
			t.Fatalf("one execution enqueued %d tenants, above the ceiling of %d; the bound is not being applied",
				len(tenants), jobs.TenantFanOutLimit)
		}
		if n != len(tenants) {
			t.Fatalf("the fan-out reported %d enqueued and inserted %d, so the log line an operator reads is fiction",
				n, len(tenants))
		}
		for _, orgID := range tenants {
			seen[orgID]++
		}

		// Each of those jobs is the SOURCE level, and it is what the tenant loop
		// used to do inline. Running them here is what makes "reached" mean the due
		// sources were actually discovered, rather than a payload was written.
		for _, orgID := range tenants {
			queue.reset()
			m, err := rec.FanOut(ctx, jobs.TenantFanOut{OrgID: orgID})
			if err != nil {
				t.Fatalf("one tenant's fan-out: %v", err)
			}
			if m != 2 {
				t.Fatalf("one tenant with one due source enqueued %d jobs, want the reconcile/silences pair", m)
			}
			queue.requirePair(t, orgID)
		}

		if len(cursors) == 0 {
			break
		}
		if len(cursors) != 1 {
			t.Fatalf("a deferred fan-out queued %d continuations; exactly one carries the cursor", len(cursors))
		}
		if cursors[0] == after {
			t.Fatal("the continuation did not advance the cursor, so the next execution asks for the same page")
		}
		after = cursors[0]
	}

	if ticks < 2 {
		t.Fatal("the tenant list did not cross the ceiling, so the continuation was never exercised")
	}
	if len(seen) != len(orgs.ids) {
		t.Fatalf("the fan-out reached %d tenants of %d; the rest are starved until somebody notices",
			len(seen), len(orgs.ids))
	}
	for _, orgID := range orgs.ids {
		if seen[orgID] != 1 {
			t.Fatalf("tenant %s was enqueued %d times; missed and duplicated are both failures, "+
				"and a cursor that failed to advance produces the second while looking like progress",
				orgID, seen[orgID])
		}
	}

	// The inner level is UNCHANGED by the conversion: one bounded `ListDue` per
	// tenant, at `FanOutLimit`, and one per tenant only.
	if !slices.Equal(orgIDsSorted(repo.listedDue), orgs.ids) {
		t.Fatalf("the source level ran for %d tenants; it must run for every tenant exactly once",
			len(repo.listedDue))
	}
	for _, limit := range repo.limits {
		if limit != FanOutLimit {
			t.Fatalf("ListDue was asked for %d sources, not FanOutLimit (%d); the inner bound moved", limit, FanOutLimit)
		}
	}
}

// TestADepartedTenantsFanOutIsNotAJobFailure is the authorisation half. The org id
// travels in a job payload, and a payload is data: the tenant it names may have
// departed in the seconds between the tick and the pass, and sweeping it would
// produce reconcile passes, silences syncs and alerts nobody will ever read.
//
// ⛔ AND IT IS NOT AN ERROR EITHER. There is nothing to do and nothing to retry, so
// burning the periodic retry budget on a row that is never coming back would only
// turn a departure into a dead-lettered job every thirty seconds.
func TestADepartedTenantsFanOutIsNotAJobFailure(t *testing.T) {
	t.Parallel()

	orgs := &fanOutOrgs{ids: orgIDsInOrder(1)}
	repo := &fanOutRepo{writeRepo: &writeRepo{}}
	rec := newFanOutReconciler(t, orgs, repo, &fanOutQueue{})

	n, err := rec.FanOut(context.Background(), jobs.TenantFanOut{OrgID: orgIDAt(99)})
	if err != nil {
		t.Fatalf("a tenant that is gone must not fail the job: %v", err)
	}
	if n != 0 {
		t.Fatalf("a departed tenant had %d jobs enqueued for it", n)
	}
	if len(repo.listedDue) != 0 {
		t.Fatal("a departed tenant was swept from a job payload; the scope must come from the tenant list")
	}
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

// newFanOutReconciler builds the Reconciler over fakes. Only the fan-out halves
// are real here — the state machine and the cluster reader exist because
// NewReconciler refuses a dependency set that cannot work, and neither is reached
// by a fan-out.
func newFanOutReconciler(t *testing.T, orgs TenantLister, repo SourceRepository, enq db.Enqueuer) *Reconciler {
	t.Helper()

	svc, _ := newWriteService(t, func(o *Options) { o.Repo = repo })
	rec, err := NewReconciler(ReconcilerOptions{
		Sources:  svc,
		Alerts:   noopObserver{},
		Clusters: noopClusters{},
		Orgs:     orgs,
		Enqueuer: enq,
	})
	if err != nil {
		t.Fatalf("build reconciler: %v", err)
	}
	return rec
}

// orgIDAt mints the id of the i'th tenant. The ids ASCEND with i, because the
// walk is a keyset over `orgs.id` and an unordered fake would make the cursor look
// like it worked whatever it did.
func orgIDAt(i int) uuid.UUID {
	var id uuid.UUID
	binary.BigEndian.PutUint32(id[:4], uint32(i)+1)
	return id
}

func orgIDsInOrder(n int) []uuid.UUID {
	out := make([]uuid.UUID, 0, n)
	for i := range n {
		out = append(out, orgIDAt(i))
	}
	return out
}

func orgIDsSorted(in []uuid.UUID) []uuid.UUID {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// fanOutOrgs is `TenantLister` over a fixed tenant list: the pager the fan-out
// walks, and the lookup that turns the id a payload names into a scope.
type fanOutOrgs struct {
	ids []uuid.UUID
}

// ScopePage honours the contract `jobs.TenantPager` states, INCLUDING the part
// that looks like paranoia: a limit it cannot serve is an error, never a quietly
// smaller page. A short page is the caller's only signal that the tenant table has
// ended, so a fake that clamped would silently stop testing the continuation.
func (o *fanOutOrgs) ScopePage(_ context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	if limit <= 0 || limit > jobs.TenantFanOutLimit {
		return nil, errs.Newf(errs.KindInternal, "org_page_too_large",
			"a tenant page of %d was asked for and this lister serves at most %d", limit, jobs.TenantFanOutLimit)
	}
	out := make([]uuid.UUID, 0, limit)
	for _, id := range o.ids {
		if bytes.Compare(id[:], after[:]) <= 0 {
			continue
		}
		out = append(out, id)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (o *fanOutOrgs) LiveScope(_ context.Context, orgID uuid.UUID) (db.TenantScope, error) {
	if !slices.Contains(o.ids, orgID) {
		return db.TenantScope{}, errs.NotFound("org_not_found", "no such org, or it has been deleted")
	}
	return db.NewTenantScope(orgID)
}

// fanOutRepo is the write path's fake with the one method the fan-out actually
// calls made to answer: one due source per tenant, so "this tenant was reached"
// and "this tenant's sources were scheduled" are the same observation.
type fanOutRepo struct {
	*writeRepo

	listedDue []uuid.UUID
	limits    []int
}

func (r *fanOutRepo) ListDue(_ context.Context, s db.TenantScope, limit int) ([]domain.Source, error) {
	r.listedDue = append(r.listedDue, s.OrgID())
	r.limits = append(r.limits, limit)
	return []domain.Source{{
		ID: uuid.New(), OrgID: s.OrgID(), Name: "prod-eu", Kind: domain.KindAlertmanager,
	}}, nil
}

// fanOutQueue is `db.Enqueuer` that keeps what it was handed. The fan-out's whole
// output is an insert, so this is the seam every assertion reads.
type fanOutQueue struct {
	reqs []db.JobRequest
}

func (q *fanOutQueue) Enqueue(_ context.Context, args db.JobArgs, _ ...db.JobOption) (db.EnqueueResult, error) {
	q.reqs = append(q.reqs, db.JobRequest{Args: args})
	return db.EnqueueResult{Kind: args.Kind()}, nil
}

func (q *fanOutQueue) EnqueueMany(_ context.Context, reqs []db.JobRequest) ([]db.EnqueueResult, error) {
	q.reqs = append(q.reqs, reqs...)
	out := make([]db.EnqueueResult, len(reqs))
	for i, r := range reqs {
		out[i] = db.EnqueueResult{Kind: r.Args.Kind()}
	}
	return out, nil
}

func (q *fanOutQueue) reset() { q.reqs = nil }

// split separates the per-tenant jobs from the continuation, which is the
// distinction the whole design turns on: one names an org and does work, the other
// names a cursor and does none.
func (q *fanOutQueue) split(t *testing.T) (orgIDs, cursors []uuid.UUID) {
	t.Helper()

	for _, r := range q.reqs {
		args, ok := r.Args.(jobs.SourceReconcileArgs)
		if !ok {
			t.Fatalf("the tenant level enqueued a %T; it expands into its own kind and nothing else", r.Args)
		}
		if args.SourceID != uuid.Nil {
			t.Fatal("the tenant level named a source; the source list is the INNER level's to read")
		}
		if args.IsFanOut() {
			if args.After == uuid.Nil {
				t.Fatal("a continuation with no cursor restarts the walk from the beginning, forever")
			}
			cursors = append(cursors, args.After)
			continue
		}
		if args.After != uuid.Nil {
			t.Fatal("a per-tenant job carried a cursor, which is meaningless on it")
		}
		orgIDs = append(orgIDs, args.OrgID)
	}
	return orgIDs, cursors
}

// requirePair asserts one tenant's source level enqueued the §G.3 pair — the
// reconcile pass and the silences sync that rides the same fan-out — for the one
// source that was due, and that neither of them carries a tenant half.
func (q *fanOutQueue) requirePair(t *testing.T, orgID uuid.UUID) {
	t.Helper()

	if len(q.reqs) != 2 {
		t.Fatalf("tenant %s enqueued %d jobs for one due source", orgID, len(q.reqs))
	}
	rec, ok := q.reqs[0].Args.(jobs.SourceReconcileArgs)
	if !ok {
		t.Fatalf("the first job of the pair is a %T, not a reconcile pass", q.reqs[0].Args)
	}
	sync, ok := q.reqs[1].Args.(jobs.SilencesSyncArgs)
	if !ok {
		t.Fatalf("the second job of the pair is a %T, not a silences sync", q.reqs[1].Args)
	}
	if rec.SourceID == uuid.Nil || rec.SourceID != sync.SourceID {
		t.Fatalf("the pair names %s and %s; both are one source's work", rec.SourceID, sync.SourceID)
	}
	if !rec.IsFanOut() || rec.After != uuid.Nil {
		t.Fatal("a pass that names a source must carry no tenant half; the source id already names the tenant")
	}
}

type noopObserver struct{}

func (noopObserver) ObserveBatch(context.Context, db.TenantScope, []alerts.Observation) (int, error) {
	return 0, nil
}

type noopClusters struct{}

func (noopClusters) Get(context.Context, db.TenantScope, uuid.UUID) (domain.Cluster, error) {
	return domain.Cluster{}, nil
}
