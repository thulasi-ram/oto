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
// It is the most consequential code in oto, and the shape of it is fixed by
// §G.7.2:
//
//	take the thread's advisory lock -> read the thread UNDER the lock ->
//	let the gate decide -> claim the row -> render NOW -> persist the bytes ->
//	call the provider -> record the handle and advance the sequence, in the
//	SAME transaction as the send.
//
// Two properties fall out of that and neither survives reordering it. Committing
// the send and the sequence advance together is what makes ordering correct: a
// crash between them would either resend the message or wedge the thread.
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
	baseURL       string
	maxInstances  int
	lease         time.Duration
	clk           clock.Clock
	log           *slog.Logger
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
	BaseURL       string
	MaxInstances  int
	// StaleClaimLease is how long a `sending` delivery is believed before it is
	// treated as abandoned (§G.5). Zero means 120 s.
	StaleClaimLease time.Duration
	Clock           clock.Clock
	Logger          *slog.Logger
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
		gates: cfg.Gates, enqueuer: cfg.Enqueuer,
		baseURL: cfg.BaseURL, maxInstances: cfg.MaxInstances,
		lease: cfg.StaleClaimLease, clk: cfg.Clock, log: cfg.Logger,
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

// Dispatch is `deliver.dispatch`.
func (s *DispatchService) Dispatch(
	ctx context.Context, scope db.TenantScope, deliveryID uuid.UUID,
) error {
	var out outcome
	err := s.txr.Tx(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.dispatch(ctx, scope, deliveryID)
		return err
	})
	if err != nil {
		return err
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

func (s *DispatchService) dispatch(
	ctx context.Context, scope db.TenantScope, deliveryID uuid.UUID,
) (outcome, error) {
	now := s.clk.Now().UTC()

	d, err := s.deliveries.Get(ctx, scope, deliveryID)
	if err != nil {
		return outcome{}, err
	}
	if d.Status.Resolved() {
		// Somebody already finished this. At-least-once means this happens, and the
		// only correct response is to exit quietly.
		return outcome{}, nil
	}

	// A destination with no thread — one that can neither thread nor amend — has
	// no ordering to respect: every message is a standalone post and there is
	// nothing for a later one to be "after".
	if d.ThreadID == nil {
		return s.send(ctx, scope, d, domain.Thread{}, now)
	}

	gate, err := s.gates.Gate(scope)
	if err != nil {
		return outcome{}, err
	}

	decision, _, err := gate.Claim(ctx, ordering.Item{
		ID:        d.ID,
		ThreadID:  *d.ThreadID,
		Seq:       d.ThreadSeq,
		NeedsRoot: d.Mode.NeedsRoot(),
		CreatedAt: d.CreatedAt,
	})
	if err != nil {
		return outcome{}, err
	}

	switch decision.Action {
	case ordering.ActionProceed:
		// fall through

	case ordering.ActionWaitForRoot, ordering.ActionWaitForPredecessor:
		// A snooze consumes NO attempt, by design: an item waiting its turn has not
		// failed, and eroding its retry budget while it queues would kill exactly
		// the messages a busy thread is trying hardest to deliver.
		return outcome{snoozeFor: decision.Wait, snoozeReason: decision.Reason}, nil

	case ordering.ActionOutOfOrder:
		// The slot was already resolved: a duplicate worker, or a redelivery after
		// gap recovery moved past this item.
		return outcome{}, nil

	case ordering.ActionAbandon:
		return outcome{}, s.abandon(ctx, scope, d, decision.Reason, now)

	case ordering.ActionRecoverThread:
		return s.recoverThread(ctx, scope, gate, d, decision.Reason, now)

	default:
		return outcome{}, errs.Newf(errs.KindInternal, "unknown_gate_action",
			"the ordering gate returned an unknown action %q", decision.Action)
	}

	th, err := s.threads.Get(ctx, scope, *d.ThreadID)
	if err != nil {
		return outcome{}, err
	}
	return s.send(ctx, scope, d, th, now)
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
func (s *DispatchService) recoverThread(
	ctx context.Context, scope db.TenantScope, gate *ordering.Gate,
	d domain.Delivery, why string, now time.Time,
) (outcome, error) {
	// Move the head past everything already finished. This is the liveness half
	// of the ordering invariant: without it, one poisoned message wedges a thread
	// forever.
	if _, err := gate.Recover(ctx, *d.ThreadID); err != nil {
		return outcome{}, err
	}

	th, err := s.threads.Get(ctx, scope, *d.ThreadID)
	if err != nil {
		return outcome{}, err
	}

	if th.State != domain.ThreadDead {
		// The gate asked for recovery because this item had waited past MaxWait,
		// not because the thread died. Recover has now advanced whatever it could;
		// re-evaluate on the next pass rather than sending out of order.
		return outcome{snoozeFor: 2 * time.Second, snoozeReason: why}, nil
	}

	if !th.DeadReason.Recoverable() {
		if err := s.deliveries.MarkDead(ctx, scope, d.ID,
			"the destination is gone: "+string(th.DeadReason), domain.ClassPermanent, now); err != nil {
			return outcome{}, err
		}
		if err := s.threads.AdvanceSent(ctx, scope, th.ID, d.ThreadSeq, now); err != nil {
			return outcome{}, err
		}
		d.Status, d.ErrorClass = domain.DeliveryDead, domain.ClassPermanent
		return outcome{}, s.settle(ctx, scope, d, string(th.DeadReason), now)
	}

	// The pointer is recoverable. Clear it and re-root.
	if err := s.threads.ClearPointer(ctx, scope, th.ID, now); err != nil {
		return outcome{}, err
	}
	if err := s.deliveries.RepointToRoot(ctx, scope, d.ID,
		"thread pointer lost ("+string(th.DeadReason)+"); posting a fresh root", now); err != nil {
		return outcome{}, err
	}
	if _, err := s.enqueuer.Enqueue(ctx, jobs.DeliverDispatchArgs{DeliveryID: d.ID}); err != nil {
		return outcome{}, err
	}
	return outcome{}, nil
}

// send claims, renders and delivers. It runs under the thread's advisory lock.
func (s *DispatchService) send(
	ctx context.Context, scope db.TenantScope, d domain.Delivery, th domain.Thread, now time.Time,
) (outcome, error) {
	claimed, ok, err := s.deliveries.Claim(ctx, scope, d.ID, now.Add(-s.lease), now)
	if err != nil {
		return outcome{}, err
	}
	if !ok {
		// Another worker holds it, or it resolved between the read and the claim.
		// Zero rows IS the mechanism (§G.5); exit quietly.
		return outcome{}, nil
	}
	d = claimed

	channel, err := s.channels.Get(ctx, scope, d.ChannelID)
	if err != nil {
		return outcome{}, err
	}
	if !channel.Live() {
		// The destination was disabled between the fan-out and now. Recording it as
		// skipped keeps "why is there no message?" answerable.
		return outcome{}, s.abandon(ctx, scope, d, string(domain.SuppressedChannelDisabled), now)
	}

	mode := s.effectiveMode(d, th, channel)

	n, err := s.notifications.Get(ctx, scope, d.NotificationID)
	if err != nil {
		return outcome{}, err
	}

	// C11: the view is built HERE, at claim time, from the world as it is now.
	view, err := s.views.Build(ctx, scope, ViewRequest{Notification: n})
	if err != nil {
		return outcome{}, err
	}

	renderer, err := s.registry.Renderer(
		ProviderType(channel.Type), RendererID(channel.Renderer))
	if err != nil {
		// A channel row naming a renderer that does not exist is a configuration
		// error, and §G.6 says configuration errors NEVER retry.
		return s.fail(ctx, scope, d, channel, err, domain.ClassConfigInvalid, now)
	}

	opts := RenderOptions{
		Mode:           RenderMode(mode),
		Verbosity:      RenderVerbosity(channel.EffectiveVerbosity()),
		ShowFieldEmoji: channel.ShowFieldEmoji,
		BaseURL:        s.baseURL,
		MaxInstances:   s.maxInstances,
	}
	msg, err := renderer.Render(ctx, view, opts)
	if err != nil {
		// §L.6: the payload is persisted alongside the failure, never silently
		// truncated, so a dead delivery can be debugged from the row.
		if len(msg.Payload) > 0 && msg.Fallback != "" {
			_ = s.deliveries.PersistRendered(ctx, scope, d.ID,
				msg.Payload, msg.Hash, msg.Fallback, now)
		}
		return s.fail(ctx, scope, d, channel, err, domain.ClassConfigInvalid, now)
	}

	// §G.7.4 coalescing: a root update whose bytes match what the card already
	// shows buys nothing. This is what turns a flapping alert's forty identical
	// updates into one send and thirty-nine visible `skipped` rows.
	if mode == domain.ModeUpdateRoot && d.ThreadID != nil {
		last, err := s.deliveries.LastRootHash(ctx, scope, *d.ThreadID)
		if err != nil {
			return outcome{}, err
		}
		if last != "" && last == msg.Hash {
			return outcome{}, s.abandon(ctx, scope, d,
				string(domain.SuppressedDuplicateRender), now)
		}
	}

	// BEFORE the network call, always.
	if err := s.deliveries.PersistRendered(ctx, scope, d.ID,
		msg.Payload, msg.Hash, msg.Fallback, now); err != nil {
		return outcome{}, err
	}

	target, err := s.open(ctx, scope, channel)
	if err != nil {
		class := domain.ClassConfigInvalid
		if errs.IsKind(err, errs.KindNotFound) {
			class = domain.ClassAuthExpired
		}
		return s.fail(ctx, scope, d, channel, err, class, now)
	}
	defer func() { _ = target.Close() }()

	// §H.6's `update_root + thread_reply` rows, delivered as one materialisation.
	// The amend goes FIRST so a reader who is drawn to the thread by the reply
	// finds a card that already agrees with it.
	if mode.IsReply() && th.RootLanded() && n.Reason.TouchesRoot() &&
		channel.Capabilities.Has(domain.CapAmend) {
		s.refreshRoot(ctx, scope, target, renderer, view, opts, d, now)
	}

	result, err := s.deliver(ctx, target, d, th, mode, msg)
	if err != nil {
		return s.fail(ctx, scope, d, channel, err, classOf(err), now)
	}

	return outcome{}, s.succeed(ctx, scope, d, th, channel, mode, result, now)
}

// refreshRoot amends the root card alongside a reply.
//
// ⚠ THE CONSTRAINT THIS EXISTS FOR. `deliveries_fanout_uniq UNIQUE
// (notification_id, channel_id)` allows exactly ONE delivery row per fact per
// destination, while §H.6 asks several Reasons for `update_root` AND
// `thread_reply`. Rather than drop one of them — a stale card, or a silent ack —
// both happen inside this one claim, under the same thread advisory lock, in the
// same transaction. The delivery row records the REPLY, because that is the
// message that is new; the amend is an edit of something the thread already
// remembers.
//
// It is deliberately best-effort. The reply is the authoritative half: if the
// amend fails, the reply still lands, and whatever killed the amend will kill
// the reply a moment later and be handled properly there. Failing the whole
// delivery because a cosmetic refresh did not take would turn a stale card into
// no message at all.
func (s *DispatchService) refreshRoot(
	ctx context.Context, scope db.TenantScope,
	target Target, renderer MessageRenderer,
	view *NotificationView, opts RenderOptions,
	d domain.Delivery, now time.Time,
) {
	rootOpts := opts
	rootOpts.Mode = RenderMode(domain.ModeUpdateRoot)

	msg, err := renderer.Render(ctx, view, rootOpts)
	if err != nil {
		s.log.WarnContext(ctx, "notification: could not render the root refresh",
			"delivery_id", d.ID, "error", err.Error())
		return
	}

	if d.ThreadID != nil {
		last, err := s.deliveries.LastRootHash(ctx, scope, *d.ThreadID)
		if err != nil {
			s.log.WarnContext(ctx, "notification: could not read the thread's last render hash",
				"delivery_id", d.ID, "error", err.Error())
			return
		}
		if last != "" && last == msg.Hash {
			// The card already says exactly this. An amend would be a call that
			// changes nothing, against a rate limit that is not free.
			return
		}
	}

	ref := MessageRef{}
	if th, err := s.threads.Get(ctx, scope, *d.ThreadID); err == nil {
		ref = MessageRef{
			ConversationID: th.ProviderConversationID,
			MessageID:      th.ProviderThreadID,
			ThreadID:       th.ProviderThreadID,
		}
	}
	if _, err := target.Amend(ctx, ref, msg); err != nil {
		s.log.WarnContext(ctx, "notification: the root refresh did not land; the reply still will",
			"delivery_id", d.ID, "error", err.Error(), "at", now)
	}
}

// effectiveMode RE-DERIVES the delivery mode against the thread as it actually
// stands, right now.
//
// The mode on the row was chosen when the intent was minted, and the world has
// moved since: a root may have landed, a root may have been lost, a thread may
// have reached the reply ceiling. Sending the stale mode is how a reply gets
// posted into nothing, or how a second root appears next to the first.
func (s *DispatchService) effectiveMode(
	d domain.Delivery, th domain.Thread, c domain.Channel,
) domain.Mode {
	mode := d.Mode

	// No thread at all: this destination only ever posts.
	if d.ThreadID == nil {
		return domain.ModePostRoot
	}

	switch {
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
	root := MessageRef{
		ConversationID: th.ProviderConversationID,
		MessageID:      th.ProviderThreadID,
		ThreadID:       th.ProviderThreadID,
	}

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

// succeed records the provider handle and advances the thread, in the SAME
// transaction as the send.
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

	if err := s.deliveries.MarkSent(ctx, scope, d.ID,
		messageID, conversationID, result.Raw, now); err != nil {
		return err
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
