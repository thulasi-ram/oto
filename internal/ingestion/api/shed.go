package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/ingestion/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The shed reasons. They are the `reason` label of `oto_ingest_shed_total`, and
// the distinction between them is what an operator actually needs: "we are out of
// database" and "the workers are behind" have different fixes.
const (
	// ShedPoolExhausted means no ingest connection became available inside the
	// acquisition budget.
	ShedPoolExhausted = "pool_exhausted"
	// ShedInFlight means the concurrency gate is full.
	ShedInFlight = "in_flight"
	// ShedQueueDepth means the `ingest` queue backlog is past the point where a
	// new batch would sit longer than Alertmanager's retry budget anyway.
	ShedQueueDepth = "queue_depth"
)

// QueueDepthSampler reports the `ingest` queue backlog.
//
// It is a port so that the sampler can be the general pool, a fake, or nothing at
// all. A nil sampler disables depth-based shedding, which is the correct default
// for a single-pod deployment where the queue is never the bottleneck.
type QueueDepthSampler interface {
	Depth(ctx context.Context) (int, error)
}

// ShedConfig configures the backpressure gate.
type ShedConfig struct {
	// MaxInFlight bounds concurrent accepts. It should equal the INGEST POOL SIZE:
	// every accept holds one connection for its transaction, so admitting more
	// than the pool can serve converts a bounded queue in oto into an unbounded
	// one in pgx, where it is invisible and where the wait is not measured.
	MaxInFlight int
	// Wait is how long a request may wait for a slot before it is shed. It IS
	// `ingest.acquire_timeout` (500 ms by default, §G.10) — pgxpool has no
	// acquisition timeout of its own, so this gate is where that budget is
	// enforced: oto would rather answer 503 in half a second and let Alertmanager
	// retry inside its ~5-minute budget than hold a connection open for five.
	Wait time.Duration
	// MaxQueueDepth is the `ingest` backlog above which new batches are shed.
	// Zero disables depth-based shedding.
	MaxQueueDepth int
	// DepthInterval is how often the backlog is sampled. The sample is CACHED
	// between intervals: querying `river_job` per webhook would put a count(*) on
	// the hot path to protect the hot path.
	DepthInterval time.Duration
	// RetryAfter is the `Retry-After` sent with every 503.
	RetryAfter time.Duration
}

// DefaultMaxQueueDepth is the backlog at which oto starts shedding.
//
// The number is derived, not chosen: 16 `ingest` workers (§G.3) at a conservative
// 100 ms per batch clear roughly 160 batches a second, so 25 000 queued batches
// is about two and a half minutes of backlog. That sits INSIDE Alertmanager's
// retry budget of `max(group_interval,10s) + peer_position×15s` (~5 minutes), so
// a shed 503 is something the upstream can actually recover from. Shedding later
// than this would mean accepting batches that will not be processed before the
// upstream gives up — a 202 that is not a promise, which is the one thing this
// path may never do.
const DefaultMaxQueueDepth = 25_000

// DefaultDepthInterval is how often the backlog is re-measured.
const DefaultDepthInterval = time.Second

// Shedder decides when oto sheds load, and is the ONLY place that decision is
// made.
//
// ⭐ SHEDDING IS A FEATURE (C17, ADR 0007). Alertmanager retries 5xx and only
// 5xx, for about five minutes; a 503 with a Retry-After is therefore a designed,
// sufficient backpressure channel and a deliberate one is strictly better than
// the accidental version, where a request queues on a pgx semaphore until the
// upstream's own deadline kills it and nobody records why.
//
// It is also why there is no rate limiter here. A 429 is a 4xx, and a 4xx makes
// Alertmanager delete the notification permanently and silently.
type Shedder struct {
	sem      chan struct{}
	wait     time.Duration
	maxDepth int
	interval time.Duration
	retry    time.Duration

	pool    *pgxpool.Pool
	sampler QueueDepthSampler
	clk     clock.Clock
	log     *slog.Logger
	metrics *service.Metrics

	mu      sync.Mutex
	depth   int
	depthAt time.Time
}

// NewShedder builds the gate. A nil sampler disables depth-based shedding.
func NewShedder(
	cfg ShedConfig, pool *pgxpool.Pool, sampler QueueDepthSampler,
	clk clock.Clock, logger *slog.Logger, metrics *service.Metrics,
) *Shedder {
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = defaultInFlight(pool)
	}
	if cfg.Wait <= 0 {
		cfg.Wait = 500 * time.Millisecond
	}
	if cfg.DepthInterval <= 0 {
		cfg.DepthInterval = DefaultDepthInterval
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = domain.RetryAfter
	}
	if clk == nil {
		clk = clock.New()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = service.NewMetrics(nil)
	}

	return &Shedder{
		sem:      make(chan struct{}, cfg.MaxInFlight),
		wait:     cfg.Wait,
		maxDepth: cfg.MaxQueueDepth,
		interval: cfg.DepthInterval,
		retry:    cfg.RetryAfter,
		pool:     pool,
		sampler:  sampler,
		clk:      clk,
		log:      logger,
		metrics:  metrics,
	}
}

// defaultInFlight sizes the gate to the ingest pool, which is what it is
// protecting. Four is the configured floor for that pool (§G.10).
func defaultInFlight(pool *pgxpool.Pool) int {
	if pool == nil {
		return 4
	}
	if n := int(pool.Config().MaxConns); n > 0 {
		return n
	}
	return 4
}

// Enter admits a request or sheds it. The returned release MUST be called.
//
// Order matters, cheapest and most decisive first:
//
//  1. Queue depth — free (a cached number), and the condition under which
//     admitting the request is pointless rather than merely slow.
//  2. Pool saturation — one atomic read. The ingest pool is shared with the
//     `ingest` QUEUE WORKERS (§G.10), so it can be empty while this handler's own
//     gate is wide open; the semaphore alone cannot see that.
//  3. The concurrency gate, with the acquisition budget as its ceiling.
func (s *Shedder) Enter(ctx context.Context) (func(), error) {
	if reason, shed := s.overloaded(ctx); shed {
		return nil, s.shed(reason)
	}
	if s.PoolSaturated() {
		return nil, s.shed(ShedPoolExhausted)
	}

	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	default:
	}

	// The gate is full. Wait for the acquisition budget and no longer — holding
	// the upstream's connection open past that spends its retry budget on queuing
	// rather than on retrying.
	timer := time.NewTimer(s.wait)
	defer timer.Stop()

	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-timer.C:
		return nil, s.shed(ShedInFlight)
	case <-ctx.Done():
		// The client hung up. Not backpressure, and counting it as such would make
		// disconnects look like overload.
		return nil, ctx.Err()
	}
}

// overloaded reports whether the ingest queue is past its shedding threshold,
// using a sample that is at most DepthInterval old.
//
// A FAILED SAMPLE DOES NOT SHED. If `river_job` cannot be counted, the honest
// state is "unknown", and turning unknown into 503 would mean a hiccup on the
// general pool stops ingestion — which is the exact coupling the two-pool design
// exists to prevent (§G.10).
func (s *Shedder) overloaded(ctx context.Context) (string, bool) {
	if s.sampler == nil || s.maxDepth <= 0 {
		return "", false
	}

	now := s.clk.Now()

	s.mu.Lock()
	fresh := now.Sub(s.depthAt) < s.interval
	depth := s.depth
	s.mu.Unlock()

	if !fresh {
		sampleCtx, cancel := context.WithTimeout(ctx, s.interval)
		n, err := s.sampler.Depth(sampleCtx)
		cancel()

		s.mu.Lock()
		if err != nil {
			s.depthAt = now
			s.mu.Unlock()
			s.log.WarnContext(ctx, "ingest: queue depth sample failed; not shedding", "error", err)
			return "", false
		}
		s.depth, s.depthAt = n, now
		depth = n
		s.mu.Unlock()
	}

	return ShedQueueDepth, depth > s.maxDepth
}

// PoolSaturated reports whether the ingest pool has no capacity left. It is
// exposed for the readiness surface; the accept path learns the same fact by
// failing to acquire, which is cheaper and never wrong.
func (s *Shedder) PoolSaturated() bool {
	if s.pool == nil {
		return false
	}
	stat := s.pool.Stat()
	return stat.AcquiredConns() >= stat.MaxConns()
}

// shed records and builds the 503.
//
// KindUnavailable is the ONLY correct failure on this path (C4). Never
// KindRateLimited: that is a 429, and a 429 is a 4xx, and Alertmanager deletes
// the notification permanently for any 4xx.
func (s *Shedder) shed(reason string) error {
	s.metrics.Shed.WithLabelValues(reason).Inc()
	return errs.Unavailable("ingest_overloaded",
		"oto is shedding load; retry shortly", s.retry)
}
