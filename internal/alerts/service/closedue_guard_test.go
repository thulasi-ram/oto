package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
)

// These are the tests for the DELAYED CLOSE's stand-down guard (migration 00057).
//
// ⭐ THEY NEED NOTHING FROM THE HARNESS. `unclosable` is a pure function over the
// row the sweep is about to overwrite, and asking it a question is the whole of
// what makes "the delayed close SPENDS a resolution and can never mint one"
// mechanical rather than aspirational. TestMain still stands the container up for
// the rest of this package (main_test.go); these cases do not touch it.
//
// ⛔ IT IS A DELIBERATE DUPLICATE of the checks inside `domain.Apply`'s due-close
// branch, for the reason `unreapable` states: Apply answers about whatever case it
// is handed, and the failure being guarded is Apply being handed a snapshot that
// had stopped being true between the candidate scan and the transaction.

// guardCase builds one `alert_cases` row as the sweep would have re-read it.
// Everything the guard reads is a parameter; nothing else matters to it.
type guardCase struct {
	closedAs     domain.ResolveReason // zero leaves the episode OPEN
	pendingAt    time.Time
	pendingEndAt time.Time
}

func (g guardCase) build(t *testing.T, startedAt time.Time) domain.Case {
	t.Helper()
	p := domain.CaseParams{
		ID:                  uuid.MustParse("018f3a4b-0000-7000-8000-0000000002a1"),
		OrgID:               uuid.MustParse("018f3a4b-0000-7000-8000-0000000002a2"),
		AlertID:             uuid.MustParse("018f3a4b-0000-7000-8000-0000000002a3"),
		Seq:                 1,
		State:               domain.CaseOpen,
		StartedAt:           startedAt,
		LastObservedAt:      startedAt,
		SourceStartsAt:      startedAt,
		ResolvePendingAt:    g.pendingAt,
		ResolvePendingEndAt: g.pendingEndAt,
		StateVersion:        3,
	}
	if !g.closedAs.IsZero() {
		p.State = domain.CaseClosed
		p.EndedAt = startedAt.Add(time.Minute)
		p.ResolveReason = g.closedAs
	}
	o, err := domain.NewCase(p)
	require.NoError(t, err)
	return o
}

// TestTheDelayedCloseStandsDownWhenTheRowItselfDisprovesIt pins every refusal
// `unclosable` can name, and the sentence each one says.
//
// ⭐⭐ THE SECOND ROW IS THE FEATURE WORKING, NOT A FAILURE. "The alert re-fired
// inside the retention window" is what a damped flap looks like from the sweep's
// side: T2 cleared the receipt, the episode is carrying the flap, and standing down
// is exactly what W was configured to buy. It is the reason that path logs at debug
// rather than at info.
//
// ⛔ AND THE FIRST ROW KEEPS `resolved` AND `expired` APART in the sentence it
// logs, exactly as `unreapable`'s does: "already closed" would collapse the one
// distinction 00007 says oto must never blur.
func TestTheDelayedCloseStandsDownWhenTheRowItselfDisprovesIt(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	dueAt := started.Add(10 * time.Minute)
	upstreamEnd := started.Add(2 * time.Minute)
	now := dueAt.Add(30 * time.Second)

	tests := []struct {
		name string
		row  guardCase
		want string
	}{
		{
			name: "an open episode whose window has elapsed justifies the close",
			row:  guardCase{pendingAt: dueAt, pendingEndAt: upstreamEnd},
			want: "",
		},
		{
			name: "the alert re-fired inside the window, so the receipt is gone",
			row:  guardCase{},
			want: "the alert re-fired inside the retention window",
		},
		{
			name: "a fresh resolve moved the due time forward",
			row:  guardCase{pendingAt: now.Add(time.Minute), pendingEndAt: upstreamEnd},
			want: "the retention window has not elapsed",
		},
		{
			name: "somebody already closed it as resolved",
			row:  guardCase{closedAs: domain.ResolveUpstream},
			want: "case is already resolved",
		},
		{
			name: "somebody already closed it as expired",
			row:  guardCase{closedAs: domain.ResolveTimeout},
			want: "case is already expired",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, unclosable(tc.row.build(t, started), now))
		})
	}
}

// TestTheCloseIsDueAtTheInstantTheWindowElapses — the sweep's own boundary. The
// candidate scan selects `resolve_pending_at <= now`, so the guard must agree that
// the instant itself is due or the sweep would hand itself the same row for ever.
func TestTheCloseIsDueAtTheInstantTheWindowElapses(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	dueAt := started.Add(10 * time.Minute)
	row := guardCase{pendingAt: dueAt, pendingEndAt: started.Add(time.Minute)}.build(t, started)

	assert.Equal(t, "the retention window has not elapsed",
		unclosable(row, dueAt.Add(-time.Nanosecond)))
	assert.Empty(t, unclosable(row, dueAt), "the boundary is due, as the candidate scan reads it")
	assert.Empty(t, unclosable(row, dueAt.Add(time.Nanosecond)))
}
