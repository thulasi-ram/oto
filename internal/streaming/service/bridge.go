package service

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/streaming/domain"
)

// Bridge defaults.
const (
	// DefaultPollInterval is the reconciling poll cadence — the worst-case
	// latency when a NOTIFY is lost.
	DefaultPollInterval = 2 * time.Second
	// DefaultLagWindow is how far behind the read floor trails the published
	// watermark. See the commentary on lag below; it is the fix for the
	// commit-order race, and it is why 10 s rather than 0.
	DefaultLagWindow = 10 * time.Second
	// DefaultFetchLimit bounds one catch-up query.
	DefaultFetchLimit = 2_000
	// recentRingSize bounds the per-org de-duplication memory.
	recentRingSize = 8_192
)

// BridgeMetrics is the bridge's Prometheus surface, injected as functions so
// streaming never reaches for a global registry.
type BridgeMetrics struct {
	NotifyReceived  func()
	NotifyMalformed func()
	Reconnects      func()
	Fetched         func(n int)
	PollRecovered   func(n int)
	FetchErrors     func()
}

func (m BridgeMetrics) call(f func()) {
	if f != nil {
		f()
	}
}

// BridgeConfig configures a Bridge.
type BridgeConfig struct {
	Repo      EventRepository
	Publisher Publisher
	Listener  *db.Listener

	// PollInterval is the reconciling poll cadence. Zero means the default.
	PollInterval time.Duration
	// LagWindow is how far the read floor trails the watermark. Zero means the
	// default. Setting it to zero-length disables the race protection.
	LagWindow time.Duration
	// FetchLimit bounds one catch-up query. Zero means the default.
	FetchLimit int

	Clock   clock.Clock
	Logger  *slog.Logger
	Metrics BridgeMetrics
}

// Bridge carries committed `ui_events` rows from Postgres into the in-process Hub.
//
// # Why there are TWO mechanisms
//
// LISTEN/NOTIFY is fast and NOT DURABLE. A notification exists only while a
// connection is holding LISTEN: if the socket drops, or Postgres restarts, or the
// listener is mid-reconnect, every notification issued in that window is gone
// with no replay and no acknowledgement. Building only on NOTIFY means a client
// silently stops updating after a network blip and nobody finds out until
// somebody refreshes.
//
// The reconciling poll is slow and TOTAL. It re-reads `ui_events` from a
// watermark on a fixed cadence, so a dropped notification costs latency rather
// than correctness. It is not a fallback that switches on: it runs always, and
// NOTIFY is simply the thing that makes the common case fast.
//
// Together: NOTIFY sets the latency, the poll sets the guarantee.
//
// # Why the read floor LAGS the watermark
//
// `seq` is a BIGSERIAL, and a sequence value is taken at INSERT time while the
// row becomes visible at COMMIT time. Two concurrent transactions can therefore
// commit out of sequence order: transaction A takes seq 5, B takes seq 6, B
// commits first. A naive reader that advances its watermark to 6 will never see
// 5, and that event is lost forever with no error anywhere.
//
// So the catch-up query reads from a floor that trails the published watermark by
// LagWindow, and a bounded per-org ring of recently-published seqs suppresses the
// re-reads. The cost is re-scanning a few seconds of rows on each poll; the
// benefit is that an event committing late is picked up rather than skipped.
// This is the only correct fix that does not require exporting transaction
// snapshots into the application.
type Bridge struct {
	repo      EventRepository
	pub       Publisher
	listener  *db.Listener
	poll      time.Duration
	lagWindow time.Duration
	limit     int
	clock     clock.Clock
	log       *slog.Logger
	metrics   BridgeMetrics

	mu    sync.Mutex
	state map[uuid.UUID]*orgState
}

// orgState is one org's read position. Its own mutex serialises catch-up for that
// org, so the NOTIFY path and the poll path can never interleave into a double
// publish or a skipped range.
type orgState struct {
	mu sync.Mutex

	// watermark is the highest seq published for this org.
	watermark int64
	// floor is where the next catch-up reads from: the watermark as it was
	// roughly LagWindow ago.
	floor int64
	// pendingFloor is the value floor will take at the next rotation.
	pendingFloor int64
	rotatedAt    time.Time

	// recent suppresses re-publishing rows the lagged floor re-reads.
	recent map[int64]struct{}
	ring   []int64
	ringAt int
}

// NewBridge builds a Bridge.
func NewBridge(cfg BridgeConfig) *Bridge {
	b := &Bridge{
		repo:      cfg.Repo,
		pub:       cfg.Publisher,
		listener:  cfg.Listener,
		poll:      cfg.PollInterval,
		lagWindow: cfg.LagWindow,
		limit:     cfg.FetchLimit,
		clock:     cfg.Clock,
		log:       cfg.Logger,
		metrics:   cfg.Metrics,
		state:     map[uuid.UUID]*orgState{},
	}
	if b.poll <= 0 {
		b.poll = DefaultPollInterval
	}
	if b.lagWindow <= 0 {
		b.lagWindow = DefaultLagWindow
	}
	if b.limit <= 0 {
		b.limit = DefaultFetchLimit
	}
	if b.clock == nil {
		b.clock = clock.New()
	}
	if b.log == nil {
		b.log = slog.Default()
	}
	return b
}

// Run drives both mechanisms until ctx ends. It returns only when both have
// stopped, so a caller can treat it as the bridge's whole lifetime.
func (b *Bridge) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	if b.listener != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.listener.Run(ctx, b.onNotification); err != nil {
				b.log.Error("streaming: listener stopped", "error", err)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		b.runPoll(ctx)
	}()

	wg.Wait()
	return nil
}

// OnListenerReconnect is the hook to pass as db.ListenerOptions.OnConnect.
//
// Every notification issued while the socket was down is unrecoverable, so a
// reconnect is followed immediately by a full catch-up rather than waiting up to
// PollInterval for the poll to notice.
func (b *Bridge) OnListenerReconnect(ctx context.Context) {
	b.metrics.call(b.metrics.Reconnects)
	b.catchUpAll(ctx)
}

// onNotification handles one `<org_id>:<seq>` doorbell.
func (b *Bridge) onNotification(ctx context.Context, n db.Notification) {
	b.metrics.call(b.metrics.NotifyReceived)

	orgID, _, ok := parseNotifyPayload(n.Payload)
	if !ok {
		b.metrics.call(b.metrics.NotifyMalformed)
		b.log.Warn("streaming: malformed notify payload", "payload", n.Payload)
		return
	}

	// The seq in the payload is deliberately IGNORED as a read position. It is a
	// doorbell, not a cursor: reading "everything above my lagged floor" is both
	// simpler and strictly more correct than trusting one seq, because it also
	// sweeps up anything a lost notification would have announced.
	if !b.watching(orgID) {
		return
	}
	b.catchUp(ctx, orgID)
}

// runPoll is the reconciling loop.
func (b *Bridge) runPoll(ctx context.Context) {
	t := time.NewTicker(b.poll)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.catchUpAll(ctx)
			b.forgetIdleOrgs()
		}
	}
}

func (b *Bridge) catchUpAll(ctx context.Context) {
	for _, orgID := range b.pub.Orgs() {
		if ctx.Err() != nil {
			return
		}
		b.catchUp(ctx, orgID)
	}
}

// watching reports whether any connection cares about this org. There is no point
// querying for a tenant nobody is looking at, and a NOTIFY storm from a busy org
// with no viewers must cost nothing.
func (b *Bridge) watching(orgID uuid.UUID) bool {
	for _, id := range b.pub.Orgs() {
		if id == orgID {
			return true
		}
	}
	return false
}

// catchUp reads and publishes everything new for one org.
func (b *Bridge) catchUp(ctx context.Context, orgID uuid.UUID) {
	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		return
	}

	st, seeded, err := b.stateFor(ctx, scope)
	if err != nil {
		b.metrics.call(b.metrics.FetchErrors)
		b.log.WarnContext(ctx, "streaming: could not seed org watermark", "org_id", orgID, "error", err)
		return
	}
	if seeded {
		// A newly-watched org starts at the current ceiling. Publishing the whole
		// retained window instead would replay twenty-four hours into every tab
		// that connects — which is what the handler's own resume path is for.
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	now := b.clock.Now()
	cutoff := now.Add(-domain.ReplayWindow)
	from := st.floor

	var published, recovered int
	for {
		events, err := b.repo.ListSince(ctx, scope, from, cutoff, b.limit)
		if err != nil {
			b.metrics.call(b.metrics.FetchErrors)
			b.log.WarnContext(ctx, "streaming: catch-up read failed", "org_id", orgID, "error", err)
			return
		}
		if len(events) == 0 {
			break
		}

		fresh := make([]domain.Event, 0, len(events))
		for _, e := range events {
			if _, seen := st.recent[e.Seq]; seen {
				continue
			}
			st.remember(e.Seq)
			if e.Seq > st.watermark {
				st.watermark = e.Seq
			} else {
				// Below the watermark and not yet seen: this is precisely the
				// late-committing row the lag window exists to catch.
				recovered++
			}
			fresh = append(fresh, e)
		}

		if len(fresh) > 0 {
			b.pub.Publish(fresh)
			published += len(fresh)
		}

		if len(events) < b.limit {
			break
		}
		from = events[len(events)-1].Seq
	}

	st.rotate(now, b.lagWindow)

	if published > 0 && b.metrics.Fetched != nil {
		b.metrics.Fetched(published)
	}
	if recovered > 0 {
		if b.metrics.PollRecovered != nil {
			b.metrics.PollRecovered(recovered)
		}
		b.log.InfoContext(ctx, "streaming: recovered late-committing events",
			"org_id", orgID, "count", recovered)
	}
}

// stateFor returns the org's read position, seeding it on first sight.
func (b *Bridge) stateFor(ctx context.Context, scope db.TenantScope) (*orgState, bool, error) {
	orgID := scope.OrgID()

	b.mu.Lock()
	st, ok := b.state[orgID]
	b.mu.Unlock()
	if ok {
		return st, false, nil
	}

	now := b.clock.Now()
	_, maxSeq, _, err := b.repo.SeqBounds(ctx, scope, now.Add(-domain.ReplayWindow))
	if err != nil {
		return nil, false, err
	}

	st = &orgState{
		watermark:    maxSeq,
		floor:        maxSeq,
		pendingFloor: maxSeq,
		rotatedAt:    now,
		recent:       make(map[int64]struct{}, recentRingSize),
		ring:         make([]int64, recentRingSize),
	}

	b.mu.Lock()
	if existing, raced := b.state[orgID]; raced {
		st = existing
		b.mu.Unlock()
		return st, false, nil
	}
	b.state[orgID] = st
	b.mu.Unlock()

	return st, true, nil
}

// forgetIdleOrgs releases the read position of orgs nobody is watching, so a
// long-lived pod does not accumulate one ring buffer per tenant that ever
// connected.
func (b *Bridge) forgetIdleOrgs() {
	live := map[uuid.UUID]struct{}{}
	for _, id := range b.pub.Orgs() {
		live[id] = struct{}{}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.state {
		if _, ok := live[id]; !ok {
			delete(b.state, id)
		}
	}
}

// remember records a published seq in the bounded ring, evicting the oldest.
func (s *orgState) remember(seq int64) {
	if old := s.ring[s.ringAt]; old != 0 {
		delete(s.recent, old)
	}
	s.ring[s.ringAt] = seq
	s.ringAt = (s.ringAt + 1) % len(s.ring)
	s.recent[seq] = struct{}{}
}

// rotate moves the read floor forward once the lag window has elapsed. The floor
// therefore always trails the watermark by between one and two lag windows, which
// is the guarantee that a transaction committing late is still inside the range
// the next catch-up reads.
func (s *orgState) rotate(now time.Time, lag time.Duration) {
	if now.Sub(s.rotatedAt) < lag {
		return
	}
	s.floor = s.pendingFloor
	s.pendingFloor = s.watermark
	s.rotatedAt = now
}

// parseNotifyPayload splits `<org_id>:<seq>`.
func parseNotifyPayload(p string) (uuid.UUID, int64, bool) {
	i := strings.LastIndexByte(p, ':')
	if i <= 0 {
		return uuid.Nil, 0, false
	}
	orgID, err := uuid.Parse(p[:i])
	if err != nil {
		return uuid.Nil, 0, false
	}
	seq, err := strconv.ParseInt(p[i+1:], 10, 64)
	if err != nil {
		return uuid.Nil, 0, false
	}
	return orgID, seq, true
}
