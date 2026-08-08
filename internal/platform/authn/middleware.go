package authn

import (
	"context"
	"net/http"
	"strings"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// The credential prefixes this middleware will and will not accept.
//
// They are declared here, in transport terms, because the middleware must refuse
// an `oto_ingest_…` BEFORE any lookup happens. An ingest token is scoped to one
// AlertSource and can read nothing (SPEC §G.2); letting one reach the API's
// resolver would make that narrowness depend on a database predicate instead of
// on a string comparison that cannot fail.
const (
	// BearerPrefixPAT is the only bearer credential the API accepts.
	BearerPrefixPAT = "oto_pat_"
	// BearerPrefixIngest is rejected here, always, on every API route.
	BearerPrefixIngest = "oto_ingest_"
)

// DefaultSessionCookie is the cookie name the contract fixes (`sessionCookie`
// security scheme). It is injected rather than read from a global so that a test
// server and the process can differ.
const DefaultSessionCookie = "oto_session"

// Resolver turns a presented credential into a Principal.
//
// ⭐ THIS PORT IS WHY THE MIDDLEWARE LIVES IN `platform/authn` AND NOT IN
// `identity/api`. depguard forbids `internal/platform/**` from importing any
// domain, so the middleware cannot name `identity/service` — and it does not
// need to. It names this interface, `identity/service` satisfies it, and
// `internal/app` is the one place the two meet. The alternative — putting the
// middleware in `identity/api` — would make every other module's route group
// import `internal/identity`, which the per-domain depguard blocks deny.
//
// Both methods take the PLAINTEXT secret, because hashing is the resolver's
// choice: the middleware must not encode a decision about sha256 versus
// anything else. The secret goes no further than the resolver, and it is never
// placed in the context, in a Principal, or in an error.
type Resolver interface {
	// ResolveBearer resolves an `Authorization: Bearer oto_pat_…` credential.
	ResolveBearer(ctx context.Context, secret string) (Principal, error)
	// ResolveSession resolves the value of the session cookie.
	ResolveSession(ctx context.Context, secret string) (Principal, error)
}

// Middleware resolves a request to a Principal and a db.TenantScope.
//
// ⛔ THE INGEST ENDPOINT MUST NOT BE MOUNTED BEHIND THIS. `POST
// /api/v1/ingest/alertmanager/{source_id}` authenticates with its own per-source
// token through `ingestion/api.Authenticator`, on its own pool, with its own
// cache and its own 401. Routing it through here would let a session cookie post
// alerts, and would put a second, slower lookup on the one path in oto with a
// latency budget an upstream enforces (SPEC §G.1).
type Middleware struct {
	resolver Resolver
	cookie   string
}

// NewMiddleware builds the middleware. An empty cookie name falls back to the
// contract's `oto_session`.
func NewMiddleware(resolver Resolver, cookieName string) *Middleware {
	if cookieName == "" {
		cookieName = DefaultSessionCookie
	}
	return &Middleware{resolver: resolver, cookie: cookieName}
}

// CookieName is the session cookie this middleware reads.
func (m *Middleware) CookieName() string { return m.cookie }

// Require authenticates a bearer PAT or a session cookie and rejects everything
// else with problem+json.
//
// Order matters: the Authorization header wins over the cookie. A browser that
// holds a stale cookie while a CLI sets an explicit header should act as the
// header says, and silently preferring the ambient credential is how a caller
// ends up authenticated as somebody they did not intend.
func (m *Middleware) Require(next http.Handler) http.Handler {
	return m.handler(next, false)
}

// RequireSession authenticates a session cookie ONLY.
//
// The contract puts `/api/v1/api-tokens*` and `/api/v1/auth/logout` behind
// `sessionCookie` alone: a token must not be able to enumerate, mint or revoke
// its own siblings. That is the one privilege boundary v1 has, and it is a
// boundary between CREDENTIAL KINDS, not between roles — there is still no RBAC
// (R2).
func (m *Middleware) RequireSession(next http.Handler) http.Handler {
	return m.handler(next, true)
}

func (m *Middleware) handler(next http.Handler, sessionOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := m.authenticate(r, sessionOnly)
		if err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}

		scope, err := p.Scope()
		if err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}

		ctx := WithScope(Into(r.Context(), p), scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authenticate resolves the request's credential.
//
// EVERY failure returns the SAME error: one code, one message, no hint about
// which check said no. A caller that can distinguish "no credential" from "known
// token, wrong kind" from "expired session" is a caller probing the credential
// store, and none of those distinctions helps a legitimate client do anything
// differently — it retries by logging in again in all three cases.
func (m *Middleware) authenticate(r *http.Request, sessionOnly bool) (Principal, error) {
	ctx := r.Context()

	if secret, ok := bearerSecret(r); ok {
		if sessionOnly || !strings.HasPrefix(secret, BearerPrefixPAT) {
			// A PAT on a session-only route, or an ingest token anywhere on the
			// API: refused without a lookup.
			return Principal{}, Unauthenticated()
		}
		p, err := m.resolver.ResolveBearer(ctx, secret)
		if err != nil || !p.Valid() {
			return Principal{}, Unauthenticated()
		}
		return p, nil
	}

	c, err := r.Cookie(m.cookie)
	if err != nil || c.Value == "" {
		return Principal{}, Unauthenticated()
	}
	p, err := m.resolver.ResolveSession(ctx, c.Value)
	if err != nil || !p.Valid() {
		return Principal{}, Unauthenticated()
	}
	return p, nil
}

// Unauthenticated is the ONE error every authentication failure in oto returns.
func Unauthenticated() error {
	return errs.Unauthorized("unauthenticated", "authentication required")
}

// bearerSecret extracts `Authorization: Bearer <secret>`.
func bearerSecret(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		// A non-bearer Authorization header is a PRESENT credential we cannot
		// read. Reporting it as absent would fall through to the cookie and
		// authenticate the request as somebody the header did not name.
		return "", true
	}
	secret := strings.TrimSpace(h[len(prefix):])
	return secret, true
}

type scopeKey struct{}

// WithScope returns a context carrying the tenant scope resolved for a request.
//
// The scope is derivable from the Principal, and `Scope(ctx)` still derives it.
// Caching it here means the per-request derivation happens once, in the one
// place that has already checked the principal, rather than in every handler.
func WithScope(ctx context.Context, s db.TenantScope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom returns the tenant scope the middleware resolved, if there is one.
func ScopeFrom(ctx context.Context) (db.TenantScope, bool) {
	s, ok := ctx.Value(scopeKey{}).(db.TenantScope)
	return s, ok && s.Valid()
}
