package api

// THE ENRICHER DISCOVERY TRANSPORT, CHECKED AGAINST THE CONTRACT ITSELF.
//
// ⭐ NOTHING HERE RE-STATES A RESPONSE SHAPE BY HAND. The only shape assertion is
// `schema.Assert`, which compiles the JSON Schema `api/openapi/openapi.yaml`
// declares for that operationId and that status and validates the bytes the
// handler actually wrote. A hand-written expectation would be a second copy of
// the contract, and a second copy drifts exactly the way the DTOs did.
//
// The properties this file protects, and what broke when each did not hold:
//
//   - the {data, meta} envelope with `meta.request_id` present — it is REQUIRED
//     by the contract and `httpx.Meta` omits it when empty, so a response
//     produced outside the request-id middleware fails its own schema;
//   - `name`, `version` and `phase` are the REGISTRY's, not the DTO's guess —
//     `EnricherDTO.name` carries a dotted `pattern` and `phase` is a closed
//     `[1, 2]` enum, and only a fixture built from real enrichers proves the
//     handler renders values that satisfy either;
//   - `health_status` is `unknown` and never invented — the rolling counters live
//     in the pipeline's Prometheus metrics and are not readable from the registry
//     this endpoint serves, and an invented `healthy` would be worse than an
//     admitted `unknown`;
//   - a caller with no principal reads NOTHING — the handler re-derives its scope
//     from `authn.Scope` rather than trusting that a middleware ran, because "the
//     middleware guarantees it" stops being true the first time a route is
//     mounted somewhere else.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/alerthistory"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/promrule"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/relatedalerts"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/runbook"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// ------------------------------------------------------------------ fixtures

// contractRegistry builds the registry from the REAL enrichers oto ships, through
// the real `service.NewRegistry`.
//
// ⭐ THE REALISM IS LOAD BEARING. `EnricherDTO.name` has a dotted pattern,
// `version` has a minimum of 1 and `phase` is a closed `[1, 2]` enum, so a
// fixture of invented names and zero values would only prove that the empty case
// validates. These four carry oto's own names, versions, phases and per-call
// budgets, and one of them declares phase 2 so BOTH enum members appear in a body
// the schema has to accept.
//
// Their collaborators are nil: this endpoint reads a name, a version, a phase and
// a timeout, and never runs an enricher.
func contractRegistry(t *testing.T) *service.Registry {
	t.Helper()

	reg, err := service.NewRegistry(
		promrule.New(nil, nil),
		runbook.New(nil),
		alerthistory.New(nil, clock.New()),
		relatedalerts.New(nil, clock.New()),
	)
	if err != nil {
		t.Fatalf("build the enricher registry: %v", err)
	}
	return reg
}

// spyRegistry counts whether the handler consulted the registry at all, which is
// what turns "an anonymous caller got a 401" into "an anonymous caller read
// nothing".
type spyRegistry struct {
	inner *service.Registry
	calls int
}

func (s *spyRegistry) All() []domain.Enricher {
	s.calls++
	return s.inner.All()
}

func (s *spyRegistry) Enabled(name string) bool { return s.inner.Enabled(name) }

type enricherFixture struct {
	reg *spyRegistry
	c   *apitest.Client
}

// newEnricherFixture mounts the router behind the request-id middleware and an
// authenticated principal.
//
// The clock is the REAL one on purpose: `meta.elapsed_ms` is `time.Since(started)`
// and the contract types it as an int32 with a minimum of 0, so a fake epoch would
// make every success body report an elapsed time of several months.
func newEnricherFixture(t *testing.T) *enricherFixture {
	t.Helper()

	reg := &spyRegistry{inner: contractRegistry(t)}
	return &enricherFixture{reg: reg, c: apitest.New(NewRouter(reg, clock.New()))}
}

// ------------------------------------------------------------- happy paths

// TestListEnrichersAnswersTheEnvelopeTheContractDeclares.
//
// The promise: `GET /api/v1/enrichers` returns {data, meta}, every row an
// `EnricherDTO` with its required members, `meta.request_id` present.
//
// What broke when it did not hold: the conformance audit found operations
// answering a bare array and operations answering an envelope with no `meta`, and
// a client cannot tell which one it is looking at until it crashes on the
// difference.
func TestListEnrichersAnswersTheEnvelopeTheContractDeclares(t *testing.T) {
	t.Parallel()

	f := newEnricherFixture(t)
	resp := f.c.GET("/enrichers").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listEnrichers", http.StatusOK, resp.Body())

	if f.reg.calls != 1 {
		t.Fatalf("the registry was consulted %d time(s), want 1", f.reg.calls)
	}
	if ct := resp.Header("Content-Type"); ct != apitest.ContentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, apitest.ContentTypeJSON)
	}

	body := resp.JSON(t)
	data, ok := body["data"].([]any)
	if !ok || len(data) != 4 {
		t.Fatalf("data = %v, want the four enrichers the fixture registers: %s", body["data"], resp)
	}
}

// TestTheEnricherListIsOrderedByPhaseThenNameSoTwoReadsAgree.
//
// The promise: the registry is ordered by (phase, name), so two calls return the
// same order.
//
// What broke: a list whose ordering depends on map iteration produces diffs
// nobody can golden-test and a discovery screen that reshuffles between two
// identical reads.
func TestTheEnricherListIsOrderedByPhaseThenNameSoTwoReadsAgree(t *testing.T) {
	t.Parallel()

	f := newEnricherFixture(t)

	first := f.c.GET("/enrichers").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listEnrichers", http.StatusOK, first.Body())
	second := f.c.GET("/enrichers").MustStatus(t, http.StatusOK)

	// `meta.request_id` differs by design, so the comparison is over `data`.
	if a, b := enricherData(t, first), enricherData(t, second); a != b {
		t.Fatalf("two identical reads disagreed:\n%s\n%s", a, b)
	}

	names := enricherNames(t, first)
	want := []string{alerthistory.Name, promrule.Name, runbook.Name, relatedalerts.Name}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v — (phase, name) ascending, and %s is the only phase-2 enricher",
				names, want, relatedalerts.Name)
		}
	}
}

// ⚠️ TestHealthStatusIsUnknownRatherThanInvented.
//
// The promise: `health_status` reports `unknown` for every enricher.
//
// What broke: `display_name`, `cache_hit_rate`, `success_rate`, `p95_duration_ms`
// and `last_run_at` were declared on this DTO, `omitempty` so their absence was
// invisible to every validator, and populated by nothing. Five fields quietly
// asserting they worked is how a resource documented as returning "observed
// health" comes to return none of it. The rolling counters are in the pipeline's
// Prometheus metrics and are not readable from the registry this endpoint serves,
// so an admitted `unknown` is the only honest answer available here.
func TestHealthStatusIsUnknownRatherThanInvented(t *testing.T) {
	t.Parallel()

	f := newEnricherFixture(t)
	resp := f.c.GET("/enrichers").MustStatus(t, http.StatusOK)
	// ⭐ `unknown` is a member of the contract's closed enum, so honesty is
	// expressible rather than a schema violation.
	schema.Assert(t, "listEnrichers", http.StatusOK, resp.Body())

	for _, row := range enricherRows(t, resp) {
		if got, _ := row["health_status"].(string); got != "unknown" {
			t.Fatalf("%v reports health_status %q; the registry cannot know it", row["name"], got)
		}
	}
}

// TestADeploymentWithEnrichmentSwitchedOffAnswersAnEmptyListRatherThanNull.
//
// The promise: a nil registry serves `[]`.
//
// What broke: `null` where an array is declared is a body that fails its own
// schema and crashes a generated client on `.length`, and "enrichment is off" is
// a configuration a deployment is allowed to choose.
func TestADeploymentWithEnrichmentSwitchedOffAnswersAnEmptyListRatherThanNull(t *testing.T) {
	t.Parallel()

	c := apitest.New(NewRouter(nil, clock.New()))
	resp := c.GET("/enrichers").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listEnrichers", http.StatusOK, resp.Body())

	if !strings.Contains(string(resp.Body()), `"data":[]`) {
		t.Fatalf("a deployment with no registry emitted something other than an empty list: %s", resp)
	}
}

// --------------------------------------------------------------- boundaries

// ⛔ TestAnUnauthenticatedCallerReadsNoEnrichers.
//
// The promise: with no principal in the context there is no tenant, so there is
// nothing to serve — the contract's `401 unauthenticated` problem, and the
// registry untouched.
//
// What broke: the registry is not tenant-scoped data, which is exactly the
// argument that would have left this route open. It still names every enricher a
// deployment runs, its version and its budget — a free description of the
// server's internals to anybody who can reach the port.
func TestAnUnauthenticatedCallerReadsNoEnrichers(t *testing.T) {
	t.Parallel()

	apitest.AssertUnauthenticated(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		f := newEnricherFixture(t)
		return f.c, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if f.reg.calls != 0 {
				t.Fatalf("an unauthenticated request reached the registry %d time(s)", f.reg.calls)
			}
		}
	}, []apitest.Route{
		{Op: "listEnrichers", Method: http.MethodGet, Path: "/enrichers"},
	})
}

// TestTheDeclaredEnricherOperationIsTheOneThisPackageServes guards the failure
// mode that made the coverage ratchet necessary: an operation declared in the
// contract that no test ever names, because the list of what to test was kept by
// hand.
func TestTheDeclaredEnricherOperationIsTheOneThisPackageServes(t *testing.T) {
	t.Parallel()

	op := schema.Op(t, "listEnrichers")
	if op.Method != http.MethodGet || op.Path != "/api/v1/enrichers" {
		t.Fatalf("listEnrichers is %s %s; this file drives GET /enrichers", op.Method, op.Path)
	}
	if op.SuccessStatus() != http.StatusOK {
		t.Fatalf("listEnrichers declares success %d, and this file asserts 200", op.SuccessStatus())
	}
	if !op.Declares(http.StatusUnauthorized) {
		t.Fatal("listEnrichers declares no 401, but the handler produces one")
	}
	if len(op.PathParams) != 0 {
		t.Fatalf("listEnrichers has grown path parameters %v; it would need a tenant probe", op.PathParams)
	}
}

// TestAnUnknownQueryParameterIsRefusedWithADeclared400.
//
// SPEC §E.3 requires an unknown query parameter to be refused — a typo'd filter
// that is silently dropped returns a plausible page of the wrong rows — and
// `listEnrichers` refuses one with `400 unknown_parameter`, exactly as
// `listSilences` and every other collection does.
//
// ⭐ THE ASSERTION THAT MATTERS IS `schema.AssertProblem`. Until git-bug ee3ae9c
// this operation's `responses:` block declared only 200, 401, 403, 429, 500 and
// 503, so the 400 the handler really produces was a status no generated client
// was prepared for and no schema validated — and the call below failed at the
// LOOKUP rather than at the validation.
func TestAnUnknownQueryParameterIsRefusedWithADeclared400(t *testing.T) {
	t.Parallel()

	apitest.AssertUnknownQueryParamRefused(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		f := newEnricherFixture(t)
		return f.c, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if f.reg.calls != 0 {
				t.Fatal("a refused query still reached the registry")
			}
		}
	}, []apitest.Route{
		{Op: "listEnrichers", Method: http.MethodGet, Path: "/enrichers?phase=1"},
	})
}

// ------------------------------------------------------------------ helpers

func enricherRows(t *testing.T, resp *apitest.Response) []map[string]any {
	t.Helper()

	data, ok := resp.JSON(t)["data"].([]any)
	if !ok {
		t.Fatalf("the body carries no data array: %s", resp)
	}
	out := make([]map[string]any, 0, len(data))
	for _, item := range data {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("data carries a non-object row: %s", resp)
		}
		out = append(out, row)
	}
	return out
}

// enricherData renders just the `data` member, so two reads can be compared
// without `meta.request_id`, which differs by design.
func enricherData(t *testing.T, resp *apitest.Response) string {
	t.Helper()

	raw, err := json.Marshal(resp.JSON(t)["data"])
	if err != nil {
		t.Fatalf("re-encode data: %v", err)
	}
	return string(raw)
}

func enricherNames(t *testing.T, resp *apitest.Response) []string {
	t.Helper()

	rows := enricherRows(t, resp)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		name, _ := row["name"].(string)
		out = append(out, name)
	}
	return out
}
