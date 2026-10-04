package api

// A FINDING'S SUGGESTIONS, CHECKED AGAINST THE CONTRACT (ADR 0053 §2, git-bug 8327c00).
//
// The properties this file protects:
//
//   - the list and the apply answer the shapes the contract declares;
//   - a membership Suggestion that would move its Case says from where, BEFORE it is
//     applied, and an apply that did not confirm it is a typed 409;
//   - a lapsed or applied Suggestion is a typed 409, never a silent second edit;
//   - applying needs a human: a system principal is a 403 before the service is reached;
//   - ⛔ THE ONLY VERB IS APPLY: walking the mounted routes finds no other route on a
//     Suggestion, and no segment anywhere in this package that declines, rejects or hides
//     one.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

var (
	fxSuggestCount  = uuid.MustParse("88888888-8888-4888-8888-888888888881")
	fxSuggestMove   = uuid.MustParse("88888888-8888-4888-8888-888888888882")
	fxSuggestLapsed = uuid.MustParse("88888888-8888-4888-8888-888888888883")
	fxPolicy        = uuid.MustParse("99999999-9999-4999-8999-999999999999")
)

func fxSuggestions() []domain.Suggestion {
	at := fxEpoch.Add(time.Minute)
	return []domain.Suggestion{
		{ID: fxSuggestCount, OrgID: apitest.OrgID, InvestigationID: fxInvestigation, Kind: domain.SuggestCountCondition,
			Count: domain.CountChange{PolicyID: fxPolicy, PolicyName: "crashloops → #platform", CountMin: 3,
				CountWindow: 10 * time.Minute, WasMin: 2, WasWindow: 5 * time.Minute},
			Why: "It flaps on every deploy.", ProposedAt: at, LapsesAt: at.Add(domain.SuggestionLapse)},
		{ID: fxSuggestMove, OrgID: apitest.OrgID, InvestigationID: fxInvestigation, Kind: domain.SuggestMembership,
			Membership: domain.MembershipChange{IncidentID: fxIncident, IncidentNumber: fxIncidentNumber, CaseID: fxCase, CaseNumber: 412},
			Why:        "Same namespace, same minute.", ProposedAt: at, LapsesAt: at.Add(domain.SuggestionLapse),
			MovesFrom: domain.IncidentRef{ID: uuid.New(), Number: 2}},
	}
}

func (f *fakeInvestigators) ListSuggestions(_ context.Context, s db.TenantScope, id uuid.UUID) ([]domain.Suggestion, error) {
	if !mine(s) || id != fxInvestigation {
		return nil, errs.NotFound("investigation_not_found", "no such Investigation")
	}
	return fxSuggestions(), nil
}

func (f *fakeInvestigators) ApplySuggestion(
	_ context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester, movesFrom int64,
) (domain.Suggestion, error) {
	f.record("apply")
	if !mine(s) {
		return domain.Suggestion{}, domain.SuggestionNotFound()
	}
	for _, sg := range fxSuggestions() {
		if sg.ID != id {
			continue
		}
		if sg.Kind == domain.SuggestMembership && movesFrom != sg.MovesFrom.Number {
			return domain.Suggestion{}, domain.SuggestionMovesCase(sg.Membership, sg.MovesFrom)
		}
		sg.AppliedAt, sg.AppliedBy, sg.MovesFrom = fxEpoch.Add(time.Hour), by, domain.IncidentRef{}
		f.mu.Lock()
		f.requester = by
		f.mu.Unlock()
		return sg, nil
	}
	if id == fxSuggestLapsed {
		lapsed := domain.Suggestion{LapsesAt: fxEpoch}
		return domain.Suggestion{}, lapsed.Applicable(fxEpoch.Add(time.Hour))
	}
	return domain.Suggestion{}, domain.SuggestionNotFound()
}

func TestTheSuggestionOperationsAnswerTheirContractShapes(t *testing.T) {
	t.Parallel()
	f, c := newClient(t)

	resp := c.GET("/investigations/"+fxInvestigation.String()+"/suggestions").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listInvestigationSuggestions", http.StatusOK, resp.Body())
	data := resp.JSON(t)["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("%d Suggestions listed", len(data))
	}
	move := data[1].(map[string]any)["membership"].(map[string]any)
	if move["moves_from_incident_number"] != float64(2) {
		t.Fatalf("the move is not said before it is applied: %v", move)
	}

	resp = c.POST(t, "/suggestions/"+fxSuggestCount.String()+"/apply", map[string]any{}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "applySuggestion", http.StatusOK, resp.Body())
	got := resp.JSON(t)["data"].(map[string]any)
	if got["state"] != "applied" || got["applied_by_label"] != "Ada Lovelace" {
		t.Fatalf("applied = %v", got)
	}
	if f.requester.Label != "Ada Lovelace" {
		t.Fatalf("the applier reached the service as %+v", f.requester)
	}

	resp = c.POST(t, "/suggestions/"+fxSuggestMove.String()+"/apply", map[string]any{"moves_from_incident_number": 2}).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "applySuggestion", http.StatusOK, resp.Body())
}

// TestASuggestionThatCannotBeAppliedIsATyped409 — an unconfirmed move, and a lapsed one.
func TestASuggestionThatCannotBeAppliedIsATyped409(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)

	resp := c.POST(t, "/suggestions/"+fxSuggestMove.String()+"/apply", map[string]any{}).MustStatus(t, http.StatusConflict)
	schema.AssertProblem(t, "applySuggestion", http.StatusConflict, resp.Body())
	if p := resp.Problem(t); p.Code != "suggestion_moves_case" || !strings.Contains(p.Detail, "from Incident #2") {
		t.Fatalf("problem = %+v", p)
	}

	resp = c.POST(t, "/suggestions/"+fxSuggestLapsed.String()+"/apply", map[string]any{}).MustStatus(t, http.StatusConflict)
	schema.AssertProblem(t, "applySuggestion", http.StatusConflict, resp.Body())
	if p := resp.Problem(t); p.Code != "suggestion_lapsed" {
		t.Fatalf("problem = %+v", p)
	}

	resp = c.POST(t, "/suggestions/"+apitest.StrangerID.String()+"/apply", map[string]any{}).MustStatus(t, http.StatusNotFound)
	schema.AssertProblem(t, "applySuggestion", http.StatusNotFound, resp.Body())
}

// TestOnlyAHumanAppliesASuggestion — the Investigator never applies its own, and neither
// does any other machine.
func TestOnlyAHumanAppliesASuggestion(t *testing.T) {
	t.Parallel()
	f, c := newClient(t)
	resp := c.As(apitest.Machine()).POST(t, "/suggestions/"+fxSuggestCount.String()+"/apply", map[string]any{}).
		MustStatus(t, http.StatusForbidden)
	schema.AssertProblem(t, "applySuggestion", http.StatusForbidden, resp.Body())
	if f.callCount() != 0 {
		t.Fatal("a machine's apply reached the service")
	}
}

func suggestionRoutes() []apitest.Route {
	return []apitest.Route{
		{Op: "listInvestigationSuggestions", Method: http.MethodGet, Path: "/investigations/" + fxInvestigation.String() + "/suggestions"},
		{Op: "applySuggestion", Method: http.MethodPost, Path: "/suggestions/" + fxSuggestCount.String() + "/apply", Body: `{}`},
	}
}

func TestEverySuggestionRouteRefusesAnAnonymousCallerAndAnotherOrg(t *testing.T) {
	t.Parallel()
	apitest.AssertUnauthenticated(t, world, suggestionRoutes())
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		t.Helper()
		_, c := newClient(t)
		return c.As(apitest.MemberOf(apitest.OtherOrgID)), nil
	}, suggestionRoutes())
	apitest.AssertUnknownQueryParamRefused(t, world, []apitest.Route{
		{Op: "listInvestigationSuggestions", Method: http.MethodGet,
			Path: "/investigations/" + fxInvestigation.String() + "/suggestions?state=lapsed"},
		{Op: "applySuggestion", Method: http.MethodPost, Path: "/suggestions/" + fxSuggestCount.String() + "/apply?force=true",
			Body: `{}`},
	})
}

// TestApplyIsTheOnlyVerbOnASuggestion walks the routes this package mounts. ⛔ A Suggestion
// is applied or it lapses (ADR 0053 §2): a second verb on it — however spelled — would be a
// way to keep it in front of somebody until they answered, which is a queue (H-1).
func TestApplyIsTheOnlyVerbOnASuggestion(t *testing.T) {
	t.Parallel()
	r := chi.NewRouter()
	NewRouter(&fakeInvestigators{}, clock.New()).Mount(r)

	var onSuggestions []string
	refusals := []string{"declin", "reject", "dismiss", "ignor", "hide", "snooze", "refus", "veto"}
	err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route := method + " " + pattern
		if strings.Contains(pattern, "suggestion") {
			onSuggestions = append(onSuggestions, route)
		}
		for _, seg := range strings.Split(strings.ToLower(pattern), "/") {
			for _, verb := range refusals {
				if strings.Contains(seg, verb) {
					t.Errorf("%s: a route that %ss something is mounted", route, verb)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /investigations/{id}/suggestions", "POST /suggestions/{id}/apply"}
	if strings.Join(onSuggestions, ",") != strings.Join(want, ",") {
		t.Fatalf("routes on a Suggestion = %v, want exactly %v", onSuggestions, want)
	}
}
