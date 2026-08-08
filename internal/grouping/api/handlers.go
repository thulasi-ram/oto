package api

import (
	"net/http"
	"sort"
	"time"

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
	req, err := parseListGroups(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.List(r.Context(), scope, req.Service)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Straight through. Every filter and the sort were applied in SQL — see the
	// note at the foot of router.go about why nothing is discarded here.
	out := make([]GroupDTO, 0, len(res.Groups))
	for _, g := range res.Groups {
		out = append(out, groupDTO(g))
	}
	httpx.List(w, r, out, httpx.PageOf(res.Cursor, req.Query.Limit), started)
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

	// The service returns the event it appended. Re-reading the timeline to find
	// it — which this handler used to do — is a second query that can legitimately
	// hand back somebody else's comment, appended a millisecond later, as if it
	// were the caller's own.
	res, err := rt.svc.Comment(r.Context(), scope, id, kind, actorID, label, body.Body)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, eventDTO(res.Event), started)
}

// snoozeAlertGroup is `POST /api/v1/alert-groups/{id}/snooze` (§B.8.3).
//
// ⛔ A FAN-OUT OF THE SAME PRIMITIVE, not a new one: one snooze per
// CURRENTLY-JOINED member alert. Alerts that join the group later are NOT
// snoozed. A snooze is never predictive, and a group-level mute would silence
// alerts nobody has ever seen — that is the difference between a quiet button
// and a blindfold.
//
// Nothing about the signals changes. Every member stays firing, stays whatever
// severity it was, and stays in the default list.
func (rt *Router) snoozeAlertGroup(w http.ResponseWriter, r *http.Request) {
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
	body, err := httpx.Bind[SnoozeRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	until, err := rt.snoozeUntil(body)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Snooze(r.Context(), scope, id, kind, actorID, label, until, body.Note)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if res.Members == 0 {
		httpx.WriteProblem(w, r, errs.Precondition("no_group_members",
			"this group has no currently-joined member alert to snooze"))
		return
	}

	detail, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, rt.detailDTO(r, scope, detail), started)
}

// unsnoozeAlertGroup is `POST /api/v1/alert-groups/{id}/unsnooze`: end the
// snooze on each currently-joined member.
//
// A member that is not snoozed is SKIPPED rather than failing the request — the
// same rule the group ack follows, and for the same reason: refusing the other
// thirty-nine because one had already woken makes the button unusable in exactly
// the situation it exists for.
func (rt *Router) unsnoozeAlertGroup(w http.ResponseWriter, r *http.Request) {
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
	if _, err := optionalBody[UnsnoozeRequest](w, r); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if _, err := rt.svc.Unsnooze(r.Context(), scope, id, kind, actorID, label); err != nil {
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

// snoozeUntil resolves the two spellings of "how long", refusing both and
// neither. There is no indefinite snooze (§B.8.3).
func (rt *Router) snoozeUntil(body SnoozeRequest) (time.Time, error) {
	switch {
	case body.Until != nil && body.DurationSeconds != nil:
		return time.Time{}, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "until", Code: "excluded_with",
				Message: "give either until or duration_seconds, never both"})
	case body.Until != nil:
		return body.Until.UTC(), nil
	case body.DurationSeconds != nil:
		return rt.now().Add(time.Duration(*body.DurationSeconds) * time.Second), nil
	default:
		return time.Time{}, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "until", Code: "required_without",
				Message: "a snooze must end: give until or duration_seconds. " +
					"There is no indefinite snooze"})
	}
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
// ⭐ The page comes from SQL. This handler used to call `Get()`, which
// materialises the ENTIRE membership, then sort and slice it in Go — correct for
// a group of forty and a full membership fetch for a storm of five thousand,
// which is the one case the endpoint exists to survive. `Members` is now a
// keyset read over `(joined_at DESC, occurrence_id DESC)`.
//
// Each member alert is still resolved through the `alerts` service port and
// never by reaching into another domain's repository (CONTEXT.md §5.4).
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
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Members(r.Context(), scope, id, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// One Alert can hold several episodes in one generation over its lifetime;
	// the LIST is of alerts, so a repeat is collapsed. The page boundary is the
	// membership row's, not the deduplicated slice's, so a duplicate never
	// silently shortens the next page.
	out := make([]AlertDTO, 0, len(res.Members))
	seen := make(map[uuid.UUID]struct{}, len(res.Members))
	for _, m := range res.Members {
		if _, dup := seen[m.AlertID()]; dup {
			continue
		}
		seen[m.AlertID()] = struct{}{}
		a, ok := rt.alert(r, scope, m.AlertID())
		if !ok {
			continue
		}
		out = append(out, alertDTO(a))
	}
	httpx.List(w, r, out, httpx.PageOf(res.Cursor, limit), started)
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

// sortedMembers orders membership newest join first, with the occurrence id as a
// deterministic tiebreak so two identical requests produce the same preview.
//
// It survives for `detailDTO`'s BOUNDED preview only, over a slice the service
// already holds. The paginated list is SQL's job — see listAlertGroupAlerts.
func sortedMembers(in []domain.Member) []domain.Member {
	out := append([]domain.Member(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].JoinedAt().Equal(out[j].JoinedAt()) {
			return out[i].OccurrenceID().String() > out[j].OccurrenceID().String()
		}
		return out[i].JoinedAt().After(out[j].JoinedAt())
	})
	return out
}
