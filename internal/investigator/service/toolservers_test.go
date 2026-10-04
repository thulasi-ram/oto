package service

// git-bug 2e9a086's "Done when", end to end: oto's loop, the real MCP adapter, and the
// official SDK's own server in-process over TLS (test/toolserverfake). An Investigator
// calls an allowlisted read-only Tool and the Step records it; a call outside the
// allowlist is refused and recorded; a timeout and a truncation are recorded and the run
// continues; results are redacted with the org's ingest rules before they are recorded
// or read; and the ToolServer's access token — the only credential oto holds — never
// reaches a Step, even when the ToolServer echoes it back. No Docker.

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/toolservers/mcpclient"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/modelfake"
	"github.com/thulasiram/oto/test/toolserverfake"
)

const toolServerToken = "tst-live-toolserver-token-must-never-reach-a-step"

// ------------------------------------------------------------------- fakes

type memToolServers struct {
	mu    sync.Mutex
	rows  map[uuid.UUID]domain.ToolServerConfig
	tools map[uuid.UUID][]domain.DiscoveredTool
}

func newMemToolServers() *memToolServers {
	return &memToolServers{rows: map[uuid.UUID]domain.ToolServerConfig{}, tools: map[uuid.UUID][]domain.DiscoveredTool{}}
}

func (m *memToolServers) Insert(_ context.Context, s db.TenantScope, d domain.ToolServerDraft, cred uuid.UUID, at time.Time) (domain.ToolServerConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.Name == d.Name {
			return domain.ToolServerConfig{}, errs.Conflict("tool_servers_org_name_uniq", "taken")
		}
	}
	c := domain.ToolServerConfig{ID: uuid.New(), OrgID: s.OrgID(), Name: d.Name, URL: d.URL, Transport: d.Transport,
		Access: d.Access, CredentialID: cred, Limits: d.Limits, CreatedAt: at, UpdatedAt: at}
	m.rows[c.ID] = c
	return c, nil
}

func (m *memToolServers) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.ToolServerConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.rows[id]
	if !ok || c.OrgID != s.OrgID() {
		return domain.ToolServerConfig{}, errs.NotFound("tool_server_not_found", "no such ToolServer")
	}
	return c, nil
}

func (m *memToolServers) List(_ context.Context, s db.TenantScope) ([]domain.ToolServerConfig, error) {
	return m.ByNames(context.Background(), s, nil)
}

func (m *memToolServers) ByNames(_ context.Context, s db.TenantScope, names []string) ([]domain.ToolServerConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.ToolServerConfig{}
	for _, c := range m.rows {
		if c.OrgID == s.OrgID() && (names == nil || contains(names, c.Name)) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memToolServers) ReplaceTools(_ context.Context, _ db.TenantScope, id uuid.UUID, tools []domain.DiscoveredTool, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.rows[id]
	c.DiscoveredAt, c.DiscoveryFailedAt, c.DiscoveryError = at, time.Time{}, ""
	m.rows[id] = c
	m.tools[id] = append([]domain.DiscoveredTool(nil), tools...)
	return nil
}

func (m *memToolServers) RecordDiscoveryFailure(_ context.Context, _ db.TenantScope, id uuid.UUID, reason string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.rows[id]
	c.DiscoveryFailedAt, c.DiscoveryError = at, reason
	m.rows[id] = c
	return nil
}

func (m *memToolServers) Tools(_ context.Context, _ db.TenantScope, id uuid.UUID) ([]domain.DiscoveredTool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.DiscoveredTool(nil), m.tools[id]...), nil
}

func (m *memToolServers) setAccess(id uuid.UUID, a domain.ToolServerAccess) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.rows[id]
	c.Access = a
	m.rows[id] = c
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// switchDialer is the rig's ToolServerDialer: the real adapter once a test points it at
// a fake server, and a refusal before then.
type switchDialer struct {
	mu     sync.Mutex
	inner  ToolServerDialer
	tokens []string
}

func (d *switchDialer) Connect(ctx context.Context, cfg domain.ToolServerConfig, token string) (domain.ToolServerClient, error) {
	d.mu.Lock()
	inner := d.inner
	d.tokens = append(d.tokens, token)
	d.mu.Unlock()
	if inner == nil {
		return nil, errs.UpstreamDown("tool_server_unreachable", "the ToolServer "+cfg.Name+" could not be reached", nil)
	}
	return inner.Connect(ctx, cfg, token)
}

type memRedaction struct {
	r   domain.ResultRedactor
	err error
}

func (m *memRedaction) ToolResultRedactor(context.Context, db.TenantScope) (domain.ResultRedactor, error) {
	return m.r, m.err
}

// globRedactor is the ingest dialect — `path.Match` globs over a name — the way
// `internal/app` hands `ingestion/decode`'s matcher in.
func globRedactor(patterns ...string) domain.ResultRedactor {
	return domain.NewResultRedactor(func(name string) bool {
		for _, p := range patterns {
			if ok, _ := path.Match(p, name); ok {
				return true
			}
		}
		return false
	}, "[redacted]")
}

// ------------------------------------------------------------------- rig

// withToolServer starts an in-process MCP server, configures it as the ToolServer `k8s`
// with a sealed token, and discovers it.
func (r *rig) withToolServer(t *testing.T, access domain.ToolServerAccess, limits domain.CallLimits, tools ...toolserverfake.Tool) (*toolserverfake.Server, domain.ToolServerConfig) {
	t.Helper()
	srv := toolserverfake.Start(t, toolserverfake.Options{Token: toolServerToken}, tools...)
	r.toolDialer.inner = mcpclient.Dialer{HTTPClient: srv.Client}
	draft, err := domain.NewToolServerDraft("k8s", srv.URL, "", string(access), toolServerToken, limits)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.svc.CreateToolServer(context.Background(), r.scope, draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.DiscoverToolServer(context.Background(), r.scope, cfg.ID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	return srv, cfg
}

func (r *rig) allow(t *testing.T, inv domain.Investigator, names ...string) domain.Investigator {
	t.Helper()
	tools, err := domain.NewAllowlist(names)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Tools: &tools}); err != nil {
		t.Fatalf("allowlist %v: %v", names, err)
	}
	out, err := r.svc.investigators.Get(context.Background(), r.scope, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func defaultLimits(t *testing.T) domain.CallLimits {
	t.Helper()
	l, err := domain.NewCallLimits(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func echoTool(name, text string) toolserverfake.Tool {
	return toolserverfake.Tool{Name: name, Description: "reads " + name,
		Handle: func(_ context.Context, c toolserverfake.Call) (string, bool) {
			return strings.ReplaceAll(text, "$AUTH", c.Authorization), false
		}}
}

func assertNoToken(t *testing.T, r *rig, got domain.Investigation, steps []domain.Step) {
	t.Helper()
	for _, s := range steps {
		for _, field := range []string{s.Text, s.Result, s.Call.Arguments, s.Call.Name} {
			if strings.Contains(field, toolServerToken) {
				t.Fatalf("the ToolServer's token reached Step %d: %q", s.Seq, field)
			}
		}
	}
	if strings.Contains(got.Ending.Detail+got.Finding, toolServerToken) {
		t.Fatal("the ToolServer's token reached the run")
	}
	for _, req := range r.dial.model().Requests() {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, toolServerToken) {
				t.Fatal("the ToolServer's token reached the model")
			}
		}
	}
}

// ------------------------------------------------------------------- tests

func TestAToolServerIsConfiguredWithASealedTokenAndDiscovered(t *testing.T) {
	r := newRig(t)
	ro := true
	_, cfg := r.withToolServer(t, domain.AccessRead, defaultLimits(t),
		toolserverfake.Tool{Name: "pods_list", Description: "List pods.", ReadOnly: &ro,
			Handle: func(context.Context, toolserverfake.Call) (string, bool) { return "[]", false }},
		toolserverfake.Tool{Name: "logs.query", Description: "Query logs.",
			Handle: func(context.Context, toolserverfake.Call) (string, bool) { return "[]", false }},
	)
	if !cfg.HasToken() || r.creds.kinds[cfg.CredentialID] != domain.ToolServerCredentialKind ||
		r.creds.sealed[cfg.CredentialID][domain.ToolServerCredentialValueKey] != toolServerToken {
		t.Fatalf("token not sealed as a tool_server_token: %+v", r.creds.kinds)
	}
	if r.toolDialer.tokens[0] != toolServerToken {
		t.Fatal("discovery did not present the unsealed token")
	}
	cat, err := r.svc.ToolServerTools(context.Background(), r.scope, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cat.ToolServer.DiscoveredAt.IsZero() || len(cat.Tools) != 2 || cat.Tools[0].Name != "logs.query" {
		t.Fatalf("catalog = %+v", cat)
	}
	if q, why := cat.Tools[1].Usable("k8s"); q != "k8s__pods_list" || why != "" || cat.Tools[1].ReadOnlyHint == nil {
		t.Fatalf("pods_list usable as %q (%s)", q, why)
	}
	// ⭐ A name a model cannot be offered is recorded, and says why, rather than dropped.
	if q, why := cat.Tools[0].Usable("k8s"); q != "" || why == "" {
		t.Fatalf("logs.query usable as %q", q)
	}
}

func TestAFailedDiscoveryIsRecordedAndKeepsTheLastList(t *testing.T) {
	r := newRig(t)
	_, cfg := r.withToolServer(t, domain.AccessRead, defaultLimits(t), echoTool("pods_list", "[]"))
	r.toolDialer.inner = nil // the ToolServer is gone
	_, err := r.svc.DiscoverToolServer(context.Background(), r.scope, cfg.ID)
	if !errs.IsKind(err, errs.KindUpstreamDown) {
		t.Fatalf("err = %v", err)
	}
	cat, _ := r.svc.ToolServerTools(context.Background(), r.scope, cfg.ID)
	if cat.ToolServer.DiscoveryError == "" || cat.ToolServer.DiscoveryFailedAt.IsZero() || len(cat.Tools) != 1 {
		t.Fatalf("after a failed discovery: %+v", cat)
	}
}

func TestAnAllowlistMayHoldOnlyToolsAReadToolServerListed(t *testing.T) {
	r := newRig(t)
	inv, _ := r.setup(t, domain.DefaultBudgets())
	_, cfg := r.withToolServer(t, domain.AccessRead, defaultLimits(t), echoTool("pods_list", "[]"))

	for name, tools := range map[string][]string{
		"unknown ToolServer": {"vm__query"},
		"never listed":       {"k8s__pods_delete"},
	} {
		t.Run(name, func(t *testing.T) {
			list, _ := domain.NewAllowlist(tools)
			_, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Tools: &list})
			if errs.CodeOf(err) != "investigator_tools_invalid" || len(errs.ViolationsOf(err)) != 1 {
				t.Fatalf("err = %v", err)
			}
		})
	}

	// ⛔ A write ToolServer's Tools are never in an Investigator's hands.
	r.toolServers.setAccess(cfg.ID, domain.AccessWrite)
	list, _ := domain.NewAllowlist([]string{"k8s__pods_list"})
	_, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Tools: &list})
	if v := errs.ViolationsOf(err); len(v) != 1 || v[0].Code != "write_tool_server" {
		t.Fatalf("err = %v", err)
	}
	r.toolServers.setAccess(cfg.ID, domain.AccessRead)
	if got := r.allow(t, inv, ToolCaseTimeline, "k8s__pods_list"); !got.Current.Tools.Allows("k8s__pods_list") {
		t.Fatal("a read ToolServer's listed Tool was not held")
	}
}

// TestARunCallsAnAllowlistedToolAndRefusesOneOutsideIt — the Step records the call,
// the result is redacted before it is recorded or read, a call outside the allowlist
// is refused and recorded, the run continues to its Finding, and the token — echoed back
// by the ToolServer — is nowhere.
func TestARunCallsAnAllowlistedToolAndRefusesOneOutsideIt(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.redaction.r = globRedactor("*PASSWORD*", "*token*")
	srv, _ := r.withToolServer(t, domain.AccessRead, defaultLimits(t),
		echoTool("pods_get", `{"pod":"api-1","env":{"DB_PASSWORD":"hunter2","LOG_LEVEL":"info"},`+
			`"log":"login ok token=abc123 user=ada","echo":"$AUTH"}`),
		echoTool("pods_delete", "deleted"),
	)
	inv = r.allow(t, inv, "k8s__pods_get")

	r.dial.script = []modelfake.Step{
		modelfake.Calls(100, 10, call("c1", "k8s__pods_get", `{"name":"api-1"}`), call("c2", "k8s__pods_delete", `{}`)),
		modelfake.Text("api-1 is crash looping on a bad password.", 100, 10),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)

	if kinds(steps) != "model_turn tool_call:k8s__pods_get:ok tool_call:k8s__pods_delete:refused model_turn" ||
		got.Status != domain.StatusCompleted {
		t.Fatalf("transcript %s, status %s", kinds(steps), got.Status)
	}
	res := steps[1].Result
	for _, leak := range []string{"hunter2", "abc123"} {
		if strings.Contains(res, leak) {
			t.Fatalf("an unredacted value reached the Step: %s", res)
		}
	}
	if !strings.Contains(res, `"LOG_LEVEL":"info"`) || !strings.Contains(res, "user=ada") ||
		!strings.Contains(res, "[redacted: 2 value(s)") {
		t.Fatalf("redaction took too much or said nothing: %s", res)
	}
	if !strings.Contains(steps[2].Result, "allowlist") || srv.Calls() != 1 {
		t.Fatalf("refusal %q; the ToolServer answered %d calls", steps[2].Result, srv.Calls())
	}
	// The model read exactly what the Step kept, and was offered only the held Tool.
	reqs := r.dial.model().Requests()
	if reqs[1].Messages[3].Content != res {
		t.Fatal("the model read something other than what the Step recorded")
	}
	offered := []string{}
	for _, tl := range reqs[0].Tools {
		offered = append(offered, tl.Name)
	}
	if strings.Join(offered, ",") != "k8s__pods_get" {
		t.Fatalf("offered %v", offered)
	}
	assertNoToken(t, r, got, steps)
}

func TestAToolServerTimeoutAndTruncationAreRecordedAndTheRunContinues(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	limits, err := domain.NewCallLimits(1, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.withToolServer(t, domain.AccessRead, limits,
		toolserverfake.Tool{Name: "slow", Handle: func(ctx context.Context, _ toolserverfake.Call) (string, bool) {
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return "too late", false
		}},
		echoTool("huge", strings.Repeat("ログ", 2000)),
	)
	inv = r.allow(t, inv, "k8s__slow", "k8s__huge")
	r.dial.script = []modelfake.Step{
		modelfake.Calls(100, 10, call("c1", "k8s__slow", `{}`), call("c2", "k8s__huge", `{}`)),
		modelfake.Text("partial picture", 100, 10),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if kinds(steps) != "model_turn tool_call:k8s__slow:timeout tool_call:k8s__huge:truncated model_turn" ||
		got.Status != domain.StatusCompleted {
		t.Fatalf("transcript %s, status %s", kinds(steps), got.Status)
	}
	if !strings.Contains(steps[1].Result, "within 1s") {
		t.Fatalf("timeout %q does not name the ToolServer's own limit", steps[1].Result)
	}
	if !strings.Contains(steps[2].Result, "[truncated: ") || len(steps[2].Result) > 1024+64 {
		t.Fatalf("truncated result is %d bytes", len(steps[2].Result))
	}
}

// TestAToolServerTurnedWriteIsRefusedAtTheRun — a version outlives the moment it was
// written, so the run checks the declaration again: refused with the reason, recorded,
// and never offered.
func TestAToolServerTurnedWriteIsRefusedAtTheRun(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	srv, cfg := r.withToolServer(t, domain.AccessRead, defaultLimits(t), echoTool("pods_get", "{}"))
	inv = r.allow(t, inv, "k8s__pods_get")
	r.toolServers.setAccess(cfg.ID, domain.AccessWrite)

	r.dial.script = []modelfake.Step{
		modelfake.Calls(100, 10, call("c1", "k8s__pods_get", `{}`)),
		modelfake.Text("no cluster access", 100, 10),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if kinds(steps) != "model_turn tool_call:k8s__pods_get:refused model_turn" || got.Status != domain.StatusCompleted {
		t.Fatalf("transcript %s", kinds(steps))
	}
	if !strings.Contains(steps[1].Result, "write ToolServer") || srv.Calls() != 0 {
		t.Fatalf("refusal %q, %d calls reached the ToolServer", steps[1].Result, srv.Calls())
	}
	if len(r.dial.model().Requests()[0].Tools) != 0 {
		t.Fatal("a write ToolServer's Tool was offered to the model")
	}
}

// TestAnUnreachableToolServerIsAFailedStepNotALostRun.
func TestAnUnreachableToolServerIsAFailedStepNotALostRun(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	_, _ = r.withToolServer(t, domain.AccessRead, defaultLimits(t), echoTool("pods_get", "{}"))
	inv = r.allow(t, inv, "k8s__pods_get")
	r.toolDialer.inner = nil

	r.dial.script = []modelfake.Step{
		modelfake.Calls(100, 10, call("c1", "k8s__pods_get", `{}`)),
		modelfake.Text("could not look", 100, 10),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)
	if kinds(steps) != "model_turn tool_call:k8s__pods_get:failed model_turn" || got.Status != domain.StatusCompleted {
		t.Fatalf("transcript %s", kinds(steps))
	}
	if !strings.Contains(steps[1].Result, "could not be reached") {
		t.Fatalf("failure %q", steps[1].Result)
	}
}

// TestARunWithoutItsRedactionRulesDoesNotStart — ⛔ never unredacted.
func TestARunWithoutItsRedactionRulesDoesNotStart(t *testing.T) {
	r := newRig(t)
	inv, c := r.setup(t, domain.DefaultBudgets())
	r.redaction.err = errs.Internal("sources_down", errors.New("db"))
	run := r.request(t, inv, c)
	if err := r.svc.RunInvestigation(context.Background(), r.scope, run.ID); err == nil {
		t.Fatal("a run started without its redaction rules")
	}
	if got, _ := r.investigations.Get(context.Background(), r.scope, run.ID); got.Status != domain.StatusQueued {
		t.Fatalf("status %s, want still queued for the retry", got.Status)
	}
}

func TestTheDraftAndConfigNeverRenderTheToken(t *testing.T) {
	limits := domain.CallLimits{Timeout: time.Second, MaxResultBytes: 1024}
	d, err := domain.NewToolServerDraft("k8s", "https://k8s-mcp.tools.svc/mcp", "", "read", toolServerToken, limits)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d.String() + d.GoString())
	if strings.Contains(string(b), toolServerToken) {
		t.Fatal("the draft rendered its token")
	}
}
