package service

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// TxRunner is the unit-of-work port.
type TxRunner interface {
	Tx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Intent is one fact worth communicating, as `notify.evaluate` receives it.
//
// It is an INTENT, not a message. Nothing here is rendered, nothing here names a
// destination, and nothing here has looked at the world yet. That separation is
// why this module can re-evaluate at send time instead of shipping a snapshot of
// a state that has since moved on.
type Intent struct {
	GroupID      uuid.UUID
	Reason       domain.Reason
	StateVersion int
	AlertID      *uuid.UUID
	OccurrenceID *uuid.UUID
	// Actor labels the human or system that caused this, for the rendered card.
	// ACTOR, NEVER SUBJECT.
	Actor string
}

// destination is one live channel and the §H.6 decision for it.
type destination struct {
	channel domain.Channel
	plan    domain.Plan
}

// NotificationService turns an Intent into a recorded Notification and its
// fan-out.
type NotificationService struct {
	txr           TxRunner
	policies      *PolicyService
	notifications NotificationStore
	deliveries    DeliveryStore
	threads       ThreadStore
	snapshots     SnapshotSource
	events        EventSink
	enqueuer      Enqueuer
	clk           clock.Clock
	log           *slog.Logger
}

// NotificationConfig is everything NewNotificationService needs.
type NotificationConfig struct {
	Tx            TxRunner
	Policies      *PolicyService
	Notifications NotificationStore
	Deliveries    DeliveryStore
	Threads       ThreadStore
	Snapshots     SnapshotSource
	Events        EventSink
	Enqueuer      Enqueuer
	Clock         clock.Clock
	Logger        *slog.Logger
}

// NewNotificationService builds the service.
//
// Every collaborator is required. There is deliberately no degraded mode in
// which oto notifies but does not record, or records but does not notify: both
// halves are the product.
func NewNotificationService(cfg NotificationConfig) (*NotificationService, error) {
	switch {
	case cfg.Tx == nil, cfg.Policies == nil, cfg.Notifications == nil,
		cfg.Deliveries == nil, cfg.Threads == nil, cfg.Snapshots == nil,
		cfg.Events == nil, cfg.Enqueuer == nil:
		return nil, errs.New(errs.KindInternal, "notification_service_deps",
			"the notification service needs a tx runner, a policy service, its stores, an event sink and an enqueuer")
	}
	s := &NotificationService{
		txr: cfg.Tx, policies: cfg.Policies, notifications: cfg.Notifications,
		deliveries: cfg.Deliveries, threads: cfg.Threads, snapshots: cfg.Snapshots,
		events: cfg.Events, enqueuer: cfg.Enqueuer,
		clk: cfg.Clock, log: cfg.Logger,
	}
	if s.clk == nil {
		s.clk = clock.New()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Result is what one evaluation did.
type Result struct {
	Notification domain.Notification
	// Created is false when this exact intent had already been recorded. That is
	// the §C.7 idempotency mechanism working, never an error.
	Created bool
	// Deliveries is how many fan-out rows now exist.
	Deliveries int
	// Suppressed carries the recorded reason when nothing was sent.
	Suppressed domain.SuppressedReason
}

// Evaluate is `notify.evaluate`: decide, record, and fan out.
//
// The whole method runs in ONE transaction, and the delivery jobs are enqueued
// inside it. That is oto's transactional outbox: a delivery job can never exist
// for a delivery row that was rolled back, and a delivery row can never sit
// forever with no job to work it.
//
// Order of operations, and why:
//
//  1. read the world ONCE, now — routing and suppression must agree with each
//     other, and two reads could disagree;
//  2. route;
//  3. record the intent, suppressed or not. The row exists either way, because
//     "oto decided not to tell you, and here is why" is a feature and silence is
//     not (§B.6);
//  4. fan out, allocating each thread sequence INSIDE this transaction so that
//     send order is the causal order of domain events (§G.7).
func (s *NotificationService) Evaluate(
	ctx context.Context, scope db.TenantScope, in Intent,
) (Result, error) {
	switch {
	case !in.Reason.Valid():
		return Result{}, errs.Validation("unknown_reason", "unknown notification reason",
			errs.Violation{Field: "reason", Code: "enum", Message: string(in.Reason)})
	case in.Reason.AlertScoped() && in.AlertID == nil:
		// notifications_focus_ck would reject this. Saying so here names the field.
		return Result{}, errs.Validation("alert_scoped_reason_needs_alert",
			"this reason is about one alert and must name it",
			errs.Violation{Field: "alert_id", Code: "required", Message: string(in.Reason)})
	case in.GroupID == uuid.Nil:
		return Result{}, errs.Validation("group_required", "a notification is about a group",
			errs.Violation{Field: "group_id", Code: "required", Message: "a group id is required"})
	}

	var out Result
	err := s.txr.Tx(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.evaluate(ctx, scope, in)
		return err
	})
	return out, err
}

func (s *NotificationService) evaluate(
	ctx context.Context, scope db.TenantScope, in Intent,
) (Result, error) {
	now := s.clk.Now().UTC()

	snap, err := s.snapshots.Snapshot(ctx, scope, domain.SnapshotQuery{
		GroupID:      in.GroupID,
		AlertID:      in.AlertID,
		OccurrenceID: in.OccurrenceID,
	})
	if err != nil {
		return Result{}, err
	}

	n := s.mint(scope, in, snap, now)

	match, err := s.policies.Evaluate(ctx, scope, MatchRequest{
		Reason: in.Reason,
		Labels: snap.Group.GroupLabels,
	})
	if err != nil {
		return Result{}, err
	}
	if match.Routed() {
		id := match.Policy.ID
		n.PolicyID = &id
	}

	sup, err := s.suppressors(ctx, scope, in, snap, match)
	if err != nil {
		return Result{}, err
	}

	// A hard suppressor means no destination will be consulted at all: there is
	// nowhere to send, or a human asked for quiet, or the cap is reached. The
	// per-destination dampers below cannot even be evaluated without a
	// destination.
	if reason, blocked := sup.Winner(); blocked {
		return s.record(ctx, scope, n, reason, sup.All(), now)
	}

	dests, softSup := s.plan(in.Reason, snap, match)
	if len(dests) == 0 {
		reason, ok := softSup.Winner()
		if !ok {
			// Every destination declined and none said why. Verbosity is the honest
			// catch-all: something in the volume settings dropped it.
			reason = domain.SuppressedVerbosity
		}
		return s.record(ctx, scope, n, reason, softSup.All(), now)
	}

	stored, created, err := s.notifications.Insert(ctx, scope, n)
	if err != nil {
		return Result{}, err
	}
	if !created && stored.Status == domain.StatusSuppressed {
		// A previous run already decided, with a reason, that this exact fact was
		// not to be sent. Re-deciding now would be a second opinion on a settled
		// question, and the two could disagree.
		return Result{Notification: stored, Suppressed: stored.SuppressedReason}, nil
	}

	// The fan-out runs even when the notification already existed. `Create` is
	// ON CONFLICT DO NOTHING so it converges, and a crash between the intent
	// insert and the fan-out would otherwise leave the fact recorded and nobody
	// told — the worst of both worlds.
	count, err := s.fanOut(ctx, scope, stored, dests, now)
	if err != nil {
		return Result{}, err
	}

	if err := s.notifications.SetStatus(ctx, scope, stored.ID, domain.StatusDispatched, now); err != nil {
		return Result{}, err
	}
	stored.Status = domain.StatusDispatched

	if created {
		if err := s.events.AppendNotificationCreated(ctx, scope, stored, count, now); err != nil {
			return Result{}, err
		}
	}
	return Result{Notification: stored, Created: created, Deliveries: count}, nil
}

// mint builds the intent row, including its §C.7 idempotency key.
func (s *NotificationService) mint(
	scope db.TenantScope, in Intent, snap domain.Snapshot, now time.Time,
) domain.Notification {
	stateVersion := in.StateVersion
	if stateVersion <= 0 {
		// The payload did not pin a version. Fall back to the group's current one:
		// a key derived from 0 would be identical for every evaluation of this
		// group forever, so the second fact about it would be swallowed as a
		// duplicate of the first.
		stateVersion = snap.Group.StateVersion
	}
	if stateVersion <= 0 {
		stateVersion = 1
	}

	n := domain.Notification{
		ID:           uuid.New(),
		OrgID:        scope.OrgID(),
		SubjectKind:  domain.SubjectAlertGroup,
		SubjectID:    in.GroupID,
		GroupID:      in.GroupID,
		AlertID:      in.AlertID,
		OccurrenceID: in.OccurrenceID,
		Reason:       in.Reason,
		StateVersion: stateVersion,
		Status:       domain.StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	n.IdempotencyKey = domain.IdempotencyKey(
		scope.OrgID(), n.SubjectKind, n.SubjectID, n.Reason, n.StateVersion)
	return n
}

// suppressors evaluates the suppressors that do not depend on a destination.
//
// They are gathered as a SET and resolved by §B.8.2 precedence at the end, never
// as a ladder of early returns. The order the evaluator happens to learn things
// in is the order its data arrives; the order that gets RECORDED has to be the
// one the spec fixes, or the reason an operator reads is an accident of query
// scheduling.
func (s *NotificationService) suppressors(
	ctx context.Context, scope db.TenantScope, in Intent,
	snap domain.Snapshot, match Match,
) (domain.Suppressors, error) {
	var sup domain.Suppressors

	sup.AddIf(!match.Routed(), domain.SuppressedNoPolicy)
	sup.AddIf(match.Routed() && !match.Deliverable(), domain.SuppressedChannelDisabled)

	// Snooze (§B.8.4) suppresses EVERY reason for the snoozed signal, including
	// rule_changed, which is otherwise never gated — a partial mute is a
	// confusing mute. The two exemptions are the snooze announcing its own
	// beginning and end, without which it would be exactly the silent
	// suppression §B.6 forbids.
	if !in.Reason.SnoozeExempt() {
		var quiet bool
		if snap.Focus != nil {
			// Snooze is scoped to an alert_key, so a fact about ONE alert is decided
			// by that alert alone.
			quiet = snap.FocusSnoozed(snap.TakenAt)
		} else {
			// A group-wide fact is quiet only when EVERY member is. One awake member
			// means somebody is still waiting to hear about it.
			quiet = snap.AllMembersSnoozed()
		}
		sup.AddIf(quiet, domain.SuppressedSnoozed)
	}

	if match.Routed() && match.Policy.Throttle.Enabled() {
		t := match.Policy.Throttle
		count, err := s.notifications.CountRecent(ctx, scope,
			domain.SubjectAlertGroup, in.GroupID, snap.TakenAt.Add(-t.Window))
		if err != nil {
			return sup, err
		}
		sup.AddIf(count >= t.Max, domain.SuppressedThrottled)
	}

	return sup, nil
}

// plan applies the §H.6 table to every live destination and collects the
// per-destination suppressors as it goes.
func (s *NotificationService) plan(
	reason domain.Reason, snap domain.Snapshot, match Match,
) ([]destination, domain.Suppressors) {
	var sup domain.Suppressors
	if !match.Routed() {
		return nil, sup
	}

	flapping := snap.AnyFlapping()
	out := make([]destination, 0, len(match.Live))

	for _, c := range match.Live {
		p := domain.PlanFor(domain.PlanInput{
			Reason:        reason,
			Verbosity:     c.EffectiveVerbosity(),
			ThreadUpdates: c.ThreadUpdates,
			Capabilities:  c.Capabilities,
			// ThreadExists is deliberately false here. The thread row is created
			// inside the fan-out transaction below, and the mode is RE-DERIVED at
			// claim time against the thread as it actually stands — which is the
			// same reason the payload is rendered at claim time and not now.
			StormMode: snap.Group.StormMode,
			Flapping:  flapping,
		})

		if p.ReplyDropped {
			switch p.ReplyDropReason {
			case "storm":
				sup.Add(domain.SuppressedStorm)
			case "flapping":
				sup.Add(domain.SuppressedFlapping)
			case "verbosity", "thread_updates", "no_threading", "fresh_root":
				sup.Add(domain.SuppressedVerbosity)
			}
		}
		if p.Empty() {
			continue
		}
		out = append(out, destination{channel: c, plan: p})
	}

	// Deterministic order. It does not affect correctness — ordering is a
	// property of each THREAD, not of the fan-out — but it makes two runs of the
	// same evaluation comparable, which is worth one sort.
	sort.Slice(out, func(i, j int) bool {
		return out[i].channel.ID.String() < out[j].channel.ID.String()
	})
	return out, sup
}

// record persists a suppressed intent.
//
// This is NOT a no-op path. The row, its reason and its timeline event ARE the
// feature: an alerting tool that quietly decides not to tell you something, and
// leaves no trace that it decided anything, is a tool nobody can trust at 3am.
func (s *NotificationService) record(
	ctx context.Context, scope db.TenantScope, n domain.Notification,
	reason domain.SuppressedReason, also []domain.SuppressedReason, now time.Time,
) (Result, error) {
	n.Status = domain.StatusSuppressed
	n.SuppressedReason = reason

	stored, created, err := s.notifications.Insert(ctx, scope, n)
	if err != nil {
		return Result{}, err
	}
	if created {
		if err := s.events.AppendNotificationSuppressed(ctx, scope, stored, also, now); err != nil {
			return Result{}, err
		}
	}
	return Result{Notification: stored, Created: created, Suppressed: stored.SuppressedReason}, nil
}

// fanOut creates one delivery per destination and enqueues its dispatch job.
func (s *NotificationService) fanOut(
	ctx context.Context, scope db.TenantScope, n domain.Notification,
	dests []destination, now time.Time,
) (int, error) {
	created := 0

	for _, d := range dests {
		mode := d.plan.PrimaryMode()
		if mode == "" {
			continue
		}

		var (
			threadID *uuid.UUID
			seq      int
		)
		if needsThread(d.channel) {
			th, err := s.threads.Ensure(ctx, scope, d.channel.ID, n.SubjectKind, n.SubjectID, now)
			if err != nil {
				return created, err
			}
			// THE SEQUENCE IS TAKEN HERE, inside the transaction that creates the
			// delivery. That is the whole ordering design (§G.7): the number is
			// allocated in the same transaction as the domain event that justified
			// the message, so send order is causal order rather than the order two
			// worker pods happened to wake up in.
			seq, err = s.threads.AllocateSeq(ctx, scope, th.ID, now)
			if err != nil {
				return created, err
			}
			id := th.ID
			threadID = &id
		}

		row, madeNew, err := s.deliveries.Create(ctx, scope, repository.NewDelivery{
			ID:             uuid.New(),
			NotificationID: n.ID,
			ChannelID:      d.channel.ID,
			ThreadID:       threadID,
			ThreadSeq:      seq,
			Mode:           mode,
			CreatedAt:      now,
		})
		if err != nil {
			return created, err
		}
		created++
		if !madeNew {
			// Already fanned out by an earlier run, and its job was enqueued in the
			// same transaction as the row. Enqueuing a second one would be harmless
			// but pointless: the claim would find nothing to take.
			continue
		}

		// Enqueued INSIDE this transaction: the job and the row it names commit
		// together or not at all (ADR 0001).
		if _, err := s.enqueuer.Enqueue(ctx, jobs.DeliverDispatchArgs{
			DeliveryID:  row.ID,
			ChannelType: string(d.channel.Type),
		}); err != nil {
			return created, err
		}
	}
	return created, nil
}

// needsThread reports whether this destination has a conversation oto must
// remember.
//
// A destination that can neither thread nor amend — the generic webhook — has no
// thread at all: every message it receives is a standalone post, there is no
// handle worth storing, and `deliveries_thread_ck` permits a NULL thread exactly
// for the `post_root` it will always be given.
func needsThread(c domain.Channel) bool {
	return c.Capabilities.Has(domain.CapThreading) || c.Capabilities.Has(domain.CapAmend)
}
