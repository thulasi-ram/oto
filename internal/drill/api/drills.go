package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
	"github.com/thulasiram/oto/internal/drill/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// DrillService is the port this layer declares. It is satisfied by
// *service.Service.
type DrillService interface {
	Start(ctx context.Context, s db.TenantScope, cmd service.StartCommand) (domain.Drill, domain.Result, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Drill, domain.Result, error)
	List(ctx context.Context, s db.TenantScope, sourceID uuid.UUID, limit int) ([]domain.Drill, []domain.Result, error)
	DisposeNow(ctx context.Context, s db.TenantScope, id uuid.UUID) error
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ DrillService = (*service.Service)(nil)

// Router serves the delivery-drill surface.
type Router struct {
	svc DrillService
	clk clock.Clock
}

// NewRouter builds it.
func NewRouter(svc DrillService, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, clk: clk}
}

// Mount registers the drill routes.
//
// ⭐ A DRILL IS ITS OWN RESOURCE, not a sub-resource of the source it drills, and
// the source travels in the body. It has its own id, its own lifetime, its own
// disposal and its own polling URL — and nesting it under `/sources/{id}` would
// have meant a chi subrouter collision with the sources module, which owns that
// whole prefix. The UI button still lives on the sources screen; where a thing is
// created is not where it belongs.
func (rt *Router) Mount(r chi.Router) {
	r.Route("/drills", func(r chi.Router) {
		r.Post("/", rt.startDrill)
		r.Get("/", rt.listDrills)
		r.Get("/{id}", rt.getDrill)
		r.Delete("/{id}", rt.disposeDrill)
	})
}

// startDrill is `POST /api/v1/drills`.
//
// ⭐ IT ANSWERS 202, NOT 200, AND THE STATUS IS THE CONTRACT. The pipeline a
// drill drives is asynchronous by design (§G.1), so the honest answer to "did it
// work" at this moment is "it started" — and the body carries the full stage list
// with everything still pending, so a client renders the same component it will
// render for every subsequent poll.
func (rt *Router) startDrill(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	principal, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	body, err := httpx.Bind[StartDrillRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	actorID := uuid.Nil
	if id, perr := uuid.Parse(principal.ActorID()); perr == nil {
		actorID = id
	}

	drill, res, err := rt.svc.Start(r.Context(), scope, service.StartCommand{
		SourceID:   body.SourceID,
		Severity:   body.Severity,
		ActorID:    actorID,
		ActorLabel: principal.ActorLabel(),
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusAccepted, drillDTO(drill, res), started)
}

// getDrill is `GET /api/v1/drills/{id}` — the poll.
func (rt *Router) getDrill(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	drill, res, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, drillDTO(drill, res), started)
}

// listDrills is `GET /api/v1/drills?source_id=…` — the recent history.
//
// ⛔ NO KEYSET AND NO CURSOR, deliberately. A drill is one operator pressing one
// button; a source with more than a handful is already a story. The bound is
// hard-coded rather than exposed, so this can never become a list endpoint whose
// paging contract has to be maintained.
func (rt *Router) listDrills(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	p := httpx.NewParams(r, "source_id")
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	sourceID, err := requiredSourceID(p.String("source_id", ""))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	drills, results, err := rt.svc.List(r.Context(), scope, sourceID, listLimit)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]DrillDTO, len(drills))
	for i := range drills {
		out[i] = drillDTO(drills[i], results[i])
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}

// disposeDrill is `DELETE /api/v1/drills/{id}`.
//
// ⭐ IT DELETES THE SYNTHETIC ROWS AND KEEPS THE RECEIPT, which is why it answers
// 200 with the drill rather than 204. "Deleted" here means "the fake alert is
// gone"; the record that a drill ran, and what it found, is not a thing an
// operator should be able to erase — that is the same rule the timeline lives by.
func (rt *Router) disposeDrill(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.svc.DisposeNow(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	drill, res, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, drillDTO(drill, res), started)
}

// listLimit bounds the drill history one source reports.
const listLimit = 20

// requiredSourceID reads the one query parameter `GET /api/v1/drills` takes.
//
// ⛔ EVERY WAY OF GETTING IT WRONG IS A 400, NEVER A 422, and the distinction is
// the contract's rather than this handler's taste. `source_id` is declared
// `required: true` on a QUERY string: a request without it — or with a value that
// is not a UUID at all — never formed a valid request against the contract, which
// is the `malformed_request` family §L.1 gives 400 and `httpx.NewParams` already
// answers for `unknown_parameter` two lines above. 422 is for a body that PARSED
// and then broke a rule, and `listDeliveryDrills` declares no 422 precisely
// because it has no body. It answered one anyway until git-bug ee3ae9c, with no
// `violations[]`, on the plainest request there is: `GET /api/v1/drills`.
//
// ⭐ AND IT CARRIES `violations[]`, ON A 400, ON PURPOSE. Violations name a
// MEMBER OF THE REQUEST; they are not a property of 422 (§L.1, and the `Problem`
// schema's own description). The sibling refusal from the very same parsing step
// on the very same query string — `unknown_parameter`, two lines above in
// `listDrills` — has always named its parameter this way. A `source_id_required`
// that named nothing would leave one of two adjacent 400s actionable and the
// other prose, on the one screen that has a source picker to highlight.
func requiredSourceID(raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, errs.Malformed("source_id_required",
			"source_id is required: a drill list is always about one source").
			WithViolations(errs.Violation{
				Field: "source_id", Code: "required", Message: "source_id is required",
			})
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, errs.Malformed("source_id_required",
			"source_id must be a UUID naming the source whose drills to list").
			WithViolations(errs.Violation{
				Field: "source_id", Code: "uuid", Message: "source_id must be a UUID",
			})
	}
	return id, nil
}

// subject resolves the tenant and the path id, and rejects unknown query
// parameters.
func (rt *Router) subject(r *http.Request) (db.TenantScope, uuid.UUID, error) {
	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	return scope, id, nil
}
