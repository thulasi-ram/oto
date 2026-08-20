package domain_test

import (
	"testing"
	"time"

	ingest "github.com/thulasiram/oto/internal/ingestion/domain"
)

// ⛔⛔ THIS FILE IS WHAT IS LEFT OF A TIE BETWEEN TWO CONSTANTS IN TWO MODULES,
// AND IT IS THE HALF THAT WAS NEVER THE DEPENDENT ONE.
//
// It lived in `internal/identity/domain/refire_grace_replay_test.go`, alongside
// two tests that pinned `refire_grace`'s floor against `DedupTTL`:
// `TestTheReplayWindowIsStrictlyInsideRefireGrace` asserted
// `MinRefireGraceSeconds >= 2 × DedupTTL`, and
// `TestTheShippedRefireGraceDefaultIsLegal` asserted the shipped default cleared
// the window. Both read the GRACE and measured it against this window; neither
// asserted anything about this window on its own. git-bug 7287b28 deleted
// `refire_grace_s`, `group_close_delay_s` and `MinRefireGraceSeconds` with them,
// so both tests lost their subject.
//
// ⭐ THE ONE BELOW DID NOT, AND MOVING IT IS THE POINT. `DedupTTL` is a TRANSPORT
// window whose floor comes entirely from Alertmanager — a three-peer gossip
// settling time and a notify retry backoff — so it is assertable without naming
// any product setting at all. It now lives beside the constant it guards rather
// than in the settings vocabulary, which is also where the layering wants it:
// `ingestion` needs no help from `identity` to state a property of its own
// ingest path, and `identity` no longer imports `ingestion` for anything.
//
// ⚠️ SO THE BOUND DID NOT SILENTLY LOSE ITS DERIVATION WHEN THE SETTING WENT.
// The derivation ran from Alertmanager to `DedupTTL` to the grace floor; only the
// last link was deleted. See the ⛔⛔ paragraph on `DedupTTL` for the equality
// trap that made the tie necessary in the first place, which is worth keeping
// legible even though the setting that sprang it is gone.

// The replay window has to do its own job. It must comfortably exceed an HA
// Alertmanager's gossip settling time and Alertmanager's own retry backoff, or
// oto starts recording an HA pair's two deliveries as two batches.
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
