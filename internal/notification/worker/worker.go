package worker

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// ScopeResolver derives the tenant that owns a job's subject.
//
// The scope comes from the SUBJECT, never from the payload: a job row is data,
// and data that decided its own authorisation would undo the whole tenancy
// boundary.
type ScopeResolver interface {
	ForCase(ctx context.Context, caseID uuid.UUID) (db.TenantScope, error)
	ForDelivery(ctx context.Context, deliveryID uuid.UUID) (db.TenantScope, error)
}

// Workers holds this module's job handlers.
//
// They are the thinnest possible layer: resolve the tenant, translate the
// payload, call the service, translate the error. Every decision worth arguing
// about lives in `service`, where it can be reasoned about without a queue.
type Workers struct {
	scopes   ScopeResolver
	notifier *service.NotificationService
	dispatch *service.DispatchService
	digests  *service.DigestService
	log      *slog.Logger
}

// Config is everything New needs.
type Config struct {
	Scopes   ScopeResolver
	Notifier *service.NotificationService
	Dispatch *service.DispatchService
	// Digests is the digest tick. REQUIRED, like the other three: a build where the
	// schema admits a `digest_window_s` and nothing ever evaluates it is a settings
	// field an operator can fill in and never hear from again, which is the specific
	// failure mode CONTEXT.md §6 calls a number no reader reads.
	Digests *service.DigestService
	Logger  *slog.Logger
}

// New builds the workers.
func New(cfg Config) (*Workers, error) {
	if cfg.Scopes == nil || cfg.Notifier == nil || cfg.Dispatch == nil || cfg.Digests == nil {
		return nil, errs.New(errs.KindInternal, "notification_worker_deps",
			"the notification workers need a scope resolver, the notification service, the dispatch service and the digest service")
	}
	w := &Workers{
		scopes: cfg.Scopes, notifier: cfg.Notifier,
		dispatch: cfg.Dispatch, digests: cfg.Digests,
		log: cfg.Logger,
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	return w, nil
}

// Register fills this module's fields on the shared handler seam.
//
// It MUTATES the caller's Handlers rather than returning one, so internal/app
// composes the modules by calling each one's Register in turn and nothing has to
// know the full set. A nil field is registered as a stub returning
// "not implemented", so the queue, the retries and the metrics were all live
// before this code existed — which is what made the seam worth having.
//
// The tenant list and the queue arrive as arguments because the digest tick is this
// module's one per-tenant periodic: both halves of its fan-out belong to
// internal/app — the same live-org pager and outbox every other per-tenant
// periodic is handed — and this module must never enumerate tenants for itself.
//
// ⛔ IT WAS TWO PERIODICS UNTIL git-bug bd0fb1d. `notify.unacked_reminder` was the
// other one, and the owner withdrew it: oto sends nothing unprompted.
func (w *Workers) Register(h *jobs.Handlers, orgs jobs.Tenants, enq db.Enqueuer) {
	if h == nil {
		return
	}
	h.NotifyEvaluate = w.NotifyEvaluate
	h.DeliverDispatch = w.DeliverDispatch
	h.NotifyDigest = w.NotifyDigest(orgs, enq)
}

// NotifyEvaluate is the `notify.evaluate` handler.
//
// IDEMPOTENCY: the §C.7 key, enforced by `notifications_idem_uniq`. A redelivery
// at the same state_version collides on that index and is swallowed; a
// redelivery at a NEWER one is a genuinely new fact and mints a new intent. The
// handler therefore needs no de-duplication of its own, which is the point of
// putting the key in the payload.
func (w *Workers) NotifyEvaluate(ctx context.Context, job *jobs.Job[jobs.NotifyEvaluateArgs]) error {
	args := job.Args

	reason := domain.Reason(args.Reason)
	if !reason.Valid() {
		// An unknown reason cannot be guessed at. It is permanent — it will still
		// be unknown on the thirteenth attempt — so it goes straight to the
		// dead-letter with its payload rather than burning a worker slot.
		return jobs.Permanent(errs.Validation("unknown_reason",
			"unknown notification reason",
			errs.Violation{Field: "reason", Code: "enum", Message: args.Reason}))
	}

	scope, err := w.scopes.ForCase(ctx, args.CaseID)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			// The Case was deleted between the enqueue and now. There is nothing to
			// notify about and nothing a retry could recover.
			return jobs.Permanent(err)
		}
		return err
	}

	res, err := w.notifier.Evaluate(ctx, scope, service.Intent{
		CaseID:       args.CaseID,
		Reason:       reason,
		StateVersion: args.StateVersion,
		AlertID:      args.AlertID,
		Actor:        args.Actor,
	})
	if err != nil {
		return classify(err)
	}

	w.log.DebugContext(ctx, "notification: evaluated",
		"case_id", args.CaseID, "reason", args.Reason,
		"notification_id", res.Notification.ID, "created", res.Created,
		"deliveries", res.Deliveries, "suppressed", string(res.Suppressed))
	return nil
}

// DeliverDispatch is the `deliver.dispatch` handler.
//
// IDEMPOTENCY: the optimistic-lock claim (§G.5). A duplicate worker's UPDATE
// matches zero rows and the handler exits.
//
// ORDERING: this is the ONLY job subject to the per-thread gate (§G.7). The gate
// answers with a snooze, which consumes no attempt — an item waiting its turn
// has not failed, and eroding its retry budget while it queues would kill
// exactly the messages a busy thread is trying hardest to deliver.
func (w *Workers) DeliverDispatch(ctx context.Context, job *jobs.Job[jobs.DeliverDispatchArgs]) error {
	args := job.Args
	if args.DeliveryID == uuid.Nil {
		return jobs.Permanent(errs.Validation("delivery_required",
			"a dispatch job must name a delivery",
			errs.Violation{Field: "delivery_id", Code: "required", Message: "a delivery id is required"}))
	}

	scope, err := w.scopes.ForDelivery(ctx, args.DeliveryID)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return jobs.Permanent(err)
		}
		return err
	}

	if err := w.dispatch.Dispatch(ctx, scope, args.DeliveryID); err != nil {
		return classify(err)
	}
	return nil
}

// NotifyDigest builds the `notify.digest` handler over the two shapes of
// jobs.TenantFanOut, exactly like the reminder above: a payload naming no org is the
// fan-out tick and only ENQUEUES — one job per live tenant, a continuation at the
// ceiling — and a payload naming an org is ONE tenant's sweep with the kind's whole
// execution timeout to itself.
//
// ⭐ THE TICK IS THE EVALUATOR, AND THE PAYLOAD CARRIES NO WINDOW. The window is
// arithmetic on the clock (`notification/domain.Digest.WindowStart`, aligned to the
// UTC day), so a job that named one would be a second, competing source of truth
// about a boundary that must be identical in every pod. The payload stays a zero
// value and the schedule stays declarative in `platform/jobs`; which windows are
// still owed is answered from the digests themselves.
//
// A window is covered EXACTLY ONCE across a restart, and not because this handler
// is careful: `(org_id, policy_id, digest_window_start)` is unique
// (`notif_digest_uniq`) and is also the §C.7 key, so a duplicate tick collides and
// the collision is read as "already covered".
//
// ⛔ IT MUST NEVER GAIN A TIME-OF-DAY PREDICATE (SCOPE-BOUNDARY §4.8). The window
// selects which facts a summary covers. A handler that could decide oto should stay
// silent until morning is quiet hours, and quiet hours need a timezone, an owner and
// a rota.
func (w *Workers) NotifyDigest(
	orgs jobs.Tenants, enq db.Enqueuer,
) jobs.Handler[jobs.NotifyDigestArgs] {
	return func(ctx context.Context, job *jobs.Job[jobs.NotifyDigestArgs]) error {
		if job.Args.IsFanOut() {
			out, err := jobs.FanOutTenants(ctx, jobs.KindNotifyDigest, enq, orgs, w.log,
				job.Args.After, func(f jobs.TenantFanOut) db.JobArgs {
					return jobs.NotifyDigestArgs{TenantFanOut: f}
				})
			if err != nil {
				return err
			}
			if out.Enqueued > 0 {
				w.log.DebugContext(ctx, "notification: digest fan-out",
					slog.Int("enqueued", out.Enqueued))
			}
			return nil
		}

		return jobs.ForTenant(ctx, jobs.KindNotifyDigest, orgs, job.Args.OrgID,
			func(ctx context.Context, scope db.TenantScope) error {
				sent, err := w.digests.SweepOrg(ctx, scope)
				if err != nil {
					return classify(err)
				}
				if sent > 0 {
					w.log.InfoContext(ctx, "notification: sent digests",
						slog.String("org_id", scope.OrgID().String()), slog.Int("count", sent))
				}
				return nil
			})
	}
}

// classify hands the service's error to the queue UNWRAPPED.
//
// It is deliberately a pass-through with a name. The temptation here is to add
// context — `fmt.Errorf("dispatch %s: %w", id, err)` — and it is the wrong
// instinct: `jobs.Classify` reads the error's own taxonomy to decide whether a
// retry could possibly help, and a snooze is not an error at all. Both survive
// wrapping today only because `errors.As` unwraps, and neither survives a
// wrapper that formats instead of wrapping. The job id, the kind and the attempt
// are already on every log line the runtime writes, so there is nothing to add.
func classify(err error) error { return err }
