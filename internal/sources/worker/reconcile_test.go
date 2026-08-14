package worker

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// THE DISCRIMINATOR, PINNED.
//
// `source.reconcile` carries three shapes under one kind, and the handler tells
// them apart with two `if`s and no help from the type system. Everything else
// about the fan-out is tested where it is implemented; what can only be tested
// HERE is the ROUTING — which shape reaches which method — because that is the one
// decision this file makes.

// TestASourceIDDispatchesThePassAndNotTheFanOut is the foot-gun's guard rail.
//
// ⛔ THE SOURCE ID IS READ BEFORE THE TENANT HALF, AND REORDERING THAT `if`
// COMPILES. `IsFanOut()` is true whenever no org id is set, which is true of every
// per-source pass ever enqueued — so a handler that asked the tenant half first
// would answer "this is the fan-out tick" to a job whose whole purpose is one
// source, re-enqueue the entire tenant list, and never run the pass. At a
// thirty-second tick that is a queue that grows by the customer count every tick
// while the reconciler — the ONLY producer of `suppressed` — silently stops. No
// other test in this repo would fail.
func TestASourceIDDispatchesThePassAndNotTheFanOut(t *testing.T) {
	t.Parallel()

	sourceID := uuid.New()
	scope := testScope(t)
	rec := &routingReconciler{}
	h := SourceReconcile(rec, staticOrg{scope: scope}, nil)

	// The org id is set as well, which nothing enqueues and which the payload
	// documents as ignored: a source id present alongside it is still one pass, and
	// the org id in a job row is never authority for anything.
	err := h(context.Background(), &jobs.Job[jobs.SourceReconcileArgs]{
		Kind: jobs.KindSourceReconcile,
		Args: jobs.SourceReconcileArgs{
			TenantFanOut: jobs.TenantFanOut{OrgID: uuid.New()},
			SourceID:     sourceID,
		},
	})
	if err != nil {
		t.Fatalf("one pass: %v", err)
	}

	if rec.fanOuts != 0 {
		t.Fatalf("a payload naming a source fanned out %d times; every pass would re-enqueue the tenant list", rec.fanOuts)
	}
	if len(rec.passes) != 1 || rec.passes[0] != sourceID {
		t.Fatalf("the pass ran for %v, want exactly [%s]", rec.passes, sourceID)
	}
	if rec.scoped != scope.OrgID() {
		t.Fatalf("the pass ran under org %s; the scope must come from the source's own row, not the payload",
			rec.scoped)
	}
}

// TestTheTwoFanOutShapesReachTheFanOutUnchanged is the other side of the same
// `if`: a payload with no source id is the fan-out, and WHICH fan-out — the tenant
// walk or one tenant's sources — is a question for the reconciler, so the half must
// arrive there untouched. A handler that dropped the cursor would restart the walk
// from the first tenant on every execution, forever.
func TestTheTwoFanOutShapesReachTheFanOutUnchanged(t *testing.T) {
	t.Parallel()

	cursor, orgID := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name string
		half jobs.TenantFanOut
	}{
		{"the periodic tick", jobs.TenantFanOut{}},
		{"a continuation", jobs.TenantFanOut{After: cursor}},
		{"one tenant's sources", jobs.TenantFanOut{OrgID: orgID}},
	} {
		rec := &routingReconciler{}
		h := SourceReconcile(rec, staticOrg{scope: testScope(t)}, nil)

		err := h(context.Background(), &jobs.Job[jobs.SourceReconcileArgs]{
			Kind: jobs.KindSourceReconcile,
			Args: jobs.SourceReconcileArgs{TenantFanOut: tc.half},
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(rec.passes) != 0 {
			t.Fatalf("%s ran a reconcile pass against %v; it names no source", tc.name, rec.passes)
		}
		if rec.fanOuts != 1 {
			t.Fatalf("%s reached the fan-out %d times, want once", tc.name, rec.fanOuts)
		}
		if rec.half != tc.half {
			t.Fatalf("%s arrived as %+v, want %+v; the shape the payload asked for is not the one that ran",
				tc.name, rec.half, tc.half)
		}
	}
}

// routingReconciler records which of the two methods the handler chose, and with
// what. It does no work: the routing IS the subject.
type routingReconciler struct {
	passes  []uuid.UUID
	scoped  uuid.UUID
	fanOuts int
	half    jobs.TenantFanOut
}

func (r *routingReconciler) Reconcile(
	_ context.Context, s db.TenantScope, sourceID uuid.UUID,
) (domain.ReconcileResult, error) {
	r.passes = append(r.passes, sourceID)
	r.scoped = s.OrgID()
	return domain.ReconcileResult{SourceID: sourceID, OK: true}, nil
}

func (r *routingReconciler) FanOut(_ context.Context, fo jobs.TenantFanOut) (int, error) {
	r.fanOuts++
	r.half = fo
	return 0, nil
}

// staticOrg is `OrgResolver` for a deployment with one tenant. The lookup is a
// fact about `alert_sources`, and the point of it is that the handler asks at all.
type staticOrg struct {
	scope db.TenantScope
}

func (o staticOrg) OrgForSource(_ context.Context, _ uuid.UUID) (db.TenantScope, error) {
	if !o.scope.Valid() {
		return db.TenantScope{}, errs.NotFound("not_found", "no such source")
	}
	return o.scope, nil
}

func testScope(t *testing.T) db.TenantScope {
	t.Helper()

	s, err := db.NewTenantScope(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return s
}
