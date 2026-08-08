package ratelimit

import (
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
)

// newClock is a hand-wound clock, so refill behaviour is asserted without any
// test sleeping.
func newClock() *clock.Fake { return clock.NewFake(time.Unix(1_700_000_000, 0)) }

func TestBucketSpendsItsBurstThenRefuses(t *testing.T) {
	t.Parallel()

	clk := newClock()
	l := New(Config{Burst: 3, Refill: 10 * time.Second, Clock: clk})

	for i := range 3 {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("attempt %d was refused inside the burst", i+1)
		}
	}
	ok, retry := l.Allow("1.2.3.4")
	if ok {
		t.Fatal("the fourth attempt was allowed; the burst is 3")
	}
	if retry <= 0 {
		t.Fatal("a refusal must carry a Retry-After the caller can act on")
	}

	// A different address has its own budget: one noisy client must not lock out
	// everybody else's login.
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("a second address was refused on its first attempt")
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	t.Parallel()

	clk := newClock()
	l := New(Config{Burst: 2, Refill: 10 * time.Second, Clock: clk})

	l.Allow("k")
	l.Allow("k")
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("the bucket should be empty")
	}

	clk.Advance(10 * time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("one token should have refilled after one interval")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("only one token should have refilled")
	}
}

// TestIdlenessCannotBankMoreThanBurst is the classic token-bucket bug: a key that
// goes quiet for an hour must come back with Burst tokens, not with an hour's
// worth. Without the clamp, an attacker waits and then gets a free flood.
func TestIdlenessCannotBankMoreThanBurst(t *testing.T) {
	t.Parallel()

	clk := newClock()
	l := New(Config{Burst: 3, Refill: time.Second, TTL: time.Hour, Clock: clk})

	l.Allow("k")
	clk.Advance(time.Hour)

	allowed := 0
	for range 100 {
		if ok, _ := l.Allow("k"); ok {
			allowed++
			continue
		}
		break
	}
	if allowed != 3 {
		t.Fatalf("an idle key banked %d tokens; the burst is 3", allowed)
	}
}

// TestResetClearsTheBucket is what a SUCCESSFUL login does: somebody who mistyped
// their password four times must not be punished on the fifth attempt that works.
func TestResetClearsTheBucket(t *testing.T) {
	t.Parallel()

	clk := newClock()
	l := New(Config{Burst: 1, Refill: time.Minute, Clock: clk})

	l.Allow("k")
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("the bucket should be empty")
	}
	l.Reset("k")
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("Reset did not clear the bucket")
	}
}

// ⛔ TestFullTableFailsClosed. The key table is bounded, and reaching the bound is
// itself an attack signal. Admitting an untracked key at that moment would make
// the limiter disappear under exactly the load it exists for.
func TestFullTableFailsClosed(t *testing.T) {
	t.Parallel()

	clk := newClock()
	l := New(Config{Burst: 1, Refill: time.Minute, MaxKeys: 2, TTL: time.Hour, Clock: clk})

	l.Allow("a")
	l.Allow("b")
	if ok, _ := l.Allow("c"); ok {
		t.Fatal("a third key was admitted past MaxKeys; the limiter would evaporate under load")
	}
}

// TestIdleKeysAreSwept keeps the table from growing without bound on a busy but
// legitimate deployment.
func TestIdleKeysAreSwept(t *testing.T) {
	t.Parallel()

	clk := newClock()
	l := New(Config{Burst: 1, Refill: time.Second, TTL: time.Minute, Clock: clk})

	l.Allow("old")
	clk.Advance(2 * time.Minute)
	l.Allow("new")

	if n := l.Len(); n != 1 {
		t.Fatalf("tracked keys = %d, want 1 after the sweep", n)
	}
}

// ⭐ TestGateBoundsConcurrencyAndNeverBlocks is the memory bound. argon2id is
// charged per CONCURRENT evaluation at 19 MiB, so this is the number that decides
// whether an unauthenticated caller can exhaust the pod — and it must SHED rather
// than queue, because a queue in front of a 19 MiB allocation is the same memory
// held for longer.
func TestGateBoundsConcurrencyAndNeverBlocks(t *testing.T) {
	t.Parallel()

	g := NewGate(2)
	// Both calls must run: Acquire has a side effect, so this is "take the two
	// slots the gate has", not a redundant condition.
	first, second := g.Acquire(), g.Acquire()
	if !first || !second {
		t.Fatal("the gate refused inside its own capacity")
	}

	done := make(chan bool, 1)
	go func() { done <- g.Acquire() }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("the gate admitted a third caller past its capacity")
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire blocked; it must shed immediately")
	}

	g.Release()
	if !g.Acquire() {
		t.Fatal("Release did not return a slot")
	}
}
