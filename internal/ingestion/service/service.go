package service

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Error codes this service mints. They are stable machine strings: the API maps
// them onto a status, and an operator greps for them.
const (
	// CodeUndecodable is a body that is not a webhook envelope, or is nested past
	// B16. Recorded, then 400 — the only 4xx oto returns for a payload's content,
	// and only because the same bytes could never decode on a retry.
	CodeUndecodable = "ingest_undecodable"
	// CodeBodyTooLarge is B1. Recorded, then 413.
	CodeBodyTooLarge = "ingest_body_too_large"
	// CodeAcceptFailed is any failure of the accept transaction. It is
	// KindUnavailable, NEVER a 4xx: a transient database problem that answered
	// 4xx would make Alertmanager delete the notification permanently (C4).
	CodeAcceptFailed = "ingest_accept_failed"
	// CodeAcceptUnstorable is the accept transaction failing because Postgres
	// REFUSED THE BYTES rather than because it was busy — SQLSTATE 22P05 and its
	// neighbours.
	//
	// ⛔ IT IS STILL A 503, and it must stay one: a 4xx here would make
	// Alertmanager discard the notification permanently (C4, ADR 0007). But it is
	// a 503 that will NEVER succeed on retry, which is the opposite of every other
	// backpressure case, so it gets its own code. `decode.PersistedPayload` is
	// supposed to make this unreachable; if it is ever seen, the pre-scan there has
	// a hole and an operator is watching an Alertmanager retry the same body until
	// its budget runs out.
	CodeAcceptUnstorable = "ingest_accept_unstorable"
	// CodeBatchNotFound is a `ingest.process_batch` job whose batch has aged out
	// of its retention partition.
	CodeBatchNotFound = "ingest_batch_not_found"
	// CodeProcessFailed is a failure inside batch processing. Retryable.
	CodeProcessFailed = "ingest_process_failed"
	// CodeSourceUnavailable means the source's configuration could not be read.
	// Retryable: without `redact_labels` oto must not persist the payload.
	CodeSourceUnavailable = "ingest_source_unavailable"
)

// Options are the Service's dependencies. Everything is a port or a pool, so the
// whole service is exercisable against fakes and one real Postgres.
type Options struct {
	// Pool is THE INGEST POOL and nothing else (SPEC §G.10): 25 % of connections,
	// a 2 s statement timeout and a 500 ms acquisition timeout, reserved for the
	// webhook handler and the ingest workers. Handing this the general pool would
	// let a slow dashboard query starve the path that must never block.
	Pool *pgxpool.Pool

	Batches    BatchRepository
	Dedup      DedupRepository
	Rejections RejectionRepository
	Sources    SourceConfigs
	Alerts     AlertObserver
	Enqueuer   db.Enqueuer

	Clock   clock.Clock
	Logger  *slog.Logger
	Metrics *Metrics
}

// Service is the ingestion module's business logic: accept durably, normalise
// later.
//
// The split is the whole architecture (CONTEXT.md §2.2). Accept does two writes
// and one enqueue in one short transaction and makes NO outbound network call.
// ProcessBatch does everything expensive, asynchronously, where a failure costs a
// retry instead of a lost alert.
type Service struct {
	pool       *pgxpool.Pool
	batches    BatchRepository
	dedup      DedupRepository
	rejections RejectionRepository
	sources    SourceConfigs
	alerts     AlertObserver
	enqueuer   db.Enqueuer

	clk     clock.Clock
	log     *slog.Logger
	metrics *Metrics
}

// New builds the Service, refusing anything it cannot run without.
func New(o Options) (*Service, error) {
	switch {
	case o.Pool == nil:
		return nil, errs.New(errs.KindInternal, "ingest_missing_pool", "the ingest pool is required")
	case o.Batches == nil:
		return nil, errs.New(errs.KindInternal, "ingest_missing_batches", "a batch repository is required")
	case o.Dedup == nil:
		return nil, errs.New(errs.KindInternal, "ingest_missing_dedup", "a dedup repository is required")
	case o.Rejections == nil:
		return nil, errs.New(errs.KindInternal, "ingest_missing_rejections", "a rejection repository is required")
	case o.Sources == nil:
		return nil, errs.New(errs.KindInternal, "ingest_missing_sources", "a source config port is required")
	case o.Enqueuer == nil:
		return nil, errs.New(errs.KindInternal, "ingest_missing_enqueuer", "an enqueuer is required")
	}

	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	lg := o.Logger
	if lg == nil {
		lg = slog.Default()
	}
	m := o.Metrics
	if m == nil {
		m = NewMetrics(nil)
	}

	return &Service{
		pool:       o.Pool,
		batches:    o.Batches,
		dedup:      o.Dedup,
		rejections: o.Rejections,
		sources:    o.Sources,
		alerts:     o.Alerts,
		enqueuer:   o.Enqueuer,
		clk:        clk,
		log:        lg,
		metrics:    m,
	}, nil
}

// Clock exposes the injected clock, so a worker built over this service reads the
// same time the service does. There is no `time.Now()` anywhere in this module.
func (s *Service) Clock() clock.Clock { return s.clk }
