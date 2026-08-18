package api

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// AlertService is the port this layer declares for itself, satisfied by
// *service.Service.
//
// It is declared HERE, by the consumer, and lists exactly the methods the
// handlers call — which is what makes the HTTP layer testable without a database
// and what stops a handler quietly acquiring a new capability.
type AlertService interface {
	List(ctx context.Context, s db.TenantScope, q service.ListQuery) (service.ListResult, error)
	Rollups(ctx context.Context, s db.TenantScope, q service.RollupQuery) (service.RollupResult, error)
	Get(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (service.AlertDetail, error)
	GetByKey(ctx context.Context, s db.TenantScope, alertKey string) (service.AlertDetail, error)
	Cases(ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset) (service.CaseResult, error)
	// ListCases is the ORG-WIDE episode list behind `GET /api/v1/cases` — a
	// different question from `Cases` above, which is one identity's history.
	ListCases(ctx context.Context, s db.TenantScope, q service.CaseListQuery) (service.CaseListResult, error)
	GetCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (domain.Case, error)
	AlertTimeline(ctx context.Context, s db.TenantScope, alertID uuid.UUID, w db.TimeWindow, p db.Keyset) (service.TimelineResult, error)
	CaseTimeline(ctx context.Context, s db.TenantScope, caseID uuid.UUID, w db.TimeWindow, p db.Keyset) (service.TimelineResult, error)
	Enrichments(ctx context.Context, s db.TenantScope, alertID uuid.UUID) ([]service.EnrichmentSummary, error)
	Notifications(ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset) (service.NotificationResult, error)
	// The two delivery roll-ups behind `delivery_summary` on the alert and
	// case detail views. They are on this interface — rather than being
	// derived from `Notifications` in the handler — because the roll-up covers
	// the GROUP generations an alert has been part of as well as the intents that
	// name it, and paging a list to add up its rows would be both wrong and N+1.
	DeliveryRollupForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (service.DeliveryRollup, error)
	DeliveryRollupForCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (service.DeliveryRollup, error)
	LabelNames(ctx context.Context, s db.TenantScope, prefix string, limit int) ([]domain.LabelCount, error)
	LabelValues(ctx context.Context, s db.TenantScope, name, prefix string, limit int) ([]domain.LabelCount, error)
	// ⭐ ACK AND UNACK ARE ADDRESSED BY CASE ID. An acknowledgement is a receipt
	// for ONE firing episode, so the route that writes it says so:
	// `POST /api/v1/cases/{id}/ack`. Snooze, three methods down, is deliberately
	// NOT the same shape — it is Alert-scoped by a separate decision (§B.8,
	// `alert_snoozes.alert_id`, migration 00048) and keeps its alert-addressed
	// route.
	Acknowledge(ctx context.Context, s db.TenantScope, caseID uuid.UUID, actor domain.Actor, note string) (domain.Case, error)
	Unacknowledge(ctx context.Context, s db.TenantScope, caseID uuid.UUID, actor domain.Actor, note string) (domain.Case, error)
	// ⭐ COMMENT AND SNOOZE CARRY THE CALLER'S `Idempotency-Key` INTENT AND ACK AND
	// UNACK DO NOT, and that asymmetry is the ruling rather than an oversight. Ack
	// and unack are idempotent by state machine — the state after N calls equals
	// the state after one — so a keyed retry meets `already_acked` / `not_acked`,
	// which is a settled answer and not a duplicated side effect. A comment is an
	// APPEND and a snooze SUPERSEDES its own incumbent; those two are the ones that
	// acted twice (ticket a6cc834). The second result is "this was a replay".
	Comment(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, body string, idem service.Idempotency) (domain.Event, bool, error)

	// ⛔ THE THIRD HUMAN VERB (§E.1.1, §B.8). Snooze writes NOTIFICATION state,
	// never signal state: the alert stays firing, stays whatever severity it was,
	// and every surface keeps rendering it that way. There is still no Resolve,
	// no Close and no Dismiss on this interface, and there never will be.
	Snooze(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, until time.Time, note string, idem service.Idempotency) (domain.Snooze, bool, error)
	Unsnooze(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, note string) (domain.Snooze, error)
	// UnsnoozeMany is the BULK wake behind `POST /api/v1/alerts/unsnooze`, and it
	// takes an EXPLICIT LIST rather than a filter: a filter-scoped bulk action would
	// resume thousands of alerts whose extent the caller cannot see, so the caller
	// must name what it is waking. It is the third member of the unsnooze family
	// beside the line above and `POST /alert-groups/{id}/unsnooze`, and it is a
	// fan-out of the SAME primitive — an alert that was not snoozed is SKIPPED and
	// reported, never allowed to fail the request.
	//
	// ⛔ THERE IS NO BULK SNOOZE ON THIS INTERFACE. Only the undo is offered.
	UnsnoozeMany(ctx context.Context, s db.TenantScope, alertIDs []uuid.UUID, actor domain.Actor, note string) (service.UnsnoozeManyResult, error)
	SnoozeHistory(ctx context.Context, s db.TenantScope, alertID uuid.UUID, limit int) ([]domain.Snooze, error)
	// ActiveSnoozes is the ORG-WIDE §B.8.6 view: everything oto is currently
	// quiet about, soonest wake-up first. It is what the persistent banner is
	// built from, and it is the reason a snooze cannot be forgotten.
	ActiveSnoozes(ctx context.Context, s db.TenantScope, p db.Keyset) (service.ActiveSnoozeResult, error)

	// ⭐⭐ THE CONFIGURATION HALF OF THE CASE. These four are `case_policy_config`
	// (migration 00057): the CASE RETENTION WINDOW W per (namespace, alertname),
	// which decides WHEN a resolved case closes and therefore whether a flap is one
	// episode or six. They are on THIS interface — the alerts module's — because the
	// module that owns the Case owns the rule that shapes it; they are not on the
	// reader the §B.3 machine consults, because the evaluator must not be able to
	// rewrite the rule it is evaluating (see service.CasePolicyConfigStore).
	//
	// CreateCasePolicy's second result reports "this was a replay" for the same
	// reason Comment's and Snooze's do.
	CasePolicies(ctx context.Context, s db.TenantScope, p db.Keyset) ([]domain.CasePolicy, db.Cursor, error)
	CreateCasePolicy(ctx context.Context, s db.TenantScope, in domain.CasePolicyDraft, idem service.Idempotency) (domain.CasePolicy, bool, error)
	UpdateCasePolicy(ctx context.Context, s db.TenantScope, policyID uuid.UUID, p domain.CasePolicyPatch) (domain.CasePolicy, error)
	DeleteCasePolicy(ctx context.Context, s db.TenantScope, policyID uuid.UUID) error
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ AlertService = (*service.Service)(nil)

// Router serves the Alerts, Cases and Discovery tags.
type Router struct {
	svc AlertService
	clk clock.Clock
}

// NewRouter builds the alerts HTTP surface.
func NewRouter(svc AlertService, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, clk: clk}
}

// Register mounts every route this package owns onto r, which `internal/app`
// has already rooted at /api/v1.
func (rt *Router) Register(r chi.Router) {
	r.Route("/alerts", func(r chi.Router) {
		r.Get("/", rt.listAlerts)
		// Registered before /{id} so the static segment wins the chi trie. A
		// roll-up is a VIEW over this list and is deliberately a sibling of it
		// rather than a mode of it: the bucket shape is not the alert shape.
		r.Get("/rollups", rt.listAlertRollups)
		// ⛔ REGISTERED BEFORE /{id} FOR THE REASON /rollups IS, and it is a
		// SIBLING of `/{id}/unsnooze` rather than a mode of it: the subject is a
		// LIST of alerts the caller names in the body, so there is no id in the
		// path to address it with. It takes no filter, and never will — see
		// UnsnoozeAlertsRequest.
		//
		// ⛔ THERE IS NO `POST /alerts/snooze` BESIDE IT. Only the undo is bulk.
		r.Post("/unsnooze", rt.unsnoozeAlerts)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", rt.getAlert)
			r.Get("/cases", rt.listAlertCases)
			r.Get("/events", rt.listAlertEvents)
			r.Get("/enrichments", rt.listAlertEnrichments)
			r.Get("/notifications", rt.listAlertNotifications)
			r.Get("/snoozes", rt.listAlertSnoozes)
			// ⛔ THERE IS NO `/ack` HERE ANY MORE. A receipt is written on ONE
			// firing episode and is addressed by that episode's id — see
			// `/cases/{id}/ack` below. Snooze stays, because a snooze is a fact
			// about oto's notifications for the IDENTITY and outlives every one of
			// its episodes (§B.8, `alert_snoozes.alert_id`, migration 00048).
			r.Post("/comments", rt.commentOnAlert)
			r.Post("/snooze", rt.snoozeAlert)
			r.Post("/unsnooze", rt.unsnoozeAlert)
		})
	})

	// The ORG-WIDE snooze list (§B.8.6). It is a sibling of /alerts and not a
	// mode of it, for the same reason /alerts/rollups is: the row shape is a
	// SNOOZE, not an alert, and the keyset is over `snoozed_until` rather than
	// `last_seen_at`.
	r.Get("/snoozes", rt.listSnoozes)

	// The ORG-WIDE case list (§E.3b) — "what is firing that I need to
	// acknowledge". It is registered flat rather than as a `/cases` subrouter
	// because `rules/api` mounts `/cases/{id}/rule` on this same parent, and a
	// chi subrouter at `/cases` would take that prefix away from it.
	//
	// ⛔ It is a sibling of `/alerts` and NOT a mode of it, for the same reason
	// `/alerts/rollups` is: the row shape is a CASE, the keyset is over
	// `started_at`, and the ack facet only exists on this side.
	r.Get("/cases", rt.listCases)

	r.Route("/cases/{id}", func(r chi.Router) {
		r.Get("/", rt.getCase)
		r.Get("/events", rt.listCaseEvents)
		// ⭐ THE TWO HUMAN VERBS THAT ARE FACTS ABOUT AN EPISODE. `ack_state`,
		// `acked_by`, `acked_at` and `ack_note` are columns of `alert_cases` and
		// of nothing else (00049), so the route that writes them is addressed by
		// the case. `POST /alert-groups/{id}/ack` survives beside this one and is
		// not a duplicate: it is a FAN-OUT that resolves each member's open case
		// and acks each of those.
		r.Post("/ack", rt.ackCase)
		r.Post("/unack", rt.unackCase)
	})

	// ⭐⭐ THE CASE RETENTION WINDOW W, per (namespace, alertname) — migration
	// 00057's `case_policy_config`. It is CONFIGURATION about the Case, so it lives
	// on the router that owns the Case, and it is a sibling of `/cases` rather than a
	// sub-resource of it: the subject is a RULE that outlives every episode it shapes,
	// and there is no case id that could address it.
	//
	// The verb set is `/api/v1/clusters`'s, which is oto's existing per-org config
	// collection with an immutable natural key: list, create, patch by id, delete by
	// id. There is no `PUT` anywhere in this codebase and no upsert-by-natural-key on
	// a human-facing collection — a duplicate (namespace, alertname) is a `409` from
	// `case_policy_axes_uniq`, and `Idempotency-Key` is what makes the retry safe.
	r.Route("/case-policies", func(r chi.Router) {
		r.Get("/", rt.listCasePolicies)
		r.Post("/", rt.createCasePolicy)
		r.Patch("/{id}", rt.updateCasePolicy)
		r.Delete("/{id}", rt.deleteCasePolicy)
	})

	r.Get("/labels", rt.listLabelNames)
	r.Get("/labels/{name}/values", rt.listLabelValues)
}

// subject resolves the tenant and the path id, and rejects unknown query
// parameters, for every endpoint addressed by `{id}` that is NOT a human verb.
//
// ⭐ IT IS NOT `action`. `action` (helpers.go) additionally demands a HUMAN actor,
// because an acknowledgement nobody signed is not a receipt. Configuration is not
// attributed — a retention window is a rule, not a receipt — so it must not require
// one, or a PAT could never write it.
func (rt *Router) subject(r *http.Request) (db.TenantScope, uuid.UUID, error) {
	scope, err := scopeOf(r)
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

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

// now is the ONE clock reading a request makes. Every duration on the page is
// measured from it, so a list never disagrees with itself about "now".
//
// ⛔ It is the injected clock and never time.Now(): a handler that reads the wall
// clock directly is a handler no test can pin.
func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

// actorOf builds the timeline attribution for a human verb.
//
// ⛔ It refuses a non-human principal. Acknowledgement identity is stored because
// it is operationally necessary, and an ack attributed to "system" would be a
// receipt nobody signed.
func actorOf(ctx context.Context) (domain.Actor, error) {
	p, err := authn.Require(ctx)
	if err != nil {
		return domain.Actor{}, err
	}
	kind, err := domain.NewActorKind(p.ActorKind())
	if err != nil {
		return domain.Actor{}, errs.Forbidden("forbidden", "this action requires a human actor")
	}
	if !kind.IsHuman() {
		return domain.Actor{}, errs.Forbidden("forbidden", "this action requires a human actor")
	}
	return domain.NewActor(kind, p.ActorID(), p.ActorLabel())
}

// scopeOf resolves the caller's tenant, which is the only sanctioned path from a
// request to a db.TenantScope.
func scopeOf(r *http.Request) (db.TenantScope, error) {
	_, s, err := authn.Scope(r.Context())
	return s, err
}

// pageOf renders the keyset envelope.
func pageOf(c db.Cursor, limit int) httpx.Page { return httpx.PageOf(c, limit) }
