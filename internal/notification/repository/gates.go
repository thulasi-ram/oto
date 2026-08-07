package repository

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/jobs/ordering"
)

// OrderingGates mints a per-tenant ordering gate.
//
// The gate itself is STATELESS — everything it knows it reads under the lock, so
// two gates in two pods cannot disagree — but its Store is tenant-scoped, and a
// gate that could read another org's threads would be a cross-tenant leak in the
// one component that decides what gets sent. Minting one per scope is cheaper
// than the round trip it is about to make anyway.
type OrderingGates struct {
	pool    *pgxpool.Pool
	clk     clock.Clock
	log     *slog.Logger
	metrics *ordering.Metrics
	lease   time.Duration
	policy  ordering.Policy
	events  *EventRepository
}

// GatesConfig configures the factory.
type GatesConfig struct {
	Pool  *pgxpool.Pool
	Clock clock.Clock
	// Logger receives one WARN per gap-recovery advance. Sustained non-zero means
	// a destination is broken and somebody should be looking at it.
	Logger *slog.Logger
	// Registerer registers the ordering metrics. Nil yields unregistered
	// collectors, which is what a test wants and what a second Client in one
	// process needs.
	Registerer prometheus.Registerer
	// StaleClaimLease is how long a `sending` delivery is believed before it is
	// treated as abandoned. Zero means the §G.5 default of 120 s.
	StaleClaimLease time.Duration
	// Policy tunes the gate's waits. The zero value means ordering.DefaultPolicy.
	Policy ordering.Policy
}

// NewOrderingGates builds the factory.
func NewOrderingGates(cfg GatesConfig) *OrderingGates {
	g := &OrderingGates{
		pool: cfg.Pool, clk: cfg.Clock, log: cfg.Logger,
		metrics: ordering.NewMetrics(cfg.Registerer),
		lease:   cfg.StaleClaimLease, policy: cfg.Policy,
	}
	if g.clk == nil {
		g.clk = clock.New()
	}
	if g.log == nil {
		g.log = slog.Default()
	}
	g.events = NewEventRepository(g.pool, g.clk)
	return g
}

// Gate builds the ordering gate for one tenant, backed by this module's
// `channel_threads` and `notification_deliveries`.
func (g *OrderingGates) Gate(s db.TenantScope) (*ordering.Gate, error) {
	return ordering.NewGate(ordering.GateConfig{
		Store:           NewOrderingStore(g.pool, s, g.events),
		DB:              g.pool,
		Policy:          g.policy,
		StaleClaimLease: g.lease,
		Clock:           g.clk,
		Logger:          g.log,
		Metrics:         g.metrics,
	})
}
