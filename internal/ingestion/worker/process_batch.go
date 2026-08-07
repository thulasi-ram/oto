package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/platform/log"
)

// BatchProcessor is the port this worker declares for itself. It is satisfied by
// *service.Service.
type BatchProcessor interface {
	// ResolveOrg discovers the batch's org, because the job payload does not carry
	// one — `IngestProcessBatchArgs` is `{batch_id, received_at}` (§G.3).
	ResolveOrg(ctx context.Context, batchID uuid.UUID, receivedAt time.Time) (db.TenantScope, error)
	ProcessBatch(ctx context.Context, s db.TenantScope, batchID uuid.UUID, receivedAt time.Time) (service.ProcessResult, error)
	MarkFailed(ctx context.Context, s db.TenantScope, batchID uuid.UUID, receivedAt time.Time, reason string)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ BatchProcessor = (*service.Service)(nil)

// NewProcessBatch builds the `ingest.process_batch` handler, replacing the
// registered stub. Wire it as `jobs.Handlers{IngestProcessBatch: …}`.
//
// ⭐ SAFELY RE-RUNNABLE, which is not optional: River is at-least-once, the
// rescuer re-queues anything a dead pod left `running`, and a redelivery of a
// batch that already committed must produce nothing at all. Three things make
// that true, and all three live below this handler rather than in it:
//
//  1. `ingest_dedup` already collapsed webhook replays onto ONE batch (§C.5), so
//     BatchID is a stable, oto-computed idempotency key.
//  2. ProcessBatch exits immediately on a batch that is `processed` or `failed`
//     (§G.4 step 1).
//  3. Every write underneath is idempotent by construction (§G.5) — the alert
//     upsert is ON CONFLICT, event appends dedupe through `alert_event_keys` — so
//     even a batch left `partial` by a dead worker can be replayed wholesale.
//
// The handler therefore holds no state and takes no lock. Correctness here is a
// property of the schema, not of the scheduler.
func NewProcessBatch(svc BatchProcessor, logger *slog.Logger) jobs.Handler[jobs.IngestProcessBatchArgs] {
	if logger == nil {
		logger = slog.Default()
	}

	return func(ctx context.Context, job *jobs.Job[jobs.IngestProcessBatchArgs]) error {
		lg := log.From(ctx)
		if lg == nil {
			lg = logger
		}
		lg = lg.With("batch_id", job.Args.BatchID)

		scope, err := svc.ResolveOrg(ctx, job.Args.BatchID, job.Args.ReceivedAt)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				// The batch aged out of its retention partition. There is nothing to
				// process and nothing to retry: the raw payload is gone by policy.
				// Succeeding here is honest — retrying thirteen times would not bring
				// a dropped partition back.
				lg.WarnContext(ctx, "ingest: batch has aged out of retention, skipping")
				return nil
			}
			return err
		}

		res, err := svc.ProcessBatch(ctx, scope, job.Args.BatchID, job.Args.ReceivedAt)
		if err != nil {
			return finish(ctx, lg, svc, scope, job, err)
		}

		if res.Skipped {
			lg.DebugContext(ctx, "ingest: batch already terminal, nothing to do",
				"status", res.FinalStat.String())
			return nil
		}

		lg.InfoContext(ctx, "ingest: batch processed",
			"observed", res.Observed, "rejected", res.Rejected)
		return nil
	}
}

// finish decides what a processing failure means.
//
// ⚠️ THE BATCH IS MARKED `failed` ONLY ON THE LAST ATTEMPT. Marking it earlier
// would make it non-resumable — ProcessBatch exits on a terminal status — and
// would throw away the retry budget §G.6 exists to spend. A transient Postgres
// error on attempt 1 of 13 must leave the batch exactly as it was: pending, with
// twelve attempts left and its payload safely on disk.
func finish(
	ctx context.Context, lg *slog.Logger, svc BatchProcessor, scope db.TenantScope,
	job *jobs.Job[jobs.IngestProcessBatchArgs], err error,
) error {
	if !job.LastAttempt() {
		lg.WarnContext(ctx, "ingest: batch processing failed, will retry",
			"attempt", job.Attempt, "max_attempts", job.MaxAttempts, "error", err)
		return err
	}

	lg.ErrorContext(ctx, "ingest: batch processing exhausted its retries", "error", err)
	svc.MarkFailed(ctx, scope, job.Args.BatchID, job.Args.ReceivedAt, err.Error())
	return err
}
