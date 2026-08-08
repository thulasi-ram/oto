package api

import (
	"net/http"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// login is `POST /api/v1/auth/login` — operationId `login`.
//
// On success it sets the session cookie AND returns the current principal, so
// the UI needs no second round trip (contract: the 200 body is a MeResponse).
//
// EVERY failure is the same 401 with the same code and the same message. The
// service makes them cost the same wall-clock time as well; see
// service.Login's DummyVerify path.
//
// ⚠️ RATE LIMITING IS NOT APPLIED HERE YET. The contract documents a 429 for
// repeated failures and `internal/platform/ratelimit` is currently an empty
// package, so this endpoint is protected by uniform timing and an unspecific
// 401 and by nothing else. This is a KNOWN GAP, not an oversight: the limiter
// belongs in front of the handler as middleware, and adding a bespoke one here
// would put oto's second rate-limiting implementation in its least reviewed
// place.
func (rt *Router) login(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	req, err := httpx.Bind[LoginRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	res, err := rt.svc.Login(r.Context(), req.toDomain(r.UserAgent()))
	if err != nil {
		// The email is deliberately absent from this path's logs. A record of
		// every failed attempt, with its address, is a list of addresses worth
		// attacking — the enumeration the uniform 401 exists to prevent.
		httpx.WriteProblem(w, r, err)
		return
	}

	scope, err := res.Principal.Scope()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	view, err := rt.svc.Me(r.Context(), scope, res.Principal)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// The cookie is written only after everything that could fail has succeeded.
	// A Set-Cookie followed by a 500 hands out a working credential with a
	// response that says the login did not happen.
	rt.cookie.setSession(w, res.Session.Secret, res.Session.Session.ExpiresAt, rt.clk.Now())

	httpx.Data(w, r, http.StatusOK, toMeDTO(view), started)
}

// logout is `POST /api/v1/auth/logout` — operationId `logout`.
//
// Idempotent, and revocation happens SERVER-SIDE FIRST. The cookie is cleared
// afterwards as a courtesy; a client that ignores the header still holds a
// credential that resolves to nothing.
func (rt *Router) logout(w http.ResponseWriter, r *http.Request) {
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if err := rt.svc.ExpireSession(r.Context(), scope, p); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rt.cookie.clearSession(w)
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

// getCurrentPrincipal is `GET /api/v1/me` — operationId `getCurrentPrincipal`.
//
// Who am I, which org am I in, and how is that org tuned. The settings block is
// what lets the UI explain its own damping decisions rather than presenting them
// as inexplicable silence.
func (rt *Router) getCurrentPrincipal(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	// authn.Scope re-derives the principal from the context rather than trusting
	// that the middleware ran. "The middleware guarantees it" is exactly the
	// assumption that stops being true the first time a route is mounted
	// somewhere else.
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	view, err := rt.svc.Me(r.Context(), scope, p)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusOK, toMeDTO(view), started)
}
