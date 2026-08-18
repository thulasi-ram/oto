package service

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
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

// digestSpan is one half-open window `[start, end)`, and it is the bucket cache's
// key. Both ends, because two policies with different window LENGTHS can share a
// start — 12:00 begins a ten-minute window and an hourly one — and a cache keyed on
// the start alone would serve one policy's aggregate to the other.
type digestSpan struct{ start, end time.Time }

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
	// Limit bounds one window's fold per tenant. Zero means digestBucketLimit.
	Limit  int
	Clock  clock.Clock
	Logger *slog.Logger
}

// NewDigestService builds the service.
func NewDigestService(cfg DigestConfig) (*DigestService, error) {
	if cfg.Policies == nil || cfg.Digests == nil || cfg.Notifier == nil {
		return nil, errs.New(errs.KindInternal, "digest_service_deps",
			"the digest service needs a policy store, a digest store and the notification service")
	}
	s := &DigestService{
		policies: cfg.Policies, digests: cfg.Digests, notifier: cfg.Notifier,
		limit: cfg.Limit, clk: cfg.Clock, log: cfg.Logger,
	}
	if s.limit <= 0 {
		s.limit = digestBucketLimit
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

	// ⭐ THE BUCKET CACHE IS WHAT MAKES N POLICIES COST FEWER THAN N QUERIES. Two
	// policies on the same window length are asking about the same windows, and the
	// aggregate does not depend on the policy — only the matcher fold does. The cache
	// is per-SWEEP and dies with it: caching across ticks would be caching a count
	// whose window is still open on the very first entry.
	//
	// ⚠️ THE KEY IS THE WHOLE SPAN, NOT ITS START, AND THE DIFFERENCE IS A REAL BUG.
	// A ten-minute window and an hourly one both start at 12:00, so a start-keyed
	// cache would hand the hourly policy the ten-minute count — an hour's digest
	// reporting a sixth of its own window, silently, and only on installs that
	// happen to run two window lengths whose boundaries coincide.
	buckets := map[digestSpan][]repository.DigestBucket{}

	for i := range policies {
		p := policies[i]
		if !p.Digests() {
			// The database says this policy has a window (`policies_digest_idx`), so
			// this can only be a policy whose `reasons` lost `digest` — which
			// `policies_digest_reason_ck` forbids. Skip rather than mint an intent that
			// is certain to be suppressed as `no_policy`, once per window, forever.
			s.log.WarnContext(ctx, "notification: a digest policy does not route the digest reason",
				slog.String("org_id", scope.OrgID().String()),
				slog.String("policy_id", p.ID.String()))
			continue
		}
		n, err := s.sweepPolicy(ctx, scope, p, now, buckets)
		if err != nil {
			// ⛔ ONE POLICY'S FAILURE MUST NOT COST THE OTHERS THEIR WINDOW. The
			// windows are independent subscriptions, and the cursor is derived from the
			// digests themselves, so a policy that failed here is simply still owed its
			// window and picks it up on the next tick — inside `MaxDigestBackfill`,
			// which is what that bound is for.
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
func (s *DigestService) sweepPolicy(
	ctx context.Context, scope db.TenantScope, p domain.Policy, now time.Time,
	buckets map[digestSpan][]repository.DigestBucket,
) (int, error) {
	last, err := s.digests.LastWindow(ctx, scope, p.ID)
	if err != nil {
		return 0, err
	}

	windows, abandoned := p.Digest.DigestWindows(now, last)
	if abandoned > 0 {
		// ⭐ THE SKIP IS SAID OUT LOUD, WHICH IS §B.6 APPLIED TO OTO'S OWN QUIET.
		// `MaxDigestBackfill` deliberately drops the oldest owed windows rather than
		// posting hours of stale summaries, and a damper that cannot report itself is
		// the silent suppression oto refuses to ship. The windows that were dropped are
		// also visible in the data: the cursor jumps, and they simply have no row.
		s.log.WarnContext(ctx, "notification: digest windows skipped, too far behind",
			slog.String("org_id", scope.OrgID().String()),
			slog.String("policy_id", p.ID.String()),
			slog.Int("skipped_windows", abandoned),
			slog.Int("covered_windows", len(windows)),
			slog.String("last_covered", last.Format(time.RFC3339)))
	}

	sent := 0
	for _, start := range windows {
		span := digestSpan{start: start, end: p.Digest.WindowEnd(start)}
		rows, ok := buckets[span]
		if !ok {
			rows, err = s.digests.Buckets(ctx, scope, span.start, span.end, s.limit)
			if err != nil {
				return sent, err
			}
			buckets[span] = rows
		}

		count, groups := foldDigest(p, rows)
		if !p.Digest.Clears(count) {
			// ⭐ NOTHING IS RECORDED FOR A WINDOW THAT DID NOT CLEAR, AND THAT IS NOT
			// THE SILENT SUPPRESSION §B.6 FORBIDS. A suppressed Notification exists
			// because oto DECIDED NOT TO SEND SOMETHING IT HAD; here there was nothing
			// to send. Minting a `suppressed` row per empty window would put one row per
			// policy per ten minutes into the audit log forever, and would make "oto
			// withheld something from me" — the state that row exists to make visible —
			// indistinguishable from "the namespace was quiet".
			//
			// The consequence is deliberate: an unsent window advances no cursor, so a
			// quiet stretch is re-examined by the next tick and then falls off the
			// `MaxDigestBackfill` horizon. That is correct, because re-examining a
			// closed window is a query whose answer cannot have changed.
			continue
		}

		res, err := s.emit(ctx, scope, p, start, count, groups)
		if err != nil {
			return sent, err
		}
		if res {
			sent++
		}
	}
	return sent, nil
}

// foldDigest applies one policy's matchers to a window's buckets, and returns the
// case count and how many generations contributed.
//
// ⭐ THE MATCHERS ARE THE NAMESPACE SELECTOR, and they are the SAME matchers the
// notification path uses against the SAME labels. That is not a coincidence to be
// preserved by care: `domain.Policy.Matches` is the single implementation, so a
// policy cannot select one set of alerts for its individual notifications and a
// different set for its digest. Since ADR 0038 the group labels are oto's own axes —
// `alertname`, and `namespace` when the alert has one — which is what makes
// `namespace = "observability"` the useful matcher the digest is for.
//
// A policy with NO matchers digests the whole tenant, which is the same thing no
// matchers already means everywhere else in this system.
func foldDigest(p domain.Policy, rows []repository.DigestBucket) (count, groups int) {
	for _, b := range rows {
		ok, err := p.Matches(b.GroupLabels)
		if err != nil || !ok {
			// A broken matcher regex must not be able to make a digest report on
			// everything. It is refused when the policy is saved; here the generation
			// simply does not match, which is the quiet direction.
			continue
		}
		count += b.Cases
		groups++
	}
	return count, groups
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
//     `WHERE g.id = <nil uuid>` and would fail as `group_not_found`.
//   - SNOOZE. A snooze is keyed by `alert_key` (§B.8.4) and suppresses every Reason
//     for that key. A digest names no alert, and "every member snoozed" is not a
//     question a namespace over a window has an answer to.
//   - THE THROTTLE. `CountRecent` counts what landed on ONE GROUP'S THREAD, and a
//     digest lands on its own. Its rate limit is the WINDOW — at most one per policy
//     per window, enforced by `notif_digest_uniq` — which is a stronger guarantee
//     than a cap.
//   - THE §H.6 MODE TABLE. `PlanFor` answers "does this transition surface in this
//     channel, and as an amend or a reply", and a digest is not a transition. Its
//     mode rule is one line and it is stated in `digestModes` below.
//
// What it DOES share is everything that makes a Notification a Notification: the
// §C.7 idempotency key, the `notifications` row, the per-channel `notification_
// deliveries` rows, the thread sequence allocated inside the creating transaction,
// and the delivery jobs enqueued in that same transaction (ADR 0001's outbox). Those
// are `fanOut`'s, and `fanOut` is what this calls.
func (s *DigestService) emit(
	ctx context.Context, scope db.TenantScope, p domain.Policy,
	start time.Time, count, groups int,
) (bool, error) {
	channels, err := s.notifier.channels.ListByIDs(ctx, scope, p.ChannelIDs)
	if err != nil {
		return false, err
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
		return false, nil
	}

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
		// No GroupID. This is the row `notifications.group_id` was relaxed for: a
		// digest spans many generations, so `notifications_target_ck` admits the NULL
		// for this kind alone.
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
		CreatedAt:         s.clk.Now().UTC(),
	}
	n.UpdatedAt = n.CreatedAt
	n.IdempotencyKey = domain.IdempotencyKey(
		scope.OrgID(), n.SubjectKind, n.SubjectID, n.Reason, n.StateVersion)

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
			if repository.IsUniqueViolation(err) {
				return nil
			}
			return err
		}
		created = madeNew
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
		return false, err
	}
	if created {
		s.log.InfoContext(ctx, "notification: digest",
			slog.String("org_id", scope.OrgID().String()),
			slog.String("policy_id", policyID.String()),
			slog.String("window_start", start.Format(time.RFC3339)),
			slog.Int("cases", count), slog.Int("groups", groups),
			slog.Int("channels", len(live)))
	}
	return created, nil
}

// DigestHeadline is the one sentence a digest asserts, and it is the ONLY thing a
// renderer that has never heard of a digest needs in order to draw a truthful card.
//
// ⚠️ IT EXISTS BECAUSE THE RENDERER DOES NOT KNOW ABOUT DIGESTS YET.
// `internal/channels/render/slack` draws `*Group.Title* — <status>` from the view's
// group, and its `default:` arm handles an unrecognised Reason. Putting the headline
// where the title goes means a digest renders as a plain, correct info card and
// produces a non-empty `rendered_fallback` (`deliveries_fb_ck`) instead of an empty
// one, which would fail the delivery with a 23514 after the message had already gone.
// A renderer that learns to lay a digest out properly should read `DigestCount` and
// `DigestWindowStart` off the notification and stop using this — it is a floor, not
// a design.
func DigestHeadline(n domain.Notification, policyName string, window time.Duration) string {
	var b strings.Builder
	b.WriteString("Digest")
	if policyName != "" {
		b.WriteString(" · ")
		b.WriteString(policyName)
	}
	b.WriteString(" — ")
	if n.DigestCount != nil {
		b.WriteString(strconv.Itoa(*n.DigestCount))
		b.WriteString(" new firing")
		if *n.DigestCount != 1 {
			b.WriteString("s")
		}
	} else {
		b.WriteString("no count recorded")
	}
	if window > 0 {
		b.WriteString(" in ")
		b.WriteString(window.String())
	}
	if n.DigestWindowStart != nil {
		b.WriteString(" from ")
		b.WriteString(n.DigestWindowStart.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// sortChannels keeps a fan-out comparable between two runs of the same tick. It does
// not affect correctness — ordering is a property of each THREAD — and it is here for
// the same reason `plan` sorts: two runs of one evaluation should be diffable.
func sortChannels(cs []domain.Channel) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID.String() < cs[j].ID.String() })
}
