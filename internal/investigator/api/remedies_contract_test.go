package api

// A FINDING'S REMEDIES, CHECKED AGAINST THE CONTRACT (ADR 0054, git-bug 4148256).
//
// The properties this file protects:
//
//   - the list, the read, the approve and the decline answer the shapes the contract declares;
//   - ⭐ the exact command is on the wire: the Tool, the exact arguments and their hash — or
//     `no_tool`, the sentence, and no arguments;
//   - approving a Remedy with no Tool is the typed `409 remedy_has_no_tool`;
//   - approving names the hash of the arguments approved, and a body without one is refused;
//   - a human approves and a human declines: a system principal is a 403 before the service is
//     reached.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

var (
	fxRemedyTool   = uuid.MustParse("abababab-abab-4bab-8bab-ababababab01")
	fxRemedyNoTool = uuid.MustParse("abababab-abab-4bab-8bab-ababababab02")
	fxWriteServer  = uuid.MustParse("abababab-abab-4bab-8bab-ababababab03")
)

const fxRemedyArgs = `{"namespace":"checkout","deployment":"api","replicas":12345678901234567890}`

func fxRemedies() []domain.Remedy {
	at := time.Now().UTC().Add(-time.Minute)
	expires := at.Add(time.Hour)
	tool := domain.Remedy{ID: fxRemedyTool, OrgID: apitest.OrgID, InvestigationID: fxInvestigation,
		SubjectKind: domain.SubjectIncident, SubjectID: fxIncident, ProposedBy: "Investigator firstlook v2",
		Tool:      domain.RemedyTool{ToolServerID: fxWriteServer, ToolServerName: "k8s-write", Tool: "scale"},
		Arguments: fxRemedyArgs, ArgumentsSHA256: domain.HashArguments(fxRemedyArgs),
		Target: "Deployment checkout/api", Description: "Scale the api back up after the bad deploy.",
		RequiredApprovals: domain.DefaultRequiredApprovals, State: domain.RemedyProposed, ProposedAt: at, ExpiresAt: expires,
		Approvals: []domain.RemedyApproval{{UserID: uuid.New(), Label: "Grace Hopper", ArgumentsSHA256: domain.HashArguments(fxRemedyArgs), ApprovedAt: at}},
		Transitions: []domain.RemedyTransition{{ID: uuid.New(), To: domain.RemedyProposed,
			Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: "Investigator firstlook v2"}, At: at,
			DeclaredIncidentID: fxIncident}}}
	none := domain.Remedy{ID: fxRemedyNoTool, OrgID: apitest.OrgID, InvestigationID: fxInvestigation,
		SubjectKind: domain.SubjectCase, SubjectID: fxCase, ProposedBy: "Investigator firstlook v2",
		Target: "the node pool eu-1", Description: "Add a node; the pool is out of memory.",
		RequiredApprovals: domain.DefaultRequiredApprovals, State: domain.RemedyProposed, ProposedAt: at, ExpiresAt: expires,
		Approvals: []domain.RemedyApproval{}, Blocked: domain.NoToolCanCarryItOut,
		Transitions: []domain.RemedyTransition{{ID: uuid.New(), To: domain.RemedyProposed,
			Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: "Investigator firstlook v2"}, At: at}}}
	return []domain.Remedy{tool, none}
}

func (f *fakeInvestigators) ListRemedies(_ context.Context, s db.TenantScope, id uuid.UUID) ([]domain.Remedy, error) {
	if !mine(s) || id != fxInvestigation {
		return nil, errs.NotFound("investigation_not_found", "no such Investigation")
	}
	return fxRemedies(), nil
}

func fxRemedy(s db.TenantScope, id uuid.UUID) (domain.Remedy, error) {
	if !mine(s) {
		return domain.Remedy{}, domain.RemedyNotFound()
	}
	for _, r := range fxRemedies() {
		if r.ID == id {
			return r, nil
		}
	}
	return domain.Remedy{}, domain.RemedyNotFound()
}

func (f *fakeInvestigators) GetRemedy(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Remedy, error) {
	return fxRemedy(s, id)
}

func (f *fakeInvestigators) ApproveRemedy(
	_ context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester, hash string,
) (domain.Remedy, error) {
	f.record("approve")
	r, err := fxRemedy(s, id)
	if err != nil {
		return domain.Remedy{}, err
	}
	if err := r.Approvable(time.Now()); err != nil {
		return domain.Remedy{}, err
	}
	if hash != r.ArgumentsSHA256 {
		return domain.Remedy{}, domain.RemedyArgumentsMismatch()
	}
	f.mu.Lock()
	f.requester = by
	f.mu.Unlock()
	r.Approvals = append(r.Approvals, domain.RemedyApproval{UserID: by.UserID, Label: by.Label, ArgumentsSHA256: hash, ApprovedAt: time.Now()})
	r.State, r.ApprovedAt = domain.RemedyApproved, time.Now().UTC()
	return r, nil
}

func (f *fakeInvestigators) DeclineRemedy(_ context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester) (domain.Remedy, error) {
	f.record("decline")
	r, err := fxRemedy(s, id)
	if err != nil {
		return domain.Remedy{}, err
	}
	r.State, r.EndedAt = domain.RemedyDeclined, time.Now().UTC()
	r.Transitions = append(r.Transitions, domain.RemedyTransition{ID: uuid.New(), From: domain.RemedyProposed,
		To: domain.RemedyDeclined, Actor: domain.UserActor(by), At: r.EndedAt})
	return r, nil
}

func TestTheRemedyOperationsAnswerTheirContractShapes(t *testing.T) {
	t.Parallel()
	f, c := newClient(t)

	resp := c.GET("/investigations/"+fxInvestigation.String()+"/remedies").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listInvestigationRemedies", http.StatusOK, resp.Body())
	data := resp.JSON(t)["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("%d Remedies listed", len(data))
	}
	// ⭐ THE EXACT ARGUMENTS ARE ON THE WIRE BYTE FOR BYTE — the 20-digit number included,
	// which a float would have rounded — with their hash.
	if !strings.Contains(resp.String(), `"arguments":"{\"namespace\":\"checkout\",\"deployment\":\"api\",\"replicas\":12345678901234567890}"`) {
		t.Fatalf("the exact arguments are not on the wire: %s", resp.String())
	}
	withTool, without := data[0].(map[string]any), data[1].(map[string]any)
	if withTool["arguments_sha256"] != domain.HashArguments(fxRemedyArgs) || withTool["no_tool"] != nil ||
		withTool["tool"].(map[string]any)["tool_name"] != "scale" {
		t.Fatalf("a Remedy with a Tool = %v", withTool)
	}
	if without["no_tool"] != domain.NoToolCanCarryItOut || without["arguments"] != nil || without["tool"] != nil ||
		without["blocked"] != domain.NoToolCanCarryItOut {
		t.Fatalf("a Remedy with no Tool does not say so: %v", without)
	}

	resp = c.GET("/remedies/"+fxRemedyTool.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getRemedy", http.StatusOK, resp.Body())

	resp = c.POST(t, "/remedies/"+fxRemedyTool.String()+"/approve",
		map[string]any{"arguments_sha256": domain.HashArguments(fxRemedyArgs)}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "approveRemedy", http.StatusOK, resp.Body())
	if f.requester.Label != "Ada Lovelace" {
		t.Fatalf("the approver reached the service as %+v", f.requester)
	}

	resp = c.Raw(http.MethodPost, "/remedies/"+fxRemedyNoTool.String()+"/decline", "", "").MustStatus(t, http.StatusOK)
	schema.Assert(t, "declineRemedy", http.StatusOK, resp.Body())
	if resp.JSON(t)["data"].(map[string]any)["state"] != "declined" {
		t.Fatalf("declined = %s", resp.String())
	}
}

// TestARemedyWithNoToolIsRefusedOnApprove — ruling of 2026-10-02: it cannot be approved, and
// the API says so with a typed error.
func TestARemedyWithNoToolIsRefusedOnApprove(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)
	resp := c.POST(t, "/remedies/"+fxRemedyNoTool.String()+"/approve",
		map[string]any{"arguments_sha256": strings.Repeat("0", 64)}).MustStatus(t, http.StatusConflict)
	schema.AssertProblem(t, "approveRemedy", http.StatusConflict, resp.Body())
	if p := resp.Problem(t); p.Code != "remedy_has_no_tool" || !strings.Contains(p.Detail, domain.NoToolCanCarryItOut) {
		t.Fatalf("problem = %+v", p)
	}
}

// TestAnApprovalNamesTheArgumentsItApproves — a body without the hash, or with one that is not
// a SHA-256, is refused before the service; one that is not this Remedy's is a typed 409.
func TestAnApprovalNamesTheArgumentsItApproves(t *testing.T) {
	t.Parallel()
	f, c := newClient(t)
	for _, body := range []map[string]any{{}, {"arguments_sha256": "abc"}, {"arguments_sha256": strings.Repeat("Z", 64)}} {
		resp := c.POST(t, "/remedies/"+fxRemedyTool.String()+"/approve", body)
		if resp.Code() != http.StatusBadRequest && resp.Code() != http.StatusUnprocessableEntity {
			t.Fatalf("%v: status %d, want a refusal of the body", body, resp.Code())
		}
	}
	if f.callCount() != 0 {
		t.Fatal("a body naming no valid hash reached the service")
	}
	resp := c.POST(t, "/remedies/"+fxRemedyTool.String()+"/approve",
		map[string]any{"arguments_sha256": strings.Repeat("a", 64)}).MustStatus(t, http.StatusConflict)
	if p := resp.Problem(t); p.Code != "remedy_arguments_changed" {
		t.Fatalf("problem = %+v", p)
	}
}

// TestOnlyAHumanApprovesOrDeclinesARemedy — the Investigator never approves its own Remedy,
// and neither does any other machine.
func TestOnlyAHumanApprovesOrDeclinesARemedy(t *testing.T) {
	t.Parallel()
	f, c := newClient(t)
	resp := c.As(apitest.Machine()).POST(t, "/remedies/"+fxRemedyTool.String()+"/approve",
		map[string]any{"arguments_sha256": domain.HashArguments(fxRemedyArgs)}).MustStatus(t, http.StatusForbidden)
	schema.AssertProblem(t, "approveRemedy", http.StatusForbidden, resp.Body())
	resp = c.As(apitest.Machine()).Raw(http.MethodPost, "/remedies/"+fxRemedyTool.String()+"/decline", "", "").
		MustStatus(t, http.StatusForbidden)
	schema.AssertProblem(t, "declineRemedy", http.StatusForbidden, resp.Body())
	if f.callCount() != 0 {
		t.Fatal("a machine reached the service")
	}
}

func remedyRoutes() []apitest.Route {
	return []apitest.Route{
		{Op: "listInvestigationRemedies", Method: http.MethodGet, Path: "/investigations/" + fxInvestigation.String() + "/remedies"},
		{Op: "getRemedy", Method: http.MethodGet, Path: "/remedies/" + fxRemedyTool.String()},
		{Op: "approveRemedy", Method: http.MethodPost, Path: "/remedies/" + fxRemedyTool.String() + "/approve",
			Body: `{"arguments_sha256":"` + domain.HashArguments(fxRemedyArgs) + `"}`},
		{Op: "declineRemedy", Method: http.MethodPost, Path: "/remedies/" + fxRemedyTool.String() + "/decline"},
	}
}

func TestEveryRemedyRouteRefusesAnAnonymousCallerAndAnotherOrg(t *testing.T) {
	t.Parallel()
	apitest.AssertUnauthenticated(t, world, remedyRoutes())
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		t.Helper()
		_, c := newClient(t)
		return c.As(apitest.MemberOf(apitest.OtherOrgID)), nil
	}, remedyRoutes())
	apitest.AssertUnknownQueryParamRefused(t, world, []apitest.Route{
		{Op: "listInvestigationRemedies", Method: http.MethodGet,
			Path: "/investigations/" + fxInvestigation.String() + "/remedies?state=proposed"},
		{Op: "approveRemedy", Method: http.MethodPost, Path: "/remedies/" + fxRemedyTool.String() + "/approve?force=true",
			Body: `{"arguments_sha256":"` + domain.HashArguments(fxRemedyArgs) + `"}`},
	})
}
