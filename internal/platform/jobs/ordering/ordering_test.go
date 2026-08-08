package ordering

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// created is a fixed birth time for an item, so "how long has this waited" is a
// property of the case rather than of the wall clock.
var created = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func item(seq int, needsRoot bool, age time.Duration) Item {
	return Item{
		ID:        uuid.New(),
		ThreadID:  uuid.New(),
		Seq:       seq,
		NeedsRoot: needsRoot,
		CreatedAt: created.Add(-age),
	}
}

// waits reports whether an action asks the caller to snooze. A snooze consumes no
// River attempt, so an action that waits can never, by itself, end anything.
func waits(a Action) bool {
	return a == ActionWaitForRoot || a == ActionWaitForPredecessor
}

func TestDecide(t *testing.T) {
	t.Parallel()

	p := DefaultPolicy()
	past := p.MaxWait + time.Minute

	cases := []struct {
		name   string
		item   Item
		thread Thread
		want   Action
		reason string
	}{
		{
			name:   "a dead thread decides now and never waits",
			item:   item(3, true, 0),
			thread: Thread{State: StateDead, LastSentSeq: 2, NextSeq: 4},
			want:   ActionRecoverThread, reason: "thread_dead",
		},
		{
			name:   "a frozen thread abandons what is still queued",
			item:   item(3, true, 0),
			thread: Thread{State: StateFrozen, LastSentSeq: 2, NextSeq: 4},
			want:   ActionAbandon, reason: "thread_frozen",
		},
		{
			name:   "an unsequenced delivery never joined the order",
			item:   item(0, false, 0),
			thread: Thread{State: StateOpen, LastSentSeq: 0, NextSeq: 1},
			want:   ActionAbandon, reason: "unsequenced",
		},
		{
			name:   "a resolved slot is a duplicate worker",
			item:   item(2, true, 0),
			thread: Thread{State: StateOpen, LastSentSeq: 4, NextSeq: 6, RootLanded: true},
			want:   ActionOutOfOrder, reason: "already_resolved",
		},
		{
			// THE REGRESSION. A root delivery that died terminally advanced the head
			// without landing anything, so this reply owns the head of a thread with
			// no root. Nothing behind it can ever post one. Waiting here consumed no
			// attempt and had no ceiling: the thread went silent permanently.
			name:   "a root-needing item at the head of a rootless thread terminates",
			item:   item(3, true, 0),
			thread: Thread{State: StateOpening, LastSentSeq: 2, NextSeq: 4},
			want:   ActionRecoverThread, reason: "root_never_landed",
		},
		{
			name:   "a root-needing item behind an unresolved slot may still wait for a root",
			item:   item(5, true, 0),
			thread: Thread{State: StateOpening, LastSentSeq: 2, NextSeq: 6},
			want:   ActionWaitForRoot, reason: "awaiting_root",
		},
		{
			name:   "a root that never landed stops being waited for at MaxWait",
			item:   item(5, true, past),
			thread: Thread{State: StateOpening, LastSentSeq: 2, NextSeq: 6},
			want:   ActionRecoverThread, reason: "root_never_landed",
		},
		{
			// The re-derived mode of a rootless thread is post_root, which needs no
			// root: this is what the caller actually asks about, and it proceeds.
			name:   "a root post at the head of a rootless thread proceeds",
			item:   item(3, false, 0),
			thread: Thread{State: StateOpening, LastSentSeq: 2, NextSeq: 4},
			want:   ActionProceed, reason: "in_order",
		},
		{
			name:   "an item behind its predecessor waits",
			item:   item(5, true, time.Second),
			thread: Thread{State: StateOpen, LastSentSeq: 2, NextSeq: 6, RootLanded: true},
			want:   ActionWaitForPredecessor, reason: "awaiting_predecessor",
		},
		{
			name:   "a head-of-line stall stops being waited for at MaxWait",
			item:   item(5, true, past),
			thread: Thread{State: StateOpen, LastSentSeq: 2, NextSeq: 6, RootLanded: true},
			want:   ActionRecoverThread, reason: "head_of_line_stalled",
		},
		{
			name:   "the next item on an open thread proceeds",
			item:   item(3, true, time.Second),
			thread: Thread{State: StateOpen, LastSentSeq: 2, NextSeq: 4, RootLanded: true},
			want:   ActionProceed, reason: "in_order",
		},
		{
			name:   "an unknown CreatedAt is never read as forever",
			item:   Item{ID: uuid.New(), ThreadID: uuid.New(), Seq: 5, NeedsRoot: true},
			thread: Thread{State: StateOpen, LastSentSeq: 2, NextSeq: 6, RootLanded: true},
			want:   ActionWaitForPredecessor, reason: "awaiting_predecessor",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Decide(tc.item, tc.thread, created, p)
			require.Equal(t, tc.want, got.Action)
			require.Equal(t, tc.reason, got.Reason)
			if waits(got.Action) {
				require.Positive(t, got.Wait, "a waiting decision must say how long")
			}
		})
	}
}

// TestDecideRootlessHeadNeverSnoozes is the liveness assertion stated on its own,
// because it is the invariant and not merely a row in a table.
//
// A snooze consumes no River attempt, so a gate that answers "wait" for a
// condition nothing can change has wedged the thread for the lifetime of the
// deployment. Re-deciding cannot help: with no root delivery left in the thread,
// every subsequent pass sees exactly the same state.
func TestDecideRootlessHeadNeverSnoozes(t *testing.T) {
	t.Parallel()

	p := DefaultPolicy()
	th := Thread{State: StateOpening, LastSentSeq: 4, NextSeq: 6, RootLanded: false}
	it := item(5, true, 0)

	now := created
	for pass := range 10 {
		d := Decide(it, th, now, p)
		require.Falsef(t, waits(d.Action),
			"pass %d asked the caller to snooze on a root that nothing can post", pass)
		require.Equal(t, ActionRecoverThread, d.Action)
		// The caller snoozes and comes back; nothing about the thread has changed.
		now = now.Add(p.RootWait)
	}
}
