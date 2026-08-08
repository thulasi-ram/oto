package ordering

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
)

// ThreadState mirrors `channel_threads.state` (threads_state_ck).
type ThreadState string

// The closed ThreadState set.
const (
	// StateOpening means the root message has been claimed but not confirmed.
	StateOpening ThreadState = "opening"
	// StateOpen means the provider handle is known and replies can attach.
	StateOpen ThreadState = "open"
	// StateFrozen means the group closed; nothing further will be sent.
	StateFrozen ThreadState = "frozen"
	// StateDead means a terminal provider error killed the thread pointer.
	// This is a STATE TRANSITION, not a retry (SPEC §H.9).
	StateDead ThreadState = "dead"
)

// Thread is the ordering state of one ChannelThread. It is deliberately a
// four-field projection of `channel_threads`: everything else about a thread is
// the notification module's business.
type Thread struct {
	ID uuid.UUID
	// State is the thread's lifecycle state.
	State ThreadState
	// LastSentSeq is the ordering gate: the highest thread_seq that has been
	// resolved. Starts at 0, so seq 1 is the first sendable slot.
	LastSentSeq int
	// NextSeq is the allocator's next value. Slots [LastSentSeq+1, NextSeq) are
	// the ones still in play.
	NextSeq int
	// RootLanded reports that `provider_thread_id` is set, i.e. there is
	// something for a reply to attach to.
	RootLanded bool
}

// Item is one delivery competing for its turn in a thread.
type Item struct {
	ID       uuid.UUID
	ThreadID uuid.UUID
	// Seq is this delivery's thread_seq, allocated at creation time.
	Seq int
	// NeedsRoot is true for every mode except post_root: a reply or an in-place
	// update has nothing to attach to until the root message exists.
	//
	// ⚠ IT MUST BE THE RE-DERIVED MODE, NEVER THE MODE STORED ON THE ROW. The row's
	// mode was chosen when the intent was minted; the sender re-derives it against
	// the thread as it actually stands and turns a reply with no root into a fresh
	// root post. A gate fed the stale mode blocks on precisely the condition the
	// sender knows how to repair, which is how a thread wedges with no root and no
	// way to grow one.
	NeedsRoot bool
	// CreatedAt is when the delivery row was written. It bounds how long this
	// item is allowed to wait before the gate stops waiting and recovers.
	CreatedAt time.Time
}

// Action is what the caller must do with an Item.
type Action string

// The closed Action set.
const (
	// ActionProceed means: it is this item's turn. Render and send.
	ActionProceed Action = "proceed"
	// ActionWaitForRoot means the root message has not landed yet. Snooze.
	ActionWaitForRoot Action = "wait_for_root"
	// ActionWaitForPredecessor means an earlier delivery is still in flight. Snooze.
	ActionWaitForPredecessor Action = "wait_for_predecessor"
	// ActionRecoverThread means the thread cannot make progress as it stands:
	// run Recover, or the §H.9 thread-pointer recovery, then re-evaluate.
	ActionRecoverThread Action = "recover_thread"
	// ActionAbandon means this item must never be sent. Mark it skipped.
	ActionAbandon Action = "abandon"
	// ActionOutOfOrder means the item's seq is at or below last_sent_seq: its
	// slot has already been resolved. This is the duplicate-worker case and the
	// correct response is to exit quietly, not to send.
	ActionOutOfOrder Action = "out_of_order"
)

// Decision is the gate's verdict.
type Decision struct {
	Action Action
	// Wait is how long to snooze, for the two waiting actions.
	Wait time.Duration
	// Reason is a short, stable label. It is a metric label and a log field, so
	// it is drawn from a small closed vocabulary rather than formatted.
	Reason string
}

// Policy tunes the gate's waits. The zero value is invalid; use DefaultPolicy.
type Policy struct {
	// RootWait is the snooze while the root message has not landed (SPEC §G.7.2).
	RootWait time.Duration
	// PredecessorWait is the snooze while an earlier delivery is in flight.
	PredecessorWait time.Duration
	// MaxWait bounds total waiting for one item.
	//
	// A River snooze does NOT consume an attempt, by design — an item waiting its
	// turn has not failed — which also means the attempt ceiling can never end the
	// wait. MaxWait is what does: past it the gate stops waiting and returns
	// ActionRecoverThread.
	//
	// ⚠ MaxWait BOUNDS THE WAIT, NOT THE WEDGE. Returning ActionRecoverThread only
	// moves the obligation to the caller: if the caller answers a recovery that
	// advanced nothing with another snooze, the loop is unbounded again and the
	// attempt ceiling still cannot end it. The contract is therefore explicit —
	// A CALLER THAT RECEIVES ActionRecoverThread AND CANNOT MAKE PROGRESS MUST
	// REACH A TERMINAL OUTCOME FOR THE ITEM. That, plus Recover, is what makes
	// "a poisoned message can never wedge a thread forever" true.
	MaxWait time.Duration
}

// DefaultPolicy is the SPEC §G.7.2 schedule: 2 s waiting for a root, 1 s waiting
// for a predecessor, and a 15-minute ceiling on either.
//
// Fifteen minutes is chosen against Alertmanager's `repeat_interval` floor: an
// item still stuck after that will be superseded by a fresher notification
// anyway, so continuing to wait preserves nothing and delays everything behind it.
func DefaultPolicy() Policy {
	return Policy{
		RootWait:        2 * time.Second,
		PredecessorWait: 1 * time.Second,
		MaxWait:         15 * time.Minute,
	}
}

func (p Policy) normalise() Policy {
	d := DefaultPolicy()
	if p.RootWait <= 0 {
		p.RootWait = d.RootWait
	}
	if p.PredecessorWait <= 0 {
		p.PredecessorWait = d.PredecessorWait
	}
	if p.MaxWait <= 0 {
		p.MaxWait = d.MaxWait
	}
	return p
}

// LockKey is the advisory-lock key for a thread.
//
// Namespaced away from River's own advisory locks (db.LockNamespace): a collision
// between "serialise this Slack thread" and "elect a queue leader" would not
// crash, it would silently serialise two unrelated things forever under load,
// which is the hardest class of bug there is to see.
func LockKey(threadID uuid.UUID) int64 {
	return db.AdvisoryKeyUUID(db.LockNamespaceThread, threadID)
}

// Decide is the pure ordering rule. It performs no I/O, so the entire gate is
// testable as a table.
//
// It MUST be called only while the thread's advisory lock is held and with a
// Thread read INSIDE the same transaction. Deciding on a stale read is exactly
// the race the lock exists to prevent.
//
// Evaluation order, and one deliberate deviation from SPEC §G.7.2:
//
//	SPEC's illustrative switch tests "root has not landed" BEFORE "thread is
//	dead". Taken literally that wedges the one case §G.7.3 exists to prevent: a
//	`channel_not_found` on the root leaves the thread dead with NO root, so every
//	reply behind it snoozes on a root that can never arrive, forever. §G.7.3's
//	promise — "a poisoned message can never wedge a thread forever" — is binding
//	and stronger than the ordering of an illustrative snippet, so the terminal
//	states are tested first. The observable behaviour for every non-terminal
//	thread is identical to the SPEC's.
func Decide(item Item, th Thread, now time.Time, p Policy) Decision {
	p = p.normalise()

	switch th.State {
	case StateDead:
		// §H.9: a dead thread is a state transition, not a retry. Either the
		// thread pointer is recoverable (post a fresh root, re-point) or the
		// channel is gone and every queued item must be skipped. Either way the
		// caller decides, and it must decide now rather than wait.
		return Decision{Action: ActionRecoverThread, Reason: "thread_dead"}
	case StateFrozen:
		// The group generation closed. Anything still queued is about a state
		// nobody will look at again; sending it would be noise, and it must be
		// recorded as skipped rather than silently dropped.
		return Decision{Action: ActionAbandon, Reason: "thread_frozen"}
	case StateOpening, StateOpen:
	}

	switch {
	case item.Seq <= 0:
		// A delivery with no sequence never joined the thread's order. That is a
		// bug in the creating transaction, not a wait.
		return Decision{Action: ActionAbandon, Reason: "unsequenced"}

	case item.Seq <= th.LastSentSeq:
		// The slot is already resolved: a duplicate worker, or a redelivery after
		// gap recovery already advanced past this item. Exit quietly.
		return Decision{Action: ActionOutOfOrder, Reason: "already_resolved"}

	case item.NeedsRoot && !th.RootLanded && item.Seq == th.LastSentSeq+1:
		// THIS ITEM IS THE HEAD AND THERE IS NO ROOT. Every earlier slot is already
		// resolved, so nothing left in the thread can post one: a root delivery that
		// died terminally advanced the head without landing anything. Waiting here is
		// waiting for a message nobody will ever send, and because a snooze consumes
		// no attempt the wait would never end — the exact wedge §G.7.3 forbids. Ask
		// for recovery NOW rather than after MaxWait; the caller re-derives the mode
		// and posts a fresh root.
		return Decision{Action: ActionRecoverThread, Reason: "root_never_landed"}

	case item.NeedsRoot && !th.RootLanded:
		if waited(item, now) > p.MaxWait {
			return Decision{Action: ActionRecoverThread, Reason: "root_never_landed"}
		}
		return Decision{Action: ActionWaitForRoot, Wait: p.RootWait, Reason: "awaiting_root"}

	case item.Seq != th.LastSentSeq+1:
		if waited(item, now) > p.MaxWait {
			return Decision{Action: ActionRecoverThread, Reason: "head_of_line_stalled"}
		}
		return Decision{Action: ActionWaitForPredecessor, Wait: p.PredecessorWait, Reason: "awaiting_predecessor"}

	default:
		return Decision{Action: ActionProceed, Reason: "in_order"}
	}
}

// waited is how long item has been queued. A zero CreatedAt means "unknown", and
// unknown must never be read as "forever" — that would recover a healthy thread.
func waited(item Item, now time.Time) time.Duration {
	if item.CreatedAt.IsZero() {
		return 0
	}
	return now.Sub(item.CreatedAt)
}
