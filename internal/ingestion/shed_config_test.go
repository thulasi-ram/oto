package ingestion

import (
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/platform/config"
)

// TestTheAcquisitionBudgetIsTheConfiguredOne.
//
// ⭐ THE TEST THAT WOULD HAVE CAUGHT THE ORIGINAL BUG. `db.ingest_acquire_timeout`
// was a documented, boot-validated setting — `.env.example` published it at
// 500 ms and §G.10 called it the ingest pool's acquisition budget — and it was
// read by nothing. pgxpool has no acquisition timeout of its own; `Acquire`
// waits on the caller's context and nothing else. The only place a webhook's
// wait is actually bounded is the shedder's gate, and that gate held a hardcoded
// literal, so an operator shortening the budget mid-incident shortened nothing
// and was told nothing.
//
// The setting now lives where its only consumer is (`ingest.acquire_timeout`)
// and this asserts it arrives there.
func TestTheAcquisitionBudgetIsTheConfiguredOne(t *testing.T) {
	cfg := config.Default().Ingest
	cfg.AcquireTimeout = 120 * time.Millisecond
	cfg.RetryAfter = 7 * time.Second

	got := shedConfig(cfg, 8)

	if got.Wait != 120*time.Millisecond {
		t.Fatalf("Wait = %v, want 120ms: the configured acquisition budget is ignored, "+
			"which is the whole defect", got.Wait)
	}
	if got.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", got.RetryAfter)
	}
	if got.MaxInFlight != 8 {
		t.Fatalf("MaxInFlight = %d, want 8 (the ingest pool size)", got.MaxInFlight)
	}
}

// TestTheDefaultAcquisitionBudgetIsUnchanged pins the shipped default, so that
// moving the key from `db.*` to `ingest.*` cannot quietly change behaviour for a
// deployment that never set it.
func TestTheDefaultAcquisitionBudgetIsUnchanged(t *testing.T) {
	if got := shedConfig(config.Default().Ingest, 4).Wait; got != 500*time.Millisecond {
		t.Fatalf("default Wait = %v, want 500ms", got)
	}
}

// TestAZeroBudgetFallsBackRatherThanAdmittingEverything.
//
// A zero Wait would make the gate non-blocking: every request that found the
// semaphore full would be shed immediately. Config validation forbids zero, but
// this function is also called from tests and from a zero-valued struct, and a
// silent behaviour change under a zero value is how a fallback becomes a bug.
func TestAZeroBudgetFallsBackRatherThanAdmittingEverything(t *testing.T) {
	if got := shedConfig(config.IngestConfig{}, 4).Wait; got != 500*time.Millisecond {
		t.Fatalf("zero-valued config gave Wait = %v, want the 500ms fallback", got)
	}
}
