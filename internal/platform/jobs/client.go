package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// DefaultQueueWorkers is the per-queue concurrency of SPEC §G.3.
//
// These are not arbitrary. `ingest` is the widest because a webhook batch must
// never queue behind anything. `deliver_slack` is the narrowest because Slack
// allows roughly one message per second per channel, so extra workers buy
// contention and 429s rather than throughput. `maintenance` is one because its
// jobs are DDL-adjacent and must not race themselves.
//
// ⛔ `reconcile`'s 8 IS A SUPPORTED TENANT COUNT, NOT A THROUGHPUT PREFERENCE, and
// SPEC §G.3.1 publishes the arithmetic that produces it. Every other number here
// trades latency for contention; this one decides whether `source.reconcile` keeps
// up with its own 30-second schedule at all, because the fan-out offers a fresh
// round of network-bound per-source passes every tick and a queue too narrow to
// drain one tick never catches up. It was 2, which is ~30 tenants — below any
// multi-tenant install — and is now 8, which is ~120. `jobs.queue_reconcile` moves
// it further without a rebuild.
func DefaultQueueWorkers() map[string]int {
	return map[string]int{
		QueueIngest:         16,
		QueueEnrich:         8,
		QueueNotify:         8,
		QueueDeliverSlack:   4,
		QueueDeliverWebhook: 8,
		QueueReconcile:      8,
		QueueLifecycle:      4,
		QueueMaintenance:    1,
	}
}

// Config configures the job Client.
type Config struct {
	// Pool backs River's own bookkeeping: fetching, completing and scheduling.
	//
	// This is the GENERAL pool, deliberately. SPEC §G.10 reserves the ingest pool
	// for the webhook handler and the ingest workers' own domain queries; River's
	// queue traffic is not domain traffic, and giving it the small pool would let
	// queue bookkeeping starve the very path the pool exists to protect.
	Pool *pgxpool.Pool

	// Queues maps queue name to worker count. Nil or empty means this client is
	// INSERT-ONLY — the shape an API pod wants, where enqueueing is required and
	// working jobs is not.
	Queues map[string]int

	// Registry supplies the workers and the periodic schedule. Required whenever
	// Queues is non-empty.
	Registry *Registry

	// FetchCooldown and FetchPollInterval bound how eagerly workers look for work.
	// Polling is the FALLBACK; River also listens for insert notifications, so the
	// poll interval is the worst-case latency when a notification is missed.
	FetchCooldown     time.Duration
	FetchPollInterval time.Duration

	// JobTimeout bounds any job whose Spec does not set its own.
	JobTimeout time.Duration

	// RescueStuckJobsAfter re-queues jobs whose worker died mid-flight. This is
	// what makes a `kill -9` recoverable rather than a permanently lost alert.
	RescueStuckJobsAfter time.Duration

	// QueueDepthInterval is how often queue depth is sampled into Prometheus.
	// Zero disables sampling.
	QueueDepthInterval time.Duration

	Logger *slog.Logger
	Clock  clock.Clock
}

// FromPlatformConfig derives a jobs.Config from the process configuration.
//
// Per-queue concurrency starts from the SPEC §G.3 defaults and is then scaled by
// the coarse knobs config exposes, so an operator who halves `jobs.queue_delivery`
// halves both delivery queues without having to know their names. Anything finer is
// set on the returned Config directly.
//
// ⚠️ `jobs.queue_reconcile` IS THE ONE KNOB THAT NAMES A SINGLE QUEUE, and it is
// deliberately not folded into `queue_default`. The other knobs are coarse because
// the queues under them are interchangeable in kind; `reconcile` is not — its
// width is the supported tenant count of the whole deployment (SPEC §G.3.1), so
// an operator widening it is answering a capacity question, not a latency one,
// and must not have to widen `enrich`, `notify` and `lifecycle` to do it.
func FromPlatformConfig(cfg config.JobsConfig) Config {
	queues := DefaultQueueWorkers()
	if cfg.QueueIngest > 0 {
		queues[QueueIngest] = cfg.QueueIngest
	}
	if cfg.QueueDefault > 0 {
		queues[QueueEnrich] = cfg.QueueDefault
		queues[QueueNotify] = cfg.QueueDefault
		queues[QueueLifecycle] = cfg.QueueDefault
	}
	if cfg.QueueDelivery > 0 {
		queues[QueueDeliverSlack] = cfg.QueueDelivery
		queues[QueueDeliverWebhook] = cfg.QueueDelivery
	}
	if cfg.QueueReconcile > 0 {
		queues[QueueReconcile] = cfg.QueueReconcile
	}

	return Config{
		Queues:               queues,
		FetchCooldown:        100 * time.Millisecond,
		FetchPollInterval:    cfg.FetchInterval,
		JobTimeout:           cfg.JobTimeout,
		RescueStuckJobsAfter: cfg.RescueAfter,
		QueueDepthInterval:   15 * time.Second,
	}
}

// Client is oto's job queue: the db.Enqueuer implementation and the worker
// runtime, wrapping River.
type Client struct {
	river   *river.Client[pgx.Tx]
	pool    *pgxpool.Pool
	rt      *Runtime
	logger  *slog.Logger
	clk     clock.Clock
	depthIn time.Duration

	stopSample context.CancelFunc
	sampleDone chan struct{}
}

// Compile-time proof that Client is the port every service depends on.
var _ db.Enqueuer = (*Client)(nil)

// New builds the job client.
func New(cfg Config) (*Client, error) {
	if cfg.Pool == nil {
		return nil, errors.New("jobs: a pool is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.New()
	}

	rt := &Runtime{Logger: logger, Clock: clk}
	if cfg.Registry != nil {
		rt = cfg.Registry.Runtime()
	}
	rt.normalise()

	rc := &river.Config{
		Logger: logger.With("component", "river"),
		// Reserve River's half of the advisory-lock space explicitly, so that a
		// key oto mints for a ChannelThread can never collide with a key River
		// mints for its own leader election. See db.LockNamespace.
		AdvisoryLockPrefix:   db.JobsAdvisoryLockPrefix,
		FetchCooldown:        cfg.FetchCooldown,
		FetchPollInterval:    cfg.FetchPollInterval,
		JobTimeout:           cfg.JobTimeout,
		RescueStuckJobsAfter: cfg.RescueStuckJobsAfter,
		RetryPolicy:          newRetryPolicy(clk),
		ErrorHandler:         &errorHandler{rt: rt},
	}

	if len(cfg.Queues) > 0 {
		if cfg.Registry == nil {
			return nil, errors.New("jobs: a registry is required when queues are configured")
		}
		rc.Workers = cfg.Registry.workers
		rc.PeriodicJobs = cfg.Registry.periodic
		rc.Queues = make(map[string]river.QueueConfig, len(cfg.Queues))
		for name, n := range cfg.Queues {
			if n <= 0 {
				continue
			}
			rc.Queues[name] = river.QueueConfig{MaxWorkers: n}
		}
	}

	rcli, err := river.NewClient(riverpgxv5.New(cfg.Pool), rc)
	if err != nil {
		return nil, fmt.Errorf("jobs: new river client: %w", err)
	}

	return &Client{
		river:   rcli,
		pool:    cfg.Pool,
		rt:      rt,
		logger:  logger,
		clk:     clk,
		depthIn: cfg.QueueDepthInterval,
	}, nil
}

// ---------------------------------------------------------------- enqueue

// Enqueue inserts one job, JOINING THE CALLER'S TRANSACTION when ctx carries one.
//
// This is the transactional outbox (ADR 0001, SPEC §G.1): the job and the row
// that justifies it commit together or not at all. There is no window in which
// oto has recorded a batch it will never process, and none in which it processes
// a batch it never recorded.
func (c *Client) Enqueue(ctx context.Context, args db.JobArgs, opts ...db.JobOption) (db.EnqueueResult, error) {
	res, err := c.EnqueueMany(ctx, []db.JobRequest{{Args: args, Opts: opts}})
	if err != nil {
		return db.EnqueueResult{}, err
	}
	return res[0], nil
}

// EnqueueMany inserts a batch in one round trip, joining the caller's transaction
// when ctx carries one. A 200-alert webhook must not become 200 inserts.
func (c *Client) EnqueueMany(ctx context.Context, reqs []db.JobRequest) ([]db.EnqueueResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}

	params := make([]river.InsertManyParams, 0, len(reqs))
	for _, r := range reqs {
		args, ok := r.Args.(river.JobArgs)
		if !ok || r.Args == nil {
			return nil, errs.Internal("job_args_invalid",
				fmt.Errorf("jobs: %T is not a registered job payload", r.Args))
		}
		params = append(params, river.InsertManyParams{
			Args:       args,
			InsertOpts: toRiverOpts(db.ApplyJobOptions(db.JobOptions{}, r.Opts...)),
		})
	}

	var (
		out []*rivertype.JobInsertResult
		err error
	)
	if tx, ok := db.FromContext(ctx, nil).(pgx.Tx); ok && tx != nil {
		out, err = c.river.InsertManyTx(ctx, tx, params)
	} else {
		out, err = c.river.InsertMany(ctx, params)
	}
	if err != nil {
		// Enqueue failure on the ingest path is backpressure, not a client error:
		// SPEC §G.2 requires 503 + Retry-After, because a 4xx makes Alertmanager
		// delete the notification forever.
		return nil, errs.Wrap(err, errs.KindUnavailable, "enqueue_failed",
			"could not enqueue background work").WithRetryAfter(5 * time.Second)
	}

	results := make([]db.EnqueueResult, 0, len(out))
	for _, r := range out {
		c.rt.Metrics.Enqueued.WithLabelValues(r.Job.Kind, r.Job.Queue).Inc()
		results = append(results, db.EnqueueResult{
			ID:      r.Job.ID,
			Kind:    r.Job.Kind,
			Queue:   r.Job.Queue,
			Skipped: r.UniqueSkippedAsDuplicate,
		})
	}
	return results, nil
}

// toRiverOpts maps the port's options onto River's, returning nil when nothing
// was overridden so that the args struct's own InsertOpts stay authoritative.
func toRiverOpts(o db.JobOptions) *river.InsertOpts {
	empty := o.Queue == "" && o.Priority == 0 && o.MaxAttempts == 0 &&
		o.ScheduledAt.IsZero() && len(o.Tags) == 0 && o.UniquePeriod == 0
	if empty {
		return nil
	}
	ro := &river.InsertOpts{
		Queue:       o.Queue,
		Priority:    o.Priority,
		MaxAttempts: o.MaxAttempts,
		ScheduledAt: o.ScheduledAt,
		Tags:        o.Tags,
	}
	if o.UniquePeriod > 0 {
		ro.UniqueOpts = river.UniqueOpts{ByArgs: true, ByQueue: true, ByPeriod: o.UniquePeriod}
	}
	return ro
}

// ---------------------------------------------------------------- lifecycle

// Start begins working jobs and sampling queue depth. It is a no-op for an
// insert-only client.
func (c *Client) Start(ctx context.Context) error {
	if err := c.river.Start(ctx); err != nil {
		return fmt.Errorf("jobs: start: %w", err)
	}

	if c.depthIn > 0 {
		sampleCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		c.stopSample = cancel
		c.sampleDone = make(chan struct{})
		go c.sampleQueueDepth(sampleCtx)
	}

	c.logger.Info("jobs: worker runtime started")
	return nil
}

// Stop drains gracefully: no new jobs are fetched and in-flight jobs are given
// until ctx expires to finish.
//
// Graceful is the point. A hard stop leaves jobs in `running`, and they only come
// back when the rescuer notices — which is minutes of an alert sitting undelivered
// for no reason at all.
func (c *Client) Stop(ctx context.Context) error {
	if c.stopSample != nil {
		c.stopSample()
		<-c.sampleDone
	}

	if err := c.river.Stop(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			c.logger.Warn("jobs: graceful drain timed out, cancelling in-flight jobs")
			// Whatever is still running will be picked up by the rescuer.
			if cerr := c.river.StopAndCancel(context.WithoutCancel(ctx)); cerr != nil {
				return fmt.Errorf("jobs: hard stop: %w", cerr)
			}
			return nil
		}
		return fmt.Errorf("jobs: stop: %w", err)
	}

	c.logger.Info("jobs: worker runtime stopped")
	return nil
}

// River exposes the underlying client. It exists for cmd/oto's migrate and
// operator subcommands ONLY; no domain package may call it.
func (c *Client) River() *river.Client[pgx.Tx] { return c.river }

// ---------------------------------------------------------------- queue depth

// queueDepthSQL counts only the non-terminal states. `completed` dominates
// river_job by orders of magnitude and tells an operator nothing about backlog,
// so it is excluded rather than aggregated and thrown away.
const queueDepthSQL = `
SELECT queue, state::text, count(*)
  FROM river_job
 WHERE state IN ('available','running','retryable','scheduled','pending')
 GROUP BY 1, 2`

func (c *Client) sampleQueueDepth(ctx context.Context) {
	defer close(c.sampleDone)

	t := time.NewTicker(c.depthIn)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.collectQueueDepth(ctx); err != nil && ctx.Err() == nil {
				c.logger.Warn("jobs: queue depth sample failed", "error", err)
			}
		}
	}
}

func (c *Client) collectQueueDepth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := c.pool.Query(ctx, queueDepthSQL)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Reset first so a queue that drained to zero stops reporting its last value.
	// A stale gauge is worse than a missing one: it is a backlog alert that never
	// clears.
	c.rt.Metrics.QueueDepth.Reset()

	for rows.Next() {
		var queue, state string
		var n int64
		if err := rows.Scan(&queue, &state, &n); err != nil {
			return err
		}
		c.rt.Metrics.QueueDepth.WithLabelValues(queue, state).Set(float64(n))
	}
	return rows.Err()
}
