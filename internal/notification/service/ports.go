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
	ListWithUnackedReminder(ctx context.Context, s db.TenantScope, orgDefaultSeconds *int) ([]domain.Policy, error)
	// ListWithDigest is the digest tick's whole view of configuration: the live
	// policies that carry a window (`digest_window_s`), in evaluation order. Unlike
	// `ListLive` it is not walked to a first match — every digest policy is its own
	// subscription — so the order buys a stable tick rather than a routing decision.
	ListWithDigest(ctx context.Context, s db.TenantScope) ([]domain.Policy, error)
}

// DigestStore is the read model the digest tick folds: what happened in a window,
// and which window this policy last covered.
//
// ⭐ IT IS TWO READS AND NO WRITE, WHICH IS THE POINT OF TICK EVALUATION. The
// alternative — evaluating a digest event-driven, as each case opens — needs durable
// per-window counters and per-policy timers, and every one of them is a row that can
// disagree with the facts it is counting. Here the count is a query over rows that
// are already stored, and the cursor is `max(digest_window_start)` over the digests
// themselves, so the only state the tick keeps is state a digest was actually sent
// for.
type DigestStore interface {
	// Buckets aggregates ONE window ONCE per tenant, per generation, so that N
	// digest policies fold the same rows instead of running N window queries. The
	// window is half-open, [start, end), so a boundary instant is counted once.
	Buckets(ctx context.Context, s db.TenantScope, start, end time.Time, limit int) ([]repository.DigestBucket, error)
	// LastWindow is the "last window covered" cursor, or the zero time for a policy
	// that has never digested — which means "cover one window", never "cover
	// everything since the policy was created".
	LastWindow(ctx context.Context, s db.TenantScope, policyID uuid.UUID) (time.Time, error)
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
	ExistsForReason(ctx context.Context, s db.TenantScope, kind domain.SubjectKind, subjectID uuid.UUID, r domain.Reason) (bool, error)
}

// DeliveryStore persists materialisations and owns their retry state.
type DeliveryStore interface {
	Create(ctx context.Context, s db.TenantScope, in repository.NewDelivery) (domain.Delivery, bool, error)
	SetThreadSeq(ctx context.Context, s db.TenantScope, id uuid.UUID, seq int, now time.Time) error
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Delivery, error)
	Claim(ctx context.Context, s db.TenantScope, id uuid.UUID, leaseCutoff, now time.Time) (domain.Delivery, bool, error)
	PersistRendered(ctx context.Context, s db.TenantScope, id uuid.UUID, payload json.RawMessage, hash, fallback string, now time.Time) error
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
	Freeze(ctx context.Context, s db.TenantScope, threadID uuid.UUID, now time.Time) error
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
	AppendDeliveryOutcome(ctx context.Context, s db.TenantScope, d domain.Delivery, groupID uuid.UUID, alertID *uuid.UUID, detail string, at time.Time) error
}

// ReminderStore serves the one-stage unacked reminder sweep.
//
// It reads under a TenantScope and nothing else: the sweep is per-tenant, one
// job per org (jobs.TenantFanOut), and the tenant list those jobs are minted
// from belongs to the fan-out's live-org pager in internal/app — not to this
// module, which must never enumerate tenants for itself.
type ReminderStore interface {
	ListUnackedGroups(ctx context.Context, s db.TenantScope, before time.Time, limit int) ([]repository.UnackedGroup, error)
}

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
