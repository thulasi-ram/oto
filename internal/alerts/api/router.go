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
	Occurrences(ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset) (service.OccurrenceResult, error)
	GetOccurrence(ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID) (domain.Occurrence, error)
	AlertTimeline(ctx context.Context, s db.TenantScope, alertID uuid.UUID, w db.TimeWindow, p db.Keyset) (service.TimelineResult, error)
	OccurrenceTimeline(ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID, w db.TimeWindow, p db.Keyset) (service.TimelineResult, error)
	Enrichments(ctx context.Context, s db.TenantScope, alertID uuid.UUID) ([]service.EnrichmentSummary, error)
	Notifications(ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset) (service.NotificationResult, error)
	// The two delivery roll-ups behind `delivery_summary` on the alert and
	// occurrence detail views. They are on this interface — rather than being
	// derived from `Notifications` in the handler — because the roll-up covers
	// the GROUP generations an alert has been part of as well as the intents that
	// name it, and paging a list to add up its rows would be both wrong and N+1.
	DeliveryRollupForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (service.DeliveryRollup, error)
	DeliveryRollupForOccurrence(ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID) (service.DeliveryRollup, error)
	LabelNames(ctx context.Context, s db.TenantScope, prefix string, limit int) ([]domain.LabelCount, error)
	LabelValues(ctx context.Context, s db.TenantScope, name, prefix string, limit int) ([]domain.LabelCount, error)
	Acknowledge(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, note string) (domain.Occurrence, error)
	Unacknowledge(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, note string) (domain.Occurrence, error)
	Comment(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, body string) (domain.Event, error)

	// ⛔ THE THIRD HUMAN VERB (§E.1.1, §B.8). Snooze writes NOTIFICATION state,
	// never signal state: the alert stays firing, stays whatever severity it was,
	// and every surface keeps rendering it that way. There is still no Resolve,
	// no Close and no Dismiss on this interface, and there never will be.
	Snooze(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, until time.Time, note string) (domain.Snooze, error)
	Unsnooze(ctx context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, note string) (domain.Snooze, error)
	SnoozeHistory(ctx context.Context, s db.TenantScope, alertID uuid.UUID, limit int) ([]domain.Snooze, error)
	// ActiveSnoozes is the ORG-WIDE §B.8.6 view: everything oto is currently
	// quiet about, soonest wake-up first. It is what the persistent banner is
	// built from, and it is the reason a snooze cannot be forgotten.
	ActiveSnoozes(ctx context.Context, s db.TenantScope, p db.Keyset) (service.ActiveSnoozeResult, error)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ AlertService = (*service.Service)(nil)

// Router serves the Alerts, Occurrences and Discovery tags.
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
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", rt.getAlert)
			r.Get("/occurrences", rt.listAlertOccurrences)
			r.Get("/events", rt.listAlertEvents)
			r.Get("/enrichments", rt.listAlertEnrichments)
			r.Get("/notifications", rt.listAlertNotifications)
			r.Get("/snoozes", rt.listAlertSnoozes)
			r.Post("/ack", rt.ackAlert)
			r.Post("/unack", rt.unackAlert)
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

	r.Route("/occurrences/{id}", func(r chi.Router) {
		r.Get("/", rt.getOccurrence)
		r.Get("/events", rt.listOccurrenceEvents)
	})

	r.Get("/labels", rt.listLabelNames)
	r.Get("/labels/{name}/values", rt.listLabelValues)
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
