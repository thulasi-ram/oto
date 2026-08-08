package app

import (
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/api/openapi"
	"github.com/thulasiram/oto/internal/platform/httpx"
	mw "github.com/thulasiram/oto/internal/platform/httpx/middleware"
)

// Router assembles the WHOLE HTTP surface: the ops endpoints, the published
// contract, and every domain's routes under /api/v1.
//
// ⭐ THE GROUPING IS THE SECURITY MODEL, and it is why this is one function
// rather than a line per module. Three routes must NOT sit where the rest do,
// and each has been a production incident somewhere:
//
//  1. GET /api/v1/stream is mounted OUTSIDE the request timeout. The handler is
//     meant to live for hours; a timeout cancels its context, and the failure is
//     not a broken page but a reconnect storm at exactly the timeout interval.
//
//  2. POST /api/v1/ingest/alertmanager/{source_id} is mounted OUTSIDE the UI's
//     authenticator, outside the global body cap and outside any rate limiter.
//     It carries its own per-source token, its own 8 MiB bound (B1) and its own
//     backpressure; a session cookie must never be able to post alerts, and
//     ⛔ A RATE LIMITER HERE WOULD ANSWER 429, WHICH MAKES ALERTMANAGER DELETE
//     THE NOTIFICATION PERMANENTLY (§G.2, C4). Shedding is 503 + Retry-After,
//     decided by the module's own Shedder, and nowhere else.
//
//  3. POST /api/v1/integrations/slack/interactions verifies Slack's HMAC over
//     the RAW body. No middleware may read, buffer or rewrite it, and it cannot
//     be behind the session authenticator because Slack has no session.
func (c *Container) Router() http.Handler {
	r := chi.NewRouter()

	// The order is load-bearing. RequestID first so every later record carries
	// one; Logger next so it sees the final status; Recover inside them both so a
	// panic is still logged with its request id; CORS before anything that can
	// write a body, so an error response still carries the headers a browser
	// needs to read it.
	r.Use(mw.RequestID)
	r.Use(mw.Logger(c.Logger))
	r.Use(mw.Recover)
	r.Use(mw.CORS(c.Config.HTTP))

	c.mountOps(r)

	// ONE sub-router for /api/v1. chi refuses to Route() the same pattern twice,
	// and the grouping below is what actually separates the security models: each
	// chi.Group gets its own middleware chain over the same tree.
	v1 := chi.NewRouter()

	// --- the ingest surface -----------------------------------------------
	// NO middleware at all. See (2) above.
	if c.routers.ingestion != nil {
		c.routers.ingestion.Router.Mount(v1)
	}

	// --- the Slack callback ------------------------------------------------
	// Also bare: the handler verifies an HMAC over the raw body. See (3) above.
	if c.routers.channels != nil {
		c.routers.channels.RegisterIntegrations(v1)
	}

	// --- the SSE stream -----------------------------------------------------
	// Authenticated, but OUTSIDE the timeout. See (1) above.
	if c.routers.streaming != nil {
		v1.Group(func(g chi.Router) {
			g.Use(mw.MaxBody(c.Config.HTTP.MaxBodyBytes))
			g.Use(c.Auth.Require)
			c.routers.streaming.Mount(g)
		})
	}

	// --- identity -----------------------------------------------------------
	// It owns its own three security groups — unauthenticated login,
	// session-or-PAT `/me`, and session-only token management — so it mounts
	// itself rather than being wrapped in one of ours.
	if c.routers.identity != nil {
		v1.Group(func(g chi.Router) {
			g.Use(mw.MaxBody(c.Config.HTTP.MaxBodyBytes))
			g.Use(mw.Timeout(c.Config.HTTP.RequestTimeout))
			c.routers.identity.Mount(g)
		})
	}

	// --- the versioned UI API ----------------------------------------------
	// Session-or-PAT. There is no RBAC in v1: a member of an org may do anything
	// within that org (R2).
	v1.Group(func(g chi.Router) {
		g.Use(mw.MaxBody(c.Config.HTTP.MaxBodyBytes))
		g.Use(mw.Timeout(c.Config.HTTP.RequestTimeout))
		g.Use(c.Auth.Require)
		c.mountDomains(g)
	})

	// GET /api/v1/version is in the contract and belongs to no domain, so it is
	// answered here rather than given a module of its own.
	v1.Get("/version", func(w http.ResponseWriter, req *http.Request) {
		httpx.JSON(w, req, http.StatusOK, map[string]any{
			"version": c.Config.Version,
			"service": c.Config.Service,
			"env":     c.Config.Env,
		})
	})

	r.Mount("/api/v1", v1)

	return r
}

// mountDomains is one line per module, which is the whole point of the seam.
func (c *Container) mountDomains(g chi.Router) {
	if c.routers.alerts != nil {
		c.routers.alerts.Mount(g)
	}
	if c.routers.grouping != nil {
		c.routers.grouping.Mount(g)
	}
	if c.routers.rules != nil {
		c.routers.rules.Mount(g)
	}
	if c.routers.sources != nil {
		c.routers.sources.Mount(g)
	}
	if c.routers.channels != nil {
		c.routers.channels.Mount(g)
	}
	if c.routers.notifs != nil {
		c.routers.notifs.Mount(g)
	}
	if c.routers.silences != nil {
		c.routers.silences.Mount(g)
	}
	if c.routers.stats != nil {
		c.routers.stats.Mount(g)
	}
	if c.routers.enrichers != nil {
		c.routers.enrichers.Mount(g)
	}
}

// mountOps registers the unauthenticated operational surface.
func (c *Container) mountOps(r chi.Router) {
	// Liveness. Deliberately does NOT touch the database: a Postgres outage must
	// not make Kubernetes restart every oto pod.
	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		httpx.JSON(w, req, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": c.Config.Service,
			"version": c.Config.Version,
		})
	})

	// Readiness. Reports the truth about dependencies; a failure here removes the
	// pod from the load balancer without killing it.
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		body := map[string]any{"status": "ok"}
		status := http.StatusOK

		if c.Pools == nil {
			status = http.StatusServiceUnavailable
			body["status"] = "unavailable"
			body["db"] = "not initialised"
		} else if err := c.Pools.Ping(req.Context(), 2*time.Second); err != nil {
			status = http.StatusServiceUnavailable
			body["status"] = "unavailable"
			body["db"] = err.Error()
		} else {
			body["db"] = "ok"
			body["pools"] = c.Pools.Stats()
		}
		httpx.JSON(w, req, status, body)
	})

	if c.Config.Telemetry.MetricsEnabled && c.Telemetry != nil {
		r.Method(http.MethodGet, c.Config.Telemetry.MetricsPath, c.Telemetry.MetricsHandler())
	}

	// The published contract, served from the SAME BYTES the TypeScript client
	// and the contract test read (§E.2, §I.2). Serving a regenerated or
	// hand-written copy here is how a published contract and a served one drift.
	r.Get("/openapi.json", c.serveOpenAPI)
}

// openAPIOnce computes the JSON contract exactly once. The document is hundreds
// of kilobytes and re-encoding it on every poll of a docs page is pure waste.
var (
	openAPIOnce sync.Once
	openAPIJSON []byte
	openAPIErr  error
)

func (c *Container) serveOpenAPI(w http.ResponseWriter, req *http.Request) {
	openAPIOnce.Do(func() { openAPIJSON, openAPIErr = openapi.JSON() })
	if openAPIErr != nil {
		httpx.Error(w, req, openAPIErr)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPIJSON)
}
