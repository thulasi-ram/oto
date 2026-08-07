package app

import (
	"github.com/go-chi/chi/v5"
)

// Register mounts every domain's api.Router under /api/v1.
//
// It is the single seam between the platform (which owns the server, the
// middleware stack and the ops surface) and the domains (which own their own
// routes). Each domain adds exactly one line here, and nothing outside
// internal/app knows the shape of the route table.
//
// Nothing is registered yet: no domain has an api package with routes. Adding one
// looks like:
//
//	alertsapi.NewRouter(c.AlertService).Mount(r)
func Register(r chi.Router) {
	_ = r
}
