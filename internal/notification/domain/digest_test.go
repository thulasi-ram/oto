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

	windows, abandoned := p.Digest.DigestWindows(
		time.Date(2026, 8, 18, 13, 47, 0, 0, time.UTC), time.Time{})
	assert.Empty(t, windows, "a policy with no window owes windows")
	assert.Zero(t, abandoned)
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
// The zero `lastCovered` means "never digested", and the honest reading of it is ONE
// window — the most recent closed one — rather than everything since the policy was
// created. The latter would make turning the feature on an outage of its own.
func TestABrandNewPolicyDigestsExactlyOneWindow(t *testing.T) {
	t.Parallel()

	d := domain.Digest{Window: 10 * time.Minute}
	now := time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC)

	windows, abandoned := d.DigestWindows(now, time.Time{})

	require.Len(t, windows, 1,
		"a policy that has never digested was offered %d windows. The cursor's zero value "+
			"means `cover one window`, never `cover everything since the epoch` — enabling a "+
			"digest would otherwise post a week of summaries in one second", len(windows))
	assert.Equal(t, time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC), windows[0],
		"the one window must be the most recent CLOSED one; 13:40 is still open and its "+
			"count would be a partial answer")
	assert.Zero(t, abandoned,
		"a brand-new policy abandoned nothing: it was never owed the windows before it existed")
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
// ⚠️ WHEN MORE IS OWED, THE NEWEST WIN. A digest is a "what just happened" message
// and a stale one is worse than none — so the gap is permanent, and the count of
// what was dropped comes back so the caller can say so out loud.
func TestAMissedTickCoversAtMostSixWindowsAndAbandonsTheOldest(t *testing.T) {
	t.Parallel()

	d := domain.Digest{Window: 10 * time.Minute}
	now := time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC)
	// 11:00 was covered; everything from 11:10 to 13:30 is owed — fifteen windows.
	last := time.Date(2026, 8, 18, 11, 5, 0, 0, time.UTC)

	windows, abandoned := d.DigestWindows(now, last)

	require.Len(t, windows, domain.MaxDigestBackfill,
		"one tick offered %d windows for one policy. The bound is what stops a deploy that "+
			"took an hour from becoming an hour of back-dated summaries arriving at once",
		len(windows))
	assert.Equal(t, 9, abandoned,
		"fifteen windows were owed and six were covered, so nine were abandoned. The number "+
			"is what the tick logs: a damper that cannot report itself is the silent "+
			"suppression §B.6 refuses")

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
func TestTheOpenWindowIsNeverDigested(t *testing.T) {
	t.Parallel()

	d := domain.Digest{Window: 10 * time.Minute}
	now := time.Date(2026, 8, 18, 13, 47, 29, 0, time.UTC)

	windows, abandoned := d.DigestWindows(now, time.Date(2026, 8, 18, 13, 20, 0, 0, time.UTC))
	require.Equal(t, []time.Time{time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC)}, windows,
		"the open 13:40 window was offered. Its count is whatever has happened so far, and "+
			"the row it would write is the one the real window can never replace")
	assert.Zero(t, abandoned)

	// The cursor already at the newest closed window: nothing is owed. This is the
	// answer on all but the first tick of each window, and it is what makes a
	// once-a-minute tick affordable.
	windows, abandoned = d.DigestWindows(now, time.Date(2026, 8, 18, 13, 30, 0, 0, time.UTC))
	assert.Empty(t, windows, "a window already covered was offered again")
	assert.Zero(t, abandoned)

	// The cursor is a window START in the data, but an instant INSIDE the covered
	// window must mean the same thing — otherwise a clock skew of one second would
	// re-offer a window that had already been sent.
	windows, _ = d.DigestWindows(now, time.Date(2026, 8, 18, 13, 35, 0, 0, time.UTC))
	assert.Empty(t, windows,
		"an instant inside the last covered window re-offered that window; the cursor is "+
			"read through WindowStart precisely so it cannot")
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
		return domain.IdempotencyKey(
			org, domain.SubjectDigest, subject, domain.ReasonDigest, d.WindowOrdinal(start))
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
		starts, abandoned := d.DigestWindows(now, time.Time{})
		if abandoned != 0 {
			t.Errorf("window %s: %d abandoned windows before 1970 has closed", w, abandoned)
		}
		for _, start := range starts {
			if ord := d.WindowOrdinal(start); ord < 1 {
				t.Errorf("window %s: DigestWindows produced start %s with ordinal %d, "+
					"which notifications_sver_ck (>= 1) refuses — the insert fails as a "+
					"23514 with no field name and takes the whole tick for that policy",
					w, start.UTC(), ord)
			}
		}
	}
}
