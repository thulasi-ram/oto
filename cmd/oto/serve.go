package main

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/httpx"
	mw "github.com/thulasiram/oto/internal/platform/httpx/middleware"
)

// newRouter assembles the whole HTTP surface: the ops endpoints, which this
// package owns, and the versioned API, which internal/app.Register owns.
func newRouter(c *app.Container) http.Handler {
	r := chi.NewRouter()

	r.Use(mw.RequestID)
	r.Use(mw.Logger(c.Logger))
	r.Use(mw.Recover)
	r.Use(mw.CORS(c.Config.HTTP))
	r.Use(mw.MaxBody(c.Config.HTTP.MaxBodyBytes))

	// --- ops surface (unauthenticated, outside /api/v1) ---

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

	if c.Config.Telemetry.MetricsEnabled {
		r.Method(http.MethodGet, c.Config.Telemetry.MetricsPath, c.Telemetry.MetricsHandler())
	}

	// --- public API ---
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(mw.Timeout(c.Config.HTTP.RequestTimeout))
		app.Register(r)
	})

	return r
}
