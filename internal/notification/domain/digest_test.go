package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// These tests pin the four decisions migration 00058 and `domain/digest.go` made
// that nothing else can restate: the window is aligned to the WALL CLOCK, the
// floor counts CASES, a missed tick backfills a BOUNDED number of windows, and one
// window is exactly one intent.
//
// They are pure. A digest's arithmetic is a function of a duration and an instant,
// so a database can only make the same statements slower — and the two facts that
// genuinely need one (the bucket query counts `alert_cases`, and
// `notif_digest_uniq` refuses the second row) are named where they are asserted.

// digestPolicy is `validPolicy` with a window on it, so a test that breaks the
// digest is testing the digest and not the six fields Validate also checks.
//
// The `digest` Reason comes with the window deliberately: policies_digest_reason_ck
// makes the window imply the Reason, so a policy carrying one without the other is
// not a weaker digest policy but an unstorable row.
func digestPolicy(window time.Duration, floor int) domain.Policy {
	p := validPolicy()
	p.Reasons = []domain.Reason{domain.ReasonFired, domain.ReasonDigest}
	p.Digest = domain.Digest{Window: window, Floor: floor}
	return p
}

// admissibleWindows is every window length an operator is likely to write, and
// every one of them divides the day. The list is the vocabulary the alignment rule
// admits, not a sample of it.
var admissibleWindows = []time.Duration{
	5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 20 * time.Minute,
	30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour,
	6 * time.Hour, 8 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// TestADigestWindowTilesTheUTCDay is the alignment decision: a window boundary is a
// WALL-CLOCK boundary, derived from the instant alone.
//
// ⭐ THE REJECTED ALTERNATIVE IS WHAT MAKES THIS WORTH ASSERTING. Aligning to the
// policy's `created_at` would make the boundary a property of a configuration row:
// two ten-minute policies would report on two different ten minutes, and recreating
// a policy would shift every future boundary — including backwards, over a window
// whose digest had already been sent. Epoch alignment makes `WindowStart` total, so
// every pod at every instant derives the same answer with no state at all.
func TestADigestWindowTilesTheUTCDay(t *testing.T) {
	t.Parallel()

	// One ordinary instant, one exactly on a midnight boundary, one late in the day
	// where a badly-chosen window would straddle midnight, and one before the epoch
	// — which is not a real instant for oto but is the half of the number line
	// integer division rounds the wrong way on.
	instants := []time.Time{
		time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC),
		time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 23, 59, 59, 0, time.UTC),
		time.Date(1969, 12, 31, 23, 12, 5, 0, time.UTC),
	}

	for _, w := range admissibleWindows {
		require.Truef(t, domain.DigestWindowAligned(w),
			"%s does not divide the day. Every length in this list is one an operator "+
				"can write, so a rule that refuses it is a rule that outlaws a legal policy", w)

		d := domain.Digest{Window: w}
		secs := int64(w / time.Second)

		for _, at := range instants {
			start := d.WindowStart(at)

			assert.Zerof(t, start.Unix()%secs,
				"the %s window containing %s starts at %s, which is not a multiple of the "+
					"window since the epoch — so it is not a wall-clock boundary and two pods "+
					"could disagree about which window an instant is in", w, at, start)
			assert.Falsef(t, start.After(at),
				"the %s window containing %s starts AFTER it (%s)", w, at, start)
			assert.Truef(t, d.WindowEnd(start).After(at),
				"the %s window containing %s ends at %s, before the instant it contains",
				w, at, d.WindowEnd(start))
			assert.Equalf(t, time.UTC, start.Location(),
				"the %s window start is not in UTC. There is no timezone in a digest and "+
					"there never will be: a per-policy timezone is the first half of quiet "+
					"hours", w)
			assert.Equalf(t, start, d.WindowStart(start),
				"WindowStart is not idempotent for %s: asking about a boundary instant "+
					"must return that boundary", w)

			// The window is HALF-OPEN. The end instant belongs to the NEXT window, which
			// is what stops an episode opening exactly on a boundary being counted twice.
			end := d.WindowEnd(start)
			assert.Equalf(t, end, d.WindowStart(end),
				"the instant %s closes one %s window and opens the next; if it belonged to "+
					"the earlier one, adjacent windows would both count it", end, w)
		}

		// No window straddles midnight, which is the property the divisor rule buys.
		midnight := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
		assert.Equalf(t, midnight, d.WindowStart(midnight),
			"midnight does not begin a %s window, so some window spans two days", w)
	}
}

// TestAWindowThatDoesNotDivideTheDayIsRefused is the other half of alignment: a
// length that cannot tile the day is refused at the door, with a field name.
//
// ⚠️ IT IS REFUSED IN GO AS WELL AS IN THE DDL ON PURPOSE. policies_digest_window_ck
// would catch these too, but as a 23514 an operator has to decode; the settings form
// needs a violation it can point a control at.
func TestAWindowThatDoesNotDivideTheDayIsRefused(t *testing.T) {
	t.Parallel()

	// Each of these is inside the 300..86400 range and still not a divisor of 86400,
	// so the ONLY thing that can refuse them is the alignment rule.
	for _, w := range []time.Duration{
		7 * time.Minute,         // 420s
		11 * time.Minute,        // 660s
		time.Hour + time.Second, // 3601s
		25 * time.Minute,        // 1500s
		7 * time.Hour,           // 25200s
		2500 * time.Millisecond, // not a whole number of seconds either
	} {
		assert.Falsef(t, domain.DigestWindowAligned(w),
			"%s was accepted as a window length. 86400 %% %d != 0, so its boundaries "+
				"drift across the day and no two of them are the same wall-clock time", w,
			int64(w/time.Second))
	}

	p := digestPolicy(7*time.Minute, 0)
	err := p.Validate()
	require.Error(t, err,
		"a seven-minute digest window was accepted. It does not divide the day, so its "+
			"boundaries walk, and policies_digest_window_ck would refuse the row as a 23514 "+
			"after the form had already said it was fine")

	v, ok := violation(err, "digest_window_seconds", "alignment")
	require.Truef(t, ok,
		"the refusal does not name digest_window_seconds/alignment: %v — the path has to be "+
			"the JSON name a client can map onto a control, never the column name",
		err)
	assert.Contains(t, v.Message, "wall-clock",
		"the message does not say what the rule IS FOR. `not a divisor of 86400` is "+
			"meaningless on its own; the reason is that every boundary has to be a wall-clock "+
			"boundary in UTC, and an operator reading the form is owed that sentence")

	// And its aligned neighbours are fine, so the rule is refusing the LENGTH rather
	// than the digest.
	require.NoError(t, digestPolicy(5*time.Minute, 0).Validate(),
		"five minutes is the shortest admissible window and must validate")
	require.NoError(t, digestPolicy(10*time.Minute, 0).Validate(),
		"a ten-minute window is the modal digest and must validate")
}

// TestADigestIsAbsentByDefault is the ticket's done-when #1: a policy without a
// window behaves exactly as every policy written before migration 00058.
func TestADigestIsAbsentByDefault(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	require.NoError(t, p.Validate())

	assert.False(t, p.Digest.Enabled(),
		"the zero Digest reports itself as enabled, so every policy that never asked "+
			"for one would be swept by the tick")
	assert.False(t, p.Digests(),
		"a policy with no window digests. `ListWithDigest` selects on `digest_window_s`, "+
			"so this is the value that decides whether the tick has anything to do")

	windows := p.Digest.DigestWindows(
		time.Date(2026, 8, 18, 13, 47, 0, 0, time.UTC), time.Time{})
	assert.Empty(t, windows, "a policy with no window owes windows")
}

// TestAnIncompleteDigestIsRefused covers the three ways a digest can be half-written.
// Each is a DDL constraint restated in Go, and each names the field it is about.
func TestAnIncompleteDigestIsRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		policy func() domain.Policy
		field  string
		code   string
	}{
		{
			// policies_digest_pair_ck. A floor with no window is a threshold over an
			// unbounded span, which is not a question anything can evaluate.
			name: "a floor with no window",
			policy: func() domain.Policy {
				p := validPolicy()
				p.Digest = domain.Digest{Floor: 5}
				return p
			},
			field: "digest_floor", code: "incomplete",
		},
		{
			// policies_digest_reason_ck. A window on a policy whose `reasons` omit
			// `digest` mints an intent that is suppressed as `no_policy` once per
			// window, forever.
			name: "a window the policy does not route",
			policy: func() domain.Policy {
				p := validPolicy()
				p.Digest = domain.Digest{Window: 10 * time.Minute}
				return p
			},
			field: "reasons", code: "required",
		},
		{
			name:   "a window below the floor of five minutes",
			policy: func() domain.Policy { return digestPolicy(time.Minute, 0) },
			field:  "digest_window_seconds", code: "range",
		},
		{
			name:   "a window longer than the alignment period",
			policy: func() domain.Policy { return digestPolicy(25*time.Hour, 0) },
			field:  "digest_window_seconds", code: "range",
		},
		{
			name:   "a floor above the ceiling",
			policy: func() domain.Policy { return digestPolicy(time.Hour, domain.MaxDigestFloor+1) },
			field:  "digest_floor", code: "range",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.policy().Validate()
			require.Errorf(t, err, "%s was accepted", tc.name)
			_, ok := violation(err, tc.field, tc.code)
			require.Truef(t, ok, "%s was refused without naming %s/%s: %v",
				tc.name, tc.field, tc.code, err)
		})
	}
}

// TestNothingIsSentForAWindowBelowItsFloor is the floor decision.
//
// ⭐ AN EMPTY WINDOW NEVER SENDS, FLOOR OR NO FLOOR, and that is not oto withholding
// anything: there is nothing to withhold. A digest saying "nothing happened" every
// ten minutes forever is the noise a digest exists to remove.
func TestNothingIsSentForAWindowBelowItsFloor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		floor int
		count int
		want  bool
	}{
		{name: "an empty window with no floor", floor: 0, count: 0, want: false},
		{name: "an empty window with a floor", floor: 1, count: 0, want: false},
		{name: "a negative count cannot clear anything", floor: 0, count: -1, want: false},
		{name: "one case with no floor", floor: 0, count: 1, want: true},
		{name: "one case below the floor", floor: 5, count: 1, want: false},
		{name: "one short of the floor", floor: 5, count: 4, want: false},
		{name: "exactly the floor", floor: 5, count: 5, want: true},
		{name: "over the floor", floor: 5, count: 6, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := domain.Digest{Window: 10 * time.Minute, Floor: tc.floor}
			assert.Equalf(t, tc.want, d.Clears(tc.count),
				"a window with %d cases against a floor of %d: the floor is a MINIMUM, so "+
					"the boundary value clears it and an empty window never does",
				tc.count, tc.floor)
		})
	}
}

// TestABrandNewPolicyDigestsExactlyOneWindow: enabling a digest must not replay last
// week into a channel.
//
// The zero `coveredTo` means "never examined", and the honest reading of it is ONE
// window — the most recent closed one — rather than everything since the policy was
// created. The latter would make turning the feature on an outage of its own.
//
// ⭐ IT IS ALSO THE UPGRADE PATH FOR MIGRATION 00070, WHICH IS WHY IT MATTERS MORE THAN
// IT USED TO. `notification_digest_coverage` starts EMPTY: a coverage instant cannot be
// backfilled from `digest_window_start` without the window length that was in force
// when each digest was sent, which is the fact nothing stored (git-bug `342e071`). So
// on the first tick after the upgrade every policy that has ever digested arrives here
// with the zero time, and this assertion is what says that costs at most one digest
// each rather than a replay of the whole history.
func TestABrandNewPolicyDigestsExactlyOneWindow(t *testing.T) {
	t.Parallel()

	d := domain.Digest{Window: 10 * time.Minute}
	now := time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC)

	windows := d.DigestWindows(now, time.Time{})

	require.Len(t, windows, 1,
		"a policy that has never been examined was offered %d windows. The cursor's zero "+
			"value means `cover one window`, never `cover everything since the epoch` — "+
			"enabling a digest, or upgrading past 00070, would otherwise post a week of "+
			"summaries in one second", len(windows))
	assert.Equal(t, time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC), windows[0],
		"the one window must be the most recent CLOSED one; 13:40 is still open and its "+
			"count would be a partial answer")
}

// TestAMissedTickCoversAtMostSixWindowsAndAbandonsTheOldest is the backfill decision,
// and it pins both halves of it.
//
// ⭐⭐ NEITHER "ALL" NOR "NONE". Covering only the newest closed window makes the
// cursor pointless — it would guard against re-sending and not against skipping.
// Covering ALL of them is an outage amplifier: a five-minute policy down for a day
// owes 288 digests, each with a thread reply, all arriving in the same second, which
// is the flood a digest exists to prevent produced by the digest's own catch-up.
//
// ⚠️ WHEN MORE IS OWED, THE NEWEST WIN AND THE GAP IS PERMANENT. A digest is a "what
// just happened" message and a stale one is worse than none.
//
// ⛔ HOW MANY WERE ABANDONED IS NO LONGER RETURNED, AND THIS TEST STOPPED ASSERTING IT
// (git-bug `893cee4`). The second result used to be logged as `skipped_windows` under
// the argument that a damper which cannot report itself is the silent suppression §B.6
// refuses. The argument was sound; the number was not. Because an unsent window advanced
// no cursor, a policy over a quiet namespace produced an "owed" span of thousands of
// windows, so every tick logged a data-loss report about a backlog nothing was ever
// owed. The §B.6 requirement is now met by a stronger fact — the abandoned windows'
// episodes are the ones carrying NO mark, which `DigestService.ReconcileOrg` counts —
// and that is a number over episodes nobody was told about rather than over windows.
// The BOUND itself is unchanged and is still asserted here, because it is the reason
// `MaxDigestBackfill` and the straggler budget had to stay two separate numbers.
func TestAMissedTickCoversAtMostSixWindowsAndAbandonsTheOldest(t *testing.T) {
	t.Parallel()

	d := domain.Digest{Window: 10 * time.Minute}
	now := time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC)
	// Examination reached 11:10; everything from 11:10 to 13:30 is owed — fifteen
	// windows.
	coveredTo := time.Date(2026, 8, 18, 11, 10, 0, 0, time.UTC)

	windows := d.DigestWindows(now, coveredTo)

	require.Len(t, windows, domain.MaxDigestBackfill,
		"one tick offered %d windows for one policy. The bound is what stops a deploy that "+
			"took an hour from becoming an hour of back-dated summaries arriving at once",
		len(windows))

	want := []time.Time{
		time.Date(2026, 8, 18, 12, 40, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 12, 50, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 10, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC),
	}
	assert.Equal(t, want, windows,
		"the covered windows must be the NEWEST six, contiguous and oldest-first. Covering "+
			"the oldest six instead would catch up eventually while spending the next hour "+
			"posting summaries of a morning nobody is looking at any more")
}

// TestTheOpenWindowIsNeverDigested, and neither is a window already covered.
//
// The window containing `now` is still open: its count is a partial answer, and
// covering it would burn the idempotency key for a window whose contents have not
// finished happening.
//
// ⭐⭐ AND THE CURSOR IS NOW AN INSTANT, SO THE BOUNDARY VALUE MOVED BY ONE WINDOW
// (git-bug `342e071`, migration 00070). It used to be a window START — "the newest
// window a digest was sent for" — and this test passed `13:30` to mean "13:30 is
// covered". It is now the EXCLUSIVE instant coverage reached, so the same fact is
// spelled `13:40`. That is not a cosmetic re-spelling: a start is a span only in
// combination with the window LENGTH that was in force when it was written, so
// re-flooring it under a NEW `digest_window_s` re-tiled a span an earlier digest had
// already summarised and reported every episode in it again. An instant does not change
// meaning when the tiling changes.
func TestTheOpenWindowIsNeverDigested(t *testing.T) {
	t.Parallel()

	d := domain.Digest{Window: 10 * time.Minute}
	now := time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC)

	windows := d.DigestWindows(now, time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC))
	require.Equal(t, []time.Time{time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC)}, windows,
		"the open 13:40 window was offered. Its count is whatever has happened so far, and "+
			"the row it would write is the one the real window can never replace")

	// Coverage already at the END of the newest closed window: nothing is owed. This is
	// the answer on all but the first tick of each window, and it is what makes a
	// once-a-minute tick affordable — and, since 00070, what makes a policy over a
	// namespace nobody has paged about for a week cost one comparison instead of six
	// queries (git-bug `893cee4`).
	windows = d.DigestWindows(now, time.Date(2026, 8, 18, 13, 40, 0, 0, time.UTC))
	assert.Empty(t, windows, "a window already covered was offered again")

	// ⭐ AN INSTANT STRICTLY INSIDE A WINDOW RE-OFFERS THAT WINDOW, AND THAT REVERSES
	// WHAT THIS ASSERTION USED TO SAY. Under start semantics `13:35` had to mean the
	// same thing as `13:30`, because a cursor one second off would otherwise re-send a
	// window. Under instant semantics it means something else and something real:
	// coverage stopped in the MIDDLE of `[13:30, 13:40)`, which is what a `digest_window_s`
	// edit leaves behind, so that window is NOT fully covered and must be examined
	// again. Re-examining it sends nothing — every episode in the part already covered
	// carries a mark, so the fold is zero and the floor is not cleared — which is how
	// `342e071`'s operator-visible clause is satisfied even though the spans do overlap.
	windows = d.DigestWindows(now, time.Date(2026, 8, 18, 13, 35, 0, 0, time.UTC))
	assert.Equal(t, []time.Time{time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC)}, windows,
		"an instant strictly inside a window means coverage stopped there, so the window is "+
			"only partly covered and has to be re-examined. Skipping it would leave the "+
			"second half of a re-tiled window reported by nothing")
}

// TestNarrowingTheWindowDoesNotReReportWhatTheLongerDigestCovered is git-bug `342e071`
// in one function, in both directions.
//
// ⛔ THE FAILURE WAS AS BAD AS A DIGEST BUG GETS. A policy on a one-hour window sends
// the digest for `[12:00, 13:00)` at 13:01. An operator narrows it to ten minutes —
// admissible, because `DigestWindowAligned` accepts any divisor of 24 h at or above
// `MinDigestWindow`, and both 3600 and 600 qualify. On the next tick the stored cursor
// `12:00` was re-floored under the NEW length, so `first` became `12:10`, and five
// windows inside the hour that had already been summarised were all treated as
// uncovered: five real thread replies about a span the operator had just read, at the
// exact moment they were tuning the policy because it was already too noisy.
//
// Neither existing guard could catch it. `notif_digest_uniq` keys on the window start
// and `12:10` is not `12:00`; `WindowOrdinal` divides by the current length so the §C.7
// key differed too. Both were working exactly as designed against a repeated tick, and
// a re-tiling is not one.
func TestNarrowingTheWindowDoesNotReReportWhatTheLongerDigestCovered(t *testing.T) {
	t.Parallel()

	// The hourly digest for [12:00, 13:00) was sent at 13:01, so coverage reached the
	// INSTANT 13:00 — which is the fact the old cursor could not hold.
	coveredTo := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 18, 13, 5, 0, 0, time.UTC)

	narrowed := domain.Digest{Window: 10 * time.Minute}
	windows := narrowed.DigestWindows(now, coveredTo)

	for _, w := range windows {
		assert.Falsef(t, w.Before(coveredTo),
			"the narrowed policy was offered window %s, which is inside the hour the 13:01 "+
				"digest already summarised. Every Case in [12:10, 13:00) would be reported a "+
				"second time, in five separate thread replies, and neither notif_digest_uniq "+
				"nor the §C.7 key can refuse them because both are derived from the CURRENT "+
				"window length", w.UTC())
	}
	assert.Empty(t, windows,
		"nothing has closed since 13:00 under a ten-minute window — 12:50 is the newest "+
			"closed window at 13:05 and it is behind the coverage instant — so the correct "+
			"answer is that the narrowing owes nothing at all")

	// ⚠️ WIDENING IS THE MILDER DIRECTION AND IS NOT SILENT EITHER. `WindowStart` floors
	// BACKWARD onto the enclosing wide boundary, so a wide window whose earlier half was
	// already covered by short digests is offered again. That is deliberate: it is the
	// only way the LATER half gets reported at all. What stops it double-reporting is the
	// per-Case mark, not the window arithmetic — every episode in the covered half folds
	// to zero — which is why `342e071`'s done-when is satisfied in effect rather than in
	// letter.
	//
	// ⚠️ `now` IS 13:05 AND THE HOUR MATTERS AS MUCH AS THE MINUTE. The claim being
	// pinned is about the RE-OFFER, so `now` has to sit in the hour immediately after
	// the one coverage stopped inside: at 13:05 the newest CLOSED hourly window is
	// exactly `[12:00, 13:00)`, so a single-element answer is the re-offer and nothing
	// else. Put `now` an hour later and `[13:00, 14:00)` has closed too and is genuinely
	// owed — a correct two-element answer that says nothing about widening, which is the
	// second assertion below rather than a weakening of this one.
	widened := domain.Digest{Window: time.Hour}
	windows = widened.DigestWindows(
		time.Date(2026, 8, 18, 13, 5, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 12, 50, 0, 0, time.UTC))
	require.Equal(t, []time.Time{time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)}, windows,
		"widening must re-offer the enclosing wide window: coverage stopped at 12:50, so "+
			"[12:50, 13:00) belongs to no digest yet and only the 12:00 hour contains it. "+
			"Skipping it would lose ten minutes permanently")

	// ⭐ AND THE RE-OFFER DOES NOT SWALLOW THE WINDOWS BEHIND IT, WHICH IS THE OTHER WAY
	// THIS COULD HAVE BEEN IMPLEMENTED AND WOULD HAVE LOST AN HOUR. A tick that arrives
	// at 14:05 with the same coverage instant owes TWO hours: `[12:00, 13:00)` because
	// coverage stopped in the middle of it, and `[13:00, 14:00)` because it closed with
	// no coverage in it at all. Returning only the enclosing wide window — treating the
	// re-flooring as a replacement for the ordinary backfill rather than as its starting
	// point — would drop the 13:00 hour permanently, and nothing downstream could notice:
	// the marks that make the re-examination silent are exactly what a fully uncovered
	// hour does not have.
	windows = widened.DigestWindows(
		time.Date(2026, 8, 18, 14, 5, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 12, 50, 0, 0, time.UTC))
	require.Equal(t, []time.Time{
		time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC),
	}, windows,
		"the re-offered wide window is the OLDEST owed one, not the ONLY one. Every hour "+
			"that closed between it and `now` is still owed, and an hour nobody covered is "+
			"an hour whose episodes carry no mark, so dropping it is silent loss rather "+
			"than a silent re-examination")
}

// TestOneWindowIsOneIntent is idempotency at the level this package owns: the §C.7
// key.
//
// ⭐ `state_version` CARRIES THE WINDOW ORDINAL, and that is what makes one window's
// digest a different intent from the next one's WITHOUT changing the key's shape.
// The database says the same thing twice — `notifications_idem_uniq` on the hash and
// `notif_digest_uniq` on (org, policy, window_start) — and the second index is why
// the tick treats a bare 23505 as "already covered" (asserted in the service tests).
func TestOneWindowIsOneIntent(t *testing.T) {
	t.Parallel()

	var (
		org    = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000d1")
		policy = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000d2")
		other  = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000d3")
	)

	d := domain.Digest{Window: 10 * time.Minute}
	first := time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC)
	next := time.Date(2026, 8, 18, 13, 40, 0, 0, time.UTC)

	require.Equal(t, d.WindowOrdinal(first)+1, d.WindowOrdinal(next),
		"the ordinal must step by exactly one per window. It is the subject's version, so "+
			"a gap or a repeat is a version that either skips an intent or reuses one")
	require.GreaterOrEqual(t, d.WindowOrdinal(first), 1,
		"notifications_sver_ck requires >= 1, and every window after the first of 1970 is")

	key := func(subject uuid.UUID, start time.Time) string {
		// No occasion: a digest's discriminator IS its window ordinal, carried in
		// `state_version`, so `uuid.Nil` and the key is the one it has always been.
		return domain.IdempotencyKey(
			org, domain.SubjectDigest, subject, domain.ReasonDigest, d.WindowOrdinal(start), uuid.Nil)
	}

	assert.Equal(t, key(policy, first), key(policy, first.Add(9*time.Minute)),
		"two instants inside one window minted two different intents. Any instant in the "+
			"window has to fold to the same key, or a retry a minute later posts the digest "+
			"a second time")
	assert.NotEqual(t, key(policy, first), key(policy, next),
		"two consecutive windows share an idempotency key, so the second window's digest "+
			"would be swallowed as a duplicate of the first and never sent")
	assert.NotEqual(t, key(policy, first), key(other, first),
		"two policies digesting the same window share a key. `subject_id` is the POLICY "+
			"half of the pair — one of them would silently lose its digest")

	// A disabled digest has no window to be asked about, and the ordinal must not be
	// a number that could collide with a real one.
	assert.Zero(t, (domain.Digest{}).WindowOrdinal(first),
		"a policy with no window reported a window ordinal")
}

// TestTheDigestReasonIsAboutAWindowRatherThanAnObject.
//
// ⚠️ IT DOES NOT RESTATE `TestTheReasonSubjectAllocationIsTotal`, which is in-package
// and asks whether the allocation covers the closed Reason set EXACTLY. This asks the
// narrower question that test cannot: that `digest` is ALONE at its altitude. The
// kind exists because the question has no object, so a second Reason here would be a
// second way to ask the same thing — and it would also be a Reason whose row has no
// `group_id`, which `notifications_target_ck` admits for `digest` alone.
func TestTheDigestReasonIsAboutAWindowRatherThanAnObject(t *testing.T) {
	t.Parallel()

	require.Equal(t, domain.SubjectDigest, domain.ReasonDigest.Subject(),
		"`digest` is not about a window. Its subject is the pair (policy, window_start), "+
			"which is the whole reason migration 00058 added a fourth subject kind")
	require.True(t, domain.SubjectDigest.Valid(),
		"`digest` is not an admitted subject kind, so notifications_subjkind_ck would "+
			"refuse every row the tick writes")
	assert.True(t, domain.ReasonDigest.Valid())
	assert.False(t, domain.ReasonDigest.AlertScoped(),
		"a digest was made to name one alert. It names a namespace over a window and has "+
			"no alert_id to give notifications_focus_ck")

	var atThisAltitude []domain.Reason
	for _, r := range domain.AllReasons() {
		if r.Subject() == domain.SubjectDigest {
			atThisAltitude = append(atThisAltitude, r)
		}
	}
	assert.Equal(t, []domain.Reason{domain.ReasonDigest}, atThisAltitude,
		"exactly one Reason may be about a window, and it is `digest`. A second one would "+
			"be a second way to ask the same question — and every reader that checks "+
			"`Subject() == SubjectDigest` before dereferencing GroupID would have to learn "+
			"its name")
}

// TestAnExplicitZeroDigestFloorIsRefusedOnThePatchPath is git-bug 2270b48.
//
// The create path was never the hole: its DTO carries
// `validate:"omitempty,min=1,max=10000"`, so anyone testing the obvious path sees
// the right error and concludes the bound is enforced. The patch path had no tag,
// and `Policy.Validate` could not close it either, because `Digest.Floor` spends
// zero on "unset" -- so an explicit zero and an explicit null fold to the same
// value and become indistinguishable one line before the check.
//
// They are NOT the same to the database: null writes SQL NULL, zero writes a
// literal 0, and `policies_digest_floor_ck` refuses it as a 23514 -- a 500 with a
// constraint name and no field path, which is the failure the whole validation
// layer exists to prevent. So the case is asserted HERE, on the patch, where the
// two are still distinct.
func TestAnExplicitZeroDigestFloorIsRefusedOnThePatchPath(t *testing.T) {
	t.Parallel()

	floor := func(v int) **int { p := &v; return &p }
	cleared := func() **int { var p *int; return &p }

	t.Run("an explicit zero is refused, naming the field", func(t *testing.T) {
		t.Parallel()

		err := domain.PolicyPatch{DigestFloor: floor(0)}.ValidateExplicit()
		require.Error(t, err, "digest_floor: 0 was accepted, and the database will not accept it")
		_, ok := violation(err, "digest_floor", "range")
		require.True(t, ok,
			"refused without naming digest_floor/range, so the settings form has no "+
				"control to point at -- which is the whole difference from a 23514: %v", err)
	})

	t.Run("a negative floor is refused", func(t *testing.T) {
		t.Parallel()

		err := domain.PolicyPatch{DigestFloor: floor(-1)}.ValidateExplicit()
		require.Error(t, err, "a negative digest_floor was accepted")
	})

	// ⭐ THE OTHER HALF, AND THE REASON THIS IS NOT JUST A RANGE CHECK. Clearing the
	// floor is a real instruction -- "send whenever the window was not empty" -- and
	// it is spelled with the same JSON field. A fix that refused zero by refusing
	// everything falsy would take this with it.
	t.Run("an explicit null clears the floor and is accepted", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, domain.PolicyPatch{DigestFloor: cleared()}.ValidateExplicit())
	})

	t.Run("a floor inside the range is accepted", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, domain.PolicyPatch{DigestFloor: floor(domain.MinDigestFloor)}.ValidateExplicit())
		require.NoError(t, domain.PolicyPatch{DigestFloor: floor(domain.MaxDigestFloor)}.ValidateExplicit())
	})

	t.Run("a patch that does not mention the floor is accepted", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, domain.PolicyPatch{}.ValidateExplicit())
	})
}

// TestNoWindowDigestWindowsProducesHasAnOrdinalBelowOne is git-bug 7c7ff0b.
//
// `WindowOrdinal` is integer division of the start's Unix seconds by the window
// length, so the window that STARTS at the epoch has ordinal 0 for EVERY window
// length -- and a digest stores that ordinal in `state_version`, where
// `notifications_sver_ck` has required >= 1 since 00011.
//
// ⛔ THE EPOCH WINDOW WAS REACHABLE FROM `DigestWindows`, NOT MERELY CONSTRUCTIBLE
// BY HAND. The guard read `newest.Before(epoch)` -- before, not at-or-before -- so
// when `now` fell in the SECOND window since 1970, `newest` was exactly the epoch,
// the guard did not fire, and the insert died with a 23514 naming a constraint the
// caller believed it satisfied. Unreachable in production, where `now` is not
// 1970; entirely reachable from a test, because oto injects its clock everywhere
// precisely so a test can put it wherever it likes, and `time.Unix(0, 0)` is the
// obvious thing to reach for.
//
// The bound is asserted over the EPOCH rather than over a comfortable timestamp,
// because a comfortable timestamp is what let this stand.
func TestNoWindowDigestWindowsProducesHasAnOrdinalBelowOne(t *testing.T) {
	t.Parallel()

	epoch := time.Unix(0, 0).UTC()

	for _, w := range []time.Duration{
		5 * time.Minute, 10 * time.Minute, time.Hour, 24 * time.Hour,
	} {
		d := domain.Digest{Window: w, Floor: 1}

		// The ordinal of the epoch window is the value the CHECK refuses. Asserted
		// so the exclusion below is protecting against something real.
		if got := d.WindowOrdinal(epoch); got != 0 {
			t.Fatalf("window %s: WindowOrdinal(epoch) = %d, want 0 — the premise of "+
				"this test is that the epoch window is the one the CHECK refuses", w, got)
		}

		// `now` inside the SECOND window since the epoch: the exact position that
		// used to make `newest` land on the epoch itself.
		now := epoch.Add(w).Add(w / 2)
		// ⭐ BOTH ARMS, BECAUSE THE INSTANT CURSOR MADE THE EPOCH WINDOW REACHABLE FROM
		// A SECOND DIRECTION (migration 00070). The zero cursor is the arm the original
		// bug lived in. A NON-ZERO cursor inside the first window of 1970 is the new
		// one: `first` used to be a start plus a whole window, so it could never BE the
		// epoch window, and it is now `WindowStart(coveredTo)`, which floors straight
		// onto it.
		for _, coveredTo := range []time.Time{{}, epoch.Add(w / 3)} {
			starts := d.DigestWindows(now, coveredTo)
			assertOrdinals(t, d, w, starts)
		}
	}
}

// assertOrdinals is the shared half of the test above: every window it produced must
// carry an ordinal `notifications_sver_ck` admits.
func assertOrdinals(t *testing.T, d domain.Digest, w time.Duration, starts []time.Time) {
	t.Helper()
	for _, start := range starts {
		if ord := d.WindowOrdinal(start); ord < 1 {
			t.Errorf("window %s: DigestWindows produced start %s with ordinal %d, "+
				"which notifications_sver_ck (>= 1) refuses — the insert fails as a "+
				"23514 with no field name and takes the whole tick for that policy",
				w, start.UTC(), ord)
		}
	}
}
