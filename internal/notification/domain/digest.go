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

// DigestLookback is `L`: HOW LATE A CASE MAY COMMIT AND STILL BE COUNTED.
//
// ⭐⭐ IT IS THE WHOLE OF THE FIX FOR git-bug `a8a4010`, AND IT IS A RE-SCAN RATHER
// THAN A LAG. `alert_cases.started_at` is oto's clock read at the START of a batch's
// processing — `ingestion/service/process.go` takes one `now` in `plan` and stamps
// every alert in the batch with it — so the instant on the row is BEFORE the
// transaction that inserted it, by decoding, the rejection write, chunking, grouping,
// the lifecycle machine and an `fsync`. A tick that reads the window `[T, T+W)` a
// fraction of a second after `T+W` therefore cannot see a Case whose `started_at`
// falls inside the window but whose transaction had not committed yet; the old
// predicate then never looked at that instant again, because every later window
// started at or after the boundary the Case was on the wrong side of.
//
// So a digest for `[T, T+W)` reads `[T-L, T+W)` and drops what it has already
// reported:
//
//	digest = Cases in [T, T+W)  ∪  Cases in [T-L, T)  minus those already reported
//
// ⛔ IT IS NOT A DELAY AND MUST NEVER BECOME ONE. The alternative shape — never
// digest a window that closed less than `G` ago — was refused by the owner because
// it converts one unmeasurable number into PERMANENT SILENT LOSS: a Case is lost
// exactly when `commit_delay > G`, and the effective margin is
// `G - (reader_clock - writer_clock)`, so inter-pod skew eats the budget invisibly
// (git-bug `b21ba93` is this repo's standing record of paying that tax). Under a
// re-scan, exceeding `L` produces a DUPLICATE rather than a hole, and a visible
// duplicate beats an invisible omission. Being generous is therefore the safe
// direction, which is why the number below is orders of magnitude above the gap it
// is sized against.
//
// ⭐ TWO MINUTES, AND EVERY TERM IN THE CHOICE IS STATED. It is twice the 60 s
// `notify.digest` tick (SPEC §G.3), so a Case that commits at any point before the
// NEXT tick after its window closed is still counted. It is 40% of
// `MinDigestWindow`, so the tail can never be a larger read than the window itself
// even for the tightest admissible configuration. And it is three orders of
// magnitude above the plan-to-commit gap, which is milliseconds to seconds.
//
// ⚠️ IT IS CHOSEN AND NOT DERIVED, AND NO MEASURED DISTRIBUTION IS OWED. Nothing in
// the schema can measure commit lateness: `started_at` is stamped app-side before
// the write, and the only database-stamped column is `created_at DEFAULT now()`,
// where a transaction-scoped `now()` returns transaction START time and so moves the
// WRONG WAY — the slower the commit, the earlier the stamp. Measuring it would need
// a `clock_timestamp()` column, which is most of the visibility mechanism the
// bounded lookback exists to avoid. `a8a4010`'s "named constant with the gap
// distribution it was sized against" clause is conditional on choosing a LAG; this
// is not a lag, so the clause that applies is its second arm — "or is counted by a
// later one".
//
// ⛔ IT IS NOT `MaxDigestBackfill` UNDER ANOTHER NAME, AND COLLAPSING THE TWO WOULD
// DELETE A DOCUMENTED BEHAVIOUR. `L` is the STRAGGLER budget — how late one Case may
// commit, sized against write visibility, seconds. `MaxDigestBackfill` is the OUTAGE
// horizon — how many owed WINDOWS a recovered tick still emits, sized against how
// long oto was gone, many windows. A single number doing both jobs would silently
// discard the whole catch-up after an outage longer than `L`.
//
// ⚠️ IT LIVES HERE AND NOT IN `platform/tuning`. That package is for a shipped
// default SEVERAL packages read; this one is read by this domain and the service that
// calls it, and `tuning`'s own header says a default only one package reads stays
// where it lives. It is also not a per-org setting: an operator cannot usefully
// reason about their own commit latency, and a tenant that set it to zero would be
// asking for the bug back.
const DigestLookback = 2 * time.Minute

// LookbackStart is the inclusive instant a digest for the window at `start` begins
// READING at — the window's start pulled back by DigestLookback.
//
// It is a separate concept from the window: everything in `[LookbackStart, start)`
// has already been offered to an earlier digest, so a Case in the tail is counted
// only if no digest reported it. See DigestLookback.
func (d Digest) LookbackStart(start time.Time) time.Time { return start.Add(-DigestLookback) }

// DigestReconcileHorizon is how far back the reconciliation detector looks for a
// matched Case that no digest ever reported.
//
// ⭐ IT IS THE HALF THAT MAKES THE BOUNDED LOOKBACK DEFENSIBLE RATHER THAN HOPEFUL.
// `L` downgrades a missed Case from an invisible hole to a duplicate — but only for
// lateness under `L`. Past that the Case is still lost, and pre-release the goal is
// AUDITABLE rather than provably correct: if it happens, it must be found from a
// number somebody can alarm on and not from a customer. The detector is what
// produces that number.
//
// One day, because that is `MaxDigestWindow`: a policy on the widest admissible
// window has not necessarily digested at all inside a shorter horizon, so a shorter
// one would report its ordinary in-flight Cases as gaps.
const DigestReconcileHorizon = MaxDigestWindow

// DigestMarkRetention is how long a per-Case digest mark is kept.
//
// ⚠️ DEDUPE IS NOT WHAT SETS THIS NUMBER, WHICH IS WHY IT IS NOT `DigestLookback`.
// Three things read a mark and they need three different depths:
//
//   - DEDUPING THE TAIL needs `DigestLookback` — two minutes.
//   - RE-EXAMINING A WINDOW needs the WIDEST WINDOW A POLICY MAY HOLD. Widening
//     `digest_window_s` re-floors the coverage instant backward onto the enclosing
//     wide boundary, so a window whose earlier half was already reported is examined
//     again (git-bug `342e071`, the milder direction). It folds to zero only while
//     the marks for that half still exist, which is up to `MaxDigestWindow` old.
//   - THE DETECTOR needs `DigestReconcileHorizon`, because the absence of a mark is
//     the only evidence it has.
//
// So it is the largest of the three plus an hour of slack, which is what lets a
// detector run that is itself late still find its evidence. It is emphatically NOT
// forever: a mark is one narrow row per (policy, Case) and the retention is what
// keeps the write amplification proportional to RECENT activity rather than to all
// of history, which is the saving that made the bounded lookback cheaper than a
// permanent membership record.
const DigestMarkRetention = MaxDigestWindow + time.Hour

// WindowOrdinal is the window's index since the epoch, and it is what a digest
// Notification stores in `state_version`.
//
// §C.7 defines `state_version` as the version of the SUBJECT this intent was minted
// against. For the three signal subjects that is `alert_groups.state_version`; for
// a subject that IS a window, the ordinal is exactly that version — it increments
// once per window and never repeats. Using it keeps the §C.7 key shape unchanged
// while making one window's digest a different intent from the next one's.
//
// ⭐ IT SATISFIES notifications_sver_ck (>= 1) FOR EVERY WINDOW `DigestWindows` CAN
// PRODUCE, and that is a statement about the caller rather than about this
// arithmetic. The ordinal of the window starting at the epoch is 0 — integer
// division, for every window length — so the bound holds because `DigestWindows`
// excludes that one window, not because the division cannot reach it. This comment
// used to end "for every window after the first of 1970", and that qualifier was
// the defect: the escape clause was written down and then not enforced anywhere.
// Call this with a hand-built epoch start and you still get 0.
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
// just happened" message and a stale one is worse than none.
//
// ⛔ HOW MANY WERE ABANDONED IS NO LONGER RETURNED, AND THE GAP IS REPORTED BETTER
// FOR IT (git-bug `893cee4`). This function used to hand back a second value, and
// `sweepPolicy` logged it as `skipped_windows` under a comment arguing that a damper
// which cannot report itself is the silent suppression §B.6 refuses. That argument
// was sound and the NUMBER was not: because an unsent window advanced no cursor, a
// namespace nobody had paged about since last Tuesday produced an "owed" span of
// thousands of windows, so every tick logged a data-loss report about a backlog
// nothing was ever owed — forever, growing by one per window, on a policy that had
// never had anything to send. An operator who alerted on that message got paged by
// every quiet namespace they had; one who did not got a log in which the single
// occurrence that meant something was buried under thousands that did not.
//
// The §B.6 requirement is met by a STRONGER fact instead. Every Case a policy
// examined carries a mark (`notification_digest_cases`), so the abandoned windows are
// not an absence that has to be inferred from a jumped cursor: their Cases are the
// ones with NO mark, and `DigestService.ReconcileOrg` counts exactly those. That is a
// number over the thing an operator actually cares about — episodes nobody was told
// about — rather than over windows, it is non-zero only when something was really
// lost, and it is safe to alert on.
const MaxDigestBackfill = 6

// DigestWindows is the list of CLOSED windows this policy owes at `now`, oldest
// first, given the INSTANT its coverage has reached.
//
// ⭐⭐ `coveredTo` IS AN INSTANT AND NOT A WINDOW START, AND THAT IS THE WHOLE OF
// git-bug `342e071`. This line used to read
//
//	first := d.WindowStart(lastCovered).Add(d.Window)
//
// where `lastCovered` was `max(notifications.digest_window_start)` — the START of the
// last window a digest was sent for. A start is meaningless without the LENGTH that
// was in force when it was sent, and the length was stored nowhere, so the
// arithmetic above supplied the CURRENT one. Narrow a policy from an hour to ten
// minutes and the hour that was already summarised is re-tiled into six new windows
// that all sit after the recorded start, all of which are therefore treated as
// uncovered: five fresh digests about a span the operator has already read, each its
// own thread reply, arriving at the exact moment somebody was tuning the policy
// because it was already too noisy. Neither guard catches it — `notif_digest_uniq`
// keys on the start and `12:10` is not `12:00`, and `WindowOrdinal` divides by the
// current length so the §C.7 key differs too. Both were working as designed against
// a repeated tick, and a re-tiling is not one.
//
// An INSTANT does not change meaning when the tiling changes. `WindowStart(coveredTo)`
// is the window CONTAINING the instant coverage reached, which is the first window
// that is not fully covered — so narrowing steps forward from where the long digest
// actually ended, and widening re-examines only the wide window whose earlier half
// was already reported. That re-examination is deliberate and it sends nothing: every
// Case in the overlap already carries a mark, so the fold is zero and the floor is
// not cleared. The literal reading of `342e071`'s done-when is still violated — the
// spans DO overlap — and the operator-visible clause it exists for is satisfied,
// because no second message is produced.
//
// `coveredTo` is the zero time when the policy has never digested. A brand-new policy
// then gets exactly ONE window — the most recent closed one — and NOT its whole
// history: enabling a digest must not replay last week into a channel. A policy whose
// digests all predate migration 00070 reads the same way, because those rows carry no
// coverage instant and inventing one from the policy's CURRENT window is precisely
// the unsafe inference above.
//
// The window containing `now` is deliberately excluded. It is still open, its count
// would be a partial answer, and covering it would burn the idempotency key for a
// window whose real contents have not happened yet.
// ⭐ THE TRUNCATION IS THE ONLY THING THIS ADDS TO `owed`, AND IT IS THE THING THE
// TICK HAS TO REPORT. `owed` answers how many windows are outstanding; this function
// answers which ones will actually be covered, and `AbandonedWindows` answers the
// difference. Three readings of one arithmetic, so no caller can compute a drop this
// function did not make.
func (d Digest) DigestWindows(now, coveredTo time.Time) []time.Time {
	first, newest, total, ok := d.owed(now, coveredTo)
	if !ok {
		return nil
	}

	if total > MaxDigestBackfill {
		first = newest.Add(-time.Duration(MaxDigestBackfill-1) * d.Window)
		total = MaxDigestBackfill
	}

	out := make([]time.Time, 0, total)
	for w := first; !w.After(newest); w = w.Add(d.Window) {
		out = append(out, w)
	}
	return out
}

// AbandonedWindows is how many of the windows this policy OWED at `now` the
// `MaxDigestBackfill` clamp threw away — the windows `DigestWindows` did NOT return
// and no later tick will, because the coverage cursor is about to advance past them.
//
// ⭐⭐ IT IS NOT `skipped_windows` COMING BACK, AND THE DIFFERENCE IS THE WHOLE OF WHY
// IT IS ADMISSIBLE (git-bug `893cee4`). That number was a FICTION: an unsent window
// advanced no cursor, so a policy over a namespace nobody had paged about since last
// Tuesday reported an "owed" backlog of thousands of windows it had never been owed,
// forever, growing by one per window. Coverage now advances for every window that was
// EXAMINED, so `owed` is bounded by how far behind the READER is and by nothing else:
// a quiet policy is one window behind and this returns 0. Non-zero here means the tick
// really did not run for more than `MaxDigestBackfill` windows, and the windows really
// are gone.
//
// ⚠️ IT IS A COUNT OF WINDOWS AND SO IT IS THE WEAKER FACT, WHICH IS WHY IT IS NOT AN
// ALERTABLE NUMBER. What an operator cares about is EPISODES nobody was told about,
// and that is `DigestService.ReconcileOrg`'s `unreported_episodes` — non-zero only
// when something was really lost. This is the tick saying out loud, once, at the
// moment it happens, that it is about to advance past windows it never looked at.
// Leaving that to be inferred from a jumped cursor is what made the abandonment
// silent.
func (d Digest) AbandonedWindows(now, coveredTo time.Time) int {
	_, _, total, ok := d.owed(now, coveredTo)
	if !ok || total <= MaxDigestBackfill {
		return 0
	}
	return total - MaxDigestBackfill
}

// owed is the closed-window arithmetic itself: the OLDEST window still outstanding,
// the NEWEST closed one, and how many windows that span holds — before any clamp.
//
// `ok` false means nothing is owed at all, which is the ordinary answer on all but the
// first tick of each window.
func (d Digest) owed(now, coveredTo time.Time) (first, newest time.Time, total int, ok bool) {
	if !d.Enabled() {
		return time.Time{}, time.Time{}, 0, false
	}

	// The newest window that has CLOSED: one step back from the one `now` is in.
	newest = d.WindowStart(now).Add(-d.Window)

	// ⛔ THE EPOCH WINDOW IS EXCLUDED, NOT MERELY EVERYTHING BEFORE IT, AND THE
	// `!After` IS THE WHOLE FIX (git-bug 7c7ff0b). `WindowOrdinal` is integer
	// division of the start's Unix seconds by the window length, so the window that
	// STARTS at the epoch has ordinal 0 for every window length — and a digest
	// stores that ordinal in `state_version`, where `notifications_sver_ck` has
	// required >= 1 since 00011. A `Before` guard admits exactly that one window:
	// when `now` falls in the SECOND window since 1970, `newest` is precisely
	// time.Unix(0,0), the guard does not fire, and the insert dies with a 23514
	// naming a constraint the caller believed it satisfied.
	//
	// Excluding the window here rather than rebasing the ordinal is deliberate. The
	// ordinal feeds the §C.7 idempotency key, so making it 1-based would change
	// `state_version` on stored digest rows and risk re-minting an intent for a
	// window already sent. Losing the single window of 1970-01-01 costs nothing.
	epoch := time.Unix(0, 0).UTC()
	if !newest.After(epoch) {
		return time.Time{}, time.Time{}, 0, false
	}

	if coveredTo.IsZero() {
		return newest, newest, 1, true
	}

	first = d.WindowStart(coveredTo)
	// ⛔ THE SAME EPOCH EXCLUSION, APPLIED TO `first` AS WELL, BECAUSE THE INSTANT
	// SEMANTICS MADE IT REACHABLE FROM A SECOND DIRECTION. The guard above only rules
	// out the case where the NEWEST closed window is the epoch one. Under the old
	// arithmetic `first` was a start plus one whole window, so it could never BE the
	// epoch window; under `WindowStart(coveredTo)` a coverage instant inside the first
	// window of 1970 floors straight onto it, and its ordinal is 0 for every window
	// length — which `notifications_sver_ck` (>= 1) refuses as a 23514 naming a
	// constraint the caller believed it satisfied. Unreachable with a real clock, and
	// so was the bug this mirrors (git-bug 7c7ff0b), which is the argument for
	// writing it down rather than reasoning that nothing produces it.
	if !first.After(epoch) {
		first = epoch.Add(d.Window)
	}
	if first.After(newest) {
		// Already covered up to the newest closed window. This is the ordinary case
		// on all but the first tick of each window, and it is why the tick is cheap.
		//
		// ⭐ IT IS ALSO WHAT MAKES A QUIET POLICY CHEAP, WHICH IT WAS NOT (git-bug
		// `893cee4`). Coverage is now recorded for every window that was EXAMINED
		// rather than only for every window that was SENT, so a namespace nobody has
		// paged about since last Tuesday arrives here with `coveredTo` one window
		// behind `now` and does exactly one comparison — instead of re-deriving a
		// span thousands of windows long, clamping it to six, and running six
		// aggregate queries per tick forever to re-answer a question whose answer
		// could not have changed.
		return time.Time{}, time.Time{}, 0, false
	}

	return first, newest, int(newest.Sub(first)/d.Window) + 1, true
}

// UnreportableBefore is the instant before which a Case this policy MATCHED can no
// longer be reported by any digest — the exact bound the reconciliation detector has
// to use, and the whole of the fix for a WARN that fired on a healthy install.
//
// ⭐⭐ IT IS PER POLICY BECAUSE THE TICK IS. `DigestService.ReconcileOrg` used to end
// its candidate span at `now - DigestLookback` for the whole tenant, and every episode
// in the currently-OPEN window was then reported as a gap BY CONSTRUCTION: the tick
// only ever examines CLOSED windows (`owed` above steps back one whole window from
// `WindowStart(now)`), so nothing inside the open one can carry a mark yet. On a
// healthy install with `digest_window_s = 86400` that is up to a day of episodes
// warned about every hour forever — the exact false-alarm shape git-bug `893cee4` was
// filed against, on the one number `DigestGap` claims is safe to alert on. A
// tenant-wide instant cannot express this bound at all: `W` is a per-policy column.
//
// Two facts make a Case unreportable, and the bound is the LATER of them:
//
//   - THE CURSOR HAS PASSED IT. Every future window this policy examines reads from
//     `coveredTo - L` at the earliest (`LookbackStart` of the next window to be
//     examined is exactly that), so a Case that opened before `coveredTo - L` will
//     never be read again.
//   - IT HAS FALLEN OFF THE BACKFILL HORIZON. Even with a frozen cursor, the oldest
//     window a later tick will ever cover starts at `WindowStart(now) - B*W` (see
//     MaxDigestBackfill), and it reads from `L` before that. Anything older is gone
//     however far behind the cursor is — which is what keeps a policy whose channel
//     was disabled for a day visible to the detector instead of hiding behind a
//     cursor that stopped moving.
//
// ⚠️ THE ZERO TIME MEANS "ASK NOTHING", AND IT IS NOT THE SAME AS "NO GAP". A policy
// with no coverage instant has never been examined, and `DigestWindows` gives such a
// policy exactly ONE window — the most recent closed one — precisely so that enabling
// a digest does not replay last week into a channel. Everything older was never owed,
// so reporting it as unreported would make every newly created digest policy announce
// a day-long gap. The cost is stated rather than hidden: a policy whose tick has never
// once run is invisible here, and what makes that acceptable is that it is invisible
// for one tick — the first sweep writes a cursor whether or not it sends anything.
func (d Digest) UnreportableBefore(now, coveredTo time.Time) time.Time {
	if !d.Enabled() || coveredTo.IsZero() {
		return time.Time{}
	}
	reach := coveredTo.UTC()
	if horizon := d.WindowStart(now).Add(-time.Duration(MaxDigestBackfill) * d.Window); horizon.After(reach) {
		reach = horizon
	}
	return d.LookbackStart(reach)
}
