package worker

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// Reconciler is the port this handler drives, satisfied by
// `*sources/service.Reconciler`.
type Reconciler interface {
	Reconcile(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (domain.ReconcileResult, error)
	FanOut(ctx context.Context, fo jobs.TenantFanOut) (int, error)
}

// OrgResolver answers "which tenant owns this source".
//
// The job payload names a source and NOT an org, because a source id is globally
// unique and an org id in a payload is one more thing that can be forged or go
// stale. Every repository method needs a `db.TenantScope`, so the worker resolves
// one here.
type OrgResolver interface {
	OrgForSource(ctx context.Context, sourceID uuid.UUID) (db.TenantScope, error)
}

// SourceReconcile is `source.reconcile` (SPEC §G.3, §G.8) — MANDATORY (ADR 0006).
//
// ⭐ THE PAYLOAD HAS THREE SHAPES, ON PURPOSE.
//
//   - No source id and no org id → this is the periodic FAN-OUT tick. It pages the
//     tenants and enqueues one of these per tenant, plus a continuation when it
//     stopped at `jobs.TenantFanOutLimit`. The periodic schedule in
//     `platform/jobs` cannot carry a source list, and putting the list in the
//     scheduler would mean restarting the process to pick up a new Alertmanager.
//   - No source id but an org id → ONE TENANT'S fan-out: its due sources, and one
//     pass plus one silences sync enqueued for each.
//   - A source id → this is ONE PASS against that source.
//
// ⭐ THE FIRST TWO USED TO BE ONE SHAPE, AND SPLITTING THEM FINISHES WHAT 2d699d6
// STARTED — that commit converted five periodics and named this kind as the one it
// left. The tick walked every tenant inside this one execution, under this kind's
// single sixty-second timeout; now the walk is bounded, the remainder rides a
// continuation, and each tenant's fan-out is a job with a budget of its own.
//
// ⛔ THE SOURCE ID IS READ FIRST, AND THE ORDER OF THAT `if` IS LOAD-BEARING.
// Reading the tenant half first would make every per-source pass look like a
// fan-out tick and re-enqueue the whole tenant list — a job that multiplies itself
// by the customer count, every thirty seconds, while doing none of the reconciling
// it was queued for. `TestASourceIDDispatchesThePassAndNotTheFanOut` is what holds
// the order in place, because nothing about swapping the branches fails to compile.
//
// ⛔ AN UNREACHABLE SOURCE IS NOT A JOB FAILURE. `Reconcile` returns a result with
// `ok: false` and has already recorded the failure in `source_health`, where three
// consecutive failures mark the source `unreachable` and BLOCK THE REAPER (§B.4).
// Returning an error here instead would spend the retry budget re-hammering an
// upstream that is down and would tell an operator nothing `source_health` does
// not already say. A returned error therefore means only "oto could not run the
// pass", which is exactly what deserves a retry.
func SourceReconcile(rec Reconciler, orgs OrgResolver, log *slog.Logger) jobs.Handler[jobs.SourceReconcileArgs] {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, job *jobs.Job[jobs.SourceReconcileArgs]) error {
		// The source id is read FIRST, so a payload that somehow carried both is one
		// pass and the org id in it is ignored. A payload is data, never authority.
		if job.Args.SourceID == uuid.Nil {
			n, err := rec.FanOut(ctx, job.Args.TenantFanOut)
			if err != nil {
				return err
			}
			if n > 0 {
				log.DebugContext(ctx, "source.reconcile: fan-out", slog.Int("enqueued", n))
			}
			return nil
		}

		if orgs == nil {
			return errs.New(errs.KindInternal, "sources_org_resolver_unwired",
				"source.reconcile cannot resolve the tenant that owns this source")
		}
		scope, err := orgs.OrgForSource(ctx, job.Args.SourceID)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				// The source was deleted between the fan-out and the pass. Nothing
				// to do and nothing to retry.
				return nil
			}
			return err
		}

		res, err := rec.Reconcile(ctx, scope, job.Args.SourceID)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return nil
			}
			return err
		}

		log.InfoContext(ctx, "source.reconcile",
			slog.String("source_id", res.SourceID.String()),
			slog.String("org_id", scope.OrgID().String()),
			slog.Bool("ok", res.OK),
			slog.Int("observed", res.Observed),
			// The ONLY place suppression can be observed at all (ADR 0006).
			slog.Int("suppressed", res.SuppressedObserved),
			slog.Int("recovered", res.Recovered),
			// Candidates for expiry ONLY. The reaper still applies its grace and
			// still refuses to run while the source is not healthy.
			slog.Int("missing_upstream", res.MissingUpstream),
			// The canary for every correctness bug in the system (§G.8.4).
			slog.Int("divergence", res.DivergenceCount),
			slog.String("error", res.Error))
		return nil
	}
}
