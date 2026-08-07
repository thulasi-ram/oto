package clock

import (
	"sync"
	"time"
)

// Clock is the port every component reads time through, so that lifecycle and
// reaper behaviour is testable without sleeping.
//
// Note the two-timestamp rule (SPEC C12): occurred_at is the upstream claim and
// recorded_at is this clock. Never conflate them.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

// System is the real clock. It always returns UTC.
type System struct{}

// Now returns the current UTC time.
func (System) Now() time.Time { return time.Now().UTC() }

// Since returns the elapsed time since t.
func (System) Since(t time.Time) time.Duration { return time.Since(t) }

// New returns the system clock.
func New() Clock { return System{} }

// Fake is a manually advanced clock for tests.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake pinned at t (normalised to UTC).
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

// Now returns the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since returns the fake's elapsed time since t.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Advance moves the fake forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set pins the fake at t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}
