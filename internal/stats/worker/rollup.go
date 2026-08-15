package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/stats/service"
)

// Roller recomputes the hygiene rollup, satisfied by `*stats/service.Service`.
type Roller interface {
	Rollup(ctx context.Context, s db.TenantScope, day time.Time) (service.RollupResult, error)
}

// TenantLister is the tenant list in the two halves a fan-out needs: a bounded
// page of ids to enqueue for, and the lookup that turns the id a payload names
// into the scope the table authorises.
//
// The rollup is per-org because every repository method takes a `db.TenantScope`
// by construction, and a periodic tick has no request to derive one from.
type TenantLister interface {
	jobs.Tenants
}

// StatsRollup is `stats.rollup` (SPEC §G.3): recompute `alert_quality_daily`.
//
// ⭐ ADR 0014 — this is what keeps analytics off the event stream. The report at
// `GET /api/v1/stats/alert-quality` reads the rollup and nothing else, so until
// this job runs the report is honest and empty.
//
// ⛔ IT IS CONVERGENT, NOT CUMULATIVE. The service recomputes each day from the
// source tables and REPLACES the stored counters. Re-running the job for a day it
// has already done is a no-op in effect — which is the only safe shape under an
// at-least-once queue, and which is what makes a per-tenant job safe to
// re-deliver, to retry, and to overlap with the next tick's copy of itself.
//
// ⭐ THE PAYLOAD HAS TWO SHAPES, exactly as `source.reconcile`'s does (2d699d6).
// No org id is the FAN-OUT tick: it enqueues one rollup per tenant and does no
// rolling up itself. An org id is ONE TENANT'S rollup, with the whole of the
// kind's five-minute execution budget to do it in. It used to loop every tenant
// inside one execution, so the budget an operator sized for the work was actually
// being spent on the customer count.
//
// ⚠️ THE FAN-OUT COPIES ITS OWN DAY ONTO EVERY JOB IT QUEUES rather than letting
// each one re-read the clock. A fan-out that straddles midnight UTC would
// otherwise roll up two different days across one tenant list — the tenants at
// the start of the walk finalising yesterday while the ones at the end started on
// today, with nothing to say which had happened.
//
// ONE TENANT'S FAILURE MUST NOT COST THE OTHERS, and separate jobs are how that
// is true now rather than a promise a log line made. A broken org fails its own
// job, retries on its own periodic budget and dead-letters under its own payload;
// the others were never in the same execution to be stopped.
func StatsRollup(roll Roller, orgs TenantLister, enq db.Enqueuer, clk clock.Clock, log *slog.Logger) jobs.Handler[jobs.StatsRollupArgs] {
	if log == nil {
		log = slog.Default()
	}
	if clk == nil {
		clk = clock.New()
	}
	return func(ctx context.Context, job *jobs.Job[jobs.StatsRollupArgs]) error {
		if orgs == nil {
			return errs.New(errs.KindInternal, "stats_rollup_unwired",
				"stats.rollup cannot enumerate tenants")
		}
		// ParseRollupDay is applied on BOTH shapes and the result is what travels,
		// so the day a tenant's job carries is the day the tick resolved — never a
		// second, later reading of the clock.
		day := service.ParseRollupDay(job.Args.Day, clk.Now())

		if job.Args.IsFanOut() {
			out, err := jobs.FanOutTenants(ctx, jobs.KindStatsRollup, enq, orgs, log,
				job.Args.After, func(f jobs.TenantFanOut) db.JobArgs {
					return jobs.StatsRollupArgs{TenantFanOut: f, Day: day.Format(time.DateOnly)}
				})
			if err != nil {
				return err
			}
			if out.Enqueued > 0 {
				log.DebugContext(ctx, "stats.rollup: fan-out",
					slog.String("day", day.Format(time.DateOnly)),
					slog.Int("enqueued", out.Enqueued))
			}
			return nil
		}

		return jobs.ForTenant(ctx, jobs.KindStatsRollup, orgs, job.Args.OrgID,
			func(ctx context.Context, scope db.TenantScope) error {
				res, err := roll.Rollup(ctx, scope, day)
				if err != nil {
					return err
				}
				if res.Rows > 0 {
					log.InfoContext(ctx, "stats.rollup",
						slog.String("org_id", scope.OrgID().String()),
						slog.String("day", day.Format(time.DateOnly)),
						slog.Int("rows", res.Rows))
				}
				return nil
			})
	}
}
