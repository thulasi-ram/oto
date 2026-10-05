package api

// WHO MAY APPROVE A REMEDY ON A TOOLSERVER, CHECKED AGAINST THE CONTRACT (ADR 0054 §4,
// git-bug 47f67c8).
//
//   - the list answers the shape the contract declares, a disabled holder marked as not
//     counting;
//   - another org's ToolServer is a 404, an anonymous caller a 401, an unknown parameter a 400;
//   - ⛔ THE GRANT IS READ-ONLY OVER HTTP: walking this package's routes finds one route on
//     it, a GET. The whole mounted tree is walked by test/scope/remedy_approver_routes_test.go.

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
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

func (f *fakeInvestigators) RemedyApprovers(_ context.Context, s db.TenantScope, id uuid.UUID) ([]domain.RemedyApprover, error) {
	if err := f.ownsToolServer(s, id); err != nil {
		return nil, err
	}
	out := []domain.RemedyApprover{}
	if id == fxWriteToolServer {
		at := fxEpoch.Add(time.Hour)
		out = []domain.RemedyApprover{
			{UserID: uuid.New(), Email: "ada@example.test", DisplayName: "Ada Lovelace", GrantedAt: at, GrantedBy: "cli", Counts: true},
			{UserID: uuid.New(), Email: "bob@example.test", DisplayName: "Bob", GrantedAt: at, GrantedBy: "cli", Counts: false},
		}
	}
	return out, nil
}

func TestTheRemedyApproverListAnswersItsContractShape(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)

	resp := c.GET("/tool-servers/"+fxWriteToolServer.String()+"/remedy-approvers").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listToolServerRemedyApprovers", http.StatusOK, resp.Body())
	data := resp.JSON(t)["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("%d approvers listed, want 2", len(data))
	}
	ada, bob := data[0].(map[string]any), data[1].(map[string]any)
	if ada["counts"] != true || ada["granted_by"] != "cli" || bob["counts"] != false {
		t.Fatalf("approvers = %v", data)
	}

	// A read ToolServer carries none, and still answers the shape.
	resp = c.GET("/tool-servers/"+fxToolServer.String()+"/remedy-approvers").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listToolServerRemedyApprovers", http.StatusOK, resp.Body())
}

func TestTheRemedyApproverListRefusesAStrangerAnAnonymousCallerAndAnUnknownParameter(t *testing.T) {
	t.Parallel()
	path := "/tool-servers/" + fxWriteToolServer.String() + "/remedy-approvers"
	routes := []apitest.Route{{Op: "listToolServerRemedyApprovers", Method: http.MethodGet, Path: path}}
	apitest.AssertUnauthenticated(t, world, routes)
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		t.Helper()
		_, c := newClient(t)
		return c.As(apitest.MemberOf(apitest.OtherOrgID)), nil
	}, routes)
	apitest.AssertUnknownQueryParamRefused(t, world, []apitest.Route{
		{Op: "listToolServerRemedyApprovers", Method: http.MethodGet, Path: path + "?email=ada@example.test"},
	})
}

// TestTheRemedyApproverGrantHasNoRouteButARead — ⛔ ADR 0054 §4: a grant is given and taken
// by `oto grant` / `oto revoke` from the host shell. Any route on it that is not a GET
// would let one holder mint a second approver and defeat double approval.
func TestTheRemedyApproverGrantHasNoRouteButARead(t *testing.T) {
	t.Parallel()
	r := chi.NewRouter()
	NewRouter(&fakeInvestigators{}, clock.New()).Mount(r)

	var onGrants []string
	err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		p := strings.ToLower(pattern)
		if strings.Contains(p, "approver") || strings.Contains(p, "grant") {
			onGrants = append(onGrants, method+" "+pattern)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /tool-servers/{id}/remedy-approvers"}
	if strings.Join(onGrants, ",") != strings.Join(want, ",") {
		t.Fatalf("routes on a Remedy approver grant = %v, want exactly %v", onGrants, want)
	}
}
