package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ⛔ BINDING. THIS FILE DETECTS AND NEVER DELIVERS.
//
// The reconciler answers one question — "is there an episode a digest policy selected
// and no digest ever mentioned?" — and its whole output is a number. It must never
// grow a send: a detector that repairs its own findings would post back-dated digests
// about spans nobody is looking at any more, which is the outage amplifier
// `MaxDigestBackfill` exists to prevent, and it would also destroy the evidence that
// there was ever a gap. If the number is non-zero, a human decides what to do about
// it.

// DigestGap is what one reconciliation pass found.
//
// ⭐⭐ IT IS THE HALF THAT MAKES THE BOUNDED LOOKBACK DEFENSIBLE RATHER THAN HOPEFUL,
// and it was ruled NOT OPTIONAL. `domain.DigestLookback` converts a Case that commits
// late from an invisible omission into a duplicate — but only for lateness under `L`.
// Past that the Case is still lost, and pre-release the goal is AUDITABLE rather than
// provably correct: if it happens, it must be found from a number somebody can alarm
// on and not from a customer.
type DigestGap struct {
	// Policies is how many digest policies were folded.
	Policies int
	// Cases is how many episodes were in the horizon at all.
	Cases int
	// Unreported is how many (policy, Case) pairs a policy MATCHED and never
	// accounted for — neither reported in a digest nor examined in a window that
	// failed its floor. It is the pair rather than the episode because the operator
	// who was not told is the one subscribed to THAT policy: an episode policy A
	// reported and policy B missed is a gap for B's channel, whatever A did.
	Unreported int
	// Episodes is the number of DISTINCT Cases behind `Unreported`, which is what a
	// human reading the log wants to know first.
	Episodes int
	// Oldest is the `started_at` of the oldest unreported episode, or the zero time
	// when there are none. It is the field that says whether this is a live problem
	// or a scar: an oldest just past a policy's own bound is a straggler that missed
	// its `DigestLookback` budget, and one near the horizon is something that has been
	// broken for a day.
	Oldest time.Time
	// Truncated reports that the episode read hit its limit, so every count above is
	// a LOWER BOUND. See ReconcileOrg.
	Truncated bool
	// Pruned is how many marks the retention sweep removed.
	Pruned int64
}

// ReconcileOrg looks for episodes a digest policy selected and no digest reported.
//
// ⛔⛔ IT REUSES `Policy.Matches` IN GO, AND A SQL ANTI-JOIN IS NOT AN ALTERNATIVE —
// IT IS A QUERY THAT CANNOT BE WRITTEN. The obvious spelling of this detector is "the
// Cases older than `now - L` that appear in no digest", and it is wrong at the first
// clause: a Case is unmentioned either because it was MISSED or because NO POLICY
// SELECTS IT, and which policies select it is decided in Go. A matcher may be `=~`, an
// Alertmanager-anchored regular expression compiled by `domain.Matcher`, with a
// missing-label rule (absent means empty string) that Postgres's `~` does not share.
// So the detector is a per-policy fold over the candidate span using the SAME matcher
// the tick uses, and its cost is closer to a tick than to a cheap query. That is the
// price of it not reporting phantom gaps: a detector with its own second
// implementation of matching would disagree with the tick, and every disagreement
// would look like data loss.
//
// ⭐ IT IS THE SAME READ THE TICK DOES, WHICH IS WHY THEY CANNOT DRIFT. `Cases` is one
// query per tenant, `Marked` is one per policy, and `foldDigest`'s matcher is
// `p.Matches` — the single implementation the notification path also uses. There is no
// second definition of "this policy's episodes" anywhere in this file.
//
// ⛔⛔ THE CANDIDATE SPAN ENDS PER POLICY, AT `Digest.UnreportableBefore`, AND A
// TENANT-WIDE INSTANT CANNOT EXPRESS THAT BOUND. This used to read
// `to := now - DigestLookback` for the whole org, and it warned on a HEALTHY INSTALL
// by construction: the tick only ever examines CLOSED windows, so every episode in the
// currently-open window is necessarily unmarked, and every one of them folded into
// `unreported`. With `digest_window_s = 86400` and forty Cases a day, an hourly pass at
// 12:00 read up to 11:58 while marks existed only to 00:00 — so half a day of perfectly
// healthy episodes were reported as "nobody was told about", every hour, forever. Even
// a five-minute window leaked the three minutes between the last closed boundary and
// `now - L` on every run. `W` is a per-policy column, so the bound is a per-policy
// question: see `domain.Digest.UnreportableBefore` for the two facts it is the later
// of, and note that the loop below therefore reads the coverage cursor per policy — the
// same cursor the tick advances, which is what keeps detector and tick from disagreeing
// about what "examined" means.
//
// ⚠️ THE TENANT-WIDE READ IS STILL ONE QUERY. It spans `[from, widest)`, where `widest`
// is the latest bound any folded policy holds, and each policy then drops the rows past
// its OWN bound. Which episodes opened is a fact about the tenant; how far back a
// policy has been examined is a fact about the policy, which is the same division
// `SweepOrg`'s span cache draws.
//
// ⚠️ AND IT STARTS AT `now - DigestReconcileHorizon` BECAUSE THE EVIDENCE IS FINITE. A
// mark is retained for `DigestMarkRetention` and the horizon is derived to sit inside
// that, so the absence of a mark inside the span really means "never accounted for"
// rather than "the receipt was swept". Looking further back would turn retention into
// data loss.
//
// ⚠️ A TRUNCATED READ MAKES EVERY COUNT A LOWER BOUND, AND THE RESULT SAYS SO. The
// horizon is a day and the read is capped like every other sweep in oto, so a tenant
// busy enough to exceed the cap has its OLDEST episodes dropped — `digestCasesSQL`
// orders newest-first, which is right for a digest and is the unhelpful direction
// here. Reporting the truncation is the honest answer; paging the whole day in would
// make the detector the most expensive query in the system in order to refine a number
// that is already telling somebody to go and look.
func (s *DigestService) ReconcileOrg(ctx context.Context, scope db.TenantScope) (DigestGap, error) {
	var out DigestGap

	policies, err := s.policies.ListWithDigest(ctx, scope)
	if err != nil {
		return out, err
	}

	now := s.clk.Now().UTC()
	from := now.Add(-domain.DigestReconcileHorizon)

	// ⭐ THE PRUNE RUNS WHATEVER ELSE HAPPENS, AND IT RUNS FIRST-CLASS RATHER THAN AS
	// A FAVOUR. The mark table is the only unbounded thing this design added, so its
	// retention belongs on a job that is guaranteed to run, and this is the only
	// slow-cadence digest job there is. Pairing them also keeps the two horizons
	// visibly in one file, where a change to either has to look at the other.
	pruned, err := s.digests.PruneMarks(ctx, scope, now.Add(-domain.DigestMarkRetention))
	if err != nil {
		return out, err
	}
	out.Pruned = pruned

	if len(policies) == 0 {
		return out, nil
	}

	// ⭐ THE BOUNDS ARE RESOLVED BEFORE ANYTHING IS READ, because the tenant-wide read
	// cannot be issued until the widest of them is known. A policy whose bound is the
	// zero time has never been examined at all and is owed nothing yet
	// (`UnreportableBefore`); one whose bound sits at or before the horizon has no span
	// left inside the evidence retention, and folding it would report "no mark" for
	// marks that were legitimately swept.
	type reconcileTarget struct {
		policy domain.Policy
		to     time.Time
	}
	targets := make([]reconcileTarget, 0, len(policies))
	var widest time.Time
	for i := range policies {
		p := policies[i]
		if !p.Digests() {
			// The same row `SweepOrg` skips: the database says it has a window but its
			// `reasons` lost `digest` or its `subject_kinds` does not bind the digest
			// altitude, which `policies_digest_reason_ck` and `policies_digest_subject_ck`
			// forbid between them. It sends nothing, so it is owed nothing, and counting
			// its episodes as gaps would be the detector reporting a configuration error
			// as data loss.
			continue
		}
		coveredTo, err := s.digests.CoveredTo(ctx, scope, p.ID)
		if err != nil {
			return out, err
		}
		to := p.Digest.UnreportableBefore(now, coveredTo)
		if to.IsZero() || !to.After(from) {
			continue
		}
		targets = append(targets, reconcileTarget{policy: p, to: to})
		if to.After(widest) {
			widest = to
		}
	}
	out.Policies = len(targets)
	if len(targets) == 0 {
		return out, nil
	}

	rows, err := s.digests.Cases(ctx, scope, from, widest, s.limit)
	if err != nil {
		return out, err
	}
	out.Cases = len(rows)
	out.Truncated = len(rows) >= s.limit

	episodes := make(map[uuid.UUID]struct{}, 16)
	for _, t := range targets {
		p := t.policy

		marked, err := s.digests.Marked(ctx, scope, p.ID, from, t.to)
		if err != nil {
			return out, err
		}

		// The rows this policy is allowed to be judged on: everything the tenant-wide
		// read returned that is already past THIS policy's last chance. An episode
		// younger than that has not had its chance yet — the digest that will count it
		// has not been minted, because its window may not even have closed — and
		// reporting it would be the false-alarm shape that makes an alertable number
		// worthless.
		candidates := make([]repository.DigestCase, 0, len(rows))
		for _, c := range rows {
			if c.StartedAt.Before(t.to) {
				candidates = append(candidates, c)
			}
		}

		// ⭐ THE SAME FOLD THE TICK USES, WITH THE SAME MEANING. `foldDigest` returns
		// the episodes this policy matched and has NOT accounted for — which inside the
		// tick is "what this digest will report" and here, past the policy's last
		// chance, is "what nothing ever reported". One function, two readings, no
		// second matcher.
		unreported, _ := foldDigest(p, candidates, marked)
		if len(unreported) == 0 {
			continue
		}
		out.Unreported += len(unreported)

		oldest := unreported[0].StartedAt
		for _, c := range unreported {
			episodes[c.ID] = struct{}{}
			if c.StartedAt.Before(oldest) {
				oldest = c.StartedAt
			}
		}
		if out.Oldest.IsZero() || oldest.Before(out.Oldest) {
			out.Oldest = oldest
		}

		// ⛔ PER POLICY, BECAUSE THE POLICY IS THE ONLY ACTIONABLE THING IN THE LINE.
		// A tenant-wide total says something is wrong; the policy id says whose
		// channel is missing messages, and its matchers are what an operator has to
		// look at. The line count is bounded by the number of digest policies with a
		// gap, which is zero on a healthy install.
		s.log.WarnContext(ctx, "notification: digest episodes nobody was told about",
			slog.String("org_id", scope.OrgID().String()),
			slog.String("policy_id", p.ID.String()),
			slog.Int("unreported_cases", len(unreported)),
			slog.String("oldest_case_at", oldest.Format(time.RFC3339)),
			slog.String("horizon_from", from.Format(time.RFC3339)),
			// This policy's OWN bound, not the tenant's: the instant past which its
			// episodes can no longer be reported by any future digest. Two policies on
			// different window lengths legitimately print different values here.
			slog.String("horizon_to", t.to.Format(time.RFC3339)),
			slog.Bool("truncated", out.Truncated))
	}
	out.Episodes = len(episodes)

	if out.Unreported > 0 {
		// ⭐ THE TENANT-WIDE LINE IS NOT A REPEAT OF THE PER-POLICY ONES, AND
		// `unreported_episodes` IS WHY. Two policies can both select the same episode,
		// so the per-policy counts sum to something LARGER than the number of firings
		// nobody heard about — and it is that second number an operator has to reason
		// about when deciding whether this is a handful of stragglers or a whole
		// namespace gone dark. This is the one number the ruling meant by "a metric over
		// unreported Cases": it is zero on a healthy install, it does not grow just
		// because a namespace is quiet — which is exactly what disqualified
		// `skipped_windows` — and it is therefore safe to alert on.
		s.log.WarnContext(ctx, "notification: digest reconciliation found a gap",
			slog.String("org_id", scope.OrgID().String()),
			slog.Int("policies", out.Policies),
			slog.Int("cases", out.Cases),
			slog.Int("unreported_pairs", out.Unreported),
			slog.Int("unreported_episodes", out.Episodes),
			slog.String("oldest_case_at", out.Oldest.Format(time.RFC3339)),
			slog.Int64("marks_pruned", out.Pruned),
			slog.Bool("truncated", out.Truncated))
	} else {
		// ⭐ THE QUIET RUN IS RECORDED AT DEBUG AND NOT AT INFO, WHICH IS THE LESSON
		// `893cee4` COST. The bug that produced this work was a WARN that fired every
		// sixty seconds on a healthy install, and the reason nobody noticed the one
		// occurrence that mattered was that it was buried under thousands that did
		// not. A detector that finds nothing has found nothing.
		s.log.DebugContext(ctx, "notification: digest reconciliation found no gap",
			slog.String("org_id", scope.OrgID().String()),
			slog.Int("policies", out.Policies),
			slog.Int("cases", out.Cases),
			slog.Int64("marks_pruned", out.Pruned),
			slog.Bool("truncated", out.Truncated))
	}
	return out, nil
}
