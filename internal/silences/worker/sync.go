package worker

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/silences/service"
)

// Syncer refreshes one source's mirror, satisfied by `*silences/service.Service`.
type Syncer interface {
	Sync(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (service.SyncResult, error)
}

// OrgResolver answers "which tenant owns this source".
//
// The payload names a source and no org (§G.3), so the scope every repository
// method requires has to be resolved here. `*sources/service.Service` satisfies
// it, through a port so this package never imports another module's internals.
type OrgResolver interface {
	OrgForSource(ctx context.Context, sourceID uuid.UUID) (db.TenantScope, error)
}

// SilencesSync is `silences.sync` (SPEC §G.3): refresh the READ-ONLY mirror of
// one Alertmanager's silences.
//
// ⛔ READ-ONLY IS ABOUT THE CLUSTER (R3). This job copies what Alertmanager
// already decided; there is no code path from oto into a silence write, and there
// will not be one in v1 — a bug in a silence write suppresses a real incident.
//
// ⭐ IT IS THE OTHER HALF OF THE SUPPRESSION ANSWER. The reconciler learns THAT an
// alert is suppressed; this learns the comment, the creator and the expiry, so
// oto can render "silenced by @ram until 14:00, because: maintenance window" —
// which is impossible from webhooks and is the visible product advantage ADR 0006
// buys.
//
// An unreachable upstream RETURNS AN ERROR here, unlike `source.reconcile`: this
// job records no health of its own, so the retry budget is the only thing that
// distinguishes a blip from an outage. The previous mirror stays in place either
// way — `mirrored_at` is what tells the UI how stale it is.
func SilencesSync(sync Syncer, orgs OrgResolver, log *slog.Logger) jobs.Handler[jobs.SilencesSyncArgs] {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, job *jobs.Job[jobs.SilencesSyncArgs]) error {
		if job.Args.SourceID == uuid.Nil {
			// The fan-out is `source.reconcile`'s (it already holds the bounded
			// due-source query). A payload with no source is a producer bug, and
			// guessing "every source" would turn one bad enqueue into a stampede.
			return errs.New(errs.KindValidation, "silences_sync_source_required",
				"silences.sync requires a source id")
		}
		if orgs == nil {
			return errs.New(errs.KindInternal, "silences_org_resolver_unwired",
				"silences.sync cannot resolve the tenant that owns this source")
		}

		scope, err := orgs.OrgForSource(ctx, job.Args.SourceID)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				// Deleted between the fan-out and the run. Nothing to do.
				return nil
			}
			return err
		}

		res, err := sync.Sync(ctx, scope, job.Args.SourceID)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return nil
			}
			return err
		}
		if res.Fetched > 0 || res.Skipped > 0 {
			log.InfoContext(ctx, "silences.sync",
				slog.String("source_id", res.SourceID.String()),
				slog.String("org_id", scope.OrgID().String()),
				slog.Int("fetched", res.Fetched),
				slog.Int("mirrored", res.Mirrored),
				slog.Int("skipped", res.Skipped))
		}
		return nil
	}
}
