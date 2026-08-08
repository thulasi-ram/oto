package ratelimit

import (
	"net"
	"net/http"
	"strings"
)

// Gate bounds how many requests may be INSIDE a handler at once.
//
// ⭐ IT IS THE MEMORY BOUND, and the limiter alone is not one. argon2id at 19 MiB
// is charged per CONCURRENT evaluation, not per request per second: a hundred
// clients each within their own budget still put a hundred evaluations in flight
// and 1.9 GiB of resident memory behind a request that carries no credential. The
// Gate caps the concurrent cost at a number an operator chose, and sheds the
// excess rather than queueing it — a queue in front of a 19 MiB allocation is the
// same memory, held for longer.
type Gate struct{ slots chan struct{} }

// DefaultConcurrency is how many password verifications may run at once. Sized
// so the worst case is tens of megabytes rather than gigabytes.
const DefaultConcurrency = 8

// NewGate builds a concurrency gate. A non-positive n means DefaultConcurrency.
func NewGate(n int) *Gate {
	if n <= 0 {
		n = DefaultConcurrency
	}
	return &Gate{slots: make(chan struct{}, n)}
}

// Acquire takes a slot without waiting, reporting whether one was free.
//
// ⛔ IT NEVER BLOCKS. Blocking would hold the request, its goroutine and its
// buffers for as long as the attacker liked, which is the resource exhaustion the
// gate exists to prevent.
func (g *Gate) Acquire() bool {
	if g == nil {
		return true
	}
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a slot.
func (g *Gate) Release() {
	if g == nil {
		return
	}
	select {
	case <-g.slots:
	default:
	}
}

// ClientKey is the rate-limiting identity of a request.
//
// ⛔ IT IS `RemoteAddr` AND NOTHING ELSE. `X-Forwarded-For` is a request header,
// which means it is attacker-controlled, which means a limiter keyed off it is a
// limiter an attacker resets by incrementing a number. oto does not know whether
// it is behind a trusted proxy — that is deployment knowledge it has no
// configuration for — so it keys off the only address it actually observed.
//
// The consequence is documented rather than hidden: behind a proxy that does not
// preserve the client address, every client shares one bucket and the limit is
// per-deployment. That is a weaker limit, not an absent one, and it fails in the
// safe direction.
func ClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return "unknown"
	}
	return host
}
