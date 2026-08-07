package service

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/streaming/domain"
)

// SSE tuning constants from SPEC §E.4 and openapi `streamEvents`. They are
// constants rather than configuration because they are part of the published
// contract: a client is entitled to assume a heartbeat every 15 s, and an
// operator who "tunes" that has changed the API.
const (
	// HeartbeatInterval is the `: ping` comment cadence.
	HeartbeatInterval = 15 * time.Second
	// CoalesceWindow is the frame-collapsing window: at most one frame per
	// connection per window per (kind, resource_id), latest wins.
	CoalesceWindow = 250 * time.Millisecond
	// DefaultBufferSize is the per-connection bound. Beyond it the connection
	// resyncs rather than the publisher blocking.
	//
	// 256 is roughly one storm-sized ingest batch of frames: big enough that a
	// browser doing normal work never overflows, small enough that a wedged tab
	// costs kilobytes and not megabytes.
	DefaultBufferSize = 256
)

// Batch is what a connection reads: a coalesced, seq-ordered group of events, or
// an instruction to resync.
type Batch struct {
	Events []domain.Event
	// Resync, when non-empty, means the client MUST refetch. Events is then
	// meaningless and is empty.
	Resync domain.ResyncReason
}

// Subscription is one SSE connection's view of the stream.
//
// Ownership: the Hub writes to `in`, the subscription's own pump goroutine reads
// `in` and writes `out`, and the HTTP handler reads `out`. Nobody else touches
// any of them, which is what keeps the fan-out lock-free on the hot path.
type Subscription struct {
	id       int64
	orgID    uuid.UUID
	interest domain.Interest

	in  chan domain.Event
	out chan Batch

	// resync is set by the Hub (under its own lock) when this connection
	// overflowed, and consumed by the pump.
	mu       sync.Mutex
	resync   domain.ResyncReason
	closeOne sync.Once
	done     chan struct{}
}

// OrgID is the tenant this subscription is pinned to. It comes from the
// authenticated principal and can never be widened by a query parameter.
func (s *Subscription) OrgID() uuid.UUID { return s.orgID }

// Batches is the connection's read side.
func (s *Subscription) Batches() <-chan Batch { return s.out }

// HubMetrics is the streaming hub's Prometheus surface, injected so that
// streaming never reaches for a global registry.
type HubMetrics struct {
	Connections     func(delta float64)
	Published       func(n int)
	Dropped         func(n int)
	Resync          func(reason string)
	CoalesceSkipped func(n int)
}

func (m HubMetrics) connections(d float64) {
	if m.Connections != nil {
		m.Connections(d)
	}
}
func (m HubMetrics) published(n int) {
	if m.Published != nil && n > 0 {
		m.Published(n)
	}
}
func (m HubMetrics) dropped(n int) {
	if m.Dropped != nil && n > 0 {
		m.Dropped(n)
	}
}
func (m HubMetrics) resync(r string) {
	if m.Resync != nil {
		m.Resync(r)
	}
}
func (m HubMetrics) coalesceSkipped(n int) {
	if m.CoalesceSkipped != nil && n > 0 {
		m.CoalesceSkipped(n)
	}
}

// HubConfig configures a Hub.
type HubConfig struct {
	// BufferSize bounds each connection. Zero means DefaultBufferSize.
	BufferSize int
	// CoalesceWindow overrides the frame-collapsing window. Zero means the
	// contract's 250 ms. Tests set it to zero-ish to flush immediately.
	CoalesceWindow time.Duration
	Logger         *slog.Logger
	Metrics        HubMetrics
}

// Hub fans durable UI events out to live SSE connections.
//
// THE ONE RULE: a publisher is never blocked by a subscriber. Every send onto a
// connection is non-blocking, and a connection that cannot keep up is dropped
// into resync rather than allowed to apply backpressure. Ingest latency must
// never be able to depend on a browser tab (SPEC §E.4).
type Hub struct {
	mu     sync.RWMutex
	subs   map[int64]*Subscription
	orgs   map[uuid.UUID]int
	nextID int64
	closed bool

	bufSize  int
	coalesce time.Duration
	log      *slog.Logger
	metrics  HubMetrics
}

// Compile-time proof that the Hub is what the bridge publishes into.
var _ Publisher = (*Hub)(nil)

// NewHub builds a Hub.
func NewHub(cfg HubConfig) *Hub {
	h := &Hub{
		subs:     map[int64]*Subscription{},
		orgs:     map[uuid.UUID]int{},
		bufSize:  cfg.BufferSize,
		coalesce: cfg.CoalesceWindow,
		log:      cfg.Logger,
		metrics:  cfg.Metrics,
	}
	if h.bufSize <= 0 {
		h.bufSize = DefaultBufferSize
	}
	if h.coalesce <= 0 {
		h.coalesce = CoalesceWindow
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	return h
}

// Subscribe registers a connection and starts its pump.
//
// It is deliberately callable BEFORE the caller replays from the database. That
// ordering is the whole resume guarantee: subscribing first means every event
// committed during the replay is buffered rather than lost, and the caller then
// discards anything at or below the last replayed seq to avoid a duplicate.
// Replay-then-subscribe would leave a hole exactly the width of the replay query.
func (h *Hub) Subscribe(orgID uuid.UUID, interest domain.Interest) *Subscription {
	s := &Subscription{
		orgID:    orgID,
		interest: interest,
		in:       make(chan domain.Event, h.bufSize),
		out:      make(chan Batch, 1),
		done:     make(chan struct{}),
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		s.finish()
		close(s.out)
		return s
	}
	h.nextID++
	s.id = h.nextID
	h.subs[s.id] = s
	h.orgs[orgID]++
	n := len(h.subs)
	h.mu.Unlock()

	h.metrics.connections(1)
	h.log.Debug("streaming: subscriber attached", "org_id", orgID, "subscribers", n)

	go h.pump(s)
	return s
}

// Unsubscribe detaches a connection and stops its pump. It is idempotent.
func (h *Hub) Unsubscribe(s *Subscription) {
	if s == nil {
		return
	}

	h.mu.Lock()
	if _, ok := h.subs[s.id]; ok {
		delete(h.subs, s.id)
		if n := h.orgs[s.orgID] - 1; n <= 0 {
			delete(h.orgs, s.orgID)
		} else {
			h.orgs[s.orgID] = n
		}
		h.metrics.connections(-1)
	}
	h.mu.Unlock()

	s.finish()
}

// Publish fans events out to every matching subscriber. It NEVER blocks.
//
// A subscriber whose buffer is full is marked for resync and its buffer is
// drained: the events it missed are unrecoverable from here, and telling it to
// refetch is both honest and cheap. Blocking instead would let one paused browser
// tab stall a database transaction.
func (h *Hub) Publish(events []domain.Event) {
	if len(events) == 0 {
		return
	}

	h.mu.RLock()
	targets := make([]*Subscription, 0, len(h.subs))
	for _, s := range h.subs {
		targets = append(targets, s)
	}
	h.mu.RUnlock()

	var sent, dropped int
	for _, s := range targets {
		for _, e := range events {
			// Org scoping is applied HERE and is not negotiable: a subscription
			// carries the org from its authenticated principal, and no interest
			// filter can widen past it.
			if e.OrgID != s.orgID || !s.interest.Matches(e) {
				continue
			}
			select {
			case s.in <- e:
				sent++
			default:
				dropped++
				h.overflow(s)
			}
		}
	}

	h.metrics.published(sent)
	h.metrics.dropped(dropped)
}

// Orgs lists the orgs with at least one live subscriber.
func (h *Hub) Orgs() []uuid.UUID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(h.orgs))
	for id := range h.orgs {
		out = append(out, id)
	}
	return out
}

// Subscribers is the current connection count, for /readyz and metrics.
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Shutdown detaches every connection. Handlers see their batch channel close and
// return, which ends the request cleanly instead of leaving the client to time out.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	h.closed = true
	subs := make([]*Subscription, 0, len(h.subs))
	for id, s := range h.subs {
		subs = append(subs, s)
		delete(h.subs, id)
	}
	h.orgs = map[uuid.UUID]int{}
	h.mu.Unlock()

	for _, s := range subs {
		h.metrics.connections(-1)
		s.finish()
	}
	h.log.Info("streaming: hub shut down", "detached", len(subs))
}

// overflow marks s for resync and drains its buffer.
//
// Draining is not merely tidy: the buffered events are now a PREFIX of a sequence
// the client will never complete, and delivering them would leave it confidently
// holding stale state. A resync says "everything you have is suspect", which is
// the truth.
func (h *Hub) overflow(s *Subscription) {
	s.mu.Lock()
	first := s.resync == ""
	if first {
		s.resync = domain.ResyncBufferOverflow
	}
	s.mu.Unlock()

	if !first {
		return
	}

	drained := 0
	for {
		select {
		case <-s.in:
			drained++
		default:
			h.metrics.coalesceSkipped(drained)
			h.metrics.resync(string(domain.ResyncBufferOverflow))
			h.log.Warn("streaming: subscriber overflowed, sending resync",
				"org_id", s.orgID, "dropped", drained)
			return
		}
	}
}

// pump coalesces a subscription's incoming events and hands them to the handler.
//
// COALESCING (SPEC §E.4): at most one frame per (kind, resource_id) per window,
// latest wins. A flapping alert can produce forty state changes a minute; the UI
// only ever renders the last one, so shipping the intermediate thirty-nine costs
// bandwidth and re-renders and buys nothing.
//
// Ordering within a flush is by seq, so the LAST frame a client sees always
// carries the HIGHEST seq of the flush. That is what keeps Last-Event-ID correct
// under coalescing: a superseded frame is never the one the cursor lands on.
func (h *Hub) pump(s *Subscription) {
	defer close(s.out)

	pending := map[coalesceKey]domain.Event{}
	ticker := time.NewTicker(h.coalesce)
	defer ticker.Stop()

	flush := func() bool {
		if len(pending) == 0 {
			return true
		}
		events := make([]domain.Event, 0, len(pending))
		for _, e := range pending {
			events = append(events, e)
		}
		clear(pending)
		sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })

		select {
		case s.out <- Batch{Events: events}:
			return true
		case <-s.done:
			return false
		}
	}

	for {
		// A pending resync outranks everything buffered: the buffered events are
		// part of the sequence the client is being told to abandon.
		if reason := s.takeResync(); reason != "" {
			clear(pending)
			select {
			case s.out <- Batch{Resync: reason}:
			case <-s.done:
				return
			}
			continue
		}

		select {
		case <-s.done:
			// Drain what is already buffered so a clean unsubscribe does not
			// silently discard frames the client could still have used.
			_ = flush()
			return

		case e, ok := <-s.in:
			if !ok {
				return
			}
			key := coalesceKey{kind: e.Kind, id: e.ResourceID}
			if prev, exists := pending[key]; exists && prev.Seq >= e.Seq {
				continue // out-of-order arrival; the newer one already won
			} else if exists {
				h.metrics.coalesceSkipped(1)
			}
			pending[key] = e

		case <-ticker.C:
			if !flush() {
				return
			}
		}
	}
}

type coalesceKey struct {
	kind domain.Kind
	id   uuid.UUID
}

func (s *Subscription) takeResync() domain.ResyncReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.resync
	s.resync = ""
	return r
}

// finish closes the subscription exactly once.
func (s *Subscription) finish() {
	s.closeOne.Do(func() { close(s.done) })
}

// Wait blocks until the subscription is detached or ctx ends. It exists so a
// handler can select on shutdown without reaching inside the Subscription.
func (s *Subscription) Wait(ctx context.Context) {
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}
