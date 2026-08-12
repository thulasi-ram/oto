package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// The ports below are declared by the CONSUMER (SPEC §F.5): this service says
// what it needs, `internal/ingestion/repository` implements it, and
// `internal/app/container.go` injects the concrete. Every method takes a
// `db.TenantScope` second, so there is no query here that can forget its org.
//
// The two exceptions are marked and justified in place. Both exist because the
// `ingest.process_batch` payload names a batch and nothing else (§G.3), so a
// worker must discover the org before a scope can exist.

// BatchRepository owns `ingest_batches`.
type BatchRepository interface {
	// Insert writes the pending batch row. It is expected to run inside the
	// caller's transaction, alongside the dedup insert and the job enqueue, so
	// that a 202 is a promise rather than a hope (§G.1).
	Insert(ctx context.Context, s db.TenantScope, in domain.NewBatchParams) (domain.Batch, error)

	// Get loads one batch. `receivedAt` is REQUIRED because it is the partition
	// key: without it this scans every retained daily partition instead of one.
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID, receivedAt time.Time) (domain.Batch, error)

	// MarkProcessed closes a batch out. `status` is one of processed, partial or
	// failed; `failed` requires a non-empty reason (ingest_batches_err_ck).
	MarkProcessed(ctx context.Context, s db.TenantScope, id uuid.UUID, receivedAt time.Time,
		status domain.Status, at time.Time, failure string) error

	// ListFailed is the failed-batch feed, newest first, keyset-paginated. It is
	// the only READ here that is not addressed by primary key, and it exists so
	// that a batch which was accepted and never processed is visible in the
	// product rather than only in `psql`.
	ListFailed(ctx context.Context, s db.TenantScope, f domain.BatchFailureFilter,
		p db.Keyset) ([]domain.BatchFailure, db.Cursor, error)

	// ⚠️ ResolveOrg is the ONE method here without a TenantScope, and the reason is
	// structural rather than lazy: `IngestProcessBatchArgs` carries `{batch_id,
	// received_at}` and no org (SPEC §G.3), so the org has to be discovered before
	// a scope can be built. It returns exactly one column of one row addressed by
	// its full primary key, and every subsequent call in the worker is scoped by
	// what it returns.
	ResolveOrg(ctx context.Context, batchID uuid.UUID, receivedAt time.Time) (uuid.UUID, error)
}

// DedupRepository owns `ingest_dedup` — webhook replay suppression (§C.5).
//
// The table is DELIBERATELY UNPARTITIONED (C14): a UNIQUE index on a partitioned
// table can only enforce uniqueness within a partition, and this uniqueness must
// be GLOBAL, or an HA Alertmanager pair straddling a partition boundary at
// midnight would double-record a batch.
type DedupRepository interface {
	// Claim is a single `INSERT … ON CONFLICT DO NOTHING RETURNING`, never a
	// read-then-write: two pods racing on the same batch must not both win, and a
	// SELECT-then-INSERT is exactly the race that lets them.
	Claim(ctx context.Context, sourceID uuid.UUID, dedupKey string, batchID uuid.UUID, at time.Time) (domain.DedupHit, error)

	// Prune deletes rows older than the TTL horizon. Run from a maintenance job;
	// the table is small precisely because this runs.
	Prune(ctx context.Context, before time.Time) (int64, error)
}

// RejectionRepository owns `ingest_rejections`.
//
// Writing a row here is what makes 202 legitimate for a partially bad payload.
// A 4xx would make Alertmanager delete the alert forever (§G.2), so a rejection
// is a RECORD, not a refusal, and oto never silently drops.
type RejectionRepository interface {
	Record(ctx context.Context, s db.TenantScope, r domain.Rejection) error
	RecordBatch(ctx context.Context, s db.TenantScope, r []domain.Rejection) error

	// List is THE PER-SOURCE REJECTION FEED, newest first, keyset-paginated —
	// the screen the table was built for, riding
	// `ingest_rejections_source_idx (org_id, source_id, received_at DESC)`. It
	// carries the rejected label set out of `raw` alongside the reason, because
	// "something was refused" is a metric and "THIS alert was refused" is the
	// answer an operator came for.
	List(ctx context.Context, s db.TenantScope, f domain.RejectionFilter,
		p db.Keyset) ([]domain.RejectionEntry, db.Cursor, error)
}

// SourceConfigs supplies the five per-source facts the ingest path needs.
//
// It is a port rather than a dependency on `sources/service` so that the ingest
// hot path can put a short-lived cache in front of it without the sources module
// knowing, and so that this service is exercisable with no database at all.
type SourceConfigs interface {
	Config(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (domain.SourceConfig, error)
}

// AlertObserver is the NARROW port into the alerts module — the only one
// ingestion has, and the only write path into `alerts` (§G.4, C18).
//
// ⭐ THE CONTRACT, which is what makes ingestion's own retry safety possible:
//
//   - ObserveBatch is IDEMPOTENT. It is called inside the caller's transaction
//     (`db.FromContext`), and re-applying the same Observations must produce no
//     second occurrence and no second event. §G.5 already requires exactly that of
//     the alert upsert (ON CONFLICT (org_id, alert_key)) and the event append
//     (`alert_event_keys`), so this is a restatement, not a new demand.
//   - It runs the §B.3 state machine, resolves the AlertGroup generation, appends
//     events and enqueues `enrich.run` / `notify.evaluate`. Ingestion does none of
//     that and must not learn how.
//   - It returns the number of observations APPLIED. Ingestion uses the count for
//     `oto_ingest_accepted_total` and for the batch's own accounting; it does not
//     inspect what happened to any individual alert.
//   - An error fails the whole chunk. Partial failure INSIDE a chunk is not
//     expressible, and deliberately so: a half-applied transaction is not a state
//     oto is willing to describe. Per-alert failures are caught before this call,
//     at the bounds, and become `ingest_rejections` rows.
//
// The parameter type is `alerts/domain.Observation` — the shared domain kernel
// (§C.1, CONTEXT.md §5.2b), which is the single sanctioned cross-domain `domain`
// import and therefore the one type both modules may name.
type AlertObserver interface {
	ObserveBatch(ctx context.Context, s db.TenantScope, obs []alerts.Observation) (int, error)
}
