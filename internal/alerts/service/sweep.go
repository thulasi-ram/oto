package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// DefaultSweepLimit is how many candidates one sweep tick considers. The sweeps
// are periodic (60 s), so a bounded pass that runs again shortly is better than
// an unbounded one that holds a transaction open.
const DefaultSweepLimit = 200

// ReapResult is the audit of one `case.reap` tick.
type ReapResult struct {
	// Considered is how many candidates the tick's scans returned, each counted
	// once.
	Considered int
	// Expired is how many were moved to `expired` — which is NOT `resolved`.
	Expired int
	// Held is how many were left exactly as they were because some live source on
	// their cluster could not be proven healthy (or none is live). THIS NUMBER IS A
	// FEATURE: it is the `source_degraded_holds` counter of §B.4, and it should be
	// exported.
	//
	// ⚠️ IT COUNTS ONLY WHAT REACHED THE GUARD. The scans pre-filter on
	// `source_health` (a cluster whose live sources are not all `healthy` is never
	// a candidate), so a source that is plainly down holds its Cases without
	// appearing here; what lands here is the guard's port disagreeing with the
	// table — a lookup that failed, or a verdict that changed between the two.
	// The per-source held count on the sources screen is the complete one.
	Held int
	// HeldSources names the sources responsible — only the ones the guard could
	// not vouch for, never a healthy replica beside them — so one
	// `source.unreachable` banner can be raised per source rather than one per
	// case.
	HeldSources []uuid.UUID
	// Superseded is how many candidates were ABANDONED because the row had moved
	// since the sweep read it: somebody else ended the episode, or a fresh
	// observation pushed `source_ends_at` forward and the alert is demonstrably
	// still live.
	//
	// THIS NUMBER IS ALSO A FEATURE. Every increment is one expiry that would have
	// been fabricated over a row that disproved it, and a sustained non-zero value
	// says the sweep is racing ingest hard enough to be worth looking at.
	Superseded int
	// Silent and SourceRemoved say how many of Expired were the two ADR 0056
	// expiries; the rest were `timeout`. They are counted apart because a burst of
	// either one is a different fact for an operator than a burst of the other.
	Silent        int
	SourceRemoved int
}

// Reap is the `case.reap` sweep — SPEC §B.3 T6.
//
// ⭐⭐ THE TWO RULES THIS METHOD EXISTS TO ENFORCE, and they are the highest-value
// correctness rules in the system:
//
//  1. A RESOLUTION IS NEVER FABRICATED. `resolved` means an explicit upstream
//     `status="resolved"` observation arrived. `expired` means oto STOPPED
//     HEARING about the alert. There is no code path in this file that can
//     produce `resolved`: the only state it writes comes back from domain.Apply
//     under TriggerReap, which the §B.3 table maps exclusively to `expired` with
//     one of the three expiry reasons — `timeout`, `silent`, `source_removed`
//     (ADR 0056) — and the assertion below refuses anything else.
//
//  2. THE REAPER IS BLOCKED WHILE THE SOURCES ARE NOT HEALTHY (§B.4). Losing
//     sight of an alert is not the same as the alert resolving. A case is HELD
//     in its current state unless its cluster has at least one live source and
//     EVERY live source is PROVEN healthy (owner ruling R1: an HA pair is two
//     witnesses, and either one oto cannot see might be carrying the alert) —
//     including when the health port is not wired at all, when no live source
//     can be resolved, and when the health lookup itself fails. Every one of
//     those is "oto does not know", and "oto does not know" must never end an
//     episode. Both `timeout` and `silent` pass through this guard.
//
//     ⚠️ `source_removed` IS THE ONE EXPIRY THAT DOES NOT, AND IT IS NOT A HOLE IN
//     THE RULE. The guard protects a source oto cannot see; a Case whose cluster
//     has no live source left has no source to be blind to. The one thing that
//     could have said the alert ended is gone, and holding the Case open would be
//     a promise nobody can keep (ADR 0056 §2). The pass is a question about the
//     CLUSTER, so an HA replica deleted beside a live one ends nothing.
//
// ⭐ THE CANDIDATE SCAN IS NOT A DECISION, AND IT DELIBERATELY TAKES NO LOCKS.
// `ReapCandidates` runs outside any transaction and its result is several round
// trips old by the time `expire` looks at it — source resolution and a health
// lookup happen in between. Holding the scan's rows locked for that whole window
// would serialise a storm against ingest, which is the one thing a background
// sweep must never do. So the candidate list is treated as nothing more than a
// LIST OF IDS TO RECONSIDER: `expire` re-reads each row inside its own small
// transaction, re-runs the machine against THAT row, and writes as a
// compare-and-set. Nothing decided out here reaches the database.
func (s *Service) Reap(ctx context.Context, scope db.TenantScope, limit int) (ReapResult, error) {
	if limit <= 0 {
		limit = DefaultSweepLimit
	}
	cfg := s.lifecycleSettings(ctx, scope)
	now := s.Now()
	before := now.Add(-cfg.ResolveGrace)

	t := newReapTally()

	// ⭐ THE TWO ADR 0056 PASSES RUN ONLY WHILE THE OPERATOR HAS TURNED THEM ON
	// (`jobs.expire_silent_and_removed`, off by default). Off, this tick is the
	// `timeout` sweep it was before 00094, and neither new reason is ever written
	// — which is what lets 00094 and its readers ship ahead of the writer.
	if s.expireUnheard {
		// ⭐ `source_removed` GOES FIRST (ADR 0056 §2). Its candidates resolve to
		// no source at all, so the guarded passes below would count every one of
		// them as HELD and log a §B.4 hold for a source that does not exist.
		// Ending them first takes them out of both scans, which read only open
		// episodes. `before` is also its cutoff: a removal younger than a resolve
		// grace ends nothing, so a source deleted and re-created in between leaves
		// its cluster's Cases exactly as they were.
		removed, err := s.cases.SourceRemovedCandidates(ctx, scope, before, limit)
		if err != nil {
			return ReapResult{}, err
		}
		for _, ac := range removed {
			t.consider(ac.ID())
			t.record(s.tryExpire(ctx, scope, ac, now, cfg, domain.ResolveSourceRemoved, nil))
		}
	}

	candidates, err := s.cases.ReapCandidates(ctx, scope, before, limit)
	if err != nil {
		return ReapResult{}, err
	}
	// `silent` is the same guarded pass over a different scan (ADR 0056 §3): the
	// §B.4 guard is asked of the same sources in the same way, because silence
	// under a source oto cannot see proves nothing. Both scans are read before the
	// guard runs, so health is still asked ONCE per tick, over both.
	var silent []domain.Case
	if s.expireUnheard {
		if silent, err = s.cases.SilentCandidates(ctx, scope, now, limit); err != nil {
			return ReapResult{}, err
		}
	}
	guarded := make([]reapCandidate, 0, len(candidates)+len(silent))
	for _, ac := range candidates {
		guarded = append(guarded, reapCandidate{ac, domain.ResolveTimeout})
	}
	for _, ac := range silent {
		guarded = append(guarded, reapCandidate{ac, domain.ResolveSilent})
	}
	if err := s.reapGuarded(ctx, scope, guarded, now, cfg, t); err != nil {
		return ReapResult{}, err
	}

	res := t.result()
	if res.Held > 0 {
		s.log.InfoContext(ctx, "alerts: reaper held cases, source not proven healthy",
			"org_id", scope.OrgID(), "held", res.Held, "sources", len(res.HeldSources))
	}
	return res, nil
}

// reapTally is one tick's audit across the three passes. A case can be a
// candidate of both guarded passes — past `source_ends_at` AND silent — so each
// case is decided once per tick: the first pass that considers it owns it, and a
// later pass skips it rather than counting a second hold, or a "superseded" for
// a row this same tick just expired.
type reapTally struct {
	res         ReapResult
	seen        map[uuid.UUID]struct{}
	heldSources map[uuid.UUID]struct{}
}

func newReapTally() *reapTally {
	return &reapTally{seen: map[uuid.UUID]struct{}{}, heldSources: map[uuid.UUID]struct{}{}}
}

// consider claims a case for the pass about to decide it, and reports false
// when an earlier pass of this tick already did.
func (t *reapTally) consider(caseID uuid.UUID) bool {
	if _, dup := t.seen[caseID]; dup {
		return false
	}
	t.seen[caseID] = struct{}{}
	t.res.Considered++
	return true
}

// hold counts one held case and names the sources responsible: the ones the
// guard could not vouch for, and ONLY those. A healthy replica beside an
// unhealthy one is not the reason anything is held, and naming it would raise a
// `source.unreachable` banner about a source that is fine. Nil `blind` is a case
// with no live source at all, which no source is responsible for.
func (t *reapTally) hold(blind []uuid.UUID) {
	t.res.Held++
	for _, src := range blind {
		t.heldSources[src] = struct{}{}
	}
}

// record counts one expire attempt. An error was already logged by tryExpire
// and costs nothing else: the next tick sees the case again.
func (t *reapTally) record(reason domain.ResolveReason, expired bool, err error) {
	switch {
	case err != nil:
	case !expired:
		t.res.Superseded++
	default:
		t.res.Expired++
		switch reason {
		case domain.ResolveSilent:
			t.res.Silent++
		case domain.ResolveSourceRemoved:
			t.res.SourceRemoved++
		}
	}
}

func (t *reapTally) result() ReapResult {
	out := t.res
	for src := range t.heldSources {
		out.HeldSources = append(out.HeldSources, src)
	}
	return out
}

// reapCandidate is one case and the expiry it was scanned for.
type reapCandidate struct {
	c      domain.Case
	reason domain.ResolveReason
}

// reapGuarded is the §B.4-guarded pass shared by `timeout` and `silent`: resolve
// every candidate's live sources, ask health once per distinct source, hold what
// cannot be vouched for, and expire the rest as the reason each was scanned for.
// A case both scans returned is decided once, as `timeout`, which comes first.
//
// ⭐ A CASE IS VOUCHED FOR ONLY WHEN EVERY LIVE SOURCE ON ITS CLUSTER IS HEALTHY
// (owner ruling R1). An HA pair is two witnesses; either one oto cannot see might
// be the one still carrying the alert, so one unhealthy replica holds the Case.
func (s *Service) reapGuarded(
	ctx context.Context, scope db.TenantScope, guarded []reapCandidate, now time.Time,
	cfg Settings, t *reapTally,
) error {
	if len(guarded) == 0 {
		return nil
	}
	cases := make([]domain.Case, len(guarded))
	for i, g := range guarded {
		cases[i] = g.c
	}
	sources, err := s.resolveSources(ctx, scope, cases)
	if err != nil {
		return err
	}
	all := make([]uuid.UUID, 0, len(sources))
	for _, srcs := range sources {
		all = append(all, srcs...)
	}
	healthy := s.healthBySource(ctx, scope, all)

	for _, g := range guarded {
		if !t.consider(g.c.ID()) {
			continue
		}
		srcs := sources[g.c.ID()]
		if len(srcs) == 0 {
			t.hold(nil)
			continue
		}
		var blind []uuid.UUID
		for _, src := range srcs {
			if !healthy[src] {
				blind = append(blind, src)
			}
		}
		if len(blind) > 0 {
			t.hold(blind)
			continue
		}
		t.record(s.tryExpire(ctx, scope, g.c, now, cfg, g.reason, srcs))
	}
	return nil
}

// resolveSources maps every candidate onto the live AlertSources of its cluster.
// A case absent from the result is one with no live source, and the caller reads
// that as "cannot prove healthy".
func (s *Service) resolveSources(
	ctx context.Context, scope db.TenantScope, candidates []domain.Case,
) (map[uuid.UUID][]uuid.UUID, error) {
	if s.occSources == nil {
		return map[uuid.UUID][]uuid.UUID{}, nil
	}
	ids := make([]uuid.UUID, len(candidates))
	for i, o := range candidates {
		ids[i] = o.ID()
	}
	return s.occSources.SourceIDs(ctx, scope, ids)
}

// healthBySource answers the §B.4 guard for every DISTINCT source in `sources`,
// in one round trip. The guard is per source, so its cost must be per source: 500
// candidates over 3 sources is 3 health rows, not 500 lookups — and the worst case
// for the per-candidate version was exactly the §B.4 case, a source outage, when
// nothing expires and every candidate returns next tick.
//
// ABSENCE FROM THE RESULT IS "NO". An unwired port, a nil source id, a failed
// lookup and a source the batch simply did not return are all the same answer —
// "oto does not know" — and the caller holds every case they own. That is
// why a failed lookup is reported by returning nothing rather than by an error:
// not knowing holds candidates, it must never abort the sweep.
func (s *Service) healthBySource(
	ctx context.Context, scope db.TenantScope, sources []uuid.UUID,
) map[uuid.UUID]bool {
	if s.health == nil || len(sources) == 0 {
		return nil
	}
	seen := make(map[uuid.UUID]struct{}, len(sources))
	distinct := make([]uuid.UUID, 0, len(sources))
	for _, src := range sources {
		if src == uuid.Nil {
			continue
		}
		if _, dup := seen[src]; dup {
			continue
		}
		seen[src] = struct{}{}
		distinct = append(distinct, src)
	}
	if len(distinct) == 0 {
		return nil
	}
	healthy, err := s.health.HealthyFor(ctx, scope, distinct)
	if err != nil {
		s.log.WarnContext(ctx, "alerts: source health unknown, holding all candidates",
			"sources", len(distinct), "error", err)
		return nil
	}
	return healthy
}

// tryExpire is expire as the sweep calls it. ⛔ AN ERROR IS LOGGED HERE AND THE
// SWEEP DOES NOT STOP FOR IT: one case failing must not cost the rest of the
// sweep, and the next tick will see it again in sixty seconds.
func (s *Service) tryExpire(
	ctx context.Context, scope db.TenantScope, candidate domain.Case, now time.Time, cfg Settings,
	reason domain.ResolveReason, proven []uuid.UUID,
) (domain.ResolveReason, bool, error) {
	ok, err := s.expire(ctx, scope, candidate, now, cfg, reason, proven)
	if err != nil {
		s.log.WarnContext(ctx, "alerts: could not expire case",
			"case_id", candidate.ID(), "resolve_reason", reason.String(), "error", err)
	}
	return reason, ok, err
}

// expire moves ONE case through T6, in its own transaction so that a
// single failure cannot roll back a whole sweep.
//
// `candidate` is the STALE SNAPSHOT the sweep scan returned, and it is used for
// exactly one thing: its id. Everything the verdict rests on is re-read inside
// the transaction, because between the scan and here the sweep has made two more
// round trips and a webhook has had every opportunity to land.
//
// It reports false — with no error — when the transition was abandoned: the row
// had already moved, or the fresh row no longer justifies an expiry. That is a
// normal outcome and the caller counts it as Superseded.
//
// ⚠️ LOCK ORDER: THIS TRANSACTION TAKES `alert_cases` BEFORE `alerts`.
// `Service.observe` (lifecycle.go) takes them the OTHER WAY ROUND — `UpsertBatch`
// locks the alert row before the case is even read. The two orders form a
// cycle, and today it is survivable only because neither side WAITS while holding
// the other's row for long: the reaper's alerts write is the last statement in a
// short transaction, and Postgres breaks a genuine cycle with a deadlock error
// that the sweep logs and retries in sixty seconds.
//
// ⛔ ADDING AN EXPLICIT LOCK — `SELECT ... FOR UPDATE`, an advisory lock, a
// widened transaction — TO EITHER SITE CLOSES THE CYCLE FOR REAL. If you need
// one, make both sites take the two tables in the SAME order first, and say so in
// both comments. The correctness of this file rests on the compare-and-set above,
// not on a lock, precisely so that no lock has to be held across the sweep.
//
// ⭐ `reason` IS WHICH EXPIRY THE CANDIDATE WAS SCANNED FOR (ADR 0056), and
// `proven` is the set of live sources whose health was proven for it — empty for
// `source_removed`, which asks no health because there is no source to ask. Every
// expiry re-reads the Case's cluster inside the transaction too, because a source
// can be registered, deleted or re-tuned between the scan and the write, and the
// health that was asked must still be the health of every live source (owner
// ruling R1).
func (s *Service) expire(
	ctx context.Context, scope db.TenantScope, candidate domain.Case, now time.Time, cfg Settings,
	reason domain.ResolveReason, proven []uuid.UUID,
) (bool, error) {
	actor, err := domain.SystemActor(domain.ActorReaper)
	if err != nil {
		return false, err
	}
	at, err := domain.NewObservationTime(now, now)
	if err != nil {
		return false, err
	}

	expired := false
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		// ⭐ THE RE-READ. The candidate came from a scan that ran outside any
		// transaction; this row is the one that will actually be overwritten.
		fresh, err := s.cases.GetByID(ctx, scope, candidate.ID())
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return nil // deleted under us; there is nothing to expire
			}
			return err
		}

		// ⛔⛔ THE ASSERTION THAT MAKES RULE 1 MECHANICAL RATHER THAN ASPIRATIONAL.
		//
		// It interrogates THE ROW ABOUT TO BE OVERWRITTEN. The version this
		// replaces inspected the DOMAIN RESULT — `r.To == expired` — which is
		// vacuously true for every T6 the machine can produce and therefore
		// guarded nothing at all: the machine had been fed a stale case,
		// answered honestly about it, and the assertion nodded at an answer to the
		// wrong question while `expired`/`timeout` went over a firing alert.
		if why := unreapable(fresh, now, cfg.ResolveGrace); reason == domain.ResolveTimeout && why != "" {
			s.log.InfoContext(ctx, "alerts: reaper stood down, the row disproved the expiry",
				"case_id", fresh.ID(), "reason", why)
			return nil
		}

		// ⭐ THE CLUSTER RE-READ. Every expiry rests on the Case's CLUSTER as well
		// as its row — `timeout` and `silent` on the live set being the one whose
		// health was proven, `source_removed` on there being none — so it is asked
		// again here rather than trusted from the scan.
		cluster, err := s.cases.Sources(ctx, scope, fresh.ID())
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return nil
			}
			return err
		}
		if why := unexpirable(fresh, now, cfg.ResolveGrace, reason, cluster, proven); why != "" {
			s.log.InfoContext(ctx, "alerts: reaper stood down, the row disproved the expiry",
				"case_id", fresh.ID(), "resolve_reason", reason.String(), "reason", why)
			return nil
		}

		// The machine now runs against the FRESH row, so its §B.4 grace check and
		// the compare-and-set below are asking about the same instant in the same
		// row's life.
		r, err := domain.Apply(fresh, domain.TransitionCommand{
			Trigger:      domain.TriggerReap,
			Actor:        actor,
			At:           at,
			EventID:      id.New(),
			ResolveGrace: cfg.ResolveGrace,
			// The guard has already been answered above; the machine re-checks it
			// because a state machine that trusts its caller is not a guard.
			// `source_removed` answered no health question, so it claims none.
			SourceHealthy: reason != domain.ResolveSourceRemoved,
			ExpireAs:      reason,
			MaxSilence:    cluster.MaxSilence,
			NoLiveSource:  cluster.Live == 0 && cluster.Removed > 0,
		})
		if err != nil {
			if errs.IsKind(err, errs.KindPrecondition) {
				return nil
			}
			return err
		}
		if r.To != domain.StateExpired || r.Case.ResolveReason() != reason || !reason.IsExpiry() {
			return errs.Internal("reaper_would_fabricate_resolution",
				errsInvariant("the reaper produced "+r.To.String()+"/"+r.Case.ResolveReason().String()+
					"; only expired as "+reason.String()+" is permitted"))
		}

		// No witnesses: the reaper has no observation, and an expiry names no
		// suppressor. transitionOf clears the column, which is what T6 means —
		// oto stopped hearing about the alert, not "Alertmanager is muting it".
		// The precondition is `fresh`'s `state_version`, and `Observe` bumps that
		// too — so a repeat webhook landing in the microseconds between the re-read
		// above and this UPDATE loses the reaper its compare-and-set even though it
		// moved no state letter. That is the intended reading: a case oto has
		// heard about since it read the row is not one oto has stopped hearing about.
		trans := transitionOf(r, domain.SuppressedBy{})
		if err := s.cases.Transition(ctx, scope, r.Case.ID(), trans); err != nil {
			// ⛔ ABANDON, never re-decide. The reaper is the one caller that must
			// NOT retry a lost compare-and-set: every reason it can lose one is a
			// reason not to expire — somebody ended the episode, or something was
			// heard about an alert oto was about to declare silent. The sweep runs
			// again in sixty seconds and will re-read from scratch, which is a
			// strictly safer place to reconsider than a hot loop holding a verdict.
			if errs.IsKind(err, errs.KindConflict) {
				s.log.InfoContext(ctx, "alerts: reaper lost the compare-and-set, expiry abandoned",
					"case_id", r.Case.ID())
				return nil
			}
			return err
		}

		alert, err := s.alerts.GetByID(ctx, scope, r.Case.AlertID())
		if err != nil {
			return err
		}
		if err := s.projectFromCase(ctx, scope, alert, r.Case, at, true, 0); err != nil {
			return err
		}
		if _, err := s.appendEvents(ctx, scope, r.Events); err != nil {
			return err
		}
		if err := s.publishCase(ctx, scope, r.Case); err != nil {
			return err
		}
		if err := s.publishAlert(ctx, scope, alert.ID(), map[string]any{
			"state": domain.StateExpired.String(),
		}); err != nil {
			return err
		}
		if _, err := s.enqueueNotify(ctx, scope, []notifyRequest{{
			reason:  reasonExpired,
			alertID: ptr(alert.ID()),
			caseID:  r.Case.ID(),
			actor:   domain.ActorReaper.String(),
		}}, nil); err != nil {
			return err
		}
		expired = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return expired, nil
}

// unreapable re-proves §B.3 T6's preconditions AGAINST THE ROW THE REAPER IS
// ABOUT TO OVERWRITE, and names the one that failed. An empty string means the
// row itself justifies the expiry.
//
// It is deliberately a duplicate of the checks inside domain.Apply's T6 arm. The
// duplication is the point: Apply answers about whatever case it is handed,
// and the bug this guards was Apply being handed a snapshot that had stopped
// being true. Asking the same questions of the row about to be written is the
// only form of the question that cannot be answered about the wrong row.
func unreapable(row domain.Case, now time.Time, grace time.Duration) string {
	switch {
	case row.ClosePending():
		// ⛔ FIRST, because a held resolve outranks every other refusal here: the
		// row already carries an explicit upstream `status="resolved"`, so expiring
		// it would stamp `timeout` over a resolution oto has in hand — the
		// resolved-versus-expired fabrication 00007 calls the distinction oto must
		// never blur. The due close is the only edge that may end such a row.
		// Unreachable at W=0: no row can carry a pending close.
		return "case holds an upstream resolve"
	case !row.IsOpen():
		// The loudest case: overwriting a `resolved` with `expired` replaces a
		// fact somebody upstream stated with one oto inferred, and leaves the
		// append-only timeline permanently disagreeing with the projection.
		// AlertState: "already resolved" and "already expired" are different refusals
		// and the sentence above is about the difference between them. "already
		// closed" would collapse exactly the distinction being protected.
		return "case is already " + row.AlertState().String()
	case row.SourceEndsAt().IsZero():
		return "no upstream end time"
	case !now.After(row.SourceEndsAt().Add(grace)):
		// A fresh observation pushed `source_ends_at` forward. The alert is
		// demonstrably still firing and there is nothing here to expire.
		return "resolve_grace has not elapsed since source_ends_at"
	default:
		return ""
	}
}

// unexpirable is unreapable's twin for the CLUSTER: it re-proves T6's
// preconditions for every expiry against the FRESH row and the cluster as re-read
// inside the same transaction, and names the one that failed. An empty string
// means the expiry stands.
//
// ⭐ `timeout` AND `silent` NEED THE LIVE SET TO BE EXACTLY `proven` (owner ruling
// R1). The guard vouched for those sources; a replica that joined since was never
// asked, and one that left takes its health verdict with it.
func unexpirable(
	row domain.Case, now time.Time, grace time.Duration, reason domain.ResolveReason,
	cluster domain.CaseSources, proven []uuid.UUID,
) string {
	switch {
	case row.ClosePending():
		// unreapable's first refusal, for its reason: a held upstream resolve
		// outranks every expiry.
		return "case holds an upstream resolve"
	case !row.IsOpen():
		return "case is already " + row.AlertState().String()
	}
	switch reason {
	case domain.ResolveTimeout:
		if !cluster.SameLiveSet(proven) {
			return "the sources proven healthy are no longer the cluster's live sources"
		}
	case domain.ResolveSilent:
		switch {
		case !cluster.SameLiveSet(proven):
			// The health that was proven belongs to a set that no longer speaks for
			// this case: a replica joined, or one was deleted.
			return "the sources proven healthy are no longer the cluster's live sources"
		case cluster.MaxSilence <= 0:
			return "a live source turned max_silence_s off"
		case !now.After(row.LastObservedAt().Add(cluster.MaxSilence)):
			return "heard about within max_silence_s"
		}
	case domain.ResolveSourceRemoved:
		switch {
		case cluster.Live > 0:
			// A source was registered for the cluster since the scan. It can say
			// when this episode ends, so oto waits for it to.
			return "a live source feeds the cluster again"
		case cluster.Removed == 0:
			return "no source was ever removed from the cluster"
		case !cluster.LastRemovedAt.Before(now.Add(-grace)):
			// The scan's own cutoff, re-asked: a source removed within a resolve
			// grace may be on its way back, and a delete-then-register ends nothing.
			return "a source was removed from the cluster within resolve_grace"
		}
	default:
		return "not an expiry: " + reason.String()
	}
	return ""
}

// ------------------------------------------------------- the delayed close (W)

// CloseDueResult is the audit of one due-close pass.
type CloseDueResult struct {
	// Considered is how many episodes were past their retention window.
	Considered int
	// Closed is how many were closed as `resolved`/`upstream` — the resolve that
	// had been waiting, finally spent.
	Closed int
	// Superseded is how many were ABANDONED because the row had moved since the
	// scan: the alert re-fired inside the window and the receipt was cleared, or
	// somebody else ended the episode.
	//
	// THIS NUMBER IS THE FEATURE WORKING. Every increment is a flap that landed in
	// the still-open case instead of opening a new one — which is the entire point
	// of W — or a race the compare-and-set caught.
	Superseded int
}

// CloseDue performs the DELAYED CLOSE: it closes every episode whose upstream
// resolve has now been held for the whole case retention window W
// (`case_policy_config.retention_window_s`, migration 00057).
//
// ⭐⭐ WHY THIS EXISTS. Without W a flapping alert produces one Case per flap — six
// cases, six root cards, six pings — and since ADR 0040 a Case is strictly terminal
// so nothing merges them. The only damper left was at DELIVERY, and a withheld
// notification is indistinguishable from a signal that never fired, which is the
// one thing an alerting product cannot afford (§B.6). W shapes the CASE instead: a
// re-fire inside the window finds the episode still open and runs T2, so the noise
// never exists.
//
// ⛔⛔ THIS METHOD DOES PRODUCE `resolved`, AND IT IS NOT A HOLE IN RULE 1 ABOVE.
// Read Reap's rule 1 precisely: A RESOLUTION IS NEVER FABRICATED. This closes an
// episode whose row ALREADY CARRIES the receipt for an explicit upstream
// `status="resolved"` — `resolve_pending_at`/`resolve_pending_end_at`, written by
// §B.3's T5 arm from an ingest observation and by nothing else. It SPENDS a
// resolution; it cannot mint one. Three mechanisms hold that line:
//
//  1. `CloseDueCandidates` selects only rows with a receipt, so an episode nobody
//     resolved is unreachable from here.
//  2. `TriggerCloseDue` refuses the edge when the FRESH row has no pending close —
//     the same shape as `unreapable`, asking the row about to be overwritten.
//  3. The assertion below refuses anything but `resolved`/`upstream`, exactly as
//     Reap's refuses anything but `expired`/`timeout`.
//
// ⭐ AND `ended_at` IS THE UPSTREAM CLAIM, NOT THIS SWEEP'S CLOCK. The window is
// oto's own damper and must not be charged to the signal: closing at `now` would
// make every reader of firing duration (R8) — the case list, the daily rollup, the
// history enrichment's percentiles — report an episode W longer than the signal
// actually burned. The machine stamps `resolve_pending_end_at`, which is what the
// resolve observation claimed.
//
// ⛔ NO §B.4 SOURCE-HEALTH GUARD, and the asymmetry with Reap is the guard's own
// reasoning rather than an omission. §B.4 stops oto INFERRING an ending out of
// silence; there is no inference here. A source going dark after a resolve arrived
// does not un-resolve the alert, and holding the close would leave the episode open
// for the whole outage — which is the failure mode W was supposed to remove.
//
// It is a NO-OP on every deployment that has set no W: the candidate scan rides a
// partial index that is empty when no row carries a pending close.
func (s *Service) CloseDue(
	ctx context.Context, scope db.TenantScope, limit int,
) (CloseDueResult, error) {
	if limit <= 0 {
		limit = DefaultSweepLimit
	}
	now := s.Now()

	candidates, err := s.cases.CloseDueCandidates(ctx, scope, now, limit)
	if err != nil {
		return CloseDueResult{}, err
	}
	if len(candidates) == 0 {
		return CloseDueResult{}, nil
	}

	res := CloseDueResult{Considered: len(candidates)}
	for _, ac := range candidates {
		closed, err := s.closeDue(ctx, scope, ac, now)
		if err != nil {
			// One case failing must not cost the rest of the pass; the next tick
			// sees it again, and the receipt is still on the row.
			s.log.WarnContext(ctx, "alerts: could not close case at end of retention window",
				"case_id", ac.ID(), "error", err)
			continue
		}
		if closed {
			res.Closed++
		} else {
			res.Superseded++
		}
	}
	return res, nil
}

// closeDue moves ONE case through the delayed half of T5, in its own transaction
// so a single failure cannot roll back the whole pass.
//
// `candidate` is the STALE SNAPSHOT the scan returned and is used for its id alone.
// Everything the verdict rests on is re-read inside the transaction: between the
// scan and here a webhook has had every opportunity to land, and a webhook landing
// is exactly the case W exists to serve — the re-fire clears the receipt and this
// must then close nothing.
//
// It reports false with no error when the transition was abandoned, which the
// caller counts as Superseded.
//
// ⚠️ LOCK ORDER: THIS TRANSACTION TAKES `alert_cases` BEFORE `alerts`, the same
// order `expire` above takes them and the OPPOSITE of `Service.observe`. The note
// on `expire` is the whole argument and it applies here verbatim: the correctness of
// this path rests on the compare-and-set, not on a lock, precisely so that no lock
// has to be held across a sweep. ⛔ Do not add one to this site alone.
func (s *Service) closeDue(
	ctx context.Context, scope db.TenantScope, candidate domain.Case, now time.Time,
) (bool, error) {
	actor, err := domain.SystemActor(domain.ActorReaper)
	if err != nil {
		return false, err
	}
	at, err := domain.NewObservationTime(now, now)
	if err != nil {
		return false, err
	}

	closed := false
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		// ⭐ THE RE-READ. The candidate came from a scan outside any transaction;
		// this row is the one that will actually be overwritten.
		fresh, err := s.cases.GetByID(ctx, scope, candidate.ID())
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return nil // deleted under us; there is nothing to close
			}
			return err
		}

		// ⛔⛔ THE ASSERTION THAT MAKES "SPENDS, NEVER MINTS" MECHANICAL. It
		// interrogates THE ROW ABOUT TO BE OVERWRITTEN, which is the only form of
		// the question that cannot be answered about the wrong row — see the note
		// on `unreapable`. A re-fire inside the window lands here as
		// "no resolve is pending", and standing down IS the feature: the episode
		// stays open and carries the flap.
		if reason := unclosable(fresh, now); reason != "" {
			s.log.DebugContext(ctx, "alerts: delayed close stood down, the row disproved it",
				"case_id", fresh.ID(), "reason", reason)
			return nil
		}

		r, err := domain.Apply(fresh, domain.TransitionCommand{
			Trigger: domain.TriggerCloseDue,
			Actor:   actor,
			At:      at,
			EventID: id.New(),
		})
		if err != nil {
			if errs.IsKind(err, errs.KindPrecondition) {
				return nil
			}
			return err
		}
		if r.To != domain.StateResolved || r.Case.ResolveReason() != domain.ResolveUpstream {
			return errs.Internal("delayed_close_would_change_meaning",
				errsInvariant("the delayed close produced "+r.To.String()+
					"; only resolved/upstream is permitted"))
		}

		// No witnesses: a resolve names no suppressor, and `transitionOf` clears the
		// column. The precondition is `fresh`'s `state_version`, and EVERY write that
		// moves a decision input bumps it — `Observe` included — so a re-fire landing
		// in the microseconds between the re-read and this UPDATE loses this pass its
		// compare-and-set even though the row is still open. That is the intended
		// reading: an episode oto has heard from since it read the row is not one to
		// close on a resolve that predates the hearing.
		trans := transitionOf(r, domain.SuppressedBy{})
		if err := s.cases.Transition(ctx, scope, r.Case.ID(), trans); err != nil {
			// ⛔ ABANDON, never re-decide — the same rule the reaper follows and for a
			// stronger reason: every way to lose this compare-and-set is a way of
			// learning something new about an alert oto was about to declare over. The
			// receipt is still on the row if it should be, and the next tick re-reads
			// from scratch.
			if errs.IsKind(err, errs.KindConflict) {
				s.log.InfoContext(ctx, "alerts: delayed close lost the compare-and-set, abandoned",
					"case_id", r.Case.ID())
				return nil
			}
			return err
		}

		alert, err := s.alerts.GetByID(ctx, scope, r.Case.AlertID())
		if err != nil {
			return err
		}
		if err := s.projectFromCase(ctx, scope, alert, r.Case, at, true, 0); err != nil {
			return err
		}
		if _, err := s.appendEvents(ctx, scope, r.Events); err != nil {
			return err
		}
		if err := s.publishCase(ctx, scope, r.Case); err != nil {
			return err
		}
		if err := s.publishAlert(ctx, scope, alert.ID(), map[string]any{
			"state": domain.StateResolved.String(),
		}); err != nil {
			return err
		}
		// ⭐ ONE NOTIFICATION FOR THE WHOLE FLAP, AND THIS IS IT. The deferred T5s
		// that ran inside the window announced nothing (see
		// TransitionResult.CloseDeferred), so this is the first and only time the
		// channel is told the episode ended.
		if _, err := s.enqueueNotify(ctx, scope, []notifyRequest{{
			// `all_resolved`, exactly as an immediate T5 produces. ⛔ It was
			// `some_resolved` with the note that group-wholeness "is a fact about
			// membership this module does not read, and the notify worker upgrades
			// it" — there is no membership and no upgrade (git-bug `7570090`).
			reason:  reasonAllResolved,
			alertID: ptr(alert.ID()),
			caseID:  r.Case.ID(),
			actor:   domain.ActorReaper.String(),
		}}, nil); err != nil {
			return err
		}
		closed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return closed, nil
}

// unclosable re-proves the delayed close's preconditions AGAINST THE ROW THE SWEEP
// IS ABOUT TO OVERWRITE, and names the one that failed. An empty string means the
// row itself justifies the close.
//
// It is deliberately a duplicate of the checks inside domain.Apply's due-close
// branch, for the reason `unreapable` states: Apply answers about whatever case it
// is handed, and the failure being guarded against is Apply being handed a snapshot
// that had stopped being true.
func unclosable(row domain.Case, now time.Time) string {
	switch {
	case !row.IsOpen():
		// Somebody ended the episode first — an immediate T5 after the operator
		// narrowed W, or a rollback completing the close. There is nothing left to
		// do and nothing to correct.
		return "case is already " + row.AlertState().String()
	case !row.ClosePending():
		// ⭐ THE FEATURE, NOT A FAILURE. The alert re-fired inside the window, T2
		// cleared the receipt, and this episode is carrying the flap exactly as
		// intended. It is the reason this pass logs at debug rather than info.
		return "the alert re-fired inside the retention window"
	case !row.CloseDue(now):
		// A fresh resolve moved the due time forward: the rule is "stayed resolved
		// for W", so the window restarts on every resolve.
		return "the retention window has not elapsed"
	default:
		return ""
	}
}

// -------------------------------------------------------------- snooze expiry

// SnoozeExpiryResult is the audit of one `snooze.expire` tick.
type SnoozeExpiryResult struct {
	Considered int
	Expired    int
}

// ExpireSnoozes is the 60-second `snooze.expire` sweep (§B.8.3).
//
// It ends every active snooze whose clock has run out — by stamping `ended_at`
// on the row and nothing else, because there is no projection to clear any more —
// appends `alert.unsnoozed` with reason `expired`, and — when the alert's episode
// is still open — enqueues `notify.evaluate(reason=unsnoozed)` so the channel is
// told oto is speaking again.
//
// The actor is `system` and never a human (§B.8.5); the domain refuses the other
// combination. Nothing here touches state, ack_state or severity: an expiring
// snooze wakes oto up, it does not change the world.
func (s *Service) ExpireSnoozes(
	ctx context.Context, scope db.TenantScope, limit int,
) (SnoozeExpiryResult, error) {
	if limit <= 0 {
		limit = DefaultSweepLimit
	}
	now := s.Now()

	due, err := s.snoozes.ExpiredCandidates(ctx, scope, now, limit)
	if err != nil {
		return SnoozeExpiryResult{}, err
	}
	if len(due) == 0 {
		return SnoozeExpiryResult{}, nil
	}

	actor, err := domain.SystemActor(domain.ActorSystem)
	if err != nil {
		return SnoozeExpiryResult{}, err
	}
	at, err := domain.NewObservationTime(now, now)
	if err != nil {
		return SnoozeExpiryResult{}, err
	}

	res := SnoozeExpiryResult{Considered: len(due)}
	for _, snz := range due {
		err := s.tx.InTx(ctx, func(ctx context.Context) error {
			alert, err := s.alerts.GetByID(ctx, scope, snz.AlertID())
			if err != nil {
				return err
			}
			// An expiry is the reaper's, and the reaper has nothing to say.
			_, evs, err := s.endSnooze(ctx, scope, snz, actor, domain.SnoozeEndedExpired, at, "")
			if err != nil {
				return err
			}
			if _, err := s.appendEvents(ctx, scope, evs); err != nil {
				return err
			}
			if err := s.publishAlert(ctx, scope, alert.ID(), map[string]any{
				"snoozed_until": nil,
			}); err != nil {
				return err
			}
			// The occasion is the snooze that just expired, so a second expiry inside
			// the same episode is a second announcement rather than a duplicate key.
			return s.notifySnoozeChange(ctx, scope, alert.ID(), reasonUnsnoozed,
				domain.ActorSystem.String(), snz.ID())
		})
		if err != nil {
			s.log.WarnContext(ctx, "alerts: could not expire snooze",
				"snooze_id", snz.ID(), "error", err)
			continue
		}
		res.Expired++
	}
	return res, nil
}

// ------------------------------------------------------ flap score (RETIRED)

// ⛔⛔ THERE IS NO `ScoreFlaps` ANY MORE, AND NO `FlapResult` WITH IT. It was the
// `flap.score` job (§B.6): count each Alert's lifecycle transitions inside
// `flap_window`, divide by the window to get transitions per hour, write the pair
// `alerts.flap_score` / `alerts.is_flapping` through `SetFlap`, and mint
// `alert.flapping_started` / `alert.flapping_ended` on a crossing. All of it is
// gone — the job kind, the periodic tick, the handler, the port method and the
// UPDATE — and the two event types are retired in `alerts/domain/event.go`.
//
// ⭐⭐ IT DID NOT GO DEAD. IT WENT BLIND, AND THAT IS WHY TUNING IT WAS NOT AN
// OPTION. The count came from `stateChangeCountsSQL`, which counts `case.opened`,
// `case.resolved`, `case.expired`, `case.suppressed` and `case.unsuppressed`. The
// case retention window W (migration 00057) damps a flap AT CASE FORMATION: a
// re-fire inside W lands in the STILL-OPEN case, so the resolve is held and no new
// case opens, and the damped episode appends NEITHER of the two events the score
// lives on. Six flaps in ten minutes used to append twelve counted events; damped
// they append about two, against `DefaultFlapThreshold = 5` over
// `DefaultFlapWindow = 7200 s`. `is_flapping` therefore read FALSE exactly when the
// alert was flapping, and `alert.flapping_ended` would have been minted BECAUSE the
// flapping got worse. A detector that lies is worse than no detector.
//
// ⛔ THE FIX THAT WAS REFUSED, so nobody re-proposes it: feeding the deferred
// resolve into the score needs a NEW `alert_events.type` for an edge that records a
// resolve without performing it — an API-contract change minted to keep a
// second-order damper alive behind the one that already works. W IS the flap
// answer now (ADR 0041, Amendment 1), and one damper is the whole point.
//
// ⭐ WHAT SURVIVES, AND WHY IT IS NOT A CONTRADICTION. `alerts.flap_score` and
// `alerts.is_flapping` are RETIRED IN PLACE, not dropped: every read keeps working —
// the list filter, the rollup, the enrichment card, the notification snapshot — so
// the last value a row carries stays interpretable rather than becoming a column
// that errors. Retired is not deleted; unwritable is not unreadable.

// PruneEventKeys ages out the C.8 dedupe keys of `alert_event_keys` and reports
// how many went. It is the alerts half of `retention.prune`.
//
// ⛔ THE CALLER OWNS THE HORIZON, and it is not this method's to guess. The floor
// is `domain.DedupeKeyRetention`, reached only when the DEPLOYMENT's
// `OTO_RETENTION_RAW_PAYLOADS` is set under 720h — not by any per-org
// `raw_retention_days`, which `app.effectiveRetention` can only ever widen past the
// deployment value. Above that floor the sweep widens to the longest
// `raw_retention_days` any tenant configured, plus the day of partition grain the
// raw payloads outlive their nominal window by, because a key deleted while its
// batch is still replayable turns SPEC acceptance criterion 36's replay into a
// silent no-op — `AppendBatch` finds nothing to write, returns zero, and zero is
// documented as the idempotency mechanism working. `app.pruneRetention` computes
// it from the same `effectiveRetention` its sibling `partitions.manage` drops on,
// so the two windows cannot drift apart.
//
// ⚠️ NO TenantScope, and this is the one place in the alerts service where that
// is right: a per-tenant sweep would need a per-tenant horizon, and the horizon
// is deliberately the widest one — a key claimed by an org that keeps payloads
// for a day still guards a timeline the reconciler may re-apply, and that org's
// raw partitions are on the install-wide window anyway.
func (s *Service) PruneEventKeys(ctx context.Context, before time.Time, limit int) (int64, error) {
	return s.events.PruneDedupeKeys(ctx, before, limit)
}

func errsInvariant(what string) error {
	return errs.New(errs.KindInternal, "invariant_violated", what)
}
