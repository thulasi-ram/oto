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

// ReapResult is the audit of one `occurrence.reap` tick.
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
	// banner can be raised per source rather than one per occurrence.
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

// Reap is the `occurrence.reap` sweep — SPEC §B.3 T6.
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
//     of an alert is not the same as the alert resolving. An occurrence whose
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

	candidates, err := s.occurrences.ReapCandidates(ctx, scope, before, limit)
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

	for _, occ := range candidates {
		sourceID, known := sources[occ.ID()]
		if !known || !healthy[sourceID] {
			res.Held++
			if known {
				heldSources[sourceID] = struct{}{}
			}
			continue
		}
		expired, err := s.expire(ctx, scope, occ, now, cfg)
		if err != nil {
			// One occurrence failing must not cost the rest of the sweep; the
			// next tick will see it again in sixty seconds.
			s.log.WarnContext(ctx, "alerts: could not expire occurrence",
				"occurrence_id", occ.ID(), "error", err)
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
		s.log.InfoContext(ctx, "alerts: reaper held occurrences, source not proven healthy",
			"org_id", scope.OrgID(), "held", res.Held, "sources", len(res.HeldSources))
	}
	return res, nil
}

// resolveSources maps every candidate onto its owning AlertSource. An occurrence
// absent from the result is one whose source could not be determined, and the
// caller reads that as "cannot prove healthy".
func (s *Service) resolveSources(
	ctx context.Context, scope db.TenantScope, candidates []domain.Occurrence,
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
// "oto does not know" — and the caller holds every occurrence they own. That is
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

// expire moves ONE occurrence through T6, in its own transaction so that a
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
// ⚠️ LOCK ORDER: THIS TRANSACTION TAKES `alert_occurrences` BEFORE `alerts`.
// `Service.observe` (lifecycle.go) takes them the OTHER WAY ROUND — `UpsertBatch`
// locks the alert row before the occurrence is even read. The two orders form a
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
	ctx context.Context, scope db.TenantScope, candidate domain.Occurrence, now time.Time, cfg Settings,
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
		fresh, err := s.occurrences.GetByID(ctx, scope, candidate.ID())
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
		// guarded nothing at all: the machine had been fed a stale occurrence,
		// answered honestly about it, and the assertion nodded at an answer to the
		// wrong question while `expired`/`timeout` went over a firing alert.
		if reason := unreapable(fresh, now, cfg.ResolveGrace); reason != "" {
			s.log.InfoContext(ctx, "alerts: reaper stood down, the row disproved the expiry",
				"occurrence_id", fresh.ID(), "reason", reason)
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
		if r.To != domain.StateExpired || r.Occurrence.ResolveReason() != domain.ResolveTimeout {
			return errs.Internal("reaper_would_fabricate_resolution",
				errsInvariant("the reaper produced "+r.To.String()+"; only expired/timeout is permitted"))
		}

		// No witnesses: the reaper has no observation, and an expiry names no
		// suppressor. transitionOf clears the column, which is what T6 means —
		// oto stopped hearing about the alert, not "Alertmanager is muting it".
		// The precondition is `fresh`'s `state_version`, and `Observe` bumps that
		// too — so a repeat webhook landing in the microseconds between the re-read
		// above and this UPDATE loses the reaper its compare-and-set even though it
		// moved no state letter. That is the intended reading: an occurrence oto has
		// heard about since it read the row is not one oto has stopped hearing about.
		trans := transitionOf(r, domain.SuppressedBy{})
		if err := s.occurrences.Transition(ctx, scope, r.Occurrence.ID(), trans); err != nil {
			// ⛔ ABANDON, never re-decide. The reaper is the one caller that must
			// NOT retry a lost compare-and-set: every reason it can lose one is a
			// reason not to expire — somebody ended the episode, or something was
			// heard about an alert oto was about to declare silent. The sweep runs
			// again in sixty seconds and will re-read from scratch, which is a
			// strictly safer place to reconsider than a hot loop holding a verdict.
			if errs.IsKind(err, errs.KindConflict) {
				s.log.InfoContext(ctx, "alerts: reaper lost the compare-and-set, expiry abandoned",
					"occurrence_id", r.Occurrence.ID())
				return nil
			}
			return err
		}

		alert, err := s.alerts.GetByID(ctx, scope, r.Occurrence.AlertID())
		if err != nil {
			return err
		}
		if err := s.projectFromOccurrence(ctx, scope, alert, r.Occurrence, at, true, 0); err != nil {
			return err
		}
		if _, err := s.appendEvents(ctx, scope, r.Events); err != nil {
			return err
		}
		if err := s.publishOccurrence(ctx, scope, r.Occurrence); err != nil {
			return err
		}
		if err := s.publishAlert(ctx, scope, alert.ID(), map[string]any{
			"state": domain.StateExpired.String(),
		}); err != nil {
			return err
		}
		if _, err := s.enqueueNotify(ctx, scope, []notifyRequest{{
			groupID:      r.Occurrence.GroupID(),
			reason:       reasonExpired,
			alertID:      ptr(alert.ID()),
			occurrenceID: ptr(r.Occurrence.ID()),
			actor:        domain.ActorReaper.String(),
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
// duplication is the point: Apply answers about whatever occurrence it is handed,
// and the bug this guards was Apply being handed a snapshot that had stopped
// being true. Asking the same questions of the row about to be written is the
// only form of the question that cannot be answered about the wrong row.
func unreapable(row domain.Occurrence, now time.Time, grace time.Duration) string {
	switch {
	case !row.IsOpen():
		// The loudest case: overwriting a `resolved` with `expired` replaces a
		// fact somebody upstream stated with one oto inferred, and leaves the
		// append-only timeline permanently disagreeing with the projection.
		return "occurrence is already " + row.State().String()
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

// -------------------------------------------------------------- snooze expiry

// SnoozeExpiryResult is the audit of one `snooze.expire` tick.
type SnoozeExpiryResult struct {
	Considered int
	Expired    int
}

// ExpireSnoozes is the 60-second `snooze.expire` sweep (§B.8.3).
//
// It ends every active snooze whose clock has run out, clears the projection,
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
			if err := s.writeSnoozeProjection(ctx, scope, alert, nil); err != nil {
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

// ---------------------------------------------------------------- flap score

// FlapResult is the audit of one `flap.score` tick.
type FlapResult struct {
	Scored          int
	FlappingStarted int
	FlappingEnded   int
}

// ScoreFlaps recomputes flap scores (§B.6).
//
// The score is transitions per hour over the configured window, and an Alert
// above `flap_threshold` is MARKED flapping. Marking is the whole point:
// occurrences still open and close normally and nothing is hidden — flapping is a
// VISIBLE UI state, never silent suppression. What changes is downstream, where
// `notify.evaluate` switches to update-only mode.
//
// The crossing is recorded on the timeline as `alert.flapping_started` /
// `alert.flapping_ended`, so a user can always see why oto went quieter.
func (s *Service) ScoreFlaps(ctx context.Context, scope db.TenantScope, limit int) (FlapResult, error) {
	if s.eventCounts == nil {
		return FlapResult{}, nil
	}
	if limit <= 0 {
		limit = DefaultSweepLimit
	}
	cfg := s.lifecycleSettings(ctx, scope)
	now := s.Now()
	window := db.TimeWindow{From: now.Add(-cfg.FlapWindow), To: now}

	// The cap travels WITH the query. Which alerts get scored when it binds is
	// the port's contract — most-changed first — not an artifact of iterating
	// this map, so everything that comes back is processed.
	counts, err := s.eventCounts.StateChangeCounts(ctx, scope, window, limit)
	if err != nil {
		return FlapResult{}, err
	}
	if len(counts) == 0 {
		return FlapResult{}, nil
	}

	at, err := domain.NewObservationTime(now, now)
	if err != nil {
		return FlapResult{}, err
	}
	actor, err := domain.SystemActor(domain.ActorSystem)
	if err != nil {
		return FlapResult{}, err
	}

	res := FlapResult{}
	for alertID, n := range counts {
		perHour := float32(n) / float32(cfg.FlapWindow.Hours())
		flapping := n >= cfg.FlapThreshold

		err := s.tx.InTx(ctx, func(ctx context.Context) error {
			alert, err := s.alerts.GetByID(ctx, scope, alertID)
			if err != nil {
				return err
			}
			// The steady state writes nothing. Most ticks recompute the same score
			// for the same alerts, and an UPDATE that changes neither column is
			// pure WAL churn on the hottest table in the system. The float32
			// comparison is exact: `flap_score` is REAL, so the value read back is
			// bit-for-bit the value SetFlap wrote.
			if alert.IsFlapping() == flapping && alert.FlapScore() == perHour {
				return nil
			}
			if err := s.alerts.SetFlap(ctx, scope, alertID, perHour, flapping); err != nil {
				return err
			}
			if alert.IsFlapping() == flapping {
				return nil
			}
			typ := domain.EventAlertFlappingEnded
			summary := "Alert stopped flapping"
			if flapping {
				typ = domain.EventAlertFlappingStarted
				summary = "Alert is flapping: notifications switch to update-only"
			}
			ev, err := domain.NewEvent(domain.EventParams{
				ID:      id.New(),
				OrgID:   scope.OrgID(),
				AlertID: alertID,
				Type:    typ,
				At:      at,
				Actor:   actor,
				Summary: summary,
				Payload: map[string]any{
					"flap_score":  perHour,
					"transitions": n,
					"window_s":    int64(cfg.FlapWindow.Seconds()),
				},
				DedupeKey: "alert:" + alertID.String() + ":flap:" + typ.String() + ":" +
					now.Truncate(cfg.FlapWindow).Format(time.RFC3339),
			})
			if err != nil {
				return err
			}
			if _, err := s.appendEvents(ctx, scope, []domain.Event{ev}); err != nil {
				return err
			}
			if flapping {
				res.FlappingStarted++
			} else {
				res.FlappingEnded++
			}
			return nil
		})
		if err != nil {
			s.log.WarnContext(ctx, "alerts: could not score flap", "alert_id", alertID, "error", err)
			continue
		}
		res.Scored++
	}
	return res, nil
}

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
