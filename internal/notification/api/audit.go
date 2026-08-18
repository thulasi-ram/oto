package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// Query-parameter allow-lists. §E.3 is binding: an unknown query parameter is
// REJECTED, because a typo'd `?statuss=dead` that is silently ignored returns the
// wrong page and looks right.
var (
	notificationParams = []string{
		"status", "reason", "suppressed_reason", "group_id", "alert_id", "policy_id",
		"since", "until", "limit", "cursor",
	}
	deliveryParams = []string{
		"status", "error_class", "mode", "channel_id", "notification_id", "ambiguous",
		"since", "until", "limit", "cursor", "since_seq",
	}
)

// The closed vocabularies the query parser accepts, mirroring the contract.
var (
	notificationStatuses = []string{"pending", "dispatched", "partial", "delivered", "failed", "suppressed"}
	// ⛔ `storm` AND `flapping` ARE NOT HERE, AND THE ABSENCE IS THE POINT. Both
	// dampers are removed, `notifications_suppmap_ck` no longer admits either
	// (migration 00059), and a filter value the column can no longer hold is a
	// query that always returns nothing — the worst possible answer to "why was I
	// not told?", because it looks like a fact rather than a dead axis.
	// ⚠️ `snoozed` IS STILL ABSENT AND THAT IS A PRE-EXISTING GAP, NOT THIS
	// CHANGE. Migration 00018 added it to `notifications_suppmap_ck`, oto writes
	// it, and this filter has never been able to select it. Closing that is a
	// widening of the contract enum and belongs to whoever owns the audit filter.
	suppressedReasons = []string{"no_policy", "throttled", "verbosity",
		"channel_disabled", "duplicate_render"}
	deliveryStatuses = []string{"pending", "sending", "sent", "failed", "dead", "skipped"}
	errorClasses     = []string{"retryable", "rate_limited", "permanent", "config_invalid", "auth_expired"}
	deliveryModes    = []string{"post_root", "update_root", "thread_reply", "broadcast_reply"}
)

// listNotifications serves GET /api/v1/notifications.
//
// ⛔ SUPPRESSED INTENTS ARE IN THIS LIST. Every intent oto formed is here,
// including the ones it deliberately said nothing about, and why. Suppression is
// always recorded: damping that cannot be inspected is indistinguishable from a
// bug, and silence that cannot be explained destroys trust (§B.6).
func (rt *Router) listNotifications(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.audit != nil, "audit_unavailable",
		"the notification audit is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, notificationParams...)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	f := domain.NotificationFilter{
		GroupID:  p.UUID("group_id"),
		AlertID:  p.UUID("alert_id"),
		PolicyID: p.UUID("policy_id"),
		Since:    p.Time("since"),
		Until:    p.Time("until"),
	}
	for _, v := range p.EnumCSV("status", notificationStatuses...) {
		f.Statuses = append(f.Statuses, domain.Status(v))
	}
	for _, v := range p.CSV("reason") {
		reason := domain.Reason(v)
		if !reason.Valid() {
			httpx.WriteProblem(w, r, errs.Validation("validation_failed", "1 field failed validation.",
				errs.Violation{Field: "reason", Code: "enum", Message: "unknown notification reason " + v}))
			return
		}
		f.Reasons = append(f.Reasons, reason)
	}
	for _, v := range p.EnumCSV("suppressed_reason", suppressedReasons...) {
		f.SuppressedReasons = append(f.SuppressedReasons, domain.SuppressedReason(v))
	}
	limit := p.Limit()
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	f.FilterHash = httpx.FilterHash(notificationFilterParts(f)...)
	cursor, err := httpx.DecodeCursor(p.Cursor(), f.FilterHash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	notifications, next, err := rt.audit.ListNotifications(
		r.Context(), scope, f, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]NotificationDTO, 0, len(notifications))
	for _, n := range notifications {
		// The list carries no per-row delivery summary: computing one would be a
		// query per row. The detail endpoint carries the full fan-out, and the
		// contract marks `delivery_summary` optional for exactly this reason.
		out = append(out, notificationDTO(n, nil))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// getNotification serves GET /api/v1/notifications/{id}.
func (rt *Router) getNotification(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.notifications != nil && rt.audit != nil, "audit_unavailable",
		"the notification audit is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	n, err := rt.notifications.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	deliveries, err := rt.audit.DeliveriesFor(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	channelIDs := make([]uuid.UUID, 0, len(deliveries))
	for _, d := range deliveries {
		channelIDs = append(channelIDs, d.ChannelID)
	}
	contexts, err := rt.audit.ChannelContextFor(r.Context(), scope, channelIDs)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows := make([]DeliveryDTO, 0, len(deliveries))
	for _, d := range deliveries {
		rows = append(rows, deliveryDTO(d, contexts[d.ChannelID]))
	}

	// The summary is set on both the embedded DTO and the detail's own field.
	// They are the same value; the detail's field is the one that reaches the
	// wire (encoding/json prefers the shallower name) and is what makes
	// `delivery_summary` structurally unskippable on this response.
	summary := summarise(deliveries)
	httpx.Data(w, r, http.StatusOK, NotificationDetailDTO{
		NotificationDTO: notificationDTO(n, &summary),
		DeliverySummary: summary,
		Deliveries:      rows,
	}, started)
}

// listDeliveries serves GET /api/v1/deliveries.
//
// The operator's view of whether messages actually LANDED. `status=dead` finds
// the channels that have stopped working; `ambiguous=true` finds the messages oto
// re-sent after a crash and deliberately labelled as possible duplicates.
func (rt *Router) listDeliveries(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.audit != nil, "audit_unavailable",
		"the delivery audit is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, deliveryParams...)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	f := domain.DeliveryFilter{
		ChannelID:      p.UUID("channel_id"),
		NotificationID: p.UUID("notification_id"),
		Ambiguous:      p.Bool("ambiguous"),
		Since:          p.Time("since"),
		Until:          p.Time("until"),
	}
	for _, v := range p.EnumCSV("status", deliveryStatuses...) {
		f.Statuses = append(f.Statuses, domain.DeliveryStatus(v))
	}
	for _, v := range p.EnumCSV("error_class", errorClasses...) {
		f.ErrorClasses = append(f.ErrorClasses, domain.ErrorClass(v))
	}
	for _, v := range p.EnumCSV("mode", deliveryModes...) {
		f.Modes = append(f.Modes, domain.Mode(v))
	}
	limit := p.Limit()
	if n := p.Int("since_seq", 0); n < 0 {
		httpx.WriteProblem(w, r, errs.Malformed("validation_failed", "since_seq must be >= 0"))
		return
	}
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	f.FilterHash = httpx.FilterHash(deliveryFilterParts(f)...)
	cursor, err := httpx.DecodeCursor(p.Cursor(), f.FilterHash)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	deliveries, contexts, next, err := rt.audit.ListDeliveries(
		r.Context(), scope, f, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]DeliveryDTO, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, deliveryDTO(d, contexts[d.ID]))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// getDelivery serves GET /api/v1/deliveries/{id}.
//
// It carries the exact provider-native payload that was rendered. When outbound
// validation rejects a message the delivery goes straight to `dead` with
// `config_invalid` and the offending payload is retrievable HERE: it is never
// truncated to fit and never sent — that would be an oto bug, and oto alerts on
// itself for it (§L.6).
func (rt *Router) getDelivery(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.deliveries != nil, "audit_unavailable",
		"the delivery audit is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	d, err := rt.deliveries.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, deliveryDetailDTO(d, rt.channelContext(r, scope, d.ChannelID)), started)
}

// retryDelivery serves POST /api/v1/deliveries/{id}/retry.
//
// ⛔ ONLY A `dead` DELIVERY CAN BE RETRIED THIS WAY; anything else is a `412`.
// Pending and failed deliveries are already on their own backoff schedule and do
// not need a nudge — nudging one would double-send.
//
// The state transition and the re-enqueue are separate on purpose: the row moving
// back to `pending` is the durable fact, and the job insert is the thing that
// makes it prompt. If the queue is unavailable the delivery is still pending and
// the retry sweep collects it, so the operator's action is never silently lost.
func (rt *Router) retryDelivery(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.audit != nil, "audit_unavailable",
		"the delivery audit is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	d, requeued, err := rt.audit.RequeueDead(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if !requeued {
		// The UPDATE matched nothing. Re-read to tell "no such delivery" from "not
		// dead": both are honest answers and they are different problems.
		if rt.deliveries != nil {
			existing, gerr := rt.deliveries.Get(r.Context(), scope, id)
			if gerr != nil {
				httpx.WriteProblem(w, r, gerr)
				return
			}
			httpx.WriteProblem(w, r, errs.Precondition("delivery_not_dead",
				"only a dead delivery can be retried; this one is "+string(existing.Status)))
			return
		}
		httpx.WriteProblem(w, r, errs.Precondition("delivery_not_dead",
			"only a dead delivery can be retried"))
		return
	}

	if rt.enqueuer != nil {
		ctxInfo := rt.channelContext(r, scope, d.ChannelID)
		if _, eerr := rt.enqueuer.Enqueue(r.Context(), jobs.DeliverDispatchArgs{
			DeliveryID:  d.ID,
			ChannelType: string(ctxInfo.ChannelType),
		}); eerr != nil {
			// The delivery is already pending, so this is not a failure of the
			// operator's request. Reporting it would invite them to retry an action
			// that has already taken effect.
			httpx.Data(w, r, http.StatusOK, deliveryDTO(d, ctxInfo), started)
			return
		}
		httpx.Data(w, r, http.StatusOK, deliveryDTO(d, ctxInfo), started)
		return
	}
	httpx.Data(w, r, http.StatusOK, deliveryDTO(d, rt.channelContext(r, scope, d.ChannelID)), started)
}

// channelContext resolves one channel's name and type for a detail response.
//
// A failure is not fatal: the delivery is real and showing it without its
// channel's display name is strictly better than a 500 on a debugging page.
func (rt *Router) channelContext(
	r *http.Request, scope db.TenantScope, channelID uuid.UUID,
) domain.DeliveryContext {
	if rt.audit == nil || channelID == uuid.Nil {
		return domain.DeliveryContext{}
	}
	contexts, err := rt.audit.ChannelContextFor(r.Context(), scope, []uuid.UUID{channelID})
	if err != nil {
		return domain.DeliveryContext{}
	}
	return contexts[channelID]
}

// ------------------------------------------------------------- filter hashing

// notificationFilterParts renders the filter for the cursor hash.
//
// The hash is what makes a cursor honest: one minted under `?status=suppressed`
// and replayed against `?status=delivered` describes a position in a sequence that
// no longer exists, and without the binding the server would serve a page from
// the middle of the wrong list and nothing would look wrong (§E.1).
func notificationFilterParts(f domain.NotificationFilter) []string {
	parts := make([]string, 0, 8)
	for _, v := range f.Statuses {
		parts = append(parts, "status="+string(v))
	}
	for _, v := range f.Reasons {
		parts = append(parts, "reason="+string(v))
	}
	for _, v := range f.SuppressedReasons {
		parts = append(parts, "suppressed_reason="+string(v))
	}
	parts = append(parts,
		"group_id="+f.GroupID.String(),
		"alert_id="+f.AlertID.String(),
		"policy_id="+f.PolicyID.String(),
		"since="+timeKey(f.Since),
		"until="+timeKey(f.Until),
	)
	return parts
}

func deliveryFilterParts(f domain.DeliveryFilter) []string {
	parts := make([]string, 0, 8)
	for _, v := range f.Statuses {
		parts = append(parts, "status="+string(v))
	}
	for _, v := range f.ErrorClasses {
		parts = append(parts, "error_class="+string(v))
	}
	for _, v := range f.Modes {
		parts = append(parts, "mode="+string(v))
	}
	ambiguous := "any"
	if f.Ambiguous != nil {
		ambiguous = "false"
		if *f.Ambiguous {
			ambiguous = "true"
		}
	}
	parts = append(parts,
		"channel_id="+f.ChannelID.String(),
		"notification_id="+f.NotificationID.String(),
		"ambiguous="+ambiguous,
		"since="+timeKey(f.Since),
		"until="+timeKey(f.Until),
	)
	return parts
}

func timeKey(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
