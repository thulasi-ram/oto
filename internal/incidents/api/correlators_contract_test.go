package api

// THE CORRELATOR TRANSPORT, CHECKED AGAINST THE CONTRACT ITSELF (ADR 0052 §2,
// git-bug 61eeddf).
//
// Every success body goes through `schema.Assert` and every refusal through
// `schema.AssertProblem`, so this file is not a second copy of the contract. What
// it protects beyond the shapes:
//
//   - a Correlator's matchers are the notification-policy grammar on the wire, and
//     a malformed one is refused at the door;
//   - the count condition's halves are nullable separately and merged against the
//     stored row, so a PATCH naming one half keeps the other;
//   - another org's Correlator id is a 404, never a 403.

import (
	"context"
	"net/http"
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

var contractCorrelatorID = uuid.MustParse("55555555-5555-4555-8555-555555555555")

// fakeCorrelators owns exactly ONE Correlator, in apitest.OrgID, and answers 404
// for every other id and every other org.
type fakeCorrelators struct {
	mu      sync.Mutex
	stored  domain.Correlator
	patches []domain.CorrelatorPatch
	drafts  []domain.CorrelatorDraft
}

func newFakeCorrelators() *fakeCorrelators {
	return &fakeCorrelators{stored: domain.Correlator{
		ID: contractCorrelatorID, Name: "payments storm", Priority: 100, Enabled: true,
		Matchers: []kernel.Matcher{
			{Name: "cluster", Op: kernel.OpEqual, Value: "prod"},
			{Name: "namespace", Op: kernel.OpEqual, Value: "payments"},
		},
		Count:     domain.Count{Min: 5, Window: 600 * time.Second},
		CreatedAt: contractEpoch, UpdatedAt: contractEpoch,
	}}
}

func (f *fakeCorrelators) owns(s db.TenantScope, id uuid.UUID) bool {
	return s.OrgID() == apitest.OrgID && id == contractCorrelatorID
}

func (f *fakeCorrelators) List(_ context.Context, s db.TenantScope) ([]domain.Correlator, error) {
	if s.OrgID() != apitest.OrgID {
		return nil, nil
	}
	return []domain.Correlator{f.stored}, nil
}

func (f *fakeCorrelators) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Correlator, error) {
	if !f.owns(s, id) {
		return domain.Correlator{}, domain.CorrelatorNotFound()
	}
	return f.stored, nil
}

func (f *fakeCorrelators) Create(_ context.Context, _ db.TenantScope, d domain.CorrelatorDraft) (domain.Correlator, error) {
	f.mu.Lock()
	f.drafts = append(f.drafts, d)
	f.mu.Unlock()
	k := d.Correlator()
	if err := k.Validate(); err != nil {
		return domain.Correlator{}, err
	}
	k.ID, k.CreatedAt, k.UpdatedAt = contractCorrelatorID, contractEpoch, contractEpoch
	return k, nil
}

func (f *fakeCorrelators) Update(
	_ context.Context, s db.TenantScope, id uuid.UUID, p domain.CorrelatorPatch,
) (domain.Correlator, error) {
	f.mu.Lock()
	f.patches = append(f.patches, p)
	f.mu.Unlock()
	if !f.owns(s, id) {
		return domain.Correlator{}, domain.CorrelatorNotFound()
	}
	merged := p.Apply(f.stored)
	if err := merged.Validate(); err != nil {
		return domain.Correlator{}, err
	}
	return merged, nil
}

func (f *fakeCorrelators) Delete(_ context.Context, s db.TenantScope, id uuid.UUID) error {
	if !f.owns(s, id) {
		return domain.CorrelatorNotFound()
	}
	return nil
}

func newCorrelatorClient(t *testing.T) (*fakeCorrelators, *apitest.Client) {
	t.Helper()
	f := newFakeCorrelators()
	return f, apitest.New(NewCorrelatorRouter(f, clock.New()))
}

const correlatorPath = "/correlators/55555555-5555-4555-8555-555555555555"

// TestEveryCorrelatorOperationAnswersItsContractShape drives all four operations
// once and validates the bytes against the declared schema.
func TestEveryCorrelatorOperationAnswersItsContractShape(t *testing.T) {
	t.Parallel()

	_, c := newCorrelatorClient(t)

	resp := c.GET("/correlators").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listCorrelators", http.StatusOK, resp.Body())

	resp = c.POST(t, "/correlators", map[string]any{
		"name":     "criticals",
		"matchers": []map[string]string{{"name": "severity", "op": "=", "value": "critical"}},
	}).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createCorrelator", http.StatusCreated, resp.Body())

	resp = c.PATCH(t, correlatorPath, map[string]any{"priority": 10}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateCorrelator", http.StatusOK, resp.Body())

	resp = c.DELETE(correlatorPath).MustStatus(t, http.StatusNoContent)
	schema.AssertNoBody(t, "deleteCorrelator", http.StatusNoContent, resp.Body())
}

// TestAPatchNamingOneCountHalfKeepsTheOther — the halves are nullable separately on
// the wire and merged against the stored row, so widening the window does not
// silently drop the threshold, and clearing only one half is a 422.
func TestAPatchNamingOneCountHalfKeepsTheOther(t *testing.T) {
	t.Parallel()

	f, c := newCorrelatorClient(t)
	c.PATCH(t, correlatorPath, map[string]any{"count_window_seconds": 900}).MustStatus(t, http.StatusOK)
	f.mu.Lock()
	got := f.patches[0].Count
	f.mu.Unlock()
	if got == nil || got.Min != 5 || got.Window != 900*time.Second {
		t.Fatalf("the merged count reached the service as %+v, want min 5 over 900 s", got)
	}

	resp := c.PATCH(t, correlatorPath, map[string]any{"count_min": nil}).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "updateCorrelator", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "count_min")
}

// TestTheQuietGraceIsSetAndClearedOnTheWire — git-bug 34a27c5. A number sets the
// grace, an explicit `null` clears it (join only while active), and omitting it
// leaves it alone; the response carries it back.
func TestTheQuietGraceIsSetAndClearedOnTheWire(t *testing.T) {
	t.Parallel()

	f, c := newCorrelatorClient(t)
	resp := c.PATCH(t, correlatorPath, map[string]any{"quiet_grace_seconds": 1800}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateCorrelator", http.StatusOK, resp.Body())
	c.PATCH(t, correlatorPath, map[string]any{"quiet_grace_seconds": nil}).MustStatus(t, http.StatusOK)
	c.PATCH(t, correlatorPath, map[string]any{"priority": 5}).MustStatus(t, http.StatusOK)

	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.patches[0].QuietGrace; g == nil || *g != 1800*time.Second {
		t.Fatalf("a number reached the service as %v, want 30m", g)
	}
	if g := f.patches[1].QuietGrace; g == nil || *g != 0 {
		t.Fatalf("an explicit null reached the service as %v, want a clear", g)
	}
	if g := f.patches[2].QuietGrace; g != nil {
		t.Fatalf("an omitted grace reached the service as %v, want untouched", *g)
	}

	resp = c.PATCH(t, correlatorPath, map[string]any{"quiet_grace_seconds": 30}).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "updateCorrelator", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "quiet_grace_seconds")
}

// TestAMalformedMatcherIsRefusedAtTheDoor — the policy grammar's operators and
// label-name rule hold for a Correlator too.
func TestAMalformedMatcherIsRefusedAtTheDoor(t *testing.T) {
	t.Parallel()

	f, c := newCorrelatorClient(t)
	resp := c.POST(t, "/correlators", map[string]any{
		"name":     "broken",
		"matchers": []map[string]string{{"name": "severity", "op": "~=", "value": "critical"}},
	}).MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "createCorrelator", http.StatusUnprocessableEntity, resp.Body())
	if len(f.drafts) != 0 {
		t.Fatalf("a malformed matcher reached the service")
	}
}

func correlatorRoutes() []apitest.Route {
	return []apitest.Route{
		{Op: "listCorrelators", Method: http.MethodGet, Path: "/correlators"},
		{Op: "createCorrelator", Method: http.MethodPost, Path: "/correlators", Body: `{"name":"criticals"}`},
		{Op: "updateCorrelator", Method: http.MethodPatch, Path: correlatorPath, Body: `{"priority":10}`},
		{Op: "deleteCorrelator", Method: http.MethodDelete, Path: correlatorPath},
	}
}

func correlatorWorld(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
	t.Helper()
	_, c := newCorrelatorClient(t)
	return c, nil
}

func TestEveryCorrelatorRouteRefusesAnAnonymousCaller(t *testing.T) {
	t.Parallel()
	apitest.AssertUnauthenticated(t, correlatorWorld, correlatorRoutes())
}

// TestAnotherOrgsCorrelatorIsA404 — the one Correlator id, and apitest.StrangerID,
// asked by a member of another org: indistinguishable from "never written".
func TestAnotherOrgsCorrelatorIsA404(t *testing.T) {
	t.Parallel()

	stranger := "/correlators/" + apitest.StrangerID.String()
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		t.Helper()
		_, c := newCorrelatorClient(t)
		return c.As(apitest.MemberOf(apitest.OtherOrgID)), nil
	}, []apitest.Route{
		{Op: "updateCorrelator", Method: http.MethodPatch, Path: correlatorPath, Body: `{"priority":10}`},
		{Op: "deleteCorrelator", Method: http.MethodDelete, Path: correlatorPath},
		{Op: "updateCorrelator", Name: "a stranger's id", Method: http.MethodPatch, Path: stranger, Body: `{"priority":10}`},
		{Op: "deleteCorrelator", Name: "a stranger's id", Method: http.MethodDelete, Path: stranger},
		{Op: "deleteCorrelator", Name: "not a uuid", Method: http.MethodDelete, Path: "/correlators/banana"},
	})
}

func TestAnUnknownCorrelatorQueryParameterIsRefused(t *testing.T) {
	t.Parallel()
	apitest.AssertUnknownQueryParamRefused(t, correlatorWorld, []apitest.Route{
		{Op: "listCorrelators", Method: http.MethodGet, Path: "/correlators?enabled=true"},
		{Op: "createCorrelator", Method: http.MethodPost, Path: "/correlators?force=true", Body: `{"name":"x"}`},
		{Op: "updateCorrelator", Method: http.MethodPatch, Path: correlatorPath + "?force=true", Body: `{"priority":10}`},
		{Op: "deleteCorrelator", Method: http.MethodDelete, Path: correlatorPath + "?force=true"},
	})
}

// TestTheConversationSettingRoundTripsOnTheWire — git-bug bf5fc7e. `true` makes the
// Correlator's Incidents conversations (ADR 0052 §6), `false` turns it back off,
// omitting it leaves it alone, and the response carries the stored value back.
func TestTheConversationSettingRoundTripsOnTheWire(t *testing.T) {
	t.Parallel()

	f, c := newCorrelatorClient(t)
	resp := c.PATCH(t, correlatorPath, map[string]any{"incidents_are_conversations": true}).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateCorrelator", http.StatusOK, resp.Body())
	c.PATCH(t, correlatorPath, map[string]any{"incidents_are_conversations": false}).MustStatus(t, http.StatusOK)
	c.PATCH(t, correlatorPath, map[string]any{"priority": 5}).MustStatus(t, http.StatusOK)

	f.mu.Lock()
	defer f.mu.Unlock()
	if v := f.patches[0].Conversations; v == nil || !*v {
		t.Fatalf("`true` reached the service as %v", v)
	}
	if v := f.patches[1].Conversations; v == nil || *v {
		t.Fatalf("`false` reached the service as %v", v)
	}
	if v := f.patches[2].Conversations; v != nil {
		t.Fatalf("an omitted setting reached the service as %v, want untouched", *v)
	}
}
