package service

// TOOLSERVERS (ADR 0016, 0053 §3 §6, 0054 §5; git-bug 2e9a086): an operator configures
// the MCP servers an Investigator may read the cluster through; oto lists their Tools
// when asked; a run holds the ones its allowlist names, on `read` ToolServers only.
//
// ⭐⭐ THE TOKEN IS UNSEALED IN ONE PLACE AND SCRUBBED FROM EVERYTHING THAT COMES BACK. It
// is resolved when a session opens and handed straight to the adapter; and because a
// ToolServer is a third party that may echo a request header into its answer, every
// result, every failure message and every listed description passes through
// scrubSecret before anything — a Step, the model, a discovery row — sees it.
//
// ⭐ A SESSION OPENS ON THE FIRST CALL, NOT WHEN THE RUN STARTS. The model is offered the
// Tools as they were last discovered (stored schemas), so a run whose model never asks
// a ToolServer anything never dials it; and a ToolServer that cannot be reached is a
// `failed` Step on the call that needed it — recorded, answered, and the run goes on —
// never a run that silently lost its Tools.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// DiscoveryTimeout bounds one discovery: open a session, list every page, close.
const DiscoveryTimeout = 30 * time.Second

// maxDiscoveryError mirrors `tool_servers_failure_ck`'s length bound.
const maxDiscoveryError = 2000

// ToolServerCatalog is one ToolServer and the Tools it listed at its last successful
// discovery, by name.
type ToolServerCatalog struct {
	ToolServer domain.ToolServerConfig
	Tools      []domain.DiscoveredTool
}

// CreateToolServer seals the draft's token and stores the ToolServer, in one
// transaction. ⛔ The token is never returned, logged or echoed.
func (s *Service) CreateToolServer(ctx context.Context, scope db.TenantScope, draft domain.ToolServerDraft) (domain.ToolServerConfig, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.ToolServerConfig{}, err
	}
	at := s.now()
	var out domain.ToolServerConfig
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		credentialID := uuid.Nil
		if draft.Token != "" {
			id, err := s.creds.CreateCredential(ctx, scope, domain.ToolServerCredentialKind,
				map[string]string{domain.ToolServerCredentialValueKey: draft.Token})
			if err != nil {
				return err
			}
			credentialID = id
		}
		stored, err := s.toolServers.Insert(ctx, scope, draft, credentialID, at)
		out = stored
		return err
	})
	if err != nil {
		return domain.ToolServerConfig{}, err
	}
	return out, nil
}

// GetToolServer reads one ToolServer, without its token.
func (s *Service) GetToolServer(ctx context.Context, scope db.TenantScope, id uuid.UUID) (domain.ToolServerConfig, error) {
	return s.toolServers.Get(ctx, scope, id)
}

// ListToolServers reads an org's ToolServers by name, without their tokens.
func (s *Service) ListToolServers(ctx context.Context, scope db.TenantScope) ([]domain.ToolServerConfig, error) {
	return s.toolServers.List(ctx, scope)
}

// ToolServerTools reads one ToolServer and the Tools it listed last.
func (s *Service) ToolServerTools(ctx context.Context, scope db.TenantScope, id uuid.UUID) (ToolServerCatalog, error) {
	cfg, err := s.toolServers.Get(ctx, scope, id)
	if err != nil {
		return ToolServerCatalog{}, err
	}
	tools, err := s.toolServers.Tools(ctx, scope, id)
	if err != nil {
		return ToolServerCatalog{}, err
	}
	return ToolServerCatalog{ToolServer: cfg, Tools: tools}, nil
}

// DiscoverToolServer asks a ToolServer for its Tools and records the answer: the list
// replaces the last one, or — when the ToolServer cannot be reached or answers badly —
// the failure is recorded on the ToolServer, the last good list is kept, and the
// failure is returned.
//
// ⭐ A WRITE TOOLSERVER IS DISCOVERED TOO. Listing reads nothing from the cluster, and a
// Remedy (ADR 0054) will need to name the write Tool it would use.
func (s *Service) DiscoverToolServer(ctx context.Context, scope db.TenantScope, id uuid.UUID) (ToolServerCatalog, error) {
	if err := db.RequireScope(scope); err != nil {
		return ToolServerCatalog{}, err
	}
	cfg, err := s.toolServers.Get(ctx, scope, id)
	if err != nil {
		return ToolServerCatalog{}, err
	}
	tools, secret, err := s.listTools(ctx, scope, cfg)
	if err != nil {
		reason := clipText(scrubSecret(safeMessage(err), secret), maxDiscoveryError)
		if rerr := s.toolServers.RecordDiscoveryFailure(context.WithoutCancel(ctx), scope, id, reason, s.now()); rerr != nil {
			return ToolServerCatalog{}, rerr
		}
		if errs.IsKind(err, errs.KindInternal) {
			return ToolServerCatalog{}, err // oto's own failure (a token that will not unseal), not the ToolServer's
		}
		return ToolServerCatalog{}, errs.UpstreamDown("tool_server_discovery_failed",
			fmt.Sprintf("the ToolServer %s could not list its Tools: %s", cfg.Name, reason), nil)
	}
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		return s.toolServers.ReplaceTools(ctx, scope, id, tools, s.now())
	})
	if err != nil {
		return ToolServerCatalog{}, err
	}
	return s.ToolServerTools(ctx, scope, id)
}

// listTools opens a session, lists every Tool, and closes it. It returns the token too,
// only so the caller can scrub it from a failure message.
func (s *Service) listTools(ctx context.Context, scope db.TenantScope, cfg domain.ToolServerConfig) ([]domain.DiscoveredTool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, DiscoveryTimeout)
	defer cancel()
	token, err := s.toolServerToken(ctx, scope, cfg)
	if err != nil {
		return nil, "", err
	}
	client, err := s.toolDialer.Connect(ctx, cfg, token)
	if err != nil {
		return nil, token, err
	}
	defer func() { _ = client.Close() }()
	listed, err := client.ListTools(ctx)
	if err != nil {
		return nil, token, err
	}
	if len(listed) > domain.MaxDiscoveredTools {
		return nil, token, errs.Newf(errs.KindUpstreamDown, "tool_server_listing_too_long",
			"the ToolServer listed %d Tools; oto records at most %d", len(listed), domain.MaxDiscoveredTools)
	}
	out := make([]domain.DiscoveredTool, 0, len(listed))
	seen := make(map[string]struct{}, len(listed))
	for _, t := range listed {
		if _, dup := seen[t.Name]; dup {
			return nil, token, errs.Newf(errs.KindUpstreamDown, "tool_server_listing_invalid",
				"the ToolServer listed two Tools named %q", t.Name)
		}
		seen[t.Name] = struct{}{}
		t.Description = scrubSecret(t.Description, token)
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b domain.DiscoveredTool) int { return strings.Compare(a.Name, b.Name) })
	return out, token, nil
}

func (s *Service) toolServerToken(ctx context.Context, scope db.TenantScope, cfg domain.ToolServerConfig) (string, error) {
	if !cfg.HasToken() {
		return "", nil
	}
	return s.tokens.ResolveToolServerToken(ctx, scope, cfg.CredentialID)
}

// ------------------------------------------------------------ the allowlist

// checkToolServerAllowlist refuses an allowlist naming a ToolServer Tool an Investigator
// may not hold: on no configured ToolServer, on a `write` one, or one the ToolServer has
// not listed. Names without `__` are oto's built-in Tools' business and pass through.
//
// ⛔ A WRITE TOOLSERVER'S TOOLS ARE NEVER IN AN INVESTIGATOR'S HANDS (ADR 0053 §3, 0054
// §5). This is where that is said to the person writing the allowlist; the run says it
// again (toolServerTools), because a version outlives the moment it was written.
func (s *Service) checkToolServerAllowlist(ctx context.Context, scope db.TenantScope, allow domain.Allowlist) error {
	wanted := allow.ToolServerTools()
	if len(wanted) == 0 {
		return nil
	}
	servers, err := s.toolServers.ByNames(ctx, scope, mapKeys(wanted))
	if err != nil {
		return err
	}
	byName := make(map[string]domain.ToolServerConfig, len(servers))
	for _, c := range servers {
		byName[c.Name] = c
	}
	var v []errs.Violation
	for _, server := range mapKeys(wanted) {
		cfg, ok := byName[server]
		if !ok {
			for _, tool := range wanted[server] {
				v = append(v, errs.Violation{Field: "tools", Code: "unknown_tool_server",
					Message: server + domain.QualifiedToolSeparator + tool + ": no ToolServer named " + server + " is configured"})
			}
			continue
		}
		if !cfg.Readable() {
			for _, tool := range wanted[server] {
				v = append(v, errs.Violation{Field: "tools", Code: "write_tool_server",
					Message: server + domain.QualifiedToolSeparator + tool + ": " + server +
						" is a write ToolServer, and an Investigator holds only read Tools"})
			}
			continue
		}
		listed, err := s.toolServers.Tools(ctx, scope, cfg.ID)
		if err != nil {
			return err
		}
		for _, tool := range wanted[server] {
			if _, why := usableTool(listed, server, tool); why != "" {
				v = append(v, errs.Violation{Field: "tools", Code: "unusable_tool",
					Message: server + domain.QualifiedToolSeparator + tool + ": " + why})
			}
		}
	}
	if len(v) > 0 {
		return errs.Validation("investigator_tools_invalid",
			"an Investigator may hold only Tools a read ToolServer has listed", v...)
	}
	return nil
}

// usableTool finds `tool` among what `server` listed and says why it cannot be held.
func usableTool(listed []domain.DiscoveredTool, server, tool string) (domain.DiscoveredTool, string) {
	for _, t := range listed {
		if t.Name != tool {
			continue
		}
		if _, why := t.Usable(server); why != "" {
			return domain.DiscoveredTool{}, why
		}
		return t, ""
	}
	return domain.DiscoveredTool{}, "the ToolServer " + server + " has not listed a Tool named " + quoteName(tool) +
		"; run its discovery"
}

// ------------------------------------------------------------ during a run

// toolServerTools resolves a version's qualified allowlist names into the Tools the run
// may call, and says why each one it cannot hold is unavailable. close ends every
// session the run opened.
func (s *Service) toolServerTools(
	ctx context.Context, scope db.TenantScope, allow domain.Allowlist,
) (offered []Tool, unavailable map[string]string, closeAll func(), err error) {
	unavailable = map[string]string{}
	wanted := allow.ToolServerTools()
	if len(wanted) == 0 {
		return nil, unavailable, func() {}, nil
	}
	servers, err := s.toolServers.ByNames(ctx, scope, mapKeys(wanted))
	if err != nil {
		return nil, nil, nil, err
	}
	byName := make(map[string]domain.ToolServerConfig, len(servers))
	for _, c := range servers {
		byName[c.Name] = c
	}
	var sessions []*toolSession
	closeAll = func() {
		for _, ss := range sessions {
			ss.close()
		}
	}
	for _, server := range mapKeys(wanted) {
		cfg, ok := byName[server]
		switch {
		case !ok:
			for _, tool := range wanted[server] {
				unavailable[server+domain.QualifiedToolSeparator+tool] = "no ToolServer named " + server + " is configured"
			}
			continue
		case !cfg.Readable():
			for _, tool := range wanted[server] {
				unavailable[server+domain.QualifiedToolSeparator+tool] = server +
					" is a write ToolServer; its Tools are never held while investigating"
			}
			continue
		}
		listed, err := s.toolServers.Tools(ctx, scope, cfg.ID)
		if err != nil {
			closeAll()
			return nil, nil, nil, err
		}
		session := &toolSession{svc: s, scope: scope, cfg: cfg}
		sessions = append(sessions, session)
		for _, tool := range wanted[server] {
			qualified := server + domain.QualifiedToolSeparator + tool
			t, why := usableTool(listed, server, tool)
			if why != "" {
				unavailable[qualified] = why
				continue
			}
			schema, err := domain.NewToolSchema(qualified, t.Description, t.InputSchema)
			if err != nil {
				unavailable[qualified] = "its listed schema cannot be offered to a model: " + safeMessage(err)
				continue
			}
			offered = append(offered, serverTool{session: session, tool: tool, schema: schema})
		}
	}
	return offered, unavailable, closeAll, nil
}

// toolSession is one run's session with one ToolServer, opened on the first call.
type toolSession struct {
	svc   *Service
	scope db.TenantScope
	cfg   domain.ToolServerConfig

	mu     sync.Mutex
	client domain.ToolServerClient
	token  string
}

// call runs one Tool, opening the session first if no call has. A failure to open is
// not cached: the next call tries again, inside its own timeout.
func (ss *toolSession) call(ctx context.Context, tool string, args json.RawMessage) (domain.ToolResult, string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.client == nil {
		token, err := ss.svc.toolServerToken(ctx, ss.scope, ss.cfg)
		if err != nil {
			return domain.ToolResult{}, "", err
		}
		client, err := ss.svc.toolDialer.Connect(ctx, ss.cfg, token)
		if err != nil {
			return domain.ToolResult{}, token, err
		}
		ss.client, ss.token = client, token
	}
	res, err := ss.client.CallTool(ctx, tool, args)
	return res, ss.token, err
}

func (ss *toolSession) close() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.client != nil {
		_ = ss.client.Close()
		ss.client = nil
	}
}

// serverTool is one ToolServer Tool as the loop calls it.
type serverTool struct {
	session *toolSession
	tool    string
	schema  domain.ToolSchema
}

func (t serverTool) Schema() domain.ToolSchema { return t.schema }

// callLimits are the ToolServer's own per-call controls.
func (t serverTool) callLimits() Limits {
	return Limits{ToolTimeout: t.session.cfg.Limits.Timeout, MaxToolResult: t.session.cfg.Limits.MaxResultBytes}
}

// Call implements Tool. The subject is not passed: a ToolServer reads the cluster, and
// what it reads is what the model's arguments ask for, within the ToolServer's RBAC.
func (t serverTool) Call(ctx context.Context, _ db.TenantScope, _ RunSubject, args json.RawMessage) (string, error) {
	res, token, err := t.session.call(ctx, t.tool, args)
	if err != nil {
		// The adapter's own code and message, which errs promises are safe to show —
		// scrubbed anyway, because a ToolServer's error text is a third party's.
		return "", errs.New(errs.KindUpstreamDown, "", scrubSecret(safeMessage(err), token))
	}
	text := scrubSecret(res.Text, token)
	if res.IsError {
		// ⭐ THE TOOLSERVER SAID THE CALL FAILED, and the model reads what it said: a
		// `failed` Step whose result is the ToolServer's own explanation.
		return "", errs.New(errs.KindUpstreamDown, "tool_error", text)
	}
	return text, nil
}

// limitedTool is a Tool with per-call controls of its own.
type limitedTool interface {
	callLimits() Limits
}

// scrubSecret replaces every instance of secret in text. ⛔ It runs on everything a
// ToolServer sends back; an empty secret is no-op.
func scrubSecret(text, secret string) string {
	if secret == "" || !strings.Contains(text, secret) {
		return text
	}
	return strings.ReplaceAll(text, secret, "[redacted]")
}

func mapKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func quoteName(s string) string { return fmt.Sprintf("%q", s) }

// clipText cuts s to at most n characters on a rune boundary.
func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
