package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/db"
)

// Every interface in this file is a PORT DECLARED BY THE CONSUMER. The
// notification module says what it needs; internal/app decides what satisfies
// it. Nothing here names a concrete implementation of another module, and
// nothing here names a provider.

// SnapshotSource is the ONE method this module needs from the alerts side of the
// system: give me everything about a group generation, as it is RIGHT NOW.
//
// It is deliberately a single method over a data snapshot rather than a dozen
// getters. A wide port would couple the two modules' release schedules; this one
// can be satisfied by `alerts/service` or by the read-model repository shipped
// alongside it, and swapping between them is a constructor argument.
//
// The contract that matters is the WORD "NOW". §C11 requires a delivery to be
// rendered at CLAIM TIME, so an implementation that caches, or that answers from
// the state at enqueue time, silently breaks the guarantee that an alert which
// fired and resolved inside a snooze window produces no stale card.
type SnapshotSource interface {
	Snapshot(ctx context.Context, s db.TenantScope, q domain.SnapshotQuery) (domain.Snapshot, error)
}

// PolicyStore reads notification policies in evaluation order.
type PolicyStore interface {
	ListLive(ctx context.Context, s db.TenantScope) ([]domain.Policy, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Policy, error)
	// ListWithDigest is the digest tick's whole view of configuration: the live
	// policies that carry a window (`digest_window_s`), in evaluation order. Unlike
	// `ListLive` it is not walked to a first match — every digest policy is its own
	// subscription — so the order buys a stable tick rather than a routing decision.
	ListWithDigest(ctx context.Context, s db.TenantScope) ([]domain.Policy, error)
}

// DigestStore is what the digest tick reads and the little it remembers: the
// episodes in a span, how far each policy has been examined, and which episodes each
// policy has already accounted for.
//
// ⭐ EVALUATION IS STILL TICK-DRIVEN, WHICH IS STILL THE COST ARGUMENT. The
// alternative — evaluating a digest event-driven, as each case opens — needs durable
// per-window counters and per-policy timers, and every one of them is a row that can
// disagree with the facts it is counting. Here the content is a query over rows that
// are already stored, and the floor is free because the count is already in hand.
//
// ⛔ IT WAS "TWO READS AND NO WRITE", AND THAT SENTENCE WAS THE STATEMENT OF THREE
// BUGS RATHER THAN A VIRTUE (git-bug `893cee4`, `342e071`, `a8a4010`; migration
// `00070`). The cursor was `max(digest_window_start)` over the digests themselves, so
// the only state that could exist was state a digest had actually been sent for — and
// three separate failures follow from exactly that:
//
//   - A WINDOW EXAMINED AND FOUND QUIET writes no row, so it is indistinguishable
//     from a window never examined and the cursor cannot advance past it. A policy
//     over a namespace nobody had paged about for a week re-derived the same owed
//     span every tick, ran six aggregate queries, and logged a data-loss warning
//     about a backlog nothing was ever owed. Coverage and "a digest was sent" are now
//     two facts (`CoveredTo`/`AdvanceCoverage`).
//   - A WINDOW START IS NOT AN INSTANT. It only means a span in combination with the
//     window LENGTH in force when it was sent, which nothing stored, so narrowing
//     `digest_window_s` re-tiled a span an earlier digest had already summarised and
//     reported every episode in it a second time. The cursor is now the instant
//     coverage reached.
//   - A CASE WHOSE TRANSACTION COMMITS AFTER THE TICK HAS READ ITS WINDOW is counted
//     by no window at all, because `started_at` is oto's clock read BEFORE the write
//     and no later window's predicate reaches back. A digest now reads a
//     `domain.DigestLookback` tail as well as its window and subtracts what it has
//     already accounted for (`Marked`/`Mark`), so a straggler lands in the next
//     digest instead of falling below a frozen cursor.
//
// The write this buys is small and it is this module's own: a per-policy cursor row
// and one narrow row per (policy, Case) with a bounded retention. It is emphatically
// not a durable per-window counter — nothing here counts anything.
type DigestStore interface {
	// Cases lists ONE span's episodes ONCE per tenant, so that N digest policies fold
	// the same rows instead of running N queries. The span is half-open, [from, to),
	// so a boundary instant belongs to exactly one window.
	Cases(ctx context.Context, s db.TenantScope, from, to time.Time, limit int) ([]repository.DigestCase, error)
	// CoveredTo is the INSTANT this policy has been examined up to, or the zero time
	// for a policy that has never been examined — which means "cover one window",
	// never "cover everything since the policy was created".
	CoveredTo(ctx context.Context, s db.TenantScope, policyID uuid.UUID) (time.Time, error)
	// AdvanceCoverage moves that instant forward, monotonically. It is called for
	// every window the tick EXAMINED, whatever the examination decided.
	AdvanceCoverage(ctx context.Context, s db.TenantScope, policyID uuid.UUID, reached, now time.Time) error
	// Marked is the set of Cases in [from, to) this policy has already accounted for
	// — reported in a digest, or examined in a window that did not clear its floor.
	// It is what the lookback subtracts.
	Marked(ctx context.Context, s db.TenantScope, policyID uuid.UUID, from, to time.Time) (map[uuid.UUID]struct{}, error)
	// Mark accounts for a set of Cases. `reportedIn` is the digest that reported
	// them, or nil when the window did not clear the floor. Write-once.
	Mark(ctx context.Context, s db.TenantScope, policyID uuid.UUID, reportedIn *uuid.UUID, cases []repository.DigestCase, now time.Time) error
	// PruneMarks is the retention sweep that keeps the dedupe state proportional to
	// RECENT activity rather than to all of history.
	PruneMarks(ctx context.Context, s db.TenantScope, before time.Time) (int64, error)
}

// NotificationStore persists intents.
type NotificationStore interface {
	Insert(ctx context.Context, s db.TenantScope, n domain.Notification) (domain.Notification, bool, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Notification, error)
	SetStatus(ctx context.Context, s db.TenantScope, id uuid.UUID, st domain.Status, now time.Time) error
	// CountRecent keys on the DELIVERY TARGET (`group_id`), not on the subject:
	// since migration 00056 a subject predicate no longer matches every row, and
	// the throttle has to count everything that landed on the thread.
	CountRecent(ctx context.Context, s db.TenantScope, groupID uuid.UUID, since time.Time) (int, error)
	// CountRecentSubjects is the count condition's numerator and is deliberately
	// NOT `CountRecent` with different arguments: it counts DISTINCT SUBJECTS of
	// the one kind the policy binds, it is scoped to the POLICY rather than to the
	// conversation, and it INCLUDES suppressed rows. Each of the three differences
	// is the opposite of the throttle's and each is forced — see
	// `countPolicySubjectsSQL`. `excluding` is the subject of the fact being
	// evaluated, which has no row yet and which the caller adds back as one.
	//
	// ⚠️ THE WINDOW IS CLOSED AT BOTH ENDS, `[since, until]`, AND `until` IS NOT
	// `now`. It is the snapshot instant of the fact being evaluated, so a retried
	// evaluation counts the same rows the first attempt would have — a floor that
	// widened on every retry would clear itself by being retried.
	CountRecentSubjects(ctx context.Context, s db.TenantScope, policyID uuid.UUID, kind domain.SubjectKind, excluding uuid.UUID, since, until time.Time) (int, error)
	ExistsForReason(ctx context.Context, s db.TenantScope, kind domain.SubjectKind, subjectID uuid.UUID, r domain.Reason) (bool, error)
}

// DeliveryStore persists materialisations and owns their retry state.
type DeliveryStore interface {
	Create(ctx context.Context, s db.TenantScope, in repository.NewDelivery) (domain.Delivery, bool, error)
	SetThreadSeq(ctx context.Context, s db.TenantScope, id uuid.UUID, seq int, now time.Time) error
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Delivery, error)
	Claim(ctx context.Context, s db.TenantScope, id uuid.UUID, leaseCutoff, now time.Time) (domain.Delivery, bool, error)
	PersistRendered(ctx context.Context, s db.TenantScope, id uuid.UUID, payload json.RawMessage, hash, fallback string, now time.Time, wordings map[string]string) error
	// MarkSent reports whether the row was actually written. False means the claim
	// was gone and the send is recorded NOWHERE — see the repository's warning.
	MarkSent(ctx context.Context, s db.TenantScope, id uuid.UUID, messageID, conversationID string, raw json.RawMessage, now time.Time) (bool, error)
	MarkFailed(ctx context.Context, s db.TenantScope, id uuid.UUID, message string, class domain.ErrorClass, nextAttemptAt, now time.Time) error
	MarkDead(ctx context.Context, s db.TenantScope, id uuid.UUID, message string, class domain.ErrorClass, now time.Time) error
	MarkSkipped(ctx context.Context, s db.TenantScope, id uuid.UUID, why string, now time.Time) error
	RepointToRoot(ctx context.Context, s db.TenantScope, id uuid.UUID, why string, now time.Time) error
	StatusesFor(ctx context.Context, s db.TenantScope, notificationID uuid.UUID) ([]domain.DeliveryStatus, error)
	LastRootHash(ctx context.Context, s db.TenantScope, threadID uuid.UUID) (string, error)
}

// ThreadStore is oto's memory of where its messages went.
type ThreadStore interface {
	Ensure(ctx context.Context, s db.TenantScope, channelID uuid.UUID, kind domain.SubjectKind, subjectID uuid.UUID, now time.Time) (domain.Thread, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Thread, error)
	AllocateSeq(ctx context.Context, s db.TenantScope, threadID uuid.UUID, now time.Time) (int, error)
	RecordRoot(ctx context.Context, s db.TenantScope, threadID uuid.UUID, conversationID, messageID string, rootDeliveryID uuid.UUID, seq int, now time.Time) error
	RecordReply(ctx context.Context, s db.TenantScope, threadID uuid.UUID, seq int, now time.Time) error
	AdvanceSent(ctx context.Context, s db.TenantScope, threadID uuid.UUID, seq int, now time.Time) error
	MarkDead(ctx context.Context, s db.TenantScope, threadID uuid.UUID, reason domain.DeadReason, now time.Time) error
	ClearPointer(ctx context.Context, s db.TenantScope, threadID uuid.UUID, now time.Time) error
	// ⛔ `Freeze` WAS HERE AND IS DELETED (git-bug e5c060b, migration 00066). It set
	// `channel_threads.state = 'frozen'` and no production code ever called it, so
	// every implementation — including every fake — paid for a method at the seam
	// that bought nothing. The state it wrote is gone from the schema too.
}

// ChannelStore reads destinations and records what a delivery learned about
// their health.
type ChannelStore interface {
	ListByIDs(ctx context.Context, s db.TenantScope, ids []uuid.UUID) ([]domain.Channel, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Channel, error)
	SetHealth(ctx context.Context, s db.TenantScope, id uuid.UUID, status domain.HealthStatus, detail string, now time.Time) error
	Credential(ctx context.Context, s db.TenantScope, id uuid.UUID) (repository.SealedCredential, error)
	// ⛔ `ClaimStormNotice` WAS HERE AND IS DELETED. It took the once-per-channel
	// storm-notice latch (`channels.storm_notice_at`, ADR 0020): true meant THIS
	// evaluation got to tell the channel oto had started withholding. Storm damping
	// is gone, so nothing withholds and nothing announces. It was the only WRITE this
	// port carried for the evaluation path, and its removal is why
	// `NotificationService` needs no channel store at all.
}

// EventSink appends this module's facts to the shared timeline.
//
// It is a REQUIRED collaborator, not an optional one. §B.6 makes suppression a
// recorded fact; a build where this is nil is a build where oto can decide not
// to tell you something and leave no trace of the decision.
type EventSink interface {
	AppendNotificationCreated(ctx context.Context, s db.TenantScope, n domain.Notification, destinations int, at time.Time) error
	AppendNotificationSuppressed(ctx context.Context, s db.TenantScope, n domain.Notification, also []domain.SuppressedReason, at time.Time) error
	AppendDeliveryOutcome(ctx context.Context, s db.TenantScope, d domain.Delivery, alertID, caseID *uuid.UUID, detail string, at time.Time) error
}

// ⛔ `ReminderStore` WAS HERE AND IS DELETED, along with `PolicyStore`'s
// `ListWithUnackedReminder` (git-bug bd0fb1d). The owner withdrew the unacked
// reminder: oto sends nothing unprompted, so there is no sweep to serve and no
// "which policies want reminding" question to ask.
//
// It was this module's ONLY port over `repository` as a package — the interface
// spoke in `repository.UnackedGroup` rather than a domain type — so removing it
// also removes that seam's one exception.

// ChannelRegistry — the port onto the channels registry — is declared in
// channels_port.go, alongside the type aliases it is expressed in. It lives
// there rather than here so that exactly one file in this module names
// `internal/channels/domain`.

// CredentialUnsealer turns a sealed blob into provider credentials.
//
// This module holds the ciphertext and never the key. An unsealer that logged
// its output, or a repository that returned plaintext, would put a workspace
// token into every stack trace this package can produce.
type CredentialUnsealer interface {
	Unseal(ctx context.Context, kind string, sealed []byte, keyVersion int) (map[string]string, error)
}

// Enqueuer is platform/db's transactional outbox, restated here so the service
// signature says what it depends on.
type Enqueuer = db.Enqueuer
