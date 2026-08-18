package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
	enrichworker "github.com/thulasiram/oto/internal/enrichment/worker"
	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
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
// tick — and the handler expands it, now in TWO steps: one job per tenant
// (`jobs.TenantFanOut`, bounded and resumable), and inside each of those, one job
// per due source. The empty payload is the tick because a nil org id means the
// tenant walk and a nil source id means the source walk, so the schedule stays a
// zero value here and stays declarative.
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
			caseScopes{pool: c.Pools.General},
		),

		// The periodic lifecycle and maintenance sweeps. Each is a per-tenant
		// fan-out because every repository method takes a TenantScope by
		// construction; the fan-out itself is the only thing that belongs here.
		//
		// ⭐ "FAN-OUT" IS LITERAL SINCE 2d699d6. Four of these six carry
		// `jobs.TenantFanOut`, so a tick ENQUEUES one job per tenant rather than
		// looping the tenants inside its own execution. The two exceptions are
		// exceptions on their own merits and not by omission: `cache.expire` is a
		// bounded eviction over a table with no tenant to loop over, and
		// `partitions.manage` is global because a partition is a property of the
		// TABLE rather than of a row — and the window it drops at is a REDUCE over
		// every tenant rather than a map. See effectiveRetention for why that one
		// cannot take this shape.
		CaseReap:         c.reapCases,
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
		StatsRollup: statsworker.StatsRollup(c.Stats, c.orgs, c.enqueuer, c.Clock, c.Logger),
	}

	// notification fills its own three fields (notify.evaluate, deliver.dispatch,
	// notify.unacked_reminder). It MUTATES the set rather than returning one, so
	// composing modules is one call each and nothing has to know the full list.
	// The reminder is the module's one per-tenant periodic, so it takes the same
	// live-org pager and outbox every other fan-out here is handed — the tenant
	// list is this container's to give, never a module's to read for itself.
	if c.NotifyWorkers != nil {
		c.NotifyWorkers.Register(&h, c.orgs, c.enqueuer)
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

// keySweepLimit bounds one tick's `alert_event_keys` prune, and it is forty times
// `sweepLimit` for a reason that is arithmetic rather than taste.
//
// ADR 0024's own volume model is ~8 events per firing episode at 10 000 firings a
// day, and every lifecycle transition carries a dedupe key: ~80 000 new keys a
// day, ~3 400 an hour. `retention.prune` is HOURLY, so a 500-row tick would
// delete 12 000 a day against 80 000 written — a pruner that provably cannot keep
// up, which is a table that still grows forever with a job in front of it saying
// otherwise. 20 000 an hour is ~6× the modelled write rate: it keeps up at that
// volume and still drains a backlog (~480 000 a day) on an install that has been
// accumulating keys since the day it shipped.
//
// It stays BOUNDED because the first tick on such an install meets tens of
// millions of rows, and one unbounded DELETE over them is the outage this whole
// pattern exists to avoid. Deleting 20 000 four-column rows through the PRIMARY
// KEY is a sub-second statement on the maintenance queue.
const keySweepLimit = 20_000

// perTenantSweep runs whichever of the two shapes this execution is.
//
// ⭐ IT REPLACED `forEachOrg`, AND THE THING IT DELETED IS THE POINT. That helper
// ran fn for every tenant inside ONE execution, logging and carrying on past a
// failure — which was the right call while there was one execution to lose, and
// is the wrong shape entirely now: it spent one fixed timeout on a variable
// number of tenants, so the budget an operator had sized for the work was
// silently being spent on the customer count instead.
//
// ⭐ THE QUEUE NOW PROVIDES WHAT `forEachOrg`'s log line PROVIDED, and provides it
// better. "One org's broken data must not stop the others" used to mean
// swallowing the error and writing a line nobody alerts on; it now means the
// tenants are SEPARATE JOBS, so a tenant that fails fails alone, retries on its
// own periodic budget, and lands in the dead-letter table with its own kind and
// payload when it keeps failing. Nothing is swallowed and nothing else stops.
//
// ⚠️ The fan-out shape returns its error rather than logging it. A tick that
// could not read the tenant list or could not reach the queue has swept NOBODY,
// which is a job failure in the ordinary sense and deserves the retry.
func (c *Container) perTenantSweep(
	ctx context.Context,
	kind string,
	fo jobs.TenantFanOut,
	build func(jobs.TenantFanOut) db.JobArgs,
	sweep func(context.Context, db.TenantScope) error,
) error {
	if !fo.IsFanOut() {
		return jobs.ForTenant(ctx, kind, c.orgs, fo.OrgID, sweep)
	}
	out, err := jobs.FanOutTenants(ctx, kind, c.enqueuer, c.orgs, c.Logger, fo.After, build)
	if err != nil {
		return err
	}
	if out.Enqueued > 0 {
		c.Logger.DebugContext(ctx, "periodic fan-out",
			slog.String("kind", kind), slog.Int("enqueued", out.Enqueued))
	}
	return nil
}

// reapCases is `case.reap` (§B.4, T6).
//
// ⭐ THE REAPER GUARD lives in the service, not here: a case whose
// AlertSource cannot be proven healthy is HELD. Losing sight of an alert is not
// the alert resolving, and `expired` is never `resolved`.
//
// The snooze expiry sweep rides the same tick. §B.8 auto-expires every snooze —
// there is no indefinite snooze — and it needs a clock somebody reads; there is
// no separate job kind for it, so it belongs on the one sweep that already runs
// every minute over the same tenants.
func (c *Container) reapCases(ctx context.Context, job *jobs.Job[jobs.CaseReapArgs]) error {
	return c.perTenantSweep(ctx, jobs.KindCaseReap, job.Args.TenantFanOut,
		func(f jobs.TenantFanOut) db.JobArgs { return jobs.CaseReapArgs{TenantFanOut: f} },
		c.reapOneTenant)
}

// reapOneTenant is `case.reap` for exactly one org — the whole of a job
// execution and the whole of its two-minute budget.
func (c *Container) reapOneTenant(ctx context.Context, scope db.TenantScope) error {
	res, err := c.Alerts.Reap(ctx, scope, sweepLimit)
	if err != nil {
		return err
	}
	if res.Expired > 0 || res.Held > 0 {
		c.Logger.InfoContext(ctx, "case.reap",
			slog.String("org_id", scope.OrgID().String()),
			slog.Int("considered", res.Considered),
			slog.Int("expired", res.Expired),
			// Held is the §B.4 `source_degraded_holds` counter and is a FEATURE,
			// not noise: it is how an operator learns that oto is deliberately
			// saying nothing about a source it cannot see.
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
}

// closeGroups is `group.close` (§G.3): close generations whose members have all
// ended, and freeze their threads.
func (c *Container) closeGroups(ctx context.Context, job *jobs.Job[jobs.GroupCloseArgs]) error {
	return c.perTenantSweep(ctx, jobs.KindGroupClose, job.Args.TenantFanOut,
		func(f jobs.TenantFanOut) db.JobArgs { return jobs.GroupCloseArgs{TenantFanOut: f} },
		func(ctx context.Context, scope db.TenantScope) error {
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
func (c *Container) scoreFlaps(ctx context.Context, job *jobs.Job[jobs.FlapScoreArgs]) error {
	return c.perTenantSweep(ctx, jobs.KindFlapScore, job.Args.TenantFanOut,
		func(f jobs.TenantFanOut) db.JobArgs { return jobs.FlapScoreArgs{TenantFanOut: f} },
		func(ctx context.Context, scope db.TenantScope) error {
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
// ⛔⛔ IT NEVER BECAME ONE JOB PER TENANT, AND THAT IS A DECISION RATHER THAN AN
// OVERSIGHT (2d699d6). Every other periodic in this file did. This one cannot,
// because it is not a MAP — it is a REDUCE. There is no per-tenant unit of work
// to enqueue: the fold produces ONE pair of numbers that drive ONE
// `oto_partitions_manage` call, and a partition holds every tenant's rows, so
// there is nothing for a per-tenant job to do with a per-tenant answer. Forcing
// it into the same shape would mean N jobs writing partial maxima somewhere and
// an N+1'th deciding they had all landed — inventing a distributed accumulator to
// replace a fold over a settings column.
//
// ⭐ THE REDUCE IS EVALUATED INSIDE `identity`, NOT HERE. The obvious bounded
// replacement for the tenant walk this used to be — one `max()` over
// `orgs.settings` — is WRONG AS WRITTEN: `identity.Service` overlays the
// deployment's Declarative onto EVERY org read and RECOMPUTES the effective
// settings from it (`Org.WithDeclarative`), and the declarative value BEATS the
// org's own, so an aggregate over the raw JSONB column computes a maximum over
// numbers nobody is using. `identity/service.MaxRetention` is the reduce
// evaluated where the overlay is applied: it walks the settings rows in bounded
// keyset pages, applies `WithDeclarative` per row, and returns ONE exact
// maximum — bounded per query and exact for the population, which is the pair
// the old one-GetOrg-per-tenant walk could never be. A truncated MAP is a
// deferral; a truncated REDUCE is a WRONG ANSWER; MaxRetention's own contract
// is exact-or-error for precisely that reason.
//
// ⛔ EVERY FAILURE WIDENS THE WINDOW RATHER THAN NARROWING IT. An unreadable
// settings walk, a read that times out — none of them may make the dropper
// delete MORE than it would have. Reading a setting is not allowed to cost
// data, and the direction of the fallback is the whole guarantee: the worst
// outcome of a broken read is that a partition lives one hour longer and the
// next tick drops it.
//
// `ui_events` is deliberately not folded in. Its 24 hours is the SSE durable
// resume buffer (ADR 0010), a transport window rather than a record, and it is
// not an `orgs.settings` key.
func (c *Container) effectiveRetention(ctx context.Context) (rawDays, eventMonths int) {
	rawDays = int(c.Config.Retention.RawPayloads.Hours() / 24)
	eventMonths = int(c.Config.Retention.Events.Hours() / 24 / 30)

	// No identity service is not a broken read: this process serves no tenant
	// settings at all, so there is no wider window it could have missed.
	if c.Identity == nil {
		return rawDays, eventMonths
	}
	return foldRetention(ctx, c.Logger, c.Identity, rawDays, eventMonths)
}

// foldRetention floors the tenants' ceiling at the deployment's own, lifted out
// of the container so that the one read it makes arrives as the
// `retentionCeiling` port (`ports.go`) rather than as the identity service —
// which is what makes a FAILING read something a test can arrange.
//
// ⛔ ITS FAILURE PATH WIDENS TO THE SETTINGS CEILING, AND THAT IS THE WHOLE OF
// THIS FUNCTION'S CORRECTNESS. The fold used to run here — the tenant list,
// then one GetOrg per tenant — and BOTH of its failure paths left it NARROWER
// than the truth in exactly the case that costs data:
//
//   - AN UNREADABLE TENANT LIST returned the CONFIG FLOOR — 30 days as shipped —
//     because the per-org loop never ran. An org configured to the 365-day bound
//     lost 335 days of raw payloads to one settings read that timed out.
//   - AN UNREADABLE ORG ROW `continue`d and kept whatever maximum the fold had
//     already accumulated, which is the right answer for every org EXCEPT the one
//     asking for the longest window — the only org whose absence changes a
//     maximum. There is no second pass and no per-org retry: the tick drops on the
//     number this returns.
//
// `identity/service.MaxRetention` collapses both into the ONE observable
// failure handled here — its walk is exact or it is an error — and the error
// widens instead of narrowing. The direction is the guarantee:
// `oto_partitions_manage` DROPS PARTITIONS, cold-storage export is scoped
// rather than built (ADR 0024), so a window that came back too narrow deletes
// rows nothing can bring back, while one that came back too wide costs an hour
// of disk and the next tick drops them anyway.
func foldRetention(
	ctx context.Context,
	log *slog.Logger,
	settings retentionCeiling,
	rawDays, eventMonths int,
) (int, int) {
	raw, event, err := settings.MaxRetention(ctx)
	if err != nil {
		log.ErrorContext(ctx, "partitions.manage: could not read the tenants' retention, widening to the settings ceiling",
			slog.String("error", err.Error()))
		return widenToSettingsCeiling(rawDays, eventMonths)
	}
	// The answer is already the effective, clamped maximum (Org.Settings via
	// WithDeclarative), so there is no second copy of the bounds here.
	if d := int(raw.Hours() / 24); d > rawDays {
		rawDays = d
	}
	if m := int(event.Hours() / 24 / 30); m > eventMonths {
		eventMonths = m
	}
	return rawDays, eventMonths
}

// widenToSettingsCeiling raises a fold to the widest window POLICY lets any org
// ask for: the upper bound of the two `orgs.settings` retention keys
// (`identity/domain.settingBounds`).
//
// ⭐ IT IS THE ONLY HONEST FALLBACK FOR A READ THAT FAILED, because it is the one
// number reachable WITHOUT the read that cannot be narrower than the answer the
// read would have given. `UpdateOrgSettings` clamps every org to these bounds, so
// no org can be asking for more than this — a fold that jumps here has lost
// precision and nothing else.
//
// ⛔ IT WIDENS, IT NEVER ASSIGNS. A bound this table does not carry, or a
// deployment whose own `OTO_RETENTION_*` is already past it, must leave the window
// exactly where it was: taking the ceiling unconditionally would let a missing
// bound (`ok == false`, so `Max` is zero) hand `oto_partitions_manage` a retention
// of zero days, which is every partition, dropped.
func widenToSettingsCeiling(rawDays, eventMonths int) (int, int) {
	if b, ok := identitydomain.Bounds(identitydomain.KeyRawRetention); ok && b.Max > rawDays {
		rawDays = b.Max
	}
	if b, ok := identitydomain.Bounds(identitydomain.KeyEventRetention); ok && b.Max > eventMonths {
		eventMonths = b.Max
	}
	return rawDays, eventMonths
}

// dedupeKeyHorizon is how long a claimed C.8 key survives: the wider of
// `alertsdomain.DedupeKeyRetention` and the raw-payload window `partitions.manage`
// actually drops on.
//
// ⭐ THE TWO HALVES ARE TWO DIFFERENT FAILURES, and taking either number alone
// re-opens one of them.
//
//   - TOO SHORT and SPEC acceptance criterion 36 breaks SILENTLY. A stored
//     `ingest_batch` is replayable only while its event keys are still claimed:
//     replay it after the keys are gone and `AppendBatch` appends the timeline a
//     SECOND time. Every org's raw partitions are kept to the LONGEST window any
//     tenant asked for (ADR 0024, "retention is a floor, never a ceiling"), so the
//     keys have to be kept that long too or the two jobs disagree about what is
//     still replayable. Reading the window from the same `effectiveRetention` the
//     dropper reads is what makes the disagreement impossible rather than unlikely.
//   - TOO SHORT THE OTHER WAY: the DEPLOYMENT's `OTO_RETENTION_RAW_PAYLOADS` has
//     no lower bound, and an operator who sets it under 720h must not thereby
//     unclaim the keys of episodes that are still open — the reconciler re-applies
//     transitions, and this table is the only thing that stops a re-applied chunk
//     writing the timeline twice. That is what the floor is for, and it is the
//     number the schema comment, SPEC §D.4 and `tuning.DefaultRawRetention` have
//     all named all along. Note it is NOT `raw_retention_days` that reaches the
//     floor: `effectiveRetention` starts the fold at `Config.Retention.RawPayloads`
//     and only ever widens, so a per-org setting of 1 day cannot pull the window
//     below the deployment's own, which ships as 30 days.
//
// It follows `effectiveRetention`'s own rule that every failure WIDENS the window:
// the fold cannot return less than the floor, so the worst a broken settings read
// can do is keep a key an hour longer.
func (c *Container) dedupeKeyHorizon(ctx context.Context) time.Duration {
	rawDays, _ := c.effectiveRetention(ctx)
	return dedupeKeyHorizonOf(rawDays)
}

// rawPartitionGrainDays is the grain `partitions.manage` drops raw payloads on:
// `ingest_batches` and `ingest_rejections` are DAILY partitions
// (00005_partitions.sql, `oto_partitions_manage`).
const rawPartitionGrainDays = 1

// dedupeKeyHorizonOf is the fold itself, kept separate from the settings read so
// the rule can be asserted as arithmetic rather than through a container.
//
// ⛔ THE EXTRA GRAIN IS NOT A SAFETY MARGIN — IT IS THE PARTITION-DROP RULE, and
// without it the horizon is SHORTER than the payload window it is meant to
// outlive. `oto_drop_partitions_before` (00005_partitions.sql:146) drops a
// partition only when its WHOLE range is past the cutoff — `CONTINUE WHEN v_hi >
// p_cutoff` — so a payload landing at any time on day D lives until D+rawDays+1,
// not D+rawDays. A key, by contrast, dies exactly at `created_at + horizon`. Take
// the raw `rawDays` and a batch received at 00:05 on day D and left `partial`
// (which `Status.Resumable` includes) has its key swept at D+rawDays 00:05 while
// its payload survives another 23h55m: replaying it from the failed-batch feed in
// that window appends every event a SECOND time, sends duplicate Slack, and
// reports success with `written>0`. Covering the grain closes that window to
// zero, which is exactly what SPEC acceptance criterion 36 asks for.
func dedupeKeyHorizonOf(rawDays int) time.Duration {
	horizon := time.Duration(rawDays+rawPartitionGrainDays) * 24 * time.Hour
	if horizon < alertsdomain.DedupeKeyRetention {
		return alertsdomain.DedupeKeyRetention
	}
	return horizon
}

// pruneRetention is `retention.prune`: the rows that are NOT reclaimed by
// dropping a partition.
//
// The partitioned tables are handled by `partitions.manage`. What is left is the
// deliberately UNPARTITIONED idempotency siblings and the session table — small
// precisely because this runs.
//
// ⭐⭐ ITS TWO SHAPES DO DIFFERENT WORK, WHICH IS THE ONLY HONEST SPLIT OF THIS
// BODY (2d699d6). The job interleaved three GLOBAL prunes with a PER-ORG drill
// sweep, and neither half is the other's shape: `ingest_dedup`,
// `idempotency_claims` and `alert_event_keys` are globally-unique tables that
// exist precisely because they cannot be partitioned or scoped, so "one job per
// tenant" has nothing to say about them, while the drill sweep is per-org like
// every other sweep in this file. So the FAN-OUT shape keeps the global prunes —
// they are genuinely one execution's worth of work, bounded by `keySweepLimit`
// and `sweepLimit` rather than by tenant count — and hands only the drill sweep
// to the per-tenant fan-out. The alternative the ticket also allowed, a second
// job kind for the drills, would have bought a new queue, a new retry policy and
// a new metric to express "the same hourly sweep, still".
//
// ⛔ THE GLOBAL PRUNES RUN ON THE TICK ALONE — NOT ON A TENANT'S JOB, AND NOT ON
// A CONTINUATION. Running them per tenant would be the same global DELETE
// executed N times an hour: convergent, so not a correctness bug, and a pure
// waste that grows with the customer count, which is the defect this whole change
// is about. Running them again on each CONTINUATION would be the same waste
// wearing a cursor — and `dedupeKeyHorizon` below folds every tenant's settings,
// so a continuation that redid it would re-pay the most expensive read in the job
// once per page of the tenant table.
func (c *Container) pruneRetention(ctx context.Context, job *jobs.Job[jobs.RetentionPruneArgs]) error {
	switch {
	case !job.Args.IsFanOut():
		return jobs.ForTenant(ctx, jobs.KindRetentionPrune, c.orgs, job.Args.OrgID, c.sweepTenantDrills)
	case job.Args.After == uuid.Nil:
		if err := c.pruneGlobal(ctx); err != nil {
			return err
		}
	}

	// ⛔ THE DRILL FAN-OUT IS AFTER THE GLOBAL PRUNES AND ITS FAILURE IS RETURNED.
	// After, because those prunes guard webhook replay and must not be skipped by
	// a tenant-list problem — the old body made the same call by swallowing every
	// drill error for the same reason. Returned, because there is nothing left
	// after it to protect: the prunes are committed, and a fan-out that could not
	// reach the queue swept no tenant at all, which is worth a retry rather than a
	// log line.
	out, err := jobs.FanOutTenants(ctx, jobs.KindRetentionPrune, c.enqueuer, c.orgs, c.Logger,
		job.Args.After, func(f jobs.TenantFanOut) db.JobArgs {
			return jobs.RetentionPruneArgs{TenantFanOut: f}
		})
	if err != nil {
		return err
	}
	if out.Enqueued > 0 {
		c.Logger.DebugContext(ctx, "periodic fan-out",
			slog.String("kind", jobs.KindRetentionPrune), slog.Int("enqueued", out.Enqueued))
	}
	return nil
}

// pruneGlobal is the half of `retention.prune` that has no tenant: the
// deliberately unpartitioned idempotency siblings and the session table.
func (c *Container) pruneGlobal(ctx context.Context) error {
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
	// `alert_event_keys` is the THIRD unpartitioned idempotency sibling, and until
	// this line it was the one nothing swept — its own `alert_event_keys_prune_idx`
	// and its own table comment have named a 30-day pruner since 00007, and the
	// index was therefore an unbounded write cost serving a query nobody had
	// written (ADR 0024, open defect 1).
	eventKeys, err := c.Alerts.PruneEventKeys(ctx, c.Clock.Now().Add(-c.dedupeKeyHorizon(ctx)), keySweepLimit)
	if err != nil {
		return err
	}
	sessions, err := c.Identity.SweepExpiredSessions(ctx, sweepLimit)
	if err != nil {
		return err
	}
	if dedup > 0 || claims > 0 || eventKeys > 0 || sessions > 0 {
		c.Logger.InfoContext(ctx, "retention.prune",
			slog.Int64("ingest_dedup", dedup), slog.Int64("idempotency_claims", claims),
			slog.Int64("alert_event_keys", eventKeys), slog.Int64("sessions", sessions))
	}
	return nil
}

// sweepTenantDrills settles one tenant's abandoned delivery drills and deletes
// the synthetic signal rows of the drills that settled long enough ago.
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
// ⛔ A FAILURE FOR ONE TENANT MUST NOT STOP THE OTHERS, and it no longer needs a
// swallowed error to achieve that. This used to be a loop over every tenant
// inside the same execution as the global prunes, so a drill error had to be
// logged and stepped over: letting it fail the job would have blocked
// `ingest_dedup`'s prune, which guards webhook replay, trading a cosmetic problem
// for a correctness one. Now each tenant is its OWN job, arriving after the
// global prunes have already committed, so the error can simply be RETURNED — the
// tenant retries on the periodic budget, nobody else is affected, and a tenant
// that keeps failing shows up in the dead-letter table instead of in a log line
// nobody reads.
//
// ⚠️ A DEPARTED TENANT'S DRILL ROWS ARE STILL NEVER SWEPT, AND THAT IS STILL AN
// ACCEPTED GAP (be3d314, 2d699d6). The tenant list filters `deleted_at IS NULL`
// and `LiveScope` applies the same filter on the way in, so a soft-deleted org
// gets no drill sweep from either end. Its open drills are never finalised and
// `Dispose` — by its own comment "the ONLY function in oto that deletes a signal
// row" — never runs for them, so the alert, group, case, thread and
// notification rows a drill MANUFACTURED persist indefinitely: ADR 0024 lists
// every one of those tables as never reaped by anything at any setting, and the
// one exception, `alert_events`, is dropped by `partitions.manage` on its own
// schedule. Nothing else will reach them. This is recorded as a known consequence
// rather than fixed here, because the fix is a decision about what a departing
// tenant's manufactured rows are FOR, not a scheduling change.
func (c *Container) sweepTenantDrills(ctx context.Context, scope db.TenantScope) error {
	if c.Drills == nil {
		return nil
	}
	finalised, disposed, err := c.Drills.Sweep(ctx, scope, sweepLimit)
	if err != nil {
		return err
	}
	if finalised > 0 || disposed > 0 {
		c.Logger.InfoContext(ctx, "retention.prune: drill sweep",
			slog.String("org_id", scope.OrgID().String()),
			slog.Int("drills_finalised", finalised), slog.Int("drills_disposed", disposed))
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
