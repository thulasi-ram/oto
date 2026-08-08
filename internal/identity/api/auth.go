package api

import (
	"net/http"
	"time"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/ratelimit"
)

// loginBusyRetryAfter is the Retry-After sent when the concurrency gate is full.
// It is short because the gate clears as fast as the verifications finish.
const loginBusyRetryAfter = 2 * time.Second

// login is `POST /api/v1/auth/login` — operationId `login`.
//
// On success it sets the session cookie AND returns the current principal, so
// the UI needs no second round trip (contract: the 200 body is a MeResponse).
//
// EVERY failure is the same 401 with the same code and the same message. The
// service makes them cost the same wall-clock time as well; see
// service.Login's DummyVerify path.
//
// ⭐ IT IS RATE LIMITED AND CONCURRENCY GATED, and both are load-bearing for the
// same reason: `service.Login` runs argon2id at 19 MiB on EVERY path, including
// `DummyVerify` for an address that does not exist. That uniform cost is what
// makes the failure paths indistinguishable to a stopwatch, and it is also what
// makes this the most expensive unauthenticated endpoint in the process. Without
// a bound, anyone who can reach it can pin gigabytes of memory and every core
// with requests carrying no credential at all.
//
//	limiter  bounds the RATE per client address        → 429 + Retry-After
//	gate     bounds CONCURRENT verifications in flight  → 429 + Retry-After
//
// Both are checked BEFORE the body is bound, so a rejected caller never gets to
// allocate. A successful login clears the caller's bucket: a person who mistyped
// their password four times must not be punished for getting it right.
func (rt *Router) login(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	key := ratelimit.ClientKey(r)
	if rt.limiter != nil {
		if ok, retry := rt.limiter.Allow(key); !ok {
			// ⚠️ The message says nothing about accounts. A limiter that answered
			// differently for a known and an unknown address would reintroduce the
			// enumeration oracle the uniform 401 exists to close.
			httpx.WriteProblem(w, r, errs.RateLimited("too_many_login_attempts",
				"too many login attempts; try again shortly", retry))
			return
		}
	}
	if rt.gate != nil {
		if !rt.gate.Acquire() {
			httpx.WriteProblem(w, r, errs.RateLimited("login_busy",
				"too many login attempts; try again shortly", loginBusyRetryAfter))
			return
		}
		defer rt.gate.Release()
	}

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

	if rt.limiter != nil {
		rt.limiter.Reset(key)
	}

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
