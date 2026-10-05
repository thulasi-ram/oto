package api

// THE INVESTIGATOR TRANSPORT, CHECKED AGAINST THE CONTRACT ITSELF (ADR 0053, git-bug
// 180a525).
//
// ⭐ NO RESPONSE SHAPE IS RE-STATED BY HAND. Every success body goes through
// `schema.Assert` for its operationId and status, and every refusal through
// `schema.AssertProblem`.
//
// The properties this file protects:
//
//   - every one of the eleven operations answers the shape the contract declares —
//     the two on an Incident's Investigations included (git-bug 74ea849);
//   - an API key is write-only: it reaches the service and no response ever carries it;
//   - asking for an Investigation answers 202, and a kill switch that is off is still
//     a 202 whose body says `skipped`/`disabled` — recorded, never silent;
//   - asking needs a human: a system principal is a 403 before the service is reached;
//   - an allowlist is exact: a wildcard is a 422 naming `tools`;
//   - another org's Investigator, Investigation or Case is a 404, never a 403.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

var (
	fxProvider      = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	fxInvestigator  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	fxVersion       = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	fxCase          = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	fxInvestigation = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	fxIncident      = uuid.MustParse("77777777-7777-4777-8777-777777777777")
	fxEpoch         = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
)

const fxKey = "sk-contract-never-echoed"

// fakeInvestigators owns ONE of everything in apitest.OrgID and answers 404 for
// every other id and every other org, so the only route to a 200 is an id this
// tenant owns.
type fakeInvestigators struct {
	mu        sync.Mutex
	calls     []string
	gotKey    string
	off       bool
	spent     bool
	requester domain.Requester
	change    domain.InvestigatorChange
}

func (f *fakeInvestigators) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeInvestigators) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func mine(s db.TenantScope) bool { return s.OrgID() == apitest.OrgID }

func fxProviderConfig() domain.ProviderConfig {
	return domain.ProviderConfig{ID: fxProvider, OrgID: apitest.OrgID, Name: "gateway",
		BaseURL: "https://llm-gateway.example.test/v1", Model: "m-1", CredentialID: uuid.New(),
		CreatedAt: fxEpoch, UpdatedAt: fxEpoch}
}

func fxInvestigatorValue() domain.Investigator {
	tools, err := domain.NewAllowlist([]string{"oto_case_timeline", "oto_rule_at_fire"})
	if err != nil {
		panic(err)
	}
	return domain.Investigator{
		ID: fxInvestigator, OrgID: apitest.OrgID, Name: "firstlook", Enabled: true,
		Budgets: domain.DefaultBudgets(),
		Current: domain.Version{ID: fxVersion, InvestigatorID: fxInvestigator, Number: 2, ProviderID: fxProvider,
			Model:  domain.ModelIdentity{Endpoint: "https://llm-gateway.example.test/v1", Model: "m-1"},
			Prompt: "Read the Case and say what is going on.", Tools: tools, CreatedAt: fxEpoch},
		CreatedAt: fxEpoch, UpdatedAt: fxEpoch,
	}
}

func fxInvestigationValue(status domain.Status) domain.Investigation {
	inv := domain.Investigation{
		ID: fxInvestigation, OrgID: apitest.OrgID, SubjectKind: domain.SubjectCase, SubjectID: fxCase,
		AlertKey: "k", InvestigatorID: fxInvestigator, InvestigatorName: "firstlook",
		VersionID: fxVersion, VersionNumber: 2,
		Model:  domain.ModelIdentity{Endpoint: "https://llm-gateway.example.test/v1", Model: "m-1"},
		Status: status, Budgets: domain.DefaultBudgets(),
		RequestedBy: domain.Requester{UserID: apitest.UserID, Label: "Ada Lovelace"}, RequestedAt: fxEpoch,
	}
	switch status {
	case domain.StatusExhausted:
		inv.Ending = domain.EndedBy(domain.ReasonStepBudget, "the step budget of 20 Tool calls was spent")
		inv.Spent, inv.ToolCalls, inv.Finding = domain.Usage{InputTokens: 900, OutputTokens: 80}, 20, "It looks like a deploy."
		inv.Classification = "deploy-regression"
		inv.StartedAt, inv.EndedAt = fxEpoch.Add(time.Second), fxEpoch.Add(time.Minute)
	case domain.StatusSkipped:
		inv.Ending = domain.EndedBy(domain.ReasonDisabled, "the Investigator firstlook is disabled")
		inv.EndedAt = fxEpoch
	}
	return inv
}

func (f *fakeInvestigators) CreateProvider(_ context.Context, _ db.TenantScope, d domain.ProviderDraft) (domain.ProviderConfig, error) {
	f.record("createProvider")
	f.mu.Lock()
	f.gotKey = d.APIKey
	f.mu.Unlock()
	return fxProviderConfig(), nil
}

func (f *fakeInvestigators) ListProviders(_ context.Context, s db.TenantScope) ([]domain.ProviderConfig, error) {
	if !mine(s) {
		return []domain.ProviderConfig{}, nil
	}
	return []domain.ProviderConfig{fxProviderConfig()}, nil
}

func (f *fakeInvestigators) CreateInvestigator(_ context.Context, _ db.TenantScope, _ domain.InvestigatorDraft) (domain.Investigator, error) {
	f.record("createInvestigator")
	return fxInvestigatorValue(), nil
}

func (f *fakeInvestigators) UpdateInvestigator(_ context.Context, s db.TenantScope, id uuid.UUID, c domain.InvestigatorChange) (domain.Investigator, error) {
	f.record("updateInvestigator")
	if !mine(s) || id != fxInvestigator {
		return domain.Investigator{}, errs.NotFound("investigator_not_found", "no such Investigator")
	}
	f.mu.Lock()
	f.change = c
	f.mu.Unlock()
	return fxInvestigatorValue(), nil
}

func (f *fakeInvestigators) ListInvestigators(_ context.Context, s db.TenantScope) ([]domain.Investigator, error) {
	if !mine(s) {
		return []domain.Investigator{}, nil
	}
	return []domain.Investigator{fxInvestigatorValue()}, nil
}

func (f *fakeInvestigators) GetInvestigator(_ context.Context, s db.TenantScope, id uuid.UUID) (service.InvestigatorDetail, error) {
	if !mine(s) || id != fxInvestigator {
		return service.InvestigatorDetail{}, errs.NotFound("investigator_not_found", "no such Investigator")
	}
	inv := fxInvestigatorValue()
	v1 := inv.Current
	v1.ID, v1.Number = uuid.MustParse("66666666-6666-4666-8666-666666666666"), 1
	return service.InvestigatorDetail{Investigator: inv, Versions: []domain.Version{inv.Current, v1}}, nil
}

func (f *fakeInvestigators) RequestCaseInvestigation(
	_ context.Context, s db.TenantScope, caseID, investigatorID uuid.UUID, by domain.Requester,
) (domain.Investigation, error) {
	f.record("request")
	if !mine(s) || caseID != fxCase {
		return domain.Investigation{}, errs.NotFound("case_not_found", "no such case")
	}
	if investigatorID != fxInvestigator {
		return domain.Investigation{}, errs.NotFound("investigator_not_found", "no such Investigator")
	}
	f.mu.Lock()
	f.requester = by
	off, spent := f.off, f.spent
	f.mu.Unlock()
	switch {
	case off:
		return fxInvestigationValue(domain.StatusSkipped), nil
	case spent:
		inv := fxInvestigationValue(domain.StatusSkipped)
		inv.Ending = domain.EndedBy(domain.ReasonBudget,
			"this org has spent 2000000 of its 2000000 daily Investigation tokens (investigation_daily_tokens) since 00:00 UTC")
		return inv, nil
	default:
		return fxInvestigationValue(domain.StatusQueued), nil
	}
}

func (f *fakeInvestigators) ListCaseInvestigations(
	_ context.Context, s db.TenantScope, caseID uuid.UUID, _ db.Keyset,
) ([]domain.Investigation, db.Cursor, error) {
	if !mine(s) || caseID != fxCase {
		return nil, db.Cursor{}, errs.NotFound("case_not_found", "no such case")
	}
	return []domain.Investigation{fxInvestigationValue(domain.StatusExhausted), fxInvestigationValue(domain.StatusSkipped)},
		db.Cursor{}, nil
}

// fxIncidentNumber is the one Incident this tenant owns.
const fxIncidentNumber = 4

func (f *fakeInvestigators) RequestIncidentInvestigation(
	_ context.Context, s db.TenantScope, number int64, investigatorID uuid.UUID, by domain.Requester,
) (domain.Investigation, error) {
	f.record("request incident")
	if !mine(s) || number != fxIncidentNumber {
		return domain.Investigation{}, errs.NotFound("incident_not_found", "no such incident")
	}
	if investigatorID != fxInvestigator {
		return domain.Investigation{}, errs.NotFound("investigator_not_found", "no such Investigator")
	}
	f.mu.Lock()
	f.requester = by
	f.mu.Unlock()
	return fxIncidentInvestigationValue(domain.StatusQueued), nil
}

func (f *fakeInvestigators) ListIncidentInvestigations(
	_ context.Context, s db.TenantScope, number int64, _ db.Keyset,
) ([]domain.Investigation, db.Cursor, error) {
	if !mine(s) || number != fxIncidentNumber {
		return nil, db.Cursor{}, errs.NotFound("incident_not_found", "no such incident")
	}
	return []domain.Investigation{fxIncidentInvestigationValue(domain.StatusExhausted)}, db.Cursor{}, nil
}

// fxIncidentInvestigationValue is a run about the Incident: no alert key, and the
// requester an automatic trigger freezes on it.
func fxIncidentInvestigationValue(status domain.Status) domain.Investigation {
	inv := fxInvestigationValue(status)
	inv.SubjectKind, inv.SubjectID, inv.AlertKey = domain.SubjectIncident, fxIncident, ""
	inv.RequestedBy = domain.Requester{Label: "oto: Incident #4 was drawn"}
	return inv
}

func (f *fakeInvestigators) GetInvestigation(_ context.Context, s db.TenantScope, id uuid.UUID) (service.InvestigationDetail, error) {
	if !mine(s) || id != fxInvestigation {
		return service.InvestigationDetail{}, errs.NotFound("investigation_not_found", "no such Investigation")
	}
	turn, err := domain.NewTurn(domain.ModelIdentity{}, "Let me read the timeline.",
		[]domain.ToolCall{{ID: "c1", Name: "oto_case_timeline", Arguments: `{}`}},
		&domain.Usage{InputTokens: 400, OutputTokens: 30}, domain.FinishToolCalls)
	if err != nil {
		panic(err)
	}
	return service.InvestigationDetail{
		Investigation: fxInvestigationValue(domain.StatusExhausted),
		Steps: []domain.Step{
			domain.NewModelTurnStep(1, turn, 800*time.Millisecond, fxEpoch.Add(2*time.Second)),
			domain.NewToolStep(2, turn.ToolCalls[0], domain.OutcomeOK, `{"entries":[]}`, 5*time.Millisecond, fxEpoch.Add(3*time.Second)),
			domain.NewToolStep(3, domain.ToolCall{ID: "c2", Name: "kubectl_exec", Arguments: `{"cmd":"rm"}`},
				domain.OutcomeRefused, `refused: "kubectl_exec" is not on this Investigator's Tool allowlist`, 0, fxEpoch.Add(4*time.Second)),
		},
	}, nil
}

func newClient(t *testing.T) (*fakeInvestigators, *apitest.Client) {
	t.Helper()
	f := &fakeInvestigators{}
	return f, apitest.New(NewRouter(f, clock.New()))
}

const createInvestigatorBody = `{"name":"firstlook","model_provider_id":"11111111-1111-4111-8111-111111111111",` +
	`"prompt":"Read the Case.","tools":["oto_case_timeline"]}`

// ------------------------------------------------------------- happy paths

func TestEveryInvestigatorOperationAnswersItsContractShape(t *testing.T) {
	t.Parallel()

	_, c := newClient(t)
	caseID, invID := fxCase.String(), fxInvestigator.String()

	resp := c.GET("/model-providers").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listModelProviders", http.StatusOK, resp.Body())

	resp = c.Raw(http.MethodPost, "/model-providers", apitest.ContentTypeJSON,
		`{"name":"gateway","base_url":"https://llm-gateway.example.test/v1","model":"m-1","api_key":"`+fxKey+`"}`).
		MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createModelProvider", http.StatusCreated, resp.Body())

	resp = c.GET("/investigators").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listInvestigators", http.StatusOK, resp.Body())

	resp = c.Raw(http.MethodPost, "/investigators", apitest.ContentTypeJSON, createInvestigatorBody).
		MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createInvestigator", http.StatusCreated, resp.Body())

	resp = c.GET("/investigators/"+invID).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getInvestigator", http.StatusOK, resp.Body())

	resp = c.PATCH(t, "/investigators/"+invID, map[string]any{"enabled": false}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateInvestigator", http.StatusOK, resp.Body())

	resp = c.POST(t, "/cases/"+caseID+"/investigations", map[string]any{"investigator_id": invID}).
		MustStatus(t, http.StatusAccepted)
	schema.Assert(t, "requestCaseInvestigation", http.StatusAccepted, resp.Body())

	resp = c.GET("/cases/"+caseID+"/investigations").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listCaseInvestigations", http.StatusOK, resp.Body())

	resp = c.GET("/investigations/"+fxInvestigation.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getInvestigation", http.StatusOK, resp.Body())

	resp = c.POST(t, "/incidents/4/investigations", map[string]any{"investigator_id": invID}).
		MustStatus(t, http.StatusAccepted)
	schema.Assert(t, "requestIncidentInvestigation", http.StatusAccepted, resp.Body())
	if got := resp.JSON(t)["data"].(map[string]any)["subject_kind"]; got != "incident" {
		t.Fatalf("an Incident's run answered subject_kind %v", got)
	}

	resp = c.GET("/incidents/4/investigations").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listIncidentInvestigations", http.StatusOK, resp.Body())
}

// TestAnIncidentIsAskedAboutByHumansOnlyAndByItsNumber — ADR 0053 §4 (git-bug 74ea849):
// "a human asks" holds for an Incident as for a Case, and an Incident is addressed by
// the number a human quotes; anything that is not one is a 404.
func TestAnIncidentIsAskedAboutByHumansOnlyAndByItsNumber(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	resp := c.As(apitest.Machine()).POST(t, "/incidents/4/investigations",
		map[string]any{"investigator_id": fxInvestigator.String()}).MustStatus(t, http.StatusForbidden)
	schema.AssertProblem(t, "requestIncidentInvestigation", http.StatusForbidden, resp.Body())
	if f.callCount() != 0 {
		t.Fatalf("a system principal reached the service %d time(s)", f.callCount())
	}
	for _, bad := range []string{"0", "-1", "four", "4.0"} {
		resp := c.GET("/incidents/"+bad+"/investigations").MustStatus(t, http.StatusNotFound)
		schema.AssertProblem(t, "listIncidentInvestigations", http.StatusNotFound, resp.Body())
	}
}

// TestInvestigatingIncidentsIsAnOptInThatChangesInPlace — which Investigators an
// Incident starts on its own is the operator's word (git-bug 74ea849): a flag that
// reaches the service as a change, never a version, and is absent unless sent.
func TestInvestigatingIncidentsIsAnOptInThatChangesInPlace(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	c.PATCH(t, "/investigators/"+fxInvestigator.String(), map[string]any{"investigates_incidents": true}).
		MustStatus(t, http.StatusOK)
	f.mu.Lock()
	ch := f.change
	f.mu.Unlock()
	if ch.InvestigatesIncidents == nil || !*ch.InvestigatesIncidents || ch.TouchesVersion() {
		t.Fatalf("the change reached the service as %+v", ch)
	}
	c.PATCH(t, "/investigators/"+fxInvestigator.String(), map[string]any{"enabled": true}).MustStatus(t, http.StatusOK)
	f.mu.Lock()
	ch = f.change
	f.mu.Unlock()
	if ch.InvestigatesIncidents != nil {
		t.Fatalf("an omitted flag reached the service as %v", *ch.InvestigatesIncidents)
	}
}

// TestAnAPIKeyIsWriteOnly — the key reaches the service and no response carries it,
// and neither does any read.
func TestAnAPIKeyIsWriteOnly(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	resp := c.Raw(http.MethodPost, "/model-providers", apitest.ContentTypeJSON,
		`{"name":"gateway","base_url":"https://llm-gateway.example.test/v1","model":"m-1","api_key":"`+fxKey+`"}`).
		MustStatus(t, http.StatusCreated)
	if strings.Contains(string(resp.Body()), fxKey) {
		t.Fatalf("the create response echoed the key:\n%s", resp)
	}
	if got := resp.JSON(t)["data"].(map[string]any)["has_key"]; got != true {
		t.Fatalf("has_key = %v, want true", got)
	}
	f.mu.Lock()
	got := f.gotKey
	f.mu.Unlock()
	if got != fxKey {
		t.Fatalf("the service was handed %q, want the key the caller sent", got)
	}
	if body := c.GET("/model-providers").MustStatus(t, http.StatusOK).Body(); strings.Contains(string(body), "sk-") {
		t.Fatalf("the list carries key material:\n%s", body)
	}
}

// TestARequestIsAcceptedAndASwitchedOffOneIsRecordedNotDropped — ADR 0053 §6: hitting
// the kill switch is recorded, never silent.
func TestARequestIsAcceptedAndASwitchedOffOneIsRecordedNotDropped(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	path := "/cases/" + fxCase.String() + "/investigations"
	body := map[string]any{"investigator_id": fxInvestigator.String()}

	var queued struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(c.POST(t, path, body).MustStatus(t, http.StatusAccepted).Body(), &queued); err != nil {
		t.Fatal(err)
	}
	if queued.Data.Status != "queued" || queued.Data.ID != fxInvestigation.String() {
		t.Fatalf("a request answered %+v, want the queued run's id", queued.Data)
	}
	f.mu.Lock()
	by := f.requester
	f.off = true
	f.mu.Unlock()
	if by.UserID != apitest.UserID || by.Label != "Ada Lovelace" {
		t.Fatalf("the run was attributed to %+v, want the caller", by)
	}

	resp := c.POST(t, path, body).MustStatus(t, http.StatusAccepted)
	schema.Assert(t, "requestCaseInvestigation", http.StatusAccepted, resp.Body())
	data := resp.JSON(t)["data"].(map[string]any)
	if data["status"] != "skipped" || data["reason"] != "disabled" || data["reason_detail"] == nil {
		t.Fatalf("a switched-off request answered %v, want skipped/disabled with a sentence", data)
	}
}

// TestARequestPastTheDailyBudgetIsRecordedNotDropped — ADR 0053 §6 (git-bug bf172fe):
// past the org's daily token budget a request is still a 202, whose run is
// `skipped`/`budget` — and the contract's reason enum admits it.
func TestARequestPastTheDailyBudgetIsRecordedNotDropped(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	f.mu.Lock()
	f.spent = true
	f.mu.Unlock()
	resp := c.POST(t, "/cases/"+fxCase.String()+"/investigations",
		map[string]any{"investigator_id": fxInvestigator.String()}).MustStatus(t, http.StatusAccepted)
	schema.Assert(t, "requestCaseInvestigation", http.StatusAccepted, resp.Body())
	data := resp.JSON(t)["data"].(map[string]any)
	if data["status"] != "skipped" || data["reason"] != "budget" || data["not_before"] != nil {
		t.Fatalf("a request past the budget answered %v, want skipped/budget", data)
	}
}

// TestAskingNeedsAHuman — a run spends money; spending attributed to nobody cannot be
// asked about.
func TestAskingNeedsAHuman(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	resp := c.As(apitest.Machine()).POST(t, "/cases/"+fxCase.String()+"/investigations",
		map[string]any{"investigator_id": fxInvestigator.String()}).MustStatus(t, http.StatusForbidden)
	schema.AssertProblem(t, "requestCaseInvestigation", http.StatusForbidden, resp.Body())
	if f.callCount() != 0 {
		t.Fatalf("a system principal reached the service %d time(s)", f.callCount())
	}
}

// TestAnAllowlistIsExact — ADR 0053 §6: "named Tools, no wildcards".
func TestAnAllowlistIsExact(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	for _, bad := range []string{"*", "oto_*", "kubectl.*"} {
		resp := c.Raw(http.MethodPost, "/investigators", apitest.ContentTypeJSON,
			`{"name":"firstlook","model_provider_id":"`+fxProvider.String()+`","prompt":"p","tools":["`+bad+`"]}`).
			MustStatus(t, http.StatusUnprocessableEntity)
		schema.AssertProblem(t, "createInvestigator", http.StatusUnprocessableEntity, resp.Body())
		resp.MustViolate(t, "tools")

		resp = c.PATCH(t, "/investigators/"+fxInvestigator.String(), map[string]any{"tools": []string{bad}}).
			MustStatus(t, http.StatusUnprocessableEntity)
		schema.AssertProblem(t, "updateInvestigator", http.StatusUnprocessableEntity, resp.Body())
		resp.MustViolate(t, "tools")
	}
	if f.callCount() != 0 {
		t.Fatalf("a wildcard reached the service %d time(s)", f.callCount())
	}
}

// TestANameTakesTheEnricherAlphabet — the Finding is published as
// `investigator.<name>`, and the enrichment store admits nothing else.
func TestANameTakesTheEnricherAlphabet(t *testing.T) {
	t.Parallel()

	_, c := newClient(t)
	for _, bad := range []string{"First-Look", "first_look", "9lives"} {
		resp := c.Raw(http.MethodPost, "/investigators", apitest.ContentTypeJSON,
			`{"name":"`+bad+`","model_provider_id":"`+fxProvider.String()+`","prompt":"p"}`).
			MustStatus(t, http.StatusUnprocessableEntity)
		resp.MustViolate(t, "name")
	}
}

// TestAPatchReachesTheServiceFieldByField — the versioned fields are passed as given
// and folded by the service; an omitted one stays nil.
func TestAPatchReachesTheServiceFieldByField(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	c.PATCH(t, "/investigators/"+fxInvestigator.String(), map[string]any{
		"prompt":  "Say less.",
		"budgets": map[string]any{"max_steps": 5, "max_tokens": 5000, "max_wall_seconds": 60},
	}).MustStatus(t, http.StatusOK)
	f.mu.Lock()
	ch := f.change
	f.mu.Unlock()
	if ch.Prompt == nil || *ch.Prompt != "Say less." || ch.Tools != nil || ch.ProviderID != nil || ch.Enabled != nil {
		t.Fatalf("the change reached the service as %+v", ch)
	}
	if ch.Budgets == nil || ch.Budgets.MaxSteps != 5 || ch.Budgets.MaxWall != time.Minute {
		t.Fatalf("budgets reached the service as %+v", ch.Budgets)
	}
	if ch.MinInterval != nil {
		t.Fatalf("an omitted interval reached the service as %v", *ch.MinInterval)
	}
}

// TestAMinimumIntervalIsBoundedAndChangesInPlace — ADR 0053 §6: a number an operator
// reads back, 0 to 86400 seconds; it reaches the service as a change, not a version.
func TestAMinimumIntervalIsBoundedAndChangesInPlace(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	c.PATCH(t, "/investigators/"+fxInvestigator.String(), map[string]any{"min_interval_seconds": 0}).
		MustStatus(t, http.StatusOK)
	f.mu.Lock()
	ch := f.change
	f.mu.Unlock()
	if ch.MinInterval == nil || *ch.MinInterval != 0 || ch.TouchesVersion() {
		t.Fatalf("the change reached the service as %+v", ch)
	}
	for _, bad := range []int{-1, 86401} {
		resp := c.PATCH(t, "/investigators/"+fxInvestigator.String(), map[string]any{"min_interval_seconds": bad}).
			MustStatus(t, http.StatusUnprocessableEntity)
		schema.AssertProblem(t, "updateInvestigator", http.StatusUnprocessableEntity, resp.Body())
		resp.MustViolate(t, "min_interval_seconds")
	}
}

// --------------------------------------------------------- shared probes

func routes() []apitest.Route {
	inv := fxInvestigator.String()
	return []apitest.Route{
		{Op: "listModelProviders", Method: http.MethodGet, Path: "/model-providers"},
		{Op: "createModelProvider", Method: http.MethodPost, Path: "/model-providers",
			Body: `{"name":"g","base_url":"https://gw.example.test","model":"m"}`},
		{Op: "listInvestigators", Method: http.MethodGet, Path: "/investigators"},
		{Op: "createInvestigator", Method: http.MethodPost, Path: "/investigators", Body: createInvestigatorBody},
		{Op: "getInvestigator", Method: http.MethodGet, Path: "/investigators/" + inv},
		{Op: "updateInvestigator", Method: http.MethodPatch, Path: "/investigators/" + inv, Body: `{"enabled":false}`},
		{Op: "requestCaseInvestigation", Method: http.MethodPost, Path: "/cases/" + fxCase.String() + "/investigations",
			Body: `{"investigator_id":"` + inv + `"}`},
		{Op: "listCaseInvestigations", Method: http.MethodGet, Path: "/cases/" + fxCase.String() + "/investigations"},
		{Op: "getInvestigation", Method: http.MethodGet, Path: "/investigations/" + fxInvestigation.String()},
		{Op: "requestIncidentInvestigation", Method: http.MethodPost, Path: "/incidents/4/investigations",
			Body: `{"investigator_id":"` + inv + `"}`},
		{Op: "listIncidentInvestigations", Method: http.MethodGet, Path: "/incidents/4/investigations"},
		{Op: "listPolicyDigestInvestigations", Method: http.MethodGet, Path: "/notification-policies/" + fxPolicy.String() + "/investigations"},
	}
}

func world(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
	t.Helper()
	_, c := newClient(t)
	return c, nil
}

func TestEveryInvestigatorRouteRefusesAnAnonymousCaller(t *testing.T) {
	t.Parallel()
	apitest.AssertUnauthenticated(t, world, routes())
}

// TestAnotherOrgsResourceIsA404 — the same ids, asked by a member of
// apitest.OtherOrgID, and apitest.StrangerID asked by this org.
func TestAnotherOrgsResourceIsA404(t *testing.T) {
	t.Parallel()

	stranger := apitest.StrangerID.String()
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		t.Helper()
		_, c := newClient(t)
		return c.As(apitest.MemberOf(apitest.OtherOrgID)), nil
	}, []apitest.Route{
		{Op: "getInvestigator", Method: http.MethodGet, Path: "/investigators/" + fxInvestigator.String()},
		{Op: "updateInvestigator", Method: http.MethodPatch, Path: "/investigators/" + fxInvestigator.String(), Body: `{"enabled":false}`},
		{Op: "requestCaseInvestigation", Method: http.MethodPost, Path: "/cases/" + fxCase.String() + "/investigations",
			Body: `{"investigator_id":"` + fxInvestigator.String() + `"}`},
		{Op: "listCaseInvestigations", Method: http.MethodGet, Path: "/cases/" + fxCase.String() + "/investigations"},
		{Op: "getInvestigation", Method: http.MethodGet, Path: "/investigations/" + fxInvestigation.String()},
		{Op: "requestIncidentInvestigation", Method: http.MethodPost, Path: "/incidents/4/investigations",
			Body: `{"investigator_id":"` + fxInvestigator.String() + `"}`},
		{Op: "listIncidentInvestigations", Method: http.MethodGet, Path: "/incidents/4/investigations"},
		{Op: "listPolicyDigestInvestigations", Method: http.MethodGet, Path: "/notification-policies/" + fxPolicy.String() + "/investigations"},
	})
	apitest.AssertCrossTenant404(t, world, []apitest.Route{
		{Op: "getInvestigator", Name: "stranger investigator", Method: http.MethodGet, Path: "/investigators/" + stranger},
		{Op: "getInvestigation", Name: "stranger investigation", Method: http.MethodGet, Path: "/investigations/" + stranger},
		{Op: "listCaseInvestigations", Name: "stranger case", Method: http.MethodGet, Path: "/cases/" + stranger + "/investigations"},
		{Op: "listIncidentInvestigations", Name: "stranger incident", Method: http.MethodGet, Path: "/incidents/999999/investigations"},
		{Op: "listPolicyDigestInvestigations", Name: "stranger policy", Method: http.MethodGet, Path: "/notification-policies/" + stranger + "/investigations"},
		{Op: "getInvestigation", Name: "not a uuid", Method: http.MethodGet, Path: "/investigations/banana"},
	})
}

func TestAnUnknownQueryParameterIsRefused(t *testing.T) {
	t.Parallel()

	inv := fxInvestigator.String()
	apitest.AssertUnknownQueryParamRefused(t, world, []apitest.Route{
		{Op: "listModelProviders", Method: http.MethodGet, Path: "/model-providers?reveal=key"},
		{Op: "createModelProvider", Method: http.MethodPost, Path: "/model-providers?force=true",
			Body: `{"name":"g","base_url":"https://gw.example.test","model":"m"}`},
		{Op: "listInvestigators", Method: http.MethodGet, Path: "/investigators?enabled=true"},
		{Op: "createInvestigator", Method: http.MethodPost, Path: "/investigators?force=true", Body: createInvestigatorBody},
		{Op: "getInvestigator", Method: http.MethodGet, Path: "/investigators/" + inv + "?include=steps"},
		{Op: "updateInvestigator", Method: http.MethodPatch, Path: "/investigators/" + inv + "?force=true", Body: `{"enabled":false}`},
		{Op: "requestCaseInvestigation", Method: http.MethodPost, Path: "/cases/" + fxCase.String() + "/investigations?wait=true",
			Body: `{"investigator_id":"` + inv + `"}`},
		{Op: "listCaseInvestigations", Method: http.MethodGet, Path: "/cases/" + fxCase.String() + "/investigations?status=failed"},
		{Op: "getInvestigation", Method: http.MethodGet, Path: "/investigations/" + fxInvestigation.String() + "?include=key"},
		{Op: "requestIncidentInvestigation", Method: http.MethodPost, Path: "/incidents/4/investigations?wait=true",
			Body: `{"investigator_id":"` + inv + `"}`},
		{Op: "listIncidentInvestigations", Method: http.MethodGet, Path: "/incidents/4/investigations?status=failed"},
		{Op: "listPolicyDigestInvestigations", Method: http.MethodGet, Path: "/notification-policies/" + fxPolicy.String() + "/investigations?status=skipped"},
	})
}
