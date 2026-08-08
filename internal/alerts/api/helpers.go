package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// maxLabelNameBytes mirrors `LabelName.maxLength` in openapi.yaml.
const maxLabelNameBytes = 1024

// simplePageParams is the allow-list of every plainly-paginated list endpoint.
var simplePageParams = []string{"limit", "cursor", "since_seq"}

// simplePage compiles a list query that has no filters of its own.
//
// The cursor is bound to the empty filter set: these endpoints are scoped by a
// path id rather than a query, so there is nothing a cursor could be minted
// under and later replayed against.
func simplePage(r *http.Request) (db.Keyset, int, error) {
	p := httpx.NewParams(r, simplePageParams...)
	if err := p.Err(); err != nil {
		return db.Keyset{}, 0, err
	}
	limit := p.Limit()
	if err := p.Err(); err != nil {
		return db.Keyset{}, 0, err
	}
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash())
	if err != nil {
		return db.Keyset{}, 0, err
	}
	return httpx.Keyset(limit, cursor), limit, nil
}

// eventsPage is one page of the append-only timeline, whatever subject it was
// read by.
type eventsPage struct {
	Events []domain.Event
	Cursor db.Cursor
}

// timeline is the shared body of every event-list endpoint.
//
// It exists so that `type`, `since`, `until`, `order` and the keyset envelope are
// implemented exactly once: three endpoints answering the same question in three
// slightly different ways is how a timeline starts disagreeing with itself.
func (rt *Router) timeline(
	w http.ResponseWriter, r *http.Request, defaultOrder string,
	fetch func(rq timelineRequest, scope db.TenantScope) (eventsPage, error),
) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	rq, err := parseTimeline(r, defaultOrder)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	page, err := fetch(rq, scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	evs := orderEvents(filterEvents(page.Events, rq.Types), rq.Query.Order)
	out := make([]AlertEventDTO, 0, len(evs))
	for _, e := range evs {
		out = append(out, eventDTO(e))
	}
	httpx.List(w, r, out, pageOf(page.Cursor, rq.Query.Limit), started)
}

// action resolves the three things every human verb needs: the tenant, the
// actor it is attributed to, and the subject.
func (rt *Router) action(r *http.Request) (db.TenantScope, domain.Actor, uuid.UUID, error) {
	scope, err := scopeOf(r)
	if err != nil {
		return db.TenantScope{}, domain.Actor{}, uuid.Nil, err
	}
	actor, err := actorOf(r.Context())
	if err != nil {
		return db.TenantScope{}, domain.Actor{}, uuid.Nil, err
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return db.TenantScope{}, domain.Actor{}, uuid.Nil, err
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, domain.Actor{}, uuid.Nil, err
	}
	return scope, actor, id, nil
}

// resolveAlert reads an alert by UUID, falling back to the §C.2 alert_key.
//
// The key is the human-copyable handle that appears in Slack buttons and URLs, so
// a detail endpoint that only accepts a UUID turns every one of those into a dead
// link.
func (rt *Router) resolveAlert(r *http.Request, scope db.TenantScope) (service.AlertDetail, error) {
	if id, err := httpx.PathUUID(r, "id"); err == nil {
		return rt.svc.Get(r.Context(), scope, id)
	}
	raw, err := httpx.PathString(r, "id", 64)
	if err != nil {
		return service.AlertDetail{}, err
	}
	if !validate.AlertKeyRe.MatchString(raw) {
		return service.AlertDetail{}, errs.NotFound("not_found", "no such alert")
	}
	return rt.svc.GetByKey(r.Context(), scope, raw)
}

// optionalBody decodes a request body the contract marks `required: false`.
//
// An absent body is the zero DTO and not an error; a PRESENT but malformed one
// still fails, because "I sent something and you ignored it" is the outcome this
// distinction exists to prevent.
func optionalBody[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var zero T
	if r.Body == nil || r.ContentLength == 0 {
		return zero, nil
	}
	dto, err := httpx.Bind[T](w, r)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) && e.Code == "empty_body" {
			return zero, nil
		}
		return zero, err
	}
	return dto, nil
}

// validateLabelName enforces `LabelName` on a path segment.
func validateLabelName(name string) error {
	if len(name) > maxLabelNameBytes || !validate.LabelNameRe.MatchString(name) {
		return errs.Validation("validation_failed", "1 field failed validation.", errs.Violation{
			Field: "name", Code: "labelname", Message: "must be a valid Prometheus label name",
		})
	}
	return nil
}
