package slack

import (
	"context"
	"sync"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
)

// Slack's posting limit is the binding constraint on oto's fan-out.
//
//   - chat.postMessage: "generally allows posting ONE MESSAGE PER SECOND PER
//     CHANNEL", with a workspace-wide ceiling and some burst tolerance.
//   - chat.update: Tier 3, 50+/minute, and NOT per-channel.
//
// That difference is a fifty-to-one cost gap, and it is the second reason
// update-in-place is the primary mechanism (the first being that it is better UX).
// An alert storm that fans out to one channel queues on the post bucket; the same
// storm expressed as amendments to one card does not queue at all.
const (
	postRatePerSecond   = 1.0
	postBurst           = 3.0
	updateRatePerSecond = 50.0 / 60.0
	updateBurst         = 10.0
)

// limiter is a per-conversation pair of token buckets.
//
// It is keyed by CONVERSATION, not by channel row or by token: Slack's limit is
// per channel per app, and two oto Channels pointed at the same Slack
// conversation share the same real budget. Getting that wrong means being
// throttled by Slack while oto believes it is within budget.
type limiter struct {
	mu     sync.Mutex
	clock  clock.Clock
	post   map[string]*bucket
	update map[string]*bucket
}

func newLimiter(clk clock.Clock) *limiter {
	if clk == nil {
		clk = clock.New()
	}
	return &limiter{
		clock:  clk,
		post:   make(map[string]*bucket),
		update: make(map[string]*bucket),
	}
}

// waitPost blocks until a chat.postMessage token is available for conversation,
// or ctx is done.
func (l *limiter) waitPost(ctx context.Context, conversation string) error {
	return l.get(l.post, conversation, postRatePerSecond, postBurst).wait(ctx, l.clock)
}

// waitUpdate blocks until a chat.update token is available.
func (l *limiter) waitUpdate(ctx context.Context, conversation string) error {
	return l.get(l.update, conversation, updateRatePerSecond, updateBurst).wait(ctx, l.clock)
}

func (l *limiter) get(m map[string]*bucket, key string, rate, burst float64) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := m[key]
	if !ok {
		b = &bucket{rate: rate, burst: burst, tokens: burst}
		m[key] = b
	}
	return b
}

// bucket is a token bucket that reads time through the injected clock, so its
// behaviour is testable without sleeping for real seconds.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

// wait consumes one token, blocking for the shortfall.
//
// The reservation is taken under the lock and the sleep happens outside it, so a
// queue of deliveries to one conversation is served in the order it arrived
// rather than in whatever order the scheduler wakes goroutines.
func (b *bucket) wait(ctx context.Context, clk clock.Clock) error {
	delay := b.reserve(clk.Now())
	if delay <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// reserve refills, takes a token, and returns how long the caller must wait for
// it. The token is deducted immediately even when the balance goes negative: that
// is what makes concurrent callers queue instead of all sleeping for the same
// short interval and then firing together.
func (b *bucket) reserve(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.last.IsZero() {
		b.last = now
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}

	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}
