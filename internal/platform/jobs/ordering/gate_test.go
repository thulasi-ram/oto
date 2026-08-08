package ordering

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/clock"
)

// memStore is an in-memory ordering.Store. The gate is stateless by design, so a
// map is a complete substitute for the two tables behind it.
type memStore struct {
	thread   Thread
	slots    map[int]Slot
	advanced []struct {
		seq    int
		item   uuid.UUID
		reason SkipReason
	}
}

func (m *memStore) LoadThread(context.Context, uuid.UUID) (Thread, error) { return m.thread, nil }

func (m *memStore) SlotAt(_ context.Context, _ uuid.UUID, seq int) (Slot, error) {
	return m.slots[seq], nil
}

func (m *memStore) Advance(_ context.Context, _ uuid.UUID, seq int, id uuid.UUID, r SkipReason) error {
	m.advanced = append(m.advanced, struct {
		seq    int
		item   uuid.UUID
		reason SkipReason
	}{seq, id, r})
	m.thread.LastSentSeq = seq
	return nil
}

func newTestGate(t *testing.T, store Store, now time.Time) *Gate {
	t.Helper()
	g, err := NewGate(GateConfig{
		Store:           store,
		Policy:          DefaultPolicy(),
		StaleClaimLease: DefaultStaleClaimLease,
		Clock:           clock.NewFake(now),
	})
	require.NoError(t, err)
	return g
}

func TestClassifySlot(t *testing.T) {
	t.Parallel()

	now := created
	fresh := now.Add(-time.Second)
	stale := now.Add(-2 * DefaultStaleClaimLease)

	cases := []struct {
		name    string
		slot    Slot
		thread  Thread
		reason  SkipReason
		advance bool
	}{
		{
			name:   "a dead thread skips everything still queued",
			slot:   Slot{Present: true, InFlight: true, UpdatedAt: fresh},
			thread: Thread{State: StateDead},
			reason: ReasonThreadDead, advance: true,
		},
		{
			name:   "a seq no row occupies can never be filled",
			slot:   Slot{Present: false},
			thread: Thread{State: StateOpen},
			reason: ReasonMissingDelivery, advance: true,
		},
		{
			// A slot that WAS delivered is not a gap. Calling it one wrote
			// `delivery.skipped` onto an append-only timeline for a message the
			// destination is currently displaying, and raised the metric documented
			// as "sustained non-zero means a channel is broken".
			name:   "a sent slot is convergence, not a skip",
			slot:   Slot{Present: true, Resolved: true, Sent: true},
			thread: Thread{State: StateOpen},
			reason: ReasonAlreadySent, advance: true,
		},
		{
			name:   "a skipped slot is a deliberate non-send",
			slot:   Slot{Present: true, Resolved: true, Skipped: true},
			thread: Thread{State: StateOpen},
			reason: ReasonSkippedDelivery, advance: true,
		},
		{
			name:   "a dead slot is a delivery that reached nobody",
			slot:   Slot{Present: true, Resolved: true},
			thread: Thread{State: StateOpen},
			reason: ReasonDeadDelivery, advance: true,
		},
		{
			name:   "a live claim is not ours to move",
			slot:   Slot{Present: true, InFlight: true, UpdatedAt: fresh},
			thread: Thread{State: StateOpen},
			reason: "", advance: false,
		},
		{
			name:   "a claim past its lease is ambiguous and stays put",
			slot:   Slot{Present: true, InFlight: true, UpdatedAt: stale},
			thread: Thread{State: StateOpen},
			reason: "", advance: false,
		},
		{
			name:   "a pending slot belongs to the dispatcher",
			slot:   Slot{Present: true, UpdatedAt: fresh},
			thread: Thread{State: StateOpen},
			reason: "", advance: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGate(t, &memStore{}, now)
			reason, advance := g.classifySlot(tc.slot, tc.thread, now)
			require.Equal(t, tc.advance, advance)
			require.Equal(t, tc.reason, reason)
		})
	}
}

func TestSkipReasonAnomalous(t *testing.T) {
	t.Parallel()
	require.False(t, ReasonAlreadySent.Anomalous())
	for _, r := range []SkipReason{
		ReasonDeadDelivery, ReasonSkippedDelivery, ReasonMissingDelivery, ReasonThreadDead,
	} {
		require.Truef(t, r.Anomalous(), "%s means something was not delivered", r)
	}
}

// TestRecoverNamesTheSlotThatOwnsTheHead is the other half of the wedge.
//
// Recovery correctly refuses to skip a `pending` slot — doing so would send a
// reply before the message it replies to — but it used to report the stall only
// for an in-flight claim. A pending delivery whose job had been discarded was
// therefore invisible to the one caller that could re-enqueue it, and the caller
// answered with another attempt-free snooze, forever.
func TestRecoverNamesTheSlotThatOwnsTheHead(t *testing.T) {
	t.Parallel()

	now := created
	stuck := uuid.New()
	store := &memStore{
		thread: Thread{State: StateOpen, LastSentSeq: 1, NextSeq: 5, RootLanded: true},
		slots: map[int]Slot{
			2: {Present: true, Resolved: true, Sent: true},
			3: {Present: true, ItemID: stuck, UpdatedAt: now.Add(-time.Hour)},
			4: {Present: true, Resolved: true},
		},
	}

	rec, err := newTestGate(t, store, now).Recover(context.Background(), uuid.New())
	require.NoError(t, err)

	require.Equal(t, 1, rec.Advanced, "the head moves to the pending slot and stops there")
	require.Equal(t, 2, rec.To)
	require.Equal(t, 3, rec.StalledAt)
	require.Equal(t, stuck, rec.StalledItem, "the caller cannot restart a delivery it is not told about")
	require.False(t, rec.StalledInFlight)

	require.Len(t, store.advanced, 1)
	require.Equal(t, ReasonAlreadySent, store.advanced[0].reason)
}
