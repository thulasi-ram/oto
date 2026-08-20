package worker

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/platform/log"
)

// Runner is the narrow slice of the enrichment service this worker drives.
type Runner interface {
	Run(ctx context.Context, s db.TenantScope, req service.RunRequest) (service.RunResult, error)
}

// ScopeResolver turns a job payload into an authenticated tenant scope.
//
// A job has no principal — nobody is logged in at 3am when a webhook fires — so
// the scope must be derived from the row the job names. This is the ONE place
// enrichment does that, it is a port so the derivation stays with `identity`,
// and it is why no other code in this module ever calls db.NewTenantScope.
type ScopeResolver interface {
	// ScopeForCase returns the tenant that owns a case.
	ScopeForCase(ctx context.Context, caseID uuid.UUID) (db.TenantScope, error)
}

// EnrichRun is the `enrich.run` handler (SPEC §G.3), replacing the registered
// stub. Wire it into jobs.Handlers.EnrichRun from internal/app.
//
// IDEMPOTENCY, on (CaseID, Phase):
//
//   - Results upsert on enrichments_subject_uniq (subject_kind, subject_id,
//     enricher), so a re-run of a phase overwrites ITS OWN rows and can never
//     accumulate duplicates or double-count a failure.
//   - A result that is still fresh at the same enricher version is skipped
//     rather than recomputed, so a retry after a partial failure pays only for
//     what actually failed.
//   - The async pass's one coalesced notification is deduplicated downstream by
//     notifications_idem_uniq on the Case and the state_version (SPEC §C.7), so a
//     re-run at the same state version mints nothing new. ⚠️ The group used to
//     supply that version and no longer exists (git-bug `7570090`); until the Case
//     carries its own, the version is constant and this leg dedupes HARDER than
//     described — see the ⛔⛔ on `service.Loaded.StateVersion`.
//
// It is therefore safe to re-run at any time, in any order, concurrently with
// itself — which is the only assumption an at-least-once queue permits.
func EnrichRun(svc Runner, scopes ScopeResolver) jobs.Handler[jobs.EnrichRunArgs] {
	return func(ctx context.Context, job *jobs.Job[jobs.EnrichRunArgs]) error {
		args := job.Args
		if args.CaseID == uuid.Nil {
			// A payload naming no case can never succeed, so retrying it
			// twelve times is twelve wasted executions. Terminal by class.
			return errs.New(errs.KindValidation, "enrichment_job_no_case",
				"enrich.run requires a case_id")
		}

		phase, err := domain.ParsePhase(args.Phase)
		if err != nil {
			// An unknown phase is a payload this build does not understand. It
			// is permanent, not retryable: a newer pod may know what it means.
			return err
		}

		scope, err := scopes.ScopeForCase(ctx, args.CaseID)
		if err != nil {
			return err
		}

		res, err := svc.Run(ctx, scope, service.RunRequest{
			CaseID:    args.CaseID,
			Phase:     phase,
			Enrichers: args.Enrichers,
		})
		if err != nil {
			return err
		}

		log.From(ctx).DebugContext(ctx, "enrich.run complete",
			"case_id", args.CaseID,
			"phase", phase.String(),
			"results", len(res.Results),
			"succeeded", res.Succeeded(),
			"skipped", len(res.Skipped),
			"deferred", len(res.Deferred),
			"notified", res.Notified)

		// A phase in which every enricher failed is NOT a job failure. The
		// failures are recorded, provenanced and visible in the UI, and
		// retrying the whole phase would re-run the enrichers that succeeded
		// alongside the ones that did not. Enrichment is best-effort by
		// construction (CONTEXT.md §2): the alert has already been delivered.
		return nil
	}
}
