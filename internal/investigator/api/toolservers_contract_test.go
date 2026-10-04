package api

// THE TOOLSERVER TRANSPORT, CHECKED AGAINST THE CONTRACT (git-bug 2e9a086).
//
//   - every one of the five operations answers the shape the contract declares;
//   - a ToolServer's token is write-only: it reaches the service and no response carries it;
//   - `access` must be declared, and a transport oto does not speak (stdio) is a 422;
//   - a Tool on a write ToolServer is listed with its qualified name and is not usable;
//   - another org's ToolServer is a 404, never a 403.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

var (
	fxToolServer      = uuid.MustParse("77777777-7777-4777-8777-777777777777")
	fxWriteToolServer = uuid.MustParse("88888888-8888-4888-8888-888888888888")
)

const fxToken = "tst-contract-never-echoed"

func fxToolServerConfig(id uuid.UUID) domain.ToolServerConfig {
	c := domain.ToolServerConfig{ID: id, OrgID: apitest.OrgID, Name: "k8s", URL: "https://k8s-mcp.tools.example.test/mcp",
		Transport: domain.TransportStreamableHTTP, Access: domain.AccessRead, CredentialID: uuid.New(),
		Limits:       domain.CallLimits{Timeout: 15 * time.Second, MaxResultBytes: domain.DefaultResultBytes},
		DiscoveredAt: fxEpoch.Add(time.Minute), CreatedAt: fxEpoch, UpdatedAt: fxEpoch}
	if id == fxWriteToolServer {
		c.Name, c.Access = "k8s-write", domain.AccessWrite
		c.DiscoveryFailedAt, c.DiscoveryError = fxEpoch.Add(2*time.Minute), "the ToolServer k8s-write could not be reached"
	}
	return c
}

func fxCatalog(id uuid.UUID) service.ToolServerCatalog {
	ro := true
	pods, _ := domain.NewDiscoveredTool("pods_list", "List pods in a namespace.",
		json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"}}}`), &ro)
	dotted, _ := domain.NewDiscoveredTool("logs.query", "Query logs.", json.RawMessage(`{"type":"object"}`), nil)
	return service.ToolServerCatalog{ToolServer: fxToolServerConfig(id), Tools: []domain.DiscoveredTool{dotted, pods}}
}

func (f *fakeInvestigators) ownsToolServer(s db.TenantScope, id uuid.UUID) error {
	if !mine(s) || (id != fxToolServer && id != fxWriteToolServer) {
		return errs.NotFound("tool_server_not_found", "no such ToolServer")
	}
	return nil
}

func (f *fakeInvestigators) CreateToolServer(_ context.Context, _ db.TenantScope, d domain.ToolServerDraft) (domain.ToolServerConfig, error) {
	f.record("createToolServer")
	f.mu.Lock()
	f.gotKey = d.Token
	f.mu.Unlock()
	return fxToolServerConfig(fxToolServer), nil
}

func (f *fakeInvestigators) ListToolServers(_ context.Context, s db.TenantScope) ([]domain.ToolServerConfig, error) {
	if !mine(s) {
		return []domain.ToolServerConfig{}, nil
	}
	return []domain.ToolServerConfig{fxToolServerConfig(fxToolServer), fxToolServerConfig(fxWriteToolServer)}, nil
}

func (f *fakeInvestigators) GetToolServer(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.ToolServerConfig, error) {
	if err := f.ownsToolServer(s, id); err != nil {
		return domain.ToolServerConfig{}, err
	}
	return fxToolServerConfig(id), nil
}

func (f *fakeInvestigators) DiscoverToolServer(_ context.Context, s db.TenantScope, id uuid.UUID) (service.ToolServerCatalog, error) {
	f.record("discoverToolServer")
	if err := f.ownsToolServer(s, id); err != nil {
		return service.ToolServerCatalog{}, err
	}
	if id == fxWriteToolServer {
		return service.ToolServerCatalog{}, errs.UpstreamDown("tool_server_discovery_failed",
			"the ToolServer k8s-write could not list its Tools: the ToolServer k8s-write could not be reached", nil)
	}
	return fxCatalog(id), nil
}

func (f *fakeInvestigators) ToolServerTools(_ context.Context, s db.TenantScope, id uuid.UUID) (service.ToolServerCatalog, error) {
	if err := f.ownsToolServer(s, id); err != nil {
		return service.ToolServerCatalog{}, err
	}
	return fxCatalog(id), nil
}

const createToolServerBody = `{"name":"k8s","url":"https://k8s-mcp.tools.example.test/mcp","access":"read","token":"` + fxToken + `"}`

func TestEveryToolServerOperationAnswersItsContractShape(t *testing.T) {
	t.Parallel()

	_, c := newClient(t)
	id := fxToolServer.String()

	resp := c.GET("/tool-servers").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listToolServers", http.StatusOK, resp.Body())

	resp = c.Raw(http.MethodPost, "/tool-servers", apitest.ContentTypeJSON, createToolServerBody).
		MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createToolServer", http.StatusCreated, resp.Body())

	resp = c.GET("/tool-servers/"+id).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getToolServer", http.StatusOK, resp.Body())

	resp = c.Raw(http.MethodPost, "/tool-servers/"+id+"/discover", "", "").MustStatus(t, http.StatusOK)
	schema.Assert(t, "discoverToolServer", http.StatusOK, resp.Body())

	resp = c.GET("/tool-servers/"+id+"/tools").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listToolServerTools", http.StatusOK, resp.Body())
	tools := resp.JSON(t)["data"].([]any)
	dotted, pods := tools[0].(map[string]any), tools[1].(map[string]any)
	if pods["qualified_name"] != "k8s__pods_list" || pods["usable"] != true || pods["read_only_hint"] != true {
		t.Fatalf("pods_list = %v", pods)
	}
	if dotted["qualified_name"] != nil || dotted["usable"] != false || dotted["unusable_reason"] == nil {
		t.Fatalf("logs.query = %v, want unusable with a reason", dotted)
	}

	// A ToolServer that cannot be reached is a 502, said as a problem.
	resp = c.Raw(http.MethodPost, "/tool-servers/"+fxWriteToolServer.String()+"/discover", "", "").
		MustStatus(t, http.StatusBadGateway)
	schema.AssertProblem(t, "discoverToolServer", http.StatusBadGateway, resp.Body())
}

// TestAWriteToolServersToolsAreListedAndNotUsable — ADR 0054 §5: never in an
// Investigator's hands, still named for a Remedy.
func TestAWriteToolServersToolsAreListedAndNotUsable(t *testing.T) {
	t.Parallel()

	_, c := newClient(t)
	resp := c.GET("/tool-servers/"+fxWriteToolServer.String()+"/tools").MustStatus(t, http.StatusOK)
	pods := resp.JSON(t)["data"].([]any)[1].(map[string]any)
	if pods["qualified_name"] != "k8s-write__pods_list" || pods["usable"] != false ||
		!strings.Contains(pods["unusable_reason"].(string), "write ToolServer") {
		t.Fatalf("a write ToolServer's Tool = %v", pods)
	}
	ts := c.GET("/tool-servers/"+fxWriteToolServer.String()).MustStatus(t, http.StatusOK).JSON(t)["data"].(map[string]any)
	if ts["access"] != "write" || ts["discovery_error"] == nil {
		t.Fatalf("write ToolServer = %v", ts)
	}
}

// TestAToolServerTokenIsWriteOnly — the token reaches the service and no response carries it.
func TestAToolServerTokenIsWriteOnly(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	resp := c.Raw(http.MethodPost, "/tool-servers", apitest.ContentTypeJSON, createToolServerBody).
		MustStatus(t, http.StatusCreated)
	if strings.Contains(string(resp.Body()), fxToken) {
		t.Fatalf("the create response echoed the token:\n%s", resp)
	}
	if got := resp.JSON(t)["data"].(map[string]any)["has_token"]; got != true {
		t.Fatalf("has_token = %v", got)
	}
	f.mu.Lock()
	got := f.gotKey
	f.mu.Unlock()
	if got != fxToken {
		t.Fatalf("the service was handed %q", got)
	}
}

// TestAToolServerDeclaresItsAccessAndSpeaksHTTP — no default access, no stdio, no token
// over plaintext, no secret in the URL; each a 422 before the service is reached.
func TestAToolServerDeclaresItsAccessAndSpeaksHTTP(t *testing.T) {
	t.Parallel()

	f, c := newClient(t)
	for field, body := range map[string]string{
		"access":    `{"name":"k8s","url":"https://k8s/mcp"}`,
		"transport": `{"name":"k8s","url":"https://k8s/mcp","access":"read","transport":"stdio"}`,
		"url":       `{"name":"k8s","url":"http://k8s/mcp","access":"read","token":"t"}`,
		"name":      `{"name":"k8s_prod","url":"https://k8s/mcp","access":"read"}`,
	} {
		resp := c.Raw(http.MethodPost, "/tool-servers", apitest.ContentTypeJSON, body).
			MustStatus(t, http.StatusUnprocessableEntity)
		schema.AssertProblem(t, "createToolServer", http.StatusUnprocessableEntity, resp.Body())
		resp.MustViolate(t, field)
	}
	resp := c.Raw(http.MethodPost, "/tool-servers", apitest.ContentTypeJSON,
		`{"name":"k8s","url":"https://k8s/mcp?api_key=x","access":"read"}`).MustStatus(t, http.StatusUnprocessableEntity)
	resp.MustViolate(t, "url")
	if f.callCount() != 0 {
		t.Fatalf("an invalid ToolServer reached the service %d time(s)", f.callCount())
	}
}

func toolServerRoutes() []apitest.Route {
	id := fxToolServer.String()
	return []apitest.Route{
		{Op: "listToolServers", Method: http.MethodGet, Path: "/tool-servers"},
		{Op: "createToolServer", Method: http.MethodPost, Path: "/tool-servers", Body: createToolServerBody},
		{Op: "getToolServer", Method: http.MethodGet, Path: "/tool-servers/" + id},
		{Op: "discoverToolServer", Method: http.MethodPost, Path: "/tool-servers/" + id + "/discover"},
		{Op: "listToolServerTools", Method: http.MethodGet, Path: "/tool-servers/" + id + "/tools"},
	}
}

func TestEveryToolServerRouteRefusesAnAnonymousCaller(t *testing.T) {
	t.Parallel()
	apitest.AssertUnauthenticated(t, world, toolServerRoutes())
}

func TestAnotherOrgsToolServerIsA404(t *testing.T) {
	t.Parallel()

	id, stranger := fxToolServer.String(), apitest.StrangerID.String()
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		t.Helper()
		_, c := newClient(t)
		return c.As(apitest.MemberOf(apitest.OtherOrgID)), nil
	}, []apitest.Route{
		{Op: "getToolServer", Method: http.MethodGet, Path: "/tool-servers/" + id},
		{Op: "discoverToolServer", Method: http.MethodPost, Path: "/tool-servers/" + id + "/discover"},
		{Op: "listToolServerTools", Method: http.MethodGet, Path: "/tool-servers/" + id + "/tools"},
	})
	apitest.AssertCrossTenant404(t, world, []apitest.Route{
		{Op: "getToolServer", Name: "stranger", Method: http.MethodGet, Path: "/tool-servers/" + stranger},
		{Op: "listToolServerTools", Name: "stranger tools", Method: http.MethodGet, Path: "/tool-servers/" + stranger + "/tools"},
	})
}

func TestAnUnknownToolServerQueryParameterIsRefused(t *testing.T) {
	t.Parallel()

	id := fxToolServer.String()
	apitest.AssertUnknownQueryParamRefused(t, world, []apitest.Route{
		{Op: "listToolServers", Method: http.MethodGet, Path: "/tool-servers?reveal=token"},
		{Op: "createToolServer", Method: http.MethodPost, Path: "/tool-servers?force=true", Body: createToolServerBody},
		{Op: "getToolServer", Method: http.MethodGet, Path: "/tool-servers/" + id + "?include=token"},
		{Op: "discoverToolServer", Method: http.MethodPost, Path: "/tool-servers/" + id + "/discover?wait=true"},
		{Op: "listToolServerTools", Method: http.MethodGet, Path: "/tool-servers/" + id + "/tools?usable=true"},
	})
}
