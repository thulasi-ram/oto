package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⛔ BINDING (SCOPE-BOUNDARY §4.8). THIS IS A WINDOW OVER FACTS, NOT A SCHEDULE
// OVER PEOPLE.
//
// The clock in this file decides WHICH FACTS A SUMMARY COVERS. It must never decide
// whether an ordinary notification is delivered. The moment a policy can say "hold
// everything until 09:00" oto has quiet hours, quiet hours need a timezone, a
// timezone needs an owner, and an owner is a rota — the boundary FR-1 draws. The
// structural guarantee is that this service has no path to `NotificationService.
// Evaluate`'s suppressors and cannot mute anything: it only ADDS a summary.
//
// There is no timezone here either. Windows are aligned in UTC, which is the same
// alignment in every pod and the same alignment for every tenant.

// digestBucketLimit bounds one window's fold per tenant: how many GENERATIONS one
// window may contribute before the aggregate is truncated.
//
// One row per generation that had activity, so this is a bound on how many distinct
// things were broken at once rather than on how many alerts fired. 5000 is far above
// any real fleet's simultaneous generation count and is here for the same reason
// `sweepLimit` is: an unbounded aggregate is an outage the first time somebody has a
// bad night, and a bad night is exactly when this runs.
//
// ⚠️ TRUNCATION CAN ONLY UNDERCOUNT, which is the safe direction: a digest may come
// out quieter than the truth or fall below its floor and not be sent. It can never
// invent activity, and it can never turn a quiet window into a loud one.
const digestBucketLimit = 5000

// errDigestWindowCovered ends the emit transaction because another tick already
// holds this window: `notif_digest_uniq` refused the insert, which is the §C.7
// mechanism working and not a failure.
//
// ⛔ IT IS AN ERROR TO THE TRANSACTION AND TO NOBODY ELSE. A 23505 has already
// aborted the Postgres transaction by the time Go sees it, so the ONLY answer that
// can be given inside `InTx` is a non-nil error — anything else asks `db.Tx` to
// commit a transaction Postgres will only roll back (`pgx.ErrTxCommitRollback`),
// which is how "this window is already covered" used to come back out as a failure
// and cost the policy its remaining owed windows. `emit` recognises it, answers
// `false, nil`, and the tick moves to the next window exactly as it would have if
// the window had never been owed.
var errDigestWindowCovered = errors.New("this digest window is already covered")

// digestSpan is one half-open READ `[start, end)`, and it is the case cache's key.
// Both ends, because two policies with different window LENGTHS can share a start —
// 12:00 begins a ten-minute window and an hourly one — and a cache keyed on the start
// alone would serve one policy's rows to the other.
//
// ⚠️ `start` IS THE LOOKBACK START AND NOT THE WINDOW START (git-bug `a8a4010`). A
// digest for the window `[T, T+W)` reads `[T - domain.DigestLookback, T+W)`, because
// `alert_cases.started_at` is oto's clock read BEFORE the inserting transaction and a
// Case that had not committed when the previous tick read its window is invisible to
// every later window's predicate. The tail is still policy-independent — the lookback
// is a constant, not a per-policy setting — so two policies on the same window length
// still share one read, which is the whole point of the cache.
type digestSpan struct{ start, end time.Time }

// digestOutcome is what examining one window decided, and it exists because the three
// answers have three different consequences FOR THE CURSOR.
//
// ⭐ THAT DISTINCTION IS THE HALF OF `893cee4` THE ARITHMETIC DOES NOT FIX. Coverage
// must advance for a window that was examined and found quiet, or a quiet policy
// re-derives the same span forever; it must NOT advance for a window that could not be
// examined to a conclusion, or oto skips it silently. "Nowhere to send it" is the
// second kind, and it used to be the same `return false, nil` as "somebody else sent
// it" — which was harmless only because the cursor did not move for either.
type digestOutcome int

const (
	// digestSent — this call minted the row and fanned it out.
	digestSent digestOutcome = iota
	// digestCovered — another tick already holds this window. It is COVERED: the
	// other tick's digest reported its episodes and marked them, so coverage advances
	// exactly as if this call had won.
	//
	// ⚠️ IT IS ALSO WHERE A WIDENED WINDOW LOSES ITS RESIDUE, AND THE LOSS IS VISIBLE
	// RATHER THAN SILENT. Widening `digest_window_s` re-offers the enclosing wide
	// window (see domain.Digest.DigestWindows), so a policy that covered
	// `[12:00, 12:50)` as five ten-minute digests and then widened to an hour examines
	// `[12:00, 13:00)` again. The already-reported episodes fold to zero, but
	// `[12:50, 13:00)` is genuinely unreported — and `notif_digest_uniq` refuses the
	// insert, because a digest for `digest_window_start = 12:00` already exists. So
	// those ten minutes are reported by nothing. They are NOT lost quietly: their
	// episodes carry no mark, which is exactly the state `ReconcileOrg` counts. Making
	// the index admit a second row per start would mean two digests claiming the same
	// window with different lengths, which is a worse answer than a number somebody
	// can see.
	digestCovered
	// digestNoDestination — the policy's channels are all disabled or gone, so there
	// is no fact to record and no message to send. The window stays OWED and coverage
	// stops here, which is what lets re-enabling a channel inside the
	// `MaxDigestBackfill` horizon still produce the digest. Recording something
	// instead would burn the window's idempotency key, and advancing past it would
	// turn a recoverable configuration mistake into permanent silence.
	digestNoDestination
)

// DigestService is the TICK: at each window boundary it counts what matched each
// digest policy and says so once.
//
// ⭐⭐ EVALUATION IS TICK-DRIVEN AND THAT IS THE WHOLE COST ARGUMENT. Asking "has
// enough happened in the last ten minutes" event-driven means every case that opens
// re-asks the question for every policy, and getting the answer right across a
// restart means durable per-window counters and per-policy timers — state that can
// drift from the facts it counts. On a tick the count is ONE QUERY OVER ROWS ALREADY
// STORED, the floor is free because the count is already in hand, and the only
// durable state is "which window was last covered", which is `max(window_start)`
// over the digests themselves.
type DigestService struct {
	policies PolicyStore
	digests  DigestStore
	notifier *NotificationService
	limit    int
	clk      clock.Clock
	log      *slog.Logger
}

// DigestConfig is everything NewDigestService needs.
type DigestConfig struct {
	Policies PolicyStore
	Digests  DigestStore
	// Notifier is used for its FAN-OUT, not for its evaluation: a digest is not
	// routed (its policy is already known), not snoozed (a snooze is keyed by
	// `alert_key` and a digest names no alert) and not throttled by a group cap
	// (it lands on no group's thread). See sweepPolicy.
	Notifier *NotificationService
	Clock    clock.Clock
	Logger   *slog.Logger
}

// NewDigestService builds the service.
func NewDigestService(cfg DigestConfig) (*DigestService, error) {
	if cfg.Policies == nil || cfg.Digests == nil || cfg.Notifier == nil {
		return nil, errs.New(errs.KindInternal, "digest_service_deps",
			"the digest service needs a policy store, a digest store and the notification service")
	}
	s := &DigestService{
		policies: cfg.Policies, digests: cfg.Digests, notifier: cfg.Notifier,
		limit: digestBucketLimit, clk: cfg.Clock, log: cfg.Logger,
	}
	if s.clk == nil {
		s.clk = clock.New()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// SweepOrg covers every window one tenant's digest policies still owe.
//
// It is one tenant's WHOLE tick, deliberately: the worker fans the periodic out into
// one of these per live tenant (`jobs.TenantFanOut`), so there is no all-tenants loop
// here for one org's broken policy to hide inside — a tenant that fails fails alone,
// on its own retry budget, and stops nobody else.
//
// The result is how many digests were actually SENT. Zero is the overwhelmingly
// common answer and is not a failure: on all but the first tick of each window every
// policy is already covered, which is what makes a once-a-minute tick affordable.
func (s *DigestService) SweepOrg(ctx context.Context, scope db.TenantScope) (int, error) {
	policies, err := s.policies.ListWithDigest(ctx, scope)
	if err != nil {
		return 0, err
	}
	if len(policies) == 0 {
		return 0, nil
	}

	now := s.clk.Now().UTC()
	sent := 0

	// ⭐ THE CASE CACHE IS WHAT MAKES N POLICIES COST FEWER THAN N QUERIES. Two
	// policies on the same window length are asking about the same span, and WHICH
	// EPISODES OPENED does not depend on the policy — only the matcher fold and the
	// mark subtraction do. The cache is per-SWEEP and dies with it: caching across
	// ticks would be caching a window that is still open on the very first entry.
	//
	// ⚠️ THE KEY IS THE WHOLE SPAN, NOT ITS START, AND THE DIFFERENCE IS A REAL BUG.
	// A ten-minute window and an hourly one both start at 12:00, so a start-keyed
	// cache would hand the hourly policy the ten-minute rows — an hour's digest
	// reporting a sixth of its own window, silently, and only on installs that
	// happen to run two window lengths whose boundaries coincide.
	spans := map[digestSpan][]repository.DigestCase{}

	for i := range policies {
		p := policies[i]
		if !p.Digests() {
			// The database says this policy has a window (`policies_digest_idx`), so this
			// can only be a policy whose `reasons` lost `digest` or whose `subject_kinds`
			// does not bind the digest altitude — which `policies_digest_reason_ck` and
			// `policies_digest_subject_ck` forbid between them. Skip rather than mint an
			// intent that is certain to be suppressed as `no_policy`, once per window,
			// forever.
			s.log.WarnContext(ctx, "notification: a digest policy does not route the digest reason",
				slog.String("org_id", scope.OrgID().String()),
				slog.String("policy_id", p.ID.String()))
			continue
		}
		n, err := s.sweepPolicy(ctx, scope, p, now, spans)
		if err != nil {
			// ⛔ ONE POLICY'S FAILURE MUST NOT COST THE OTHERS THEIR WINDOW. The
			// windows are independent subscriptions, and coverage advances only past
			// the windows this policy actually examined, so a policy that failed here
			// is simply still owed its window and picks it up on the next tick —
			// inside `MaxDigestBackfill`, which is what that bound is for.
			s.log.ErrorContext(ctx, "notification: could not digest a policy's window",
				slog.String("org_id", scope.OrgID().String()),
				slog.String("policy_id", p.ID.String()),
				slog.String("error", err.Error()))
			continue
		}
		sent += n
	}
	return sent, nil
}

// sweepPolicy covers the closed windows one policy still owes.
//
// ⭐⭐ IT IS A BOUNDED-LOOKBACK SET OF CASES PER WINDOW, NOT A SLICE OF CLOCK, AND
// THAT ONE RESHAPE IS WHAT CLOSES ALL THREE TICKETS AT ONCE (git-bug `893cee4`,
// `342e071`, `a8a4010`; migration `00070`):
//
//		digest = Cases in [T, T+W)  ∪  Cases in [T-L, T)  minus those already accounted for
//
//	  - `a8a4010`: a Case whose transaction committed after the previous tick had read
//	    its window is inside the `L` tail of THIS window's read, carries no mark, and is
//	    counted. The old predicate never looked at that instant again.
//	  - `342e071`: the cursor is the INSTANT coverage reached rather than a window
//	    START, so narrowing `digest_window_s` steps forward from where the long digest
//	    actually ended instead of re-tiling a span it had already summarised.
//	  - `893cee4`: coverage advances for every window EXAMINED rather than only for
//	    every window SENT, so a policy over a namespace nobody has paged about for a
//	    week does one comparison per tick instead of six aggregate queries and a
//	    data-loss warning about a backlog nothing was ever owed.
//
// ⚠️ ONE OR TWO QUERIES FOR A QUIET POLICY, NOT ZERO, AND THE TICKET'S "does no
// per-window query it does not need" HAS TO BE READ THAT WAY. You cannot know a
// window is quiet without asking. What changed is that the asking happens once per
// window that has actually closed since the last tick — which for a once-a-minute
// tick and a ten-minute window is one window in ten ticks — rather than six times a
// minute forever.
func (s *DigestService) sweepPolicy(
	ctx context.Context, scope db.TenantScope, p domain.Policy, now time.Time,
	spans map[digestSpan][]repository.DigestCase,
) (int, error) {
	coveredTo, err := s.digests.CoveredTo(ctx, scope, p.ID)
	if err != nil {
		return 0, err
	}

	var (
		sent int
		// reached is the newest instant this policy has been examined to IN THIS
		// TICK. It is written once, at the end, rather than per window: the cursor is
		// monotone and one UPSERT is enough to record the furthest point, while a
		// write per window would put a statement between every pair of examinations
		// for no fact the last one does not already carry.
		reached time.Time
		// floor is where the PREVIOUS examination ended — the coverage instant as it
		// stood before the window now being examined. It is what keeps two published
		// spans from overlapping (see coveredSpanOf) and it moves in step with
		// `reached`, one window at a time, because a span may only be claimed once.
		floor = coveredTo
		// failed is the first error, kept rather than returned immediately so that the
		// coverage earned by the windows BEFORE it is still recorded. Dropping it would
		// make a transient failure on the third of six owed windows re-examine the
		// first two on the next tick — which is harmless for the messages, because
		// their episodes are marked, and is exactly the redundant work the cursor
		// exists to prevent.
		failed error
	)

windows:
	for _, start := range p.Digest.DigestWindows(now, coveredTo) {
		span := digestSpan{start: p.Digest.LookbackStart(start), end: p.Digest.WindowEnd(start)}
		rows, ok := spans[span]
		if !ok {
			rows, err = s.digests.Cases(ctx, scope, span.start, span.end, s.limit)
			if err != nil {
				failed = err
				break windows
			}
			spans[span] = rows
		}

		// ⚠️ THE MARKS ARE READ PER POLICY AND THE CASES ARE NOT, WHICH IS WHY THEY ARE
		// TWO CALLS. Which episodes opened is a fact about the tenant; which of them
		// this policy has accounted for is a fact about the policy. Folding the second
		// into the first would make the cached read policy-shaped and cost N queries
		// per span again.
		marked, err := s.digests.Marked(ctx, scope, p.ID, span.start, span.end)
		if err != nil {
			failed = err
			break windows
		}

		fresh, axes := foldDigest(p, rows, marked)
		if !p.Digest.Clears(len(fresh)) {
			// ⭐ NOTHING IS SENT FOR A WINDOW THAT DID NOT CLEAR, AND THAT IS STILL NOT
			// THE SILENT SUPPRESSION §B.6 FORBIDS. A suppressed Notification exists
			// because oto DECIDED NOT TO SEND SOMETHING IT HAD; here there was nothing
			// to send. Minting a `suppressed` row per empty window would put one row per
			// policy per ten minutes into the audit log forever, and would make "oto
			// withheld something from me" — the state that row exists to make visible —
			// indistinguishable from "the namespace was quiet".
			//
			// ⛔ WHAT CHANGED IS THAT THE QUIET IS NOW RECORDED WHERE IT COSTS NOTHING,
			// AND THE COMMENT THAT USED TO BE HERE WAS WRONG (git-bug `893cee4`). It
			// said: "an unsent window advances no cursor, so a quiet stretch is
			// re-examined by the next tick and then falls off the `MaxDigestBackfill`
			// horizon. That is correct, because re-examining a closed window is a query
			// whose answer cannot have changed." The second sentence is true and the
			// first is a permanent leak: because the cursor never moved, the owed span
			// grew by one window every window, forever, and every tick spent six queries
			// re-answering that unchanged question and then logged a data-loss warning
			// about windows nothing was ever owed.
			//
			// So the episodes are MARKED — `reported_in` NULL, meaning "examined and
			// found quiet" — and coverage advances. The mark is the §B.6 receipt in its
			// STRONG form: a marked-but-unreported Case is oto saying "I looked at this
			// and it did not clear your floor", and the ABSENCE of a mark on a matched
			// Case older than the lookback is the unrecoverable gap `ReconcileOrg`
			// counts. A window with no matching episodes at all writes nothing, because
			// there is nothing to say anything about; the coverage row is what records
			// that it was looked at.
			if err := s.digests.Mark(ctx, scope, p.ID, nil, fresh, now); err != nil {
				failed = err
				break windows
			}
			reached, floor = span.end, span.end
			continue
		}

		out, err := s.emit(ctx, scope, p, start, floor, fresh, axes)
		if err != nil {
			failed = err
			break windows
		}
		if out == digestNoDestination {
			// The window is still owed. Coverage stops at the previous window's end,
			// so re-enabling a channel inside the `MaxDigestBackfill` horizon still
			// produces this digest. See digestNoDestination.
			break windows
		}
		if out == digestSent {
			sent++
		}
		reached, floor = span.end, span.end
	}

	if !reached.IsZero() {
		if err := s.digests.AdvanceCoverage(ctx, scope, p.ID, reached, now); err != nil {
			if failed == nil {
				failed = err
			}
		}
		s.reportAbandoned(ctx, scope, p, now, coveredTo, reached)
	}
	return sent, failed
}

// reportAbandoned says out loud that the cursor has just moved past windows this
// policy owed and nothing ever examined.
//
// ⛔⛔ THE ABANDONMENT WAS SILENT, AND SILENT IS THE ONE THING IT MAY NOT BE. A pod
// down three hours on a ten-minute window owes eighteen digests;
// `MaxDigestBackfill` covers the newest six, deliberately, because a summary of a
// morning nobody is looking at any more is worse than none — and then
// `AdvanceCoverage` above steps the cursor over all twelve of the others, which is
// what makes the choice stick. Nothing anywhere recorded that twelve windows had been
// dropped. `MaxDigestBackfill`'s own ⛔ block explains why the OLD number
// (`skipped_windows`) had to go, and it is worth reading for what it actually
// deleted: a FICTION, in which a quiet policy's un-advanced cursor manufactured a
// backlog of thousands of windows nothing was ever owed. Coverage now advances for
// every window EXAMINED, so this count is only ever "how far behind the reader really
// was". A genuinely abandoned window is a different fact from a fictional one, and it
// must not inherit the fiction's silence.
//
// ⚠️ IT IS LOGGED AT THE MOMENT THE CURSOR ACTUALLY JUMPS, NOT WHENEVER WINDOWS ARE
// OWED, and that is what keeps it off a healthy install and off a stuck one. A policy
// whose channel is disabled advances no cursor (`digestNoDestination`), so its windows
// are still owed rather than abandoned and this says nothing about them once a tick
// forever — the failure that discredited `skipped_windows`. A recovered pod logs this
// once, on the tick that catches up, and then its cursor is current again.
//
// ⭐ AND IT IS A WEAKER FACT THAN THE ONE TO ALERT ON, WHICH THE LINE SAYS ITSELF.
// Windows are not episodes: an abandoned window over a quiet namespace lost nothing.
// The number an operator alarms on is `ReconcileOrg`'s `unreported_episodes`, and it
// is named in the line so that whoever reads this knows where to look for the harm.
func (s *DigestService) reportAbandoned(
	ctx context.Context, scope db.TenantScope, p domain.Policy, now, coveredTo, reached time.Time,
) {
	abandoned := p.Digest.AbandonedWindows(now, coveredTo)
	if abandoned == 0 {
		return
	}
	s.log.WarnContext(ctx, "notification: digest windows abandoned, the cursor moved past them",
		slog.String("org_id", scope.OrgID().String()),
		slog.String("policy_id", p.ID.String()),
		slog.Int("abandoned_windows", abandoned),
		slog.Int("backfill_cap", domain.MaxDigestBackfill),
		slog.String("window_seconds", p.Digest.Window.String()),
		slog.String("coverage_was", coveredTo.Format(time.RFC3339)),
		slog.String("coverage_now", reached.Format(time.RFC3339)),
		slog.String("harm", "episodes in those windows were never examined; "+
			"DigestService.ReconcileOrg counts them as unreported_episodes"))
}

// foldDigest applies one policy's matchers to a span's episodes and subtracts the
// ones it has already accounted for. It returns the episodes THIS digest would report,
// and how many distinct signal axes they span.
//
// ⭐ THE MATCHERS ARE THE NAMESPACE SELECTOR, and they are the SAME matchers the
// notification path uses against the SAME labels. That is not a coincidence to be
// preserved by care: `domain.Policy.Matches` is the single implementation, so a
// policy cannot select one set of alerts for its individual notifications and a
// different set for its digest. Since ADR 0038 the labels are oto's own axes —
// `alertname`, and `namespace` when the alert has one — which is what makes
// `namespace = "observability"` the useful matcher the digest is for.
//
// A policy with NO matchers digests the whole tenant, which is the same thing no
// matchers already means everywhere else in this system.
//
// ⭐⭐ THE MARK SUBTRACTION IS WHAT MAKES THE LOOKBACK SAFE, and it is the reason
// this function returns EPISODES rather than a count. The read spans
// `[T - L, T + W)`, so almost everything in the `L` tail was already offered to the
// previous window's digest; without the subtraction a two-minute tail would re-report
// two minutes of episodes in every single digest, which is a louder bug than the one
// the lookback fixes. And the caller needs the episodes themselves, not their number,
// because it has to MARK exactly the ones it reported — a count cannot be marked.
//
// ⚠️ MATCHING IS CHECKED BEFORE THE MARK, NOT AFTER, AND THE ORDER IS THE CHEAPER ONE
// BY ACCIDENT AND THE CORRECT ONE ON PURPOSE. A policy only ever marks episodes it
// MATCHED: a mark means "this policy accounted for this episode", and marking an
// episode a policy does not select would make the reconciler unable to tell a missed
// report from an episode no policy was ever interested in. That is also why the
// reconciler cannot be a SQL anti-join — whether a policy matches is decided here, in
// Go, by a compiled regular expression.
func foldDigest(
	p domain.Policy, rows []repository.DigestCase, marked map[uuid.UUID]struct{},
) (fresh []repository.DigestCase, axes int) {
	fresh = make([]repository.DigestCase, 0, len(rows))
	seen := make(map[string]struct{}, 8)
	for _, c := range rows {
		ok, err := p.Matches(c.Labels)
		if err != nil || !ok {
			// A broken matcher regex must not be able to make a digest report on
			// everything. It is refused when the policy is saved; here the episode
			// simply does not match, which is the quiet direction.
			continue
		}
		if _, done := marked[c.ID]; done {
			continue
		}
		fresh = append(fresh, c)
		// The axis pair, for the log line only. The separator is a NUL because it
		// cannot occur in a Prometheus label value, so `{alertname: "a\x00b"}` cannot
		// collide with `{alertname: "a", namespace: "b"}`.
		seen[c.Labels["alertname"]+"\x00"+c.Labels["namespace"]] = struct{}{}
	}
	return fresh, len(seen)
}

// coveredSpanOf is the span a digest for `start` truthfully covers: the window, pulled
// back to include the oldest straggler it swept up out of the lookback tail, and then
// stopped at `floor` — the instant the policy's coverage had already reached.
//
// ⭐ IT IS THE FACT `342e071` FOUND MISSING FROM THE ROW. `digest_window_start` alone
// is not a span — it is a span only in combination with the window LENGTH that was in
// force when the digest was sent, and nothing stored the length, so every reader that
// wanted one multiplied the start by the policy's CURRENT `digest_window_s`. Storing
// both ends means a card can state its own coverage without consulting a configuration
// row that may have changed since.
//
// `from` is at or before the window's start and `to` is its exclusive end, so the span
// always contains the window (`notifications_digcover_ck`). The asymmetry is real
// rather than sloppy: a digest may reach BACKWARDS past its window, because that is
// what the lookback is, and it can never reach forwards, because the window after it
// is still open.
//
// ⛔⛔ `floor` IS WHAT MAKES CONSECUTIVE SPANS ABUT, AND WITHOUT IT THEY OVERLAPPED.
// The `Digest` doc in `channels/render/webhookjson/envelope.go` promises that
// "consecutive digests from one policy ABUT; they do not overlap", and a straggler
// broke the promise arithmetically: with `W = 600`, the digest for `[12:00, 12:10)`
// stores `covered_to = 12:10`, and if the next window sweeps a Case that opened at
// 12:09:30 out of its lookback tail, the unclamped `from` is 12:09:30 — thirty seconds
// claimed by two messages. A consumer partitioning time by these spans double-attributes
// that Case, which is precisely what the promise exists to let it not do.
//
// The clamp is a raise to the coverage instant rather than a drop of the straggler: the
// episode is still REPORTED here — the lookback exists so that a late commit is a
// duplicate rather than a hole — and what changes is only the boundary the card and the
// envelope claim, which now begins where the previous message's claim ended.
//
// ⭐ IT ALSO MAKES THE ABUTMENT TRUE ACROSS QUIET WINDOWS, WHICH IS THE HALF THE CLAIM
// WOULD OTHERWISE STILL BE WRONG ABOUT. A window examined and found quiet advances the
// cursor and sends nothing, so two digests either side of it are not adjacent windows;
// a `from` of this window's own start would leave the quiet span attributed to no
// message at all. The floor carries the claim back over exactly those windows, and the
// claim is honest — they were examined and there was nothing in them.
//
// ⚠️ ONE OVERLAP SURVIVES AND IT IS THE RE-TILING `DigestWindows` DOCUMENTS. Widening
// `digest_window_s` re-floors the cursor onto the enclosing wide boundary, so `floor`
// can land AFTER `start`; `notifications_digcover_ck` requires `from <= start`, so the
// window start wins and the earlier half of that one wide window is claimed twice. It
// takes a policy edit, it sends nothing at all unless the wide window's later half has
// fresh episodes, and the alternative — a span that does not contain its own window —
// is a row the constraint refuses.
func coveredSpanOf(
	d domain.Digest, start, floor time.Time, cases []repository.DigestCase,
) (from, to time.Time) {
	from, to = start, d.WindowEnd(start)
	for _, c := range cases {
		if c.StartedAt.Before(from) {
			from = c.StartedAt
		}
	}
	if from.Before(floor) {
		from = floor
	}
	if from.After(start) {
		from = start
	}
	return from.UTC(), to.UTC()
}

// emit records ONE digest and fans it out. It reports whether this call created the
// row.
//
// ⭐⭐ IT DOES NOT GO THROUGH `NotificationService.Evaluate`, AND THAT IS A DECISION
// RATHER THAN A SHORTCUT. Four of the five things `Evaluate` does are meaningless
// here, and the fifth is wrong:
//
//   - ROUTING. `Evaluate` walks the live policies and stops at the first match. The
//     digest's policy is the input, not the output; re-deriving it could route the
//     digest to a DIFFERENT policy's channels — a higher-priority policy that also
//     lists `digest` would silently steal every digest in the tenant.
//   - THE SNAPSHOT. `Evaluate` reads one group generation to decide §H.6 and to
//     render. A digest has no generation; the snapshot query would be
//     `WHERE ac.id = <nil uuid>` and would fail as `case_not_found`.
//   - SNOOZE. A snooze is keyed by `alert_key` (§B.8.4) and suppresses every Reason
//     for that key. A digest names no alert, and "every member snoozed" is not a
//     question a namespace over a window has an answer to.
//   - THE THROTTLE. `CountRecent` counts what landed on ONE GROUP'S THREAD, and a
//     digest lands on its own. Its rate limit is the WINDOW — at most one per policy
//     per window, enforced by `notif_digest_uniq` — which is a stronger guarantee
//     than a cap.
//   - THE §H.6 MODE TABLE. `PlanFor` answers "does this transition surface in this
//     channel, and as an amend or a reply", and a digest is not a transition. Its
//     mode rule is two lines — OPEN THE CONVERSATION ONCE, THEN REPLY TO IT ONCE PER
//     WINDOW — and it is stated in `digestModes`, in notify.go beside the table it
//     replaces.
//
// What it DOES share is everything that makes a Notification a Notification: the
// §C.7 idempotency key, the `notifications` row, the per-channel `notification_
// deliveries` rows, the thread sequence allocated inside the creating transaction,
// and the delivery jobs enqueued in that same transaction (ADR 0001's outbox). Those
// are `fanOut`'s, and `fanOut` is what this calls.
func (s *DigestService) emit(
	ctx context.Context, scope db.TenantScope, p domain.Policy,
	start, floor time.Time, cases []repository.DigestCase, axes int,
) (digestOutcome, error) {
	channels, err := s.notifier.channels.ListByIDs(ctx, scope, p.ChannelIDs)
	if err != nil {
		return digestSent, err
	}
	live := make([]domain.Channel, 0, len(channels))
	for _, c := range channels {
		if c.Live() {
			live = append(live, c)
		}
	}
	sortChannels(live)
	if len(live) == 0 {
		// ⚠️ NO DESTINATION MEANS NO ROW, WHICH IS THE ONE PLACE THIS PATH IS QUIETER
		// THAN `Evaluate`. `Evaluate` records `channel_disabled` because the fact
		// existed and oto could not deliver it; a digest is MANUFACTURED for a
		// destination, so with no destination there is no fact to record. Recording one
		// would also burn the window's idempotency key, so re-enabling a channel two
		// minutes later would produce no digest for a window that had cleared its floor.
		//
		// ⛔ AND NOTHING IS MARKED EITHER, WHICH IS WHAT KEEPS THAT SENTENCE TRUE NOW
		// THAT MARKS EXIST. A mark would say oto had accounted for these episodes, and
		// it has not — it found nowhere to put them. Leaving them unmarked means the
		// window is genuinely still owed, `digestNoDestination` stops the cursor short
		// of it, and if the channel is never re-enabled the episodes surface as
		// unreported in `ReconcileOrg` rather than vanishing.
		return digestNoDestination, nil
	}

	count := len(cases)
	coveredFrom, coveredTo := coveredSpanOf(p.Digest, start, floor, cases)
	policyID := p.ID
	windowStart := start
	n := domain.Notification{
		ID:          uuid.New(),
		OrgID:       scope.OrgID(),
		SubjectKind: domain.SubjectDigest,
		// The POLICY half of the pair. The window half is on `DigestWindowStart`,
		// because one UUID column cannot hold a pair and hashing the two into a
		// synthetic id would make `subject_id` resolve against no table (00058).
		SubjectID: policyID,
		// The policy IS the conversation: a digest opens its own, keyed by the
		// policy, and replies into it once per window.
		ConversationKind: domain.ConversationDigest,
		ConversationID:   policyID,
		// No GroupID. A digest spans many generations, so it has no one thread to
		// land in.
		//
		// ⭐ AND IT IS NO LONGER AN EXCEPTION. `notifications_target_ck` used to
		// admit the missing group for this kind alone; migration 00064 retired it and
		// a digest now names its conversation the same way every other fact does —
		// with the pair below. What was a hole in a CHECK is an ordinary value.
		Reason:   domain.ReasonDigest,
		PolicyID: &policyID,
		// The WINDOW ORDINAL as the subject's version (§C.7). It is what makes one
		// window's digest a different intent from the next one's without changing the
		// key's shape, and it is >= 1 for every window after the epoch
		// (notifications_sver_ck).
		StateVersion:      p.Digest.WindowOrdinal(start),
		Status:            domain.StatusPending,
		DigestWindowStart: &windowStart,
		DigestCount:       &count,
		// The SPAN THIS MESSAGE COVERS, stored rather than inferred (migration 00070).
		// `DigestWindowStart` says where the window began; without the length in force
		// at the time it cannot say where the coverage ENDED, which is what made
		// narrowing a policy re-report a span it had already summarised. These two are
		// also what let a renderer draw the span WITHOUT being handed the policy's
		// CURRENT window: they reach a card as `channels/domain.DigestView.CoveredFrom`
		// /`CoveredTo` (via `ViewService.digest`), and both renderers read them off the
		// row rather than multiplying `digest_window_s` by anything. That is what
		// retired `DigestHeadline`, the pre-composed sentence this pair used to feed
		// (git-bug `78388fb`).
		//
		// ⚠️ THE PAIR IS HALF-OPEN — `[covered_from, covered_to)`, enforced by
		// `notifications_digcover_ck` — so anything downstream that prints it must say
		// "up to" and never "to". The Case that opened at exactly `covered_to` belongs
		// to the NEXT window, and a reader shown a closed span double-counts one
		// boundary per digest.
		DigestCoveredFrom: &coveredFrom,
		DigestCoveredTo:   &coveredTo,
		CreatedAt:         s.clk.Now().UTC(),
	}
	n.UpdatedAt = n.CreatedAt
	// ⭐ NO OCCASION, AND THAT IS THE POINT OF THE WINDOW ORDINAL ABOVE. The occasion
	// (§C.7) exists for Reasons whose facts `state_version` cannot tell apart; a
	// digest's version IS its window, so every window is already a distinct key and
	// `uuid.Nil` writes nothing into the pre-image. A digest's stored key is
	// therefore byte-identical to the one this line computed before the field
	// existed — which is what `one digest per (tenant, policy, window)` depends on.
	n.IdempotencyKey = domain.IdempotencyKey(
		scope.OrgID(), n.SubjectKind, n.SubjectID, n.Reason, n.StateVersion, uuid.Nil)

	var created bool
	err = s.notifier.txr.InTx(ctx, func(ctx context.Context) error {
		stored, madeNew, err := s.notifier.notifications.Insert(ctx, scope, n)
		if err != nil {
			// ⭐ A UNIQUE VIOLATION HERE IS THE MECHANISM WORKING (§L.9). `Insert`
			// swallows a conflict on its own arbiter, `(org_id, idempotency_key)`, but
			// `notif_digest_uniq` is a SECOND unique index over the same triple and
			// Postgres may report either — so a concurrent tick can surface as a bare
			// 23505. It means precisely "this window is already covered", which is the
			// answer, not an error.
			//
			// ⛔⛔ IT IS STILL AN ERROR TO **THIS TRANSACTION**, AND SAYING SO IS THE
			// ONLY WAY THE ANSWER SURVIVES. Everywhere else in this tree a duplicate
			// oto expected is swallowed by `ON CONFLICT DO NOTHING` before it becomes
			// an error at all — `notifications.Insert` (repository/notifications.go:135),
			// `deliveries.Create` (:136), `threads.Ensure` (:101), `alert_events`
			// (alerts/repository/event.go:121), `idempotency_claims`
			// (platform/idempotency/repository.go:43, whose mapErr calls a 23505 that
			// reaches Go INTERNAL for exactly this reason). A 23505 that DOES reach Go
			// has already aborted the Postgres transaction: every statement after it
			// answers 25P02 and `COMMIT` answers `ErrTxCommitRollback`. Returning nil
			// here — which is what this used to do — asked `db.Tx` to commit a dead
			// transaction, so "already covered, carry on" came back out of `InTx` as an
			// ERROR, and `sweepPolicy` gave up the policy's remaining owed windows for
			// the whole tick. The sentinel below aborts the transaction honestly and is
			// read as the answer OUTSIDE it, where there is no transaction left to
			// corrupt — the shape `errFanOutSettled` uses in grouping/service.
			if repository.IsUniqueViolation(err) {
				return errDigestWindowCovered
			}
			return err
		}
		created = madeNew

		// ⭐⭐ THE MARKS COMMIT WITH THE MESSAGE, AND THAT ATOMICITY IS THE WHOLE
		// GUARANTEE. If the row committed and the marks did not, the next tick's
		// lookback would find these episodes unaccounted for and report them again —
		// the double-report the marks exist to prevent, produced by a crash between two
		// statements. If the marks committed and the row did not, oto would have
		// recorded that it accounted for episodes it never mentioned, which is exactly
		// the invisible hole §B.6 refuses. One transaction, both facts, or neither.
		//
		// ⚠️ IT IS WRITTEN FOR A ROW THAT ALREADY EXISTED TOO. `madeNew` false means
		// another run holds this §C.7 key, and it either marked these episodes already
		// — in which case `ON CONFLICT DO NOTHING` makes this a no-op — or it died
		// before it could, in which case this is the repair. Skipping the write on the
		// idempotent path would leave the second case unfixable.
		if err := s.digests.Mark(ctx, scope, p.ID, &stored.ID, cases, n.CreatedAt); err != nil {
			return err
		}

		if !madeNew && stored.Status == domain.StatusSuppressed {
			return nil
		}

		dests := make([]destination, 0, len(live))
		for _, c := range live {
			dests = append(dests, destination{channel: c})
		}
		if _, err := s.notifier.fanOut(ctx, scope, stored, dests, n.CreatedAt); err != nil {
			return err
		}
		return s.notifier.notifications.SetStatus(
			ctx, scope, stored.ID, domain.StatusDispatched, n.CreatedAt)
	})
	if err != nil {
		if errors.Is(err, errDigestWindowCovered) {
			// ⭐ THE WINDOW IS COVERED, WHICH IS A SUCCESS WITH NOTHING TO SHOW FOR IT.
			// The transaction that discovered it is rolled back — it created no row, had
			// none to keep, and its marks went with it — and the tick carries on to the
			// policy's next owed window. Nothing is logged and nothing is counted: the
			// digest that covers this window is the other tick's, and so are its marks.
			return digestCovered, nil
		}
		return digestSent, err
	}
	if created {
		s.log.InfoContext(ctx, "notification: digest",
			slog.String("org_id", scope.OrgID().String()),
			slog.String("policy_id", policyID.String()),
			slog.String("window_start", start.Format(time.RFC3339)),
			slog.String("covered_from", coveredFrom.Format(time.RFC3339)),
			slog.String("covered_to", coveredTo.Format(time.RFC3339)),
			slog.Int("cases", count), slog.Int("groups", axes),
			slog.Int("channels", len(live)))
		return digestSent, nil
	}
	// The §C.7 key was already held — the same window, minted by a previous run of
	// this same tick — which is `Insert`'s idempotency working. It is covered, not
	// sent.
	return digestCovered, nil
}

// sortChannels keeps a fan-out comparable between two runs of the same tick. It does
// not affect correctness — ordering is a property of each THREAD — and it is here for
// the same reason `plan` sorts: two runs of one evaluation should be diffable.
func sortChannels(cs []domain.Channel) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID.String() < cs[j].ID.String() })
}
