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

	// A SECOND source, and the rows mirrored from it. A per-source link is only
	// observable on a page whose rows do not all come from one source: with a
	// single-source fixture, a handler that resolves every row against whichever
	// source it happened to look up first passes every assertion.
	contractOtherSilenceID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	contractOtherSourceID  = uuid.MustParse("66666666-6666-4666-8666-666666666666")

	// A THIRD row, deliberately from `contractSourceID` again, so the batch
	// lookup has something to deduplicate.
	contractRepeatSilenceID = uuid.MustParse("77777777-7777-4777-8777-777777777777")
)

const (
	contractAlertmanagerURL      = "https://alertmanager.example.com"
	contractOtherAlertmanagerURL = "https://alertmanager.eu.example.com"

	contractSourceSilenceID = "b7f3a1c2-9d84-4e15-a0f6-3c8b2d7e1a94"
	contractOtherSilenceUID = "c1e4b2d3-0a95-4f26-b1e7-4d9c3e8f2b05"
	contractRepeatUID       = "d2f5c3e4-1ba6-4037-a2f8-5ead4f903c16"
)

var contractEpoch = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

// ------------------------------------------------------------------- doubles

// fakeSilenceService owns exactly the silences its fixture seeded, and answers
// 404 for every other id. That is what makes the tenant probe honest: the handler
// cannot pass by accident, because the only path to a 200 is an id this tenant
// owns.
type fakeSilenceService struct {
	silences []domain.Silence
	matched  []alertdomain.Alert

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
	return service.ListResult{Silences: f.silences}, nil
}

func (f *fakeSilenceService) Get(
	_ context.Context, _ db.TenantScope, id uuid.UUID,
) (service.Detail, error) {
	f.getCalls++
	if f.getErr != nil {
		return service.Detail{}, f.getErr
	}
	for _, s := range f.silences {
		if s.ID() == id {
			return service.Detail{
				Silence:      s,
				Matched:      f.matched,
				MatchedCount: len(f.matched),
			}, nil
		}
	}
	// ⛔ Anything this tenant does not own is a 404, never a 403 and never
	// somebody else's row.
	return service.Detail{}, errs.NotFound("not_found", "no such resource")
}

// fakeSourceBaseURLs answers ONLY about the ids it was actually asked for, out of
// the roots its fixture declared.
//
// ⛔ IT MUST NOT ANSWER ABOUT ANYTHING ELSE, and an earlier version of this double
// did: it ignored `ids` and returned the one known URL unconditionally, so a
// handler that resolved every row against whichever source it looked up first
// would have passed the whole suite. Honouring `ids` is what makes "null rather
// than guessed" a real assertion — a link can only appear if the handler named
// the silence's OWN source.
type fakeSourceBaseURLs struct {
	byID map[uuid.UUID]string
	err  error

	calls int
	ids   []uuid.UUID
}

func (f *fakeSourceBaseURLs) BaseURLs(
	_ context.Context, _ db.TenantScope, ids []uuid.UUID,
) (map[uuid.UUID]string, error) {
	f.calls++
	f.ids = append(f.ids, ids...)
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[uuid.UUID]string, len(ids))
	for _, id := range ids {
		if base, ok := f.byID[id]; ok {
			out[id] = base
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ fixtures

// contractSilence builds a mirror row upstream could actually have produced —
// through the domain constructor, so the fixture cannot drift into a shape the
// repository could never hand a handler.
func contractSilence(t *testing.T) domain.Silence {
	t.Helper()
	return contractSilenceFrom(t, contractSilenceID, contractSourceID, contractSourceSilenceID)
}

// contractSilenceFrom is contractSilence with the identity varied, for the pages
// that carry rows from more than one source. Everything else is held constant on
// purpose: those tests are about which source each row's link was derived from,
// and a second thing varying alongside it would blur the reading.
func contractSilenceFrom(
	t *testing.T, id, sourceID uuid.UUID, sourceSilenceID string,
) domain.Silence {
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
		ID:              id,
		OrgID:           apitest.OrgID,
		SourceID:        sourceID,
		SourceSilenceID: sourceSilenceID,
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
	svc     *fakeSilenceService
	sources *fakeSourceBaseURLs
	c       *apitest.Client
}

func newSilenceFixture(t *testing.T) *silenceFixture {
	t.Helper()
	return newSilenceFixtureWith(t,
		[]domain.Silence{contractSilence(t)},
		map[uuid.UUID]string{contractSourceID: contractAlertmanagerURL})
}

// newSilenceFixtureWith builds the router over an explicit page and an explicit
// set of resolvable Alertmanager roots, because the deep link is a claim about
// the PAIRING of the two: which row got which root, and which row got none.
// Both halves therefore have to be stated per test rather than assumed.
//
// The clock is the REAL one on purpose: `meta.elapsed_ms` is derived from
// `time.Since(started)`, and a fake epoch would make every success body report an
// elapsed time of several months.
func newSilenceFixtureWith(
	t *testing.T, page []domain.Silence, bases map[uuid.UUID]string,
) *silenceFixture {
	t.Helper()

	svc := &fakeSilenceService{silences: page}
	svc.matched = []alertdomain.Alert{contractMatchedAlert(t)}
	sources := &fakeSourceBaseURLs{byID: bases}

	rt := NewRouter(svc, sources, clock.New())
	return &silenceFixture{svc: svc, sources: sources, c: apitest.New(rt)}
}

// silenceRows is the list page's rows keyed by silence id, so an assertion about
// one row does not silently depend on the order the fixture was written in.
func silenceRows(t *testing.T, resp *apitest.Response, want int) map[uuid.UUID]map[string]any {
	t.Helper()

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != want {
		t.Fatalf("data has %v row(s), want %d: %s", resp.JSON(t)["data"], want, resp)
	}
	out := make(map[uuid.UUID]map[string]any, want)
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("a row is not an object: %s", resp)
		}
		raw, _ := row["id"].(string)
		id, err := uuid.Parse(raw)
		if err != nil {
			t.Fatalf("a row's id is not a uuid: %v", row["id"])
		}
		out[id] = row
	}
	if len(out) != want {
		t.Fatalf("the page carries a duplicate silence id: %s", resp)
	}
	return out
}

// silenceLink is one row's `alertmanager_url`, and reports whether it is set at
// all — the null case is a real answer here, not a missing one.
func silenceLink(t *testing.T, row map[string]any) (string, bool) {
	t.Helper()

	raw, present := row["alertmanager_url"]
	if !present || raw == nil {
		return "", false
	}
	link, ok := raw.(string)
	if !ok {
		t.Fatalf("alertmanager_url = %v, want a string or null", raw)
	}
	return link, true
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

// TestTheAlertmanagerDeepLinkIsBuiltFromTheSilencesOwnSource.
//
// The promise: `alertmanager_url` addresses the Alertmanager the silence was
// mirrored FROM.
//
// What broke: the link used to be a single process-wide URL handed in at
// construction — which in an N-source deployment sends every cluster's silences
// to one cluster's UI — and the composition root supplied it as the empty string,
// so the field was null in every response oto ever served.
func TestTheAlertmanagerDeepLinkIsBuiltFromTheSilencesOwnSource(t *testing.T) {
	t.Parallel()

	f := newSilenceFixture(t)
	resp := f.c.GET("/silences").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSilences", http.StatusOK, resp.Body())

	row := silenceRows(t, resp, 1)[contractSilenceID]
	want := contractAlertmanagerURL + "/#/silences/" + contractSourceSilenceID
	if got, _ := silenceLink(t, row); got != want {
		t.Fatalf("alertmanager_url = %q, want %q", got, want)
	}
	if len(f.sources.ids) != 1 || f.sources.ids[0] != contractSourceID {
		t.Fatalf("the lookup asked about %v, want just the silence's own source", f.sources.ids)
	}
}

// TestEachSilenceOnAPageIsLinkedToItsOwnSourceAndNotItsNeighbours.
//
// The promise: on a page whose rows come from DIFFERENT sources, every row's
// link is derived from the source that row names — never from another row's.
//
// What broke: this is the defect the per-source lookup exists to fix, and the
// only page shape that can catch it. With every fixture row sharing one source,
// a handler that resolved the page against a single base URL — the old
// process-wide one, or simply the first entry it found — is indistinguishable
// from a correct one, which is exactly how the single-URL version survived
// review in the first place.
func TestEachSilenceOnAPageIsLinkedToItsOwnSourceAndNotItsNeighbours(t *testing.T) {
	t.Parallel()

	f := newSilenceFixtureWith(t,
		[]domain.Silence{
			contractSilence(t),
			contractSilenceFrom(t, contractOtherSilenceID, contractOtherSourceID, contractOtherSilenceUID),
		},
		map[uuid.UUID]string{
			contractSourceID:      contractAlertmanagerURL,
			contractOtherSourceID: contractOtherAlertmanagerURL,
		})

	resp := f.c.GET("/silences").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSilences", http.StatusOK, resp.Body())

	rows := silenceRows(t, resp, 2)
	for _, want := range []struct {
		silence uuid.UUID
		link    string
	}{
		{contractSilenceID, contractAlertmanagerURL + "/#/silences/" + contractSourceSilenceID},
		{contractOtherSilenceID, contractOtherAlertmanagerURL + "/#/silences/" + contractOtherSilenceUID},
	} {
		got, ok := silenceLink(t, rows[want.silence])
		if !ok {
			t.Fatalf("silence %s got no link at all, want %q", want.silence, want.link)
		}
		if got != want.link {
			t.Fatalf("silence %s links to %q, want %q — a row sent to another "+
				"cluster's Alertmanager is a 404 mid-incident", want.silence, got, want.link)
		}
	}
}

// TestOneUnresolvableSourceDoesNotCostTheOtherRowsTheirLink.
//
// The promise: the lookup answers about the sources it can vouch for and stays
// silent about the rest, so a partial answer degrades ONE row to null and leaves
// the others linked.
//
// What broke: the alternative failure modes are both worse than a null. Dropping
// the whole page's links because one source is unresolvable punishes every other
// row for it, and filling the gap from a neighbour's base URL sends an operator
// to somebody else's Alertmanager.
func TestOneUnresolvableSourceDoesNotCostTheOtherRowsTheirLink(t *testing.T) {
	t.Parallel()

	f := newSilenceFixtureWith(t,
		[]domain.Silence{
			contractSilence(t),
			contractSilenceFrom(t, contractOtherSilenceID, contractOtherSourceID, contractOtherSilenceUID),
		},
		// ⛔ The second source is ABSENT, not empty: this is what a deleted source,
		// or one whose kind this link's shape does not address, looks like from
		// here.
		map[uuid.UUID]string{contractSourceID: contractAlertmanagerURL})

	resp := f.c.GET("/silences").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSilences", http.StatusOK, resp.Body())

	rows := silenceRows(t, resp, 2)
	want := contractAlertmanagerURL + "/#/silences/" + contractSourceSilenceID
	if got, _ := silenceLink(t, rows[contractSilenceID]); got != want {
		t.Fatalf("alertmanager_url = %q, want %q; one unresolvable source must not "+
			"cost the rows that did resolve their link", got, want)
	}
	if got, ok := silenceLink(t, rows[contractOtherSilenceID]); ok {
		t.Fatalf("a source the lookup said nothing about was linked to %q anyway", got)
	}
}

// TestThePageIsResolvedInOneLookupOfDistinctSourceIDs.
//
// The promise: ONE lookup per page, carrying each source id once, however many
// rows name it.
//
// What broke: a link is a rendering detail, and a rendering detail that issues a
// query per row turns every list in the product into an N+1 — the failure that
// shows up as a slow page under exactly the load an incident produces.
func TestThePageIsResolvedInOneLookupOfDistinctSourceIDs(t *testing.T) {
	t.Parallel()

	f := newSilenceFixtureWith(t,
		// ⭐ Three rows, TWO sources: the repeat is what makes the deduplication
		// observable rather than a property of a page that never repeats anything.
		[]domain.Silence{
			contractSilence(t),
			contractSilenceFrom(t, contractOtherSilenceID, contractOtherSourceID, contractOtherSilenceUID),
			contractSilenceFrom(t, contractRepeatSilenceID, contractSourceID, contractRepeatUID),
		},
		map[uuid.UUID]string{
			contractSourceID:      contractAlertmanagerURL,
			contractOtherSourceID: contractOtherAlertmanagerURL,
		})

	resp := f.c.GET("/silences").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSilences", http.StatusOK, resp.Body())

	if f.sources.calls != 1 {
		t.Fatalf("the source lookup ran %d time(s), want 1 for the page", f.sources.calls)
	}
	want := []uuid.UUID{contractSourceID, contractOtherSourceID}
	if len(f.sources.ids) != len(want) {
		t.Fatalf("the lookup asked about %v, want each source once: %v", f.sources.ids, want)
	}
	for i, id := range want {
		if f.sources.ids[i] != id {
			t.Fatalf("the lookup asked about %v, want %v in first-seen order", f.sources.ids, want)
		}
	}
	// The dedup did not cost the repeated row its link.
	rows := silenceRows(t, resp, 3)
	link := contractAlertmanagerURL + "/#/silences/" + contractRepeatUID
	if got, _ := silenceLink(t, rows[contractRepeatSilenceID]); got != link {
		t.Fatalf("the repeated source's second row linked to %q, want %q", got, link)
	}
}

// TestTheAlertmanagerDeepLinkIsNullRatherThanGuessedWhenTheSourceResolvesToNothing.
//
// The promise: `alertmanager_url` is null when the silence's source carries no
// base URL — or has been deleted, or is not an Alertmanager at all, or the lookup
// failed — and never a fabricated one.
//
// What broke: a guessed link is worse than none. An operator who clicks it during
// an incident and lands on a 404 has lost the one silence affordance v1 offers,
// and has lost it at the worst possible moment.
func TestTheAlertmanagerDeepLinkIsNullRatherThanGuessedWhenTheSourceResolvesToNothing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		bases map[uuid.UUID]string
		err   error
	}{
		{
			name:  "the source carries no base URL",
			bases: map[uuid.UUID]string{contractSourceID: ""},
		},
		{
			// ⛔ The kind guard, seen from this side of the port. `/#/silences/<id>`
			// is Alertmanager's own console; a Grafana-backed source keeps its
			// silences at `/alerting/silences`, so the composition root refuses to
			// vouch for it and the id comes back unanswered. This layer never learns
			// the kind — it may not name `sources/domain` — and does not need to.
			name:  "the source is a Grafana, whose silences do not live at this path",
			bases: map[uuid.UUID]string{},
		},
		{
			name: "the lookup itself failed",
			err:  errs.New(errs.KindInternal, "sources_unavailable", "the source registry is unavailable"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newSilenceFixtureWith(t, []domain.Silence{contractSilence(t)}, tc.bases)
			f.sources.err = tc.err

			// ⚠️ A source lookup that fails does not fail the request: the rows are
			// what was asked for and the link is an affordance on top of them.
			resp := f.c.GET("/silences").MustStatus(t, http.StatusOK)
			// ⭐ Still a legal SilenceListResponse: `alertmanager_url` is nullable,
			// so "no link" is expressible without breaking the schema.
			schema.Assert(t, "listSilences", http.StatusOK, resp.Body())

			if !strings.Contains(string(resp.Body()), `"alertmanager_url":null`) {
				t.Fatalf("a source that resolved to nothing emitted a link anyway: %s", resp)
			}
		})
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

	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		return newSilenceFixture(t).c, nil
	}, []apitest.Route{
		{Name: "an id owned by another org", Op: "getSilence",
			Method: http.MethodGet, Path: "/silences/" + apitest.StrangerID.String()},
		{Name: "an id that is not a uuid at all", Op: "getSilence",
			Method: http.MethodGet, Path: "/silences/banana"},
		{Name: "the nil uuid", Op: "getSilence",
			Method: http.MethodGet, Path: "/silences/00000000-0000-0000-0000-000000000000"},
	})
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

	apitest.AssertUnknownQueryParamRefused(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		f := newSilenceFixture(t)
		return f.c, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if f.svc.listCalls != 0 {
				t.Fatal("a refused query still reached the service")
			}
		}
	}, []apitest.Route{
		{Op: "listSilences", Method: http.MethodGet, Path: "/silences?stat=active"},
	})
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

	routes := []apitest.Route{
		{Op: "listSilences", Method: http.MethodGet, Path: "/silences"},
		{Op: "getSilence", Method: http.MethodGet, Path: "/silences/" + contractSilenceID.String()},
	}

	apitest.AssertUnauthenticated(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		f := newSilenceFixture(t)
		return f.c, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if f.svc.listCalls != 0 || f.svc.getCalls != 0 {
				t.Fatal("an unauthenticated request reached the service")
			}
		}
	}, routes)
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
