package domain

import (
	"time"

	"github.com/google/uuid"
)

// ThreadState mirrors `channel_threads.state` (threads_state_ck).
type ThreadState string

// The thread states.
const (
	// ThreadOpening means a root delivery has been created but the provider has
	// not yet confirmed a handle.
	ThreadOpening ThreadState = "opening"
	// ThreadOpen means the provider handle is known and replies can attach.
	ThreadOpen ThreadState = "open"
	// ⛔ THERE IS NO `frozen`. It meant "the group generation closed" and nothing
	// ever wrote it — `Freeze` had no production caller — so it was removed by
	// git-bug e5c060b and migration 00066. The trigger it waited for is going too:
	// a conversation holds exactly ONE Case, so there is no generation to close.
	//
	// ThreadDead means a terminal provider error killed the thread. THIS IS A
	// STATE TRANSITION, NOT A RETRY (§H.9).
	ThreadDead ThreadState = "dead"
)

// ⛔ `Valid()` WAS HERE AND IS DELETED (git-bug b04a2f3). It was the closed-set
// check for this vocabulary and NOTHING had ever called it — not one caller in the
// whole tree, tests included. An unwired closed-set guard is worse than no guard:
// it reads as protection while the two conversions that build a ThreadState from
// raw column text (`repository/threads.go:46` and `:363`) went straight past it,
// and it is the same defect `e5c060b` was filed about one type over.
//
// ⭐ THE GUARD THAT MATTERS NOW LIVES WHERE THE DAMAGE WAS. An unrecognised state
// used to fall through `ordering.Decide`'s switch and SEND; it now hits an explicit
// `default` that abandons. That is a check on the path that acts, rather than a
// predicate next to the type hoping someone calls it.

// Terminal reports whether nothing further will be sent on this thread as it
// stands.
//
// One state, since 00066 removed `frozen`. It stays a predicate rather than being
// inlined into its ONE caller — `Sendable`, below — because `Sendable` asks "may a
// delivery proceed", which is a different question from "did a provider error kill
// this". Collapsing them would tie the two together the next time a terminal state
// is added.
//
// ⚠️ ONE CALLER, AND THE ASYMMETRY IS REAL: the mirror in `platform/jobs/ordering`
// tests its state inline instead. Left as it is rather than harmonised here, which
// would be a second change riding along inside a deletion.
func (s ThreadState) Terminal() bool { return s == ThreadDead }

// DeadReason is the terminal provider error that killed a thread — the closed
// set of `channel_threads.dead_reason` (threads_deadmap_ck).
//
// These strings are the PROVIDER'S OWN error codes, stored verbatim so that a
// support question can be answered without a packet capture. They are schema
// vocabulary, not an SDK dependency: this module receives them as opaque strings
// on a classified provider error and never constructs a provider call.
type DeadReason string

// The closed DeadReason set.
const (
	// DeadChannelNotFound: the conversation no longer exists.
	DeadChannelNotFound DeadReason = "channel_not_found"
	// DeadIsArchived: the conversation was archived.
	DeadIsArchived DeadReason = "is_archived"
	// DeadNotInChannel: oto was removed from the conversation.
	DeadNotInChannel DeadReason = "not_in_channel"
	// DeadMessageNotFound: the root message is gone. The THREAD POINTER is dead,
	// but the destination is fine.
	DeadMessageNotFound DeadReason = "message_not_found"
	// DeadCannotReplyToMessage: the root will not accept replies.
	DeadCannotReplyToMessage DeadReason = "cannot_reply_to_message"
	// DeadThreadLocked: the thread was locked by a workspace rule.
	DeadThreadLocked DeadReason = "restricted_action_thread_locked"
	// DeadEditWindowClosed: the workspace disallows editing this message now.
	DeadEditWindowClosed DeadReason = "edit_window_closed"
	// DeadTokenRevoked: the credential was withdrawn.
	DeadTokenRevoked DeadReason = "token_revoked"
	// DeadAccountInactive: the installing account is gone.
	DeadAccountInactive DeadReason = "account_inactive"
)

var deadReasons = map[DeadReason]bool{
	DeadChannelNotFound: true, DeadIsArchived: true, DeadNotInChannel: true,
	DeadMessageNotFound: true, DeadCannotReplyToMessage: true, DeadThreadLocked: true,
	DeadEditWindowClosed: true, DeadTokenRevoked: true, DeadAccountInactive: true,
}

// DeadReasonFor maps a provider error code onto the stored vocabulary. The
// second result is false for a code that is not terminal for a THREAD — a rate
// limit or a 5xx must never end up here, because those are retries and a thread
// that dies on a retryable error loses its history for nothing.
func DeadReasonFor(code string) (DeadReason, bool) {
	r := DeadReason(code)
	return r, deadReasons[r]
}

// Recoverable reports that the DESTINATION is fine and only the thread POINTER
// is gone (§H.9).
//
// The recovery is to clear `provider_thread_id`, post a FRESH ROOT carrying a
// visible `continued` marker, and re-point the thread. oto NEVER reads the
// provider back to rediscover its own state: this table is oto's memory of
// the destination, and a system that re-derives its memory from the thing it is
// remembering has no memory at all (C9).
func (r DeadReason) Recoverable() bool {
	switch r {
	case DeadMessageNotFound, DeadCannotReplyToMessage, DeadThreadLocked, DeadEditWindowClosed:
		return true
	default:
		return false
	}
}

// DestinationHealth is the `channels.health_status` a dead thread implies. A
// credential failure and a missing conversation are different operator problems
// and must not collapse into one banner.
func (r DeadReason) DestinationHealth() HealthStatus {
	switch r {
	case DeadTokenRevoked, DeadAccountInactive:
		return HealthAuthFailed
	case DeadChannelNotFound, DeadIsArchived, DeadNotInChannel:
		return HealthDegraded
	default:
		// A pointer-only failure says nothing about the destination's health.
		return ""
	}
}

// MaxThreadReplies is oto's policy ceiling (S14): past this many replies a
// thread is unreadable, so a fresh root is posted with a link back.
const MaxThreadReplies = 30

// Thread is the binding of ONE AlertGroup generation to ONE provider
// conversation.
//
// THE DURABLE HANDLE IS THE PAIR (ProviderConversationID, ProviderThreadID), and
// the conversation id comes from the API RESPONSE rather than from config,
// because a configured name can be stale, renamed or ambiguous (§H.1 S7).
type Thread struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	ChannelID   uuid.UUID
	SubjectKind SubjectKind
	// SubjectID is the alert_groups row — ONE GENERATION. A re-opened group is a
	// new generation and therefore a new thread.
	SubjectID uuid.UUID

	ProviderConversationID string
	// ProviderThreadID is the root message id. A STRING, NEVER A FLOAT: float
	// rounding silently breaks threading and the damage is invisible until a
	// reply lands in the wrong place.
	ProviderThreadID string
	RootDeliveryID   *uuid.UUID

	ReplyCount int
	// LastSentSeq is the ordering gate: the highest thread_seq resolved.
	LastSentSeq int
	// NextSeq is the allocator. A delivery takes NextSeq++ inside the CREATING
	// transaction, so sequence order is the causal order of domain events.
	NextSeq int

	State      ThreadState
	DeadReason DeadReason

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RootLanded reports that there is something for a reply or an update to attach
// to.
func (t Thread) RootLanded() bool { return t.ProviderThreadID != "" }

// Sendable reports whether a delivery may proceed on this thread right now.
func (t Thread) Sendable() bool { return !t.State.Terminal() }

// NeedsFreshRoot reports that the thread has reached the reply ceiling and the
// next root-touching delivery should start a new card (S14).
func (t Thread) NeedsFreshRoot() bool { return t.ReplyCount >= MaxThreadReplies }
