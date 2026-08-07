package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/decode"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ProcessResult is what one run of `ingest.process_batch` did.
type ProcessResult struct {
	BatchID uuid.UUID
	// Skipped is true when the batch was already terminal, which is the normal
	// outcome of a redelivered job and not an error.
	Skipped   bool
	Observed  int
	Rejected  int
	FinalStat domain.Status
}

// ProcessBatch is SPEC §G.4: the ONLY write path into `alerts`.
//
// It is safely re-runnable at three levels, because a job queue is at-least-once
// and a pod can die between any two statements:
//
//  1. A batch that is already `processed` or `failed` exits immediately, having
//     done nothing (§G.4 step 1).
//  2. A batch left `partial` by a worker that died mid-chunking is RESUMED rather
//     than abandoned — see domain.Status.Resumable for why the literal reading of
//     §G.4 would strand it.
//  3. Re-applying a chunk that already committed produces no second observation,
//     because the alert upsert is ON CONFLICT and events dedupe through
//     `alert_event_keys` (§G.5). That is a contract on AlertObserver, restated in
//     its doc comment.
//
// Partial failure never loses the batch. One bad alert becomes one
// `ingest_rejections` row with a reason and the offending element, and the rest
// of the batch proceeds. That asymmetry is the entire justification for having
// answered 202 in the first place.
func (s *Service) ProcessBatch(ctx context.Context, scope db.TenantScope, batchID uuid.UUID, receivedAt time.Time) (ProcessResult, error) {
	started := s.clk.Now()

	batch, err := s.batches.Get(ctx, scope, batchID, receivedAt)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			// Aged out of its retention partition. Nothing to do and nothing to
			// retry: the raw payload is gone by policy, not by accident.
			return ProcessResult{BatchID: batchID, Skipped: true}, nil
		}
		return ProcessResult{}, errs.Wrap(err, errs.KindUnavailable, CodeProcessFailed,
			"the batch could not be loaded")
	}

	if !batch.Status.Resumable() {
		s.metrics.ProcessDuration.WithLabelValues("skipped").Observe(s.clk.Since(started).Seconds())
		return ProcessResult{BatchID: batchID, Skipped: true, FinalStat: batch.Status}, nil
	}

	res, err := s.processResumable(ctx, scope, batch)
	outcome := "processed"
	if err != nil {
		outcome = "failed"
	}
	s.metrics.ProcessDuration.WithLabelValues(outcome).Observe(s.clk.Since(started).Seconds())
	return res, err
}

// processResumable does the work for a batch that is pending or partial.
func (s *Service) processResumable(ctx context.Context, scope db.TenantScope, batch domain.Batch) (ProcessResult, error) {
	res := ProcessResult{BatchID: batch.ID}

	env, err := decode.Decode(batch.Payload)
	if err != nil {
		// The payload failed its own CHECK on the way in, so this is oto's bug, not
		// the upstream's. Mark the batch failed rather than retrying thirteen times
		// against bytes that will never decode.
		s.markFailed(ctx, scope, batch, "stored payload is not decodable: "+err.Error())
		res.FinalStat = domain.StatusFailed
		return res, nil
	}

	src, err := s.sources.Config(ctx, scope, batch.SourceID)
	if err != nil {
		return res, errs.Wrap(err, errs.KindUnavailable, CodeSourceUnavailable,
			"this source's configuration is temporarily unreadable")
	}

	observations, rejections := s.normalise(ctx, env, batch, src)
	res.Rejected = len(rejections)

	// Rejections are written FIRST and outside the observation transactions. If
	// the chunk below fails and the job retries, the worst case is a duplicate
	// rejection row — visible, harmless, and vastly better than the alternative,
	// which is a batch that succeeded while the record of what it dropped rolled
	// back with the failure.
	if len(rejections) > 0 {
		if err := s.rejections.RecordBatch(ctx, scope, rejections); err != nil {
			return res, errs.Wrap(err, errs.KindUnavailable, CodeProcessFailed,
				"rejections could not be recorded")
		}
		for _, r := range rejections {
			s.metrics.countRejections(r.Reason.String())
		}
	}

	observed, err := s.applyChunks(ctx, scope, batch, observations)
	res.Observed = observed
	if err != nil {
		// Retryable. The batch is left pending/partial on purpose so that the job's
		// own retry budget (§G.6) gets to do its work; only the worker's last
		// attempt marks it failed.
		return res, err
	}

	res.FinalStat = domain.StatusProcessed
	if err := s.batches.MarkProcessed(ctx, scope, batch.ID, batch.ReceivedAt,
		domain.StatusProcessed, s.clk.Now(), ""); err != nil {
		return res, errs.Wrap(err, errs.KindUnavailable, CodeProcessFailed,
			"the batch could not be closed out")
	}
	return res, nil
}

// normalise turns wire alerts into Observations, one at a time, collecting the
// failures rather than aborting on them.
//
// ⭐ ONE BAD ALERT MUST NOT COST THE BATCH. A 10 000-alert storm batch containing
// one alert with a 9 KiB label value has 9 999 alerts that are real, and a
// customer's cluster is on fire while we decide what to do about the one.
func (s *Service) normalise(
	ctx context.Context, env decode.Envelope, batch domain.Batch, src domain.SourceConfig,
) ([]alerts.Observation, []domain.Rejection) {
	now := s.clk.Now()
	opt := decode.AlertOptions{Now: now, InjectLabels: src.InjectLabels}

	observations := make([]alerts.Observation, 0, len(env.Alerts))
	var rejections []domain.Rejection

	for i := range env.Alerts {
		wire := env.Alerts[i]

		n, err := decode.Normalise(wire, opt)
		if err != nil {
			rejections = append(rejections, s.rejectionFor(batch, wire, domain.ReasonFromError(err), err.Error()))
			continue
		}

		// Non-fatal bounds still leave a record: B7, B8 and B13 keep the alert and
		// say so. Silence here would make "we dropped 40 annotations" indetectable.
		for _, note := range n.Notes {
			rejections = append(rejections, s.rejectionFor(batch, wire, note.Reason, note.Detail))
		}

		if n.FingerprintMismatch {
			s.metrics.FingerprintMismatch.Inc()
			s.log.WarnContext(ctx, "ingest: fingerprint mismatch",
				"batch_id", batch.ID, "source_id", batch.SourceID,
				"wire", n.WireFingerprint, "computed", n.Fingerprint.String())
		}

		skew := now.Sub(n.StartsAt)
		s.metrics.ClockSkew.Observe(skew.Seconds())

		observations = append(observations, alerts.Observation{
			Source:     observationSource(batch.Mode),
			BatchID:    batch.ID,
			SourceID:   batch.SourceID,
			ClusterID:  src.ClusterID,
			ClusterKey: src.ClusterKey,

			AlertKey:          alerts.ComputeAlertKey(batch.OrgID, src.ClusterKey, n.Labels, src.IgnoreLabels),
			SourceFingerprint: n.Fingerprint,
			Labels:            n.Labels,
			Annotations:       n.Annotations,
			GeneratorURL:      n.GeneratorURL,

			// A webhook can carry only firing or resolved, and it can NEVER carry
			// suppressed: MuteStage drops muted alerts before the webhook fires, so
			// SuppressionReason stays empty here by construction (C1). Its arrival is
			// nonetheless positive proof that suppression has ENDED, which is what
			// lets ingest drive T4 (§B.3.1).
			Status: n.Status,

			SourceStartsAt: n.StartsAt,
			SourceEndsAt:   n.EndsAt,
			// SourceUpdatedAt is the batch's receipt: Alertmanager sends no per-alert
			// updatedAt on a webhook, and inventing one from `startsAt` would claim
			// the upstream told us something it did not.
			SourceUpdatedAt: batch.ReceivedAt,
			Value:           n.Value,

			ObservedAt: now,
			SkewMS:     skew.Milliseconds(),
		})
	}

	return observations, rejections
}

// applyChunks hands the observations to the alerts service, in transactions of
// at most domain.ChunkSize (B17).
//
// A batch at or under ChunkThreshold is ONE transaction, which is the common case
// and the fast one. Above it, the batch is split and marked `partial` until the
// last chunk commits, because a single transaction holding twenty thousand
// upserts blocks vacuum, bloats WAL and is the one shape that can make a 2 s
// statement timeout look like an outage.
func (s *Service) applyChunks(
	ctx context.Context, scope db.TenantScope, batch domain.Batch, obs []alerts.Observation,
) (int, error) {
	if len(obs) == 0 {
		return 0, nil
	}
	if s.alerts == nil {
		// The alerts service is not wired yet. Say so loudly rather than marking a
		// batch processed that nothing has processed — a false `processed` is a lost
		// alert wearing a green tick.
		return 0, errs.New(errs.KindInternal, CodeProcessFailed,
			"no alert observer is wired; the batch stays pending")
	}

	chunked := len(obs) > domain.ChunkThreshold
	if chunked {
		if err := s.batches.MarkProcessed(ctx, scope, batch.ID, batch.ReceivedAt,
			domain.StatusPartial, s.clk.Now(), ""); err != nil {
			return 0, errs.Wrap(err, errs.KindUnavailable, CodeProcessFailed,
				"the batch could not be marked partial")
		}
	}

	applied := 0
	for start := 0; start < len(obs); start += domain.ChunkSize {
		end := min(start+domain.ChunkSize, len(obs))

		chunk := obs[start:end]
		err := db.Tx(ctx, s.pool, func(ctx context.Context) error {
			n, err := s.alerts.ObserveBatch(ctx, scope, chunk)
			applied += n
			return err
		})
		if err != nil {
			return applied, errs.Wrap(err, errs.KindUnavailable, CodeProcessFailed,
				fmt.Sprintf("observations %d..%d could not be applied", start, end))
		}
	}
	return applied, nil
}

// markFailed closes a batch out as failed. `error` is REQUIRED when status is
// failed (ingest_batches_err_ck), so the reason is never optional.
func (s *Service) markFailed(ctx context.Context, scope db.TenantScope, batch domain.Batch, reason string) {
	if reason == "" {
		reason = "unspecified failure"
	}
	if err := s.batches.MarkProcessed(ctx, scope, batch.ID, batch.ReceivedAt,
		domain.StatusFailed, s.clk.Now(), truncate(reason, maxDetailBytes)); err != nil {
		s.log.ErrorContext(ctx, "ingest: could not mark batch failed",
			"batch_id", batch.ID, "error", err)
	}
}

// MarkFailed is the worker's terminal hook: it records why a batch gave up, on
// the LAST attempt only. Marking earlier would make the batch non-resumable and
// throw away the retry budget that §G.6 exists to spend.
func (s *Service) MarkFailed(ctx context.Context, scope db.TenantScope, batchID uuid.UUID, receivedAt time.Time, reason string) {
	s.markFailed(ctx, scope, domain.Batch{ID: batchID, ReceivedAt: receivedAt}, reason)
}

// PruneDedup deletes `ingest_dedup` rows past the C.5 horizon and returns how
// many went.
//
// The horizon is ten minutes, and the table stays small precisely because this
// runs — it is the ONE unpartitioned table on the ingest path, so it cannot age
// out by dropping a partition and a sweeper is not optional. It is exposed for
// the `retention.prune` maintenance job (§G.3) rather than scheduled here:
// ingestion owns what pruning MEANS, the maintenance queue owns when it happens.
func (s *Service) PruneDedup(ctx context.Context) (int64, error) {
	n, err := s.dedup.Prune(ctx, s.clk.Now().Add(-domain.DedupTTL))
	if err != nil {
		return 0, errs.Wrap(err, errs.KindUnavailable, CodeProcessFailed,
			"the dedup horizon could not be pruned")
	}
	return n, nil
}

// ResolveOrg discovers the org that owns a batch, so a worker can build the scope
// its payload does not carry. See BatchRepository.ResolveOrg for why.
func (s *Service) ResolveOrg(ctx context.Context, batchID uuid.UUID, receivedAt time.Time) (db.TenantScope, error) {
	orgID, err := s.batches.ResolveOrg(ctx, batchID, receivedAt)
	if err != nil {
		return db.TenantScope{}, err
	}
	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		return db.TenantScope{}, errs.Wrap(err, errs.KindInternal, CodeBatchNotFound,
			"the batch names an org that does not exist")
	}
	return scope, nil
}

// rejectionFor builds one per-alert rejection row, with the offending element
// kept as evidence. The element is POST-REDACTION: it comes out of the stored
// payload, which redaction already passed over.
func (s *Service) rejectionFor(
	batch domain.Batch, wire decode.Alert, reason domain.Reason, detail string,
) domain.Rejection {
	batchID := batch.ID
	return domain.Rejection{
		ID:         id.New(),
		OrgID:      batch.OrgID,
		SourceID:   batch.SourceID,
		BatchID:    &batchID,
		ReceivedAt: batch.ReceivedAt,
		Reason:     reason,
		Detail:     truncate(detail, maxDetailBytes),
		Raw:        alertEvidence(wire),
	}
}

// alertEvidence renders one wire alert as the `raw` column. On a marshalling
// failure it degrades to an object naming the alert rather than to nothing: the
// column is NOT NULL and an empty rejection tells an operator nothing.
func alertEvidence(a decode.Alert) json.RawMessage {
	raw, err := json.Marshal(a)
	if err != nil {
		fallback, _ := json.Marshal(map[string]string{
			"alertname": decode.AlertName(a),
			"error":     "the offending element could not be re-encoded",
		})
		return fallback
	}
	return raw
}

// observationSource maps a batch mode onto the state machine's witness kind.
//
// It is load-bearing (§B.3.1): only ObservedByReconciler may ENTER `suppressed`
// (T3), because only a reconcile pass can see it; either may LEAVE it (T4),
// because a webhook arrival is itself positive proof that suppression ended.
func observationSource(m domain.Mode) alerts.ObservationSource {
	if m == domain.ModeReconcile {
		return alerts.ObservedByReconciler
	}
	return alerts.ObservedByIngest
}
