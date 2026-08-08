package ratelimit

import (
	"math"
	"sync"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
)

// Defaults for the login limiter. Five failures then a slow drip is enough for a
// human who mistyped a password and far too little for anyone enumerating.
const (
	// DefaultBurst is how many attempts a fresh key may make back to back.
	DefaultBurst = 5
	// DefaultRefill is how long one token takes to come back.
	DefaultRefill = 12 * time.Second
	// DefaultTTL is how long an idle key is kept before the sweeper drops it. It
	// must exceed Burst*Refill or a key could be evicted while still in debt.
	DefaultTTL = 10 * time.Minute
	// DefaultMaxKeys bounds the table. Reaching it is itself an attack signal.
	DefaultMaxKeys = 50_000
)

// Config builds a Limiter. The zero value is usable and uses the defaults above.
type Config struct {
	// Burst is the bucket depth.
	Burst int
	// Refill is the time to regain one token.
	Refill time.Duration
	// TTL is how long an idle bucket is retained.
	TTL time.Duration
	// MaxKeys bounds the number of tracked keys.
	MaxKeys int
	// Clock is the time source. Nil means the system clock.
	Clock clock.Clock
}

func (c Config) normalise() Config {
	if c.Burst <= 0 {
		c.Burst = DefaultBurst
	}
	if c.Refill <= 0 {
		c.Refill = DefaultRefill
	}
	if c.TTL <= 0 {
		c.TTL = DefaultTTL
	}
	if fullRefill := time.Duration(c.Burst) * c.Refill; c.TTL < fullRefill {
		// An idle bucket evicted before it has refilled would hand a blocked key a
		// full burst back for free, which is the whole limit undone.
		c.TTL = fullRefill
	}
	if c.MaxKeys <= 0 {
		c.MaxKeys = DefaultMaxKeys
	}
	if c.Clock == nil {
		c.Clock = clock.New()
	}
	return c
}

// bucket is one key's token count, stored as the time at which it would next be
// completely full. Holding an instant rather than a float means there is no
// accumulated rounding error and no timer per key.
type bucket struct {
	full time.Time
	seen time.Time
}

// Limiter is an in-process token bucket keyed by an arbitrary string.
//
// ⚠️ IN-PROCESS IS A DELIBERATE, DOCUMENTED LIMITATION. Across N replicas a
// client gets N times the budget, which is still bounded and still turns an
// unlimited argon2id firehose into a trickle. A Postgres-backed bucket would put
// a write on the unauthenticated path — an attacker-controlled INSERT rate
// against the same database the alert pipeline needs — which trades one denial of
// service for a better one. If oto ever needs a shared budget it belongs behind a
// dedicated store, not in the general pool.
type Limiter struct {
	cfg Config

	mu      sync.Mutex
	buckets map[string]*bucket
	nextGC  time.Time
}

// New builds a Limiter.
func New(cfg Config) *Limiter {
	c := cfg.normalise()
	return &Limiter{cfg: c, buckets: make(map[string]*bucket)}
}

// Allow takes one token for key, reporting whether it was available and, when it
// was not, how long the caller must wait for the next one.
//
// The returned duration is what becomes `Retry-After`. It is derived from the
// bucket rather than fixed, so a caller that keeps hammering is told the truth
// rather than being invited back at a constant interval.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.cfg.Clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.cfg.MaxKeys {
			// The table is full, which means either an attack or a
			// misconfiguration. FAIL CLOSED: admitting an untracked key is how a
			// limiter with a bounded table becomes no limiter at all under exactly
			// the load it exists for.
			return false, ceilSeconds(l.cfg.Refill)
		}
		b = &bucket{full: now}
		l.buckets[key] = b
	}
	b.seen = now

	// `full` before now means the bucket is already brim-full. Clamping it to
	// `now` is what stops idleness banking tokens: without it, a key that goes
	// quiet for an hour comes back with an hour's worth of budget, which is a free
	// flood for anyone patient enough to wait.
	if b.full.Before(now) {
		b.full = now
	}

	// One token is available while the bucket would fill no later than
	// `now + (Burst-1)*Refill` — that is exactly "at least one of Burst tokens is
	// present right now".
	deadline := now.Add(time.Duration(l.cfg.Burst-1) * l.cfg.Refill)
	if b.full.After(deadline) {
		return false, ceilSeconds(b.full.Sub(deadline))
	}

	b.full = b.full.Add(l.cfg.Refill)
	return true, 0
}

// ceilSeconds rounds a wait up to a whole second. `Retry-After` is expressed in
// seconds, and truncating invites the client back before there is anything for
// it — which produces a second 429 and a client that concludes oto is broken.
func ceilSeconds(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	return time.Duration(math.Ceil(d.Seconds())) * time.Second
}

// Reset drops a key's bucket. It is called on a SUCCESSFUL login so that one
// person's typo streak does not cost them the next attempt after they get it
// right.
func (l *Limiter) Reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}

// Len reports how many keys are tracked. It exists for tests and metrics.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// sweep drops idle keys. It runs inline on a cadence rather than in a goroutine:
// a background janitor on a limiter that may never be used is a goroutine that
// definitely is not.
func (l *Limiter) sweep(now time.Time) {
	if now.Before(l.nextGC) {
		return
	}
	l.nextGC = now.Add(l.cfg.TTL / 2)
	cutoff := now.Add(-l.cfg.TTL)
	for k, b := range l.buckets {
		if b.seen.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}
