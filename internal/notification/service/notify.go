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
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
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
	CaseID       *uuid.UUID
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
// two channels disagreed about whether a resolve warrants a broadcast would be very
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
	// ⛔ `StormCooldown` WAS HERE AND IS DELETED WITH THE LATCH IT WINDOWED. It
	// carried the org's `storm_cooldown_s` into the once-per-channel storm-notice
	// latch, so that a storm's start and its own end each got through while every
	// other group's storm inside the window collapsed into the first. There is no
	// latch and no notice: nothing evaluates a storm. `storm_cooldown_s` is no longer an
	// org setting either: all three `storm_*` keys are deleted from
	// `identity/domain.settingBounds`, which is where a settings key is declared —
	// `orgs.settings` is one JSONB document, so no migration drops a key from it, and
	// `NewDeclarative` refuses a config still naming one with `unknown_key`. Migration
	// 00059 dropped the column the latch itself lived in, `channels.storm_notice_at`.
	//
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
	reason domain.Reason, c domain.Channel, def OrgDefaults, rootExists, flapping bool,
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
	// Channels is the destination store.
	//
	// ⛔ IT IS READ-ONLY NOW, AND IT DID NOT USED TO BE. `Evaluate` resolves its
	// destinations through routing and touches no channel row; the ONE write this
	// service ever made to `channels` was taking the once-per-channel storm-notice
	// latch inside the evaluation transaction, and that latch is deleted with storm
	// damping. What still needs the store is the DIGEST path, which lists a policy's
	// channels directly because a digest is not routed by a group's labels.
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
	// ⛔ A `case in.Reason.Retired()` ARM STOOD HERE AND IS DELETED WITH THE ONE
	// VALUE IT GUARDED. `storm` was the only retired Reason; it is now deleted from
	// the vocabulary outright (reason.go) and migration 00060 narrows
	// `notifications_reason_ck` to refuse it, so `Valid()` below is the whole
	// answer — a retired-but-valid value no longer exists to be told apart from an
	// unknown one. `alerts/service.refuseRetired` stays, because `alert_events.type`
	// still has retirements (`group.member_*`, `case.reopened`) that this
	// vocabulary no longer has.
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
	err := s.txr.InTx(ctx, func(ctx context.Context) error {
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
		GroupID: in.GroupID,
		AlertID: in.AlertID,
		CaseID:  in.CaseID,
	})
	if err != nil {
		return Result{}, err
	}

	// §H.6, and the ONE place the wire table is applied. The intent arrived
	// carrying a Reason derived from ONE alert's transition; this is the first
	// moment the whole group is in scope, so it is the only moment at which
	// "one alert resolved" can become "all alerts resolved". Everything
	// downstream — the mode plan, the verbosity gate, the broadcast decision,
	// the footer phrase and the idempotency key — reads the reconciled value.
	in.Reason = domain.ReconcileWithWire(
		in.Reason, snap.Group.NotificationReason, snap.Group.AllResolved())

	n := s.mint(scope, in, snap, now)

	// ⭐ THE MATCHER SEES THE GROUP'S LABELS PLUS THE FOCUSED ALERT'S OWN, and the
	// second half is git-bug 7570090's declared prerequisite rather than a
	// convenience.
	//
	// The group's labels are the only set true of EVERY member, which is why they
	// were the whole input: since ADR 0038 they are oto's own axes — `alertname`,
	// and `namespace` when the alert has one — rather than whatever the operator
	// put in `group_by`. Before that, a policy matching `namespace` matched nothing
	// on most deployments and failed as a `no_policy` suppression rather than as an
	// error, which is a filter that silently deletes notifications.
	//
	// ⛔ BUT TWO AXES IS ALSO EVERY LABEL A POLICY CAN SEE, AND THAT IS THE LIMIT
	// THAT HAS TO GO FIRST. `alert_groups` is to be replaced by a `group_by` on
	// `notification_policies`, with the collapse key computed at delivery — and a
	// policy cannot group by `node` while the matcher is handed a set that contains
	// only `alertname` and `namespace`. Landing that column on top of this input
	// would ship a `group_by` over labels the matcher cannot see, which is why the
	// ticket calls this "a prerequisite, not a detail".
	//
	// ⭐ MERGED, NOT REPLACED, SO THE CHANGE IS MONOTONE. The focus's labels are a
	// superset of the group's for that alert — the group axes are DERIVED from them
	// — so every matcher that matched before still matches, and the only new
	// outcomes are matches that were previously impossible to express. Replacing
	// outright would have made this a behaviour change in both directions on a path
	// whose failure mode is a silently deleted notification.
	//
	// Group-scoped reasons (`all_resolved`, `new_alerts`) carry no focus and are
	// unchanged: `Focus` is nil and the group's own labels are the whole input,
	// exactly as before.
	match, err := s.policies.Evaluate(ctx, scope, MatchRequest{
		Reason: in.Reason,
		Labels: matchLabels(snap),
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

	kind, subjectID := subjectOf(in)

	n := domain.Notification{
		ID:          uuid.New(),
		OrgID:       scope.OrgID(),
		SubjectKind: kind,
		SubjectID:   subjectID,
		GroupID:     in.GroupID,
		// The delivery target, stored rather than re-derived at fan-out. Every
		// intent that reaches `mint` names a group — `Notify` rejects a nil GroupID
		// at :244 — so this arm is total here; the digest path builds its own row in
		// `digest.go` and names its own conversation there.
		ConversationKind: domain.ConversationAlertGroup,
		ConversationID:   in.GroupID,
		AlertID:          in.AlertID,
		CaseID:           in.CaseID,
		Reason:           in.Reason,
		StateVersion:     stateVersion,
		Status:           domain.StatusPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	n.IdempotencyKey = domain.IdempotencyKey(
		scope.OrgID(), n.SubjectKind, n.SubjectID, n.Reason, n.StateVersion)
	return n
}

// subjectOf resolves WHAT this fact is about — the (`subject_kind`, `subject_id`)
// pair — from the Reason's own declaration (`domain.Reason.Subject`, the
// allocation reason.go owns and migration 00056 widened the CHECK for).
//
// ⛔ IT IS NOT WHERE THE FACT IS DELIVERED. `GroupID` stays on the row for every
// Reason and is NOT NULL for every Reason, and the thread is keyed by it — see the
// pinned `threads.Ensure` call in fanOut. Fusing the two is what made an
// acknowledgement unaddressable: the row recording a receipt against ONE FIRING
// said it was about the group of forty.
//
// ⚠️ THE FALLBACK TO THE GROUP IS notifications_subject_ck SPEAKING, NOT A SHRUG.
// That CHECK requires the typed column its kind names to be PRESENT:
// `subject_kind = 'case'` beside a NULL `case_id` is a 23514, not a row. Only four
// Reasons are forced to name an alert (notifications_focus_ck / `AlertScoped`) and
// none is forced to name a case, so a per-alert or per-case Reason can legitimately
// arrive without its id — `comment` on an alert with no open episode is the plain
// example. Such an intent records the subject release N wrote, the group: coarser
// than the truth, but insertable and honest about it, and far better than refusing
// to notify at all because the id was optional at the door.
func subjectOf(in Intent) (domain.SubjectKind, uuid.UUID) {
	switch in.Reason.Subject() {
	case domain.SubjectAlert:
		if in.AlertID != nil {
			return domain.SubjectAlert, *in.AlertID
		}
	case domain.SubjectCase:
		if in.CaseID != nil {
			return domain.SubjectCase, *in.CaseID
		}
	}
	return domain.SubjectAlertGroup, in.GroupID
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
		// ⭐ THE THROTTLE COUNTS BY DELIVERY TARGET, NOT BY SUBJECT. A policy cap is
		// "how many things may oto say into this thread per window", so the numerator
		// has to include every fact that lands there — the per-alert ones (suppressed,
		// snoozed, comment) and the per-case ones (acked, expired, enriched) as much as
		// the group ones. While `subject_kind` could only be `alert_group` (before
		// migration 00056) a subject predicate happened to do that; the moment
		// `subjectOf` started writing honest subjects it would have counted only the
		// group-subject subset and quietly loosened every throttle in the fleet. If you
		// are allocating a new SubjectKind in reason.go, this line needs no change —
		// that is the point of keying it on `in.GroupID`.
		count, err := s.notifications.CountRecent(ctx, scope,
			in.GroupID, snap.TakenAt.Add(-t.Window))
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
// `scope` is `_`: planning is a pure decision over the snapshot, the match and the
// org defaults, and it reads no tenant-scoped row. The parameter stays in the
// signature because every sibling at this altitude carries the scope and dropping it
// here would make `plan` the one method a reader has to check before trusting that a
// tenant boundary is in hand.
func (s *NotificationService) plan(
	ctx context.Context, _ db.TenantScope, reason domain.Reason,
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
		in := planInput(reason, c, def, false, flapping)

		// ⛔ THE ONCE-PER-CHANNEL STORM-NOTICE CLAIM WAS HERE AND IS DELETED. It took
		// `channels.storm_notice_at` inside this transaction so that exactly one
		// destination won the right to announce that oto had started withholding.
		// Nothing withholds on storm any more, so there is nothing to announce and no
		// latch to take — see broadcast.go for the whole tombstone.

		p := domain.PlanFor(in)

		if p.BroadcastDamped {
			// ⭐ A DAMPED BROADCAST IS RECORDED, NEVER SILENT. The one remaining reason
			// for it is `no_capability`: this destination cannot surface a reply
			// in-channel, so the fact lands on the thread instead. That is the world's
			// constraint rather than a decision of oto's, and it is still written down,
			// because §B.6 requires every quiet to be accounted for.
			s.log.DebugContext(ctx, "notification: broadcast damped",
				"reason", string(reason), "channel_id", c.ID, "damped_by", p.BroadcastDampReason)
		}

		if p.ReplyDropped {
			// ⛔ `storm` AND `flapping` USED TO BE ARMS HERE AND BOTH ARE GONE.
			// `PlanFor` can no longer return either label, and `Suppressors.Add`
			// refuses both values besides — a retired reason stays decodable and
			// unwritable (see domain/suppression.go). Every arm that remains records a
			// fact about the DESTINATION, never oto's opinion of the signal.
			switch p.ReplyDropReason {
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

// ⛔ `claimStormNotice` WAS HERE AND IS DELETED, and with it the ONLY WRITE this
// service ever made to `channels`: routing resolves destinations, and the latch was
// the one thing that made the evaluation path touch a channel row at all. It took
// `channels.storm_notice_at` inside the evaluation transaction so a rolled-back
// evaluation could not consume a channel's one storm notice, and degraded a failed
// read to "not claimed" — the QUIET direction — so a database hiccup could not stop
// a storm reply reaching a thread. Nothing withholds on storm, so there is nothing
// to announce and no latch to ration.

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
			kind, subject := threadSubjectOf(n)
			th, err := s.threads.Ensure(ctx, scope, d.channel.ID, kind, subject, now)
			if err != nil {
				return created, err
			}
			id := th.ID
			threadID, rootLanded = &id, th.RootLanded()
		}

		for _, mode := range s.modesFor(n, d, rootLanded) {
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

// threadSubjectOf is WHICH CONVERSATION this fact belongs in, as distinct from what
// the fact is about (`subjectOf`) and from where it is delivered (`GroupID`).
//
// ⛔ FOR EVERY SIGNAL FACT IT IS THE ALERTGROUP GENERATION, AND IT MUST STAY PINNED.
// `fanOut` used to pass `n.SubjectKind, n.SubjectID` straight through, which was
// correct only while every Notification claimed the group; since migration 00056 a
// Notification says what it is ABOUT (`case` for an ack, `alert` for a snooze).
// Handing that to `Ensure` would key a conversation per Case or per Alert under
// `threads_subject_uniq (channel_id, subject_kind, subject_id)`, and forty alerts in
// one group would shatter into forty separate Slack threads. A conversation is keyed
// per GROUP; only the FACT varies.
//
// ⭐ A DIGEST IS THE ONE EXCEPTION, AND IT IS KEYED BY ITS POLICY, NOT BY ITS WINDOW.
// A digest has no group to be pinned to (migration 00058 relaxed `group_id` for
// exactly this row), and keying its thread by the WINDOW — which is what passing the
// notification's own subject would do, since `subject_id` is the policy but
// `state_version` is the window — is not what happens either: `subject_id` IS the
// policy, so one conversation per policy per channel falls out, with one reply per
// tick. That is the design the ticket asks for and it needs no change to
// `threads_subject_uniq`: a fresh thread every ten minutes would be a channel full of
// one-message threads, which is the noise a digest exists to replace.
func threadSubjectOf(n domain.Notification) (domain.SubjectKind, uuid.UUID) {
	// ⭐ READ OFF THE ROW, NOT DERIVED FROM A DIGEST BRANCH (migration 00064).
	// This function used to BE the derivation — `if n.Digest() { digest, SubjectID }
	// else { alert_group, GroupID }` — computed on every fan-out and thrown away.
	// The pair is that return value, stored, so the next conversation kind arrives
	// without another arm here and without another branch in every reader.
	//
	// `dispatch.go` already branched on the THREAD's kind rather than on
	// `n.Digest()`, so one reader was in this shape before the column existed.
	return n.ConversationKind.SubjectKind(), n.ConversationID
}

// digestModes is the whole of a digest's §H.6, and it is two lines because a digest
// is not a transition.
//
// The mode table in `domain/mode.go` answers "does this CHANGE surface in this
// channel, and as an amend or a reply, and may it be broadcast" — questions with no
// meaning for a periodic summary. A digest has exactly one rule: OPEN THE
// CONVERSATION ONCE, THEN REPLY TO IT ONCE PER WINDOW. Never `update_root`: amending
// the root would overwrite last window's summary with this window's, which is the one
// thing a digest must not do, because the whole point is a readable history of
// windows. Never `broadcast_reply`: a digest is already the batched, quiet form, and
// surfacing every window in-channel would undo that.
//
// A destination that can neither thread nor amend — the generic webhook — gets
// `post_root` every time, which for it means a standalone post, and
// `deliveries_thread_ck` permits the NULL thread.
//
// ⚠️ IT IS DELIBERATELY NOT IN `domain/mode.go`. Adding a digest arm to `PlanFor`
// would mean giving the §H.6 table a Reason whose columns — verbosity,
// thread_updates, thread-exists — are all inapplicable, and every one of those columns
// would then need a documented "not for digests" answer. The rule is smaller than its
// exception list, so it lives with the thing it describes.
func digestModes(c domain.Channel, rootLanded bool) []domain.Mode {
	if !needsThread(c) || !rootLanded {
		return []domain.Mode{domain.ModePostRoot}
	}
	return []domain.Mode{domain.ModeThreadReply}
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
func (s *NotificationService) modesFor(
	n domain.Notification, d destination, rootLanded bool,
) []domain.Mode {
	// A digest is not a transition, so the table has nothing to say about it. See
	// digestModes.
	if n.Digest() {
		return digestModes(d.channel, rootLanded)
	}
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

// matchLabels is the label set a policy is evaluated against: the group's axes,
// widened by the focused alert's own labels when the intent has a focus.
//
// The group's axes are always present, whatever the focus carries, because they
// are what every member shares and what a policy written before this change
// targets. The focus wins on a collision only because it is the more specific
// fact about the same alert; in practice it cannot collide, since the group axes
// are computed from the focused alert's labels in the first place.
//
// Returns the group's labels unchanged when there is no focus, and never returns
// a nil map, so a caller cannot tell "no labels" apart from "no group" by shape.
func matchLabels(snap domain.Snapshot) map[string]string {
	if snap.Focus == nil || len(snap.Focus.Labels) == 0 {
		return snap.Group.GroupLabels
	}
	out := make(map[string]string, len(snap.Group.GroupLabels)+len(snap.Focus.Labels))
	for k, v := range snap.Group.GroupLabels {
		out[k] = v
	}
	for k, v := range snap.Focus.Labels {
		out[k] = v
	}
	return out
}
