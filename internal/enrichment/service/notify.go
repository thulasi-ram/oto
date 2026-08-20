package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// ReasonEnriched is the §H.6 notification reason for "oto now knows more about
// this than it did when it first told you".
const ReasonEnriched = "enriched"

// EnrichedDebounce is how long the queue is asked to collapse identical
// `enriched` evaluations (SPEC §B.3, transition T11: "debounced 10s").
//
// It is what turns a burst of slow enrichers finishing within a few hundred
// milliseconds of each other into ONE amended card, and it is why the pipeline
// can afford to record every result honestly rather than batching in-process.
const EnrichedDebounce = 10 * time.Second

// QueueNotifier is the production Notifier: it enqueues `notify.evaluate` and
// lets the `notification` module decide what, if anything, to say.
//
// The queue is the seam, exactly as it is between `alerts` and `notification`
// (SPEC §I.1). Enrichment therefore has no compile-time dependency on the
// notification module at all — it states a fact on a queue and stops caring.
type QueueNotifier struct {
	enqueuer db.Enqueuer
	debounce time.Duration
}

// QueueNotifier satisfies the port.
var _ Notifier = (*QueueNotifier)(nil)

// NewQueueNotifier builds the production notifier.
func NewQueueNotifier(e db.Enqueuer) *QueueNotifier {
	return &QueueNotifier{enqueuer: e, debounce: EnrichedDebounce}
}

// WithDebounce overrides the coalescing window.
func (n *QueueNotifier) WithDebounce(d time.Duration) *QueueNotifier {
	if d > 0 {
		n.debounce = d
	}
	return n
}

// NotifyEnriched enqueues exactly one `notify.evaluate(reason=enriched)`.
//
// The uniqueness window is a CONVENIENCE, not the correctness mechanism:
// idempotency is owned by `notifications_idem_uniq` on
// (org_id, idempotency_key), which is derived from the Case and the state_version
// (SPEC §C.7). Two evaluations at the same state version collide on that index
// and the second is swallowed, whether or not the queue collapsed them first.
//
// ⚠️ AND THAT LAST SENTENCE IS CURRENTLY TOO STRONG, WHICH IS WHY IT IS FLAGGED
// HERE RATHER THAN LEFT TO READ AS TRUE. The version the group used to supply
// went with the group (git-bug `7570090`) and nothing replaces it yet, so every
// `enriched` evaluation for a Case arrives at version 0 and collides with the
// FIRST one forever — not merely with its own duplicates. See the ⛔⛔ on
// `Loaded.StateVersion`; the debounce below is doing more work than it should
// have to until that is answered.
func (n *QueueNotifier) NotifyEnriched(ctx context.Context, _ db.TenantScope, notice EnrichedNotice) error {
	if n.enqueuer == nil {
		return errs.New(errs.KindInternal, "enrichment_no_enqueuer",
			"the enriched notifier was built without a queue")
	}

	args := jobs.NotifyEvaluateArgs{
		CaseID:       notice.CaseID,
		Reason:       ReasonEnriched,
		StateVersion: notice.StateVersion,
		Actor:        "enricher",
	}
	// ⛔ THE `if notice.CaseID != uuid.Nil` BLOCK THAT TOOK ITS ADDRESS IS DELETED
	// (git-bug `7570090`). `CaseID` was an OPTIONAL `*uuid.UUID` narrowing of the
	// group; it is the required subject now, so a nil case is not "unnarrowed", it
	// is unsendable — and the pipeline refuses to build a notice without one rather
	// than letting an evaluation run against the zero UUID.
	if notice.AlertID != uuid.Nil {
		// AlertID stays optional and stays a pointer, because it is the FOCUS and
		// not the subject: `enriched` is a fact about the whole Case and names an
		// alert only when the enrichment happened to be about one.
		alertID := notice.AlertID
		args.AlertID = &alertID
	}

	_, err := n.enqueuer.Enqueue(ctx, args, db.WithUniquePeriod(n.debounce))
	return err
}

// ReasonFired is the §H.6 Reason for a first notification. It is named here
// because the inline pass RELEASES one; it never mints one.
const ReasonFired = "fired"

// NotifyPreNotificationReady enqueues the `fired` evaluation `alerts` deferred.
//
// It is enqueued with NO delay and NO uniqueness window: the correctness
// mechanism is `notifications_idem_uniq`, not the queue, and asking the queue to
// collapse this onto the backstop it is trying to overtake would defeat the
// point. A duplicate evaluation is cheap and idempotent; a card that waits the
// full budget when the rule was ready in 80 ms is not.
func (n *QueueNotifier) NotifyPreNotificationReady(
	ctx context.Context, _ db.TenantScope, notice PreNotificationNotice,
) error {
	if n.enqueuer == nil {
		return errs.New(errs.KindInternal, "enrichment_no_enqueuer",
			"the pre-notification notifier was built without a queue")
	}
	if notice.CaseID == uuid.Nil {
		// ⛔ THIS READ `notice.GroupID == uuid.Nil` (git-bug `7570090`), and the
		// reason behind it survives the rename untouched: no conversation means no
		// card to post, and `alerts` did not enqueue an evaluation either, so there
		// is nothing to release. Only the id that names the conversation changed.
		return nil
	}

	args := jobs.NotifyEvaluateArgs{
		CaseID:       notice.CaseID,
		Reason:       ReasonFired,
		StateVersion: notice.StateVersion,
	}
	if notice.AlertID != uuid.Nil {
		alertID := notice.AlertID
		args.AlertID = &alertID
	}

	_, err := n.enqueuer.Enqueue(ctx, args)
	return err
}
