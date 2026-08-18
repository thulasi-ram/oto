package domain

import (
	"time"
)

// ⛔ BINDING (SCOPE-BOUNDARY §4.8, §5.3). A DIGEST IS A WINDOW, NEVER A SCHEDULE
// OF WHEN OTO IS ALLOWED TO SPEAK.
//
// The window in this file selects WHICH FACTS A SUMMARY COVERS. It must never
// become a predicate on whether an ordinary notification is delivered: the moment
// a policy can say "only tell me between 09:00 and 17:00" oto has a quiet-hours
// feature, which is a rota by another name and is the boundary FR-1 draws. The
// structural guarantee is that `Digest` below is consulted by ONE caller — the
// digest tick — and by nothing on the evaluation path. `matchers` still cannot
// reference a time of day (see the binding block in policy.go).
//
// There is also no timezone here, and there never will be. Windows are aligned in
// UTC. A per-policy timezone is the first half of quiet hours.

// Digest window bounds, mirrored from policies_digest_window_ck and
// policies_digest_floor_ck. They are validated in Go as well as in the database so
// that a bad policy comes back as a field-level error rather than as a 23514.
const (
	// MinDigestWindow is five minutes. Below that a digest is the per-event stream
	// it exists to replace, wearing a delay.
	MinDigestWindow = 5 * time.Minute
	// MaxDigestWindow is one day, which is also the alignment period: every
	// admissible window divides it (see DigestWindowAligned).
	MaxDigestWindow = 24 * time.Hour
	// MinDigestFloor is 1 — "send if anything at all happened". Zero is not a floor
	// but a request to send for an empty window, which never happens.
	MinDigestFloor = 1
	// MaxDigestFloor is the ceiling policies_digest_floor_ck enforces.
	MaxDigestFloor = 10000
)

// digestAlignmentPeriod is the span every window length must divide: one UTC day.
//
// ⭐ IT IS WHAT MAKES EPOCH ALIGNMENT AND WALL-CLOCK ALIGNMENT THE SAME THING. The
// Unix epoch is midnight UTC, so a window length that divides 86400 has every
// boundary land on a wall-clock boundary and no window straddles midnight. That is
// why the constraint is a divisor rule rather than a plain range.
const digestAlignmentPeriod = 24 * time.Hour

// DigestWindowAligned reports whether d is a length whose windows tile the UTC day
// exactly — the Go half of policies_digest_window_ck.
//
// It refuses a length that is not a whole number of seconds as well, because the
// column is `INT` seconds and a fractional duration would be silently truncated
// into a different policy than the one somebody wrote.
func DigestWindowAligned(d time.Duration) bool {
	if d <= 0 || d%time.Second != 0 {
		return false
	}
	return digestAlignmentPeriod%d == 0
}

// Digest is a policy's window and its floor: "summarise what matched me over
// Window, and stay silent unless at least Floor things happened".
//
// ⭐ IT IS THE ONLY PART OF A POLICY WITH A CLOCK IN IT, AND THE CLOCK POINTS AT
// THE FACTS RATHER THAN AT THE OPERATOR. `Window` bounds which Cases a summary
// counts. It does not decide when oto may speak — see the binding block above.
type Digest struct {
	// Window is `digest_window_s`. Zero means this policy sends no digest, which is
	// the default and is exactly the behaviour that shipped before migration 00058.
	Window time.Duration
	// Floor is `digest_floor`: the minimum number of Cases that must have OPENED
	// inside the window. Zero means no floor — send whenever the window was not
	// empty. It counts CASES rather than alerts or notifications; migration 00058
	// carries the argument.
	Floor int
}

// Enabled reports whether this policy digests at all.
//
// ⚠️ THE FLOOR ALONE IS NOT ENOUGH AND CANNOT BE. A floor with no window is a
// threshold over an unbounded span, which is not a question anybody asked;
// policies_digest_pair_ck refuses the row, and this method refuses to act on one
// that somehow exists.
func (d Digest) Enabled() bool { return d.Window > 0 }

// Clears reports whether a window's count is enough to send.
//
// An EMPTY window never sends, floor or no floor: a digest saying "nothing
// happened" every ten minutes forever is the noise a digest exists to remove, and
// silence here is not oto withholding anything — there is nothing to withhold.
func (d Digest) Clears(count int) bool {
	if count <= 0 {
		return false
	}
	return count >= d.Floor
}

// WindowStart is the inclusive start of the window containing t, aligned to the
// epoch in UTC.
//
// ⭐ IT IS COMPUTED, NEVER REMEMBERED, AND THAT IS THE WHOLE DESIGN. The tick keeps
// no durable state beyond "which window was last covered" precisely because this
// function is total: every pod, at every instant, derives the same boundary from
// the clock alone. Aligning to the policy's `created_at` instead would make the
// boundary a property of a configuration row — two ten-minute policies would report
// on two different ten minutes, and recreating a policy would shift every future
// boundary and could re-open a window already covered.
//
// The zero Duration returns t unchanged rather than dividing by zero. A caller with
// a disabled digest has no window to ask about, and `Enabled` is the gate.
func (d Digest) WindowStart(t time.Time) time.Time {
	if !d.Enabled() {
		return t.UTC()
	}
	secs := int64(d.Window / time.Second)
	u := t.UTC().Unix()
	// Floor division, so a pre-1970 instant does not round the wrong way. Nothing
	// produces one, and a boundary function that is wrong for half the number line
	// is a trap for whoever first writes a test with a zero time.
	start := u - u%secs
	if u < 0 && u%secs != 0 {
		start -= secs
	}
	return time.Unix(start, 0).UTC()
}

// WindowEnd is the EXCLUSIVE end of the window that starts at start.
func (d Digest) WindowEnd(start time.Time) time.Time { return start.Add(d.Window) }

// WindowOrdinal is the window's index since the epoch, and it is what a digest
// Notification stores in `state_version`.
//
// §C.7 defines `state_version` as the version of the SUBJECT this intent was minted
// against. For the three signal subjects that is `alert_groups.state_version`; for
// a subject that IS a window, the ordinal is exactly that version — it increments
// once per window and never repeats. Using it keeps the §C.7 key shape unchanged
// while making one window's digest a different intent from the next one's, and it
// satisfies notifications_sver_ck (>= 1) for every window after the first of 1970.
func (d Digest) WindowOrdinal(start time.Time) int {
	if !d.Enabled() {
		return 0
	}
	return int(start.UTC().Unix() / int64(d.Window/time.Second))
}

// MaxDigestBackfill is how many closed windows one tick will cover for one policy.
//
// ⭐⭐ IT IS THE ANSWER TO "WHAT HAPPENS WHEN A TICK IS MISSED", AND IT IS NEITHER
// "ALL" NOR "NONE".
//
//   - NONE — only ever the newest closed window — makes the cursor pointless: it
//     would guard against re-sending and not against skipping, and a deploy that
//     took twelve minutes would silently lose a ten-minute window with nothing
//     recording that it had.
//   - ALL is an outage amplifier. A policy on a five-minute window that was down
//     for a day owes 288 digests, each with its own thread reply, and they would
//     all arrive in the same second — the flood a digest exists to prevent,
//     produced by the digest's own catch-up.
//
// So a bounded number of windows is covered per policy per tick. Six is chosen
// because the tick runs every minute and the shortest window is five minutes: six
// windows is at least half an hour of catch-up for the tightest configuration, which
// covers a deploy, a leader election, a failover and a short database incident —
// every outage whose digests are still worth reading — while capping one tick's
// work at six queries per policy however long the process was gone.
//
// ⚠️ WHEN MORE THAN THIS IS OWED, THE NEWEST WINDOWS WIN AND THE GAP IS PERMANENT.
// Covering the OLDEST six first would catch up eventually but would spend hours
// posting summaries of a morning nobody is looking at any more; a digest is a "what
// just happened" message and a stale one is worse than none. The abandoned span is
// logged and is visible in the data — the cursor jumps, and the windows that were
// not covered simply have no row — which is the §B.6 requirement that oto's quiet
// be inspectable rather than invisible.
const MaxDigestBackfill = 6

// DigestWindows is the list of CLOSED windows this policy owes at `now`, oldest
// first, given the newest window it has already covered.
//
// `lastCovered` is the zero time when the policy has never digested. A brand-new
// policy then gets exactly ONE window — the most recent closed one — and NOT its
// whole history: enabling a digest must not replay last week into a channel.
//
// The window containing `now` is deliberately excluded. It is still open, its count
// would be a partial answer, and covering it would burn the idempotency key for a
// window whose real contents have not happened yet.
//
// The second result reports how many older windows were ABANDONED to the
// MaxDigestBackfill cap, so the caller can say so out loud.
func (d Digest) DigestWindows(now, lastCovered time.Time) ([]time.Time, int) {
	if !d.Enabled() {
		return nil, 0
	}

	// The newest window that has CLOSED: one step back from the one `now` is in.
	newest := d.WindowStart(now).Add(-d.Window)
	if newest.Before(time.Unix(0, 0)) {
		return nil, 0
	}

	if lastCovered.IsZero() {
		return []time.Time{newest}, 0
	}

	first := d.WindowStart(lastCovered).Add(d.Window)
	if first.After(newest) {
		// Already covered up to the newest closed window. This is the ordinary case
		// on all but the first tick of each window, and it is why the tick is cheap.
		return nil, 0
	}

	total := int(newest.Sub(first)/d.Window) + 1
	abandoned := 0
	if total > MaxDigestBackfill {
		abandoned = total - MaxDigestBackfill
		first = newest.Add(-time.Duration(MaxDigestBackfill-1) * d.Window)
		total = MaxDigestBackfill
	}

	out := make([]time.Time, 0, total)
	for w := first; !w.After(newest); w = w.Add(d.Window) {
		out = append(out, w)
	}
	return out, abandoned
}
