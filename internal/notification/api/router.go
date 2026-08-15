package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// Options are the Router's dependencies. Everything is a port, so the whole
// surface is exercisable with fakes and an httptest.Server.
type Options struct {
	Policies PolicyStore
	// PolicyWrites registers a policy. It is a DIFFERENT collaborator from
	// Policies — the service rather than the repository — because an
	// `Idempotency-Key` claim has to join the insert's transaction, and the
	// transaction boundary is not an HTTP concern. Nil is a declared `503` on
	// `createNotificationPolicy` and nothing at all to the reads.
	PolicyWrites  PolicyCreator
	Audit         AuditStore
	Notifications NotificationReader
	Deliveries    DeliveryReader

	Preview   Previewer
	Views     ViewBuilder
	Renderers RendererSource
	Subjects  SubjectResolver
	Enqueuer  Requeuer

	Clock clock.Clock
	// BaseURL is oto's public root, handed to the renderer so a previewed card's
	// deep links point somewhere real.
	BaseURL string
}

// Router serves the Notification tag: policies, the dry-run preview, the
// notification intents and their deliveries.
type Router struct {
	policies      PolicyStore
	policyWrites  PolicyCreator
	audit         AuditStore
	notifications NotificationReader
	deliveries    DeliveryReader

	preview   Previewer
	views     ViewBuilder
	renderers RendererSource
	subjects  SubjectResolver
	enqueuer  Requeuer

	clk     clock.Clock
	baseURL string
}

// NewRouter builds the notification HTTP surface.
func NewRouter(o Options) *Router {
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &Router{
		policies:      o.Policies,
		policyWrites:  o.PolicyWrites,
		audit:         o.Audit,
		notifications: o.Notifications,
		deliveries:    o.Deliveries,
		preview:       o.Preview,
		views:         o.Views,
		renderers:     o.Renderers,
		subjects:      o.Subjects,
		enqueuer:      o.Enqueuer,
		clk:           clk,
		baseURL:       o.BaseURL,
	}
}

// Register mounts every route this package owns onto r, which `internal/app` has
// already rooted at /api/v1.
func (rt *Router) Register(r chi.Router) {
	r.Route("/notification-policies", func(r chi.Router) {
		r.Get("/", rt.listNotificationPolicies)
		r.Post("/", rt.createNotificationPolicy)
		// `preview` is registered BEFORE `{id}` so that the literal segment wins.
		// chi resolves static patterns ahead of parameters, but writing them in
		// this order makes the intent readable rather than implied.
		r.Post("/preview", rt.previewNotificationPolicy)
		r.Patch("/{id}", rt.updateNotificationPolicy)
		r.Delete("/{id}", rt.deleteNotificationPolicy)
	})

	r.Route("/notifications", func(r chi.Router) {
		r.Get("/", rt.listNotifications)
		r.Get("/{id}", rt.getNotification)
	})

	r.Route("/deliveries", func(r chi.Router) {
		r.Get("/", rt.listDeliveries)
		r.Get("/{id}", rt.getDelivery)
		r.Post("/{id}/retry", rt.retryDelivery)
	})
}

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

// now is the ONE clock reading a request makes.
//
// ⛔ It is the injected clock and never time.Now(): a handler that reads the wall
// clock directly is a handler no test can pin.
func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

// scopeOf resolves the caller's tenant, which is the only sanctioned path from a
// request to a db.TenantScope.
func scopeOf(r *http.Request) (db.TenantScope, error) {
	_, s, err := authn.Scope(r.Context())
	return s, err
}

// subject resolves the tenant and the path id, and rejects unknown query
// parameters, for every endpoint addressed by `{id}`.
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

// idempotencyIntent reads the caller's `Idempotency-Key` into the intent the
// write facade acts on (see idempotency.IntentFromRequest for the seam's rules —
// the claiming transaction belongs to `notification/service`).
func idempotencyIntent(r *http.Request, hash idempotency.RequestHash) (service.Idempotency, error) {
	return idempotency.IntentFromRequest(r, hash)
}

// requireDependency turns a missing collaborator into an honest 503 rather than a
// nil-pointer panic.
func requireDependency(present bool, code, what string) error {
	if present {
		return nil
	}
	return errs.Unavailable(code, what, 0)
}
