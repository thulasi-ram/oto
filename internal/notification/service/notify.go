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
	// input is the §H.6 question, kept so that fanOut can ask it again once it
	// knows whether a root card actually exists. Everything in it except
	// ThreadExists is settled before any thread is touched.
	input domain.PlanInput
}

// OrgDefaults is the org-level tuning the §H.6 table consults, as distinct from
// the per-channel switches on `channels`.
//
// It exists because two of §H.6's inputs are now org-configurable: which
// transitions surface in the channel (ADR 0020) and what verbosity a Channel that
// names none falls back to. Both are read ONCE per evaluation, so every
// destination in one fan-out is judged against the same policy — a fan-out where
// two channels disagreed about whether a storm warrants a broadcast would be very
// hard to explain to the person reading the channel.
type OrgDefaults struct {
	Broadcast domain.BroadcastPolicy
	// Verbosity is the fallback for a Channel with no verbosity of its own. Empty
	// means the schema default.
	Verbosity domain.Verbosity
	// UnackedReminderAfter is the org's fallback delay for a policy that names
	// none. Zero means the org sets none, which is what shipped.
	//
	// ⛔ ONE STAGE, FOREVER (§G.9.1). A scalar, and a FALLBACK — never a second
	// threshold and never a ladder.
	UnackedReminderAfter time.Duration
	// StormCooldown is the org's `storm_cooldown_s`, reused as the window of the
	// once-per-channel storm-notice latch (ADR 0020). It is the right window
	// because it is already the minimum distance between a storm starting and the
	// same storm ending, so a storm's start and its own end each get through while
	// every other group's storm inside it collapses into the first. Zero means
	// "unusable", and the domain substitutes its own floor rather than removing the
	// latch — no latch is the per-group flood.
	StormCooldown time.Duration
	// ReminderMention is who the ONE unacked reminder addresses (ADR 0020). The
	// zero value is `none`, which is the shipped default and a deliberate one:
	// Slack does not notify on @here/@channel from inside a thread, and the
	// reminder is a thread reply.
	//
	// ⛔ NOT A ROTA (§4.8). A fixed audience and a severity floor, nothing else.
	ReminderMention domain.MentionPolicy
}

// SettingsReader reads one org's notification-level tuning from `orgs.settings`.
// It is OPTIONAL: a nil reader, or one that fails, yields the shipped defaults,
// because a settings lookup must never be able to stop a notification.
type SettingsReader interface {
	NotificationDefaults(ctx context.Context, s db.TenantScope) (OrgDefaults, error)
}

// planInput is the §H.6 question for one destination.
func planInput(
	reason domain.Reason, c domain.Channel, def OrgDefaults, rootExists, storm, flapping bool,
) domain.PlanInput {
	// The channel's own setting wins; the org default is only consulted when the
	// channel names nothing. Verbosity is a per-destination volume dial, and an
	// org default that could override one would make a quiet channel loud from a
	// screen nobody was looking at.
	verbosity := c.Verbosity
	if !verbosity.Valid() {
		verbosity = def.Verbosity
	}
	return domain.PlanInput{
		Reason:        reason,
		Verbosity:     verbosity.Normalise(),
		ThreadUpdates: c.ThreadUpdates,
		Capabilities:  c.Capabilities,
		ThreadExists:  rootExists,
		StormMode:     storm,
		Flapping:      flapping,
		Broadcast:     def.Broadcast,
	}
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
	channels      ChannelStore
	settings      SettingsReader
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
	// Channels is the destination store. This service reads no channel row of its
	// own — routing resolves them — but it TAKES the once-per-channel storm-notice
	// latch here, inside the evaluation transaction, so a rolled-back evaluation
	// cannot consume a channel's one storm notice (ADR 0020).
	Channels ChannelStore
	// Settings reads the org's notification tuning. OPTIONAL: nil means every org
	// runs oto's shipped defaults, which is the correct degraded answer — a
	// settings lookup must never be able to stop a notification.
	Settings SettingsReader
	Clock    clock.Clock
	Logger   *slog.Logger
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
		cfg.Events == nil, cfg.Enqueuer == nil, cfg.Channels == nil:
		return nil, errs.New(errs.KindInternal, "notification_service_deps",
			"the notification service needs a tx runner, a policy service, its stores, a channel store, an event sink and an enqueuer")
	}
	s := &NotificationService{
		txr: cfg.Tx, policies: cfg.Policies, notifications: cfg.Notifications,
		deliveries: cfg.Deliveries, threads: cfg.Threads, snapshots: cfg.Snapshots,
		events: cfg.Events, enqueuer: cfg.Enqueuer, channels: cfg.Channels,
		settings: cfg.Settings, clk: cfg.Clock, log: cfg.Logger,
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
	// Deliveries is how many fan-out rows THIS evaluation created. A re-run of an
	// already-fanned-out intent reports 0, because it made nothing; counting the
	// pre-existing rows would put an invented number on the timeline.
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

	// The org's tuning is read ONCE per evaluation, straight from `orgs.settings`
	// — no cache sits in front of it, so a settings change takes effect on the
	// very next evaluation in every pod, with no restart.
	def := s.orgDefaults(ctx, scope)

	dests, softSup := s.plan(ctx, scope, in.Reason, snap, match, def)
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

// orgDefaults reads the org's notification tuning, degrading to oto's shipped
// defaults on any failure.
//
// ⛔ IT NEVER RETURNS AN ERROR, and that is the rule this whole path is built
// around: a settings lookup MUST NOT be able to stop an alert being announced. A
// tenant whose settings row is unreadable gets the defaults and a warning in the
// log, not silence in their Slack channel.
func (s *NotificationService) orgDefaults(ctx context.Context, scope db.TenantScope) OrgDefaults {
	def := OrgDefaults{
		Broadcast: domain.DefaultBroadcastPolicy(),
		Verbosity: domain.VerbosityStatusChanges,
	}
	if s.settings == nil {
		return def
	}
	got, err := s.settings.NotificationDefaults(ctx, scope)
	if err != nil {
		s.log.WarnContext(ctx, "notification: could not read org tuning, using defaults",
			"org_id", scope.OrgID(), "error", err.Error())
		return def
	}
	if got.Verbosity.Valid() {
		def.Verbosity = got.Verbosity
	}
	def.Broadcast = got.Broadcast
	def.StormCooldown = got.StormCooldown
	def.ReminderMention = got.ReminderMention
	return def
}

// plan answers ONE question per destination: does this fact reach it at all, and
// if a reply was dropped, why?
//
// It deliberately does NOT decide the concrete rows. It cannot: §H.6's root
// column turns on whether a root card already exists, and that is thread state
// this method has no access to. `fanOut` re-applies the table against the thread
// it just ensured — see modesFor — and that is where `Plan.Modes` becomes rows.
func (s *NotificationService) plan(
	ctx context.Context, scope db.TenantScope, reason domain.Reason,
	snap domain.Snapshot, match Match, def OrgDefaults,
) ([]destination, domain.Suppressors) {
	var sup domain.Suppressors
	if !match.Routed() {
		return nil, sup
	}

	flapping := snap.AnyFlapping()
	out := make([]destination, 0, len(match.Live))

	for _, c := range match.Live {
		// rootExists is false HERE, and only here: this pass answers "does this
		// destination get anything", which the first-notification view answers
		// conservatively and identically for every destination. The row-level
		// decision is taken in fanOut against the real thread.
		in := planInput(reason, c, def, false, snap.Group.StormMode, flapping)

		// ⭐ THE ONCE-PER-CHANNEL STORM NOTICE (ADR 0020). "oto has started
		// withholding individual notifications" is a fact about the CHANNEL, and
		// storm mode is decided per GROUP: without this latch, twenty generations
		// collapsing inside one minute would post twenty "going quiet" messages
		// into one channel — the flood the damper exists to prevent, produced by
		// the damper's own announcement.
		//
		// The claim runs INSIDE the evaluation transaction, so an evaluation that
		// rolls back cannot consume a channel's one notice, and it runs per
		// DESTINATION, because two channels are two audiences and each is entitled
		// to be told once.
		if domain.WarrantsChannelNotice(reason) {
			in.ChannelNoticeClaimed = s.claimStormNotice(ctx, scope, c.ID, def, snap.TakenAt)
		}

		p := domain.PlanFor(in)

		if p.BroadcastDamped {
			// ⭐ A DAMPED BROADCAST IS RECORDED, NEVER SILENT. ADR 0020 permits
			// exactly one broadcast during a storm — the storm announcement — and
			// this is the line where the other ninety-nine were turned down. The
			// fact still lands on the thread, so this is not a suppression of the
			// notification; it is oto accounting for its own quiet, which is what
			// §B.6 requires of every damper it runs.
			s.log.DebugContext(ctx, "notification: broadcast damped",
				"reason", string(reason), "channel_id", c.ID, "damped_by", p.BroadcastDampReason)
		}

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
		out = append(out, destination{channel: c, plan: p, input: in})
	}

	// Deterministic order. It does not affect correctness — ordering is a
	// property of each THREAD, not of the fan-out — but it makes two runs of the
	// same evaluation comparable, which is worth one sort.
	sort.Slice(out, func(i, j int) bool {
		return out[i].channel.ID.String() < out[j].channel.ID.String()
	})
	return out, sup
}

// claimStormNotice takes the channel-level storm-notice latch, or reports that
// this channel has already been told.
//
// ⛔ A FAILED CLAIM IS NOT A FAILED NOTIFICATION. The latch is bookkeeping for
// oto's own volume; a database hiccup while taking it must not stop the storm
// reply reaching the group's thread. So an error degrades to "not claimed" — the
// QUIET direction, which is the safe one here: the alternative would be a channel
// receiving one broadcast per storming group because a latch read failed.
func (s *NotificationService) claimStormNotice(
	ctx context.Context, scope db.TenantScope, channelID uuid.UUID,
	def OrgDefaults, now time.Time,
) bool {
	window := domain.NormaliseStormNoticeWindow(def.StormCooldown)
	claimed, err := s.channels.ClaimStormNotice(ctx, scope, channelID, now, now.Add(-window))
	if err != nil {
		s.log.WarnContext(ctx, "notification: could not take the storm-notice latch",
			"channel_id", channelID, "error", err.Error())
		return false
	}
	return claimed
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

// fanOut creates ONE DELIVERY PER MODE per destination and enqueues its dispatch
// job.
//
// §H.6 asks several Reasons for `update_root` AND `thread_reply` on the same
// destination, and `deliveries_fanout_uniq` is keyed by
// (notification_id, channel_id, mode) precisely so both can exist. Each gets its
// own row, its own sequence, its own claim and its own retry budget — so a root
// amend that fails is retried and dead-lettered like anything else, and a root
// amend that SUCCEEDS becomes a `sent` delivery that `LastRootHash` can see.
// Riding the amend along inside the reply's claim, which is what the single-row
// constraint used to force, could do neither: a failure vanished into a log line
// and a success left the cached hash describing a card that had since changed.
//
// The root mode is allocated FIRST, so the gate sends the amend before the reply
// and a reader drawn to the thread by the reply finds a card that agrees with it.
//
// ORDER OF THE TWO WRITES IS ALSO LOAD-BEARING: the row is INSERTED FIRST and the
// thread sequence is allocated only for a row this transaction actually created.
// `Create` is `ON CONFLICT DO NOTHING` because `Evaluate` re-runs — it is a job
// on an at-least-once queue and the fan-out deliberately runs again even when the
// notification already existed. Allocating in front of that insert committed a
// `next_seq++` with nothing behind it on every re-run, and every delivery queued
// behind the empty slot then waited the full ordering MaxWait — fifteen minutes of
// silence on a Slack thread — before gap recovery stepped past it.
func (s *NotificationService) fanOut(
	ctx context.Context, scope db.TenantScope, n domain.Notification,
	dests []destination, now time.Time,
) (int, error) {
	created := 0

	for _, d := range dests {
		var (
			threadID   *uuid.UUID
			rootLanded bool
		)
		if needsThread(d.channel) {
			th, err := s.threads.Ensure(ctx, scope, d.channel.ID, n.SubjectKind, n.SubjectID, now)
			if err != nil {
				return created, err
			}
			id := th.ID
			threadID, rootLanded = &id, th.RootLanded()
		}

		for _, mode := range s.modesFor(d, rootLanded) {
			made, err := s.createDelivery(ctx, scope, n, d, threadID, mode, now)
			if err != nil {
				return created, err
			}
			if made {
				created++
			}
		}
	}
	return created, nil
}

// modesFor re-applies the §H.6 table now that the thread is known.
//
// ⚠ THE ROOT COLUMN OF THAT TABLE TURNS ON WHETHER A ROOT CARD EXISTS, and
// nothing before this point can answer that. Asking with `ThreadExists: false`
// collapses every root-touching Reason to a single `post_root` and drops its
// reply as `fresh_root` — which is right for the FIRST notification and wrong for
// every one after it. The mode is still RE-DERIVED again at claim time, because a
// root can land or be lost in between; what cannot be deferred is HOW MANY ROWS
// the fact needs, and that is decided here.
func (s *NotificationService) modesFor(d destination, rootLanded bool) []domain.Mode {
	in := d.input
	in.ThreadExists = rootLanded
	if p := domain.PlanFor(in); !p.Empty() {
		return p.Modes
	}
	// The first pass said this destination gets something; the second must not
	// silently disagree. Fall back to what was already decided.
	if mode := d.plan.PrimaryMode(); mode != "" {
		return []domain.Mode{mode}
	}
	return nil
}

// createDelivery writes one (notification, channel, mode) row, takes its sequence
// and enqueues its job. It reports whether the row is new.
func (s *NotificationService) createDelivery(
	ctx context.Context, scope db.TenantScope, n domain.Notification,
	d destination, threadID *uuid.UUID, mode domain.Mode, now time.Time,
) (bool, error) {
	row, madeNew, err := s.deliveries.Create(ctx, scope, repository.NewDelivery{
		ID:             uuid.New(),
		NotificationID: n.ID,
		ChannelID:      d.channel.ID,
		ThreadID:       threadID,
		Mode:           mode,
		CreatedAt:      now,
	})
	if err != nil {
		return false, err
	}
	if !madeNew {
		// Already fanned out by an earlier run, with its sequence taken and its job
		// enqueued in the same transaction as the row. Nothing to allocate and
		// nothing to enqueue: a second job would find nothing to claim.
		return false, nil
	}

	if threadID != nil {
		// THE SEQUENCE IS TAKEN HERE, inside the transaction that creates the
		// delivery. That is the whole ordering design (§G.7): the number is
		// allocated in the same transaction as the domain event that justified the
		// message, so send order is causal order rather than the order two worker
		// pods happened to wake up in.
		seq, err := s.threads.AllocateSeq(ctx, scope, *threadID, now)
		if err != nil {
			return false, err
		}
		if err := s.deliveries.SetThreadSeq(ctx, scope, row.ID, seq, now); err != nil {
			return false, err
		}
	}

	// Enqueued INSIDE this transaction: the job and the row it names commit
	// together or not at all (ADR 0001).
	if _, err := s.enqueuer.Enqueue(ctx, jobs.DeliverDispatchArgs{
		DeliveryID:  row.ID,
		ChannelType: string(d.channel.Type),
	}); err != nil {
		return false, err
	}
	return true, nil
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
