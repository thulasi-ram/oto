package jobs

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// TestAPayloadWithNoTenantHalfIsTheFanOutTick is the deploy-safety property, and
// it is the reason the payload version stays at 1 across this addition.
//
// ⛔ EVERY ROW ALREADY IN `river_job` WHEN THIS SHIPS WAS WRITTEN BY A SCHEDULER
// THAT HAD NEVER HEARD OF `org_id`. Those payloads are `{"v":1}` and nothing
// else, and a worker that read them as "a job for the nil org" would sweep no
// tenant at all — a fleet-wide silent stop, one deploy long, on the sweeps that
// expire occurrences and close groups. An absent field decodes to the zero value,
// the zero value IS the fan-out tick, and the fan-out tick is exactly what those
// rows meant. That equivalence is what makes adding the field a v1 change rather
// than a v2 one: a field was added, no field's meaning changed.
func TestAPayloadWithNoTenantHalfIsTheFanOutTick(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		wire string
		into func([]byte) (TenantFanOut, int, error)
	}{
		{"occurrence.reap", `{"v":1}`, decodeInto[OccurrenceReapArgs]},
		{"group.close", `{"v":1}`, decodeInto[GroupCloseArgs]},
		{"flap.score", `{}`, decodeInto[FlapScoreArgs]},
		{"retention.prune", `{"v":1}`, decodeInto[RetentionPruneArgs]},
		{"stats.rollup", `{"v":1,"day":"2026-08-07"}`, decodeInto[StatsRollupArgs]},
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
func TestTheTenantHalfSurvivesARoundTrip(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	wire, err := json.Marshal(OccurrenceReapArgs{TenantFanOut: TenantFanOut{OrgID: orgID}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back OccurrenceReapArgs
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.OrgID != orgID {
		t.Errorf("org id did not survive the payload: got %s, want %s (%s)", back.OrgID, orgID, wire)
	}

	other, err := json.Marshal(OccurrenceReapArgs{TenantFanOut: TenantFanOut{OrgID: uuid.New()}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(wire) == string(other) {
		t.Error("two tenants produced identical payloads; ByArgs uniqueness would collapse them onto one job")
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
