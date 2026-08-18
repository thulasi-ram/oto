package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/service"
)

// ⭐⭐ THIS FILE IS ABOUT THE ONE LEGAL EXIT FROM `failed`, and about the reason
// the first attempt at it was BACKED OUT.
//
// §G.4 was built to be replayed — every write underneath is idempotent by
// construction (§G.5), and `ingest_batches` keeps the payload for exactly this —
// and yet `ingest.process_batch` was enqueued from precisely one place: the accept
// transaction that first received the body. A parser bug therefore cost every
// alert in every affected batch, permanently, with Alertmanager's retry budget
// already spent.
//
// ⛔ THE FIRST IMPLEMENTATION GATED ON AGE AND THE GATE PROTECTED AN EMPTY SET.
// It refused a batch older than the `ingest_dedup` horizon, on the theory that
// surviving dedupe keys make a re-append a no-op. But dedupe keys are
// CASE-scoped and a FAILED batch committed nothing, so it holds zero keys
// at any age whatsoever. Meanwhile the real harm sailed straight through: a batch
// that failed on Monday carrying `firing`, replayed on Thursday after Tuesday's
// batch resolved the alert, takes T7 — a BRAND NEW case id, which the
// dedupe key is keyed to and therefore misses by construction. New episode, new
// `case.opened`, fresh `notify.evaluate`. Somebody is paged for an incident
// that closed two days ago.
//
// So the gate is SUPERSESSION, and it has two limbs that no single comparison
// covers:
//
//	(a) OVERTAKEN — `source_updated_at` on the alert's latest case is after
//	    the batch's `received_at`. A later batch already wrote here.
//	(b) CLOSED — that case is terminal while the batch carries `firing`. It
//	    is a SEPARATE limb because reaper expiry closes an episode without any
//	    upstream saying so, and expiry NEVER touches `source_updated_at`: on the
//	    timestamp axis a reaper-closed alert looks untouched.
//
// ⚠️ EVERY BATCH SEEDED HERE IS ZERO-WRITE. That is not incidental. The backed-out
// attempt's own test processed a batch to completion and only then marked it
// failed, which is the one shape replay never has to worry about — and is why that
// test could not have caught any of this.

// ----------------------------------------------------------------- fixtures

// replayEnvelope is a v4 webhook body around one or more alerts.
//
// ⚠️ `groupKey` MUST BE UNIQUE PER CALL, for the reason envelopeOf gives:
// `batch_dedup_key` is computed from the meaning of the payload and not its
// bytes, so two bodies differing only in padding collapse onto one batch.
func replayEnvelope(groupKey string, alerts []map[string]any) map[string]any {
	return map[string]any{
		"version":         "4",
		"groupKey":        groupKey,
		"truncatedAlerts": 0,
		"status":          "firing",
		"receiver":        "oto",
		"groupLabels":     map[string]string{"alertname": "Replay"},
		"alerts":          alerts,
	}
}

// replayAlert is one wire alert at a given upstream status. `resolved` carries an
// `endsAt`, because an episode that ended without an end time is a claim oto does
// not make.
func replayAlert(labels map[string]string, status string) map[string]any {
	now := time.Now().UTC()
	endsAt := "0001-01-01T00:00:00Z"
	if status == "resolved" {
		endsAt = now.Format(time.RFC3339Nano)
	}
	return map[string]any{
		"status":      status,
		"labels":      labels,
		"annotations": map[string]string{"summary": "replay fixture"},
		"startsAt":    now.Add(-time.Hour).Format(time.RFC3339Nano),
		"endsAt":      endsAt,
	}
}

// pushAndProcess accepts a webhook and then runs the batch all the way through,
// so the alert and its episode exist the way the product made them rather than
// the way a fixture imagined them.
func (e *ingestEnv) pushAndProcess(t *testing.T, groupKey string, alerts []map[string]any) time.Time {
	t.Helper()

	raw, err := json.Marshal(replayEnvelope(groupKey, alerts))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	receipt := e.accept(t, raw)
	batchID, err := uuid.Parse(receipt.Data.BatchID)
	if err != nil {
		t.Fatalf("batch id: %v", err)
	}

	at := e.receivedAt(t, batchID)
	res, err := e.container.Ingestion.Service.ProcessBatch(e.ctx, e.scope, batchID, at)
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if res.Skipped {
		t.Fatalf("the seeding batch was skipped; the fixture built no alert")
	}
	return at
}

// seedFailedBatch writes a batch that FAILED HAVING WRITTEN NOTHING — the exact
// shape replay exists for, and the shape a fixture cannot reach by driving the
// pipeline, because a batch the pipeline fails has usually written something.
//
// It is raw SQL for that reason and for no other. `status = 'failed'` needs a
// reason (ingest_batches_err_ck) and a `processed_at` (ingest_batches_proc_ck),
// and the reason is the sentence the CLI prints back to the operator.
//
// ⛔ `receivedAt` MUST ALREADY BE IN THE PAST, and getting this wrong costs an
// hour. `ingest_batches_procts_ck` is `processed_at >= received_at`, and a batch
// that is replayed and then PROCESSED stamps `processed_at` with oto's clock at
// the moment it runs — so a fixture that dated its batch a second into the future
// to get the ordering it wanted makes the eventual close-out violate the check,
// and the failure surfaces as `ingest_process_failed: the batch could not be
// closed out`, three layers away from the timestamp that caused it. The real
// accept path can never produce this: `received_at` IS oto's clock at accept.
// Order these fixtures relative to `time.Now()`, never ahead of it.
func (e *ingestEnv) seedFailedBatch(
	t *testing.T, receivedAt time.Time, envelope map[string]any, failure string,
) uuid.UUID {
	t.Helper()

	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	batchID := uuid.New()
	sum := sha256.Sum256(raw)
	// `ingest_batches_dedup_ck` is `^[0-9a-f]{64}$`, and the value must be unique
	// per batch or `ingest_dedup` would have collapsed it upstream.
	dedup := sha256.Sum256([]byte(batchID.String()))

	alerts, _ := envelope["alerts"].([]map[string]any)
	if _, err := e.pool.Exec(e.ctx, `
INSERT INTO ingest_batches (id, org_id, source_id, mode, received_at, body_bytes, checksum,
                            dedup_key, group_key, receiver, alert_count, payload,
                            status, processed_at, error)
VALUES ($1, $2, $3, 'push', $4, $5, $6, $7, $8, 'oto', $9, $10::jsonb, 'failed', $11, $12)`,
		batchID, e.orgID, e.sourceID, receivedAt, len(raw), sum[:],
		hex.EncodeToString(dedup[:]), envelope["groupKey"], len(alerts), raw,
		receivedAt, failure,
	); err != nil {
		t.Fatalf("seed failed batch: %v", err)
	}
	return batchID
}

// receivedAt reads a batch's partition key, which the 202 receipt does not carry.
func (e *ingestEnv) receivedAt(t *testing.T, batchID uuid.UUID) time.Time {
	t.Helper()
	var at time.Time
	if err := e.pool.QueryRow(e.ctx,
		`SELECT received_at FROM ingest_batches WHERE org_id = $1 AND id = $2`,
		e.orgID, batchID).Scan(&at); err != nil {
		t.Fatalf("read received_at: %v", err)
	}
	return at
}

// batchStatus is the status and the failure reason as they stand now. A refusal
// has to leave BOTH untouched, which is a stronger claim than "it did not run".
func (e *ingestEnv) batchStatus(t *testing.T, batchID uuid.UUID) (string, string) {
	t.Helper()
	var (
		status  string
		failure *string
	)
	if err := e.pool.QueryRow(e.ctx,
		`SELECT status, error FROM ingest_batches WHERE org_id = $1 AND id = $2`,
		e.orgID, batchID).Scan(&status, &failure); err != nil {
		t.Fatalf("read batch status: %v", err)
	}
	if failure == nil {
		return status, ""
	}
	return status, *failure
}

// queuedReplays counts the `ingest.process_batch` jobs sitting in the outbox for
// one batch. The accept transaction already put one there, so every assertion
// below is about the DELTA — and a refusal's delta must be zero.
func (e *ingestEnv) queuedReplays(t *testing.T, batchID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'ingest.process_batch' AND args->>'batch_id' = $1`,
		batchID.String()).Scan(&n); err != nil {
		t.Fatalf("count queued jobs: %v", err)
	}
	return n
}

// expireCase closes the alert's latest episode the way the REAPER does:
// `expired` / `timeout`, with `source_updated_at` LEFT ALONE.
//
// ⭐ THAT LAST CLAUSE IS THE WHOLE TEST. Nothing upstream said anything, so there
// is no newer upstream timestamp to find — which is precisely why the overtaken
// limb cannot see this and a second limb has to exist.
// `endedAt` is passed rather than taken from `now()` so the assertion about it is
// arithmetic instead of a race with how fast the test ran.
func (e *ingestEnv) expireCase(t *testing.T, alertname string, endedAt time.Time) {
	t.Helper()
	tag, err := e.pool.Exec(e.ctx, `
UPDATE alert_cases o
   SET state = 'expired', resolve_reason = 'timeout', ended_at = $3
  FROM alerts a
 WHERE a.id = o.alert_id AND a.org_id = $1 AND a.alertname = $2`,
		e.orgID, alertname, endedAt)
	if err != nil {
		t.Fatalf("expire case: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expired %d cases, want exactly 1", tag.RowsAffected())
	}
}

// failAgain puts a processed batch back into `failed`, standing in for the second
// failure an operator would be reacting to. It is SQL because there is no other
// way to say "and then it broke again" without breaking something.
func (e *ingestEnv) failAgain(t *testing.T, batchID uuid.UUID, reason string) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, `
UPDATE ingest_batches SET status = 'failed', processed_at = now(), error = $3
 WHERE org_id = $1 AND id = $2`, e.orgID, batchID, reason); err != nil {
		t.Fatalf("re-fail batch: %v", err)
	}
}

// ------------------------------------------------------------------- limb (a)

// TestReplayRefusesAnOvertakenBatch is the first refusal limb: a later batch has
// already written to an alert this one names.
//
// Replaying would apply Monday's observation over Tuesday's, and Tuesday's is the
// one the customer's timeline is built from.
func TestReplayRefusesAnOvertakenBatch(t *testing.T) {
	t.Parallel()

	e := newIngestEnv(t, "replay-overtaken")
	labels := map[string]string{"alertname": "HighErrorRate", "severity": "critical", "service": "checkout"}

	// Monday: the alert exists and is firing.
	firstAt := e.pushAndProcess(t, "{}:{replay=\"overtaken-1\"}",
		[]map[string]any{replayAlert(labels, "firing")})

	// Tuesday: a later batch resolves it.
	secondAt := e.pushAndProcess(t, "{}:{replay=\"overtaken-3\"}",
		[]map[string]any{replayAlert(labels, "resolved")})

	// ⚠️ AND THE FAILED BATCH SITS BETWEEN THEM, which is the ordering the whole
	// case is about — so its `received_at` is computed from the other two rather
	// than from a literal offset that a fast test run could accidentally jump past.
	// It carries the same alert and it wrote NOTHING.
	failed := e.seedFailedBatch(t, firstAt.Add(secondAt.Sub(firstAt)/2),
		replayEnvelope("{}:{replay=\"overtaken-2\"}", []map[string]any{replayAlert(labels, "firing")}),
		"observations 0..1 could not be applied")

	before := e.queuedReplays(t, failed)

	res, err := e.container.Ingestion.Service.Replay(e.ctx, service.ReplayCommand{BatchID: failed})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if !res.Refused() {
		t.Fatalf("the replay was not refused; a stale observation would have been applied over a fresher one")
	}
	if res.AlertsTouched != 1 {
		t.Errorf("AlertsTouched = %d, want 1", res.AlertsTouched)
	}
	if len(res.Superseded) != 1 {
		t.Fatalf("Superseded = %d entries, want 1: %+v", len(res.Superseded), res.Superseded)
	}
	got := res.Superseded[0]
	if got.Limb != service.LimbOvertaken {
		t.Errorf("limb = %q, want %q", got.Limb, service.LimbOvertaken)
	}
	if got.Identity != "HighErrorRate{service=checkout}" {
		t.Errorf("identity = %q, want HighErrorRate{service=checkout}", got.Identity)
	}
	if !strings.HasPrefix(got.AlertKey, alertsdomain.AlertKeyPrefix) {
		t.Errorf("alert key = %q, want an %s… key", got.AlertKey, alertsdomain.AlertKeyPrefix)
	}

	// ⛔ NOTHING CHANGED. A refusal that had already reopened the batch, or
	// enqueued its job, would be a replay with a warning printed over it.
	if status, failure := e.batchStatus(t, failed); status != "failed" || failure == "" {
		t.Errorf("batch is %q with error %q, want it left failed with its reason", status, failure)
	}
	if after := e.queuedReplays(t, failed); after != before {
		t.Errorf("queued jobs went %d → %d; a refusal must enqueue nothing", before, after)
	}
}

// ------------------------------------------------------------------- limb (b)

// TestReplayRefusesAFiringBatchOverAClosedEpisode is the second refusal limb, and
// the one the age gate could never have expressed.
//
// The episode was closed by the REAPER, so `source_updated_at` still points at
// the day the alert last fired — BEFORE the failed batch was received. On the
// timestamp axis nothing has happened. Replay the batch anyway and `selectRule`
// sees `firing` on a terminal case, takes T7, and mints a new case id
// that the `alert_event_keys` dedupe cannot possibly match.
func TestReplayRefusesAFiringBatchOverAClosedEpisode(t *testing.T) {
	t.Parallel()

	e := newIngestEnv(t, "replay-closed")
	labels := map[string]string{"alertname": "PodCrashLooping", "severity": "warning", "namespace": "payments"}

	e.pushAndProcess(t, "{}:{replay=\"closed-1\"}",
		[]map[string]any{replayAlert(labels, "firing")})

	// After the first batch and before anything else — read off the wall clock
	// rather than offset from it, so it is behind `now()` when the batch is closed
	// out. See seedFailedBatch.
	failedAt := time.Now().UTC()
	failed := e.seedFailedBatch(t, failedAt,
		replayEnvelope("{}:{replay=\"closed-2\"}", []map[string]any{replayAlert(labels, "firing")}),
		"observations 0..1 could not be applied")

	// The reaper, not an upstream: the grace elapsed a minute after the batch was
	// received. `source_updated_at` is untouched and still predates the failed
	// batch, so limb (a) sees a perfectly quiet alert.
	e.expireCase(t, labels["alertname"], failedAt.Add(time.Minute))

	before := e.queuedReplays(t, failed)

	res, err := e.container.Ingestion.Service.Replay(e.ctx, service.ReplayCommand{BatchID: failed})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if !res.Refused() {
		t.Fatalf("the replay was not refused; T7 would have reopened a closed episode and paged for it")
	}
	if len(res.Superseded) != 1 {
		t.Fatalf("Superseded = %d entries, want 1: %+v", len(res.Superseded), res.Superseded)
	}
	got := res.Superseded[0]
	if got.Limb != service.LimbClosed {
		t.Fatalf("limb = %q, want %q — the overtaken limb cannot see a reaper-closed episode",
			got.Limb, service.LimbClosed)
	}
	if got.State != "expired" {
		t.Errorf("state = %q, want expired", got.State)
	}
	if !got.MovedAt.After(res.ReceivedAt) {
		t.Errorf("MovedAt = %s is not after the batch's %s; the refusal would print a date that predates the batch it refuses",
			got.MovedAt, res.ReceivedAt)
	}

	if status, _ := e.batchStatus(t, failed); status != "failed" {
		t.Errorf("batch is %q, want it left failed", status)
	}
	if after := e.queuedReplays(t, failed); after != before {
		t.Errorf("queued jobs went %d → %d; a refusal must enqueue nothing", before, after)
	}
}

// ------------------------------------------------------------- the happy path

// TestReplayEnqueuesWhenNothingHasMoved is the case the feature exists for: a
// batch that failed, an alert nobody has touched since, and a fix that has
// shipped.
//
// The batch must come back to `pending` — losing its `processed_at` and its stale
// failure reason with it — and the job must be in the SAME outbox the accept path
// enqueues through, in the same transaction as the status change.
func TestReplayEnqueuesWhenNothingHasMoved(t *testing.T) {
	t.Parallel()

	e := newIngestEnv(t, "replay-clean")
	labels := map[string]string{"alertname": "DiskFillingUp", "severity": "warning", "service": "storage"}

	e.pushAndProcess(t, "{}:{replay=\"clean-1\"}",
		[]map[string]any{replayAlert(labels, "firing")})

	// Behind `now()`, because this batch gets processed. See seedFailedBatch.
	failed := e.seedFailedBatch(t, time.Now().UTC(),
		replayEnvelope("{}:{replay=\"clean-2\"}", []map[string]any{replayAlert(labels, "firing")}),
		"observations 0..1 could not be applied")

	before := e.queuedReplays(t, failed)

	res, err := e.container.Ingestion.Service.Replay(e.ctx, service.ReplayCommand{BatchID: failed})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if res.Refused() {
		t.Fatalf("the replay was refused with nothing having moved: %+v", res.Superseded)
	}
	if !res.Enqueued {
		t.Fatal("the replay reported nothing enqueued")
	}
	if res.AlertsTouched != 1 {
		t.Errorf("AlertsTouched = %d, want 1", res.AlertsTouched)
	}

	status, failure := e.batchStatus(t, failed)
	if status != "pending" {
		t.Errorf("batch is %q, want pending — `failed` has exactly one exit and this is it", status)
	}
	if failure != "" {
		t.Errorf("batch still carries %q; a pending batch has not stopped for any reason", failure)
	}
	if after := e.queuedReplays(t, failed); after != before+1 {
		t.Errorf("queued jobs went %d → %d, want exactly one more", before, after)
	}

	// ⭐ AND THE JOB IT QUEUED ACTUALLY RUNS. `Reopen` is only worth anything if
	// ProcessBatch's own `Resumable` gate now lets the batch through; a reopen that
	// left it un-runnable would pass every assertion above and process nothing.
	done, err := e.container.Ingestion.Service.ProcessBatch(e.ctx, e.scope, failed, res.ReceivedAt)
	if err != nil {
		t.Fatalf("process the replayed batch: %v", err)
	}
	if done.Skipped {
		t.Fatal("the replayed batch was skipped; the reopen did not make it resumable")
	}
	if done.Observed != 1 {
		t.Errorf("observed %d alerts, want 1", done.Observed)
	}
}

// --------------------------------------------------------------- idempotence

// TestReplayTwiceDoesNotDuplicateRejections is the precondition without which
// this whole feature makes the product worse.
//
// `ingest_rejections` had no `ON CONFLICT` and minted a fresh uuidv7 per row per
// attempt, so every replay appended that batch's rejections to the feed AGAIN: a
// batch with forty rejections replayed twice showed a hundred and twenty. That is
// not a cosmetic duplicate — it is an operator counting the wrong number during
// the incident the feed exists to explain. The ids are derived from
// (batch, ordinal, reason) now, so a replayed row collides with itself exactly.
func TestReplayTwiceDoesNotDuplicateRejections(t *testing.T) {
	t.Parallel()

	e := newIngestEnv(t, "replay-rejections")
	good := map[string]string{"alertname": "QueueBacklog", "severity": "warning", "service": "ingest"}
	// One element over B5, which costs THAT ALERT and not the batch.
	bad := map[string]string{
		"alertname": "QueueBacklog",
		"severity":  "warning",
		"pod":       strings.Repeat("v", alertsdomain.MaxLabelValueBytes+1),
	}

	e.pushAndProcess(t, "{}:{replay=\"rej-1\"}",
		[]map[string]any{replayAlert(good, "firing")})

	// Behind `now()`, because this batch gets processed twice. See seedFailedBatch.
	failed := e.seedFailedBatch(t, time.Now().UTC(),
		replayEnvelope("{}:{replay=\"rej-2\"}", []map[string]any{
			replayAlert(good, "firing"),
			replayAlert(bad, "firing"),
		}),
		"observations 0..2 could not be applied")

	replayOnce := func(pass int) {
		t.Helper()
		res, err := e.container.Ingestion.Service.Replay(e.ctx, service.ReplayCommand{BatchID: failed})
		if err != nil {
			t.Fatalf("replay pass %d: %v", pass, err)
		}
		if res.Refused() {
			t.Fatalf("replay pass %d was refused: %+v", pass, res.Superseded)
		}
		if _, err := e.container.Ingestion.Service.ProcessBatch(e.ctx, e.scope, failed, res.ReceivedAt); err != nil {
			t.Fatalf("process pass %d: %v", pass, err)
		}
	}

	replayOnce(1)
	first := e.reasonsFor(t, failed.String())
	if total(first) == 0 {
		t.Fatal("the fixture recorded no rejections, so this test asserts nothing")
	}

	// The batch breaks a second time — a later parser change, or the same job
	// exhausting its retries — and the operator replays it again. This is also the
	// shape of the §G.6 retry that writes rejections before every chunking attempt.
	e.failAgain(t, failed, "observations 0..2 could not be applied")

	// ⭐ THE SECOND PASS IS THE TEST. Everything before it is setup.
	replayOnce(2)
	second := e.reasonsFor(t, failed.String())

	if total(second) != total(first) {
		t.Errorf("rejections went %d → %d across a replay; the feed now double-counts what this batch dropped",
			total(first), total(second))
	}
	for reason, n := range second {
		if first[reason] != n {
			t.Errorf("reason %q: %d → %d", reason, first[reason], n)
		}
	}
}

func total(counts map[string]int) int {
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}
