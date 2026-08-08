package api

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/rules/domain"
	"github.com/thulasiram/oto/internal/rules/service"
)

// RuleService is the port this layer declares for itself, satisfied by
// *service.Service.
type RuleService interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Snapshot, error)
	History(ctx context.Context, s db.TenantScope, key domain.Key) (domain.History, error)
	DiffSince(ctx context.Context, s db.TenantScope, key domain.Key, boundFingerprint string) (domain.Diff, bool, error)
}

// AlertReader is the cross-domain port that resolves an alert or an occurrence to
// the rule snapshot bound to it.
//
// It is declared HERE, by the consumer (CONTEXT.md §5.4), and is satisfied by
// `alerts/service`. `rules` never reaches into the alerts repository.
type AlertReader interface {
	Get(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (alerts.AlertDetail, error)
	GetOccurrence(ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID) (alertdomain.Occurrence, error)
}

// Compile-time proof that the services satisfy the ports this layer declares.
var (
	_ RuleService = (*service.Service)(nil)
	_ AlertReader = (*alerts.Service)(nil)
)

// Router serves the Rules tag plus the two rule reads the contract files under
// Alerts and Occurrences.
type Router struct {
	svc    RuleService
	alerts AlertReader
	clk    clock.Clock
}

// NewRouter builds the rules HTTP surface.
func NewRouter(svc RuleService, alertReader AlertReader, clk clock.Clock) *Router {
	if clk == nil {
		clk = clock.New()
	}
	return &Router{svc: svc, alerts: alertReader, clk: clk}
}

// Register mounts every route this package owns onto r, already rooted at
// /api/v1.
//
// `/alerts/{id}/rule` and `/occurrences/{id}/rule` are registered HERE rather
// than by `alerts/api`, because rendering a `RuleSnapshotDTO` means naming
// `rules/domain` and only this package may (CONTEXT.md §5.4). The contract cares
// about the path and the payload, not about which Go package produced them.
func (rt *Router) Register(r chi.Router) {
	r.Route("/rule-snapshots", func(r chi.Router) {
		r.Get("/", rt.listRuleSnapshots)
		r.Get("/{id}", rt.getRuleSnapshot)
	})
	r.Get("/alerts/{id}/rule", rt.getAlertRuleHistory)
	r.Get("/occurrences/{id}/rule", rt.getOccurrenceRule)
}

// Mount is Register under the name the other domain routers use.
func (rt *Router) Mount(r chi.Router) { rt.Register(r) }

// now is the ONE clock reading a request makes. It is the injected clock and
// never time.Now().
func (rt *Router) now() time.Time { return rt.clk.Now().UTC() }

func scopeOf(r *http.Request) (db.TenantScope, error) {
	_, s, err := authn.Scope(r.Context())
	return s, err
}

var listSnapshotParams = []string{"source_id", "rule_name", "rule_group", "rule_file", "limit", "cursor"}

func parseListSnapshots(r *http.Request) (ListSnapshotsQuery, error) {
	p := httpx.NewParams(r, listSnapshotParams...)
	if err := p.Err(); err != nil {
		return ListSnapshotsQuery{}, err
	}
	q := ListSnapshotsQuery{
		SourceID:  p.String("source_id", ""),
		RuleName:  p.String("rule_name", ""),
		RuleGroup: p.String("rule_group", ""),
		RuleFile:  p.String("rule_file", ""),
		Limit:     p.Limit(),
		Cursor:    p.Cursor(),
	}
	if err := p.Err(); err != nil {
		return ListSnapshotsQuery{}, err
	}
	return httpx.BindEmpty(q)
}

// notFound is the one shape a missing rule read takes. A `404` on the occurrence
// rule endpoint means NO SNAPSHOT COULD BE CAPTURED AT ALL — which is a different
// fact from a stored snapshot whose `origin` is `unavailable`, and the two must
// stay distinguishable.
func notFound(what string) error { return errs.NotFound("not_found", "no such "+what) }
