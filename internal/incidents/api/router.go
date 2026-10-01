package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/incidents/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// IncidentService is the port this layer declares for itself, satisfied by
// *service.Service. It lists exactly the methods the handlers call.
//
// ⛔ THERE IS NO `SetState`, NO `Resolve` AND NO `Close` ON IT, and no route below
// could reach one if there were. An Incident's state is read off its Cases
// (ADR 0052 §3); its response is the incident tool's (§5).
type IncidentService interface {
	List(ctx context.Context, s db.TenantScope, p db.Keyset) ([]domain.Incident, db.Cursor, error)
	HoldingCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID) ([]domain.Incident, error)
	Get(ctx context.Context, s db.TenantScope, number int64) (domain.Detail, error)
	Draw(ctx context.Context, s db.TenantScope, caseIDs []uuid.UUID, by domain.Attribution) (domain.Detail, error)
	Add(ctx context.Context, s db.TenantScope, number int64, caseID uuid.UUID, by domain.Attribution) (domain.Detail, error)
	Remove(ctx context.Context, s db.TenantScope, number int64, caseID uuid.UUID, by domain.Attribution) (domain.Detail, error)
	Move(ctx context.Context, s db.TenantScope, from, to int64, caseID uuid.UUID, by domain.Attribution) (domain.Detail, error)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ IncidentService = (*service.Service)(nil)

// Router serves the Incidents tag.
type Router struct {
	svc IncidentService
	clk clock.Clock
}

// NewRouter builds the incidents HTTP surface.
func NewRouter(svc IncidentService, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, clk: clk}
}

// Mount registers every route this package owns onto r, which `internal/app` has
// already rooted at /api/v1.
//
// ⭐ ADDRESSED BY `{number}`, NOT BY ID. An Incident is something people say out
// loud — "it's in #4" — and the per-org number is its name (migration 00083), so
// the URL a human pastes is the one the API serves. The id is on every response
// for the clients that want it.
//
// ⭐ THE MEMBERSHIP VERBS ARE POSTs TO NAMED ACTIONS, NOT A DELETE. Removing a
// Case writes a tombstone and keeps the record — a DELETE would promise the row
// is gone — and `move` is one transaction over two Incidents that no single
// resource's DELETE could describe. None of the segments is one of AC-51's five
// closing verbs: a Case is never closed by being taken out of a story.
func (rt *Router) Mount(r chi.Router) {
	r.Route("/incidents", func(r chi.Router) {
		r.Get("/", rt.listIncidents)
		r.Post("/", rt.createIncident)
		r.Route("/{number}", func(r chi.Router) {
			r.Get("/", rt.getIncident)
			r.Post("/cases", rt.addIncidentCase)
			r.Post("/cases/{case_id}/remove", rt.removeIncidentCase)
			r.Post("/cases/{case_id}/move", rt.moveIncidentCase)
		})
	})
}

func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

// listIncidents serves GET /api/v1/incidents.
func (rt *Router) listIncidents(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	page, limit, caseID, err := listPage(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if caseID != uuid.Nil {
		// ⭐ "WHICH INCIDENT IS THIS CASE IN?" — none or one, so there is nothing
		// to page. A Case this org does not have is simply in none: the answer is
		// the same empty list another org's Case gets, and tells a caller nothing.
		held, err := rt.svc.HoldingCase(r.Context(), scope, caseID)
		if err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}
		out := make([]IncidentDTO, 0, len(held))
		for _, i := range held {
			out = append(out, incidentDTO(i))
		}
		httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, limit), started)
		return
	}
	incs, next, err := rt.svc.List(r.Context(), scope, page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]IncidentDTO, 0, len(incs))
	for _, i := range incs {
		out = append(out, incidentDTO(i))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// createIncident serves POST /api/v1/incidents: a human draws an Incident.
//
// ⚠️ NO IDEMPOTENCY CLAIM, AND THE RULE IS WHY ONE IS NOT NEEDED. A retried draw
// meets the first draw's memberships and is refused with `case_in_incident`
// naming the Incident the first attempt drew — the retry learns the answer it lost
// instead of drawing a second Incident. This is ack's "idempotent by state"
// argument, made by a unique index rather than a state machine.
func (rt *Router) createIncident(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, by, err := rt.actor(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[CreateIncidentRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	d, err := rt.svc.Draw(r.Context(), scope, dto.CaseIDs, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, detailDTO(d), started)
}

// getIncident serves GET /api/v1/incidents/{number}.
func (rt *Router) getIncident(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	number, err := pathNumber(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	d, err := rt.svc.Get(r.Context(), scope, number)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, detailDTO(d), started)
}

// addIncidentCase serves POST /api/v1/incidents/{number}/cases.
func (rt *Router) addIncidentCase(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, by, err := rt.actor(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	number, err := pathNumber(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[AddIncidentCaseRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	d, err := rt.svc.Add(r.Context(), scope, number, dto.CaseID, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, detailDTO(d), started)
}

// removeIncidentCase serves POST /api/v1/incidents/{number}/cases/{case_id}/remove.
func (rt *Router) removeIncidentCase(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, by, err := rt.actor(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	number, caseID, err := pathMember(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	d, err := rt.svc.Remove(r.Context(), scope, number, caseID, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, detailDTO(d), started)
}

// moveIncidentCase serves POST /api/v1/incidents/{number}/cases/{case_id}/move.
// The response is the DESTINATION Incident.
func (rt *Router) moveIncidentCase(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, by, err := rt.actor(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	number, caseID, err := pathMember(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[MoveIncidentCaseRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	d, err := rt.svc.Move(r.Context(), scope, number, dto.ToNumber, caseID, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, detailDTO(d), started)
}

// ------------------------------------------------------------------- helpers

// actor resolves the tenant and the HUMAN behind a membership verb, and rejects
// unknown query parameters.
//
// ⛔ IT REFUSES A NON-HUMAN PRINCIPAL. ADR 0052 §2 gives an Incident two possible
// authors, a human and a Correlator, and a Correlator draws through its own path;
// a membership change attributed to "system" would be a decision nobody can be
// asked about.
func (rt *Router) actor(r *http.Request) (db.TenantScope, domain.Attribution, error) {
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		return db.TenantScope{}, domain.Attribution{}, err
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, domain.Attribution{}, err
	}
	if kind, err := kernel.NewActorKind(p.ActorKind()); err != nil || !kind.IsHuman() {
		return db.TenantScope{}, domain.Attribution{},
			errs.Forbidden("forbidden", "this action requires a human actor")
	}
	by, err := domain.Human(p.UserID, p.ActorLabel())
	if err != nil {
		return db.TenantScope{}, domain.Attribution{}, err
	}
	return scope, by, nil
}

// scopeOf resolves the caller's tenant — the only sanctioned path from a request
// to a db.TenantScope.
func scopeOf(r *http.Request) (db.TenantScope, error) {
	_, s, err := authn.Scope(r.Context())
	return s, err
}

// maxNumberDigits bounds the `{number}` segment before it is parsed: int64 has 19
// digits, and anything longer names nothing.
const maxNumberDigits = 19

// pathNumber reads `{number}`.
//
// A malformed or non-positive number is a 404 and not a 422, for `PathUUID`'s
// reason: `/incidents/banana` names no Incident, and a well-formedness lecture
// tells a scanner more than it tells a caller.
func pathNumber(r *http.Request) (int64, error) {
	raw, err := httpx.PathString(r, "number", maxNumberDigits)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 0, domain.NotFound()
	}
	return n, nil
}

// pathMember reads `{number}` and `{case_id}`.
func pathMember(r *http.Request) (int64, uuid.UUID, error) {
	number, err := pathNumber(r)
	if err != nil {
		return 0, uuid.Nil, err
	}
	caseID, err := httpx.PathUUID(r, "case_id")
	if err != nil {
		return 0, uuid.Nil, domain.MemberNotFound()
	}
	return number, caseID, nil
}

// listParams is the allow-list of `GET /incidents`.
var listParams = []string{"limit", "cursor", "case_id"}

// listPage compiles the list query: a keyset page and, optionally, the one Case
// whose current Incident is asked for.
//
// The cursor is bound to that filter, so a cursor minted by the unfiltered list
// and replayed with `case_id` is the ordinary `cursor_filter_mismatch` — and the
// filtered answer is at most one row, so it never mints one of its own.
func listPage(r *http.Request) (db.Keyset, int, uuid.UUID, error) {
	p := httpx.NewParams(r, listParams...)
	limit := p.Limit()
	caseID := p.UUID("case_id")
	if err := p.Err(); err != nil {
		return db.Keyset{}, 0, uuid.Nil, err
	}
	hash := httpx.FilterHash()
	if caseID != uuid.Nil {
		hash = httpx.FilterHash("case_id=" + caseID.String())
	}
	cursor, err := httpx.DecodeCursor(p.Cursor(), hash)
	if err != nil {
		return db.Keyset{}, 0, uuid.Nil, err
	}
	return httpx.Keyset(limit, cursor), limit, caseID, nil
}
