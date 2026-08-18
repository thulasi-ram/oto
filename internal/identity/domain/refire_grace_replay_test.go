package domain_test

import (
	"testing"
	"time"

	identity "github.com/thulasiram/oto/internal/identity/domain"
	ingest "github.com/thulasiram/oto/internal/ingestion/domain"
)

// ⛔⛔ THIS FILE IS THE TIE BETWEEN TWO CONSTANTS IN TWO MODULES THAT MUST NOT BE
// EQUAL, AND THEY WERE.
//
// `ingestion/domain.DedupTTL` is the §C.5 REPLAY window: how long an identical
// batch is treated as the same delivery arriving twice (an HA Alertmanager
// sibling, a retry after a 5xx). `refire_grace` is the §B.5 LIFECYCLE window:
// how long after a resolve a re-fire reopens the existing case (T8) rather
// than opening a new generation and a new Slack root message (T7).
//
// Both were ten minutes. A re-fire whose alert set has not changed produces a
// BYTE-IDENTICAL dedup key, so the two windows being equal made T8 unreachable
// by arithmetic:
//
//   - a re-fire inside `refire_grace` was also inside the replay window, and was
//     dropped at ingest before the state machine could see it;
//   - a re-fire the replay window let through was already outside the grace, and
//     opened a new generation.
//
// The first live tester had to alter the alert set — changing the dedup key — to
// exercise re-fire at all. Nobody noticed, because nothing connected the two
// numbers. This test is that connection.
//
// The modules do not import each other and must not: a settings vocabulary has no
// business depending on the ingest path. So the invariant lives here, in a test,
// which is the same idiom `test/integration/alert_identity_test.go` uses for the
// `severity`-in-`alert_key` invariant.

func TestTheReplayWindowIsStrictlyInsideRefireGrace(t *testing.T) {
	t.Parallel()

	bound, ok := identity.Bounds(identity.KeyRefireGrace)
	if !ok {
		t.Fatal("refire_grace_s has no bound; every integer setting must have one")
	}
	floor := time.Duration(bound.Min) * time.Second

	if floor <= ingest.DedupTTL {
		t.Fatalf("the re-fire grace floor (%s) does not clear the ingest replay window (%s): "+
			"every re-fire inside the grace is dropped as a duplicate delivery, so T8 is unreachable "+
			"and every re-fire opens a new Slack thread", floor, ingest.DedupTTL)
	}

	// ⭐ TWICE, NOT MERELY MORE. The reachable band — the interval in which a
	// re-fire is both VISIBLE to ingest and INSIDE the grace — is
	// `refire_grace - DedupTTL`. At `2 ×` that band is as wide as the window it has
	// to clear, so the feature has real room at the floor rather than a sliver.
	if want := 2 * ingest.DedupTTL; floor < want {
		t.Fatalf("the re-fire grace floor is %s; it must be at least 2 × the replay window (%s) "+
			"so the reachable band is as wide as the window it clears", floor, want)
	}
}

// The shipped default must itself be legal, and must sit at or above the floor.
// A default the server would refuse is a product that fails its own validation on
// a fresh install.
func TestTheShippedRefireGraceDefaultIsLegal(t *testing.T) {
	t.Parallel()

	bound, _ := identity.Bounds(identity.KeyRefireGrace)
	def := int(identity.DefaultRefireGrace / time.Second)

	if def < bound.Min || def > bound.Max {
		t.Fatalf("DefaultRefireGrace = %ds, outside its own bound %d..%d", def, bound.Min, bound.Max)
	}
	if time.Duration(def)*time.Second <= ingest.DedupTTL {
		t.Fatalf("the SHIPPED default (%ds) is inside the replay window (%s): "+
			"an out-of-the-box install cannot observe a re-fire at all", def, ingest.DedupTTL)
	}
}

// The replay window still has to do its own job. It must comfortably exceed an
// HA Alertmanager's gossip settling time and Alertmanager's own retry backoff,
// or oto starts recording an HA pair's two deliveries as two batches.
func TestTheReplayWindowStillCoversHAAndRetries(t *testing.T) {
	t.Parallel()

	// Three peers × the default 15s `cluster.peer-timeout`.
	const haSettle = 45 * time.Second
	// Alertmanager's notify retry backoff tops out around five minutes.
	const retryCeiling = 5 * time.Minute

	if ingest.DedupTTL < haSettle {
		t.Fatalf("DedupTTL %s is below the HA settling time %s", ingest.DedupTTL, haSettle)
	}
	if ingest.DedupTTL < retryCeiling {
		t.Fatalf("DedupTTL %s is below Alertmanager's retry ceiling %s: a retried batch "+
			"would be ingested twice", ingest.DedupTTL, retryCeiling)
	}
}
