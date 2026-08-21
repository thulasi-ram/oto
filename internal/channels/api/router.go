package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// TestTimeout bounds `testChannel`.
//
// ⛔ The test renders and SENDS, which means an outbound call to somebody else's
// API. A wedged Slack workspace must not be able to hold an HTTP worker for as
// long as it likes, so the handler runs under its own deadline derived from the
// request context — a client disconnect still cancels immediately, and a blown
// deadline is a `504` rather than a hang.
const TestTimeout = 15 * time.Second

// Options are the Router's dependencies. Everything is a port, so the whole
// surface is exercisable with fakes and an httptest.Server.
type Options struct {
	Registry    ProviderRegistry
	Channels    ChannelStore
	Connections ConnectionStore
	Creds       CredentialWriter
	// Writes owns `createChannel` and `testChannel`. It is a DIFFERENT
	// collaborator from Channels — the service rather than the repository —
	// because an `Idempotency-Key` claim has to join the transaction of the act it
	// guards, and the transaction boundary is not an HTTP concern. Nil is a
	// declared `503` on those two and nothing at all to the rest.
	Writes ChannelWriter
	// Resolver answers a Slack name↔id lookup. Nil is a declared `503` on that
	// one endpoint and nothing at all to the rest.
	Resolver     ConnectionResolver
	Interactions SlackInteractions
	// SigningSecret is Slack's HMAC signing secret for the HTTP interactivity
	// transport. Empty DISABLES the endpoint entirely rather than accepting
	// unverified requests: slack-go below v0.23.1 accepted an empty signing secret
	// and therefore forged requests, and that mistake is not repeated here.
	SigningSecret string
	Clock         clock.Clock
}

// Router serves the Channels tag and the Slack half of the Integrations tag.
type Router struct {
	registry     ProviderRegistry
	channels     ChannelStore
	connections  ConnectionStore
	creds        CredentialWriter
	writes       ChannelWriter
	resolver     ConnectionResolver
	interactions SlackInteractions
	signing      []byte
	clk          clock.Clock
}

// NewRouter builds the channels HTTP surface.
func NewRouter(o Options) *Router {
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &Router{
		registry:     o.Registry,
		channels:     o.Channels,
		connections:  o.Connections,
		creds:        o.Creds,
		writes:       o.Writes,
		resolver:     o.Resolver,
		interactions: o.Interactions,
		signing:      []byte(o.SigningSecret),
		clk:          clk,
	}
}

// Register mounts every route this package owns onto r, which `internal/app` has
// already rooted at /api/v1.
func (rt *Router) Register(r chi.Router) {
	r.Get("/channel-types", rt.listChannelTypes)

	r.Route("/channels", func(r chi.Router) {
		r.Get("/", rt.listChannels)
		r.Post("/", rt.createChannel)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", rt.getChannel)
			r.Patch("/", rt.updateChannel)
			r.Delete("/", rt.deleteChannel)
			r.Post("/test", rt.testChannel)
		})
	})

	r.Route("/channel-connections", func(r chi.Router) {
		r.Get("/", rt.listConnections)
		r.Post("/", rt.createConnection)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", rt.getConnection)
			r.Patch("/", rt.updateConnection)
			r.Delete("/", rt.deleteConnection)
			r.Post("/slack/resolve", rt.resolveSlackConversation)
		})
	})
}

// RegisterIntegrations mounts the inbound provider callbacks.
//
// It is SEPARATE from Register because these routes have a different security
// model: they authenticate by Slack HMAC signature over the raw body, not by
// session or PAT, so `internal/app` mounts them outside the authenticated group.
// Folding them into Register would either wrap them in an auth middleware that
// Slack cannot satisfy, or punch a hole in the authenticated group.
func (rt *Router) RegisterIntegrations(r chi.Router) {
	r.Post("/integrations/slack/interactions", rt.receiveSlackInteraction)
}

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

// now is the ONE clock reading a request makes.
//
// ⛔ It is the injected clock and never time.Now(): a handler that reads the wall
// clock directly is a handler no test can pin — and this one gates a replay
// window, so an untestable clock here is a security property nobody can prove.
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
// write facade acts on (see idempotency.IntentFromRequest for the seam's rules).
// The hash is passed in because what "the same request" means differs by
// operation: a create is identified by the RAW bytes it sent, and a test — which
// has no body — by the channel it would send to.
func idempotencyIntent(r *http.Request, hash idempotency.RequestHash) (service.Idempotency, error) {
	return idempotency.IntentFromRequest(r, hash)
}

// requireDependency turns a missing collaborator into an honest 503 rather than a
// nil-pointer panic. A deployment wired without a tester is a misconfiguration,
// not a caller error.
func requireDependency(present bool, code, what string) error {
	if present {
		return nil
	}
	return errs.Unavailable(code, what, 0)
}
