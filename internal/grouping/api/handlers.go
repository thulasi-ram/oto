package api

import (
	"net/http"
	"sort"

	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// maxTopAlerts bounds the member preview on the detail view. The full list is
// `/alert-groups/{id}/alerts`.
const maxTopAlerts = 20

// listAlertGroups is `GET /api/v1/alert-groups` — the default UI landing view.
func (rt *Router) listAlertGroups(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	q, page, err := parseListGroups(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.List(r.Context(), scope, q.State, page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]GroupDTO, 0, len(res.Groups))
	for _, g := range res.Groups {
		if !matchesFilters(g, q) {
			continue
		}
		out = append(out, groupDTO(g))
	}
	httpx.List(w, r, out, httpx.PageOf(res.Cursor, q.Limit), started)
}

// getAlertGroup is `GET /api/v1/alert-groups/{id}` — one generation with its
// roll-up counts and a bounded member preview.
func (rt *Router) getAlertGroup(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	detail, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, rt.detailDTO(r, scope, detail), started)
}

// ackAlertGroup is `POST /api/v1/alert-groups/{id}/ack`.
//
// Every open member occurrence gets the same acknowledgement; members that have
// already ended are SKIPPED rather than failing the request, because refusing the
// other thirty-nine because one resolved would make the button unusable in
// exactly the storm it exists for. A group with no open members at all is a 412.
func (rt *Router) ackAlertGroup(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	kind, actorID, label, err := actorOf(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	body, err := optionalBody[AckRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Acknowledge(r.Context(), scope, id, kind, actorID, label, body.Note)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if res.Applied == 0 {
		httpx.WriteProblem(w, r, errs.Precondition("no_open_occurrence",
			"this group has no open member occurrence to acknowledge"))
		return
	}

	detail, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, rt.detailDTO(r, scope, detail), started)
}

// commentOnAlertGroup is `POST /api/v1/alert-groups/{id}/comments`.
//
// The contract returns the appended event. A group comment fans out onto every
// member's timeline, so the event returned here is the one written against the
// group itself — the others are reachable from each member's own timeline.
func (rt *Router) commentOnAlertGroup(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	kind, actorID, label, err := actorOf(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	body, err := httpx.Bind[CommentRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if _, err := rt.svc.Comment(r.Context(), scope, id, kind, actorID, label, body.Body); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Read the freshly appended entry back off the timeline rather than
	// synthesising one: the timeline IS the record, and a response assembled
	// from the request would be a claim about what was written instead of a
	// reading of it.
	ev, err := rt.latestComment(r, scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, ev, started)
}

// getAlertGroupTimeline is `GET /api/v1/alert-groups/{id}/timeline` — the
// signature view: every event from every member alert, occurrence, notification
// and delivery, merged into one ordered list.
func (rt *Router) getAlertGroupTimeline(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subjectNoParamCheck(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	rq, err := parseTimeline(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Timeline(r.Context(), scope, id, rq.Window, rq.Page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	evs := orderEvents(filterEvents(res.Events, rq.Types), rq.Query.Order)
	out := make([]AlertEventDTO, 0, len(evs))
	for _, e := range evs {
		out = append(out, eventDTO(e))
	}
	httpx.List(w, r, out, httpx.PageOf(res.Cursor, rq.Query.Limit), started)
}

// listAlertGroupAlerts is `GET /api/v1/alert-groups/{id}/alerts`.
//
// Membership is read from the generation's current members, newest join first,
// and each member alert is resolved through the `alerts` service port — never by
// reaching into another domain's repository.
func (rt *Router) listAlertGroupAlerts(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	p := httpx.NewParams(r, simplePageParams...)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	limit := p.Limit()
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	detail, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	members := sortedMembers(detail.Members)
	members = afterCursor(members, cursor)

	out := make([]AlertDTO, 0, limit)
	next := db.Cursor{Hash: cursor.Hash}
	seen := map[uuid.UUID]struct{}{}
	for _, m := range members {
		if _, dup := seen[m.AlertID()]; dup {
			continue
		}
		seen[m.AlertID()] = struct{}{}
		if len(out) == limit {
			next.HasMore = true
			break
		}
		a, ok := rt.alert(r, scope, m.AlertID())
		if !ok {
			continue
		}
		out = append(out, alertDTO(a))
		next.SortKey = m.JoinedAt()
		next.ID = m.AlertID()
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// ------------------------------------------------------------------ helpers

func (rt *Router) subject(r *http.Request) (db.TenantScope, uuid.UUID, error) {
	scope, id, err := rt.subjectNoParamCheck(r)
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	return scope, id, nil
}

func (rt *Router) subjectNoParamCheck(r *http.Request) (db.TenantScope, uuid.UUID, error) {
	scope, err := scopeOf(r)
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	return scope, id, nil
}

// detailDTO renders the generation, its severity roll-up and a bounded member
// preview.
func (rt *Router) detailDTO(r *http.Request, scope db.TenantScope, d service.Detail) GroupDetailDTO {
	dto := GroupDetailDTO{
		GroupDTO:       groupDTO(d.Group),
		SeverityCounts: map[string]int32{},
		TopAlerts:      []AlertRefDTO{},
	}
	for _, m := range sortedMembers(d.Members) {
		if len(dto.TopAlerts) >= maxTopAlerts {
			break
		}
		a, ok := rt.alert(r, scope, m.AlertID())
		if !ok {
			continue
		}
		dto.TopAlerts = append(dto.TopAlerts, alertRefDTO(a))
		if sev := a.Severity().String(); sev != "" {
			dto.SeverityCounts[sev]++
		}
	}
	return dto
}

// alert resolves one member alert through the cross-domain port. A member whose
// alert cannot be read is skipped rather than failing the page: a group card that
// refuses to render because one row is missing is worse than one that is short.
func (rt *Router) alert(r *http.Request, scope db.TenantScope, alertID uuid.UUID) (alertdomain.Alert, bool) {
	if rt.alerts == nil {
		return alertdomain.Alert{}, false
	}
	detail, err := rt.alerts.Get(r.Context(), scope, alertID)
	if err != nil {
		return alertdomain.Alert{}, false
	}
	return detail.Alert, true
}

// latestComment reads back the newest `comment.added` entry on the group
// timeline.
func (rt *Router) latestComment(
	r *http.Request, scope db.TenantScope, groupID uuid.UUID,
) (AlertEventDTO, error) {
	res, err := rt.svc.Timeline(r.Context(), scope, groupID, db.TimeWindow{}, httpx.Keyset(50, db.Cursor{}))
	if err != nil {
		return AlertEventDTO{}, err
	}
	var newest *alertdomain.Event
	for i := range res.Events {
		e := res.Events[i]
		if e.Type().String() != "comment.added" {
			continue
		}
		if newest == nil || e.RecordedAt().After(newest.RecordedAt()) {
			newest = &res.Events[i]
		}
	}
	if newest == nil {
		return AlertEventDTO{}, errs.Internal("comment_not_readable", errCommentUnreadable)
	}
	return eventDTO(*newest), nil
}

var errCommentUnreadable = errs.New(errs.KindInternal, "comment_not_readable",
	"the comment was written but could not be read back")

// sortedMembers orders membership newest join first, with the alert id as a
// deterministic tiebreak so two identical requests produce the same page.
func sortedMembers(in []domain.Member) []domain.Member {
	out := append([]domain.Member(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].JoinedAt().Equal(out[j].JoinedAt()) {
			return out[i].AlertID().String() > out[j].AlertID().String()
		}
		return out[i].JoinedAt().After(out[j].JoinedAt())
	})
	return out
}

// afterCursor drops everything at or before the caller's keyset position.
func afterCursor(in []domain.Member, c db.Cursor) []domain.Member {
	if c.IsZero() {
		return in
	}
	for i, m := range in {
		if m.JoinedAt().Before(c.SortKey) ||
			(m.JoinedAt().Equal(c.SortKey) && m.AlertID().String() < c.ID.String()) {
			return in[i:]
		}
	}
	return nil
}
