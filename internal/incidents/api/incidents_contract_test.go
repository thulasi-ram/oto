package api

// THE INCIDENTS TRANSPORT, CHECKED AGAINST THE CONTRACT ITSELF (ADR 0052,
// git-bug b2672a1).
//
// ⭐ NO RESPONSE SHAPE IS RE-STATED BY HAND. Every success body goes through
// `schema.Assert` for its operationId and status, and every refusal through
// `schema.AssertProblem`, so this file cannot become a second copy of the
// contract that drifts from the first.
//
// The properties this file protects:
//
//   - every one of the six operations answers the shape the contract declares;
//   - the at-most-one refusal is a 409 `case_in_incident` whose `detail` POINTS AT
//     THE MOVE, because a refusal without the way forward is how a rule gets routed
//     around;
//   - `state` is on every response and on NO request: there is no body field and no
//     route that could set it (ADR 0052 §3);
//   - a membership verb needs a human: a system principal is a 403, because an
//     Incident decision attributed to nobody cannot be asked about;
//   - another org's Incident number is a 404, never a 403.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

var (
	contractIncidentID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	contractCaseID     = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	contractAlertID    = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	contractRemovedID  = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	contractEpoch      = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
)

const (
	contractNumber      = 4
	contractOtherNumber = 7
)

// fakeIncidents owns exactly ONE Incident, #4, in apitest.OrgID, and answers 404
// for every other number and for every other org — so the only route to a 200 is a
// number this tenant drew, and the tenant probe cannot pass by accident.
type fakeIncidents struct {
	mu sync.Mutex

	// refuse, when set, is what every write answers instead of succeeding.
	refuse error

	calls   []string
	by      []domain.Attribution
	moves   []int64
	holding []uuid.UUID
	// listed is the filter every unfiltered list read reached the service with.
	listed []domain.ListFilter
}

func (f *fakeIncidents) record(call string, by domain.Attribution) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.by = append(f.by, by)
}

func (f *fakeIncidents) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeIncidents) owns(s db.TenantScope, number int64) bool {
	return s.OrgID() == apitest.OrgID && number == contractNumber
}

// contractDetail is the one Incident the fake owns. It panics rather than taking
// a `testing.TB` because the fake's methods build it with no test in hand, and a
// fixed literal that fails to construct is a broken fixture, not a test outcome.
func contractDetail() domain.Detail {
	by, err := domain.Human(apitest.UserID, "Ada Lovelace")
	if err != nil {
		panic("contract fixture: " + err.Error())
	}
	return domain.Detail{
		Incident: domain.Incident{
			ID:              contractIncidentID,
			Number:          contractNumber,
			DrawnAt:         contractEpoch,
			DrawnBy:         by,
			MemberCount:     1,
			OpenMemberCount: 1,
			Alertnames:      []string{"KubePodCrashLooping"},
		},
		Members: []domain.Member{
			{
				CaseID: contractCaseID, CaseNumber: 412, CaseState: kernel.CaseOpen,
				AlertID: contractAlertID, Alertname: "KubePodCrashLooping",
				Labels:  map[string]string{"alertname": "KubePodCrashLooping", "namespace": "prod"},
				AddedAt: contractEpoch, AddedBy: by,
			},
			{
				// A TOMBSTONE, so every response carries the nullable members set
				// as well as unset — `removed_at`, `removed_by_label` and
				// `moved_to_number` are only checked if something renders them.
				CaseID: contractRemovedID, CaseNumber: 413, CaseState: kernel.CaseClosed,
				AlertID: contractAlertID, Alertname: "KubePodCrashLooping",
				Labels:  map[string]string{"alertname": "KubePodCrashLooping"},
				AddedAt: contractEpoch, AddedBy: by,
				RemovedAt: contractEpoch.Add(time.Minute), RemovedByLabel: "Ada Lovelace",
				MovedToNumber: contractOtherNumber,
			},
		},
	}
}

func (f *fakeIncidents) List(
	_ context.Context, s db.TenantScope, _ db.Keyset, lf domain.ListFilter,
) ([]domain.Incident, db.Cursor, error) {
	f.mu.Lock()
	f.listed = append(f.listed, lf)
	f.mu.Unlock()
	if s.OrgID() != apitest.OrgID {
		return nil, db.Cursor{}, nil
	}
	return []domain.Incident{contractDetail().Incident}, db.Cursor{}, nil
}

// HoldingCase answers #4 for the one current member the fake knows, and nothing
// for any other Case or any other org.
func (f *fakeIncidents) HoldingCase(_ context.Context, s db.TenantScope, caseID uuid.UUID) ([]domain.Incident, error) {
	f.mu.Lock()
	f.holding = append(f.holding, caseID)
	f.mu.Unlock()
	if s.OrgID() != apitest.OrgID || caseID != contractCaseID {
		return []domain.Incident{}, nil
	}
	return []domain.Incident{contractDetail().Incident}, nil
}

func (f *fakeIncidents) Get(_ context.Context, s db.TenantScope, number int64) (domain.Detail, error) {
	if !f.owns(s, number) {
		return domain.Detail{}, domain.NotFound()
	}
	return contractDetail(), nil
}

func (f *fakeIncidents) Draw(_ context.Context, _ db.TenantScope, _ []uuid.UUID, by domain.Attribution) (domain.Detail, error) {
	f.record("draw", by)
	if f.refuse != nil {
		return domain.Detail{}, f.refuse
	}
	return contractDetail(), nil
}

func (f *fakeIncidents) Add(_ context.Context, s db.TenantScope, number int64, _ uuid.UUID, by domain.Attribution) (domain.Detail, error) {
	f.record("add", by)
	if !f.owns(s, number) {
		return domain.Detail{}, domain.NotFound()
	}
	if f.refuse != nil {
		return domain.Detail{}, f.refuse
	}
	return contractDetail(), nil
}

func (f *fakeIncidents) Remove(_ context.Context, s db.TenantScope, number int64, _ uuid.UUID, by domain.Attribution) (domain.Detail, error) {
	f.record("remove", by)
	if !f.owns(s, number) {
		return domain.Detail{}, domain.NotFound()
	}
	return contractDetail(), nil
}

func (f *fakeIncidents) Move(_ context.Context, s db.TenantScope, from, to int64, _ uuid.UUID, by domain.Attribution) (domain.Detail, error) {
	f.record("move", by)
	f.mu.Lock()
	f.moves = append(f.moves, from, to)
	f.mu.Unlock()
	if from == to {
		return domain.Detail{}, domain.MoveToSelf()
	}
	if !f.owns(s, from) {
		return domain.Detail{}, domain.NotFound()
	}
	return contractDetail(), nil
}

func newIncidentClient(t *testing.T) (*fakeIncidents, *apitest.Client) {
	t.Helper()
	f := &fakeIncidents{}
	return f, apitest.New(NewRouter(f, clock.New()))
}

// ------------------------------------------------------------- happy paths

// TestEveryIncidentOperationAnswersItsContractShape drives all six operations
// once, as a member, and validates the bytes against the declared schema.
func TestEveryIncidentOperationAnswersItsContractShape(t *testing.T) {
	t.Parallel()

	f, c := newIncidentClient(t)
	caseID := contractCaseID.String()

	resp := c.GET("/incidents").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listIncidents", http.StatusOK, resp.Body())

	resp = c.POST(t, "/incidents", map[string]any{"case_ids": []string{caseID}}).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createIncident", http.StatusCreated, resp.Body())

	resp = c.GET("/incidents/4").MustStatus(t, http.StatusOK)
	schema.Assert(t, "getIncident", http.StatusOK, resp.Body())

	resp = c.POST(t, "/incidents/4/cases", map[string]any{"case_id": caseID}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "addIncidentCase", http.StatusOK, resp.Body())

	resp = c.Raw(http.MethodPost, "/incidents/4/cases/"+caseID+"/remove", "", "").MustStatus(t, http.StatusOK)
	schema.Assert(t, "removeIncidentCase", http.StatusOK, resp.Body())

	resp = c.POST(t, "/incidents/4/cases/"+caseID+"/move", map[string]any{"to_number": contractOtherNumber}).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "moveIncidentCase", http.StatusOK, resp.Body())

	if got := strings.Join(f.calls, ","); got != "draw,add,remove,move" {
		t.Fatalf("the service saw %q, want draw,add,remove,move", got)
	}
	// ⭐ THE PATH NAMES THE SOURCE AND THE BODY THE DESTINATION, and the handler
	// must not swap them: a move is a statement about where the Case is NOW.
	if len(f.moves) != 2 || f.moves[0] != contractNumber || f.moves[1] != contractOtherNumber {
		t.Fatalf("move reached the service as %v, want [%d %d]", f.moves, contractNumber, contractOtherNumber)
	}
}

// TestEveryVerbIsAttributedToTheHumanWhoCalled — ADR 0052 §2: an Incident has an
// answer to "who decided?", and on this path it is the caller, frozen as a label.
func TestEveryVerbIsAttributedToTheHumanWhoCalled(t *testing.T) {
	t.Parallel()

	f, c := newIncidentClient(t)
	c.POST(t, "/incidents", map[string]any{"case_ids": []string{contractCaseID.String()}}).
		MustStatus(t, http.StatusCreated)

	if len(f.by) != 1 {
		t.Fatalf("the service saw %d attribution(s), want 1", len(f.by))
	}
	by := f.by[0]
	if !by.IsHuman() || by.Label() != "Ada Lovelace" || by.UserID() != apitest.UserID {
		t.Fatalf("the draw was attributed to (human=%v, label=%q, user=%s), want Ada Lovelace's session",
			by.IsHuman(), by.Label(), by.UserID())
	}
}

// TestTheStateIsDerivedAndNothingCanSetIt — ADR 0052 §3.
//
// It holds the rule from both ends: the response carries `state` and the count it
// is derived from, and NO request body admits a `state` — an unknown field is a
// 422, so a client that tries is told rather than silently ignored.
func TestTheStateIsDerivedAndNothingCanSetIt(t *testing.T) {
	t.Parallel()

	_, c := newIncidentClient(t)
	resp := c.GET("/incidents/4").MustStatus(t, http.StatusOK)

	var body struct {
		Data struct {
			State           string `json:"state"`
			OpenMemberCount int    `json:"open_member_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.State != "active" || body.Data.OpenMemberCount != 1 {
		t.Fatalf("state = %q with %d open member(s), want active with 1",
			body.Data.State, body.Data.OpenMemberCount)
	}

	for _, probe := range []struct {
		op, path string
		body     map[string]any
	}{
		{"createIncident", "/incidents", map[string]any{"case_ids": []string{contractCaseID.String()}, "state": "quiet"}},
		{"addIncidentCase", "/incidents/4/cases", map[string]any{"case_id": contractCaseID.String(), "state": "quiet"}},
		{"moveIncidentCase", "/incidents/4/cases/" + contractCaseID.String() + "/move",
			map[string]any{"to_number": contractOtherNumber, "state": "quiet"}},
	} {
		resp := c.POST(t, probe.path, probe.body).MustStatus(t, http.StatusUnprocessableEntity)
		schema.AssertProblem(t, probe.op, http.StatusUnprocessableEntity, resp.Body())
	}
}

// ------------------------------------------------------------- refusals

// TestTheAtMostOneRefusalPointsAtTheMove — ADR 0052 §4. The 409 is the contract's
// Conflict problem, its code is `case_in_incident`, and its detail names the
// holding Incident and the move request.
func TestTheAtMostOneRefusalPointsAtTheMove(t *testing.T) {
	t.Parallel()

	f, c := newIncidentClient(t)
	f.refuse = domain.CaseInIncident(
		domain.CaseRef{ID: contractCaseID, Number: 412, AlertID: contractAlertID},
		domain.Ref{ID: contractIncidentID, Number: contractOtherNumber})

	for _, probe := range []struct {
		op, path string
		body     map[string]any
	}{
		{"createIncident", "/incidents", map[string]any{"case_ids": []string{contractCaseID.String()}}},
		{"addIncidentCase", "/incidents/4/cases", map[string]any{"case_id": contractCaseID.String()}},
	} {
		resp := c.POST(t, probe.path, probe.body).MustStatus(t, http.StatusConflict)
		schema.AssertProblem(t, probe.op, http.StatusConflict, resp.Body())
		p := resp.Problem(t)
		if p.Code != "case_in_incident" {
			t.Fatalf("%s: code = %q, want case_in_incident", probe.op, p.Code)
		}
		want := "/api/v1/incidents/7/cases/" + contractCaseID.String() + "/move"
		if !strings.Contains(p.Detail, want) || !strings.Contains(p.Detail, "Incident #7") {
			t.Fatalf("%s: detail %q does not point at the holding Incident and %s", probe.op, p.Detail, want)
		}
	}
}

// TestAMoveToItselfIsRefused — a move whose destination is its source is a 422
// naming `to_number`, not a no-op that writes a tombstone and a membership.
func TestAMoveToItselfIsRefused(t *testing.T) {
	t.Parallel()

	_, c := newIncidentClient(t)
	resp := c.POST(t, "/incidents/4/cases/"+contractCaseID.String()+"/move",
		map[string]any{"to_number": contractNumber}).MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "moveIncidentCase", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "to_number")
}

// TestADrawNeedsAtLeastOneCase — an Incident is a set of ONE or more Cases.
func TestADrawNeedsAtLeastOneCase(t *testing.T) {
	t.Parallel()

	f, c := newIncidentClient(t)
	resp := c.POST(t, "/incidents", map[string]any{"case_ids": []string{}}).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "createIncident", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "case_ids")
	if f.callCount() != 0 {
		t.Fatalf("the service was reached %d time(s) by an empty draw", f.callCount())
	}
}

// TestAMembershipVerbNeedsAHuman — a system principal is refused before the
// service is reached.
func TestAMembershipVerbNeedsAHuman(t *testing.T) {
	t.Parallel()

	f, c := newIncidentClient(t)
	resp := c.As(apitest.Machine()).
		POST(t, "/incidents", map[string]any{"case_ids": []string{contractCaseID.String()}}).
		MustStatus(t, http.StatusForbidden)
	schema.AssertProblem(t, "createIncident", http.StatusForbidden, resp.Body())
	if f.callCount() != 0 {
		t.Fatalf("a system principal reached the service %d time(s)", f.callCount())
	}
}

// --------------------------------------------------------- shared probes

func incidentRoutes() []apitest.Route {
	caseID := contractCaseID.String()
	return []apitest.Route{
		{Op: "listIncidents", Method: http.MethodGet, Path: "/incidents"},
		{Op: "createIncident", Method: http.MethodPost, Path: "/incidents", Body: `{"case_ids":["` + caseID + `"]}`},
		{Op: "getIncident", Method: http.MethodGet, Path: "/incidents/4"},
		{Op: "addIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases", Body: `{"case_id":"` + caseID + `"}`},
		{Op: "removeIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases/" + caseID + "/remove"},
		{Op: "moveIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases/" + caseID + "/move",
			Body: `{"to_number":7}`},
	}
}

func incidentWorld(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
	t.Helper()
	_, c := newIncidentClient(t)
	return c, nil
}

func TestEveryIncidentRouteRefusesAnAnonymousCaller(t *testing.T) {
	t.Parallel()
	apitest.AssertUnauthenticated(t, incidentWorld, incidentRoutes())
}

// TestAnotherOrgsIncidentIsA404 — the same numbers, asked by a member of
// apitest.OtherOrgID. Every org counts from 1, so #4 exists in both; the caller's
// #4 is not this one, and the answer must be indistinguishable from "never drawn".
// The Case in the path is apitest.StrangerID: another org's Case is never a member
// of anything here.
func TestAnotherOrgsIncidentIsA404(t *testing.T) {
	t.Parallel()

	stranger := apitest.StrangerID.String()
	routes := []apitest.Route{
		{Op: "getIncident", Method: http.MethodGet, Path: "/incidents/4"},
		{Op: "addIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases", Body: `{"case_id":"` + stranger + `"}`},
		{Op: "removeIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases/" + stranger + "/remove"},
		{Op: "moveIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases/" + stranger + "/move",
			Body: `{"to_number":7}`},
		{Op: "getIncident", Name: "a number that is not a number", Method: http.MethodGet, Path: "/incidents/banana"},
		{Op: "getIncident", Name: "number zero", Method: http.MethodGet, Path: "/incidents/0"},
	}
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		t.Helper()
		_, c := newIncidentClient(t)
		return c.As(apitest.MemberOf(apitest.OtherOrgID)), nil
	}, routes)
}

func TestAnUnknownQueryParameterIsRefused(t *testing.T) {
	t.Parallel()

	caseID := contractCaseID.String()
	apitest.AssertUnknownQueryParamRefused(t, incidentWorld, []apitest.Route{
		{Op: "listIncidents", Method: http.MethodGet, Path: "/incidents?state=active"},
		{Op: "createIncident", Method: http.MethodPost, Path: "/incidents?force=true",
			Body: `{"case_ids":["` + caseID + `"]}`},
		{Op: "getIncident", Method: http.MethodGet, Path: "/incidents/4?include=members"},
		{Op: "addIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases?force=true",
			Body: `{"case_id":"` + caseID + `"}`},
		{Op: "removeIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases/" + caseID + "/remove?force=true"},
		{Op: "moveIncidentCase", Method: http.MethodPost, Path: "/incidents/4/cases/" + caseID + "/move?force=true",
			Body: `{"to_number":7}`},
	})
}

// TestTheListAnswersWhichIncidentACaseIsIn — git-bug f89c9cc. `?case_id=` is how
// a Case's screen, and a selection on the Cases list, learn BEFORE a draw that the
// Case is already in a story and would be moved rather than drawn twice.
//
// ⭐ NONE OR ONE, AND NOTHING TO PAGE. A Case is in at most one Incident, so the
// filtered answer is complete in one response; a Case in none — or another org's
// Case, which is in none HERE — is the same empty list, never a 404 that would
// confirm the id exists somewhere.
func TestTheListAnswersWhichIncidentACaseIsIn(t *testing.T) {
	t.Parallel()

	f, c := newIncidentClient(t)

	resp := c.GET("/incidents?case_id="+contractCaseID.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listIncidents", http.StatusOK, resp.Body())
	var held struct {
		Data []struct {
			Number int64 `json:"number"`
		} `json:"data"`
		Page struct {
			HasMore bool `json:"has_more"`
		} `json:"page"`
	}
	if err := json.Unmarshal(resp.Body(), &held); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(held.Data) != 1 || held.Data[0].Number != contractNumber || held.Page.HasMore {
		t.Fatalf("a held Case answered %+v, want exactly #%d and no further page", held, contractNumber)
	}

	resp = c.GET("/incidents?case_id="+contractRemovedID.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listIncidents", http.StatusOK, resp.Body())
	if got := resp.JSON(t)["data"]; got == nil || len(got.([]any)) != 0 {
		t.Fatalf("a Case in no Incident answered data=%v, want []", got)
	}

	resp = c.As(apitest.MemberOf(apitest.OtherOrgID)).
		GET("/incidents?case_id="+contractCaseID.String()).MustStatus(t, http.StatusOK)
	if got := resp.JSON(t)["data"]; got == nil || len(got.([]any)) != 0 {
		t.Fatalf("another org's caller learned of this org's Incident: data=%v", got)
	}

	f.mu.Lock()
	asked := append([]uuid.UUID(nil), f.holding...)
	f.mu.Unlock()
	if len(asked) != 3 || asked[0] != contractCaseID {
		t.Fatalf("the service was asked about %v, want the Case in the query each time", asked)
	}
	if got := f.callCount(); got != 0 {
		t.Fatalf("a read reached %d write(s)", got)
	}

	c.GET("/incidents?case_id=banana").MustViolate(t, "case_id")
}

// TestTheListHidesEmptyIncidentsUnlessAsked — owner ruling 2026-10-04. An Incident
// whose every Case was removed or moved away is a record, kept and served by
// number, but the list a human scans leaves it out unless `include_empty=true`
// asks for it.
//
// ⭐ THE DEFAULT IS THE HIDING, AND THE HANDLER — NOT THE CLIENT — OWNS IT. A list
// call that says nothing must reach the service asking for non-empty Incidents
// only; an explicit `false` is the same request.
func TestTheListHidesEmptyIncidentsUnlessAsked(t *testing.T) {
	t.Parallel()

	f, c := newIncidentClient(t)

	for _, path := range []string{"/incidents", "/incidents?include_empty=false", "/incidents?include_empty=true"} {
		resp := c.GET(path).MustStatus(t, http.StatusOK)
		schema.Assert(t, "listIncidents", http.StatusOK, resp.Body())
	}

	f.mu.Lock()
	got := append([]domain.ListFilter(nil), f.listed...)
	f.mu.Unlock()
	want := []domain.ListFilter{{IncludeEmpty: false}, {IncludeEmpty: false}, {IncludeEmpty: true}}
	if len(got) != len(want) {
		t.Fatalf("the service was listed %d time(s), want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("list call %d reached the service as %+v, want %+v", i, got[i], want[i])
		}
	}

	// The record is never hidden from its address: `getIncident` takes no filter.
	resp := c.GET("/incidents/4").MustStatus(t, http.StatusOK)
	schema.Assert(t, "getIncident", http.StatusOK, resp.Body())

	c.GET("/incidents?include_empty=banana").MustViolate(t, "include_empty")
}
