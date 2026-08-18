package api

import (
	"context"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// IdentityService is the port THIS LAYER declares for itself (CONTEXT.md §5.4).
// It is satisfied by *service.Service, which is injected by
// `internal/app/container.go`; nothing here names a repository, and the row
// models are unexported in a package this one cannot import anyway.
type IdentityService interface {
	Login(ctx context.Context, cmd service.LoginCommand) (service.LoginResult, error)
	ExpireSession(ctx context.Context, scope db.TenantScope, p authn.Principal) error
	Me(ctx context.Context, scope db.TenantScope, p authn.Principal) (service.MeView, error)

	GetOrg(ctx context.Context, scope db.TenantScope) (domain.Org, error)
	UpdateOrgSettings(ctx context.Context, scope db.TenantScope, p domain.SettingsPatch, reset []domain.SettingKey) (domain.Org, error)

	ListTokens(ctx context.Context, scope db.TenantScope, p authn.Principal, k db.Keyset) ([]domain.APIToken, db.Cursor, error)
	IssueToken(ctx context.Context, scope db.TenantScope, p authn.Principal, cmd service.CreateTokenCommand) (service.IssuedToken, error)
	RevokeToken(ctx context.Context, scope db.TenantScope, tokenID uuid.UUID) error
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ IdentityService = (*service.Service)(nil)

// LoginLimiter is the port this layer declares for login rate limiting,
// satisfied by `*platform/ratelimit.Limiter`.
//
// It reports whether the caller may attempt a login, and how long until it may
// try again. `Reset` is called after a SUCCESSFUL login so a person who mistyped
// their password four times does not pay for it on the fifth attempt that works.
type LoginLimiter interface {
	Allow(key string) (bool, time.Duration)
	Reset(key string)
}

// UnitOfWork runs fn inside ONE database transaction, satisfied by
// `*identity/repository.TxRunner`.
//
// ⭐⭐ IT IS WHAT MAKES A MINT AND ITS IDEMPOTENCY CLAIM ONE FACT. This path had
// no transaction at all until the claim needed one: `createApiToken` inserted a
// credential and returned its plaintext in a single unguarded commit. A claim
// that lost the race must roll the mint back with it, or a second live token
// exists that nobody knows the secret of and nobody knows to revoke.
type UnitOfWork interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// IdempotencyClaims takes a client-supplied `Idempotency-Key` for one operation,
// satisfied by `*platform/idempotency.Repository`.
//
// It is a PORT THIS LAYER DECLARES (CONTEXT.md §5.4) even though the store is a
// platform type: the transport is what reads the header, so the transport is what
// owns the dependency.
type IdempotencyClaims interface {
	Claim(ctx context.Context, s db.TenantScope, c idempotency.Claim) (idempotency.Result, error)
}

// LoginGate bounds how many password verifications run CONCURRENTLY, satisfied
// by `*platform/ratelimit.Gate`. It is a separate port from LoginLimiter because
// it answers a different question: the limiter bounds the rate, the gate bounds
// the resident memory of everything currently in flight.
type LoginGate interface {
	Acquire() bool
	Release()
}

// Router mounts the Identity surface of SPEC §E.2.
//
// ⛔ THE INGEST ENDPOINT IS NOT HERE AND MUST NOT BE. `POST
// /api/v1/ingest/alertmanager/{source_id}` carries its own per-source token
// auth, on its own pool, mounted by `ingestion/api` outside every group below.
// A session cookie must never be able to post alerts.
type Router struct {
	svc     IdentityService
	auth    *authn.Middleware
	cookie  CookieConfig
	limiter LoginLimiter
	gate    LoginGate
	tx      UnitOfWork
	claims  IdempotencyClaims
	clk     clock.Clock
}

// Options are the identity router's dependencies.
type Options struct {
	Service IdentityService
	// Auth is passed IN rather than constructed here because `internal/app`
	// mounts the same instance in front of every other module's routes: there is
	// one authenticator in the process, and a second one would be a second place a
	// credential rule could be written differently.
	Auth   *authn.Middleware
	Cookie CookieConfig
	// Limiter bounds the login RATE. Nil means unlimited, which is what shipped
	// and is a documented gap rather than a choice; production always wires it.
	Limiter LoginLimiter
	// Gate bounds concurrent password verifications, which is what actually
	// bounds argon2id's 19 MiB-per-evaluation memory cost.
	Gate LoginGate
	// Tx makes a minted credential and the idempotency claim that guards it one
	// commit. A nil Tx degrades to no transaction, which is what this path used to
	// do; production always wires it.
	Tx UnitOfWork
	// Claims is the store behind `Idempotency-Key`. Nil means this deployment
	// cannot honour the header, which is a declared `503` rather than a silently
	// unprotected create — the whole defect was a header the contract promised and
	// the server ignored, and ignoring it quietly a second time is the same bug.
	Claims IdempotencyClaims
	Clock  clock.Clock
}

// NewRouter builds the identity router.
func NewRouter(o Options) *Router {
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &Router{
		svc:     o.Service,
		auth:    o.Auth,
		cookie:  o.Cookie.normalise(),
		limiter: o.Limiter,
		gate:    o.Gate,
		tx:      o.Tx,
		claims:  o.Claims,
		clk:     clk,
	}
}

// inTx runs fn in one transaction when a unit of work is wired, and inline
// otherwise.
func (rt *Router) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if rt.tx == nil {
		return fn(ctx)
	}
	return rt.tx.InTx(ctx, fn)
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
		// Reading the tuning is a read: a token that can list alerts can see the
		// numbers that decided how loudly they were announced.
		g.Get("/org/settings", rt.getOrgSettings)
	})

	// sessionCookie only.
	r.Group(func(g chi.Router) {
		g.Use(rt.auth.RequireSession)
		g.Post("/auth/logout", rt.logout)
		// ⚠️ WRITING THE TUNING IS SESSION-ONLY. These numbers decide how much
		// oto says; a leaked ingest-adjacent token must not be able to widen
		// `default_verbosity` or lengthen `resolve_grace_s` and change what an
		// operator hears. It joins the same privilege boundary as minting
		// credentials. (The example used to be `storm_threshold`, back when oto
		// still withheld notifications on its own judgement; it no longer does.)
		g.Patch("/org/settings", rt.updateOrgSettings)
		g.Get("/api-tokens", rt.listAPITokens)
		g.Post("/api-tokens", rt.createAPIToken)
		g.Delete("/api-tokens/{id}", rt.revokeAPIToken)
	})
}

// Register is Mount as a free function, for a caller that would rather write
// `identityapi.Register(r, rt)`. It is the same hook; there is only one route
// table.
func Register(r chi.Router, rt *Router) { rt.Mount(r) }
