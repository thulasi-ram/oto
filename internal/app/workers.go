package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	enrichworker "github.com/thulasiram/oto/internal/enrichment/worker"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/idempotency"
	"github.com/thulasiram/oto/internal/platform/jobs"
	silencesworker "github.com/thulasiram/oto/internal/silences/worker"
	sourcesworker "github.com/thulasiram/oto/internal/sources/worker"
	statsworker "github.com/thulasiram/oto/internal/stats/worker"
)

// reconcileFanOutInterval is how often the fan-out tick looks for sources whose
// own `reconcile_interval_s` has elapsed. The tick only ENQUEUES; `ListDue`
// decides which sources are actually due.
//
// ⚠️ It matches `SourceReconcileArgs.InsertOpts`'s own 30-second uniqueness
// window, and it cannot usefully be shorter: River would collapse the extra ticks
// onto the same unique key. The consequence is worth knowing — a source
// configured below 30 s (the DDL floor is 10 s) is still reconciled at 30 s. That
// is a property of the job kind's uniqueness period in `platform/jobs`, not of
// this schedule, and it is the SPEC's own default interval.
const reconcileFanOutInterval = 30 * time.Second

// addSourcePeriodic installs the per-source schedule of SPEC §G.3.
//
// ⚠️ IT CANNOT LIVE IN `platform/jobs`, and that package says so: the payloads of
// `source.reconcile` and `silences.sync` name a SOURCE, so the schedule needs the
// source list, and the source list lives in a tenant-scoped table that only a
// service can read. So the periodic here carries an EMPTY payload — the fan-out
// tick — and the handler expands it into one job per due source.
//
// `silences.sync` gets no periodic of its own. It is enqueued by the same
// fan-out, and its args carry a 60-second uniqueness window, so being offered
// every 10 seconds collapses to one run a minute: the §G.3 schedule, enforced by
// the queue rather than by a second clock that could drift from it.
func (c *Container) addSourcePeriodic(registry *jobs.Registry) {
	if c.Reconciler == nil {
		return
	}
	registry.AddPeriodic(river.NewPeriodicJob(
		river.PeriodicInterval(reconcileFanOutInterval),
		func() (river.JobArgs, *river.InsertOpts) { return jobs.SourceReconcileArgs{}, nil },
		// RunOnStart: a deploy must not cost a reconcile interval of blindness.
		// The pass is idempotent, so running one extra costs one HTTP call.
		&river.PeriodicJobOpts{ID: jobs.KindSourceReconcile + ".fanout", RunOnStart: true},
	))
}

// handlers fills the SEAM `platform/jobs` publishes: one field per job kind in
// SPEC §G.3, each nil field registered as a stub that returns "not implemented".
//
// The seam is why the queue, the retry policy, the metrics and the schedule were
// all live and observable before any of the business logic existed. Every field
// is now filled: a stub that fails loudly was the honest state while a kind had
// no implementation, and `source.reconcile` in particular could never be left
// there — ADR 0006 makes it mandatory, and without it `suppressed` is an
// unreachable state.
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

		// ⭐ source.reconcile (§G.8, ADR 0006) — MANDATORY. Alertmanager's
		// MuteStage drops suppressed alerts before any webhook fires, so polling
		// API v2 is the ONLY way oto can learn that an alert was silenced. It is
		// also the recovery path for a webhook that was never delivered.
		//
		// A payload with no source id is the FAN-OUT tick; one with a source id is
		// a single pass. Both shapes go to the same handler because both are the
		// same job kind, and the kind is what the queue, the retry policy and the
		// metrics are keyed on.
		SourceReconcile: sourcesworker.SourceReconcile(c.Reconciler, c.Sources, c.Logger),

		// ⭐ slack.interaction (§H.8) — the Acknowledge button, off the request.
		//
		// Slack gives an endpoint three seconds before it tells the user "This app
		// is not responding". Resolving the tenant, checking the group and acking
		// every open member is several round trips, so the HTTP handler answers
		// 200 and enqueues, and this is where the work lands. It carries the
		// highest priority oto has, because a human is watching the card.
		SlackInteraction: c.applySlackInteraction,

		// silences.sync — the read-only mirror (R3). It carries the comment, the
		// creator and the expiry that let oto say WHY something is quiet, which is
		// the half of suppression the reconciler cannot see.
		SilencesSync: silencesworker.SilencesSync(c.Silences, c.Sources, c.Logger),

		// stats.rollup (ADR 0014) — what keeps the hygiene report off a scan of
		// the event stream, and therefore what makes Postgres-only viable.
		StatsRollup: statsworker.StatsRollup(c.Stats, c.orgs, c.Clock, c.Logger),
	}

	// notification fills its own three fields (notify.evaluate, deliver.dispatch,
	// notify.unacked_reminder). It MUTATES the set rather than returning one, so
	// composing modules is one call each and nothing has to know the full list.
	if c.NotifyWorkers != nil {
		c.NotifyWorkers.Register(&h)
	}
	return h
}

// applySlackInteraction is `slack.interaction` (§H.8).
//
// ⛔ IT IS THE ONE HANDLER HERE WITH NO ORG FAN-OUT AND NO TenantScope ARGUMENT,
// because the tenant is what the job resolves FIRST: an interaction payload
// names a Slack workspace and a conversation, never an org, so the scope is an
// OUTPUT of the work rather than an input to it. Everything after that
// resolution is scoped exactly like every other write in oto.
func (c *Container) applySlackInteraction(ctx context.Context, job *jobs.Job[jobs.SlackInteractionArgs]) error {
	if c.SlackInteractions == nil {
		// A deployment with the endpoint mounted and no consumer is the defect
		// this job exists to fix. Fail loudly rather than dropping the press.
		return jobs.ErrNotImplemented(jobs.KindSlackInteraction)
	}
	return c.SlackInteractions.Apply(ctx, job.Args)
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
//
// ⛔ IT DROPS AT THE MAXIMUM RETENTION ANY ORG ASKS FOR, NOT AT THE PROCESS
// DEFAULT (ADR 0024). A partition holds every tenant's rows, so a per-org
// retention cannot be honoured by dropping one — the only two honest choices are
// "shortest wins", which silently deletes data an org configured itself to keep,
// or "longest wins", which keeps data a shorter org would have let go. oto takes
// the second: **retention is a floor, never a ceiling.** An org that raises its
// window gets everything it asked for; an org that lowers its window may have its
// rows survive a while longer, which costs disk and destroys nothing.
//
// Until this existed the two `orgs.settings` retention keys were validated,
// rendered with an origin and enforced NOWHERE — a settings screen showing a
// number no reaper read. That is the specific failure this fold exists to close.
func (c *Container) managePartitions(ctx context.Context, _ *jobs.Job[jobs.PartitionsManageArgs]) error {
	rawDays, eventMonths := c.effectiveRetention(ctx)
	_, err := c.Pools.General.Exec(ctx, managePartitionsSQL,
		rawDays, eventMonths, int(c.Config.Retention.UIEvents.Hours()),
	)
	if err != nil {
		return err
	}
	c.Logger.InfoContext(ctx, "partitions.manage: partitions created ahead and expired ones dropped",
		slog.Int("raw_retention_days", rawDays),
		slog.Int("event_retention_months", eventMonths))
	return nil
}

// effectiveRetention is the widest window any tenant has asked for, floored at
// the deployment's own configured retention.
//
// ⛔ EVERY FAILURE WIDENS THE WINDOW RATHER THAN NARROWING IT. An unreadable org
// row, an org list that errors, a settings lookup that times out — none of them
// may make the dropper delete MORE than it would have. Reading a setting is not
// allowed to cost data, and the direction of the fallback is the whole guarantee:
// the worst outcome of a broken read is that a partition lives one hour longer
// and the next tick drops it.
//
// `ui_events` is deliberately not folded in. Its 24 hours is the SSE durable
// resume buffer (ADR 0010), a transport window rather than a record, and it is
// not an `orgs.settings` key.
func (c *Container) effectiveRetention(ctx context.Context) (rawDays, eventMonths int) {
	rawDays = int(c.Config.Retention.RawPayloads.Hours() / 24)
	eventMonths = int(c.Config.Retention.Events.Hours() / 24 / 30)

	if c.Identity == nil {
		return rawDays, eventMonths
	}
	scopes, err := c.orgs.Scopes(ctx)
	if err != nil {
		c.Logger.ErrorContext(ctx, "partitions.manage: could not list tenants, keeping the configured window",
			slog.String("error", err.Error()))
		return rawDays, eventMonths
	}
	for _, scope := range scopes {
		org, err := c.Identity.GetOrg(ctx, scope)
		if err != nil {
			c.Logger.ErrorContext(ctx, "partitions.manage: could not read one tenant's retention, keeping the wider window",
				slog.String("org_id", scope.OrgID().String()),
				slog.String("error", err.Error()))
			continue
		}
		// Settings are already the effective, clamped values (Org.Settings), so
		// there is no second copy of the bounds here.
		if d := int(org.Settings.RawRetention.Hours() / 24); d > rawDays {
			rawDays = d
		}
		if m := int(org.Settings.EventRetention.Hours() / 24 / 30); m > eventMonths {
			eventMonths = m
		}
	}
	return rawDays, eventMonths
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
	// `idempotency_claims` is the same kind of row for the same reason: a
	// client-supplied `Idempotency-Key` has no partition key to carry, so the
	// uniqueness that stops a retried create minting a second live credential has
	// to be global, so the table cannot be partitioned and ages out only here.
	// ⛔ THE HORIZON IS THE ONE THE CONTRACT PROMISES, so a claim that is still
	// inside `idempotency.RetentionWindow` must survive this sweep: deleting one
	// early re-opens the exact hole the table closes, silently.
	claims, err := c.Idempotency.Prune(ctx, c.Clock.Now().Add(-idempotency.RetentionWindow))
	if err != nil {
		return err
	}
	sessions, err := c.Identity.SweepExpiredSessions(ctx, sweepLimit)
	if err != nil {
		return err
	}
	finalised, disposed := c.sweepDrills(ctx)
	if dedup > 0 || claims > 0 || sessions > 0 || finalised > 0 || disposed > 0 {
		c.Logger.InfoContext(ctx, "retention.prune",
			slog.Int64("ingest_dedup", dedup), slog.Int64("idempotency_claims", claims),
			slog.Int64("sessions", sessions),
			slog.Int("drills_finalised", finalised), slog.Int("drills_disposed", disposed))
	}
	return nil
}

// sweepDrills settles abandoned delivery drills and deletes the synthetic signal
// rows of drills that settled long enough ago.
//
// ⭐⭐ IT BELONGS HERE, IN `retention.prune`, AND NOT IN `partitions.manage`.
// ADR 0024 divides retention in two: partitions dropped wholesale, and a short
// list of side tables pruned BY ROW because they cannot age out any other way.
// A drill's rows are the second kind. They live in tables ADR 0024 promises never
// to reap — and that promise is about the record of an UPSTREAM event, of which a
// drill is none: nothing fired, no cluster was involved, oto manufactured every
// byte to answer a question an operator asked by pressing a button. No partition
// is dropped here and no row recording something a cluster actually did is
// touched.
//
// ⛔ A FAILURE FOR ONE TENANT MUST NOT STOP THE OTHERS, and none of it may fail
// the job: `retention.prune` also prunes `ingest_dedup`, which guards webhook
// replay, and letting a drill cleanup error block that would trade a cosmetic
// problem for a correctness one.
func (c *Container) sweepDrills(ctx context.Context) (finalised, disposed int) {
	if c.Drills == nil {
		return 0, 0
	}
	scopes, err := c.orgs.Scopes(ctx)
	if err != nil {
		c.Logger.ErrorContext(ctx, "retention.prune: could not list tenants for the drill sweep",
			slog.String("error", err.Error()))
		return 0, 0
	}
	for _, scope := range scopes {
		f, d, serr := c.Drills.Sweep(ctx, scope, sweepLimit)
		if serr != nil {
			c.Logger.ErrorContext(ctx, "retention.prune: the drill sweep failed for one tenant",
				slog.String("org_id", scope.OrgID().String()), slog.String("error", serr.Error()))
			continue
		}
		finalised += f
		disposed += d
	}
	return finalised, disposed
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
