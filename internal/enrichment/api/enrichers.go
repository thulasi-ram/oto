package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// EnricherDTO renders `EnricherDTO`: one registered enricher with its phase,
// version and observed health.
//
// Every json tag is byte-identical to api/openapi/openapi.yaml.
type EnricherDTO struct {
	Name          string     `json:"name"`
	DisplayName   string     `json:"display_name,omitempty"`
	Version       int32      `json:"version"`
	Phase         int32      `json:"phase"`
	TimeoutMS     int32      `json:"timeout_ms,omitempty"`
	Enabled       bool       `json:"enabled"`
	HealthStatus  string     `json:"health_status,omitempty"`
	CacheHitRate  *float32   `json:"cache_hit_rate,omitempty"`
	SuccessRate   *float32   `json:"success_rate,omitempty"`
	P95DurationMS *int32     `json:"p95_duration_ms,omitempty"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
}

// Registry is the port this layer declares for itself, satisfied by
// *service.Registry.
type Registry interface {
	All() []domain.Enricher
	Enabled(name string) bool
}

// Compile-time proof that the registry satisfies the port this layer declares.
var _ Registry = (*service.Registry)(nil)

// Router serves the enricher half of the Discovery tag.
type Router struct {
	reg Registry
	clk clock.Clock
}

// NewRouter builds the enrichment HTTP surface. A nil registry serves an empty
// list, which is the correct answer for a deployment running with enrichment
// switched off.
func NewRouter(reg Registry, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{reg: reg, clk: clk}
}

// Register mounts every route this package owns onto r, already rooted at
// /api/v1.
func (rt *Router) Register(r chi.Router) { r.Get("/enrichers", rt.listEnrichers) }

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

// listEnrichers is `GET /api/v1/enrichers`.
//
// The registry is ordered by (phase, name), so two calls always return the same
// order — a list whose ordering depends on map iteration produces diffs nobody
// can golden-test.
//
// `health_status` reports `unknown` rather than guessing: the rolling health
// counters are held by the pipeline's metrics and are not readable from the
// registry, and an invented `healthy` would be worse than an honest `unknown`.
func (rt *Router) listEnrichers(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now().UTC()

	if _, _, err := authn.Scope(r.Context()); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := []EnricherDTO{}
	if rt.reg != nil {
		for _, e := range rt.reg.All() {
			out = append(out, EnricherDTO{
				Name:         e.Name(),
				Version:      int32(e.Version()),
				Phase:        int32(e.Phase()),
				TimeoutMS:    int32(e.Timeout().Milliseconds()),
				Enabled:      rt.reg.Enabled(e.Name()),
				HealthStatus: "unknown",
			})
		}
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}
