package api

// THE DELIVERY-DRILL TRANSPORT, CHECKED AGAINST THE CONTRACT ITSELF.
//
// This surface is new, which is exactly why it is checked this way. Every finding
// in the conformance audit lived in an `api` package that nobody had ever driven:
// a missing {data,meta} envelope, a DTO short of a required member, an enum value
// the server emitted and the contract rejected. None of those are visible to a
// compiler and all three are visible to `schema.Assert`, which compiles the JSON
// Schema `api/openapi/openapi.yaml` declares for an operationId + status and
// validates the bytes the handler actually wrote.
//
// The promises this file protects:
//
//   - starting a drill answers 202 AND NOT 200, because the pipeline is
//     asynchronous by contract and "it started" is the only honest answer now;
//   - disposal answers 200 with the drill AND NOT 204, because the receipt
//     survives the synthetic rows — "deleted" means the fake alert is gone, and
//     the record that a drill ran is not something an operator can erase;
//   - every stage is reported, including the ones that never ran, because "how
//     far did it get" is half of what the screen is read for;
//   - another tenant's drill id is a 404, never a 403.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
	"github.com/thulasiram/oto/internal/drill/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// Constant ids: a tenant-scoping failure has to be reproducible from the message
// it printed, and a freshly generated "other org" cannot be.
var (
	contractDrillID   = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	contractSourceID  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	contractChannelID = uuid.MustParse("66666666-6666-4666-8666-666666666666")
	contractEpoch     = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
)

// ------------------------------------------------------------------- doubles

// fakeDrillService owns exactly one drill. Every other id is a not-found, which
// is what makes the tenant probe mean something: there is no path to a 200 for an
// id this tenant does not own.
type fakeDrillService struct {
	drill  domain.Drill
	result domain.Result

	startErr   error
	disposeErr error

	started      int
	disposed     int
	lastCommand  service.StartCommand
	lastSourceID uuid.UUID
	lastLimit    int
}

func (f *fakeDrillService) Start(
	_ context.Context, _ db.TenantScope, cmd service.StartCommand,
) (domain.Drill, domain.Result, error) {
	f.started++
	f.lastCommand = cmd
	if f.startErr != nil {
		return domain.Drill{}, domain.Result{}, f.startErr
	}
	return runningDrill(), runningResult(), nil
}

func (f *fakeDrillService) Get(
	_ context.Context, _ db.TenantScope, id uuid.UUID,
) (domain.Drill, domain.Result, error) {
	if id != f.drill.ID {
		return domain.Drill{}, domain.Result{}, errs.NotFound("not_found", "no such resource")
	}
	return f.drill, f.result, nil
}

func (f *fakeDrillService) List(
	_ context.Context, _ db.TenantScope, sourceID uuid.UUID, limit int,
) ([]domain.Drill, []domain.Result, error) {
	f.lastSourceID = sourceID
	f.lastLimit = limit
	return []domain.Drill{f.drill}, []domain.Result{f.result}, nil
}

func (f *fakeDrillService) DisposeNow(_ context.Context, _ db.TenantScope, id uuid.UUID) error {
	if id != f.drill.ID {
		return errs.NotFound("not_found", "no such resource")
	}
	if f.disposeErr != nil {
		return f.disposeErr
	}
	f.disposed++
	f.drill.DisposedAt = contractEpoch.Add(10 * time.Minute)
	return nil
}

// ------------------------------------------------------------------ fixtures

// runningDrill is the drill as it looks the instant `POST /drills` returns: one
// stage passed, every artefact still unknown.
func runningDrill() domain.Drill {
	return domain.Drill{
		ID:             contractDrillID,
		SourceID:       contractSourceID,
		Label:          contractDrillID.String(),
		Severity:       "critical",
		BatchID:        uuid.MustParse("77777777-7777-4777-8777-777777777777"),
		StartedByLabel: "Ada Lovelace",
		StartedAt:      contractEpoch,
		DeadlineAt:     contractEpoch.Add(domain.Deadline),
	}
}

// runningResult reports EVERY stage, most of them still pending. A result screen
// that showed only what happened would make a chain that stopped at `policy` look
// like a chain with three stages.
func runningResult() domain.Result {
	stages := make([]domain.Stage, 0, len(domain.AllStages()))
	for i, name := range domain.AllStages() {
		st := domain.Stage{Name: name, Status: domain.StatusPending, Detail: "not reached yet"}
		if i == 0 {
			st.Status = domain.StatusPassed
			st.Detail = "the synthetic batch is on disk and its processing job is queued"
			st.Facts = map[string]string{"batch_id": "77777777-7777-4777-8777-777777777777"}
		}
		stages = append(stages, st)
	}
	return domain.Summarise(stages, false)
}

// settledDrill is a drill that finished, with every artefact it produced.
func settledDrill() domain.Drill {
	d := runningDrill()
	d.AlertID = uuid.MustParse("88888888-8888-4888-8888-888888888888")
	d.OccurrenceID = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	d.GroupID = uuid.MustParse("aaaaaaa1-aaaa-4aaa-8aaa-aaaaaaaaaaa1")
	d.NotificationID = uuid.MustParse("bbbbbbb1-bbbb-4bbb-8bbb-bbbbbbbbbbb1")
	d.FinishedAt = contractEpoch.Add(9 * time.Second)
	return d
}

// settledResult is the frozen verdict: every stage passed except the one that is
// allowed to skip, and one destination the card actually reached.
func settledResult() domain.Result {
	stages := make([]domain.Stage, 0, len(domain.AllStages()))
	for _, name := range domain.AllStages() {
		st := domain.Stage{Name: name, Status: domain.StatusPassed, Detail: "ok"}
		if name == domain.StageRuleSnapshot {
			// ⭐ `skipped` is not a failure and never sets `failed_stage`: a drill's
			// alert corresponds to no Prometheus rule, and saying so is more honest
			// than a green tick.
			st.Status = domain.StatusSkipped
			st.Detail = "a drill's alert matches no Prometheus rule, so there was nothing to capture"
		}
		stages = append(stages, st)
	}
	res := domain.Summarise(stages, false)
	res.Destinations = []domain.Destination{{
		ChannelID:         contractChannelID,
		ChannelName:       "#sre-alerts",
		Status:            "sent",
		Mode:              "post_root",
		ThreadID:          "1723023262.114300",
		ProviderMessageID: "1723023262.114300",
		Broadcast:         false,
	}}
	return res
}

type drillFixture struct {
	svc *fakeDrillService
	c   *apitest.Client
}

// newDrillFixture builds the router on the REAL clock: `meta.elapsed_ms` is
// `time.Since(started)`, and a fake epoch would make every success body claim an
// elapsed time of several months — which `format: int32` would then refuse.
func newDrillFixture(t *testing.T) *drillFixture {
	t.Helper()

	svc := &fakeDrillService{drill: settledDrill(), result: settledResult()}
	rt := NewRouter(svc, clock.New())
	return &drillFixture{svc: svc, c: apitest.New(rt)}
}

// startBody is the smallest request a real client could send, proven against the
// contract's own request schema before it is sent.
func startBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal the start request: %v", err)
	}
	schema.AssertRequest(t, "startDeliveryDrill", raw)
	return body
}

// ------------------------------------------------------------- happy paths

// ⭐ TestStartingADrillAnswers202WithEveryStageReported.
//
// The promise: 202, not 200, and a body carrying the full stage list with
// everything still pending.
//
// What broke without it: the pipeline a drill drives is four jobs deep, so a 200
// would claim a verdict that does not exist yet — and a client that rendered one
// component for the 202 and another for the poll would be two components that
// have to be kept in agreement forever.
func TestStartingADrillAnswers202WithEveryStageReported(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	body := startBody(t, map[string]any{
		"source_id": contractSourceID.String(),
		"severity":  "critical",
	})

	resp := f.c.POST(t, "/drills", body).MustStatus(t, http.StatusAccepted)
	schema.Assert(t, "startDeliveryDrill", http.StatusAccepted, resp.Body())

	if f.svc.started != 1 {
		t.Fatalf("the service started %d drill(s), want 1", f.svc.started)
	}
	if f.svc.lastCommand.SourceID != contractSourceID {
		t.Fatalf("the command names source %s, want %s", f.svc.lastCommand.SourceID, contractSourceID)
	}
	// ⛔ ACTOR, NEVER SUBJECT. The label is frozen at write time so the receipt
	// stays readable after the user is gone, and no per-person metric is derived
	// from it.
	if f.svc.lastCommand.ActorLabel != apitest.Member().DisplayName {
		t.Fatalf("started_by label = %q, want the caller's display name", f.svc.lastCommand.ActorLabel)
	}

	data := dataObject(t, resp.JSON(t))
	stages, ok := data["stages"].([]any)
	if !ok || len(stages) != len(domain.AllStages()) {
		t.Fatalf("stages = %v; every stage is reported, including the ones that never started", data["stages"])
	}
	if data["failed_stage"] != nil {
		t.Fatalf("a freshly started drill named a failed stage: %v", data["failed_stage"])
	}
	// A nil destination slice would render `null`; the contract says this member
	// is always an array.
	if _, ok := data["destinations"].([]any); !ok {
		t.Fatalf("destinations = %v, want [] rather than null", data["destinations"])
	}
}

// TestPollingADrillAnswersTheStagedResultTheContractDeclares.
func TestPollingADrillAnswersTheStagedResultTheContractDeclares(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	resp := f.c.GET("/drills/"+contractDrillID.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getDeliveryDrill", http.StatusOK, resp.Body())

	data := dataObject(t, resp.JSON(t))
	if data["status"] != string(domain.DrillPassed) {
		t.Fatalf("status = %v, want passed", data["status"])
	}
	// ⭐ A skipped stage is not a failure and must not set `failed_stage`.
	if data["failed_stage"] != nil {
		t.Fatalf("a passing drill named a failed stage: %v", data["failed_stage"])
	}
}

// TestListingADrillHistoryAnswersTheUncursoredEnvelope.
//
// The promise: {data, meta} and deliberately NO `page`. A drill is one operator
// pressing one button; the bound is hard-coded rather than exposed so this can
// never become a list endpoint with a paging contract to maintain.
func TestListingADrillHistoryAnswersTheUncursoredEnvelope(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	resp := f.c.GET("/drills?source_id="+contractSourceID.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listDeliveryDrills", http.StatusOK, resp.Body())

	if f.svc.lastSourceID != contractSourceID {
		t.Fatalf("the service was asked about %s, want %s", f.svc.lastSourceID, contractSourceID)
	}
	if f.svc.lastLimit != listLimit {
		t.Fatalf("limit = %d, want the hard-coded %d", f.svc.lastLimit, listLimit)
	}
	if _, present := resp.JSON(t)["page"]; present {
		t.Fatal("the drill history grew a `page`; it is uncursored on purpose")
	}
}

// ⭐ TestDisposingADrillKeepsTheReceiptAndAnswers200.
//
// The promise: 200 with the drill and `disposed_at` set — NOT 204.
//
// "Deleted" here means the fake alert is gone. The record that a drill ran, and
// what it found, is not something an operator can erase; that is the same rule
// the timeline lives by. A 204 would have made the two indistinguishable.
func TestDisposingADrillKeepsTheReceiptAndAnswers200(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	resp := f.c.DELETE("/drills/"+contractDrillID.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "disposeDeliveryDrill", http.StatusOK, resp.Body())

	if f.svc.disposed != 1 {
		t.Fatalf("the synthetic rows were disposed %d time(s), want 1", f.svc.disposed)
	}

	data := dataObject(t, resp.JSON(t))
	if data["disposed_at"] == nil {
		t.Fatal("the disposal answered without setting disposed_at")
	}
	// The receipt: the verdict is still there after the rows are gone.
	if data["finished_at"] == nil || data["status"] != string(domain.DrillPassed) {
		t.Fatalf("the receipt did not survive disposal: %s", resp)
	}
}

// --------------------------------------------------------------- boundaries

// ⛔ TestADrillOutsideTheCallersTenantIsANotFound.
//
// The promise: every id-addressed drill operation answers 404 for an id it does
// not own — never 403, never another tenant's staged result, never a 500.
//
// A 403 confirms the id exists somewhere. v1's only cause of 403 is cross-org
// access, which is exactly the case that must be indistinguishable from "no such
// drill".
func TestADrillOutsideTheCallersTenantIsANotFound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		op     string
		method string
		path   string
	}{
		{"poll another org's drill", "getDeliveryDrill", http.MethodGet,
			"/drills/" + apitest.StrangerID.String()},
		{"dispose another org's drill", "disposeDeliveryDrill", http.MethodDelete,
			"/drills/" + apitest.StrangerID.String()},
		{"poll an id that is not a uuid", "getDeliveryDrill", http.MethodGet,
			"/drills/banana"},
		{"dispose an id that is not a uuid", "disposeDeliveryDrill", http.MethodDelete,
			"/drills/banana"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newDrillFixture(t)
			var resp *apitest.Response
			if tc.method == http.MethodGet {
				resp = f.c.GET(tc.path)
			} else {
				resp = f.c.DELETE(tc.path)
			}
			resp.MustStatus(t, http.StatusNotFound)
			schema.AssertProblem(t, tc.op, http.StatusNotFound, resp.Body())

			if f.svc.disposed != 0 {
				t.Fatal("a cross-tenant request still deleted rows")
			}
			if strings.Contains(string(resp.Body()), apitest.OtherOrgID.String()) {
				t.Fatalf("the 404 names the owning org: %s", resp)
			}
		})
	}
}

// TestStartingADrillWithoutASourceNamesTheField.
//
// The promise: 422 with `violations[]` naming `source_id` and carrying a machine
// code, so a form can highlight the control rather than showing prose.
func TestStartingADrillWithoutASourceNamesTheField(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	resp := f.c.POST(t, "/drills", map[string]any{}).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "startDeliveryDrill", http.StatusUnprocessableEntity, resp.Body())

	resp.MustViolate(t, "source_id")
	if f.svc.started != 0 {
		t.Fatal("an invalid request still started a drill")
	}
}

// TestAnUnknownFieldInTheStartBodyIsRefusedRatherThanIgnored.
//
// The contract marks `StartDrillRequest` `additionalProperties: false`, and the
// decoder refuses unknown fields for the same reason: the drill's whole value is
// that it resembles a real alert, so a knob the server quietly dropped would be a
// caller believing they configured something they did not.
func TestAnUnknownFieldInTheStartBodyIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	resp := f.c.POST(t, "/drills", map[string]any{
		"source_id": contractSourceID.String(),
		"labels":    map[string]string{"team": "payments"},
	}).MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "startDeliveryDrill", http.StatusUnprocessableEntity, resp.Body())

	resp.MustViolate(t, "labels")
	if f.svc.started != 0 {
		t.Fatal("a request with an unknown field still started a drill")
	}
}

// TestASecondDrillOnTheSameSourceIsRefusedWith412.
//
// The promise: at most one drill per source in flight, refused as a precondition
// rather than as a validation failure. The request is well formed and the caller
// has permission; the ENTITY is in the wrong state, and that is the distinction
// 412 exists to carry.
func TestASecondDrillOnTheSameSourceIsRefusedWith412(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	f.svc.startErr = errs.Precondition("drill_in_flight",
		"a drill is already running against this source")

	body := startBody(t, map[string]any{"source_id": contractSourceID.String()})
	resp := f.c.POST(t, "/drills", body).MustStatus(t, http.StatusPreconditionFailed)
	schema.AssertProblem(t, "startDeliveryDrill", http.StatusPreconditionFailed, resp.Body())

	if code := resp.Problem(t).Code; code != "drill_in_flight" {
		t.Fatalf("code = %q; a client branches on the code, not the prose", code)
	}
}

// TestDisposingAnUnsettledDrillIsRefusedWith412.
//
// Deleting the rows a running drill is still being judged from would make it
// report failures that never happened — an answer worse than no answer, because
// it is confidently wrong.
func TestDisposingAnUnsettledDrillIsRefusedWith412(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	f.svc.disposeErr = errs.Precondition("drill_running",
		"this drill has not settled; disposing now would make it report failures that never happened")

	resp := f.c.DELETE("/drills/"+contractDrillID.String()).
		MustStatus(t, http.StatusPreconditionFailed)
	schema.AssertProblem(t, "disposeDeliveryDrill", http.StatusPreconditionFailed, resp.Body())

	if f.svc.disposed != 0 {
		t.Fatal("a refused disposal still deleted rows")
	}
}

// TestAnUnknownQueryParameterOnTheDrillHistoryIsRefused — SPEC §E.3.
//
// A typo'd `?source=…` that is silently dropped would answer with the drills of
// whatever source the server picked instead, which is the wrong answer wearing
// the right shape.
func TestAnUnknownQueryParameterOnTheDrillHistoryIsRefused(t *testing.T) {
	t.Parallel()

	f := newDrillFixture(t)
	resp := f.c.GET("/drills?source="+contractSourceID.String()).
		MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "listDeliveryDrills", http.StatusBadRequest, resp.Body())

	p := resp.MustViolate(t, "source")
	if p.Code != "unknown_parameter" {
		t.Fatalf("code = %q, want unknown_parameter", p.Code)
	}
}

// TestAnUnauthenticatedCallerGetsTheSame401OnEveryDrillRoute.
func TestAnUnauthenticatedCallerGetsTheSame401OnEveryDrillRoute(t *testing.T) {
	t.Parallel()

	routes := []struct {
		op     string
		method string
		path   string
	}{
		{"startDeliveryDrill", http.MethodPost, "/drills"},
		{"listDeliveryDrills", http.MethodGet, "/drills?source_id=" + contractSourceID.String()},
		{"getDeliveryDrill", http.MethodGet, "/drills/" + contractDrillID.String()},
		{"disposeDeliveryDrill", http.MethodDelete, "/drills/" + contractDrillID.String()},
	}

	for _, route := range routes {
		t.Run(route.op, func(t *testing.T) {
			t.Parallel()

			f := newDrillFixture(t)
			anon := f.c.Anonymous()

			var resp *apitest.Response
			switch route.method {
			case http.MethodPost:
				resp = anon.POST(t, route.path, map[string]any{"source_id": contractSourceID.String()})
			case http.MethodDelete:
				resp = anon.DELETE(route.path)
			default:
				resp = anon.GET(route.path)
			}
			resp.MustStatus(t, http.StatusUnauthorized)
			schema.AssertProblem(t, route.op, http.StatusUnauthorized, resp.Body())

			if code := resp.Problem(t).Code; code != "unauthenticated" {
				t.Fatalf("code = %q, want unauthenticated", code)
			}
			if f.svc.started != 0 || f.svc.disposed != 0 {
				t.Fatal("an unauthenticated request reached the service")
			}
		})
	}
}

// --------------------------------------------------------- declared surface

// TestTheDrillOperationsDeclareTheStatusesTheseHandlersProduce is the guard
// against the failure that made the audit necessary: an operation whose declared
// status set and whose handler's status set were never compared.
func TestTheDrillOperationsDeclareTheStatusesTheseHandlersProduce(t *testing.T) {
	t.Parallel()

	if got := schema.Op(t, "startDeliveryDrill").SuccessStatus(); got != http.StatusAccepted {
		t.Fatalf("startDeliveryDrill declares success %d; 202 is the contract, because the "+
			"pipeline is asynchronous and a 200 would claim a verdict that does not exist yet", got)
	}
	dispose := schema.Op(t, "disposeDeliveryDrill")
	if dispose.SuccessStatus() != http.StatusOK {
		t.Fatalf("disposeDeliveryDrill declares success %d, want 200: the receipt survives the "+
			"synthetic rows, so there is a body", dispose.SuccessStatus())
	}
	if dispose.Declares(http.StatusNoContent) {
		t.Fatal("disposeDeliveryDrill declares a 204; a drill's receipt is not erasable")
	}
	for _, id := range []string{"getDeliveryDrill", "disposeDeliveryDrill"} {
		if !schema.Op(t, id).HasPathParam("id") {
			t.Fatalf("%s no longer addresses a drill by id", id)
		}
		if !schema.Op(t, id).Declares(http.StatusNotFound) {
			t.Fatalf("%s declares no 404, and 404 is how a cross-tenant id is refused", id)
		}
	}
}

// ------------------------------------------------------------ known defects

// TestBUG_ListDeliveryDrillsRefusesAMissingSourceIDWithAnUndeclaredStatus.
//
// WHAT THE SERVER DOES: `listDrills` (internal/drill/api/drills.go:138) answers
// `errs.Validation("source_id_required", …)` when the required `source_id` query
// parameter is absent, which `httpx.WriteProblem` renders as **422**. The error
// carries no `violations[]` either.
//
// WHAT THE CONTRACT SAYS: `listDeliveryDrills` declares 200, 400, 401, 403, 429,
// 500 and 503 — there is no 422 at all — and marks `source_id` as
// `required: true`, whose omission RFC-wise is a malformed request. The contract
// also states that a 422 "always carries violations[]".
//
// WHICH IS WRONG: the server. A missing required QUERY PARAMETER is the
// `400 malformed_request` family that `httpx.NewParams` already uses for
// `unknown_parameter`; 422 is for a body that parsed and is semantically invalid.
// As it stands a generated client validating against the contract fails on a
// status the server can produce on its very first call.
func TestBUG_ListDeliveryDrillsRefusesAMissingSourceIDWithAnUndeclaredStatus(t *testing.T) {
	t.Skip("live conformance defect: the handler answers 422 for a missing required query " +
		"parameter and the contract declares no 422 for listDeliveryDrills; see the doc comment")

	f := newDrillFixture(t)
	resp := f.c.GET("/drills").MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "listDeliveryDrills", http.StatusBadRequest, resp.Body())
	resp.MustViolate(t, "source_id")
}

// TestBUG_TheIdAddressedDrillRoutesCanAnswerAnUndeclared400.
//
// WHAT THE SERVER DOES: `subject` (internal/drill/api/drills.go:195) runs
// `httpx.NewParams(r).Err()`, which refuses ANY query parameter on
// `GET /api/v1/drills/{id}` and `DELETE /api/v1/drills/{id}` with
// `400 unknown_parameter`.
//
// WHAT THE CONTRACT SAYS: neither `getDeliveryDrill` nor `disposeDeliveryDrill`
// declares a 400 at all (openapi.yaml:3689-3707, 3724-3744).
//
// WHICH IS WRONG: the contract. Rejecting an unknown parameter is SPEC §E.3 and
// is right; the declaration is simply missing a `'400': $ref BadRequest`, exactly
// as `listDeliveryDrills` and `listSilences` have. The refusal is not reachable
// by any correct client, which is why it survived review.
func TestBUG_TheIdAddressedDrillRoutesCanAnswerAnUndeclared400(t *testing.T) {
	t.Skip("live conformance defect: the handler answers 400 unknown_parameter on the " +
		"id-addressed drill routes and the contract declares no 400 for them; see the doc comment")

	f := newDrillFixture(t)
	resp := f.c.GET("/drills/"+contractDrillID.String()+"?verbose=true").
		MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "getDeliveryDrill", http.StatusBadRequest, resp.Body())
}

// ------------------------------------------------------------------ helpers

func dataObject(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("the response has no `data` object: %v", body)
	}
	return data
}
