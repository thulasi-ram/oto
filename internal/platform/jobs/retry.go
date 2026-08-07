package jobs

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"time"

	"github.com/riverqueue/river/rivertype"

	"github.com/thulasiram/oto/internal/platform/clock"
)

// Backoff parameters from SPEC §G.6, `retryable`: exponential, base 2 s, factor 2,
// jitter ±50 %, cap 300 s.
const (
	// RetryBaseDelay is the first retry delay.
	RetryBaseDelay = 2 * time.Second
	// RetryFactor is the exponential growth factor.
	RetryFactor = 2.0
	// RetryMaxDelay caps the backoff. Five minutes is long enough to ride out a
	// Postgres failover or an Alertmanager restart and short enough that a
	// recovered dependency is noticed while the alert still matters.
	RetryMaxDelay = 300 * time.Second
	// RetryJitterFraction is the ±proportion applied to every delay.
	RetryJitterFraction = 0.5
)

// retryPolicy implements River's ClientRetryPolicy with oto's §G.6 schedule.
//
// Jitter is not decoration. Without it, a Postgres blip fails every in-flight job
// at the same instant, and they then retry at the same instant, forever, in a
// synchronised wave that keeps the database down. ±50 % breaks the convoy on the
// first retry.
type retryPolicy struct {
	clock clock.Clock
}

func newRetryPolicy(clk clock.Clock) *retryPolicy {
	if clk == nil {
		clk = clock.New()
	}
	return &retryPolicy{clock: clk}
}

// NextRetry returns when a failed job should next be attempted.
func (p *retryPolicy) NextRetry(job *rivertype.JobRow) time.Time {
	return p.clock.Now().Add(BackoffFor(job.Attempt))
}

// BackoffFor is the jittered §G.6 delay for a 1-based attempt number. It is
// exported so a test can assert the schedule without a queue.
func BackoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	d := float64(RetryBaseDelay) * math.Pow(RetryFactor, float64(attempt-1))
	if d > float64(RetryMaxDelay) || math.IsInf(d, 0) {
		d = float64(RetryMaxDelay)
	}

	// Scale by [1-f, 1+f).
	f := RetryJitterFraction
	return time.Duration(d * (1 - f + 2*f*randFraction()))
}

// randFraction returns a uniform float in [0,1).
//
// crypto/rand because math/rand and math/rand/v2 are banned repo-wide. The cost
// is a few hundred nanoseconds against a delay measured in seconds.
func randFraction() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failing CSPRNG is unrecoverable, but a retry schedule is not the
		// place to panic: fall back to the un-jittered midpoint.
		return 0.5
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11) / float64(uint64(1)<<53)
}
