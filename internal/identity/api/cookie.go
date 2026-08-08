package api

import (
	"net/http"
	"time"
)

// CookieConfig describes the session cookie this module sets.
//
// ⚠️ THERE IS NO `Secure: false` KNOB, deliberately. The contract fixes the
// cookie as `HttpOnly; Secure; SameSite=Lax; Path=/`, and a configurable Secure
// flag is a flag somebody turns off in dev, copies into a Helm chart, and ships.
// Local development is unaffected: browsers treat `http://localhost` as a secure
// context, so a Secure cookie is set and sent there. A deployment serving oto
// over plain HTTP on a non-loopback host has a bigger problem than its cookie.
//
// Deriving Secure from `r.TLS` would be worse still: oto behind a
// TLS-terminating reverse proxy — the normal deployment — sees plain HTTP and
// would ship an insecure cookie on every production install.
type CookieConfig struct {
	// Name is the cookie the contract fixes: `oto_session`.
	Name string
	// Path scopes the cookie. `/` per the contract.
	Path string
	// Domain is optional and normally empty, which scopes the cookie to the exact
	// host that set it. Widening it shares the session with every subdomain.
	Domain string
}

// DefaultCookieConfig is the contract's `oto_session=…; HttpOnly; Secure;
// SameSite=Lax; Path=/`.
func DefaultCookieConfig(name string) CookieConfig {
	if name == "" {
		name = "oto_session"
	}
	return CookieConfig{Name: name, Path: "/"}
}

func (c CookieConfig) normalise() CookieConfig {
	if c.Name == "" {
		c.Name = "oto_session"
	}
	if c.Path == "" {
		c.Path = "/"
	}
	return c
}

// setSession writes the Set-Cookie header for a freshly minted session.
//
// ⭐ THE FLAGS, and why each one is not negotiable:
//
//   - HttpOnly — JavaScript cannot read the value, so one XSS does not become a
//     stolen session that outlives the page.
//   - Secure — the value never travels in clear text.
//   - SameSite=Lax — a cross-site POST does not carry the cookie, which is what
//     stops a CSRF from acking, snoozing or minting a token. Lax rather than
//     Strict because Strict would drop the cookie on a normal top-level
//     navigation from a Slack link, which is how most people reach oto.
//   - Expires AND MaxAge — set to the SERVER's expiry so the browser stops
//     sending a cookie the server has already stopped accepting. It is a
//     courtesy, never the enforcement: expiry is a column, checked server-side
//     on every request (§L: the client is not a trust boundary).
func (c CookieConfig) setSession(w http.ResponseWriter, secret string, expiresAt time.Time, now time.Time) {
	c = c.normalise()
	maxAge := int(expiresAt.Sub(now).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     c.Name,
		Value:    secret,
		Path:     c.Path,
		Domain:   c.Domain,
		Expires:  expiresAt.UTC(),
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSession expires the cookie in the browser.
//
// It is a COURTESY, not the logout. The session row is revoked server-side
// first; a client that ignores this header still cannot use the cookie, because
// the resolver will not find a live row for it. Doing it the other way round —
// clearing the cookie and trusting the browser to forget — is a logout that
// leaves a working credential in whatever intercepted it.
func (c CookieConfig) clearSession(w http.ResponseWriter) {
	c = c.normalise()
	http.SetCookie(w, &http.Cookie{
		Name:     c.Name,
		Value:    "",
		Path:     c.Path,
		Domain:   c.Domain,
		Expires:  time.Unix(0, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
