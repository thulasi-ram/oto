//go:build load

package load

import (
	"fmt"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/platform/config"
)

// ⭐⭐ THE TESTS THAT WOULD HAVE CAUGHT THE ORIGINAL DEFECT.
//
// `grouping/service` used to recompute the rollup ONCE PER JOINED MEMBER. A
// 500-alert Alertmanager batch therefore performed 500 full
// aggregates over one group plus 500 compare-and-set writes to the SAME
// `alert_groups` row, which the CAS then serialised — O(n) contention on one row,
// arriving at exactly the moment a customer's cluster is on fire and Alertmanager's
// ~5-minute retry budget is the only thing between a slow ingest and an alert that
// is lost silently (ADR 0007).
//
// `internal/grouping/service/joinmany_test.go` proves the new shape against
// counting fakes. These are its end-to-end counterpart: the same property, against
// a real Postgres, through the real HTTP route, the real River workers and a Slack
// double that enforces Slack's published contract. Under the old code
// `rollup_publishes_per_group` here would read ~500 instead of ~3.
//
// ⛔ THESE WERE `TestStorm*` AND THEY ASSERTED STORM COLLAPSE. ADR 0042 removed
// storm damping outright, so every claim of the form "oto withholds N replies
// because a group is busy" is gone. What survived is what was never damping in
// the first place, and it is the more valuable half: five hundred alerts share
// one `group_key`, so they open ONE generation, ONE Slack thread and ONE root
// card, and oto's chatter stays an order of magnitude below the alert count
// because a notification is minted per triggering change rather than per alert.

// ------------------------------------------------------------ the 500 batch

// TestBurstSingleBatchOf500Alerts is the case the issue names: at least five
// hundred alerts in ONE Alertmanager notification.
//
// Five hundred is exactly B17's ChunkSize, so it is ONE processing transaction —
// the shape a real node failure produces and the one the O(n) rollup punished
// hardest. The reason is the chunk size and NOT ChunkThreshold: chunking is
// unconditional, and the threshold governs only whether the batch is marked
// `partial` while the chunks run.
func TestBurstSingleBatchOf500Alerts(t *testing.T) {
	const alerts = 500

	e := newEnv(t, nil)
	d := newDriver(e)

	push := time.Now()
	d.post(batchSpec{Group: "NodeDown", Wave: 1, Alerts: alerts, Reason: "first notification"})
	pushFor := time.Since(push)

	drained := time.Now()
	e.drain(4 * time.Minute)
	drainFor := time.Since(drained)

	requireHarnessSanity(t, e)
	r := e.measure("single_batch_500", d, pushFor, drainFor)
	r.Note = "500 alerts in one Alertmanager notification, one group, one Slack thread."
	r.publish(t)

	assertBurstInvariants(t, e, r, invariantSpec{
		DistinctAlerts: alerts,
		Groups:         1,
		// One `group.upserted` for the generation opening and one for the single
		// material rollup this batch produces — plus slack for a periodic sweep
		// that recomputes. The defect this bounds would read 500.
		MaxRollupsPerGroup: 8,
	})

	// ⛔ THE THING THE PRODUCT IS FOR. Five hundred alerts must not become five
	// hundred Slack threads, or five hundred messages of any kind.
	if r.SlackRoots != 1 {
		t.Errorf("500 alerts opened %d Slack root messages, want 1 — this is the exact "+
			"failure oto exists to prevent", r.SlackRoots)
	}
	// ⭐ AND IT IS NOT DAMPING THAT BUYS THIS. One batch arriving at one group is
	// ONE triggering change, so it mints one notification and spends one or two
	// Slack calls on it — the same small constant it would spend on four alerts.
	// Storm collapse used to be credited with this bound; ADR 0042 removed storm
	// collapse and the bound did not move.
	if r.SlackCalls > 12 {
		t.Errorf("500 alerts cost %d Slack calls (%v); one batch into one group is one "+
			"triggering change and must cost a small constant, with no damping involved",
			r.SlackCalls, r.SlackByMethod)
	}
}

// ---------------------------------------------------------- B17 chunking

// TestBurstChunkedBatchExercisesB17 pushes a batch OVER `ChunkThreshold` (2000),
// so `applyChunks` splits it into `ChunkSize` (500) transactions and marks the
// batch `partial` until the last one commits.
//
// ⛔ THAT PATH HAD NEVER BEEN RUN. `partial` is a status a crashed worker resumes
// from, and a batch that ends its life there is an accepted 202 whose alerts were
// never applied. The invariant is that the batch reaches `processed` and that the
// per-chunk rollup stays O(chunks) rather than becoming O(alerts) again.
func TestBurstChunkedBatchExercisesB17(t *testing.T) {
	const alerts = 2200 // > domain.ChunkThreshold, so ceil(2200/500) = 5 chunks

	e := newEnv(t, nil)
	d := newDriver(e)

	push := time.Now()
	d.post(batchSpec{Group: "RackFailure", Wave: 1, Alerts: alerts, Reason: "first notification"})
	pushFor := time.Since(push)

	drained := time.Now()
	e.drain(8 * time.Minute)
	drainFor := time.Since(drained)

	requireHarnessSanity(t, e)
	r := e.measure("chunked_batch_2200", d, pushFor, drainFor)
	r.Note = "2200 alerts in one notification: over B17's 2000 threshold, so five chunked " +
		"transactions and a `partial` batch status on the way through."
	r.publish(t)

	assertBurstInvariants(t, e, r, invariantSpec{
		DistinctAlerts: alerts,
		Groups:         1,
		// One per chunk is the CORRECT shape here: each chunk is its own
		// transaction and its own `Recompute`. Five chunks, one opening, plus
		// slack.
		MaxRollupsPerGroup: 12,
	})

	if r.SlackRoots != 1 {
		t.Errorf("2200 alerts opened %d Slack root messages, want 1", r.SlackRoots)
	}
}

// --------------------------------------------------------- sustained burst

// The sustained shape. ⚠️ IT IS DELIBERATELY MODEST. The reference run was taken
// on a 2-CPU / 4 GB colima VM under concurrent load, and a load case that cannot
// finish on the machine a contributor actually has is a load case nobody runs.
const (
	sustainedGroups = 6
	// ⭐ THE FIRST WAVE IS DELIBERATELY SMALL AND IS ALLOWED TO SETTLE. It opens
	// six generations, posts six root cards and drains — so that every later wave
	// arrives at a thread that ALREADY HAS A ROOT, and §H.6 answers `update_root`
	// plus a reply rather than `post_root`. That is the amend-in-place path, and it
	// is the one that has to hold under volume: it is what keeps thirty batch
	// arrivals at six `chat.postMessage` calls with the rest `chat.update`.
	//
	// It used to be sized at 20 to sit below the 25-alert storm threshold. ADR 0042
	// deleted the threshold; the wave stays small because a settled opening is what
	// makes the burst measure amendment rather than creation.
	sustainedOpeningAlerts = 20
	sustainedBurstWaves    = 5
	sustainedAlertsPerWave = 100
	sustainedWaveGap       = 12 * time.Second
)

// TestBurstSustainedAcrossSeveralMinutes pushes several thousand alerts through
// several groups over a few minutes.
//
// It is the case the single batch cannot cover:
//
//   - MANY GROUPS ON ONE CHANNEL, which is the only condition under which "a burst
//     adds nothing to the channel" is falsifiable. Six busy generations share one
//     conversation; each must keep its own thread, and none of them may broadcast
//     out of that thread at any volume.
//   - REPEATED WAVES INTO A LIVE GENERATION, which is what puts hundreds of
//     deliveries behind one per-thread ordering gate and what would expose a
//     wedge.
//   - SUSTAINED CONCURRENCY on the ingest pool, which is where the shedder lives.
func TestBurstSustainedAcrossSeveralMinutes(t *testing.T) {
	waves := 1 + sustainedBurstWaves
	total := sustainedGroups * (sustainedOpeningAlerts + sustainedBurstWaves*sustainedAlertsPerWave)

	e := newEnv(t, nil)
	d := newDriver(e)

	names := make([]string, 0, sustainedGroups)
	for g := 0; g < sustainedGroups; g++ {
		names = append(names, fmt.Sprintf("SustainedBurst%02d", g))
	}

	push := time.Now()

	// Wave 1: quiet. Six generations open, six root cards land, and the run waits
	// for them — the burst below must arrive at threads that already have a root.
	opening := make([]batchSpec, 0, sustainedGroups)
	for _, name := range names {
		opening = append(opening, batchSpec{
			Group: name, Wave: 1, Alerts: sustainedOpeningAlerts, Reason: "first notification",
		})
	}
	d.wave(opening)
	e.drain(5 * time.Minute)

	// Waves 2..N: the burst.
	for wave := 2; wave <= waves; wave++ {
		specs := make([]batchSpec, 0, sustainedGroups)
		for _, name := range names {
			specs = append(specs, batchSpec{
				Group: name, Wave: wave, Alerts: sustainedAlertsPerWave, Reason: "new alerts added",
			})
		}
		d.wave(specs)
		if wave < waves {
			time.Sleep(sustainedWaveGap)
		}
	}
	pushFor := time.Since(push)

	drained := time.Now()
	e.drain(10 * time.Minute)
	drainFor := time.Since(drained)

	requireHarnessSanity(t, e)
	r := e.measure("sustained", d, pushFor, drainFor)
	r.Note = fmt.Sprintf(
		"%d alerts into ONE Slack channel: %d groups, one quiet opening wave of %d that is "+
			"allowed to settle, then %d burst waves of %d, %s apart.",
		total, sustainedGroups, sustainedOpeningAlerts,
		sustainedBurstWaves, sustainedAlertsPerWave, sustainedWaveGap)
	r.publish(t)

	assertBurstInvariants(t, e, r, invariantSpec{
		DistinctAlerts: total,
		Groups:         sustainedGroups,
		// One opening and one material rollup per wave — plus generous slack for
		// the periodic sweeps that also recompute.
		MaxRollupsPerGroup: 2*waves + 6,
	})

	// ⭐ ONE ROOT PER GROUP AND NOTHING PER ALERT. The shared assertions already
	// bound the total chatter and require that nothing was broadcast; what is
	// specific here is that thirty batch arrivals still produced ONE conversation
	// per incident rather than one per notification — and that this holds with no
	// damping anywhere in the path (ADR 0042).
	if r.SlackRoots != sustainedGroups {
		t.Errorf("%d Slack root messages for %d groups, want one each",
			r.SlackRoots, sustainedGroups)
	}
}

// ------------------------------------------------------------- the shedder

// TestBurstSheddingSays503NeverAnd429 forces the shedder to fire and proves the
// two things about it that matter.
//
// ⭐ SHEDDING IS A FEATURE (C17, ADR 0007) — but only because it is a 503. A 429
// is a 4xx, and Alertmanager deletes the notification permanently and silently for
// any 4xx, during exactly the window when the customer's cluster is on fire. The
// audit that raised this issue observed "40 requests, 40x202, no 429" and could
// not tell a working shedder from an absent one, because it never reached the
// gate.
//
// The gate is reached HERE by CONFIGURATION, not by volume: the ingest pool is
// taken to its floor and the acquisition budget to a millisecond, which is the
// documented knob (`ingest.acquire_timeout`, §G.10) and the thing an operator
// would lower during an incident. The claim proved is not "oto sheds at N
// requests per second" — that is a property of the machine — but "when oto sheds
// it answers 503 with a Retry-After, never a 4xx, and an Alertmanager-shaped
// retry recovers every alert".
func TestBurstSheddingSays503NeverA429(t *testing.T) {
	const (
		concurrent = 48
		perBatch   = 40
	)
	total := concurrent * perBatch

	e := newEnv(t, func(c *config.Config) {
		// The ingest pool at its §G.10 floor: 25 % of 8 is 2, so IngestMinConns
		// pins it at 4. MaxInFlight is the pool size, by construction.
		c.DB.MaxConns = 8
		// One millisecond of acquisition budget. Anything that cannot be admitted
		// immediately is shed rather than queued, which is the whole design.
		c.Ingest.AcquireTimeout = time.Millisecond
	})
	d := newDriver(e)

	specs := make([]batchSpec, 0, concurrent)
	for i := 0; i < concurrent; i++ {
		specs = append(specs, batchSpec{
			Group: "ShedBurst", Wave: i + 1, Alerts: perBatch, Reason: "new alerts added",
		})
	}

	push := time.Now()
	d.wave(specs)
	pushFor := time.Since(push)

	drained := time.Now()
	e.drain(8 * time.Minute)
	drainFor := time.Since(drained)

	requireHarnessSanity(t, e)
	r := e.measure("shedding_burst", d, pushFor, drainFor)
	r.Note = fmt.Sprintf(
		"%d concurrent batches of %d alerts against a 4-connection ingest pool with a "+
			"1 ms acquisition budget, retried Alertmanager-style.", concurrent, perBatch)
	r.publish(t)

	// ⛔ NOT ONE 4xx. This is the assertion the whole endpoint is built around.
	for code, n := range r.StatusCodes {
		switch code {
		case "202", "503":
		default:
			t.Errorf("the ingest endpoint answered %s x%d under load; only 202 and 503 are "+
				"permitted, because Alertmanager retries 5xx and ONLY 5xx (ADR 0007)", code, n)
		}
	}
	if r.StatusCodes["429"] > 0 {
		t.Error("the ingest endpoint answered 429 — a 429 is a 4xx and deletes the alert " +
			"at the upstream forever")
	}

	assertBurstInvariants(t, e, r, invariantSpec{
		DistinctAlerts: total,
		Groups:         1,
		// 48 batches into one group. Each is its own `Recompute`, so the bound is
		// per-batch and not per-alert: that is still O(batches x groups) and 1920
		// alerts produce at most ~50 rollups rather than 1920.
		MaxRollupsPerGroup: concurrent + 8,
	})

	// Whether the gate actually fired is a property of the machine, so it is
	// REPORTED rather than required. When it did fire, every shed batch must have
	// been recovered by the retry — which is what makes a 503 a promise rather
	// than a loss.
	if r.Shed503 == 0 {
		t.Logf("NOTE: the shedder did not fire on this machine (%d concurrent accepts "+
			"were all admitted). The 4xx assertions above still hold; the recovery "+
			"assertion was vacuous.", concurrent)
		return
	}
	t.Logf("the shedder fired %d times and %d retries recovered every one of them",
		r.Shed503, r.Retries)
}
