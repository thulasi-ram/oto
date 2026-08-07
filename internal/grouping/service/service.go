package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// Deps is the explicit dependency set of the grouping service.
//
// The two repositories, the transaction runner and the group-key function are
// required. Events, streaming, notification and member actions are optional and
// degrade to doing nothing, so oto runs with notifications entirely disabled.
type Deps struct {
	Groups  GroupRepository
	Members MemberRepository
	Tx      TxRunner

	Events   EventAppender
	Timeline TimelineReader
	Actions  MemberActions
	Stream   StreamAppender
	Enqueuer db.Enqueuer
	Settings SettingsReader

	Clock  clock.Clock
	Logger *slog.Logger
}

// Service owns durable AlertGroups: their §C.4 identity, their generations, their
// membership, their rollups and their storm damping.
//
// ⛔ An AlertGroup here is ONE GENERATION of ONE Alertmanager notification group,
// and it OWNS EXACTLY ONE Slack thread. It is never a UI grouping — that is a
// view, and a view has no row, no thread and no generation (§A.1).
//
// ⛔ It NEVER calls time.Now(); every instant comes from the injected clock.
type Service struct {
	groups  GroupRepository
	members MemberRepository
	tx      TxRunner

	events   EventAppender
	timeline TimelineReader
	actions  MemberActions
	stream   StreamAppender
	enqueuer db.Enqueuer
	settings SettingsReader

	clock clock.Clock
	log   *slog.Logger
}

// New builds the grouping service, refusing a dependency set that cannot work.
func New(d Deps) (*Service, error) {
	switch {
	case d.Groups == nil:
		return nil, errs.Internal("group_repo_required", errMissingDep("GroupRepository"))
	case d.Members == nil:
		return nil, errs.Internal("member_repo_required", errMissingDep("MemberRepository"))
	case d.Tx == nil:
		return nil, errs.Internal("tx_runner_required", errMissingDep("TxRunner"))
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.New()
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		groups:   d.Groups,
		members:  d.Members,
		tx:       d.Tx,
		events:   d.Events,
		timeline: d.Timeline,
		actions:  d.Actions,
		stream:   d.Stream,
		enqueuer: d.Enqueuer,
		settings: d.Settings,
		clock:    clk,
		log:      logger,
	}, nil
}

func errMissingDep(name string) error {
	return errs.New(errs.KindInternal, "missing_dependency", "grouping service requires "+name)
}

// Now is the service's clock reading, in UTC.
func (s *Service) Now() time.Time { return s.clock.Now().UTC() }

func (s *Service) policy(ctx context.Context, scope db.TenantScope) domain.StormPolicy {
	if s.settings == nil {
		return domain.DefaultStormPolicy()
	}
	p, err := s.settings.Storm(ctx, scope)
	if err != nil {
		s.log.WarnContext(ctx, "grouping: falling back to default storm policy",
			"org_id", scope.OrgID(), "error", err)
		return domain.DefaultStormPolicy()
	}
	return p.Normalise()
}

// ------------------------------------------------------------------- resolve

// ResolveRequest names the notification group an observation belongs to.
type ResolveRequest struct {
	SourceID  uuid.UUID
	ClusterID uuid.UUID
	// Receiver is the Alertmanager receiver name, "" for a reconciler-sourced
	// group with no groupLabels.
	Receiver string
	// GroupLabels are the labels Alertmanager grouped by.
	GroupLabels map[string]string
	// SourceGroupKey is Alertmanager's raw groupKey. It is stored VERBATIM for
	// observability and MUST NOT be parsed: it is unescaped and unbounded (C3).
	SourceGroupKey string
	// NotificationReason is Alertmanager's own notification_reason for this
	// delivery, feeding the §H.6 decision table.
	NotificationReason string
	At                 time.Time
}

// Resolve returns the OPEN generation for a group key, opening one if none is
// open (§G.4 step 4).
//
// ⭐ The key is the DURABLE §C.4 key and never Alertmanager's `groupKey`. AM's
// value embeds the route path and changes on every `alertmanager.yml` reload, so
// a group keyed by it would be reborn — with a new Slack thread — every time an
// operator edited a route.
//
// ⭐ Opening a generation when the previous one CLOSED is what gives a re-opened
// group a new thread: yesterday's conversation is not where today's incident
// belongs. Rejoining a still-open generation reuses the thread, which is why
// `chat.update` is the primary Slack verb and a new root message is the exception
// (§B.5).
func (s *Service) Resolve(
	ctx context.Context, scope db.TenantScope, in ResolveRequest,
) (domain.Group, error) {
	key, err := alerts.GroupKeyFor(scope.OrgID(), in.SourceID, in.Receiver, in.GroupLabels)
	if err != nil {
		return domain.Group{}, err
	}
	at := in.At
	if at.IsZero() {
		at = s.Now()
	}

	var out domain.Group
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		if g, ok, err := s.groups.GetOpenByKey(ctx, scope, key); err != nil {
			return err
		} else if ok {
			if err := s.groups.Touch(ctx, scope, g.ID(), at); err != nil {
				return err
			}
			if in.NotificationReason != "" {
				if err := s.groups.SetNotificationReason(ctx, scope, g.ID(),
					in.NotificationReason); err != nil {
					return err
				}
			}
			out = g.Touch(at)
			return nil
		}

		g, err := s.groups.OpenGeneration(ctx, scope, repository.NewGeneration{
			ID:             id.New(),
			SourceID:       in.SourceID,
			ClusterID:      in.ClusterID,
			GroupKey:       key,
			SourceGroupKey: in.SourceGroupKey,
			Receiver:       in.Receiver,
			GroupLabels:    in.GroupLabels,
			Title:          domain.Title(in.GroupLabels, in.Receiver),
			At:             at,
		})
		if err != nil {
			// Two ingest workers can race the same first observation. The unique
			// key (org_id, group_key, generation) decides it; the loser re-reads
			// rather than opening a second live generation of one group.
			if errs.IsKind(err, errs.KindConflict) {
				if g, ok, rerr := s.groups.GetOpenByKey(ctx, scope, key); rerr == nil && ok {
					out = g
					return nil
				}
			}
			return err
		}

		if err := s.appendGroupEvent(ctx, scope, alerts.GroupEventRequest{
			Type:    alerts.GroupEventOpened,
			GroupID: g.ID(),
			Summary: "Alert group opened: " + g.Title(),
			Payload: map[string]any{
				"group_key":  g.Key().String(),
				"generation": g.Generation(),
				"receiver":   g.Receiver(),
			},
			DedupeKey:  "group:" + g.ID().String() + ":opened",
			OccurredAt: at,
		}); err != nil {
			return err
		}
		if err := s.publish(ctx, scope, g); err != nil {
			return err
		}
		out = g
		return nil
	})
	if err != nil {
		return domain.Group{}, err
	}
	return out, nil
}

// -------------------------------------------------------------- membership

// JoinResult is what one Join did.
type JoinResult struct {
	Group domain.Group
	// Joined is false when the occurrence was already a member, which is a
	// redelivered batch working as designed rather than an error.
	Joined bool
	// StormStarted and StormEnded report a damping transition, which is a VISIBLE
	// state change and is recorded on the timeline (§B.6).
	StormStarted bool
	StormEnded   bool
}

// Join adds an occurrence to a generation and re-derives everything that follows
// from membership: the rollup, the group state, the severity, the storm decision
// and — when any of it was material — the `state_version` a Notification is
// minted against (§C.7).
func (s *Service) Join(
	ctx context.Context, scope db.TenantScope, groupID, alertID, occurrenceID uuid.UUID, at time.Time,
) (JoinResult, error) {
	if at.IsZero() {
		at = s.Now()
	}
	policy := s.policy(ctx, scope)

	var out JoinResult
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		joined, err := s.members.Join(ctx, scope, groupID, occurrenceID, alertID, at)
		if err != nil {
			return err
		}
		out.Joined = joined

		if joined {
			if err := s.appendGroupEvent(ctx, scope, alerts.GroupEventRequest{
				Type:         alerts.GroupEventMemberJoined,
				GroupID:      groupID,
				AlertID:      alertID,
				OccurrenceID: occurrenceID,
				Summary:      "Alert joined the group",
				DedupeKey:    "group:" + groupID.String() + ":joined:" + occurrenceID.String(),
				OccurredAt:   at,
			}); err != nil {
				return err
			}
		}

		g, material, err := s.recompute(ctx, scope, groupID, at)
		if err != nil {
			return err
		}
		out.Group = g

		stormed, err := s.evaluateStorm(ctx, scope, g, at, policy)
		if err != nil {
			return err
		}
		out.Group = stormed.group
		out.StormStarted = stormed.started
		out.StormEnded = stormed.ended

		if material || stormed.started || stormed.ended {
			if err := s.publish(ctx, scope, out.Group); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return JoinResult{}, err
	}
	return out, nil
}

// Leave records that an occurrence stopped being a member and re-derives the
// rollup. The membership row survives — membership is history (§D.5).
func (s *Service) Leave(
	ctx context.Context, scope db.TenantScope, groupID, alertID, occurrenceID uuid.UUID, at time.Time,
) error {
	if at.IsZero() {
		at = s.Now()
	}
	return s.tx.InTx(ctx, func(ctx context.Context) error {
		left, err := s.members.Leave(ctx, scope, groupID, occurrenceID, at)
		if err != nil {
			return err
		}
		if left {
			if err := s.appendGroupEvent(ctx, scope, alerts.GroupEventRequest{
				Type:         alerts.GroupEventMemberLeft,
				GroupID:      groupID,
				AlertID:      alertID,
				OccurrenceID: occurrenceID,
				Summary:      "Alert left the group",
				DedupeKey:    "group:" + groupID.String() + ":left:" + occurrenceID.String(),
				OccurredAt:   at,
			}); err != nil {
				return err
			}
		}
		g, material, err := s.recompute(ctx, scope, groupID, at)
		if err != nil {
			return err
		}
		if material {
			return s.publish(ctx, scope, g)
		}
		return nil
	})
}

// Recompute re-derives a generation's rollup after a member's state changed.
//
// It is the entry point the alerts lifecycle calls after a transition: the group
// counts, the group state and the group severity are all PROJECTIONS of the
// member occurrences, and this is where the projection is refreshed.
func (s *Service) Recompute(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, at time.Time,
) (domain.Group, error) {
	if at.IsZero() {
		at = s.Now()
	}
	var out domain.Group
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		g, material, err := s.recompute(ctx, scope, groupID, at)
		if err != nil {
			return err
		}
		out = g
		if material {
			return s.publish(ctx, scope, g)
		}
		return nil
	})
	if err != nil {
		return domain.Group{}, err
	}
	return out, nil
}

// recompute rolls the current members up onto the generation and reports whether
// anything MATERIAL moved.
//
// Material means a count changed or the severity changed. Only a material change
// bumps `state_version`, and only a bumped version lets a new Notification exist
// (§C.7) — which is exactly what stops a repeat observation re-notifying.
func (s *Service) recompute(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, at time.Time,
) (domain.Group, bool, error) {
	g, err := s.groups.GetByID(ctx, scope, groupID)
	if err != nil {
		return domain.Group{}, false, err
	}
	counts, severity, err := s.members.Rollup(ctx, scope, groupID)
	if err != nil {
		return domain.Group{}, false, err
	}
	next, material, err := g.WithRollup(counts, severity, at)
	if err != nil {
		return domain.Group{}, false, err
	}
	if !material {
		return next, false, nil
	}
	if err := s.groups.SetRollup(ctx, scope, next); err != nil {
		return domain.Group{}, false, err
	}
	return next, true, nil
}

// ------------------------------------------------------------------- storm

type stormOutcome struct {
	group   domain.Group
	started bool
	ended   bool
}

// evaluateStorm applies §B.6 storm collapse.
//
// ⭐ Storm mode is a VISIBLE UI STATE, never silent suppression. Entering it does
// not hide a single alert: every occurrence still opens, closes and appears in the
// list and on the timeline. What collapses is oto's OWN chatter — one root message
// with a count and a link instead of N thread replies — and the fact that oto went
// quiet is itself posted, recorded and shown.
func (s *Service) evaluateStorm(
	ctx context.Context, scope db.TenantScope, g domain.Group, at time.Time, p domain.StormPolicy,
) (stormOutcome, error) {
	joins, lastJoin, err := s.members.DistinctJoinsSince(ctx, scope, g.ID(), at.Add(-p.Window))
	if err != nil {
		return stormOutcome{}, err
	}
	decision := domain.EvaluateStorm(g, joins, lastJoin, at, p)
	next, changed := domain.ApplyStorm(g, decision)
	if !changed {
		return stormOutcome{group: g}, nil
	}
	if err := s.groups.SetStorm(ctx, scope, next); err != nil {
		return stormOutcome{}, err
	}

	typ := alerts.GroupEventStormEnded
	summary := "Storm mode ended: per-alert replies resume"
	reason := notifyReasonStorm
	if decision.Action == domain.StormStart {
		typ = alerts.GroupEventStormStarted
		summary = "Storm mode: collapsing this group to one message"
	}
	if err := s.appendGroupEvent(ctx, scope, alerts.GroupEventRequest{
		Type:    typ,
		GroupID: next.ID(),
		Summary: summary,
		Payload: map[string]any{
			"distinct_alerts": decision.DistinctJoins,
			"threshold":       decision.Threshold,
			"window_s":        int64(decision.Window.Seconds()),
			"state_version":   next.StateVersion(),
		},
		DedupeKey:  "group:" + next.ID().String() + ":storm:" + typ + ":" + strconv.Itoa(next.StateVersion()),
		OccurredAt: at,
	}); err != nil {
		return stormOutcome{}, err
	}

	// The channel is TOLD it is going quiet. A damper that does not announce
	// itself is the silent suppression §B.6 forbids.
	if err := s.enqueueNotify(ctx, next, reason); err != nil {
		return stormOutcome{}, err
	}

	return stormOutcome{
		group:   next,
		started: decision.Action == domain.StormStart,
		ended:   decision.Action == domain.StormEnd,
	}, nil
}

// ------------------------------------------------------------------- close

// CloseResult is the audit of one `group.close` tick.
type CloseResult struct {
	Considered int
	Closed     int
	// Held is how many idle generations still had a live member and were left
	// open. Freezing the thread of a live incident is worse than a stale card.
	Held int
}

// CloseIdle is the `group.close` sweep (§G.3): open generations idle past
// group_close_delay_s, with no member still firing or suppressed.
//
// Closing freezes the generation's Slack thread. The next observation for the
// same group key opens generation N+1 and therefore a NEW thread — which is the
// entire reason generations exist (§B.5).
func (s *Service) CloseIdle(ctx context.Context, scope db.TenantScope, limit int) (CloseResult, error) {
	if limit <= 0 {
		limit = 200
	}
	p := s.policy(ctx, scope)
	now := s.Now()

	candidates, err := s.groups.CloseCandidates(ctx, scope, now.Add(-p.CloseDelay), limit)
	if err != nil {
		return CloseResult{}, err
	}
	res := CloseResult{Considered: len(candidates)}

	for _, g := range candidates {
		err := s.tx.InTx(ctx, func(ctx context.Context) error {
			// Re-derive the rollup first: `last_activity_at` says nothing was
			// WRITTEN recently, not that nothing is FIRING.
			fresh, _, err := s.recompute(ctx, scope, g.ID(), now)
			if err != nil {
				return err
			}
			if !fresh.CanCloseAt(now, p.CloseDelay) {
				res.Held++
				return nil
			}
			closed, err := fresh.Close(now)
			if err != nil {
				return err
			}
			if err := s.groups.Close(ctx, scope, closed); err != nil {
				return err
			}
			if err := s.appendGroupEvent(ctx, scope, alerts.GroupEventRequest{
				Type:    alerts.GroupEventClosed,
				GroupID: closed.ID(),
				Summary: "Alert group closed: " + closed.Title(),
				Payload: map[string]any{
					"generation":    closed.Generation(),
					"total":         closed.Counts().Total,
					"state_version": closed.StateVersion(),
				},
				DedupeKey:  "group:" + closed.ID().String() + ":closed",
				OccurredAt: now,
			}); err != nil {
				return err
			}
			if err := s.publish(ctx, scope, closed); err != nil {
				return err
			}
			res.Closed++
			return nil
		})
		if err != nil {
			s.log.WarnContext(ctx, "grouping: could not close group",
				"group_id", g.ID(), "error", err)
		}
	}
	return res, nil
}

// ------------------------------------------------------------------ helpers

func (s *Service) appendGroupEvent(
	ctx context.Context, scope db.TenantScope, in alerts.GroupEventRequest,
) error {
	if s.events == nil {
		return nil
	}
	return s.events.AppendGroupEvent(ctx, scope, in)
}

func (s *Service) publish(ctx context.Context, scope db.TenantScope, g domain.Group) error {
	if s.stream == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"group_id":      g.ID(),
		"state":         g.State().String(),
		"generation":    g.Generation(),
		"firing":        g.Counts().Firing,
		"total":         g.Counts().Total,
		"acked":         g.Counts().Acked,
		"storm_mode":    g.StormMode(),
		"state_version": g.StateVersion(),
	})
	if err != nil {
		return errs.Internal("ui_event_encode_failed", err)
	}
	return s.stream.Append(ctx, scope, StreamGroupUpserted, g.ID(), payload)
}

// notifyReasonStorm is the §H.6 Reason for a storm-mode transition.
const notifyReasonStorm = "storm"

// enqueueNotify queues policy evaluation IN THE CALLER'S TRANSACTION — the
// transactional outbox of ADR 0001.
func (s *Service) enqueueNotify(ctx context.Context, g domain.Group, reason string) error {
	if s.enqueuer == nil {
		return nil
	}
	_, err := s.enqueuer.Enqueue(ctx, jobs.NotifyEvaluateArgs{
		GroupID:      g.ID(),
		Reason:       reason,
		StateVersion: g.StateVersion(),
	})
	if err != nil {
		return errs.Wrap(err, errs.KindInternal, "enqueue_notify_failed",
			"could not queue notification evaluation")
	}
	return nil
}
