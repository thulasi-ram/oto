package api

import (
	"net/http"
	"time"

	"github.com/thulasiram/oto/internal/platform/db"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// listAlerts is `GET /api/v1/alerts` — the workhorse.
//
// Every filter the contract exposes is applied here: state, severity, cluster,
// namespace, alertname, the label selector, ack, flapping, a time lower bound,
// free text and the two sort keys, keyset-paginated throughout. Sorting is
// restricted to two keys because a keyset cursor is only sound over an indexed,
// total ordering.
func (rt *Router) listAlerts(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	req, err := parseListAlerts(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.List(r.Context(), scope, req.Service)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]AlertDTO, 0, len(res.Alerts))
	for _, a := range res.Alerts {
		dto := alertDTO(a)
		rt.embed(r, scope, &dto, a, req.Include, started)
		out = append(out, dto)
	}
	httpx.List(w, r, out, pageOf(res.Cursor, req.Query.Limit), started)
}

// embed batch-loads the `include=` sub-resources.
//
// ⛔ `include=rule` carries the snapshot ID and nothing more. The full
// `RuleSnapshotDTO` is owned by `rules/api` — CONTEXT.md §5.4 forbids this
// package from naming `rules/domain` — and `/alerts/{id}/rule` serves it whole.
func (rt *Router) embed(
	r *http.Request, scope db.TenantScope, dto *AlertDTO, a domain.Alert, inc includeSet, now time.Time,
) {
	if !inc.CurrentOccurrence && !inc.Enrichments && !inc.Rule {
		return
	}

	if inc.CurrentOccurrence || inc.Rule {
		detail, err := rt.svc.Get(r.Context(), scope, a.ID())
		if err == nil {
			occ := detail.CurrentOccurrence
			if occ == nil {
				occ = detail.LatestOccurrence
			}
			if occ != nil {
				if inc.CurrentOccurrence {
					o := occurrenceDTO(*occ, now)
					dto.CurrentOccurrence = &o
				}
				if inc.Rule {
					if id := idPtr(occ.RuleSnapshotID()); id != nil {
						dto.Rule = &RuleSnapshotRef{ID: *id}
					}
				}
			}
		}
	}

	if inc.Enrichments {
		if rows, err := rt.svc.Enrichments(r.Context(), scope, a.ID()); err == nil {
			out := make([]EnrichmentDTO, 0, len(rows))
			for _, e := range rows {
				out = append(out, enrichmentDTO(e))
			}
			dto.Enrichments = out
		}
	}
}

// getAlert is `GET /api/v1/alerts/{id}`.
//
// It accepts either the UUID or the §C.2 `alert_key`, because the key is the
// human-copyable handle that appears in Slack buttons and in URLs, and a detail
// page that cannot open one is a dead link.
func (rt *Router) getAlert(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	detail, err := rt.resolveAlert(r, scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto := AlertDetailDTO{AlertDTO: alertDTO(detail.Alert)}
	occ := detail.CurrentOccurrence
	if occ == nil {
		occ = detail.LatestOccurrence
	}
	if occ != nil {
		o := occurrenceDTO(*occ, started)
		dto.CurrentOccurrence = &o
	}
	if detail.Snooze != nil {
		s := snoozeDTO(*detail.Snooze)
		dto.Snooze = &s
	}

	rows, err := rt.svc.Enrichments(r.Context(), scope, detail.Alert.ID())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto.EnrichmentSummary = make([]EnrichmentSummaryDTO, 0, len(rows))
	for _, e := range rows {
		dto.EnrichmentSummary = append(dto.EnrichmentSummary, enrichmentSummaryDTO(e))
	}

	httpx.Data(w, r, http.StatusOK, dto, started)
}

func (rt *Router) listAlertOccurrences(w http.ResponseWriter, r *http.Request) {
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
	page, limit, err := simplePage(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Occurrences(r.Context(), scope, id, page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]OccurrenceDTO, 0, len(res.Occurrences))
	for _, o := range res.Occurrences {
		out = append(out, occurrenceDTO(o, started))
	}
	httpx.List(w, r, out, pageOf(res.Cursor, limit), started)
}

// listAlertEvents is `GET /api/v1/alerts/{id}/events` — the alert-scoped
// timeline, ordered by oto's own clock so a skewed upstream can never make it
// render out of order.
func (rt *Router) listAlertEvents(w http.ResponseWriter, r *http.Request) {
	rt.timeline(w, r, "desc", func(rq timelineRequest, scope db.TenantScope) (eventsPage, error) {
		id, err := httpx.PathUUID(r, "id")
		if err != nil {
			return eventsPage{}, err
		}
		res, err := rt.svc.AlertTimeline(r.Context(), scope, id, rq.Window, rq.Page)
		if err != nil {
			return eventsPage{}, err
		}
		return eventsPage{Events: res.Events, Cursor: res.Cursor}, nil
	})
}

// listOccurrenceEvents is `GET /api/v1/occurrences/{id}/events` — "what happened
// during THIS outage", which is a different question from "what has this rule
// ever done" and therefore defaults to ascending order.
func (rt *Router) listOccurrenceEvents(w http.ResponseWriter, r *http.Request) {
	rt.timeline(w, r, "asc", func(rq timelineRequest, scope db.TenantScope) (eventsPage, error) {
		id, err := httpx.PathUUID(r, "id")
		if err != nil {
			return eventsPage{}, err
		}
		res, err := rt.svc.OccurrenceTimeline(r.Context(), scope, id, rq.Window, rq.Page)
		if err != nil {
			return eventsPage{}, err
		}
		return eventsPage{Events: res.Events, Cursor: res.Cursor}, nil
	})
}

func (rt *Router) getOccurrence(w http.ResponseWriter, r *http.Request) {
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
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	occ, err := rt.svc.GetOccurrence(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto := OccurrenceDetailDTO{
		OccurrenceDTO: occurrenceDTO(occ, started),
		Enrichments:   []EnrichmentDTO{},
	}
	if detail, err := rt.svc.Get(r.Context(), scope, occ.AlertID()); err == nil {
		ref := alertRefDTO(detail.Alert)
		dto.Alert = &ref
	}
	if rows, err := rt.svc.Enrichments(r.Context(), scope, occ.AlertID()); err == nil {
		for _, e := range rows {
			dto.Enrichments = append(dto.Enrichments, enrichmentDTO(e))
		}
	}

	httpx.Data(w, r, http.StatusOK, dto, started)
}

// listAlertEnrichments is `GET /api/v1/alerts/{id}/enrichments`.
//
// Every result is stamped with its producer, version, phase, duration and cache
// provenance, so a wrong answer is always traceable. A failed enrichment and a
// missing one are deliberately distinguishable — that is what `status` is for.
func (rt *Router) listAlertEnrichments(w http.ResponseWriter, r *http.Request) {
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
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, err := rt.svc.Enrichments(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]EnrichmentDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, enrichmentDTO(e))
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}

// listAlertNotifications is `GET /api/v1/alerts/{id}/notifications`.
//
// ⭐ Delivery failure MUST be visible per alert: oto's silence must never be
// indistinguishable from "no alert fired", which is why every row carries its
// delivery roll-up and its suppression reason rather than a single boolean.
func (rt *Router) listAlertNotifications(w http.ResponseWriter, r *http.Request) {
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
	page, limit, err := simplePage(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Notifications(r.Context(), scope, id, page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]NotificationDTO, 0, len(res.Notifications))
	for _, n := range res.Notifications {
		out = append(out, notificationDTO(n))
	}
	httpx.List(w, r, out, pageOf(res.Cursor, limit), started)
}

// ackAlert is `POST /api/v1/alerts/{id}/ack`.
//
// This is the same service method the Slack acknowledge button calls, so acking
// from chat and acking from the API produce byte-identical state. Acking an
// occurrence that has already ended is a 412 and not a 409: the request is valid,
// the entity is simply in the wrong state — which the service says by returning a
// precondition error, translated here by the shared problem writer.
func (rt *Router) ackAlert(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, actor, id, err := rt.action(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	body, err := optionalBody[AckRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	occ, err := rt.svc.Acknowledge(r.Context(), scope, id, actor, body.Note)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, occurrenceDTO(occ, started), started)
}

// unackAlert is `POST /api/v1/alerts/{id}/unack`: a DELIBERATE withdrawal,
// recorded with `reason: manual` to distinguish it from the automatic unack that
// happens when a new occurrence opens.
func (rt *Router) unackAlert(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, actor, id, err := rt.action(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if _, err := optionalBody[UnackRequest](w, r); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	occ, err := rt.svc.Unacknowledge(r.Context(), scope, id, actor)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, occurrenceDTO(occ, started), started)
}

// commentOnAlert is `POST /api/v1/alerts/{id}/comments`.
//
// A comment is an event like any other: it cannot be edited or deleted, because
// the timeline IS the record. 201 with the appended event is therefore the whole
// response.
func (rt *Router) commentOnAlert(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, actor, id, err := rt.action(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	body, err := httpx.Bind[CommentRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	ev, err := rt.svc.Comment(r.Context(), scope, id, actor, body.Body)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, eventDTO(ev), started)
}

// listLabelNames is `GET /api/v1/labels` — the filter bar's typeahead.
func (rt *Router) listLabelNames(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	q, err := parseLabelQuery(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	names, err := rt.svc.LabelNames(r.Context(), scope, q.Q, q.Limit)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]LabelNameDTO, 0, len(names))
	for _, n := range names {
		out = append(out, labelNameDTO(n))
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}

// listLabelValues is `GET /api/v1/labels/{name}/values`.
func (rt *Router) listLabelValues(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	name, err := httpx.PathString(r, "name", domain.MaxLabelNameBytes)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := validateLabelName(name); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	q, err := parseLabelQuery(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	values, err := rt.svc.LabelValues(r.Context(), scope, name, q.Q, q.Limit)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]LabelValueDTO, 0, len(values))
	for _, v := range values {
		out = append(out, labelValueDTO(v))
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}
