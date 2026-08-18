package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
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
		// ⭐ The snooze comes off the map the service batch-loaded beside the
		// page — ONE query for the whole list, or none when nothing on it is
		// snoozed. Asking per row would be the N+1 that kept this field off the
		// list in the first place, leaving the default view unable to tell a
		// quiet alert from a noisy one (§B.8.6).
		s, snoozed := res.Snoozes[a.ID()]
		withSnooze(&dto, s, snoozed)
		rt.embed(r, scope, &dto, a, req.Include, started)
		out = append(out, dto)
	}
	httpx.List(w, r, out, pageOf(res.Cursor, req.Query.Limit), started)
}

// listAlertRollups is `GET /api/v1/alerts/rollups` — server-side grouping.
//
// ⭐ WHY IT IS A SEPARATE OPERATION and not a `group_by` mode of `listAlerts`.
// Three reasons, all of them about honesty rather than taste:
//
//  1. The response shape is different in kind. A bucket is not an alert, so one
//     operation would have to return `oneOf` two schemas and every generated
//     client would have to narrow it at runtime.
//  2. The keyset is over a different total order — the bucket key, a string —
//     while `sort` on the list is over `last_seen_at`. One operation would carry
//     a `sort` enum half of whose values are invalid half of the time.
//  3. `include=` embeds sub-resources of an alert. A bucket has none.
//
// ⛔ And it is NOT `/alert-groups`. That endpoint is one generation of one
// ALERTMANAGER NOTIFICATION GROUP — it has a row, a generation and a chat
// thread. This is a view over the alert list (§A.1). Conflating them is the
// ambiguity the ubiquitous language bans by name.
func (rt *Router) listAlertRollups(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	req, err := parseListRollups(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Rollups(r.Context(), scope, req.Service)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	by := req.Service.By.String()
	out := make([]AlertRollupDTO, 0, len(res.Rollups))
	for _, b := range res.Rollups {
		out = append(out, rollupDTO(b, by))
	}

	page := httpx.Page{Limit: req.Query.Limit, HasMore: res.HasMore}
	if res.HasMore && len(res.Rollups) > 0 {
		page.NextCursor = encodeKeyCursor(res.Rollups[len(res.Rollups)-1].Key, req.Hash)
	}
	httpx.List(w, r, out, page, started)
}

// embed batch-loads the `include=` sub-resources.
//
// ⛔ `include=rule` carries the snapshot ID and nothing more. The full
// `RuleSnapshotDTO` is owned by `rules/api` — CONTEXT.md §5.4 forbids this
// package from naming `rules/domain` — and `/alerts/{id}/rule` serves it whole.
//
// ⭐ THIS IS NOT A REASON THE LIST CANNOT SHOW THE RULE, and if you came here to
// widen the ref into an embedded DTO, read **ADR 0025 first — the question is
// settled.** The id is half of a two-call join: the list returns one snapshot id
// per row, and `GET /api/v1/rule-snapshots/batch?id=…` returns the snapshots for
// the whole page in ONE further call. Two requests, never one per alert. Because
// snapshots are content-addressed, a page of fifty alerts under an unchanged rule
// asks about one snapshot, not fifty.
//
// The alternative — copying `RuleSnapshotDTO` into this package behind a
// consumer-declared port — was rejected on two counts and neither has expired: it
// puts a second, hand-maintained copy of a `rules`-owned schema under `alerts`'
// ownership, and it puts `expr` (up to 64 KiB) plus two label maps into every row
// of a two-hundred-row page, which is the payload explosion `include=` exists to
// keep opt-in.
func (rt *Router) embed(
	r *http.Request, scope db.TenantScope, dto *AlertDTO, a domain.Alert, inc includeSet, now time.Time,
) {
	if !inc.CurrentCase && !inc.Enrichments && !inc.Rule {
		return
	}

	if inc.CurrentCase || inc.Rule {
		detail, err := rt.svc.Get(r.Context(), scope, a.ID())
		if err == nil {
			ac := detail.CurrentCase
			if ac == nil {
				ac = detail.LatestCase
			}
			if ac != nil {
				if inc.CurrentCase {
					o := caseDTO(*ac, now)
					dto.CurrentCase = &o
				}
				if inc.Rule {
					if id := idPtr(ac.RuleSnapshotID()); id != nil {
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
	ac := detail.CurrentCase
	if ac == nil {
		ac = detail.LatestCase
	}
	if ac != nil {
		o := caseDTO(*ac, started)
		dto.CurrentCase = &o
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

	// ⭐ THE ANSWER TO "WAS ANYBODY TOLD". A failure to read it FAILS THE REQUEST
	// rather than quietly omitting the field: an alert page that renders without
	// a delivery roll-up is the exact false silence this field exists to prevent,
	// and a caller cannot distinguish "no deliveries" from "we could not look".
	rollup, err := rt.svc.DeliveryRollupForAlert(r.Context(), scope, detail.Alert.ID())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto.DeliverySummary = deliverySummaryDTO(rollup)

	httpx.Data(w, r, http.StatusOK, dto, started)
}

func (rt *Router) listAlertCases(w http.ResponseWriter, r *http.Request) {
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

	res, err := rt.svc.Cases(r.Context(), scope, id, page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]CaseDTO, 0, len(res.Cases))
	for _, o := range res.Cases {
		out = append(out, caseDTO(o, started))
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

// listCaseEvents is `GET /api/v1/cases/{id}/events` — "what happened
// during THIS outage", which is a different question from "what has this rule
// ever done" and therefore defaults to ascending order.
func (rt *Router) listCaseEvents(w http.ResponseWriter, r *http.Request) {
	rt.timeline(w, r, "asc", func(rq timelineRequest, scope db.TenantScope) (eventsPage, error) {
		id, err := httpx.PathUUID(r, "id")
		if err != nil {
			return eventsPage{}, err
		}
		res, err := rt.svc.CaseTimeline(r.Context(), scope, id, rq.Window, rq.Page)
		if err != nil {
			return eventsPage{}, err
		}
		return eventsPage{Events: res.Events, Cursor: res.Cursor}, nil
	})
}

// listCases is `GET /api/v1/cases` — the ORG-WIDE episode list, and the surface
// an operator actually opens: "what is firing that I need to acknowledge".
//
// ⛔ IT IS NOT `GET /alerts` WITH `?ack=`. That list pages IDENTITIES, and
// `alerts` has carried no ack column since 00049 — a receipt belongs to the
// firing it was given for, so an ack filter over that table asked whether a
// closed episode had been acknowledged. The facet lives here, where its subject
// still exists, and `?state=open&ack=unacked` is the shape case_ack_idx serves.
//
// ⭐ EVERY ROW CARRIES ITS ALERT, AND IT COSTS ONE QUERY FOR THE WHOLE PAGE. An
// episode has no `alertname` and no `severity` of its own; the service batch-loads
// the identities beside the page, exactly as it batch-loads snoozes for the alert
// list, so the list is readable without a request per row.
func (rt *Router) listCases(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	req, err := parseListCases(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.ListCases(r.Context(), scope, req.Service)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]CaseListItemDTO, 0, len(res.Cases))
	for _, c := range res.Cases {
		out = append(out, caseListItemDTO(c, res.Alerts[c.AlertID()], started))
	}
	httpx.List(w, r, out, pageOf(res.Cursor, req.Query.Limit), started)
}

func (rt *Router) getCase(w http.ResponseWriter, r *http.Request) {
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

	ac, err := rt.svc.GetCase(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto := CaseDetailDTO{
		CaseDTO:     caseDTO(ac, started),
		Enrichments: []EnrichmentDTO{},
	}
	if detail, err := rt.svc.Get(r.Context(), scope, ac.AlertID()); err == nil {
		ref := alertRefDTO(detail.Alert)
		dto.Alert = &ref
	}
	if rows, err := rt.svc.Enrichments(r.Context(), scope, ac.AlertID()); err == nil {
		for _, e := range rows {
			dto.Enrichments = append(dto.Enrichments, enrichmentDTO(e))
		}
	}

	// The episode-scoped answer to "was anybody told". Unlike the enrichments
	// above, a failure here is NOT swallowed: an absent enrichment is a missing
	// nicety, an absent delivery roll-up is oto claiming silence it has not
	// checked.
	rollup, err := rt.svc.DeliveryRollupForCase(r.Context(), scope, ac.ID())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto.DeliverySummary = deliverySummaryDTO(rollup)

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

// ackCase is `POST /api/v1/cases/{id}/ack`.
//
// ⭐ `{id}` IS A CASE ID. A receipt is a fact about ONE contiguous firing
// episode — it lives on `alert_cases` and is cleared when the next episode opens
// — so the route names the episode rather than the identity that is having it.
// The alert-addressed spelling had to resolve "whatever is open right now",
// which made the subject of the receipt a race with the state machine.
//
// This is the same service method the Slack acknowledge button reaches (through
// the group fan-out), so acking from chat and acking from the API produce
// byte-identical state. Acking a case that has already ended is a 412 and not a
// 409: the request is valid, the entity is simply in the wrong state — which the
// service says by returning a precondition error, translated here by the shared
// problem writer.
func (rt *Router) ackCase(w http.ResponseWriter, r *http.Request) {
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

	ac, err := rt.svc.Acknowledge(r.Context(), scope, id, actor, body.Note)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, caseDTO(ac, started), started)
}

// unackCase is `POST /api/v1/cases/{id}/unack`: a DELIBERATE withdrawal,
// recorded with `reason: manual` to distinguish it from the automatic unack that
// happens when a new case opens. `{id}` is a case id, for the reason argued on
// ackCase.
func (rt *Router) unackCase(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, actor, id, err := rt.action(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	body, err := optionalBody[UnackRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// `note` is carried through to the `case.unacknowledged` event. It used
	// to be bound here, length-validated here, and then thrown away.
	ac, err := rt.svc.Unacknowledge(r.Context(), scope, id, actor, body.Note)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, caseDTO(ac, started), started)
}

// commentOnAlert is `POST /api/v1/alerts/{id}/comments`.
//
// A comment is an event like any other: it cannot be edited or deleted, because
// the timeline IS the record. 201 with the appended event is therefore the whole
// response.
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` IS REPLAYED, NOT REPEATED, and
// the `201` it gets back names the SAME event id as the first attempt. This
// endpoint declared the header and read it nowhere; nothing else protected it,
// because a comment has no state machine to refuse a repeat and its §C.8 dedupe
// key was minted from the wall clock. A dropped response to a comment — during
// exactly the network conditions the header exists for — used to leave two
// identical annotations on the timeline that IS the record.
func (rt *Router) commentOnAlert(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, actor, id, err := rt.action(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// The RAW bytes, before Bind consumes the stream: "the same body" is decided
	// by the sha256 of what the caller actually sent, not by a re-encoding of the
	// DTO it parsed into.
	raw, err := httpx.ReadBody(w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	body, err := httpx.Bind[CommentRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	idem, err := idempotencyIntent(r, service.OpCommentOnAlert, id, raw)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	ev, _, err := rt.svc.Comment(r.Context(), scope, id, actor, body.Body, idem)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// A replay answers `201` with the ORIGINAL event and not a `200`: the contract
	// promises "the original result rather than acting twice", and a caller that
	// could not tell the two apart is a caller whose retry logic works.
	httpx.Data(w, r, http.StatusCreated, eventDTO(ev), started)
}

// ------------------------------------------------------------------- snooze

// snoozeAlert is `POST /api/v1/alerts/{id}/snooze` (§B.8).
//
// ⛔ IT CHANGES NOTHING ABOUT THE SIGNAL. Snooze suppresses OTO'S OWN
// notifications for one alert_key until a fixed time; it writes nothing into
// Alertmanager, moves no state, and touches no severity. The alert stays firing
// and every surface MUST keep rendering it that way — colouring a snoozed
// critical calm would be the exact lie §E.1.1 exists to prevent.
//
// It is auto-expiring by construction: 5 minutes to 30 days, never indefinite.
// An unexpiring snooze is a mute, and mutes are how channels die.
//
// The response is the alert detail, so the caller sees the snooze in force
// alongside the state it did not change.
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` REPLAYS THE SNOOZE IT ALREADY
// GRANTED. Unguarded, a retry ended the caller's own snooze as `superseded`,
// inserted a replacement, and enqueued a second "snoozed" notification — one user
// click, two rows and two outbound messages. `until` is resolved BEFORE the
// intent because a relative `duration` would otherwise digest differently on
// every attempt; the hash is over the bytes the caller sent, which do not move.
func (rt *Router) snoozeAlert(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, actor, id, err := rt.action(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	raw, err := httpx.ReadBody(w, r)
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
	idem, err := idempotencyIntent(r, service.OpSnoozeAlert, id, raw)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if _, _, err := rt.svc.Snooze(r.Context(), scope, id, actor, until, body.Note, idem); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	rt.writeAlertDetail(w, r, scope, id, started)
}

// unsnoozeAlert is `POST /api/v1/alerts/{id}/unsnooze` — end an active snooze
// early. An alert that is not snoozed is a `412`: the request is well-formed,
// the entity is simply in the wrong state.
func (rt *Router) unsnoozeAlert(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, actor, id, err := rt.action(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	body, err := optionalBody[UnsnoozeRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if _, err := rt.svc.Unsnooze(r.Context(), scope, id, actor, body.Note); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	rt.writeAlertDetail(w, r, scope, id, started)
}

// unsnoozeAlerts is `POST /api/v1/alerts/unsnooze` — wake several NAMED alerts
// at once, so the Quiet tab can resume a selection in one gesture.
//
// ⛔⛔ THE SUBJECTS ARE IN THE BODY AND WILL NEVER BE A FILTER. See
// UnsnoozeAlertsRequest for the argument; the short form is that a filter is
// evaluated against rows the caller never saw, and this verb makes oto TALK.
//
// ⭐⭐ PARTIAL SUCCESS IS A `200`, AND THE STATUS IS THE DECISION WORTH STATING.
// Every id named is reached and concluded on, and the answer is a per-alert
// account: an alert that was not snoozed is SKIPPED, never an error, because
// refusing the other ninety-nine over one that had already woken makes the button
// unusable in exactly the situation it exists for. That is the rule
// `POST /alert-groups/{id}/unsnooze` already follows for its members.
//
//   - It is not a `207`. Multi-Status would say something went wrong, and nothing
//     did — a skip is an outcome this endpoint exists to report. It would also be
//     the only status in this contract carrying a success envelope that is neither
//     a 2xx-with-`{data,meta}` nor a `Problem`, which every generated client and
//     every gate in test/contract is built around.
//   - It is not a `412` when NOTHING woke. The single-alert route answers
//     `412 not_snoozed` because it addresses ONE entity and has nothing else to
//     say; here there is always an account, and "all five were already awake" is a
//     complete, correct answer to the question that was asked.
//   - It is not a `404` for an id this org does not own. That id is reported as
//     `skipped`/`alert_not_found`, identically to an id belonging to nobody — the same
//     answer the single route gives, and for the same reason: any other treatment
//     is an existence oracle, and this endpoint would let it be walked a hundred
//     ids at a time.
//
// ⛔ A HARD FAILURE PARTWAY IS STILL A PROBLEM DOCUMENT, as it is on the group
// fan-out. The service hands the partial account back beside the error so nothing
// is lost at the seam, but the caller is told the request failed — and a retry
// converges, because the alerts that already woke answer `not_snoozed` on the
// second pass. That is also why this verb claims no `Idempotency-Key`: like ack,
// unack and the other two unsnoozes, it is idempotent by state machine, and the
// state after N calls equals the state after one.
func (rt *Router) unsnoozeAlerts(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	actor, err := actorOf(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	// ⛔ Bind AND NOT optionalBody. `alert_ids` is required, so an absent body is a
	// caller that has not said what to wake — which is precisely the "everything"
	// reading this endpoint refuses to have.
	body, err := httpx.Bind[UnsnoozeAlertsRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.UnsnoozeMany(r.Context(), scope, body.AlertIDs, actor, body.Note)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, unsnoozeAlertsDTO(res), started)
}

// listAlertSnoozes is `GET /api/v1/alerts/{id}/snoozes` — the §B.8.6 history.
//
// ⭐ Membership of a snooze is HISTORY, not a boolean. This is what makes the
// feature safe to ship: every quiet period is attributable, bounded and visible
// after the fact, so a snooze can be reviewed rather than merely forgotten.
func (rt *Router) listAlertSnoozes(w http.ResponseWriter, r *http.Request) {
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
	p := httpx.NewParams(r, "limit")
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, err := rt.svc.SnoozeHistory(r.Context(), scope, id, limit)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]SnoozeHistoryDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, snoozeHistoryDTO(s))
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}

// listSnoozes is `GET /api/v1/snoozes` — the §B.8.6 ORG-WIDE view of every quiet
// period currently in force, soonest wake-up first.
//
// ⭐ THIS IS THE COUNTERWEIGHT THAT MAKES SNOOZE SAFE TO SHIP. §B.8.6 requires a
// persistent banner enumerating every active snooze in the org with its expiry,
// "so a snooze cannot be forgotten". Without it the feature is a mute switch with
// no indicator light.
//
// ⛔ It is NOT `GET /alerts?snoozed=true`, and that endpoint cannot be made into
// it. That one pages ALERTS: it answers "which alerts are quiet" and structurally
// cannot answer "who asked, why, and until when", because those are facts about
// an `alert_snoozes` row and one alert has a whole history of them. Nor can it
// order by expiry, which is the one ordering a banner is read in.
func (rt *Router) listSnoozes(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	page, limit, err := simplePage(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.ActiveSnoozes(r.Context(), scope, page)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]ActiveSnoozeDTO, 0, len(res.Snoozes))
	for _, s := range res.Snoozes {
		// The Alert is looked up in the batch the service already loaded. A
		// snooze whose alert is missing is still listed, with a null `alert`:
		// dropping the row would hide a quiet period, which is the exact failure
		// this endpoint exists to prevent.
		var alert *domain.Alert
		if a, ok := res.Alerts[s.AlertKey().String()]; ok {
			alert = &a
		}
		out = append(out, activeSnoozeDTO(s, alert, started))
	}
	httpx.List(w, r, out, pageOf(res.Cursor, limit), started)
}

// snoozeUntil resolves the two spellings of "how long".
//
// ⛔ EXACTLY ONE of them, never both and never neither. `duration_seconds` is
// resolved against OTO'S clock rather than the caller's, so a client with a
// skewed clock cannot talk its way past the §B.8.3 bounds; the domain factory
// re-proves them either way, because a bound that lives in one place lives
// nowhere.
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

// writeAlertDetail re-reads and renders the alert after a snooze verb, so the
// response describes the row as it now stands rather than as the request hoped.
func (rt *Router) writeAlertDetail(
	w http.ResponseWriter, r *http.Request, scope db.TenantScope, id uuid.UUID, started time.Time,
) {
	detail, err := rt.svc.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto := AlertDetailDTO{AlertDTO: alertDTO(detail.Alert)}
	ac := detail.CurrentCase
	if ac == nil {
		ac = detail.LatestCase
	}
	if ac != nil {
		o := caseDTO(*ac, started)
		dto.CurrentCase = &o
	}
	if detail.Snooze != nil {
		s := snoozeDTO(*detail.Snooze)
		dto.Snooze = &s
	}
	dto.EnrichmentSummary = []EnrichmentSummaryDTO{}
	if rows, err := rt.svc.Enrichments(r.Context(), scope, detail.Alert.ID()); err == nil {
		for _, e := range rows {
			dto.EnrichmentSummary = append(dto.EnrichmentSummary, enrichmentSummaryDTO(e))
		}
	}
	// The snooze verbs return the same schema as `GET /alerts/{id}`, so they
	// carry the same delivery roll-up. A field that is present on one rendering
	// of a schema and absent on another is the drift this whole DTO layer exists
	// to prevent.
	rollup, err := rt.svc.DeliveryRollupForAlert(r.Context(), scope, detail.Alert.ID())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto.DeliverySummary = deliverySummaryDTO(rollup)

	httpx.Data(w, r, http.StatusOK, dto, started)
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
