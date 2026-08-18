package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// ReplayCommand is one operator asking for a stored batch to be processed again,
// after shipping the fix that stops it failing.
type ReplayCommand struct {
	// BatchID is all the operator has. The org and the partition key are
	// discovered from it — see BatchRepository.Locate.
	BatchID uuid.UUID
	// Force bypasses the SUPERSESSION GATE and nothing else. It does not bypass
	// the status gate and it cannot conjure a payload that has aged out of its
	// partition. See Replay for why it exists at all.
	Force bool
}

// SupersessionLimb names WHICH of the two ways an alert moved on without this
// batch. They are different failures and they read differently to a human.
type SupersessionLimb string

const (
	// LimbOvertaken is a later batch having already written to this alert:
	// `source_updated_at` on its latest case is after the replayed batch's
	// `received_at`. Replaying would apply a stale observation over a fresher one.
	LimbOvertaken SupersessionLimb = "overtaken"
	// LimbClosed is the alert's latest case being TERMINAL — resolved or
	// expired — while this batch carries `firing` for it.
	//
	// ⭐ THIS IS THE T7/T8 DOUBLE-WRITE, and it is the limb the first attempt at
	// this feature missed. `selectRule` sees a firing observation on a terminal
	// case and takes `refireAfterGrace`, which MINTS A NEW CASE ID —
	// so the `alert_event_keys` dedupe, which is keyed to the case, is a
	// guaranteed MISS. The result is a new episode, a new `case.opened` and
	// a fresh `notify.evaluate`: somebody is paged for an incident that closed two
	// days ago.
	//
	// ⛔ IT CANNOT BE EXPRESSED AS A TIMESTAMP COMPARISON, which is why it is a
	// second limb rather than a wider window on the first. The reaper expires an
	// case with nothing upstream saying so, and expiry never touches
	// `source_updated_at` — so a reaper-closed alert is invisible to LimbOvertaken
	// and is exactly as dangerous.
	LimbClosed SupersessionLimb = "closed"
)

// Supersession is one alert that moved on after the batch was received, and the
// reason a replay of that batch is refused.
type Supersession struct {
	Limb SupersessionLimb
	// AlertKey is the §C.2 identity, full. The CLI abbreviates it; the value here
	// is the one you can paste into a query.
	AlertKey string
	// Identity is the alert rendered for a human, e.g. `HighErrorRate{service=checkout}`.
	Identity string
	// State is the latest case's state, rendered.
	State string
	// MovedAt is when it last moved. See AlertState.MovedAt for why this is not
	// simply `source_updated_at`.
	MovedAt time.Time
}

// ReplayResult is what one `oto replay` did, or refused to do.
//
// ⛔ A REFUSAL IS A RESULT, NOT AN ERROR. It is the expected outcome for exactly
// the batches this feature is most likely to be pointed at — old ones — and it
// carries a list the operator has to read. An error would carry a string.
type ReplayResult struct {
	BatchID    uuid.UUID
	ReceivedAt time.Time
	// Status is the status the batch was in when the replay looked at it.
	Status domain.Status
	// Failure is `ingest_batches.error` verbatim: the reason it stopped, which is
	// the sentence that tells the operator whether their fix was the right one.
	Failure string
	// AlertsTouched is how many DISTINCT alerts the stored payload names. It is the
	// denominator of "3 of 40 alerts moved".
	AlertsTouched int
	// Superseded is every alert that moved after ReceivedAt, in payload order.
	// Non-empty with Enqueued false is a refusal; non-empty with Enqueued true is
	// a --force.
	Superseded []Supersession
	// Forced is true when the supersession gate was bypassed.
	Forced bool
	// Enqueued is true when the batch was reopened and its job queued.
	Enqueued bool
}

// Refused reports whether this result changed nothing because alerts had moved.
func (r ReplayResult) Refused() bool { return len(r.Superseded) > 0 && !r.Enqueued }

// Replay re-enqueues `ingest.process_batch` for a batch that failed, after a
// human has decided it is safe.
//
// ⭐⭐ IT EXISTS BECAUSE THE PROCESSING PATH WAS BUILT TO BE REPLAYED AND NOTHING
// COULD TRIGGER A REPLAY. Every write underneath is idempotent by construction
// (§G.5) and `ingest_batches` retains the payload for exactly this, and yet
// `ingest.process_batch` was enqueued from ONE place — the accept transaction
// that first received the body. A parser bug therefore cost every alert in every
// affected batch, permanently, with Alertmanager's retry budget already spent:
// the precise loss `ingest_rejections` and the 202-never-4xx rule exist to
// prevent, arriving through the one door nobody had put a handle on.
//
// ⛔ IT IS A SUBCOMMAND AND NOT A ROUTE. It is an operator recovery action taken
// after a code fix ships, and it crosses the org boundary the API is scoped by —
// whoever fixed the parser does not know which org's batches broke, which is the
// same reason Locate exists. A tenant must never be able to re-enqueue work onto
// the ingest pool from outside.
//
// ⭐ THE SUPERSESSION GATE IS THE SAFETY ARGUMENT, and it replaced a WRONG one.
// The first attempt gated on AGE: refuse a batch older than the `ingest_dedup`
// horizon, on the theory that surviving dedupe keys make a re-append a no-op.
// That gate protects an empty set. Dedupe keys are CASE-scoped
// (`alerts/domain.lifecycle`, `"case:"+id+":opened"`) and a FAILED batch committed
// nothing, so it holds zero keys at any age whatsoever. The real risk is not
// being old; it is being OVERTAKEN — and the only way to know is to work out
// which alerts the payload names and go and look at them.
//
// ⛔ NOTHING IS WRITTEN BEFORE THE GATE PASSES. The touched set is computed by
// running `plan`, which is the read-only prefix of processing: decode, source
// config, normalise. Not one rejection row, not one status change. A refusal
// leaves the batch exactly as it found it.
func (s *Service) Replay(ctx context.Context, cmd ReplayCommand) (ReplayResult, error) {
	if cmd.BatchID == uuid.Nil {
		return ReplayResult{}, errs.New(errs.KindValidation, CodeReplayRefused,
			"a replay names one batch")
	}
	if s.alertStates == nil {
		// ⛔ NEVER "no reader, no gate". A replay that cannot ask whether alerts
		// moved is a replay that cannot be shown to be safe, and running it anyway
		// would page someone for a closed incident on the strength of a nil check.
		return ReplayResult{}, errs.New(errs.KindInternal, CodeReplayRefused,
			"no alert state reader is wired; a replay cannot be checked and will not be run")
	}

	orgID, receivedAt, err := s.batches.Locate(ctx, cmd.BatchID)
	if err != nil {
		return ReplayResult{}, err
	}
	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		return ReplayResult{}, errs.Wrap(err, errs.KindInternal, CodeBatchNotFound,
			"the batch names an org that does not exist")
	}

	batch, err := s.batches.Get(ctx, scope, cmd.BatchID, receivedAt)
	if err != nil {
		// ⛔ --force DOES NOT REACH HERE. A payload that has aged out of its
		// retention partition is not a decision an operator can override; there are
		// no bytes to replay. This is the same NotFound ProcessBatch treats as "done,
		// by policy" — said out loud, because a human asked.
		return ReplayResult{}, err
	}

	res := ReplayResult{
		BatchID:    batch.ID,
		ReceivedAt: batch.ReceivedAt,
		Status:     batch.Status,
		Failure:    batch.Error,
		Forced:     cmd.Force,
	}

	if !batch.Status.Replayable() {
		// ⛔ ALSO NOT FORCEABLE. `processed` already reached the product, and
		// `pending` is on its way there — replaying either is asking for the
		// double-write with none of the recovery.
		return res, errs.Newf(errs.KindPrecondition, CodeReplayNotReplayable,
			"a %s batch is not replayable; only failed and partial batches are", batch.Status)
	}

	observations, _, err := s.plan(ctx, scope, batch)
	if err != nil {
		// Including CodeUndecodable: bytes that will never decode will not decode
		// this time either, and the batch is already marked failed saying so.
		return res, err
	}

	keys, firing := touchedAlerts(observations)
	res.AlertsTouched = len(keys)

	states, err := s.alertStates.StatesByAlertKey(ctx, scope, keys)
	if err != nil {
		return res, errs.Wrap(err, errs.KindUnavailable, CodeReplayFailed,
			"the alerts this batch touches could not be read; nothing was changed")
	}
	res.Superseded = supersededBy(keys, firing, states, batch.ReceivedAt)

	if len(res.Superseded) > 0 && !cmd.Force {
		// The refusal IS the outcome. No Reopen, no outbox row, no rejection rows:
		// the caller prints the list and exits non-zero.
		return res, nil
	}

	// ⭐ ONE TRANSACTION, and it is the accept path's own outbox (ADR 0001, §G.1).
	// The status change and the job that acts on it commit together or not at all:
	// there is no window in which a batch reads `pending` with nothing queued to
	// process it, and none in which a job runs against a row that is still `failed`
	// and would exit immediately.
	err = db.Tx(ctx, s.pool, func(ctx context.Context) error {
		if err := s.batches.Reopen(ctx, scope, batch.ID, batch.ReceivedAt,
			[]domain.Status{domain.StatusFailed, domain.StatusPartial}); err != nil {
			return err
		}
		_, err := s.enqueuer.Enqueue(ctx, jobs.IngestProcessBatchArgs{
			BatchID:    batch.ID,
			ReceivedAt: batch.ReceivedAt,
		})
		return err
	})
	if err != nil {
		return res, err
	}

	res.Enqueued = true
	s.log.InfoContext(ctx, "ingest: batch replayed",
		"batch_id", batch.ID, "org_id", orgID, "received_at", batch.ReceivedAt,
		"alerts", res.AlertsTouched, "superseded", len(res.Superseded), "forced", cmd.Force)
	return res, nil
}

// touchedAlerts reduces the planned observations to the DISTINCT alert keys the
// batch would act on, in payload order, plus which of them the batch carries
// `firing` for.
//
// Order is preserved rather than sorted because the refusal list is read by a
// human next to the payload, and a batch can legitimately name the same alert
// twice — the second mention does not make it two alerts, but a `firing` anywhere
// in the batch is a firing write.
func touchedAlerts(observations []alerts.Observation) ([]string, map[string]bool) {
	keys := make([]string, 0, len(observations))
	firing := make(map[string]bool, len(observations))
	seen := make(map[string]struct{}, len(observations))

	for _, o := range observations {
		k := o.AlertKey.String()
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
		if o.Status == domain.TopStatusFiring {
			firing[k] = true
		}
	}
	return keys, firing
}

// supersededBy applies the two refusal limbs to the touched set.
//
// An alert with no row and an alert with no case are both SAFE and are not
// listed: a replay cannot overtake a timeline that does not exist, and creating
// the first case for an alert is the ordinary thing this batch was supposed
// to do in the first place.
func supersededBy(
	keys []string, firing map[string]bool, states map[string]AlertState, receivedAt time.Time,
) []Supersession {
	var out []Supersession
	for _, k := range keys {
		st, ok := states[k]
		if !ok || !st.Exists || st.State == "" {
			continue
		}

		var limb SupersessionLimb
		switch {
		case st.SourceUpdatedAt.After(receivedAt):
			// A later batch already wrote to this alert. Replaying applies a stale
			// observation over a fresher one.
			limb = LimbOvertaken
		case st.Terminal && firing[k]:
			// The episode is closed and this batch says firing. See LimbClosed.
			limb = LimbClosed
		default:
			continue
		}

		out = append(out, Supersession{
			Limb:     limb,
			AlertKey: k,
			Identity: st.Identity,
			State:    st.State,
			MovedAt:  st.MovedAt,
		})
	}
	return out
}
