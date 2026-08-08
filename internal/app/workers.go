package app

import (
	"context"
	"log/slog"

	enrichworker "github.com/thulasiram/oto/internal/enrichment/worker"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// handlers fills the SEAM `platform/jobs` publishes: one field per job kind in
// SPEC §G.3, each nil field registered as a stub that returns "not implemented".
//
// The seam is why the queue, the retry policy, the metrics and the schedule were
// all live and observable before any of this existed — and it is why the two
// kinds that still have NO implementation anywhere in the tree
// (`source.reconcile`, `silences.sync`, `stats.rollup`) stay visible as
// `oto_jobs_failed_total{kind=…}` rather than quietly vanishing. A stub that
// fails loudly is the honest state; a handler that returns nil would claim the
// work was done.
func (c *Container) handlers() jobs.Handlers {
	h := jobs.Handlers{
		// ingest.process_batch — THE ONLY WRITE PATH INTO `alerts` (§G.4).
		IngestProcessBatch: c.Ingestion.ProcessBatch,

		// enrich.run — the budgeted, two-phase pipeline.
		EnrichRun: enrichworker.EnrichRun(
			c.Enrichment,
			occurrenceScopes{pool: c.Pools.General},
		),

		// The periodic lifecycle and maintenance sweeps. Each is a per-tenant
		// fan-out because every repository method takes a TenantScope by
		// construction; the fan-out itself is the only thing that belongs here.
		OccurrenceReap:   c.reapOccurrences,
		GroupClose:       c.closeGroups,
		FlapScore:        c.scoreFlaps,
		PartitionsManage: c.managePartitions,
		RetentionPrune:   c.pruneRetention,
		CacheExpire:      c.expireCache,

		// ⛔ NOT IMPLEMENTED ANYWHERE IN THIS TREE, and deliberately left as the
		// loud stub rather than faked:
		//
		//   source.reconcile  (§G.8) — nothing produces Observations from the
		//                     Alertmanager v2 API. It is the ONLY producer of
		//                     `suppressed`, so a no-op handler would silently
		//                     make an entire alert state unreachable.
		//   silences.sync     — `silences/service` is read-only by ruling (R3)
		//                     and has no Sync; `silences/worker` is empty.
		//   stats.rollup      — `stats/service` reads `alert_quality_daily` but
		//                     nothing writes it; `stats/worker` is empty.
		SourceReconcile: nil,
		SilencesSync:    nil,
		StatsRollup:     nil,
	}

	// notification fills its own three fields (notify.evaluate, deliver.dispatch,
	// notify.unacked_reminder). It MUTATES the set rather than returning one, so
	// composing modules is one call each and nothing has to know the full list.
	if c.NotifyWorkers != nil {
		c.NotifyWorkers.Register(&h)
	}
	return h
}

// sweepLimit bounds one tick's work per tenant. A sweep that is not bounded is a
// sweep that becomes an outage the first time somebody has a bad night.
const sweepLimit = 500

// forEachOrg runs fn for every tenant, logging and CONTINUING on failure.
//
// One org's broken data must not stop the others being swept. The tick repeats
// within a minute or two anyway, which makes "carry on" strictly better than
// "abort": aborting converts one tenant's problem into every tenant's silence.
func (c *Container) forEachOrg(ctx context.Context, what string, fn func(context.Context, db.TenantScope) error) error {
	scopes, err := c.orgs.Scopes(ctx)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		if err := fn(ctx, scope); err != nil {
			c.Logger.ErrorContext(ctx, "sweep failed for one tenant",
				slog.String("sweep", what),
				slog.String("org_id", scope.OrgID().String()),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

// reapOccurrences is `occurrence.reap` (§B.4, T6).
//
// ⭐ THE REAPER GUARD lives in the service, not here: an occurrence whose
// AlertSource cannot be proven healthy is HELD. Losing sight of an alert is not
// the alert resolving, and `expired` is never `resolved`.
//
// The snooze expiry sweep rides the same tick. §B.8 auto-expires every snooze —
// there is no indefinite snooze — and it needs a clock somebody reads; there is
// no separate job kind for it, so it belongs on the one sweep that already runs
// every minute over the same tenants.
func (c *Container) reapOccurrences(ctx context.Context, _ *jobs.Job[jobs.OccurrenceReapArgs]) error {
	return c.forEachOrg(ctx, "occurrence.reap", func(ctx context.Context, scope db.TenantScope) error {
		res, err := c.Alerts.Reap(ctx, scope, sweepLimit)
		if err != nil {
			return err
		}
		if res.Expired > 0 || res.Held > 0 {
			c.Logger.InfoContext(ctx, "occurrence.reap",
				slog.String("org_id", scope.OrgID().String()),
				slog.Int("considered", res.Considered),
				slog.Int("expired", res.Expired),
				// Held is the §B.4 `source_degraded_holds` counter and is a
				// FEATURE, not noise: it is how an operator learns that oto is
				// deliberately saying nothing about a source it cannot see.
				slog.Int("held", res.Held))
		}

		snoozes, err := c.Alerts.ExpireSnoozes(ctx, scope, sweepLimit)
		if err != nil {
			return err
		}
		if snoozes.Expired > 0 {
			c.Logger.InfoContext(ctx, "snooze.expire",
				slog.String("org_id", scope.OrgID().String()),
				slog.Int("expired", snoozes.Expired))
		}
		return nil
	})
}

// closeGroups is `group.close` (§G.3): close generations whose members have all
// ended, and freeze their threads.
func (c *Container) closeGroups(ctx context.Context, _ *jobs.Job[jobs.GroupCloseArgs]) error {
	return c.forEachOrg(ctx, "group.close", func(ctx context.Context, scope db.TenantScope) error {
		res, err := c.Grouping.CloseIdle(ctx, scope, sweepLimit)
		if err != nil {
			return err
		}
		if res.Closed > 0 {
			c.Logger.InfoContext(ctx, "group.close",
				slog.String("org_id", scope.OrgID().String()),
				slog.Int("closed", res.Closed), slog.Int("held", res.Held))
		}
		return nil
	})
}

// scoreFlaps is `flap.score` (§B.6).
//
// Flapping is a VISIBLE state, never silent suppression: this marks alerts, it
// does not mute them.
func (c *Container) scoreFlaps(ctx context.Context, _ *jobs.Job[jobs.FlapScoreArgs]) error {
	return c.forEachOrg(ctx, "flap.score", func(ctx context.Context, scope db.TenantScope) error {
		res, err := c.Alerts.ScoreFlaps(ctx, scope, sweepLimit)
		if err != nil {
			return err
		}
		if res.FlappingStarted > 0 || res.FlappingEnded > 0 {
			c.Logger.InfoContext(ctx, "flap.score",
				slog.String("org_id", scope.OrgID().String()),
				slog.Int("scored", res.Scored),
				slog.Int("started", res.FlappingStarted),
				slog.Int("ended", res.FlappingEnded))
		}
		return nil
	})
}

// managePartitionsSQL is the §D.11 maintenance entry point, which the schema
// publishes as a function precisely so that nothing creates a partition by hand.
//
// RETENTION IS DROP PARTITION, ALWAYS. A DELETE on a thirteen-month event table
// is an outage. The function takes the advisory lock itself, so two pods cannot
// race the same DDL.
const managePartitionsSQL = `SELECT oto_partitions_manage($1, $2, $3)`

// managePartitions is `partitions.manage` (§D.11): create ahead, drop expired.
//
// It is global rather than per-tenant — partitions are a property of the table,
// not of a row — so it is the one sweep here with no org fan-out.
func (c *Container) managePartitions(ctx context.Context, _ *jobs.Job[jobs.PartitionsManageArgs]) error {
	r := c.Config.Retention
	_, err := c.Pools.General.Exec(ctx, managePartitionsSQL,
		int(r.RawPayloads.Hours()/24),
		int(r.Events.Hours()/24/30),
		int(r.UIEvents.Hours()),
	)
	if err != nil {
		return err
	}
	c.Logger.InfoContext(ctx, "partitions.manage: partitions created ahead and expired ones dropped")
	return nil
}

// pruneRetention is `retention.prune`: the rows that are NOT reclaimed by
// dropping a partition.
//
// The partitioned tables are handled by `partitions.manage`. What is left is the
// deliberately UNPARTITIONED idempotency siblings and the session table — small
// precisely because this runs.
func (c *Container) pruneRetention(ctx context.Context, _ *jobs.Job[jobs.RetentionPruneArgs]) error {
	// `ingest_dedup` guards webhook replay and must stay globally unique, which
	// is exactly why it cannot live in a partition and must be pruned by hand.
	dedup, err := c.Ingestion.Service.PruneDedup(ctx)
	if err != nil {
		return err
	}
	sessions, err := c.Identity.SweepExpiredSessions(ctx, sweepLimit)
	if err != nil {
		return err
	}
	if dedup > 0 || sessions > 0 {
		c.Logger.InfoContext(ctx, "retention.prune",
			slog.Int64("ingest_dedup", dedup), slog.Int64("sessions", sessions))
	}
	return nil
}

// expireCache is `cache.expire`: evict stale `enrichment_cache` entries.
//
// ⛔ The CACHE, never `enrichments`. Truncating the cache costs latency;
// truncating the provenanced record destroys the answer to "who computed this,
// and when".
func (c *Container) expireCache(ctx context.Context, _ *jobs.Job[jobs.CacheExpireArgs]) error {
	n, err := c.Enrichment.ExpireCache(ctx, 5_000)
	if err != nil {
		return err
	}
	if n > 0 {
		c.Logger.InfoContext(ctx, "cache.expire", slog.Int64("evicted", n))
	}
	return nil
}
