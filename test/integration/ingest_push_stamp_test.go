package integration

import (
	"testing"
	"time"
)

// TestAnAcceptedPushStampsLastPushAtOnBothArms is the accept transaction's half of
// `source_health.last_push_at`, seen from the webhook a customer's Alertmanager
// actually calls. `ingestion/repository.PushRepository` pins the SQL; this pins
// that `commitAccept` reaches it — on a fresh batch AND on the §C.5 duplicate arm,
// which returns early and whose stamp is the only write it makes. Nothing else
// asserts either call, so deleting one would leave the column NULL again, as it
// was on every source oto had ever had, with the whole suite green.
func TestAnAcceptedPushStampsLastPushAtOnBothArms(t *testing.T) {
	e := newIngestEnv(t, "push-stamp")

	lastPush := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := e.pool.QueryRow(e.ctx,
			`SELECT last_push_at FROM source_health WHERE source_id = $1`, e.sourceID).Scan(&at); err != nil {
			t.Fatalf("read last_push_at: %v", err)
		}
		return at
	}
	if at := lastPush(); at != nil {
		t.Fatalf("a source nobody has pushed to has last_push_at = %s; the stamp below proves nothing", at)
	}

	body := mustJSON(t, envelopeOf("push-stamp", bulkAlerts(1)))
	first := e.accept(t, body)
	if first.Data.Duplicate {
		t.Fatal("the first push was answered as a duplicate")
	}
	stamped := lastPush()
	if stamped == nil {
		t.Fatal("an accepted push left source_health.last_push_at NULL")
	}

	// Age the stamp past domain.PushStampEvery so the duplicate's stamp is not
	// throttled away, then send the SAME batch again.
	if _, err := e.pool.Exec(e.ctx, `UPDATE source_health SET last_push_at = last_push_at - interval '1 hour'
	                                  WHERE source_id = $1`, e.sourceID); err != nil {
		t.Fatalf("age last_push_at: %v", err)
	}
	aged := lastPush()

	again := e.accept(t, body)
	if !again.Data.Duplicate || again.Data.BatchID != first.Data.BatchID {
		t.Fatalf("the re-sent batch was not the §C.5 duplicate (duplicate=%v, batch %s vs %s)",
			again.Data.Duplicate, again.Data.BatchID, first.Data.BatchID)
	}
	if got := lastPush(); got == nil || !got.After(*aged) {
		t.Fatalf("a duplicate push left last_push_at at %v (aged to %s): an HA sibling's copy "+
			"still proves the source is reaching oto", got, aged)
	}
}
