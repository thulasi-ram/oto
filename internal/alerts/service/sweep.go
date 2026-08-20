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
	// Considered is how many open episodes were past source_ends_at +
	// resolve_grace.
	Considered int
	// Expired is how many were moved to `expired` — which is NOT `resolved`.
	Expired int
	// Held is how many were left exactly as they were because their AlertSource
	// could not be proven healthy. THIS NUMBER IS A FEATURE: it is the
	// `source_degraded_holds` counter of §B.4, and it should be exported.
	Held int
	// HeldSources names the sources responsible, so one `source.unreachable`
	// banner can be raised per source rather than one per case.
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
//     `resolve_reason='timeout'`, and the assertion below refuses anything else.
//
//  2. THE REAPER IS BLOCKED WHILE THE SOURCE IS NOT HEALTHY (§B.4). Losing sight
//     of an alert is not the same as the alert resolving. A case whose
//     AlertSource cannot be PROVEN healthy is HELD in its current state —
//     including when the health port is not wired at all, when the source cannot
//     be resolved, and when the health lookup itself fails. Every one of those is
//     "oto does not know", and "oto does not know" must never end an episode.
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

	candidates, err := s.cases.ReapCandidates(ctx, scope, before, limit)
	if err != nil {
		return ReapResult{}, err
	}
	if len(candidates) == 0 {
		return ReapResult{}, nil
	}

	sources, err := s.resolveSources(ctx, scope, candidates)
	if err != nil {
		return ReapResult{}, err
	}
	healthy := s.healthBySource(ctx, scope, sources)

	res := ReapResult{Considered: len(candidates)}
	heldSources := map[uuid.UUID]struct{}{}

	for _, ac := range candidates {
		sourceID, known := sources[ac.ID()]
		if !known || !healthy[sourceID] {
			res.Held++
			if known {
				heldSources[sourceID] = struct{}{}
			}
			continue
		}
		expired, err := s.expire(ctx, scope, ac, now, cfg)
		if err != nil {
			// One case failing must not cost the rest of the sweep; the
			// next tick will see it again in sixty seconds.
			s.log.WarnContext(ctx, "alerts: could not expire case",
				"case_id", ac.ID(), "error", err)
			continue
		}
		if expired {
			res.Expired++
		} else {
			res.Superseded++
		}
	}

	for src := range heldSources {
		res.HeldSources = append(res.HeldSources, src)
	}
	if res.Held > 0 {
		s.log.InfoContext(ctx, "alerts: reaper held cases, source not proven healthy",
			"org_id", scope.OrgID(), "held", res.Held, "sources", len(res.HeldSources))
	}
	return res, nil
}

// resolveSources maps every candidate onto its owning AlertSource. A case
// absent from the result is one whose source could not be determined, and the
// caller reads that as "cannot prove healthy".
func (s *Service) resolveSources(
	ctx context.Context, scope db.TenantScope, candidates []domain.Case,
) (map[uuid.UUID]uuid.UUID, error) {
	if s.occSources == nil {
		return map[uuid.UUID]uuid.UUID{}, nil
	}
	ids := make([]uuid.UUID, len(candidates))
	for i, o := range candidates {
		ids[i] = o.ID()
	}
	return s.occSources.SourceIDs(ctx, scope, ids)
}

// healthBySource answers the §B.4 guard for every DISTINCT source the tick's
// candidates resolved to, in one round trip. The guard is per source, so its
// cost must be per source: 500 candidates over 3 sources is 3 health rows, not
// 500 lookups — and the worst case for the per-candidate version was exactly the
// §B.4 case, a source outage, when nothing expires and every candidate returns
// next tick.
//
// ABSENCE FROM THE RESULT IS "NO". An unwired port, a nil source id, a failed
// lookup and a source the batch simply did not return are all the same answer —
// "oto does not know" — and the caller holds every case they own. That is
// why a failed lookup is reported by returning nothing rather than by an error:
// not knowing holds candidates, it must never abort the sweep.
func (s *Service) healthBySource(
	ctx context.Context, scope db.TenantScope, sources map[uuid.UUID]uuid.UUID,
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
func (s *Service) expire(
	ctx context.Context, scope db.TenantScope, candidate domain.Case, now time.Time, cfg Settings,
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
		if reason := unreapable(fresh, now, cfg.ResolveGrace); reason != "" {
			s.log.InfoContext(ctx, "alerts: reaper stood down, the row disproved the expiry",
				"case_id", fresh.ID(), "reason", reason)
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
			SourceHealthy: true,
		})
		if err != nil {
			if errs.IsKind(err, errs.KindPrecondition) {
				return nil
			}
			return err
		}
		if r.To != domain.StateExpired || r.Case.ResolveReason() != domain.ResolveTimeout {
			return errs.Internal("reaper_would_fabricate_resolution",
				errsInvariant("the reaper produced "+r.To.String()+"; only expired/timeout is permitted"))
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
			return s.notifySnoozeChange(ctx, scope, alert.ID(), reasonUnsnoozed,
				domain.ActorSystem.String())
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
