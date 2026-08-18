package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAPayloadWithNoTenantHalfIsTheFanOutTick is the deploy-safety property, and
// it is the reason the payload version stays at 1 across this addition.
//
// ⛔ EVERY ROW ALREADY IN `river_job` WHEN THIS SHIPS WAS WRITTEN BY A SCHEDULER
// THAT HAD NEVER HEARD OF `org_id`. Those payloads are `{"v":1}` and nothing
// else, and a worker that read them as "a job for the nil org" would sweep no
// tenant at all — a fleet-wide silent stop, one deploy long, on the sweeps that
// expire cases and close groups. An absent field decodes to the zero value,
// the zero value IS the fan-out tick, and the fan-out tick is exactly what those
// rows meant. That equivalence is what makes adding the field a v1 change rather
// than a v2 one: a field was added, no field's meaning changed.
//
// ⚠️ `source.reconcile` IS THE SIXTH ROW AND THE ONLY ONE WHOSE PRE-EXISTING
// PAYLOADS ARE NOT ALL TICKS. Its queue holds two kinds of old row: `{"v":1}`,
// which meant the fan-out tick and still does, and `{"v":1,"source_id":"…"}`,
// which meant ONE PASS against that source. The second decodes to a zero tenant
// half, so `IsFanOut()` answers true for it — and that answer is not wrong, it is
// simply not the question: the handler reads the SOURCE id first, and only a
// payload with neither id is the tick. An in-flight per-source pass therefore
// survives the deploy as a pass rather than becoming a tick that re-enqueues the
// whole tenant list. TestAPreExistingSourceReconcilePayloadIsStillOnePass is where
// that half is pinned; the row below covers the tick half.
func TestAPayloadWithNoTenantHalfIsTheFanOutTick(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		wire string
		into func([]byte) (TenantFanOut, int, error)
	}{
		{"case.reap", `{"v":1}`, decodeInto[CaseReapArgs]},
		{"group.close", `{"v":1}`, decodeInto[GroupCloseArgs]},
		{"retention.prune", `{"v":1}`, decodeInto[RetentionPruneArgs]},
		{"stats.rollup", `{"v":1,"day":"2026-08-07"}`, decodeInto[StatsRollupArgs]},
		{"source.reconcile", `{"v":1}`, decodeInto[SourceReconcileArgs]},
	} {
		fo, version, err := tc.into([]byte(tc.wire))
		if err != nil {
			t.Fatalf("%s: a payload written before the field existed no longer decodes: %v", tc.name, err)
		}
		if !fo.IsFanOut() {
			t.Errorf("%s: a pre-existing payload was read as one tenant's pass, so every other tenant is skipped", tc.name)
		}
		if version != 1 {
			t.Errorf("%s: payload version = %d, want 1 — adding a field must not park every queued job", tc.name, version)
		}
	}
}

// TestAnOrgIDMakesItOneTenantsPass is the other direction: the discriminator has
// to actually discriminate, or a per-tenant job re-enters the fan-out and every
// tick multiplies itself by the tenant count.
func TestAnOrgIDMakesItOneTenantsPass(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	if (TenantFanOut{OrgID: orgID}).IsFanOut() {
		t.Error("a payload naming an org was read as the fan-out tick; each tenant's job would fan out again")
	}
	// A cursor alone is still a fan-out — it is the SAME tick, resumed.
	if !(TenantFanOut{After: orgID}).IsFanOut() {
		t.Error("a continuation was read as one tenant's pass, so the tenants past the ceiling are never reached")
	}
}

// TestTheTenantHalfSurvivesARoundTrip guards the thing River's ByArgs uniqueness
// actually hashes. If the fields did not serialise, every tenant's job would
// share one unique key per period and 499 of every 500 would be collapsed as
// duplicates — a fan-out that enqueues 500 jobs and runs one.
//
// Two shapes is the common case and `case.reap` stands for all of them.
// `source.reconcile` has four, which is a strictly harder version of the same
// question: see TestTheFourSourceReconcileShapesAreFourUniqueKeys.
func TestTheTenantHalfSurvivesARoundTrip(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	wire, err := json.Marshal(CaseReapArgs{TenantFanOut: TenantFanOut{OrgID: orgID}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back CaseReapArgs
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.OrgID != orgID {
		t.Errorf("org id did not survive the payload: got %s, want %s (%s)", back.OrgID, orgID, wire)
	}

	other, err := json.Marshal(CaseReapArgs{TenantFanOut: TenantFanOut{OrgID: uuid.New()}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(wire) == string(other) {
		t.Error("two tenants produced identical payloads; ByArgs uniqueness would collapse them onto one job")
	}
}

// TestAPreExistingSourceReconcilePayloadIsStillOnePass is the other half of the
// deploy-safety property, and it exists only for this kind because only this kind
// had a non-tick payload in the queue before the embed arrived.
//
// ⛔ A ROW THAT MEANT "RECONCILE THIS SOURCE" MUST NOT COME BACK AS A FAN-OUT. The
// tenant half decodes to zero on it — nothing wrote those fields — so the tenant
// half alone cannot tell the two apart, and the discriminator that can is the
// SOURCE id the row already carried. What this pins is that the source id survives
// the added field: if it did not, every in-flight pass across a deploy would turn
// into a tick that re-enqueues the whole tenant list, and the pass itself would
// simply never happen.
func TestAPreExistingSourceReconcilePayloadIsStillOnePass(t *testing.T) {
	t.Parallel()

	sourceID := uuid.New()
	var args SourceReconcileArgs
	if err := json.Unmarshal([]byte(`{"v":1,"source_id":"`+sourceID.String()+`"}`), &args); err != nil {
		t.Fatalf("a payload written before the tenant half existed no longer decodes: %v", err)
	}
	if args.SourceID != sourceID {
		t.Fatalf("source id = %s, want %s; an in-flight pass became a fan-out tick", args.SourceID, sourceID)
	}
	if args.PayloadVersion() != 1 {
		t.Errorf("payload version = %d, want 1 — adding a field must not park every queued job", args.PayloadVersion())
	}
	if args.OrgID != uuid.Nil || args.After != uuid.Nil {
		t.Errorf("the tenant half decoded to %+v from a payload that never carried one", args.TenantFanOut)
	}
}

// TestTheFourSourceReconcileShapesAreFourUniqueKeys is the same ByArgs property as
// above, for the one kind that has FOUR shapes rather than two.
//
// ⛔ RIVER HASHES THE WHOLE PAYLOAD, AND EVERY SHAPE HAS TO LAND ON ITS OWN KEY.
// The tick, a continuation resuming after some tenant, one tenant's fan-out and one
// source's pass all share a kind, a queue and a thirty-second uniqueness period, so
// two of them serialising alike would be silently collapsed into one job. The
// dangerous pair is the tick and its own continuation: a continuation that hashed
// like the tick it came from would be dropped as a duplicate within the same
// period, and every tenant past the first page would go unswept while the log line
// still said a continuation had been queued.
func TestTheFourSourceReconcileShapesAreFourUniqueKeys(t *testing.T) {
	t.Parallel()

	orgID, cursor, sourceID := uuid.New(), uuid.New(), uuid.New()
	shapes := map[string]SourceReconcileArgs{
		"the fan-out tick":  {},
		"a continuation":    {TenantFanOut: TenantFanOut{After: cursor}},
		"one tenant's pass": {TenantFanOut: TenantFanOut{OrgID: orgID}},
		"one source's pass": {SourceID: sourceID},
	}

	seen := map[string]string{}
	for name, args := range shapes {
		wire, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if other, dup := seen[string(wire)]; dup {
			t.Fatalf("%s and %s serialise identically (%s); ByArgs uniqueness would collapse them onto one job",
				name, other, wire)
		}
		seen[string(wire)] = name

		var back SourceReconcileArgs
		if err := json.Unmarshal(wire, &back); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if back != args {
			t.Errorf("%s did not survive the payload: got %+v, want %+v (%s)", name, back, args, wire)
		}
	}
}

// TestTheRetryBudgetIsPerSourceReconcileShape pins which of the four shapes may
// retry thirteen times and which may retry three.
//
// ⛔ THE SPLIT IS A ROW-COUNT DECISION AND IT IS INVISIBLE AT THE CALL SITE.
// `InsertOpts` is read by River, not by us, so a budget that drifted would show up
// only as a queue that will not drain: at thirteen attempts the backoff sum is
// about 28.5 minutes while the tick that created the row runs every 30 s, which is
// on the order of 57 live retryable rows per persistently-failing tenant against
// about 0.2 at three. And a retried CONTINUATION is worse than a wasted row — it
// replays a cursor up to 28 minutes stale, which `ByPeriod` will not dedupe because
// the cursor makes it a distinct key.
//
// ⛔ THE PER-SOURCE PASS IS THE EXCEPTION AND MUST STAY ONE. It is the mandatory
// reconciler (ADR 0006) and the only producer of `suppressed`, so it keeps the
// ladder that rides out a blip. A blanket periodic budget here would be a quiet
// downgrade of the one job in this kind that nothing else can do.
func TestTheRetryBudgetIsPerSourceReconcileShape(t *testing.T) {
	t.Parallel()

	orgID, cursor, sourceID := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name string
		args SourceReconcileArgs
		want int
	}{
		{"the fan-out tick", SourceReconcileArgs{}, MaxAttemptsPeriodic},
		{"a continuation", SourceReconcileArgs{TenantFanOut: TenantFanOut{After: cursor}}, MaxAttemptsPeriodic},
		{"one tenant's sources", SourceReconcileArgs{TenantFanOut: TenantFanOut{OrgID: orgID}}, MaxAttemptsPeriodic},
		{"one source's pass", SourceReconcileArgs{SourceID: sourceID}, MaxAttemptsRetryable},
		// A source id alongside an org id is still one pass — the handler reads the
		// source id first, and the budget has to agree with the handler.
		{
			"one source's pass, with an org id nobody wrote",
			SourceReconcileArgs{TenantFanOut: TenantFanOut{OrgID: orgID}, SourceID: sourceID},
			MaxAttemptsRetryable,
		},
	} {
		got := tc.args.InsertOpts()
		if got.MaxAttempts != tc.want {
			t.Errorf("%s: MaxAttempts = %d, want %d", tc.name, got.MaxAttempts, tc.want)
		}
		// The rest of the row is one kind's routing and does not vary by shape: a
		// continuation that landed on another queue or lost its uniqueness window
		// would be a different job wearing the same name.
		if got.Queue != QueueReconcile || got.Priority != PriorityNormal {
			t.Errorf("%s: queue/priority = %s/%d, want %s/%d",
				tc.name, got.Queue, got.Priority, QueueReconcile, PriorityNormal)
		}
		if !got.UniqueOpts.ByArgs || got.UniqueOpts.ByPeriod != 30*time.Second {
			t.Errorf("%s: uniqueness = %+v, want ByArgs over a 30 s period", tc.name, got.UniqueOpts)
		}
	}
}

// tenantHalf reads the embedded half back out. It is a method ON the half, so it
// is PROMOTED by the embed and by nothing else — which is what makes the constraint
// below a compile-time statement about the embed rather than a hopeful one.
//
// It lives in this file because only these tests need it: an exported accessor on
// a production type with no production caller is the shape `tools/lintreach` exists
// to find.
func (t TenantFanOut) tenantHalf() TenantFanOut { return t }

// tenantScopedArgs is what a per-tenant periodic's args type IS: a versioned
// payload with the tenant half embedded.
//
// ⛔ THE CONSTRAINT IS THE POINT, AND THE TYPE SWITCH IT REPLACED WAS NOT. That
// switch listed the five args types and fell through to `TenantFanOut{}` — whose
// `IsFanOut()` is TRUE — so a sixth args type that forgot the embed did not fail to
// compile, it silently reported "this is the fan-out tick" and every assertion in
// this file passed for it. The comment said the opposite. A method set cannot be
// faked: an args struct without the embed does not satisfy this, and `decodeInto`
// refuses it at the call site.
type tenantScopedArgs interface {
	versioned
	tenantHalf() TenantFanOut
}

// decodeInto is the generic half of the table above: decode a wire payload into
// one args type and hand back the two things under test.
func decodeInto[T tenantScopedArgs](wire []byte) (TenantFanOut, int, error) {
	var a T
	if err := json.Unmarshal(wire, &a); err != nil {
		return TenantFanOut{}, 0, err
	}
	return a.tenantHalf(), a.PayloadVersion(), nil
}
