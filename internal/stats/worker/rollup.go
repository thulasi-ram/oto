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

// TenantLister enumerates tenants. The rollup is per-org because every repository
// method takes a `db.TenantScope` by construction, and a periodic tick has no
// request to derive one from.
type TenantLister interface {
	Scopes(ctx context.Context) ([]db.TenantScope, error)
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
// at-least-once queue.
//
// ONE TENANT'S FAILURE MUST NOT COST THE OTHERS. A broken org logs and the sweep
// continues; the tick repeats in fifteen minutes, so "carry on" is strictly
// better than "abort", which would turn one tenant's problem into every tenant's
// stale report.
func StatsRollup(roll Roller, orgs TenantLister, clk clock.Clock, log *slog.Logger) jobs.Handler[jobs.StatsRollupArgs] {
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
		day := service.ParseRollupDay(job.Args.Day, clk.Now())

		scopes, err := orgs.Scopes(ctx)
		if err != nil {
			return err
		}
		rows, failed := 0, 0
		for _, scope := range scopes {
			res, err := roll.Rollup(ctx, scope, day)
			if err != nil {
				failed++
				log.ErrorContext(ctx, "stats.rollup failed for one tenant",
					slog.String("org_id", scope.OrgID().String()),
					slog.String("day", day.Format(time.DateOnly)),
					slog.String("error", err.Error()))
				continue
			}
			rows += res.Rows
		}
		if rows > 0 || failed > 0 {
			log.InfoContext(ctx, "stats.rollup",
				slog.String("day", day.Format(time.DateOnly)),
				slog.Int("orgs", len(scopes)),
				slog.Int("rows", rows),
				slog.Int("failed", failed))
		}
		return nil
	}
}
