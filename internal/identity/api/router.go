package api

import (
	"context"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// IdentityService is the port THIS LAYER declares for itself (CONTEXT.md §5.4).
// It is satisfied by *service.Service, which is injected by
// `internal/app/container.go`; nothing here names a repository, and the row
// models are unexported in a package this one cannot import anyway.
type IdentityService interface {
	Login(ctx context.Context, cmd service.LoginCommand) (service.LoginResult, error)
	ExpireSession(ctx context.Context, scope db.TenantScope, p authn.Principal) error
	Me(ctx context.Context, scope db.TenantScope, p authn.Principal) (service.MeView, error)

	ListTokens(ctx context.Context, scope db.TenantScope, p authn.Principal, k db.Keyset) ([]domain.APIToken, db.Cursor, error)
	IssueToken(ctx context.Context, scope db.TenantScope, p authn.Principal, cmd service.CreateTokenCommand) (service.IssuedToken, error)
	RevokeToken(ctx context.Context, scope db.TenantScope, tokenID uuid.UUID) error
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ IdentityService = (*service.Service)(nil)

// Router mounts the Identity surface of SPEC §E.2.
//
// ⛔ THE INGEST ENDPOINT IS NOT HERE AND MUST NOT BE. `POST
// /api/v1/ingest/alertmanager/{source_id}` carries its own per-source token
// auth, on its own pool, mounted by `ingestion/api` outside every group below.
// A session cookie must never be able to post alerts.
type Router struct {
	svc    IdentityService
	auth   *authn.Middleware
	cookie CookieConfig
	clk    clock.Clock
}

// NewRouter builds the identity router.
//
// The middleware is passed IN rather than constructed here because
// `internal/app` mounts the same instance in front of every other module's
// routes: there is one authenticator in the process, and a second one would be a
// second place a credential rule could be written differently.
func NewRouter(svc IdentityService, auth *authn.Middleware, cookie CookieConfig, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, auth: auth, cookie: cookie.normalise(), clk: clk}
}

// Middleware is the authenticator this router was built with, so
// `internal/app` can mount the same instance in front of the other domains.
func (rt *Router) Middleware() *authn.Middleware { return rt.auth }

// Mount registers the Identity routes on r, which `internal/app` has already
// scoped to `/api/v1`.
//
// ⭐ THE THREE SECURITY GROUPS ARE THE CONTRACT'S, VERBATIM:
//
//	login                 security: []              — unauthenticated
//	getCurrentPrincipal   session | pat
//	logout, api-tokens*   sessionCookie ONLY
//
// The session-only group is v1's single privilege boundary, and it is a boundary
// between CREDENTIAL KINDS rather than between roles: a token must not be able
// to enumerate, mint or revoke its own siblings. There is still no RBAC — a
// member of an org can do anything within that org (R2).
func (rt *Router) Mount(r chi.Router) {
	// Unauthenticated. Login is how a credential is obtained, so it cannot be
	// behind one.
	r.Post("/auth/login", rt.login)

	// session | pat.
	r.Group(func(g chi.Router) {
		g.Use(rt.auth.Require)
		g.Get("/me", rt.getCurrentPrincipal)
	})

	// sessionCookie only.
	r.Group(func(g chi.Router) {
		g.Use(rt.auth.RequireSession)
		g.Post("/auth/logout", rt.logout)
		g.Get("/api-tokens", rt.listAPITokens)
		g.Post("/api-tokens", rt.createAPIToken)
		g.Delete("/api-tokens/{id}", rt.revokeAPIToken)
	})
}

// Register is Mount as a free function, for a caller that would rather write
// `identityapi.Register(r, rt)`. It is the same hook; there is only one route
// table.
func Register(r chi.Router, rt *Router) { rt.Mount(r) }
