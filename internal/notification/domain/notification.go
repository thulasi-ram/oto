package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status is the aggregate state of one Notification — the closed set of
// `notifications.status` (notifications_status_ck).
type Status string

// The notification statuses.
const (
	// StatusPending means the intent exists and its fan-out has not been sent.
	StatusPending Status = "pending"
	// StatusDispatched means deliveries were created and are in flight.
	StatusDispatched Status = "dispatched"
	// StatusPartial means some destinations got it and some did not. It is a
	// FIRST-CLASS outcome, not a rounding error: "three of four channels heard
	// about the outage" is exactly what an operator needs to know.
	StatusPartial Status = "partial"
	// StatusDelivered means every destination got it.
	StatusDelivered Status = "delivered"
	// StatusFailed means no destination got it.
	StatusFailed Status = "failed"
	// StatusSuppressed means oto deliberately said nothing, and recorded why.
	StatusSuppressed Status = "suppressed"
)

// DeliveryStatus is the state of ONE materialisation on ONE Channel — the closed
// set of `notification_deliveries.status` (deliveries_status_ck).
type DeliveryStatus string

// The delivery statuses.
const (
	// DeliveryPending is created and waiting for its turn.
	DeliveryPending DeliveryStatus = "pending"
	// DeliverySending is claimed by a worker. Combined with a stale updated_at
	// this is the AMBIGUOUS case of §G.5.
	DeliverySending DeliveryStatus = "sending"
	// DeliverySent reached the provider and the handle was recorded.
	DeliverySent DeliveryStatus = "sent"
	// DeliveryFailed will be retried.
	DeliveryFailed DeliveryStatus = "failed"
	// DeliveryDead will not be retried.
	DeliveryDead DeliveryStatus = "dead"
	// DeliverySkipped was deliberately not sent: a coalesced no-op update, or a
	// slot the gap recovery moved past.
	DeliverySkipped DeliveryStatus = "skipped"
)

// Resolved reports that this delivery will not change again without a new claim.
func (s DeliveryStatus) Resolved() bool {
	switch s {
	case DeliverySent, DeliveryDead, DeliverySkipped:
		return true
	default:
		return false
	}
}

// Claimable reports whether the optimistic-lock claim may take this row. It is
// the Go statement of `WHERE status IN ('pending','failed')`.
func (s DeliveryStatus) Claimable() bool {
	return s == DeliveryPending || s == DeliveryFailed
}

// ErrorClass mirrors `notification_deliveries.error_class`
// (deliveries_errclass_ck) and drives the §G.6 retry policy.
type ErrorClass string

// The error classes.
const (
	// ClassRetryable backs off exponentially.
	ClassRetryable ErrorClass = "retryable"
	// ClassRateLimited honours the provider's Retry-After EXACTLY. Guessing
	// shorter than the provider asked is how a soft limit becomes a hard block.
	ClassRateLimited ErrorClass = "rate_limited"
	// ClassPermanent is dead immediately, UNLESS it is a thread-pointer error,
	// which is a state transition (§H.9).
	ClassPermanent ErrorClass = "permanent"
	// ClassConfigInvalid is dead. oto sent something the provider refused; this
	// is an oto bug and raises a banner on the destination.
	ClassConfigInvalid ErrorClass = "config_invalid"
	// ClassAuthExpired is dead. The credential is gone; raise a banner.
	ClassAuthExpired ErrorClass = "auth_expired"
)

// Valid reports whether c is in the closed set.
func (c ErrorClass) Valid() bool {
	switch c {
	case ClassRetryable, ClassRateLimited, ClassPermanent, ClassConfigInvalid, ClassAuthExpired:
		return true
	default:
		return false
	}
}

// Terminal reports whether this class must never be retried (§G.6).
func (c ErrorClass) Terminal() bool {
	switch c {
	case ClassPermanent, ClassConfigInvalid, ClassAuthExpired:
		return true
	default:
		return false
	}
}

// DestinationHealth is the `channels.health_status` this class implies, or "" if
// the class says nothing about the destination.
func (c ErrorClass) DestinationHealth() HealthStatus {
	switch c {
	case ClassAuthExpired:
		return HealthAuthFailed
	case ClassConfigInvalid:
		return HealthConfigInvalid
	case ClassPermanent:
		return HealthDegraded
	default:
		return ""
	}
}

// Retry ceilings from §G.6, expressed as ATTEMPTS on the delivery row.
const (
	// MaxRetryableAttempts is 12 attempts, then dead.
	MaxRetryableAttempts = 12
	// MaxRateLimitedAttempts is 20 attempts, then dead. A throttled destination
	// is the common case and must not burn the retryable budget.
	MaxRateLimitedAttempts = 20
	// MaxDeliveryAttempts is the hard schema ceiling (deliveries_attempts_ck).
	MaxDeliveryAttempts = 32
)

// Backoff bounds from §G.6.
const (
	backoffBase = 2 * time.Second
	backoffCap  = 300 * time.Second
	// DefaultRateLimitDelay is the wait when a rate-limited error carries no
	// Retry-After.
	DefaultRateLimitDelay = 60 * time.Second
)

// Exhausted reports whether attempts has consumed this class's budget.
func (c ErrorClass) Exhausted(attempts int) bool {
	if c.Terminal() {
		return true
	}
	if attempts >= MaxDeliveryAttempts {
		return true
	}
	if c == ClassRateLimited {
		return attempts >= MaxRateLimitedAttempts
	}
	return attempts >= MaxRetryableAttempts
}

// Backoff is the §G.6 schedule: base 2 s, factor 2, capped at 300 s, with jitter
// applied by the caller.
//
// jitter is a fraction in [-0.5, +0.5] and is supplied rather than generated
// because randomness is I/O and this package is pure. Passing 0 gives the
// deterministic schedule, which is what a test wants.
func Backoff(attempts int, jitter float64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := backoffBase
	for i := 1; i < attempts && d < backoffCap; i++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	switch {
	case jitter > 0.5:
		jitter = 0.5
	case jitter < -0.5:
		jitter = -0.5
	}
	out := time.Duration(float64(d) * (1 + jitter))
	if out < time.Second {
		out = time.Second
	}
	return out
}

// Notification is the channel-agnostic INTENT to communicate ONE FACT.
//
// It is NOT a message. A message is a NotificationDelivery. The distinction is
// the reason this module exists: an intent can be idempotent, re-evaluated
// against current state, suppressed with a recorded reason, and fanned out to
// destinations that did not exist when it was minted. A rendered message can do
// none of those things.
type Notification struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	SubjectKind SubjectKind
	SubjectID   uuid.UUID
	// ConversationKind and ConversationID are THE DELIVERY TARGET, as a pair
	// (migration 00064, git-bug 7570090). They answer "where does this land", the
	// way (SubjectKind, SubjectID) answers "what is this about".
	//
	// ⭐ THEY REPLACED `GroupID` PLUS AN EXCEPTION. The old shape was `group_id`
	// with `notifications_target_ck` reading "every fact names a group EXCEPT a
	// digest", which does not extend: the Case conversation that replaces
	// `alert_groups` is not a group row either, and adding it meant a third arm in
	// the CHECK and a third branch in every reader. As a pair, a digest is ONE KIND
	// AMONG SEVERAL and the next kind needs no migration.
	//
	// ⛔ AND `GroupID` IS NOW GONE — THIS IS THE MOMENT THAT NOTE POINTED AT. It said
	// the column survived because several readers used it to answer SUBJECT-shaped
	// questions — the per-alert rollup, the drill artifact read, the audit filter —
	// and that those would be "answered deliberately when `alert_groups` goes, not by
	// deleting a column they happen to use". `alert_groups` has gone (migration
	// `00069`), and each of those readers was re-pointed at the pair or at
	// `(SubjectKind, SubjectID)` according to which question it was actually asking.
	ConversationKind ConversationKind
	ConversationID   uuid.UUID
	// AlertID is set when the fact is about ONE alert. It is MANDATORY for the
	// alert-scoped reasons (notifications_focus_ck).
	AlertID *uuid.UUID
	CaseID  *uuid.UUID

	Reason   Reason
	PolicyID *uuid.UUID
	// StateVersion is the alert_groups.state_version this intent was minted
	// against. It is hashed into IdempotencyKey, which is what makes a re-run at
	// a newer version mint a NEW intent instead of resending an old one.
	StateVersion     int
	IdempotencyKey   string
	Status           Status
	SuppressedReason SuppressedReason

	// DigestWindowStart is the window half of a digest's subject: the inclusive,
	// epoch-aligned start of the CLOSED window this digest reports on. Present
	// exactly when SubjectKind is SubjectDigest (notifications_digest_ck).
	DigestWindowStart *time.Time
	// DigestCount is how many Cases OPENED inside that window — the number the
	// digest asserts, and the number the policy's floor was compared against.
	//
	// ⭐ IT IS STORED RATHER THAN RECOMPUTED AT CLAIM TIME, which is a deliberate
	// exception to C11 and migration 00058 carries the argument: the window is closed
	// so there is no newer truth to render, but `alert_cases` is reapable, so a
	// recomputed count would shrink as the episodes aged out and the row would say a
	// different thing every time it was read.
	DigestCount *int
	// DigestCoveredFrom and DigestCoveredTo are THE SPAN THIS DIGEST ACTUALLY
	// COVERED, and they are the fact the row was missing (git-bug `342e071`,
	// migration 00070).
	//
	// ⭐⭐ `DigestWindowStart` ALONE IS MEANINGLESS AND THAT IS NOT A STYLE
	// COMPLAINT. A start is only a span in combination with the LENGTH that was in
	// force when the digest was sent, and no column stored the length — so every
	// reader that wanted a span inferred one from the policy's CURRENT
	// `digest_window_s`, which is exactly the inference that re-reported an hour as
	// six ten-minute digests the moment somebody narrowed the window.
	// `service.DigestHeadline` still takes the current window as a parameter for the
	// same reason, and these two columns are what let it stop.
	//
	// `From` is at or before `DigestWindowStart`: it is pulled back when the digest
	// swept up a straggler that committed too late for the previous window's read
	// (see domain.DigestLookback), so the honest sentence is "since the last digest,
	// plus stragglers". `To` is the window's exclusive end, which is the instant
	// coverage reached and the value the next tick's cursor is derived from.
	//
	// ⚠️ BOTH ARE NIL ON EVERY DIGEST WRITTEN BEFORE MIGRATION 00070, and they stay
	// nil. Backfilling them would mean multiplying the stored start by the policy's
	// window as it is TODAY — the one arithmetic this pair exists to retire — so a
	// row that does not know its span says so. `notifications_digcover_ck` therefore
	// permits the absence, and a policy whose newest digest is one of those is read
	// as "never digested", which covers one window rather than replaying a history.
	DigestCoveredFrom *time.Time
	DigestCoveredTo   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Digest reports whether this Notification is about a window rather than about an
// object — and therefore that GroupID is nil, that its thread is keyed by its
// policy, and that its card is drawn from DigestCount instead of from a snapshot.
func (n Notification) Digest() bool { return n.SubjectKind == SubjectDigest }

// Delivery is ONE MATERIALISATION of a Notification on ONE Channel. It owns the
// retry state, the provider ids and the rendered payload.
type Delivery struct {
	ID             uuid.UUID
	OrgID          uuid.UUID
	NotificationID uuid.UUID
	ChannelID      uuid.UUID
	ThreadID       *uuid.UUID
	// ThreadSeq is the FIFO position within the thread, allocated from
	// channel_threads.next_seq inside the CREATING transaction. Ordering is a
	// property of the write, not of the queue.
	ThreadSeq int

	Mode          Mode
	Status        DeliveryStatus
	Attempts      int
	NextAttemptAt *time.Time

	// Rendered is the exact provider payload, persisted BEFORE the network call
	// and at CLAIM time, so a crash mid-send leaves evidence of what was sent and
	// the card reflects the world at claim rather than at enqueue (C11).
	Rendered         json.RawMessage
	RenderedHash     string
	RenderedFallback string

	ProviderMessageID      string
	ProviderConversationID string
	ProviderResponse       json.RawMessage

	Error      string
	ErrorClass ErrorClass
	// Ambiguous is the honest flag: oto crashed after the provider may have
	// accepted a post_root, and re-sent it. The card carries a visible marker.
	// Exactly-once does not exist and the schema says so.
	Ambiguous bool

	SentAt    *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AggregateStatus folds a fan-out into the Notification status.
//
// The rules are chosen so that the aggregate can never be more optimistic than
// the deliveries: anything still in flight keeps the whole notification
// `dispatched`, and a fan-out where some destinations were told and some were
// not is `partial` rather than `delivered`.
//
// A `skipped` delivery counts as SUCCESS. It is a coalesced no-op — the
// destination already shows exactly this content — and calling that a failure
// would make a healthy, quiet thread look broken.
func AggregateStatus(statuses []DeliveryStatus) Status {
	if len(statuses) == 0 {
		return StatusSuppressed
	}

	var sent, failed, inFlight int
	for _, s := range statuses {
		switch s {
		case DeliverySent, DeliverySkipped:
			sent++
		case DeliveryDead:
			failed++
		case DeliveryPending, DeliverySending, DeliveryFailed:
			inFlight++
		}
	}

	switch {
	case inFlight > 0:
		return StatusDispatched
	case sent == len(statuses):
		return StatusDelivered
	case sent == 0:
		return StatusFailed
	default:
		return StatusPartial
	}
}

// ConversationKind is WHERE a fact lands: which kind of conversation owns the
// thread it belongs to.
//
// ⛔ IT IS DELIBERATELY NOT `SubjectKind`, THOUGH THEY OVERLAP. A subject is what
// a fact is ABOUT; a conversation is where it is DELIVERED. `alert` and `case` are
// subjects that no conversation is ever keyed by, so the sets are not the same set.
// Sharing one type would tie two vocabularies that have different reasons to change,
// and `notifications_convkind_ck` is a separate CHECK for the same reason.
//
// ⚠️ THIS ARGUMENT IS WEAKER THAN IT WAS, AND SAYING SO IS THE POINT. It used to
// have a second leg: the policy collapse that was going to replace `alert_groups`
// would have been a conversation that is no subject at all, so the two vocabularies
// differed at BOTH ends. Migration 00065 deleted that collapse — the owner ruled a
// conversation holds exactly one Case — and the leg went with it. What is left is
// one-directional: every ConversationKind today IS also a SubjectKind, which
// `SubjectKind()` below proves by being total, and the difference is only the
// subjects that are never conversations. `case` is about to leave even that list
// when it becomes a conversation kind, narrowing it to `alert`.
//
// The types stay apart on the surviving leg, not on the dead one. If `alert` ever
// becomes a conversation the argument is spent, and THAT is the moment to merge
// them — not before, and not on the strength of a sentence about a collapse that
// no longer exists.
type ConversationKind string

const (
	// ConversationCase is one Case's thread. A conversation holds exactly ONE Case.
	//
	// ⛔ IT WAS `ConversationAlertGroup ConversationKind = "alert_group"` AND THE
	// REPLACEMENT IS THE ONE ITS OWN COMMENT PREDICTED (git-bug `7570090`). That
	// comment called itself TRANSITIONAL and said the replacement was "already
	// ruled: `case`", warning that quietly widening the kind to mean "generation or
	// case" was how the distinction would be lost — a generation could hold many
	// cases and a conversation may not. So this is a REPLACEMENT, not a rename: the
	// cardinality changed with the spelling.
	ConversationCase ConversationKind = "case"
	// ConversationDigest is a digest's own conversation, keyed by its policy. It
	// spans many generations, which is why it could never carry a group id and why
	// it was the exception the pair exists to retire.
	ConversationDigest ConversationKind = "digest"
)

// SubjectKind maps a conversation kind onto the `channel_threads.subject_kind`
// spelling.
//
// The thread table still calls this column `subject_kind`, and renaming it is
// 7570090's later stage rather than this one's. The mapping is total over the two
// kinds that exist; an unknown kind returns the empty SubjectKind, which
// `threads_subjkind_ck` refuses at the write — a loud failure rather than a thread
// silently opened under the wrong key.
func (k ConversationKind) SubjectKind() SubjectKind {
	switch k {
	case ConversationCase:
		return SubjectCase
	case ConversationDigest:
		return SubjectDigest
	}
	return ""
}
