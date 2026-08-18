package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
)

// This is the test for the REAPER's stand-down on a held upstream resolve
// (migration 00057, §B.3 T6).
//
// ⭐ IT NEEDS NOTHING FROM THE HARNESS, exactly as `closedue_guard_test.go` needs
// nothing: `unreapable` is a pure function over the row the reaper is about to
// overwrite. TestMain still stands the container up for the rest of this package
// (main_test.go); this case does not touch it.
//
// ⛔ AND IT ASKS `unreapable` ITSELF, not `Reap`. Going through the sweep would
// pass for the wrong reason — `expire` swallows the `KindPrecondition` error
// domain.Apply's T6 arm returns for the same row — and would therefore keep
// passing with the guard deleted. The claim being pinned is that the CANDIDATE
// SCAN's own re-proof refuses the row, so the refusal is designed rather than
// accidental.

// reapRow builds one `alert_cases` row that is reapable in every respect the
// guard can name, so the only refusal it can produce is the one under test.
func reapRow(t *testing.T, startedAt, endsAt time.Time, pendingAt, pendingEndAt time.Time) domain.Case {
	t.Helper()
	o, err := domain.NewCase(domain.CaseParams{
		ID:                  uuid.MustParse("018f3a4b-0000-7000-8000-0000000003b1"),
		OrgID:               uuid.MustParse("018f3a4b-0000-7000-8000-0000000003b2"),
		AlertID:             uuid.MustParse("018f3a4b-0000-7000-8000-0000000003b3"),
		Seq:                 1,
		State:               domain.CaseOpen,
		StartedAt:           startedAt,
		LastObservedAt:      endsAt,
		SourceStartsAt:      startedAt,
		SourceEndsAt:        endsAt,
		ResolvePendingAt:    pendingAt,
		ResolvePendingEndAt: pendingEndAt,
		StateVersion:        3,
	})
	require.NoError(t, err)
	return o
}

// TestTheReaperStandsDownOnACaseHoldingAnUpstreamResolve pins the refusal T6 and
// migration 00057 both CLAIM the candidate scan makes, against the row the reaper
// is about to overwrite.
//
// ⭐⭐ THE TWO ROWS DIFFER IN ONE FIELD. Both are open, both carry a
// `source_ends_at` older than the grace, so both are otherwise expirable — the
// first holds a receipt for an explicit upstream resolve and the second does not.
// Delete the `ClosePending` arm from `unreapable` and the first row comes back
// empty, i.e. reapable, and this test fails: `expired` would be stamped over a
// resolution oto has in hand, which is the distinction 00007 says oto must never
// blur.
func TestTheReaperStandsDownOnACaseHoldingAnUpstreamResolve(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	endsAt := started.Add(2 * time.Minute)
	grace := 5 * time.Minute
	now := endsAt.Add(grace).Add(time.Minute)

	held := reapRow(t, started, endsAt, endsAt.Add(10*time.Minute), endsAt)
	assert.True(t, held.ClosePending(), "the row under test must actually hold the receipt")
	assert.Equal(t, "case holds an upstream resolve", unreapable(held, now, grace))

	// The control. Same row without the receipt, so the guard has nothing to say
	// and the reaper proceeds — which is what makes the assertion above about the
	// receipt and not about some other precondition this row also fails.
	free := reapRow(t, started, endsAt, time.Time{}, time.Time{})
	assert.Empty(t, unreapable(free, now, grace))
}

// TestTheHeldResolveOutranksEveryOtherUnreapableReason — the arm's POSITION, not
// just its presence. A row can hold a receipt and simultaneously fail the grace
// check, and the sentence the reaper logs must be the one that explains why this
// row is off limits rather than the one that merely says "not yet".
func TestTheHeldResolveOutranksEveryOtherUnreapableReason(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	endsAt := started.Add(2 * time.Minute)
	grace := 5 * time.Minute

	// now is inside the grace, so `resolve_grace has not elapsed` also applies.
	held := reapRow(t, started, endsAt, endsAt.Add(10*time.Minute), endsAt)
	assert.Equal(t, "case holds an upstream resolve",
		unreapable(held, endsAt.Add(time.Minute), grace))
}
