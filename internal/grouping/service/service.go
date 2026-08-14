package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
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
	// Synthetic marks a generation opened by a DELIVERY DRILL. It is carried from
	// the observations that triggered the resolve — which carry it from
	// `ingest_batches.mode` — and it is written to `alert_groups.synthetic` so
	// the dashboard counts can exclude drills.
	//
	// ⛔ It changes NOTHING about how the group behaves. A drill that took a
	// different code path anywhere below this line would stop being evidence.
	Synthetic bool
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
	// The §C.4 identity function lives in the shared domain kernel and is called
	// directly: depguard RULE K sanctions the import, and re-exporting a pure
	// identity function through a service would give it a fake dependency on a
	// database.
	labels, err := kernel.NewLabels(in.GroupLabels)
	if err != nil {
		return domain.Group{}, err
	}
	if scope.OrgID() == uuid.Nil || in.SourceID == uuid.Nil {
		return domain.Group{}, errs.Validation("group_key_inputs_required",
			"a group key needs both an org and a source")
	}
	key := kernel.ComputeGroupKey(scope.OrgID(), in.SourceID, in.Receiver, labels).String()
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
			Synthetic:      in.Synthetic,
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

		if err := s.appendGroupEvent(ctx, scope, alerts.TimelineEventRequest{
			Type:    kernel.EventGroupOpened,
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

// ⛔ THERE IS NO SINGLE-MEMBER `Join`. JoinMany is the only shape.
//
// One existed as a `JoinMany` of one, kept so a caller holding a single
// occurrence would not have to build a slice. It had no production caller —
// `alertObserver.joinMembers` batches — so its only consumer was its own test,
// which is the exact trap `tools/lintreach` exists to find and which SPEC §C.6/§C.7
// had already sprung once: a function that compiles, lints, is exported and is
// wired to nothing reads as canonical to the next person who edits it.
//
// If a single-member caller ever appears, it can write `JoinMany(..., []JoinMember{m}, ...)`
// at the call site. That is one line, and it does not create a second answer to
// "what does joining do".

// JoinMember names one occurrence to add to a generation.
//
// It is a service-layer type on purpose: the composition root hands these in, and
// a composition root that had to build a `grouping/domain` value would be reaching
// into another module's domain (CONTEXT.md §5.4).
type JoinMember struct {
	AlertID      uuid.UUID
	OccurrenceID uuid.UUID
}

// JoinManyResult is what ONE JoinMany did to ONE generation.
type JoinManyResult struct {
	Group domain.Group
	// Joined is how many of the requested members were NEWLY joined. The rest were
	// already members, which is a redelivered batch working as designed rather than
	// an error.
	Joined int

	// ⛔ THERE ARE NO StormStarted/StormEnded FIELDS. A damping transition is a
	// VISIBLE state change, and `evaluateStorm` already does everything it implies
	// inside the transaction: it flips the mode, appends `group.storm_started` /
	// `group.storm_ended` to the timeline (§B.6) and enqueues the one
	// `notify.evaluate` that the §H.6 latch on `channels.storm_notice_at` needs.
	// Reporting it again to a caller that must not act on it a second time is how a
	// storm gets announced twice. Group.StormMode() is the state; the timeline is
	// the record.
}

// JoinMany adds EVERY member of one batch to one generation and then re-derives
// the generation ONCE.
//
// ⭐ THE ROLLUP IS RECOMPUTED ONCE PER GROUP PER BATCH, NOT ONCE PER MEMBER. A
// rollup is a PURE PROJECTION of the current members (see `recompute`), so joining
// 500 occurrences and rolling up 500 times produces 499 results nobody reads —
// each one a full aggregate over the group plus a compare-and-set write to the
// same `alert_groups` row, which the CAS then serialises. That is O(n) contention
// on one row, arriving exactly when a 500-alert Alertmanager batch is landing and
// Alertmanager's ~5-minute retry budget (ADR 0007, `ingestion/api/shed.go`) is the
// only thing between a slow ingest and an alert that is lost silently. Joining
// first and projecting once makes the same batch O(1) in rollups.
//
// ⭐ The storm evaluation moves with it, and MUST. It counts DISTINCT JOINS INSIDE
// A WINDOW by querying `alert_group_members` (§B.6) — it has never counted its own
// invocations — so one evaluation over the settled membership sees every member
// this batch added and reaches the same verdict the last of 500 per-member
// evaluations would have reached. What it does NOT do is announce that verdict
// once per member: one transition, one `group.storm_started`, one `notify.evaluate`
// job, and therefore still exactly one storm notice per channel behind the
// `channels.storm_notice_at` latch.
//
// Members are joined in the order given, so the `group.member_joined` events land
// on the timeline in batch order.
func (s *Service) JoinMany(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, members []JoinMember, at time.Time,
) (JoinManyResult, error) {
	if at.IsZero() {
		at = s.Now()
	}
	// One policy read for the batch: the tuning cannot change inside it, and
	// re-reading `orgs.settings` per member was the same 500× waste in miniature.
	policy := s.policy(ctx, scope)

	var out JoinManyResult
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		out = JoinManyResult{}
		for _, m := range members {
			joined, err := s.members.Join(ctx, scope, groupID, m.OccurrenceID, m.AlertID, at)
			if err != nil {
				return err
			}
			if !joined {
				continue
			}
			out.Joined++
			if err := s.appendGroupEvent(ctx, scope, alerts.TimelineEventRequest{
				Type:         kernel.EventGroupMemberJoined,
				GroupID:      groupID,
				AlertID:      m.AlertID,
				OccurrenceID: m.OccurrenceID,
				Summary:      "Alert joined the group",
				DedupeKey:    "group:" + groupID.String() + ":joined:" + m.OccurrenceID.String(),
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

		if material || stormed.started || stormed.ended {
			if err := s.publish(ctx, scope, out.Group); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return JoinManyResult{}, err
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
			if err := s.appendGroupEvent(ctx, scope, alerts.TimelineEventRequest{
				Type:         kernel.EventGroupMemberLeft,
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
// It RE-READS AND RECOMPUTES when it loses the `state_version` compare-and-set.
// Read-recompute-write at READ COMMITTED is exactly the shape the alerts reaper
// got wrong: without the version predicate two concurrent recomputes both derive
// version N+1 from N, both write it, and the loser's counts vanish while §C.7's
// idempotency key claims both states are the same fact. Retrying is always
// correct here because a rollup is a PURE PROJECTION of the current members —
// recomputing it from a fresh read discards nothing.
func (s *Service) recompute(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, at time.Time,
) (domain.Group, bool, error) {
	for attempt := 1; ; attempt++ {
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
		err = s.groups.SetRollup(ctx, scope, next, g.StateVersion())
		if err == nil {
			return next, true, nil
		}
		if !errs.IsKind(err, errs.KindConflict) || attempt >= groupMaxAttempts {
			return domain.Group{}, false, err
		}
		s.log.InfoContext(ctx, "grouping: rollup lost the state_version compare-and-set, recomputing",
			"group_id", groupID, "attempt", attempt)
	}
}

// groupMaxAttempts bounds every optimistic-lock retry in this file. A generation
// that loses three in a row is contending with a writer that is winning every
// time; the conflict is returned and the caller's own retry budget takes over
// rather than spinning a worker.
const groupMaxAttempts = 3

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
	// The storm verdict was derived from `g`, so `g`'s version is what it is
	// entitled to overwrite. A lost compare-and-set means the membership moved
	// while the window was being counted; the caller's next Join re-evaluates
	// against the newer generation rather than announcing a damping transition
	// for counts that no longer exist.
	if err := s.groups.SetStorm(ctx, scope, next, g.StateVersion()); err != nil {
		return stormOutcome{}, err
	}

	typ := kernel.EventGroupStormEnded
	summary := "Storm mode ended: per-alert replies resume"
	reason := notifyReasonStorm
	if decision.Action == domain.StormStart {
		typ = kernel.EventGroupStormStarted
		summary = "Storm mode: collapsing this group to one message"
	}
	if err := s.appendGroupEvent(ctx, scope, alerts.TimelineEventRequest{
		Type:    typ,
		GroupID: next.ID(),
		Summary: summary,
		Payload: map[string]any{
			"distinct_alerts": decision.DistinctJoins,
			"threshold":       decision.Threshold,
			"window_s":        int64(decision.Window.Seconds()),
			"state_version":   next.StateVersion(),
		},
		// `.String()` because the §C.8 key is a `TEXT` column, and the type is now
		// a value object rather than the raw string it used to concatenate. The key
		// bytes are unchanged, which matters: a different key would unclaim every
		// storm transition already recorded.
		DedupeKey:  "group:" + next.ID().String() + ":storm:" + typ.String() + ":" + strconv.Itoa(next.StateVersion()),
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
			// `fresh` is what proved no member is still live, so `fresh`'s version
			// is what the close is entitled to overwrite. A member that joined in
			// between bumps it, the close is refused, and the live incident keeps
			// its thread.
			if err := s.groups.Close(ctx, scope, closed, fresh.StateVersion()); err != nil {
				return err
			}
			if err := s.appendGroupEvent(ctx, scope, alerts.TimelineEventRequest{
				Type:    kernel.EventGroupClosed,
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
	ctx context.Context, scope db.TenantScope, in alerts.TimelineEventRequest,
) error {
	if s.events == nil {
		return nil
	}
	return s.events.AppendTimelineEvent(ctx, scope, in)
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
