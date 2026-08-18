package service

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/platform/jobs/ordering"
)

// GateFactory mints the per-tenant ordering gate.
type GateFactory interface {
	Gate(s db.TenantScope) (*ordering.Gate, error)
}

// DispatchService sends ONE delivery.
//
// It is the most consequential code in oto, and it runs in THREE PHASES with the
// provider call deliberately BETWEEN two transactions:
//
//	TX 1  take the thread's advisory lock -> read the thread UNDER the lock ->
//	      RE-DERIVE THE MODE -> let the gate decide -> claim the row (status
//	      becomes 'sending', durably) -> render NOW -> persist the bytes. COMMIT.
//	--    call the provider. No transaction, no pooled connection, no lock held.
//	TX 2  take the lock again -> record the handle -> advance the sequence.
//
// ⚠ WHY THE CALL IS NOT INSIDE THE TRANSACTION. It used to be, on the theory that
// committing the send and the sequence advance together made ordering correct. It
// cannot: a network call and a database write are not committable together, and
// pretending otherwise inverted the failure mode. A cancelled context or a failed
// COMMIT between `chat.postMessage` and `MarkSent` rolled the claim back to
// `pending`, un-incremented `attempts`, and let the job re-post the message —
// while erasing the `sending` status that is the only thing the ambiguity latch
// keys on. So the one crash window it was meant to close was the one it opened,
// silently, as a DUPLICATE ROOT in somebody's channel.
//
// What replaces it is honest: the claim is COMMITTED as `sending` before the call,
// so a crash anywhere afterwards leaves a row that says "this may have been sent".
// The §G.5 lease reclaims it, `ambiguous` is latched on exactly the mode where a
// re-send is a new message rather than an idempotent edit, and the card carries a
// visible marker. Exactly-once does not exist; a labelled duplicate is what oto
// chooses over silence.
//
// Ordering survives the shorter lock because ordering was never the lock's job:
// `last_sent_seq` does not move until TX 2, so every item behind this one still
// sees itself out of turn and waits. The lock serialises deciding, not sending.
//
// Rendering after the claim rather than at enqueue is what makes the card true:
// the message describes the world as it is when it goes out, not as it was when
// somebody decided to say something.
//
// THIS FILE CONTAINS NO PROVIDER-SPECIFIC CODE. It never asks what kind of
// destination it is talking to; it asks what the destination can DO. That is not
// tidiness — it is the only way the §H.6 table can be trusted, because a table
// with an "unless it is this provider" branch is not a table.
type DispatchService struct {
	txr           TxRunner
	notifications NotificationStore
	deliveries    DeliveryStore
	threads       ThreadStore
	channels      ChannelStore
	events        EventSink
	views         *ViewService
	registry      ChannelRegistry
	unsealer      CredentialUnsealer
	gates         GateFactory
	enqueuer      Enqueuer
	settings      SettingsReader
	baseURL       string
	maxInstances  int
	lease         time.Duration
	clk           clock.Clock
	log           *slog.Logger
	metrics       *Metrics
}

// DispatchConfig is everything NewDispatchService needs.
type DispatchConfig struct {
	Tx            TxRunner
	Notifications NotificationStore
	Deliveries    DeliveryStore
	Threads       ThreadStore
	Channels      ChannelStore
	Events        EventSink
	Views         *ViewService
	Registry      ChannelRegistry
	Unsealer      CredentialUnsealer
	Gates         GateFactory
	Enqueuer      Enqueuer
	// Settings reads the org's notification tuning — here, ONLY the unacked
	// reminder's mention audience (ADR 0020). OPTIONAL: nil means `none`, which is
	// the shipped default anyway, because a settings lookup must never be able to
	// stop a delivery.
	Settings     SettingsReader
	BaseURL      string
	MaxInstances int
	// StaleClaimLease is how long a `sending` delivery is believed before it is
	// treated as abandoned (§G.5). Zero means 120 s.
	StaleClaimLease time.Duration
	Clock           clock.Clock
	Logger          *slog.Logger
	// Metrics is the delivery-side Prometheus surface. Nil yields unregistered
	// collectors, so a test needs no registry.
	Metrics *Metrics
}

// NewDispatchService builds the service.
func NewDispatchService(cfg DispatchConfig) (*DispatchService, error) {
	switch {
	case cfg.Tx == nil, cfg.Notifications == nil, cfg.Deliveries == nil,
		cfg.Threads == nil, cfg.Channels == nil, cfg.Events == nil,
		cfg.Views == nil, cfg.Registry == nil, cfg.Gates == nil, cfg.Enqueuer == nil:
		return nil, errs.New(errs.KindInternal, "dispatch_service_deps",
			"the dispatch service needs a tx runner, its stores, an event sink, a view service, a channel registry, an ordering gate and an enqueuer")
	}
	s := &DispatchService{
		txr: cfg.Tx, notifications: cfg.Notifications, deliveries: cfg.Deliveries,
		threads: cfg.Threads, channels: cfg.Channels, events: cfg.Events,
		views: cfg.Views, registry: cfg.Registry, unsealer: cfg.Unsealer,
		gates: cfg.Gates, enqueuer: cfg.Enqueuer, settings: cfg.Settings,
		baseURL: cfg.BaseURL, maxInstances: cfg.MaxInstances,
		lease: cfg.StaleClaimLease, clk: cfg.Clock, log: cfg.Logger,
		metrics: cfg.Metrics,
	}
	if s.maxInstances <= 0 {
		s.maxInstances = DefaultMaxInstances
	}
	if s.lease <= 0 {
		s.lease = ordering.DefaultStaleClaimLease
	}
	if s.clk == nil {
		s.clk = clock.New()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.metrics == nil {
		s.metrics = NewMetrics(nil)
	}
	return s, nil
}

// outcome is what the transaction decided the JOB should do, once it committed.
//
// It exists because a failure must be RECORDED and then retried, and those are
// contradictory inside one transaction: returning the retry error from inside
// would roll back the very row that says the attempt failed. So the transaction
// always commits its bookkeeping and hands the job's verdict out here.
type outcome struct {
	snoozeFor    time.Duration
	snoozeReason string
	// retry is returned to the queue so River applies §G.6's backoff. It is set
	// only after the failure has already been persisted.
	retry error
}

// sendPlan is everything TX 1 decided, handed across the transaction boundary to
// the provider call and then to TX 2.
//
// It exists so that nothing between the COMMIT and the network call has to ask
// the database another question: the claim is already durable, the bytes are
// already on disk, and every decision — which mode, which thread, which
// destination — was made under the lock and is carried here rather than re-read.
type sendPlan struct {
	delivery domain.Delivery
	thread   domain.Thread
	channel  domain.Channel
	// mode is the RE-DERIVED mode: the one the gate was asked about and the one
	// the provider call will use. There is exactly one of it.
	mode   domain.Mode
	msg    RenderedMessage
	target Target
	gate   *ordering.Gate
}

// Dispatch is `deliver.dispatch`.
func (s *DispatchService) Dispatch(
	ctx context.Context, scope db.TenantScope, deliveryID uuid.UUID,
) error {
	var (
		out  outcome
		plan *sendPlan
	)
	err := s.txr.InTx(ctx, func(ctx context.Context) error {
		var err error
		out, plan, err = s.prepare(ctx, scope, deliveryID)
		return err
	})
	if err != nil {
		return err
	}

	if plan != nil {
		// THE CLAIM IS NOW COMMITTED. Everything from here is outside the
		// transaction and the thread lock.
		out, err = s.perform(ctx, scope, plan)
		if err != nil {
			return err
		}
	}

	switch {
	case out.snoozeFor > 0:
		return jobs.Snooze(out.snoozeFor, out.snoozeReason)
	case out.retry != nil:
		return out.retry
	default:
		return nil
	}
}

// prepare is TX 1: decide, claim, render. It returns a plan when — and only when
// — a provider call is owed.
func (s *DispatchService) prepare(
	ctx context.Context, scope db.TenantScope, deliveryID uuid.UUID,
) (outcome, *sendPlan, error) {
	now := s.clk.Now().UTC()

	d, err := s.deliveries.Get(ctx, scope, deliveryID)
	if err != nil {
		return outcome{}, nil, err
	}
	if d.Status.Resolved() {
		// Somebody already finished this. At-least-once means this happens, and the
		// only correct response is to exit quietly.
		return outcome{}, nil, nil
	}
	if d.Status == domain.DeliveryFailed && d.NextAttemptAt != nil && d.NextAttemptAt.After(now) {
		// §G.6's per-class schedule is PERSISTED, so it is the schedule of record.
		// The queue's own backoff may wake this job earlier; claiming anyway would
		// make the stored schedule decorative and re-hit a destination that has
		// asked for room.
		return outcome{snoozeFor: d.NextAttemptAt.Sub(now), snoozeReason: "not_due"}, nil, nil
	}

	channel, err := s.channels.Get(ctx, scope, d.ChannelID)
	if err != nil {
		return outcome{}, nil, err
	}

	// A destination with no thread — one that can neither thread nor amend — has
	// no ordering to respect: every message is a standalone post and there is
	// nothing for a later one to be "after".
	if d.ThreadID == nil {
		return s.claim(ctx, scope, d, domain.Thread{}, channel, nil, domain.ModePostRoot, now)
	}

	gate, err := s.gates.Gate(scope)
	if err != nil {
		return outcome{}, nil, err
	}

	decision, th, mode, err := s.decide(ctx, scope, gate, d, channel, true)
	if err != nil {
		return outcome{}, nil, err
	}

	switch decision.Action {
	case ordering.ActionProceed:
		return s.claim(ctx, scope, d, th, channel, gate, mode, now)

	case ordering.ActionWaitForRoot, ordering.ActionWaitForPredecessor:
		// A snooze consumes NO attempt, by design: an item waiting its turn has not
		// failed, and eroding its retry budget while it queues would kill exactly
		// the messages a busy thread is trying hardest to deliver.
		return outcome{snoozeFor: decision.Wait, snoozeReason: decision.Reason}, nil, nil

	case ordering.ActionOutOfOrder:
		// The slot was already resolved: a duplicate worker, or a redelivery after
		// gap recovery moved past this item.
		return outcome{}, nil, nil

	case ordering.ActionAbandon:
		return outcome{}, nil, s.abandon(ctx, scope, d, decision.Reason, now)

	case ordering.ActionRecoverThread:
		return s.recoverThread(ctx, scope, gate, d, channel, decision.Reason, now)

	default:
		return outcome{}, nil, errs.Newf(errs.KindInternal, "unknown_gate_action",
			"the ordering gate returned an unknown action %q", decision.Action)
	}
}

// decide is the ONE PLACE the gate is consulted, and the one place the mode is
// derived.
//
// ⚠ THE MODE THE GATE IS ASKED ABOUT AND THE MODE THE SENDER USES ARE THE SAME
// VALUE. They used to differ: `Decide` read the mode stored on the row while the
// sender re-derived it, and the gate won. A root delivery that died terminally
// left its thread `opening` with no root, every later delivery still carried a
// root-needing mode on its row, and the gate answered `wait_for_root` — for a
// root nothing left in the thread would ever post — while `effectiveMode`, three
// call frames away, already knew to post the card instead of failing. Because a
// snooze consumes no attempt, that thread went silent permanently. Deriving once,
// here, before the decision, is the fix.
//
// eagerRecover asks for a gap sweep when the item is merely waiting its turn. A
// slot that no delivery will ever fill — a `next_seq` that was allocated and
// never used — is otherwise only noticed after the full MaxWait, which is fifteen
// minutes of silence bought by nothing: the sweep is a handful of indexed reads
// under a lock we already hold.
func (s *DispatchService) decide(
	ctx context.Context, scope db.TenantScope, gate *ordering.Gate,
	d domain.Delivery, channel domain.Channel, eagerRecover bool,
) (ordering.Decision, domain.Thread, domain.Mode, error) {
	// The lock FIRST, so the thread this reads is the thread the gate decides on.
	if err := gate.Lock(ctx, *d.ThreadID); err != nil {
		return ordering.Decision{}, domain.Thread{}, "", err
	}

	th, err := s.threads.Get(ctx, scope, *d.ThreadID)
	if err != nil {
		return ordering.Decision{}, domain.Thread{}, "", err
	}
	mode := s.effectiveMode(d, th, channel)

	decision, _, err := gate.Claim(ctx, s.item(d, mode))
	if err != nil {
		return ordering.Decision{}, domain.Thread{}, "", err
	}
	if decision.Action != ordering.ActionWaitForPredecessor || !eagerRecover {
		return decision, th, mode, nil
	}

	rec, err := gate.Recover(ctx, *d.ThreadID)
	if err != nil {
		return ordering.Decision{}, domain.Thread{}, "", err
	}
	if rec.Advanced == 0 {
		return decision, th, mode, nil
	}

	if th, err = s.threads.Get(ctx, scope, *d.ThreadID); err != nil {
		return ordering.Decision{}, domain.Thread{}, "", err
	}
	mode = s.effectiveMode(d, th, channel)
	decision, _, err = gate.Claim(ctx, s.item(d, mode))
	return decision, th, mode, err
}

// item is the delivery as the ordering gate sees it.
func (s *DispatchService) item(d domain.Delivery, mode domain.Mode) ordering.Item {
	return ordering.Item{
		ID:        d.ID,
		ThreadID:  *d.ThreadID,
		Seq:       d.ThreadSeq,
		NeedsRoot: mode.NeedsRoot(),
		CreatedAt: d.CreatedAt,
	}
}

// abandon records a delivery that must never be sent and lets the thread move on.
func (s *DispatchService) abandon(
	ctx context.Context, scope db.TenantScope, d domain.Delivery, why string, now time.Time,
) error {
	if err := s.deliveries.MarkSkipped(ctx, scope, d.ID, why, now); err != nil {
		return err
	}
	if d.ThreadID != nil {
		if err := s.threads.AdvanceSent(ctx, scope, *d.ThreadID, d.ThreadSeq, now); err != nil {
			return err
		}
	}
	d.Status = domain.DeliverySkipped
	return s.settle(ctx, scope, d, why, now)
}

// recoverThread handles a thread that cannot make progress as it stands (§H.9).
//
// The distinction it turns on is the whole of §H.9: a terminal provider error is
// a STATE TRANSITION, not a retry.
//
//   - `message_not_found`, `cannot_reply_to_message`, `edit_window_closed`,
//     `restricted_action_thread_locked` — the DESTINATION IS FINE and only oto's
//     handle is gone. Clear the pointer, turn this delivery into a fresh root,
//     and let it re-point the thread when it lands.
//   - `channel_not_found`, `is_archived`, `not_in_channel`, `token_revoked`,
//     `account_inactive` — the destination or the credential is gone. Nothing
//     will ever send here again; every queued item is marked skipped so the
//     backlog becomes visibly abandoned rather than invisibly pending forever.
//
// oto never goes and LOOKS. Its database is its memory of the destination, and a
// system that re-derives its memory from the thing it is remembering has none.
//
// ⚠ THIS FUNCTION MAY NOT ANSWER A FRUITLESS RECOVERY WITH A SNOOZE. A snooze
// consumes no attempt, so "recover, advance nothing, snooze two seconds" is an
// infinite loop that no ceiling can end — it is the wedge §G.7.3 forbids, dressed
// as liveness. Every path below either makes progress, hands the head to a
// delivery it has just re-enqueued, or reaches a TERMINAL outcome for this item.
func (s *DispatchService) recoverThread(
	ctx context.Context, scope db.TenantScope, gate *ordering.Gate,
	d domain.Delivery, channel domain.Channel, why string, now time.Time,
) (outcome, *sendPlan, error) {
	// THE THREAD IS READ BEFORE ANYTHING IS SWEPT, and that order is load-bearing.
	// On a DEAD thread `classifySlot` skips every remaining slot — correctly, when
	// the destination is gone — INCLUDING the slot of the delivery being dispatched
	// right now. For a RECOVERABLE dead reason that is precisely wrong: this
	// delivery is about to be turned into the fresh root that revives the thread,
	// and sweeping its slot first leaves it re-pointed, pending, and permanently
	// out of order behind a head that has already moved past it. Sweep only where
	// there is genuinely nothing left to send.
	th, err := s.threads.Get(ctx, scope, *d.ThreadID)
	if err != nil {
		return outcome{}, nil, err
	}
	if th.State == domain.ThreadDead {
		return s.recoverDeadThread(ctx, scope, gate, d, th, now)
	}

	// Move the head past everything already finished. This is the liveness half
	// of the ordering invariant: without it, one poisoned message wedges a thread
	// forever.
	rec, err := gate.Recover(ctx, *d.ThreadID)
	if err != nil {
		return outcome{}, nil, err
	}

	// Recovery stopped on a slot it may not skip, and the head cannot move until
	// that delivery runs. The usual reason it has not is that its job is gone —
	// discarded past its ceiling, cancelled, or lost with the pod that held it — so
	// give it one. Skipping it instead would send a reply before the message it
	// replies to; waiting without re-enqueuing is waiting on nothing.
	if rec.StalledItem != uuid.Nil && rec.StalledItem != d.ID {
		if _, err := s.enqueuer.Enqueue(ctx,
			jobs.DeliverDispatchArgs{DeliveryID: rec.StalledItem}); err != nil {
			return outcome{}, nil, err
		}
	}

	if th, err = s.threads.Get(ctx, scope, *d.ThreadID); err != nil {
		return outcome{}, nil, err
	}
	if th.State == domain.ThreadDead {
		// The sweep above found a thread that died under us. Fall through to §H.9.
		return s.recoverDeadThread(ctx, scope, gate, d, th, now)
	}

	// Not dead: the gate asked for recovery because this item could not make
	// progress as things stood. Re-decide against the state recovery just produced
	// — including the RE-DERIVED mode, which turns a reply with no root into the
	// fresh root that repairs the thread.
	decision, th, mode, err := s.decide(ctx, scope, gate, d, channel, false)
	if err != nil {
		return outcome{}, nil, err
	}

	switch decision.Action {
	case ordering.ActionProceed:
		return s.claim(ctx, scope, d, th, channel, gate, mode, now)
	case ordering.ActionOutOfOrder:
		return outcome{}, nil, nil
	case ordering.ActionAbandon:
		return outcome{}, nil, s.abandon(ctx, scope, d, decision.Reason, now)
	}

	if rec.Advanced > 0 || rec.StalledItem != uuid.Nil {
		// Either the head moved, or a real delivery owns it and now has a job. Both
		// are progress, and progress is bounded: the head can only advance as far as
		// next_seq, and the slot ahead resolves or dies on its own attempt ceiling.
		return outcome{snoozeFor: 2 * time.Second, snoozeReason: why}, nil, nil
	}

	// NOTHING moved and NOTHING owns the head. There is no state left that another
	// pass could find, so another snooze is the wedge. Dead-letter this delivery
	// and let the thread past it: an operator reading "oto gave up" on the alert
	// page is the whole point of §H.9, and it is strictly better than a channel
	// that has been silently quiet for a week.
	message := "the thread could not make progress and recovery had nothing to advance (" + why + ")"
	if err := s.deliveries.MarkDead(ctx, scope, d.ID, message, domain.ClassPermanent, now); err != nil {
		return outcome{}, nil, err
	}
	if err := s.threads.AdvanceSent(ctx, scope, th.ID, d.ThreadSeq, now); err != nil {
		return outcome{}, nil, err
	}
	d.Status, d.ErrorClass = domain.DeliveryDead, domain.ClassPermanent
	return outcome{}, nil, s.settle(ctx, scope, d, message, now)
}

// recoverDeadThread runs the §H.9 transition for a thread a terminal provider
// error killed.
func (s *DispatchService) recoverDeadThread(
	ctx context.Context, scope db.TenantScope, gate *ordering.Gate,
	d domain.Delivery, th domain.Thread, now time.Time,
) (outcome, *sendPlan, error) {
	if !th.DeadReason.Recoverable() {
		// Nothing will ever send here again. NOW sweep, so the backlog behind this
		// delivery becomes visibly skipped rather than invisibly pending forever.
		if _, err := gate.Recover(ctx, *d.ThreadID); err != nil {
			return outcome{}, nil, err
		}
		if err := s.deliveries.MarkDead(ctx, scope, d.ID,
			"the destination is gone: "+string(th.DeadReason), domain.ClassPermanent, now); err != nil {
			return outcome{}, nil, err
		}
		if err := s.threads.AdvanceSent(ctx, scope, th.ID, d.ThreadSeq, now); err != nil {
			return outcome{}, nil, err
		}
		d.Status, d.ErrorClass = domain.DeliveryDead, domain.ClassPermanent
		return outcome{}, nil, s.settle(ctx, scope, d, string(th.DeadReason), now)
	}

	// The pointer is recoverable. Clear it and re-root.
	if err := s.threads.ClearPointer(ctx, scope, th.ID, now); err != nil {
		return outcome{}, nil, err
	}
	if err := s.deliveries.RepointToRoot(ctx, scope, d.ID,
		"thread pointer lost ("+string(th.DeadReason)+"); posting a fresh root", now); err != nil {
		return outcome{}, nil, err
	}
	if _, err := s.enqueuer.Enqueue(ctx, jobs.DeliverDispatchArgs{DeliveryID: d.ID}); err != nil {
		return outcome{}, nil, err
	}
	return outcome{}, nil, nil
}

// claim is the tail of TX 1: take the row, render, persist the bytes, and open
// the destination. It runs under the thread's advisory lock and returns a plan
// for the provider call that follows the COMMIT.
func (s *DispatchService) claim(
	ctx context.Context, scope db.TenantScope,
	d domain.Delivery, th domain.Thread, channel domain.Channel,
	gate *ordering.Gate, mode domain.Mode, now time.Time,
) (outcome, *sendPlan, error) {
	claimed, ok, err := s.deliveries.Claim(ctx, scope, d.ID, now.Add(-s.lease), now)
	if err != nil {
		return outcome{}, nil, err
	}
	if !ok {
		// Another worker holds it, or it resolved between the read and the claim.
		// Zero rows IS the mechanism (§G.5); exit quietly.
		return outcome{}, nil, nil
	}
	d = claimed

	if !channel.Live() {
		// The destination was disabled between the fan-out and now. Recording it as
		// skipped keeps "why is there no message?" answerable.
		return outcome{}, nil, s.abandon(ctx, scope, d, string(domain.SuppressedChannelDisabled), now)
	}

	n, err := s.notifications.Get(ctx, scope, d.NotificationID)
	if err != nil {
		return outcome{}, nil, err
	}

	// C11: the view is built HERE, at claim time, from the world as it is now.
	view, err := s.views.Build(ctx, scope, ViewRequest{Notification: n})
	if err != nil {
		return outcome{}, nil, err
	}

	renderer, err := s.registry.Renderer(
		ProviderType(channel.Type), RendererID(channel.Renderer))
	if err != nil {
		// A channel row naming a renderer that does not exist is a configuration
		// error, and §G.6 says configuration errors NEVER retry.
		out, err := s.fail(ctx, scope, d, channel, err, domain.ClassConfigInvalid, now)
		return out, nil, err
	}

	opts := RenderOptions{
		Mode:           RenderMode(mode),
		Verbosity:      RenderVerbosity(channel.EffectiveVerbosity()),
		ShowFieldEmoji: channel.ShowFieldEmoji,
		BaseURL:        s.baseURL,
		MaxInstances:   s.maxInstances,
		Continued:      continuedRoot(d, th, mode),
		Mentions:       s.mentionsFor(ctx, scope, n, view),
	}
	msg, err := renderer.Render(ctx, view, opts)
	if err != nil {
		// §L.6: the payload is persisted alongside the failure, never silently
		// truncated, so a dead delivery can be debugged from the row.
		if len(msg.Payload) > 0 && msg.Fallback != "" {
			_ = s.deliveries.PersistRendered(ctx, scope, d.ID,
				msg.Payload, msg.Hash, msg.Fallback, now)
		}
		out, err := s.fail(ctx, scope, d, channel, err, domain.ClassConfigInvalid, now)
		return out, nil, err
	}

	// §G.7.4 coalescing: a root update whose bytes match what the card already
	// shows buys nothing. This is what turns a flapping alert's forty identical
	// updates into one send and thirty-nine visible `skipped` rows.
	if mode == domain.ModeUpdateRoot && d.ThreadID != nil {
		last, err := s.deliveries.LastRootHash(ctx, scope, *d.ThreadID)
		if err != nil {
			return outcome{}, nil, err
		}
		if last != "" && last == msg.Hash {
			return outcome{}, nil, s.abandon(ctx, scope, d,
				string(domain.SuppressedDuplicateRender), now)
		}
	}

	// BEFORE the network call, always.
	if err := s.deliveries.PersistRendered(ctx, scope, d.ID,
		msg.Payload, msg.Hash, msg.Fallback, now); err != nil {
		return outcome{}, nil, err
	}

	plan := &sendPlan{
		delivery: d, thread: th, channel: channel, mode: mode, msg: msg, gate: gate,
	}

	target, err := s.open(ctx, scope, channel)
	if err != nil {
		class := domain.ClassConfigInvalid
		if errs.IsKind(err, errs.KindNotFound) {
			class = domain.ClassAuthExpired
		}
		out, err := s.fail(ctx, scope, d, channel, err, class, now)
		return out, nil, err
	}
	plan.target = target
	return outcome{}, plan, nil
}

// perform is the provider call and the transaction that records it.
//
// THE CALL HAPPENS HERE, WITH NO TRANSACTION OPEN, NO POOLED CONNECTION HELD AND
// NO ADVISORY LOCK TAKEN. A rate-limited destination that asks for thirty seconds
// gets them without pinning a general-pool connection or serialising an unrelated
// thread behind it.
//
// There is exactly ONE provider call, for exactly one mode. The root amend that
// §H.6 pairs with a reply is a delivery in its own right — its own row, its own
// sequence ahead of the reply's, its own claim and retry budget — so it arrives
// here as its own dispatch rather than as an unrecorded rider on somebody else's.
func (s *DispatchService) perform(
	ctx context.Context, scope db.TenantScope, p *sendPlan,
) (outcome, error) {
	defer func() { _ = p.target.Close() }()

	result, err := s.deliver(ctx, p.target, p.delivery, p.thread, p.mode, p.msg)
	return s.record(ctx, scope, p, result, err)
}

// record is TX 2: it writes what the provider said.
//
// It runs on a context that CANNOT BE CANCELLED. The message may already be in
// somebody's channel, and losing the row that says so is how oto forgets a send
// and does it again; a shutdown that interrupts this must interrupt it as a
// timeout on a transaction that was at least attempted, not as an inherited
// cancellation that never opened one.
func (s *DispatchService) record(
	ctx context.Context, scope db.TenantScope, p *sendPlan,
	result DeliverResult, sendErr error,
) (outcome, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()

	now := s.clk.Now().UTC()
	d := p.delivery

	var out outcome
	err := s.txr.InTx(ctx, func(ctx context.Context) error {
		if d.ThreadID != nil && p.gate != nil {
			// Back under the thread lock to advance the sequence.
			if err := p.gate.Lock(ctx, *d.ThreadID); err != nil {
				return err
			}
		}
		if sendErr != nil {
			var err error
			out, err = s.fail(ctx, scope, d, p.channel, sendErr, classOf(sendErr), now)
			return err
		}
		return s.succeed(ctx, scope, d, p.thread, p.channel, p.mode, result, now)
	})
	return out, err
}

// recordTimeout bounds TX 2. It is generous because the alternative to writing
// this row is forgetting a message that has already been delivered.
const recordTimeout = 30 * time.Second

// continuedRoot reports that this post_root REPLACES a card the destination
// already had for this generation, rather than opening the conversation.
//
// It is derived, never stored, from two facts oto already keeps. A thread that
// carries a conversation id has been posted to before: ClearPointer drops the
// message id on the §H.9 recovery and on the reply-ceiling re-root, but keeps the
// conversation id deliberately, because it came from an API response and is still
// the right destination. And `ambiguous` marks the §G.5 re-send, whose
// predecessor may still be sitting in the channel.
//
// The very first root of a generation matches neither, which is the point: it is
// not continuing anything.
func continuedRoot(d domain.Delivery, th domain.Thread, mode domain.Mode) bool {
	if mode != domain.ModePostRoot || d.ThreadID == nil {
		return false
	}
	return d.Ambiguous || th.ProviderConversationID != ""
}

// rootRef is the provider handle of a thread's root message.
func rootRef(th domain.Thread) MessageRef {
	return MessageRef{
		ConversationID: th.ProviderConversationID,
		MessageID:      th.ProviderThreadID,
		ThreadID:       th.ProviderThreadID,
	}
}

// effectiveMode RE-DERIVES the delivery mode against the thread as it actually
// stands, right now.
//
// The mode on the row was chosen when the intent was minted, and the world has
// moved since: a root may have landed, a root may have been lost, a thread may
// have reached the reply ceiling. Sending the stale mode is how a reply gets
// posted into nothing, or how a second root appears next to the first.
//
// ⚠ IT IS THE ONLY SOURCE OF TRUTH FOR THE MODE, AND THE ORDERING GATE IS ASKED
// ABOUT ITS ANSWER, NOT ABOUT THE ROW. See decide: a gate reading the stale mode
// blocks on the very condition the second case below knows how to repair.
func (s *DispatchService) effectiveMode(
	d domain.Delivery, th domain.Thread, c domain.Channel,
) domain.Mode {
	mode := d.Mode

	// No thread at all: this destination only ever posts.
	if d.ThreadID == nil {
		return domain.ModePostRoot
	}

	switch {
	case mode == domain.ModePostRoot && th.SubjectKind == domain.SubjectDigest && th.RootLanded():
		// ⛔ A DIGEST IS NEVER AMENDED, AND THIS IS THE ARM THAT KEEPS IT SO. A digest
		// thread is one ongoing conversation per policy with ONE REPLY PER WINDOW, and
		// each reply is a different window's summary — so amending the root would
		// overwrite last window's numbers with this window's, destroying exactly the
		// history a digest exists to provide. The generic arm below would do that: it
		// reads a landed root plus `post_root` as "a second incident is about to
		// appear", which is right for a signal card and wrong here.
		//
		// The race is narrow and real: two windows minted before the first one's root
		// landed both carry `post_root`, and by the time the second is claimed the root
		// exists. It becomes the reply it should have been.
		//
		// A destination that can amend but cannot thread posts a second card instead,
		// which is honest — two summaries — where an amend would be a silent overwrite.
		if c.Capabilities.Has(domain.CapThreading) {
			return domain.ModeThreadReply
		}
		return domain.ModePostRoot

	case mode == domain.ModePostRoot && th.RootLanded():
		// A root already exists for this generation. Posting a second one would
		// read, to everybody in the channel, as a second incident.
		if c.Capabilities.Has(domain.CapAmend) {
			return domain.ModeUpdateRoot
		}
		return domain.ModePostRoot

	case mode.NeedsRoot() && !th.RootLanded():
		// Nothing to attach to or amend. Post the card instead of failing: the
		// fresh root carries every fact the reply would have restated.
		return domain.ModePostRoot

	case mode.IsReply() && th.NeedsFreshRoot():
		// S14: past the reply ceiling a thread is unreadable. Start a fresh card
		// rather than adding reply thirty-one to a thread nobody will scroll.
		return domain.ModePostRoot

	default:
		return mode
	}
}

// deliver performs the provider call for one mode.
//
// It branches on MODE, never on provider. `Amend` and `Deliver` are the whole of
// the port, and the reference each one needs comes from oto's own record of what
// the provider previously said.
func (s *DispatchService) deliver(
	ctx context.Context, target Target,
	d domain.Delivery, th domain.Thread, mode domain.Mode, msg RenderedMessage,
) (DeliverResult, error) {
	root := rootRef(th)

	if mode == domain.ModeUpdateRoot {
		return target.Amend(ctx, root, msg)
	}

	req := DeliverRequest{
		Message:    msg,
		Mode:       RenderMode(mode),
		DeliveryID: d.ID,
	}
	if mode.IsReply() {
		req.ReplyTo = &root
	}
	return target.Deliver(ctx, req)
}

// succeed records the provider handle and advances the thread.
func (s *DispatchService) succeed(
	ctx context.Context, scope db.TenantScope,
	d domain.Delivery, th domain.Thread, c domain.Channel,
	mode domain.Mode, result DeliverResult, now time.Time,
) error {
	messageID := result.Ref.MessageID
	conversationID := result.Ref.ConversationID
	if conversationID == "" {
		conversationID = th.ProviderConversationID
	}
	if messageID == "" && mode == domain.ModeUpdateRoot {
		// An amend that returned no id still amended the root oto already knows.
		messageID = th.ProviderThreadID
	}

	recorded, err := s.deliveries.MarkSent(ctx, scope, d.ID,
		messageID, conversationID, result.Raw, now)
	if err != nil {
		return err
	}
	if !recorded {
		// THE MESSAGE WENT OUT AND THIS ROW IS NO LONGER OURS. The claim guard
		// matched nothing: the §G.5 lease expired while the provider was answering
		// and another worker took the row, or a recovery resolved the slot. Writing
		// the thread's root handle from a claim we do not hold would overwrite a
		// newer truth with an older one, and returning an error here would retry a
		// send that has already happened. So: record nothing, advance nothing, and
		// make the orphan findable — the provider id is in the log and the counter
		// is the alert.
		s.metrics.ClaimLost.WithLabelValues(string(mode)).Inc()
		s.log.ErrorContext(ctx, "notification: a delivery landed but its claim was gone; nothing was recorded",
			"delivery_id", d.ID, "thread_id", d.ThreadID, "mode", string(mode),
			"provider_message_id", messageID, "provider_conversation_id", conversationID,
			"attempts", d.Attempts)
		return nil
	}

	if d.ThreadID != nil {
		switch {
		case mode == domain.ModePostRoot:
			// THE HANDLE COMES FROM THE RESPONSE, both halves of it. A configured
			// conversation name can be stale, renamed, or ambiguous; the response is
			// the only statement of where the message actually landed (S7).
			if err := s.threads.RecordRoot(ctx, scope, *d.ThreadID,
				conversationID, messageID, d.ID, d.ThreadSeq, now); err != nil {
				return err
			}
		case mode.IsReply():
			if err := s.threads.RecordReply(ctx, scope, *d.ThreadID, d.ThreadSeq, now); err != nil {
				return err
			}
		default:
			if err := s.threads.AdvanceSent(ctx, scope, *d.ThreadID, d.ThreadSeq, now); err != nil {
				return err
			}
		}
	}

	if c.HealthStatus != domain.HealthHealthy {
		// A successful send is the only honest way to clear a banner. Clearing it
		// on a retry that has not yet succeeded would hide a dead token.
		if err := s.channels.SetHealth(ctx, scope, c.ID, domain.HealthHealthy, "", now); err != nil {
			return err
		}
	}

	d.Status = domain.DeliverySent
	d.Mode = mode
	d.ProviderMessageID = messageID
	return s.settle(ctx, scope, d, "", now)
}

// fail records a failed attempt and decides whether the job should retry.
//
// Every branch here is §G.6, and the classification — not the provider — decides.
// `config_invalid` and `auth_expired` NEVER retry: they are the difference
// between "the destination is flaky" and "your token was revoked three days ago
// and nobody noticed", and telling those apart loudly is a product feature.
func (s *DispatchService) fail(
	ctx context.Context, scope db.TenantScope,
	d domain.Delivery, c domain.Channel, cause error, class domain.ErrorClass, now time.Time,
) (outcome, error) {
	code := providerCode(cause)
	message := cause.Error()

	// A terminal error that names a THREAD is a state transition, not a failure of
	// this delivery (§H.9).
	if class == domain.ClassPermanent || class == domain.ClassAuthExpired {
		if reason, isThreadError := domain.DeadReasonFor(code); isThreadError && d.ThreadID != nil {
			if err := s.threads.MarkDead(ctx, scope, *d.ThreadID, reason, now); err != nil {
				return outcome{}, err
			}
			if health := reason.DestinationHealth(); health != "" {
				if err := s.channels.SetHealth(ctx, scope, c.ID, health, message, now); err != nil {
					return outcome{}, err
				}
			}
			if err := s.deliveries.MarkFailed(ctx, scope, d.ID, message, class, now, now); err != nil {
				return outcome{}, err
			}
			// Re-enqueue so the next pass sees a dead thread and runs the §H.9
			// recovery with the gate held, which is the only place it is safe.
			if _, err := s.enqueuer.Enqueue(ctx, jobs.DeliverDispatchArgs{DeliveryID: d.ID}); err != nil {
				return outcome{}, err
			}
			return outcome{}, nil
		}
	}

	if health := class.DestinationHealth(); health != "" {
		if err := s.channels.SetHealth(ctx, scope, c.ID, health, message, now); err != nil {
			return outcome{}, err
		}
	}

	if class.Exhausted(d.Attempts) {
		if err := s.deliveries.MarkDead(ctx, scope, d.ID, message, class, now); err != nil {
			return outcome{}, err
		}
		if d.ThreadID != nil {
			// Advance the head so the deliveries queued behind this one are not
			// held hostage by it (§G.7.3).
			if err := s.threads.AdvanceSent(ctx, scope, *d.ThreadID, d.ThreadSeq, now); err != nil {
				return outcome{}, err
			}
		}
		d.Status, d.ErrorClass = domain.DeliveryDead, class
		return outcome{}, s.settle(ctx, scope, d, message, now)
	}

	wait := s.retryAfter(cause, class, d.Attempts)
	if err := s.deliveries.MarkFailed(ctx, scope, d.ID, message, class, now.Add(wait), now); err != nil {
		return outcome{}, err
	}
	d.Status, d.ErrorClass = domain.DeliveryFailed, class
	if err := s.settle(ctx, scope, d, message, now); err != nil {
		return outcome{}, err
	}

	if class == domain.ClassRateLimited {
		// Honoured EXACTLY, and as a snooze rather than a failed attempt: guessing
		// shorter than the provider asked is how a soft rate limit becomes a hard
		// block, and burning attempts while a destination merely asks us to slow
		// down is how a busy channel reaches `dead`.
		return outcome{snoozeFor: wait, snoozeReason: "rate_limited"}, nil
	}
	return outcome{retry: cause}, nil
}

// retryAfter is §G.6's schedule.
func (s *DispatchService) retryAfter(cause error, class domain.ErrorClass, attempts int) time.Duration {
	if class == domain.ClassRateLimited {
		var perr *ProviderError
		if errors.As(cause, &perr) && perr.RetryAfter > 0 {
			return perr.RetryAfter
		}
		if d, ok := errs.RetryAfterOf(cause); ok && d > 0 {
			return d
		}
		return domain.DefaultRateLimitDelay
	}
	return domain.Backoff(attempts, jitter())
}

// settle folds the fan-out back into the notification's aggregate status and
// appends the delivery's timeline row.
func (s *DispatchService) settle(
	ctx context.Context, scope db.TenantScope, d domain.Delivery, detail string, now time.Time,
) error {
	n, err := s.notifications.Get(ctx, scope, d.NotificationID)
	if err != nil {
		return err
	}
	if err := s.events.AppendDeliveryOutcome(ctx, scope, d, n.GroupID, n.AlertID, detail, now); err != nil {
		return err
	}

	statuses, err := s.deliveries.StatusesFor(ctx, scope, d.NotificationID)
	if err != nil {
		return err
	}
	aggregate := domain.AggregateStatus(statuses)
	if aggregate == domain.StatusSuppressed || aggregate == n.Status {
		// AggregateStatus returns `suppressed` for an empty fan-out, which cannot
		// legitimately happen here and must never overwrite a real status.
		return nil
	}
	return s.notifications.SetStatus(ctx, scope, n.ID, aggregate, now)
}

// open mints the destination for one send.
//
// The credential is read sealed, unsealed by a port, and handed straight to the
// provider. This module never holds a key and never logs a value, so no stack
// trace it can produce contains a workspace token.
func (s *DispatchService) open(
	ctx context.Context, scope db.TenantScope, c domain.Channel,
) (Target, error) {
	cfg := TargetConfig{
		ChannelID:      c.ID,
		OrgID:          c.OrgID,
		Name:           c.Name,
		Raw:            c.Config,
		Renderer:       RendererID(c.Renderer),
		Verbosity:      RenderVerbosity(c.EffectiveVerbosity()),
		ThreadUpdates:  c.ThreadUpdates,
		ShowFieldEmoji: c.ShowFieldEmoji,
	}

	var cred TargetCredential
	if c.CredentialID != nil {
		sealed, err := s.channels.Credential(ctx, scope, *c.CredentialID)
		if err != nil {
			return nil, err
		}
		if s.unsealer == nil {
			return nil, errs.New(errs.KindInternal, "no_credential_unsealer",
				"this destination has a sealed credential and no unsealer is configured")
		}
		values, err := s.unsealer.Unseal(ctx, sealed.Kind, sealed.Sealed, sealed.KeyVersion)
		if err != nil {
			return nil, err
		}
		cred = TargetCredential{Kind: sealed.Kind, Values: values}
	}

	return s.registry.Open(ctx, ProviderType(c.Type), cfg, cred)
}

// classOf maps a provider failure onto oto's retry taxonomy.
//
// A classified provider error wins outright: the provider is the only thing that
// knows its own error codes. Anything else falls back to the platform's
// kind-based inference, which retries only what a retry could plausibly fix.
func classOf(err error) domain.ErrorClass {
	var perr *ProviderError
	if errors.As(err, &perr) && perr.Class != "" {
		return domain.ErrorClass(perr.Class)
	}
	switch errs.KindOf(err) {
	case errs.KindRateLimited:
		return domain.ClassRateLimited
	case errs.KindUnauthorized, errs.KindForbidden:
		return domain.ClassAuthExpired
	case errs.KindValidation, errs.KindMalformed, errs.KindTooLarge, errs.KindUnsupported:
		return domain.ClassConfigInvalid
	case errs.KindNotFound, errs.KindPrecondition:
		return domain.ClassPermanent
	default:
		return domain.ClassRetryable
	}
}

// providerCode is the provider's own error string, verbatim. It is what
// `channel_threads.dead_reason` stores, so that a support question can be
// answered without a packet capture.
func providerCode(err error) string {
	var perr *ProviderError
	if errors.As(err, &perr) {
		return perr.Code
	}
	return ""
}

// jitter is §G.6's ±50 %, from crypto/rand because math/rand is banned and
// because a fleet of pods sharing a seed would retry in lockstep — which is a
// thundering herd aimed at a destination that has already told us it is
// struggling.
func jitter() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	// Map to [-0.5, +0.5).
	return float64(binary.BigEndian.Uint64(b[:])>>11)/float64(1<<53) - 0.5
}

// mentionsFor resolves the audience of ONE unacked reminder (ADR 0020).
//
// ⭐ IT IS SCOPED TO EXACTLY ONE REASON, AND THE GUARD IS THE FIRST LINE. Every
// other delivery returns before the settings read, so the common path costs no
// query at all — and, more importantly, so no other message oto sends can ever
// acquire a mention. A mention is the loudest thing oto can do and the reminder
// is the only fact that has argued for it.
//
// ⛔ IT NEVER PROPAGATES A FAILURE. An unreadable settings row yields NO mention,
// which is the shipped default and the quiet direction. A reminder that is a
// little less loud than configured still lands; a reminder that fails to send
// because a settings lookup timed out does not.
//
// ⛔ NOT A ROTA (§4.8, ADR 0013). The audience is read from configuration and is
// the same at 03:00 as at 15:00. Nothing here reads a clock, and that absence is
// the guarantee.
func (s *DispatchService) mentionsFor(
	ctx context.Context, scope db.TenantScope,
	n domain.Notification, view *NotificationView,
) []string {
	if n.Reason != domain.ReasonUnackedReminder || s.settings == nil || view == nil {
		return nil
	}
	def, err := s.settings.NotificationDefaults(ctx, scope)
	if err != nil {
		s.log.WarnContext(ctx, "notification: could not read the reminder mention policy",
			"org_id", scope.OrgID(), "error", err.Error())
		return nil
	}
	// The FOCUS severity when there is one, the group's otherwise. The gate is
	// about "how bad is the thing nobody has acknowledged", and a group's severity
	// is §H.2's max over its members, which is the right answer for a group-scoped
	// reminder.
	severity := view.Group.Severity
	if view.Focus != nil && view.Focus.Severity != "" {
		severity = view.Focus.Severity
	}
	return def.ReminderMention.Audience(severity)
}
