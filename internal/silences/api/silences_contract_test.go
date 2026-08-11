package api

// THE SILENCES TRANSPORT, CHECKED AGAINST THE CONTRACT ITSELF.
//
// ⭐ NOTHING HERE RE-STATES A RESPONSE SHAPE BY HAND. Every success body is
// handed to `schema.Assert`, which compiles the JSON Schema
// `api/openapi/openapi.yaml` declares for that operationId and that status and
// validates the bytes the handler actually wrote. A test that spelled the shape
// out a second time would be a second copy of the contract, and a second copy
// drifts exactly the way the DTOs did — which is how the conformance audit found
// a `delivery_summary` missing two required members with a green test suite
// beside it.
//
// The properties this file protects, and what broke when each did not hold:
//
//   - the {data,page,meta} envelope, with `meta.request_id` present — it is
//     REQUIRED by the contract and `httpx.Meta` omits it when empty, so a
//     response produced outside the request-id middleware fails its own schema;
//   - a silence in another tenant answers 404 and not 403 — a 403 confirms the
//     row exists somewhere, which is a cross-tenant existence oracle;
//   - an unknown query parameter is REFUSED — a typo'd `?stat=active` that is
//     silently dropped returns a plausible page of the wrong rows.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/silences/domain"
	"github.com/thulasiram/oto/internal/silences/service"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// The fixture ids are CONSTANTS, never freshly generated: a tenant-scoping
// failure has to be reproducible from the failure message, and the whole point of
// the probe is that the message names the exact id that leaked.
var (
	contractSilenceID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	contractSourceID  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	contractAlertID   = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	contractClusterID = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

const contractAlertmanagerURL = "https://alertmanager.example.com"

var contractEpoch = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

// ------------------------------------------------------------------- doubles

// fakeSilenceService owns exactly one silence, and answers 404 for every other
// id. That is what makes the tenant probe honest: the handler cannot pass by
// accident, because the only path to a 200 is the id this tenant owns.
type fakeSilenceService struct {
	silence domain.Silence
	matched []alertdomain.Alert

	// listErr and getErr force the failure branches.
	listErr error
	getErr  error

	listCalls int
	getCalls  int
}

func (f *fakeSilenceService) List(
	_ context.Context, _ db.TenantScope, _ domain.Filter, _ db.Keyset,
) (service.ListResult, error) {
	f.listCalls++
	if f.listErr != nil {
		return service.ListResult{}, f.listErr
	}
	return service.ListResult{Silences: []domain.Silence{f.silence}}, nil
}

func (f *fakeSilenceService) Get(
	_ context.Context, _ db.TenantScope, id uuid.UUID,
) (service.Detail, error) {
	f.getCalls++
	if f.getErr != nil {
		return service.Detail{}, f.getErr
	}
	// ⛔ Anything this tenant does not own is a 404, never a 403 and never
	// somebody else's row.
	if id != f.silence.ID() {
		return service.Detail{}, errs.NotFound("not_found", "no such resource")
	}
	return service.Detail{
		Silence:      f.silence,
		Matched:      f.matched,
		MatchedCount: len(f.matched),
	}, nil
}

// ------------------------------------------------------------------ fixtures

// contractSilence builds a mirror row upstream could actually have produced —
// through the domain constructor, so the fixture cannot drift into a shape the
// repository could never hand a handler.
func contractSilence(t *testing.T) domain.Silence {
	t.Helper()

	byNamespace, err := domain.NewMatcher("namespace", "prod", false, true)
	if err != nil {
		t.Fatalf("build the namespace matcher: %v", err)
	}
	byName, err := domain.NewMatcher("alertname", "KubePod.*", true, true)
	if err != nil {
		t.Fatalf("build the alertname matcher: %v", err)
	}

	s, err := domain.New(domain.Params{
		ID:              contractSilenceID,
		OrgID:           apitest.OrgID,
		SourceID:        contractSourceID,
		SourceSilenceID: "b7f3a1c2-9d84-4e15-a0f6-3c8b2d7e1a94",
		Matchers:        []domain.Matcher{byNamespace, byName},
		StartsAt:        contractEpoch,
		EndsAt:          contractEpoch.Add(2 * time.Hour),
		CreatedBy:       "priya@example.com",
		Comment:         "Deploy window, expected error spike",
		Annotations:     map[string]string{"ticket": "OPS-1421"},
		State:           domain.StateActive,
		SourceUpdatedAt: contractEpoch.Add(time.Minute),
		MirroredAt:      contractEpoch.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("build the mirrored silence: %v", err)
	}
	return s
}

// contractMatchedAlert builds the alert the silence is believed to cover. It goes
// through `alerts/domain` too, so `alert_key` and `cluster_key` are the real
// derivations rather than strings that merely look like them — the contract
// asserts both patterns.
func contractMatchedAlert(t *testing.T) alertdomain.Alert {
	t.Helper()

	labels, err := alertdomain.NewLabelSet(map[string]string{
		"alertname": "KubePodCrashLooping",
		"severity":  "critical",
		"namespace": "prod",
	})
	if err != nil {
		t.Fatalf("build the label set: %v", err)
	}
	clusterKey, err := alertdomain.NewClusterKey("prod-eu")
	if err != nil {
		t.Fatalf("build the cluster key: %v", err)
	}

	a, err := alertdomain.NewAlert(alertdomain.AlertParams{
		ID:                contractAlertID,
		OrgID:             apitest.OrgID,
		ClusterID:         contractClusterID,
		Key:               alertdomain.ComputeAlertKey(apitest.OrgID, clusterKey, labels, nil),
		Fingerprint:       alertdomain.ComputeSourceFingerprint(labels),
		ClusterKey:        clusterKey,
		Labels:            labels,
		State:             alertdomain.StateFiring,
		AckState:          alertdomain.AckStateUnacked,
		FirstSeenAt:       contractEpoch,
		LastSeenAt:        contractEpoch.Add(time.Minute),
		LastStateChangeAt: contractEpoch,
		TotalOccurrences:  1,
	})
	if err != nil {
		t.Fatalf("build the matched alert: %v", err)
	}
	return a
}

type silenceFixture struct {
	svc *fakeSilenceService
	c   *apitest.Client
}

func newSilenceFixture(t *testing.T) *silenceFixture {
	t.Helper()
	return newSilenceFixtureAt(t, contractAlertmanagerURL)
}

// newSilenceFixtureAt builds the router with an explicit Alertmanager base URL, so
// the "no deep link configured" case is reachable.
//
// The clock is the REAL one on purpose: `meta.elapsed_ms` is derived from
// `time.Since(started)`, and a fake epoch would make every success body report an
// elapsed time of several months.
func newSilenceFixtureAt(t *testing.T, alertmanagerURL string) *silenceFixture {
	t.Helper()

	svc := &fakeSilenceService{silence: contractSilence(t)}
	svc.matched = []alertdomain.Alert{contractMatchedAlert(t)}

	rt := NewRouter(svc, alertmanagerURL, clock.New())
	return &silenceFixture{svc: svc, c: apitest.New(rt)}
}

// ------------------------------------------------------------- happy paths

// TestListSilencesAnswersTheEnvelopeTheContractDeclares.
//
// The promise: `GET /api/v1/silences` returns {data, page, meta}, every row a
// `SilenceDTO` with its required members, `meta.request_id` present.
//
// What broke when it did not hold: the audit found operations answering a bare
// array and operations answering an envelope missing `meta`, and a client cannot
// tell which one it is looking at until it crashes on the difference.
func TestListSilencesAnswersTheEnvelopeTheContractDeclares(t *testing.T) {
	t.Parallel()

	f := newSilenceFixture(t)
	resp := f.c.GET("/silences").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSilences", http.StatusOK, resp.Body())

	if f.svc.listCalls != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", f.svc.listCalls)
	}
	if ct := resp.Header("Content-Type"); ct != apitest.ContentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, apitest.ContentTypeJSON)
	}
}

// TestGetSilenceAnswersTheDetailShapeTheContractDeclares.
//
// The promise: the detail body carries the silence AND the alerts oto believes it
// covers, each one an `AlertRefDTO` whose `alert_key` and `cluster_key` match
// their contract patterns.
//
// What broke: a DTO that renders a derived key by hand rather than through the
// domain type is a DTO that renders an internal integer id the day somebody
// changes the derivation, and `format`/`pattern` are the only checks that notice.
func TestGetSilenceAnswersTheDetailShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	f := newSilenceFixture(t)
	resp := f.c.GET("/silences/"+contractSilenceID.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getSilence", http.StatusOK, resp.Body())

	body := resp.JSON(t)
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("the detail body has no data object: %s", resp)
	}
	matched, ok := data["matched_alerts"].([]any)
	if !ok || len(matched) != 1 {
		t.Fatalf("matched_alerts = %v, want the one alert the fixture matches: %s", data["matched_alerts"], resp)
	}
	if count, _ := data["matched_count"].(float64); int(count) != 1 {
		t.Fatalf("matched_count = %v, want 1; the count and the list must agree", data["matched_count"])
	}
}

// TestTheAlertmanagerDeepLinkIsNullRatherThanGuessedWhenNoBaseURLIsConfigured.
//
// The promise: `alertmanager_url` is null when the deployment configured no
// Alertmanager base URL, and never a fabricated one.
//
// What broke: a guessed link is worse than none. An operator who clicks it during
// an incident and lands on a 404 has lost the one silence affordance v1 offers,
// and has lost it at the worst possible moment.
func TestTheAlertmanagerDeepLinkIsNullRatherThanGuessedWhenNoBaseURLIsConfigured(t *testing.T) {
	t.Parallel()

	f := newSilenceFixtureAt(t, "")
	resp := f.c.GET("/silences").MustStatus(t, http.StatusOK)
	// ⭐ Still a legal SilenceListResponse: `alertmanager_url` is nullable, so
	// "no link" is expressible without breaking the schema.
	schema.Assert(t, "listSilences", http.StatusOK, resp.Body())

	if !strings.Contains(string(resp.Body()), `"alertmanager_url":null`) {
		t.Fatalf("an unconfigured deployment emitted a link anyway: %s", resp)
	}
}

// --------------------------------------------------------------- boundaries

// ⛔ TestASilenceOutsideTheCallersTenantIsANotFound.
//
// The promise: every id-addressed operation answers 404 for an id it does not
// own — never 403, never another tenant's row, never a 500.
//
// What broke: a 403 confirms the id exists somewhere, which turns the id space
// into a cross-tenant existence oracle. v1's only cause of 403 is cross-org
// access, which is precisely the case this must not distinguish from "no such
// thing".
func TestASilenceOutsideTheCallersTenantIsANotFound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op   string
		path string
	}{
		{
			name: "an id owned by another org",
			op:   "getSilence",
			path: "/silences/" + apitest.StrangerID.String(),
		},
		{
			name: "an id that is not a uuid at all",
			op:   "getSilence",
			path: "/silences/banana",
		},
		{
			name: "the nil uuid",
			op:   "getSilence",
			path: "/silences/00000000-0000-0000-0000-000000000000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newSilenceFixture(t)
			resp := f.c.GET(tc.path).MustStatus(t, http.StatusNotFound)
			schema.AssertProblem(t, tc.op, http.StatusNotFound, resp.Body())

			if ct := resp.Header("Content-Type"); !strings.Contains(ct, "problem+json") {
				t.Fatalf("Content-Type = %q, want application/problem+json", ct)
			}
			// ⚠️ The refusal says nothing about the other tenant.
			if strings.Contains(string(resp.Body()), apitest.OtherOrgID.String()) {
				t.Fatalf("the 404 names the owning org: %s", resp)
			}
		})
	}
}

// TestAnUnknownQueryParameterIsRefusedRatherThanIgnored — SPEC §E.3.
//
// The promise: `400 unknown_parameter`, with `violations[]` naming the parameter.
//
// What broke: a silently dropped `?stat=active` returns a full page of every
// silence and looks exactly like a filtered one, which is how the UI and the API
// stop agreeing with each other without anybody noticing.
func TestAnUnknownQueryParameterIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	f := newSilenceFixture(t)
	resp := f.c.GET("/silences?stat=active").MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "listSilences", http.StatusBadRequest, resp.Body())

	p := resp.MustViolate(t, "stat")
	if p.Code != "unknown_parameter" {
		t.Fatalf("code = %q, want unknown_parameter", p.Code)
	}
	if f.svc.listCalls != 0 {
		t.Fatal("a refused query still reached the service")
	}
}

// TestAMalformedFilterIsRefusedWithTheFieldNamed.
//
// The promise: a `source_id` that is not a UUID is a 422 whose `violations[]`
// names `source_id` and carries a machine code a form can branch on.
//
// What broke: a refusal that carries only prose leaves a form with nothing to
// highlight, so the caller retries a different wrong value.
func TestAMalformedFilterIsRefusedWithTheFieldNamed(t *testing.T) {
	t.Parallel()

	f := newSilenceFixture(t)
	resp := f.c.GET("/silences?source_id=not-a-uuid").
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "listSilences", http.StatusUnprocessableEntity, resp.Body())

	p := resp.MustViolate(t, "source_id")
	if p.Status != http.StatusUnprocessableEntity {
		t.Fatalf("the problem says %d, the status line said 422", p.Status)
	}
	if f.svc.listCalls != 0 {
		t.Fatal("an invalid filter still reached the service")
	}
}

// TestAnUnauthenticatedCallerGetsTheSame401OnEverySilenceRoute.
//
// The promise: with no principal in the context there is no tenant, so there is
// nothing to read — one code, one shape, on both routes.
//
// What broke: a handler that resolved its scope from anything other than
// `authn.Scope` would serve rows to a caller who proved nothing. Both handlers
// re-derive it rather than trusting that a middleware ran, because "the
// middleware guarantees it" stops being true the first time a route is mounted
// somewhere else.
func TestAnUnauthenticatedCallerGetsTheSame401OnEverySilenceRoute(t *testing.T) {
	t.Parallel()

	routes := []struct {
		op   string
		path string
	}{
		{"listSilences", "/silences"},
		{"getSilence", "/silences/" + contractSilenceID.String()},
	}

	for _, route := range routes {
		t.Run(route.op, func(t *testing.T) {
			t.Parallel()

			f := newSilenceFixture(t)
			resp := f.c.Anonymous().GET(route.path).MustStatus(t, http.StatusUnauthorized)
			schema.AssertProblem(t, route.op, http.StatusUnauthorized, resp.Body())

			if code := resp.Problem(t).Code; code != "unauthenticated" {
				t.Fatalf("code = %q, want unauthenticated", code)
			}
			if f.svc.listCalls != 0 || f.svc.getCalls != 0 {
				t.Fatal("an unauthenticated request reached the service")
			}
		})
	}
}

// TestTheDeclaredSilenceOperationsAreTheOnesThisPackageServes is a guard against
// the failure mode that made the audit necessary: an operation declared in the
// contract that no test ever names, because the list of what to test was kept by
// hand.
func TestTheDeclaredSilenceOperationsAreTheOnesThisPackageServes(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"listSilences", "getSilence"} {
		op := schema.Op(t, id)
		if op.SuccessStatus() != http.StatusOK {
			t.Fatalf("%s declares success %d, and this file asserts 200", id, op.SuccessStatus())
		}
		if !op.Declares(http.StatusUnauthorized) {
			t.Fatalf("%s declares no 401, but the handler can produce one", id)
		}
	}
	// ⛔ Read-only, forever (SPEC R3). A write path into somebody's cluster is
	// safety-critical, and a silence oto created would suppress nothing.
	for _, id := range []string{"createSilence", "deleteSilence", "expireSilence"} {
		if _, err := schema.Lookup(id); err == nil {
			t.Fatalf("the contract has grown a %q operation; oto has no write path into your cluster", id)
		}
	}
}
