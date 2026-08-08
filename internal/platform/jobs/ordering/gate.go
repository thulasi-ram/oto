package ordering

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// SkipReason records WHY a slot was advanced past without being sent. It is
// persisted by the Store as a `delivery.skipped` event, because the UI must show
// delivery state per alert — oto's silence must never be indistinguishable from
// "no alert" (SPEC §H.9).
type SkipReason string

// The closed SkipReason set.
const (
	// ReasonDeadDelivery: the delivery reached a terminal provider error.
	ReasonDeadDelivery SkipReason = "dead_delivery"
	// ReasonSkippedDelivery: the delivery was a coalesced no-op update.
	ReasonSkippedDelivery SkipReason = "skipped_delivery"
	// ReasonMissingDelivery: the seq was allocated but no delivery row exists.
	// The allocating transaction rolled back after `next_seq++`; the slot can
	// never be filled.
	ReasonMissingDelivery SkipReason = "missing_delivery"
	// ReasonThreadDead: the thread itself is terminal and nothing more will send.
	ReasonThreadDead SkipReason = "thread_dead"
	// ReasonAlreadySent: the slot WAS delivered and only the head had not caught
	// up. It is NOT a skip and NOT a gap: nothing was lost, so it must never be
	// recorded as `delivery.skipped` and must never raise the gap-recovery metric.
	// It exists as a distinct reason precisely so that neither happens.
	ReasonAlreadySent SkipReason = "already_sent"
)

// Anomalous reports whether this reason means something was NOT delivered.
// `oto_thread_gap_recovered_total` counts only these: a head catching up with a
// message that did land is convergence, not breakage, and counting it would make
// "sustained non-zero means a channel is broken" untrue.
func (r SkipReason) Anomalous() bool { return r != ReasonAlreadySent }

// Slot describes what occupies one thread_seq.
type Slot struct {
	// Present is false when no delivery row holds this seq.
	Present bool
	ItemID  uuid.UUID
	// Resolved is true for status in (sent, dead, skipped): the slot is finished
	// and the head may move past it.
	Resolved bool
	// Sent is true only for status='sent'. Resolved-but-not-Sent is the gap.
	Sent bool
	// Skipped is true only for status='skipped': a deliberate non-send, which is
	// not the same fact as a delivery that died and must not be labelled as one.
	Skipped bool
	// InFlight is true for status='sending': somebody claimed it and has not
	// finished. UpdatedAt bounds how long that is believable.
	InFlight  bool
	UpdatedAt time.Time
}

// Store is the persistence port the ordering gate needs. It is implemented by
// the notification module over `channel_threads` and `notification_deliveries`;
// this package declares it so platform never depends on a domain.
//
// Every method MUST run inside the caller's transaction (db.FromContext) and
// under the thread's advisory lock. A Store that reads outside the lock returns
// state that may already be false.
type Store interface {
	// LoadThread reads the ordering projection of one thread.
	LoadThread(ctx context.Context, threadID uuid.UUID) (Thread, error)
	// SlotAt describes the delivery occupying seq, if any.
	SlotAt(ctx context.Context, threadID uuid.UUID, seq int) (Slot, error)
	// Advance sets last_sent_seq to seq and records a delivery.skipped event.
	// itemID is uuid.Nil when the slot was empty.
	Advance(ctx context.Context, threadID uuid.UUID, seq int, itemID uuid.UUID, reason SkipReason) error
}

// Metrics is the ordering gate's Prometheus surface.
type Metrics struct {
	Decisions *prometheus.CounterVec
	Recovered *prometheus.CounterVec
	HeadWait  *prometheus.HistogramVec
}

// NewMetrics registers the ordering metrics on reg. A nil registry yields
// unregistered collectors.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_thread_order_decisions_total",
			Help: "Ordering-gate verdicts, by action and reason.",
		}, []string{"action", "reason"}),
		Recovered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "oto_thread_gap_recovered_total",
			Help: "Thread sequence slots advanced past without being sent, by reason. Sustained non-zero means a channel is broken.",
		}, []string{"reason"}),
		HeadWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "oto_thread_head_wait_seconds",
			Help:    "How long a delivery had been queued when the gate finally let it proceed.",
			Buckets: []float64{0.05, 0.25, 1, 2.5, 5, 15, 60, 300, 900},
		}, []string{"outcome"}),
	}
	if reg != nil {
		reg.MustRegister(m.Decisions, m.Recovered, m.HeadWait)
	}
	return m
}

// GateConfig configures a Gate.
type GateConfig struct {
	// Store is the persistence port. Required.
	Store Store
	// DB is the fallback querier used for the advisory lock when ctx carries no
	// transaction — in which case Claim fails, which is the point.
	DB db.Querier
	// Policy tunes the waits. The zero value means DefaultPolicy.
	Policy Policy
	// StaleClaimLease is how long a `sending` delivery is believed before it is
	// treated as abandoned. SPEC §G.5 puts it at 120 s.
	StaleClaimLease time.Duration

	Clock   clock.Clock
	Logger  *slog.Logger
	Metrics *Metrics
}

// DefaultStaleClaimLease is the SPEC §G.5 claim lease.
const DefaultStaleClaimLease = 120 * time.Second

// Gate serialises and orders sends within one thread.
//
// It is stateless: everything it knows comes from the row it reads under the
// lock. Two Gates in two pods therefore cannot disagree.
type Gate struct {
	store   Store
	q       db.Querier
	policy  Policy
	lease   time.Duration
	clock   clock.Clock
	log     *slog.Logger
	metrics *Metrics
}

// NewGate builds a Gate.
func NewGate(cfg GateConfig) (*Gate, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("ordering: a Store is required")
	}
	g := &Gate{
		store:   cfg.Store,
		q:       cfg.DB,
		policy:  cfg.Policy.normalise(),
		lease:   cfg.StaleClaimLease,
		clock:   cfg.Clock,
		log:     cfg.Logger,
		metrics: cfg.Metrics,
	}
	if g.lease <= 0 {
		g.lease = DefaultStaleClaimLease
	}
	if g.clock == nil {
		g.clock = clock.New()
	}
	if g.log == nil {
		g.log = slog.Default()
	}
	if g.metrics == nil {
		g.metrics = NewMetrics(nil)
	}
	return g, nil
}

// Lock takes the thread's transaction-scoped advisory lock, blocking until it is
// held. It MUST be called inside a transaction; the lock is released by COMMIT or
// ROLLBACK, so there is no unlock and no way to leak one.
//
// Blocking rather than try-locking is correct here: losing the race does not mean
// "somebody else already did this", it means "wait your turn", and a try-lock
// would turn a two-millisecond wait into a one-second snooze.
func (g *Gate) Lock(ctx context.Context, threadID uuid.UUID) error {
	return db.AdvisoryXactLock(ctx, db.FromContext(ctx, g.q), LockKey(threadID))
}

// Claim is the whole gate in one call: take the lock, read the thread under it,
// and decide.
//
// The returned Thread is the state as of INSIDE the lock, which is the only state
// a caller may act on. On ActionProceed the caller renders and sends, then — in
// the SAME transaction — records the provider ids and sets
// `last_sent_seq = item.Seq`. Committing the send and the sequence advance
// together is what makes the gate correct: a crash between them would either
// resend or wedge.
func (g *Gate) Claim(ctx context.Context, item Item) (Decision, Thread, error) {
	if err := g.Lock(ctx, item.ThreadID); err != nil {
		return Decision{}, Thread{}, err
	}

	th, err := g.store.LoadThread(ctx, item.ThreadID)
	if err != nil {
		return Decision{}, Thread{}, fmt.Errorf("ordering: load thread %s: %w", item.ThreadID, err)
	}

	now := g.clock.Now()
	d := Decide(item, th, now, g.policy)
	g.metrics.Decisions.WithLabelValues(string(d.Action), d.Reason).Inc()

	if d.Action == ActionProceed && !item.CreatedAt.IsZero() {
		g.metrics.HeadWait.WithLabelValues("proceed").Observe(now.Sub(item.CreatedAt).Seconds())
	}

	g.log.DebugContext(ctx, "ordering: gate decision",
		"thread_id", item.ThreadID, "delivery_id", item.ID, "thread_seq", item.Seq,
		"last_sent_seq", th.LastSentSeq, "thread_state", string(th.State),
		"action", string(d.Action), "reason", d.Reason)

	return d, th, nil
}

// Recovery reports what Recover did.
type Recovery struct {
	// Advanced is how many slots the head moved past.
	Advanced int
	// From and To are last_sent_seq before and after.
	From, To int
	// StalledAt and StalledItem name the slot recovery stopped on: the delivery
	// that now owns the head and is the only thing that can move it.
	//
	// THE CALLER MUST ACT ON THIS. Recovery deliberately refuses to skip the slot —
	// skipping a possibly-sent root would orphan every reply behind it — so the
	// head moves only when that delivery runs. The usual reason it has not is that
	// its job is gone: discarded past its attempt ceiling, cancelled by an
	// operator, or lost with the pod that held it. Re-enqueuing it is what turns
	// "wait and hope" into progress, and it is why these fields exist.
	//
	// StalledInFlight distinguishes a live claim from a `pending`/`failed` row. A
	// claim past its lease is the AMBIGUOUS case of SPEC §G.5: it may or may not
	// have reached the provider, and only the dispatcher may decide to re-send it.
	StalledAt       int
	StalledItem     uuid.UUID
	StalledInFlight bool
}

// Recover advances the thread's head past every finished-but-unsent slot.
//
// THIS IS THE LIVENESS HALF OF THE INVARIANT (SPEC §G.7.3). Sequence gating alone
// wedges permanently the first time a delivery can never complete; Recover is
// what guarantees that a poisoned message costs one skipped reply rather than a
// dead thread.
//
// It MUST run under the thread's advisory lock, inside a transaction. Call Lock
// first, or call it from a context that already holds the lock via Claim.
//
// It walks forward from `last_sent_seq + 1` and stops at the first slot that is
// still legitimately in play. It never skips over an unresolved slot: doing so
// would send a reply before the message it replies to.
func (g *Gate) Recover(ctx context.Context, threadID uuid.UUID) (Recovery, error) {
	th, err := g.store.LoadThread(ctx, threadID)
	if err != nil {
		return Recovery{}, fmt.Errorf("ordering: load thread %s: %w", threadID, err)
	}

	rec := Recovery{From: th.LastSentSeq, To: th.LastSentSeq}
	now := g.clock.Now()

	for seq := th.LastSentSeq + 1; seq < th.NextSeq; seq++ {
		slot, err := g.store.SlotAt(ctx, threadID, seq)
		if err != nil {
			return rec, fmt.Errorf("ordering: slot %d of thread %s: %w", seq, threadID, err)
		}

		reason, advance := g.classifySlot(slot, th, now)
		if !advance {
			// Report the blocking slot WHATEVER its status. Only reporting in-flight
			// claims left the commonest stall — a `pending` or `failed` row whose job
			// no longer exists — invisible to the one caller that could restart it.
			rec.StalledAt, rec.StalledItem, rec.StalledInFlight = seq, slot.ItemID, slot.InFlight
			break
		}

		if err := g.store.Advance(ctx, threadID, seq, slot.ItemID, reason); err != nil {
			return rec, fmt.Errorf("ordering: advance thread %s to %d: %w", threadID, seq, err)
		}

		if reason.Anomalous() {
			g.metrics.Recovered.WithLabelValues(string(reason)).Inc()
			g.log.WarnContext(ctx, "ordering: advanced past an unsent slot",
				"thread_id", threadID, "thread_seq", seq,
				"delivery_id", slot.ItemID, "reason", string(reason))
		} else {
			g.log.DebugContext(ctx, "ordering: head caught up with a slot that had already been sent",
				"thread_id", threadID, "thread_seq", seq, "delivery_id", slot.ItemID)
		}

		rec.Advanced++
		rec.To = seq
	}

	return rec, nil
}

// classifySlot decides whether the head may move past slot, and why.
func (g *Gate) classifySlot(slot Slot, th Thread, now time.Time) (SkipReason, bool) {
	switch {
	case th.State == StateDead:
		// Nothing will ever send on this thread again. Every remaining slot is
		// skipped so that the deliveries behind it become visibly skipped rather
		// than invisibly pending forever.
		return ReasonThreadDead, true

	case !slot.Present:
		// No row holds this seq. Either the allocating transaction rolled back
		// after taking the number, or it committed the allocation and never wrote
		// the row. Either way nothing will ever fill the slot — the number is not
		// returned to the pool — so advance immediately: waiting for a row that
		// cannot exist is exactly the wedge we are preventing.
		return ReasonMissingDelivery, true

	case slot.Sent:
		// Already sent; the head simply had not been advanced. Convergent, and NOT
		// a skip: labelling it one puts "oto skipped a delivery" on the timeline
		// for a message the destination is currently displaying.
		return ReasonAlreadySent, true

	case slot.Skipped:
		// A deliberate non-send — a coalesced no-op update, or a fact that arrived
		// after its thread was frozen.
		return ReasonSkippedDelivery, true

	case slot.Resolved:
		// dead: finished, and nothing reached the provider.
		return ReasonDeadDelivery, true

	case slot.InFlight && now.Sub(slot.UpdatedAt) <= g.lease:
		// Somebody is working it right now. Not our slot to move.
		return "", false

	default:
		// Either pending and waiting its turn, or in-flight past its lease. Both
		// belong to the dispatcher: a pending item will run, and an expired claim
		// is the AMBIGUOUS case of SPEC §G.5, which is re-sent with
		// `ambiguous = true` rather than skipped. Under-delivering a firing alert
		// is worse than a visible, labelled duplicate.
		//
		// Refusing to advance is only safe because Recovery reports the slot to the
		// caller, which re-enqueues it. Without that this branch is a wedge: it
		// declines to move and names nothing that would.
		return "", false
	}
}
