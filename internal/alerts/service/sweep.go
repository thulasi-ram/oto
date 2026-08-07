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

	res := ReapResult{Considered: len(candidates)}
	heldSources := map[uuid.UUID]struct{}{}

	for _, occ := range candidates {
		sourceID, known := sources[occ.ID()]
		healthy := known && s.sourceHealthy(ctx, scope, sourceID)
		if !healthy {
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

// sourceHealthy answers the §B.4 guard, and answers "no" whenever it does not
// know. An unwired port, a failed lookup and an unhealthy source are the same
// answer here, deliberately.
func (s *Service) sourceHealthy(ctx context.Context, scope db.TenantScope, sourceID uuid.UUID) bool {
	if s.health == nil || sourceID == uuid.Nil {
		return false
	}
	ok, err := s.health.Healthy(ctx, scope, sourceID)
	if err != nil {
		s.log.WarnContext(ctx, "alerts: source health unknown, holding occurrence",
			"source_id", sourceID, "error", err)
		return false
	}
	return ok
}

// expire moves ONE occurrence through T6, in its own transaction so that a
// single failure cannot roll back a whole sweep.
func (s *Service) expire(
	ctx context.Context, scope db.TenantScope, occ domain.Occurrence, now time.Time, cfg Settings,
) (bool, error) {
	actor, err := domain.SystemActor(domain.ActorReaper)
	if err != nil {
		return false, err
	}
	at, err := domain.NewObservationTime(now, now)
	if err != nil {
		return false, err
	}

	r, err := domain.Apply(occ, domain.TransitionCommand{
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
			return false, nil
		}
		return false, err
	}

	// ⛔ The assertion that makes rule 1 above mechanical rather than aspirational.
	if r.To != domain.StateExpired || r.Occurrence.ResolveReason() != domain.ResolveTimeout {
		return false, errs.Internal("reaper_would_fabricate_resolution",
			errsInvariant("the reaper produced "+r.To.String()+"; only expired/timeout is permitted"))
	}

	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.occurrences.Transition(ctx, scope, r.Occurrence.ID(), transitionOf(r)); err != nil {
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
		_, err = s.enqueueNotify(ctx, scope, []notifyRequest{{
			groupID:      r.Occurrence.GroupID(),
			reason:       reasonExpired,
			alertID:      ptr(alert.ID()),
			occurrenceID: ptr(r.Occurrence.ID()),
			actor:        domain.ActorReaper.String(),
		}})
		return err
	})
	if err != nil {
		return false, err
	}
	return true, nil
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
			_, evs, err := s.endSnooze(ctx, scope, snz, actor, domain.SnoozeEndedExpired, at)
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

	counts, err := s.eventCounts.StateChangeCounts(ctx, scope, window)
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
		if res.Scored >= limit {
			break
		}
		perHour := float32(n) / float32(cfg.FlapWindow.Hours())
		flapping := n >= cfg.FlapThreshold

		err := s.tx.InTx(ctx, func(ctx context.Context) error {
			alert, err := s.alerts.GetByID(ctx, scope, alertID)
			if err != nil {
				return err
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

func errsInvariant(what string) error {
	return errs.New(errs.KindInternal, "invariant_violated", what)
}
