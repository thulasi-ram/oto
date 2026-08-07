package ingestion

import (
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/thulasiram/oto/internal/ingestion/api"
	"github.com/thulasiram/oto/internal/ingestion/repository"
	"github.com/thulasiram/oto/internal/ingestion/service"
	"github.com/thulasiram/oto/internal/ingestion/worker"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// Deps is everything the ingestion module needs from the process.
//
// Pools is a *db.Pools rather than two loose pools on purpose: the split between
// them is the load-bearing part (§G.10), and handing a caller two interchangeable
// arguments is how the ingest path quietly ends up on the general pool.
type Deps struct {
	Pools    *db.Pools
	Enqueuer db.Enqueuer
	Config   config.IngestConfig

	// Alerts is the narrow port into the alerts module — the only write path into
	// `alerts` (§G.4). It may be nil before that module is wired, in which case
	// batches are accepted and persisted (a 202 is still a promise) and their jobs
	// retry until an observer exists. Nothing is lost; processing is deferred.
	Alerts service.AlertObserver

	// Sources supplies per-source redaction, injection and cluster identity. Nil
	// falls back to reading `alert_sources` directly; see repository.SourceConfigRepository
	// for the layering note and the intended replacement.
	Sources service.SourceConfigs

	Clock    clock.Clock
	Logger   *slog.Logger
	Registry prometheus.Registerer
}

// Module is the assembled ingestion module. It is what `internal/app` holds.
type Module struct {
	// Service is the business logic. Other modules must depend on THIS and never
	// on the repositories underneath it.
	Service *service.Service
	// Router mounts POST /ingest/alertmanager/{source_id}.
	Router *api.Router
	// ProcessBatch is the `ingest.process_batch` handler. Assign it to
	// jobs.Handlers.IngestProcessBatch to replace the registered stub.
	ProcessBatch jobs.Handler[jobs.IngestProcessBatchArgs]
	// Metrics is the module's Prometheus surface, exposed so a test can read it.
	Metrics *service.Metrics
}

// New assembles the module.
//
// The pool assignments below are the §G.10 contract in code, and they are the
// reason UI queries cannot starve ingestion:
//
//   - every repository on the accept and process paths is built over Pools.Ingest
//     — 25 % of connections, a 2 s statement timeout, a 500 ms acquisition budget,
//     reserved for the webhook handler and the `ingest` queue workers;
//   - the queue-depth probe, which exists only to decide whether to shed, is built
//     over Pools.General, because spending an ingest connection to ask "am I out
//     of ingest capacity?" would make the measurement part of the problem.
//
// A read on the general pool can therefore be arbitrarily slow without delaying a
// single webhook by a microsecond.
func New(d Deps) (*Module, error) {
	if d.Pools == nil || d.Pools.Ingest == nil || d.Pools.General == nil {
		return nil, errs.New(errs.KindInternal, "ingest_missing_pools",
			"both database pools are required")
	}

	clk := d.Clock
	if clk == nil {
		clk = clock.New()
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	metrics := service.NewMetrics(d.Registry)

	ingest := d.Pools.Ingest

	sources := d.Sources
	if sources == nil {
		sources = repository.NewSourceConfigRepository(ingest)
	}

	svc, err := service.New(service.Options{
		Pool:       ingest,
		Batches:    repository.NewBatchRepository(ingest),
		Dedup:      repository.NewDedupRepository(ingest),
		Rejections: repository.NewRejectionRepository(ingest),
		Sources:    sources,
		Alerts:     d.Alerts,
		Enqueuer:   d.Enqueuer,
		Clock:      clk,
		Logger:     logger,
		Metrics:    metrics,
	})
	if err != nil {
		return nil, err
	}

	auth := api.NewAuthenticator(repository.NewTokenRepository(ingest), clk, api.DefaultTokenTTL)
	shed := api.NewShedder(
		shedConfig(d.Config, ingest.Config().MaxConns),
		ingest,
		repository.NewQueueDepthRepository(d.Pools.General),
		clk, logger, metrics,
	)

	return &Module{
		Service:      svc,
		Router:       api.NewRouter(svc, auth, shed, clk, logger, metrics),
		ProcessBatch: worker.NewProcessBatch(svc, logger),
		Metrics:      metrics,
	}, nil
}

// shedConfig derives the backpressure gate from process configuration.
//
// MaxInFlight is the INGEST POOL SIZE, not a separate knob: every accept holds
// one connection for its transaction, so admitting more than the pool can serve
// only moves the queue from a place where it is measured into a place where it is
// not.
func shedConfig(cfg config.IngestConfig, poolMaxConns int32) api.ShedConfig {
	retry := cfg.RetryAfter
	if retry <= 0 {
		retry = 5 * time.Second
	}
	return api.ShedConfig{
		MaxInFlight:   int(poolMaxConns),
		Wait:          500 * time.Millisecond,
		MaxQueueDepth: api.DefaultMaxQueueDepth,
		DepthInterval: api.DefaultDepthInterval,
		RetryAfter:    retry,
	}
}
