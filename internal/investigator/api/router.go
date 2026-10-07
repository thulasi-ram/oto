package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// InvestigatorService is the port this layer declares for itself, satisfied by
// *service.Service. It lists exactly the methods the handlers call.
//
// ⛔ THERE IS NO METHOD HERE THAT SENDS, SUPPRESSES OR HOLDS A NOTIFICATION, and no
// route below could reach one if there were (ADR 0053 §2).
type InvestigatorService interface {
	CreateProvider(ctx context.Context, s db.TenantScope, d domain.ProviderDraft) (domain.ProviderConfig, error)
	ListProviders(ctx context.Context, s db.TenantScope) ([]domain.ProviderConfig, error)
	// A rule change from Settings takes a second person (ADR 0054 §3, owner ruling O3): proposed,
	// then confirmed by a DIFFERENT member, or discarded. ⛔ Nothing here writes a rule directly.
	ProposeRemedyRiskChange(ctx context.Context, s db.TenantScope, by domain.Requester, d domain.RiskChangeDraft) (domain.RiskChange, error)
	PendingRemedyRiskChange(ctx context.Context, s db.TenantScope) (domain.RiskChange, bool, error)
	ConfirmRemedyRiskChange(ctx context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester) (domain.RemedyRiskSettings, error)
	DiscardRemedyRiskChange(ctx context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester) error

	RotateProviderKey(ctx context.Context, s db.TenantScope, id uuid.UUID, apiKey string) (domain.ProviderConfig, error)
	DeleteProvider(ctx context.Context, s db.TenantScope, id uuid.UUID) error

	CreateToolServer(ctx context.Context, s db.TenantScope, d domain.ToolServerDraft) (domain.ToolServerConfig, error)
	ListToolServers(ctx context.Context, s db.TenantScope) ([]domain.ToolServerConfig, error)
	GetToolServer(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.ToolServerConfig, error)
	DiscoverToolServer(ctx context.Context, s db.TenantScope, id uuid.UUID) (service.ToolServerCatalog, error)
	ToolServerTools(ctx context.Context, s db.TenantScope, id uuid.UUID) (service.ToolServerCatalog, error)
	// ⛔ READ-ONLY: who holds the Remedy approval grant on a ToolServer. There is no method
	// here that grants or revokes one, and there must never be (ADR 0054 §4).
	RemedyApprovers(ctx context.Context, s db.TenantScope, id uuid.UUID) ([]domain.RemedyApprover, error)

	CreateInvestigator(ctx context.Context, s db.TenantScope, d domain.InvestigatorDraft) (domain.Investigator, error)
	UpdateInvestigator(ctx context.Context, s db.TenantScope, id uuid.UUID, c domain.InvestigatorChange) (domain.Investigator, error)
	ListInvestigators(ctx context.Context, s db.TenantScope) ([]domain.Investigator, error)
	GetInvestigator(ctx context.Context, s db.TenantScope, id uuid.UUID) (service.InvestigatorDetail, error)

	RequestCaseInvestigation(ctx context.Context, s db.TenantScope, caseID, investigatorID uuid.UUID,
		by domain.Requester) (domain.Investigation, error)
	ListCaseInvestigations(ctx context.Context, s db.TenantScope, caseID uuid.UUID, p db.Keyset) ([]domain.Investigation, db.Cursor, error)
	RequestIncidentInvestigation(ctx context.Context, s db.TenantScope, number int64, investigatorID uuid.UUID,
		by domain.Requester) (domain.Investigation, error)
	ListIncidentInvestigations(ctx context.Context, s db.TenantScope, number int64, p db.Keyset) ([]domain.Investigation, db.Cursor, error)
	ListPolicyDigestInvestigations(ctx context.Context, s db.TenantScope, policyID uuid.UUID, p db.Keyset) ([]domain.Investigation, db.Cursor, error)
	GetInvestigation(ctx context.Context, s db.TenantScope, id uuid.UUID) (service.InvestigationDetail, error)

	ListSuggestions(ctx context.Context, s db.TenantScope, investigationID uuid.UUID) ([]domain.Suggestion, error)
	ApplySuggestion(ctx context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester, movesFrom int64) (domain.Suggestion, error)

	// Remedies (ADR 0054, git-bug 4148256): read, approved by a holder of the grant on the
	// Remedy's ToolServer, declined by any human. ⛔ No method here executes one.
	ListRemedies(ctx context.Context, s db.TenantScope, investigationID uuid.UUID) ([]domain.Remedy, error)
	GetRemedy(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Remedy, error)
	ApproveRemedy(ctx context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester, argumentsSHA256 string) (domain.Remedy, error)
	DeclineRemedy(ctx context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester) (domain.Remedy, error)

	ClassSet(ctx context.Context, s db.TenantScope) (domain.ClassSet, error)
	ReplaceClassSet(ctx context.Context, s db.TenantScope, set domain.ClassSet) (domain.ClassSet, error)

	// The Remedy risk rules and risk model (ADR 0054 §3, git-bug eb4f21b), READ only: they are
	// written by `oto remedy-rules apply` from the host shell (owner ruling 2026-10-05).
	RemedyRisk(ctx context.Context, s db.TenantScope) (domain.RemedyRiskSettings, error)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ InvestigatorService = (*service.Service)(nil)

// Router serves the Investigators tag.
type Router struct {
	svc InvestigatorService
	clk clock.Clock
}

// NewRouter builds the investigator HTTP surface.
func NewRouter(svc InvestigatorService, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, clk: clk}
}

// Mount registers every route this package owns onto r, which `internal/app` has
// already rooted at /api/v1.
//
// ⭐ A CASE'S INVESTIGATIONS ARE ADDRESSED BY THE CASE, flat on the parent like
// `rules/api`'s `/cases/{id}/rule`, because `alerts/api` owns the `/cases/{id}`
// subrouter and a second one would take the prefix from it. An INCIDENT'S are addressed
// by the number a human quotes, flat beside `incidents/api`'s `/incidents` subrouter
// for the same reason — chi matches the longer parameterised pattern first, whichever
// is mounted first. A run itself is addressed by its own id: a Finding cites it, and
// the citation should not need its subject.
func (rt *Router) Mount(r chi.Router) {
	r.Route("/model-providers", func(r chi.Router) {
		r.Get("/", rt.listModelProviders)
		r.Post("/", rt.createModelProvider)
		// ⭐ THE KEY AND THE ROW'S EXISTENCE ARE THE ONLY THINGS THAT CHANGE AFTER CREATE.
		// There is no PATCH: `base_url` and `model` are the identity an Investigator version
		// pins and a Finding names, so a different endpoint is a new provider (00096).
		r.Put("/{id}/key", rt.rotateModelProviderKey)
		r.Delete("/{id}", rt.deleteModelProvider)
	})
	r.Route("/tool-servers", func(r chi.Router) {
		r.Get("/", rt.listToolServers)
		r.Post("/", rt.createToolServer)
		r.Get("/{id}", rt.getToolServer)
		r.Post("/{id}/discover", rt.discoverToolServer)
		r.Get("/{id}/tools", rt.listToolServerTools)
		// ⛔ GET ONLY (ADR 0054 §4, git-bug 47f67c8). A Remedy approver is granted and
		// revoked from the host shell — `oto grant` / `oto revoke` — and by nothing
		// mounted here: a route that let one holder mint a second approver would defeat
		// double approval. test/scope walks the mounted routes to hold that.
		r.Get("/{id}/remedy-approvers", rt.listToolServerRemedyApprovers)
	})
	r.Route("/investigators", func(r chi.Router) {
		r.Get("/", rt.listInvestigators)
		r.Post("/", rt.createInvestigator)
		r.Get("/{id}", rt.getInvestigator)
		r.Patch("/{id}", rt.updateInvestigator)
	})
	r.Get("/cases/{id}/investigations", rt.listCaseInvestigations)
	r.Post("/cases/{id}/investigations", rt.requestCaseInvestigation)
	r.Get("/incidents/{number}/investigations", rt.listIncidentInvestigations)
	r.Post("/incidents/{number}/investigations", rt.requestIncidentInvestigation)
	r.Get("/notification-policies/{id}/investigations", rt.listPolicyDigestInvestigations)
	r.Get("/investigations/{id}", rt.getInvestigation)
	// ⛔ ONE VERB ON A SUGGESTION, AND IT IS APPLY (ADR 0053 §2, git-bug 8327c00). An
	// unapplied one lapses on its own; no route declines one, because a Suggestion that
	// waited on a human's answer would be a queue.
	r.Get("/investigations/{id}/suggestions", rt.listInvestigationSuggestions)
	r.Post("/suggestions/{id}/apply", rt.applySuggestion)
	// ⭐ A REMEDY IS APPROVED OR DECLINED, AND EXECUTED BY NO ROUTE (ADR 0054, git-bug
	// 4148256). Approval is a human holding the grant on its ToolServer; two DIFFERENT ones
	// make it `approved`, and execution is a separate job after that — never a request.
	r.Get("/investigations/{id}/remedies", rt.listInvestigationRemedies)
	r.Get("/remedies/{id}", rt.getRemedy)
	r.Post("/remedies/{id}/approve", rt.approveRemedy)
	r.Post("/remedies/{id}/decline", rt.declineRemedy)
	r.Get("/investigation-classes", rt.getInvestigationClasses)
	r.Put("/investigation-classes", rt.replaceInvestigationClasses)
	// ⭐ HOW MANY APPROVALS A REMEDY NEEDS (ADR 0054 §3, git-bug eb4f21b): the operator's rules
	// and risk model, READ here, and CHANGED only through a pending change.
	//
	// ⛔⛔ NO ROUTE WRITES A RULE (owner ruling 2026-10-05, refined by O3 on 2026-10-06). A rule saying
	// one lets one grant holder approve alone, so a member who could write one could approve alone.
	// What is mounted is a PROPOSAL (which changes no tier), and a CONFIRMATION that only a DIFFERENT
	// member's browser session may make; the writer itself is in `internal/app`, and
	// `oto remedy-rules apply` from the host shell remains. test/scope/remedy_risk_rules_routes_test.go
	// walks the mounted router and holds exactly these routes, no more.
	r.Route("/remedy-risk-rules", func(r chi.Router) {
		r.Get("/", rt.getRemedyRiskRules)
		r.Post("/changes", rt.proposeRemedyRiskChange)
		r.Post("/changes/{id}/confirm", rt.confirmRemedyRiskChange)
		r.Post("/changes/{id}/discard", rt.discardRemedyRiskChange)
	})
}

func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

// maxListed is the page a settings list reports: the whole set, never paged.
const maxListed = 200

// ---------------------------------------------------------------- endpoints

// listModelProviders serves GET /api/v1/model-providers.
func (rt *Router) listModelProviders(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	ps, err := rt.svc.ListProviders(r.Context(), scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]ModelProviderDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, modelProviderDTO(p))
	}
	httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, maxListed), started)
}

// createModelProvider serves POST /api/v1/model-providers.
//
// ⛔ THE KEY IS SEALED AND NEVER ECHOED. The response is the stored row's public half,
// with `has_key`; nothing on any error path renders the request body.
func (rt *Router) createModelProvider(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[CreateModelProviderRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	draft := domain.ProviderDraft{Name: dto.Name, BaseURL: dto.BaseURL, Model: dto.Model}
	if dto.APIKey != nil {
		draft.APIKey = *dto.APIKey
	}
	p, err := rt.svc.CreateProvider(r.Context(), scope, draft)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, modelProviderDTO(p), started)
}

// rotateModelProviderKey serves PUT /api/v1/model-providers/{id}/key.
//
// ⛔ THE KEY IS SEALED AND NEVER ECHOED, as at create: the response is the endpoint's public
// half with `has_key`, and nothing on any error path renders the request body.
func (rt *Router) rotateModelProviderKey(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[RotateModelProviderKeyRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	p, err := rt.svc.RotateProviderKey(r.Context(), scope, id, dto.APIKey)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, modelProviderDTO(p), started)
}

// deleteModelProvider serves DELETE /api/v1/model-providers/{id}: `204`, or a `409` naming
// what still holds the endpoint (an Investigator version, or the Remedy risk model).
func (rt *Router) deleteModelProvider(w http.ResponseWriter, r *http.Request) {
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.svc.DeleteProvider(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

// listToolServers serves GET /api/v1/tool-servers.
func (rt *Router) listToolServers(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	ts, err := rt.svc.ListToolServers(r.Context(), scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]ToolServerDTO, 0, len(ts))
	for _, c := range ts {
		out = append(out, toolServerDTO(c))
	}
	httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, maxListed), started)
}

// createToolServer serves POST /api/v1/tool-servers.
//
// ⛔ THE TOKEN IS SEALED AND NEVER ECHOED. The response is the stored row's public half,
// with `has_token`; nothing on any error path renders the request body. Creating one
// lists nothing: discovery is its own, explicit request.
func (rt *Router) createToolServer(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[CreateToolServerRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	draft, err := dto.toDomain()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	c, err := rt.svc.CreateToolServer(r.Context(), scope, draft)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, toolServerDTO(c), started)
}

// getToolServer serves GET /api/v1/tool-servers/{id}.
func (rt *Router) getToolServer(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	c, err := rt.svc.GetToolServer(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, toolServerDTO(c), started)
}

// discoverToolServer serves POST /api/v1/tool-servers/{id}/discover: ask the ToolServer
// for its Tools now, and answer with what it listed. A ToolServer that cannot be reached
// is a 502, and the failure is recorded on it.
func (rt *Router) discoverToolServer(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	cat, err := rt.svc.DiscoverToolServer(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.List(w, r, toolServerToolDTOs(cat), httpx.PageOf(db.Cursor{}, maxListedTools), started)
}

// listToolServerTools serves GET /api/v1/tool-servers/{id}/tools: what it listed at its
// last successful discovery, by name.
func (rt *Router) listToolServerTools(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	cat, err := rt.svc.ToolServerTools(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.List(w, r, toolServerToolDTOs(cat), httpx.PageOf(db.Cursor{}, maxListedTools), started)
}

// listToolServerRemedyApprovers serves GET /api/v1/tool-servers/{id}/remedy-approvers:
// who holds the Remedy approval grant on this ToolServer, by address, disabled holders
// included and marked as not counting. Read-only; `oto grant` is the only writer.
func (rt *Router) listToolServerRemedyApprovers(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	got, err := rt.svc.RemedyApprovers(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]RemedyApproverDTO, 0, len(got))
	for _, a := range got {
		out = append(out, remedyApproverDTO(a))
	}
	httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, maxListed), started)
}

// maxListedTools is the page a Tool list reports: the whole list, never paged.
const maxListedTools = domain.MaxDiscoveredTools

func toolServerToolDTOs(cat service.ToolServerCatalog) []ToolServerToolDTO {
	out := make([]ToolServerToolDTO, 0, len(cat.Tools))
	for _, t := range cat.Tools {
		out = append(out, toolServerToolDTO(cat.ToolServer, t))
	}
	return out
}

// listInvestigators serves GET /api/v1/investigators.
func (rt *Router) listInvestigators(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	is, err := rt.svc.ListInvestigators(r.Context(), scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]InvestigatorDTO, 0, len(is))
	for _, i := range is {
		out = append(out, investigatorDTO(i))
	}
	httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, maxListed), started)
}

// createInvestigator serves POST /api/v1/investigators: version 1.
func (rt *Router) createInvestigator(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[CreateInvestigatorRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	draft, err := dto.toDomain()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	inv, err := rt.svc.CreateInvestigator(r.Context(), scope, draft)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, investigatorDetailDTO(service.InvestigatorDetail{
		Investigator: inv, Versions: []domain.Version{inv.Current}}), started)
}

func (dto CreateInvestigatorRequest) toDomain() (domain.InvestigatorDraft, error) {
	name, err := domain.NewInvestigatorName(dto.Name)
	if err != nil {
		return domain.InvestigatorDraft{}, err
	}
	budgets := domain.DefaultBudgets()
	if dto.Budgets != nil {
		if budgets, err = dto.Budgets.toDomain(); err != nil {
			return domain.InvestigatorDraft{}, err
		}
	}
	tools, err := domain.NewAllowlist(dto.Tools)
	if err != nil {
		return domain.InvestigatorDraft{}, err
	}
	spec, err := domain.NewVersionSpec(dto.ModelProviderID, dto.Prompt, tools)
	if err != nil {
		return domain.InvestigatorDraft{}, err
	}
	enabled := true
	if dto.Enabled != nil {
		enabled = *dto.Enabled
	}
	incidents := false
	if dto.InvestigatesIncidents != nil {
		incidents = *dto.InvestigatesIncidents
	}
	interval := domain.DefaultMinInterval()
	if dto.MinIntervalSeconds != nil {
		if interval, err = domain.NewMinInterval(*dto.MinIntervalSeconds); err != nil {
			return domain.InvestigatorDraft{}, err
		}
	}
	return domain.InvestigatorDraft{Name: name, Enabled: enabled, Budgets: budgets, MinInterval: interval,
		InvestigatesIncidents: incidents, Spec: spec}, nil
}

// getInvestigator serves GET /api/v1/investigators/{id}.
func (rt *Router) getInvestigator(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	d, err := rt.svc.GetInvestigator(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, investigatorDetailDTO(d), started)
}

// updateInvestigator serves PATCH /api/v1/investigators/{id}.
func (rt *Router) updateInvestigator(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[UpdateInvestigatorRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	change, err := dto.toDomain()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if _, err := rt.svc.UpdateInvestigator(r.Context(), scope, id, change); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	d, err := rt.svc.GetInvestigator(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, investigatorDetailDTO(d), started)
}

func (dto UpdateInvestigatorRequest) toDomain() (domain.InvestigatorChange, error) {
	change := domain.InvestigatorChange{Enabled: dto.Enabled, InvestigatesIncidents: dto.InvestigatesIncidents,
		ProviderID: dto.ModelProviderID, Prompt: dto.Prompt}
	if dto.Budgets != nil {
		b, err := dto.Budgets.toDomain()
		if err != nil {
			return domain.InvestigatorChange{}, err
		}
		change.Budgets = &b
	}
	if dto.MinIntervalSeconds != nil {
		d, err := domain.NewMinInterval(*dto.MinIntervalSeconds)
		if err != nil {
			return domain.InvestigatorChange{}, err
		}
		change.MinInterval = &d
	}
	if dto.Tools != nil {
		tools, err := domain.NewAllowlist(*dto.Tools)
		if err != nil {
			return domain.InvestigatorChange{}, err
		}
		change.Tools = &tools
	}
	return change, nil
}

// listCaseInvestigations serves GET /api/v1/cases/{id}/investigations, latest first.
func (rt *Router) listCaseInvestigations(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	caseID, err := pathCase(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	p := httpx.NewParams(r, "limit", "cursor")
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	hash := httpx.FilterHash("case_id=" + caseID.String())
	cursor, err := httpx.DecodeCursor(p.Cursor(), hash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	runs, next, err := rt.svc.ListCaseInvestigations(r.Context(), scope, caseID, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]InvestigationDTO, 0, len(runs))
	for _, inv := range runs {
		out = append(out, investigationDTO(inv))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// requestCaseInvestigation serves POST /api/v1/cases/{id}/investigations.
//
// ⭐ 202, BECAUSE NOTHING HAS RUN YET. The run is recorded and its job enqueued in one
// transaction, and the answer is that record — `queued`, or `skipped` with reason
// `disabled` when a kill switch is off, which is still a 202: the request was taken and
// its outcome is in the body, never silently dropped.
//
// ⛔ A HUMAN ASKS (ADR 0053 §4). A system principal is refused: a run spends money,
// and spending attributed to nobody cannot be asked about.
func (rt *Router) requestCaseInvestigation(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if kind, err := kernel.NewActorKind(p.ActorKind()); err != nil || !kind.IsHuman() {
		httpx.WriteProblem(w, r, errs.Forbidden("forbidden", "asking for an Investigation requires a human actor"))
		return
	}
	caseID, err := pathCase(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[RequestInvestigationRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	by, err := domain.NewRequester(p.UserID, p.ActorLabel())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	inv, err := rt.svc.RequestCaseInvestigation(r.Context(), scope, caseID, dto.InvestigatorID, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusAccepted, investigationDetailDTO(service.InvestigationDetail{
		Investigation: inv, Steps: []domain.Step{}}), started)
}

// listIncidentInvestigations serves GET /api/v1/incidents/{number}/investigations,
// latest first.
func (rt *Router) listIncidentInvestigations(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	number, err := pathIncident(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	p := httpx.NewParams(r, "limit", "cursor")
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	hash := httpx.FilterHash("incident=" + strconv.FormatInt(number, 10))
	cursor, err := httpx.DecodeCursor(p.Cursor(), hash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	runs, next, err := rt.svc.ListIncidentInvestigations(r.Context(), scope, number, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]InvestigationDTO, 0, len(runs))
	for _, inv := range runs {
		out = append(out, investigationDTO(inv))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// requestIncidentInvestigation serves POST /api/v1/incidents/{number}/investigations:
// a human asking one Investigator to look at the Incident as a whole. 202, for
// requestCaseInvestigation's reason, and refused to a system principal for the same.
func (rt *Router) requestIncidentInvestigation(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if kind, err := kernel.NewActorKind(p.ActorKind()); err != nil || !kind.IsHuman() {
		httpx.WriteProblem(w, r, errs.Forbidden("forbidden", "asking for an Investigation requires a human actor"))
		return
	}
	number, err := pathIncident(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[RequestInvestigationRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	by, err := domain.NewRequester(p.UserID, p.ActorLabel())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	inv, err := rt.svc.RequestIncidentInvestigation(r.Context(), scope, number, dto.InvestigatorID, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusAccepted, investigationDetailDTO(service.InvestigationDetail{
		Investigation: inv, Steps: []domain.Step{}}), started)
}

// getInvestigation serves GET /api/v1/investigations/{id}: the run, its Steps and
// its Finding.
func (rt *Router) getInvestigation(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	d, err := rt.svc.GetInvestigation(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, investigationDetailDTO(d), started)
}

// listInvestigationSuggestions serves GET /api/v1/investigations/{id}/suggestions: the
// changes the run's Finding proposed that are still shown — open, or applied — in the order
// they were proposed. A lapsed one is not listed. An open membership Suggestion that would
// MOVE its Case says from which Incident, so the screen can say so before anyone applies it.
func (rt *Router) listInvestigationSuggestions(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	list, err := rt.svc.ListSuggestions(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]SuggestionDTO, 0, len(list))
	for _, s := range list {
		out = append(out, suggestionDTO(s))
	}
	httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, maxListed), started)
}

// applySuggestion serves POST /api/v1/suggestions/{id}/apply: a human applying one
// Suggestion, which makes the ORDINARY edit — the policy PATCH's own service path, or the
// Incident's add or move — with the applier as actor and the Investigation as provenance.
//
// ⛔ A HUMAN APPLIES. A system principal is refused before the service is reached: the
// Investigator never applies its own Suggestion, and neither does any other machine.
//
// Refusals: `suggestion_already_applied`, `suggestion_lapsed`, `suggestion_target_gone` and
// `suggestion_moves_case` are 409s; the edit's own refusals come back as the edit made by
// hand would answer them.
func (rt *Router) applySuggestion(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if kind, err := kernel.NewActorKind(p.ActorKind()); err != nil || !kind.IsHuman() {
		httpx.WriteProblem(w, r, errs.Forbidden("forbidden", "applying a Suggestion requires a human actor"))
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, domain.SuggestionNotFound())
		return
	}
	dto, err := httpx.Bind[ApplySuggestionRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	by, err := domain.NewRequester(p.UserID, p.ActorLabel())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	var movesFrom int64
	if dto.MovesFromIncidentNumber != nil {
		movesFrom = *dto.MovesFromIncidentNumber
	}
	applied, err := rt.svc.ApplySuggestion(r.Context(), scope, id, by, movesFrom)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, suggestionDTO(applied), started)
}

// getInvestigationClasses serves GET /api/v1/investigation-classes: the org's
// Classification set (ADR 0053 §5), in the operator's order.
func (rt *Router) getInvestigationClasses(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	set, err := rt.svc.ClassSet(r.Context(), scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, classSetDTO(set), started)
}

// replaceInvestigationClasses serves PUT /api/v1/investigation-classes: the whole set,
// replacing the old one. ⛔ No Finding is rewritten — each keeps the class it was given.
func (rt *Router) replaceInvestigationClasses(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[ReplaceInvestigationClassesRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	set, err := dto.toDomain()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	stored, err := rt.svc.ReplaceClassSet(r.Context(), scope, set)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, classSetDTO(stored), started)
}

// ------------------------------------------------------------------- helpers

// plainScope resolves the tenant and refuses every query parameter: none of these
// routes declares one.
func plainScope(r *http.Request) (db.TenantScope, error) {
	_, s, err := authn.Scope(r.Context())
	if err != nil {
		return db.TenantScope{}, err
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, err
	}
	return s, nil
}

// scopeAndID is plainScope and the `{id}` segment.
func scopeAndID(r *http.Request) (db.TenantScope, uuid.UUID, error) {
	s, err := plainScope(r)
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	return s, id, nil
}

// pathCase reads the Case `{id}`. A malformed id names no Case: 404, `case_not_found`,
// the answer another org's Case gets.
func pathCase(r *http.Request) (uuid.UUID, error) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return uuid.Nil, errs.NotFound("case_not_found", "no such case")
	}
	return id, nil
}

// pathIncident reads the Incident `{number}`. Anything that is not a positive number
// names no Incident: 404, `incident_not_found`, the answer another org's Incident gets.
func pathIncident(r *http.Request) (int64, error) {
	n, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil || n < 1 {
		return 0, errs.NotFound("incident_not_found", "no such incident")
	}
	return n, nil
}
