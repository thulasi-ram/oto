package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/decode"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// AcceptCommand is one raw inbound batch.
type AcceptCommand struct {
	SourceID uuid.UUID
	// Body is the RAW request body, already bounded by B1 at the transport. It is
	// hashed verbatim for `checksum` and redacted before it is persisted.
	Body []byte
	Mode domain.Mode
}

// AcceptResult is the receipt. It is the whole of `IngestAcceptedDTO`.
type AcceptResult struct {
	// BatchID is the durable handle — or the NIL UUID in the one case where a
	// batch is answered 202 without a batch row: a source that does not accept
	// pushes. That case is recorded as an `unknown_source` rejection instead,
	// because a 4xx would delete the notification at the upstream and an operator
	// toggling a flag must not be able to erase evidence.
	BatchID    uuid.UUID
	AlertCount int
	// Duplicate is true when `ingest_dedup` collapsed this onto an earlier batch.
	// The ORIGINAL batch id is returned, so the response is stable across every
	// replay — an HA sibling and three retries all get the same answer.
	Duplicate bool
	// TruncatedAlerts is the upstream's own `truncatedAlerts` plus whatever B2
	// dropped. Both are permanent losses of alert BODIES; oto can only say "+N".
	TruncatedAlerts int
	// RejectedAlerts is how many `ingest_rejections` rows this accept wrote.
	RejectedAlerts int
}

// Accept is SPEC §G.1: durably record one raw batch and enqueue its processing,
// in ONE short transaction, with NO outbound network call.
//
//	authenticate  (the transport's job, already done)
//	read body, cap 8 MiB (B1)               <- the transport's job
//	decode leniently (B16)
//	batch bounds B2, B15
//	redact                                   <- BEFORE persistence
//	checksum + batch_dedup_key (C.5)
//	BEGIN
//	  INSERT ingest_dedup … ON CONFLICT DO NOTHING
//	    -> lost the race: return 202 with the ORIGINAL batch_id
//	  INSERT ingest_batches (status='pending')
//	  enqueue ingest.process_batch           <- same tx: the transactional outbox
//	COMMIT
//
// Per-alert bounds B3-B14 are deliberately NOT here. They need the source's
// `ignore_labels` and `inject_labels` and they build a LabelSet per alert, which
// on a 10 000-alert batch is real work — and every millisecond of it is spent
// inside Alertmanager's retry budget while it holds the connection open. They run
// in `ingest.process_batch`, against the durable payload, where a failure costs a
// retry rather than an alert.
//
// The error contract is narrow on purpose. Only two failures may be anything but
// KindUnavailable, and both are recorded as rejections first:
// CodeUndecodable (400) and, from the transport, CodeBodyTooLarge (413).
// Everything else — a slow Postgres, an exhausted pool, a failed enqueue — is
// KindUnavailable with a Retry-After, because 5xx is the only class Alertmanager
// retries and a 4xx deletes the notification forever (C4).
func (s *Service) Accept(ctx context.Context, scope db.TenantScope, cmd AcceptCommand) (AcceptResult, error) {
	started := s.clk.Now()
	receivedAt := started

	if !cmd.Mode.Valid() {
		cmd.Mode = domain.ModePush
	}

	// The source config is needed BEFORE persistence, because redaction precedes
	// persistence (§L.3.3 step 7). A source oto cannot read is backpressure, not a
	// client error: persisting an unredacted payload is not an option, and a 4xx
	// would delete the alert.
	src, err := s.sources.Config(ctx, scope, cmd.SourceID)
	if err != nil {
		s.metrics.Duration.WithLabelValues("unavailable").Observe(s.clk.Since(started).Seconds())
		return AcceptResult{}, errs.Wrap(err, errs.KindUnavailable, CodeSourceUnavailable,
			"this source's configuration is temporarily unreadable").WithRetryAfter(domain.RetryAfter)
	}

	env, err := decode.Decode(cmd.Body)
	if err != nil {
		s.recordBodyRejection(ctx, scope, cmd, receivedAt, domain.ReasonUndecodable, err.Error())
		s.metrics.Duration.WithLabelValues("undecodable").Observe(s.clk.Since(started).Seconds())
		return AcceptResult{}, errs.Wrap(err, errs.KindMalformed, CodeUndecodable,
			"the request body is not an Alertmanager webhook payload")
	}

	if !src.AcceptsPush() {
		// Recorded, not refused. An operator who disabled pushes, or a source that
		// was soft deleted while its token was still live, must not silently vanish
		// alerts — and a 4xx here would delete them at the upstream too.
		s.recordBodyRejection(ctx, scope, cmd, receivedAt, domain.ReasonUnknownSource,
			"the source does not accept pushes (disabled or deleted)")
		s.metrics.Duration.WithLabelValues("unknown_source").Observe(s.clk.Since(started).Seconds())
		return AcceptResult{BatchID: uuid.Nil, Duplicate: false}, nil
	}

	bounds := decode.ApplyBatchBounds(&env)
	redactor := decode.NewRedactor(src.RedactLabels, src.RedactAnnotations)

	// ⭐ REDACTION PRECEDES PERSISTENCE (§C.9.2). Everything below writes to disk,
	// and the in-memory envelope is redacted too so that the C.5 dedup key is
	// computed over the same values the batch will be processed from.
	redactor.Envelope(&env)

	truncateTo := 0
	if bounds.Dropped > 0 {
		truncateTo = len(env.Alerts)
	}
	payload, err := decode.PersistedPayload(cmd.Body, redactor, truncateTo)
	if err != nil {
		s.metrics.Duration.WithLabelValues("unavailable").Observe(s.clk.Since(started).Seconds())
		return AcceptResult{}, errs.Wrap(err, errs.KindUnavailable, CodeAcceptFailed,
			"the redacted payload could not be encoded").WithRetryAfter(domain.RetryAfter)
	}

	params := domain.NewBatchParams{
		ID:         id.New(),
		SourceID:   cmd.SourceID,
		Mode:       cmd.Mode,
		ReceivedAt: receivedAt,
		BodyBytes:  len(cmd.Body),
		Checksum:   domain.Checksum(cmd.Body),
		DedupKey: domain.ComputeBatchDedupKey(
			cmd.SourceID, env.GroupKey, env.Receiver, env.NotificationReason, decode.FingerprintsOf(env)),
		AMVersion:          env.Version,
		GroupKey:           env.GroupKey,
		Receiver:           env.Receiver,
		NotificationReason: env.NotificationReason,
		StatusTop:          topStatus(env.Status),
		AlertCount:         len(env.Alerts),
		TruncatedAlerts:    env.TruncatedAlerts + bounds.Dropped,
		Payload:            payload,
	}

	res, err := s.commitAccept(ctx, scope, params, bounds.Notes)
	if err != nil {
		s.metrics.Duration.WithLabelValues("unavailable").Observe(s.clk.Since(started).Seconds())
		return AcceptResult{}, err
	}

	outcome := "accepted"
	if res.Duplicate {
		outcome = "duplicate"
		s.metrics.Duplicates.Inc()
	} else {
		s.metrics.Accepted.WithLabelValues(cmd.Mode.String()).Inc()
		s.metrics.Alerts.Add(float64(res.AlertCount))
	}
	s.metrics.Duration.WithLabelValues(outcome).Observe(s.clk.Since(started).Seconds())
	return res, nil
}

// commitAccept is the transaction itself, kept separate so the shape of §G.1 is
// readable in one screen and so the rollback path has exactly one owner.
func (s *Service) commitAccept(
	ctx context.Context, scope db.TenantScope, params domain.NewBatchParams, notes []decode.Note,
) (AcceptResult, error) {
	var out AcceptResult

	err := db.Tx(ctx, s.pool, func(ctx context.Context) error {
		hit, err := s.dedup.Claim(ctx, params.SourceID, params.DedupKey, params.ID, params.ReceivedAt)
		if err != nil {
			return err
		}
		if !hit.Inserted {
			// The batch is already on disk under another id, and its job is already
			// queued. Answer with the ORIGINAL id and do nothing else — that is the
			// whole of §C.5's "on conflict, 202 with the original batch_id".
			out = AcceptResult{BatchID: hit.BatchID, Duplicate: true}
			return nil
		}

		if _, err := s.batches.Insert(ctx, scope, params); err != nil {
			return err
		}

		if len(notes) > 0 {
			rejections := make([]domain.Rejection, 0, len(notes))
			for _, n := range notes {
				rejections = append(rejections, domain.Rejection{
					ID:         id.New(),
					OrgID:      scope.OrgID(),
					SourceID:   params.SourceID,
					BatchID:    &params.ID,
					ReceivedAt: params.ReceivedAt,
					Reason:     n.Reason,
					Detail:     n.Detail,
					Raw:        batchRejectionEvidence(params, n),
				})
			}
			if err := s.rejections.RecordBatch(ctx, scope, rejections); err != nil {
				return err
			}
		}

		// THE TRANSACTIONAL OUTBOX (ADR 0001, §G.1). The job and the row that
		// justifies it commit together or not at all: there is no window in which
		// oto has recorded a batch it will never process, and none in which it
		// processes a batch it never recorded.
		if _, err := s.enqueuer.Enqueue(ctx, jobs.IngestProcessBatchArgs{
			BatchID:    params.ID,
			ReceivedAt: params.ReceivedAt,
		}); err != nil {
			return err
		}

		out = AcceptResult{
			BatchID:         params.ID,
			AlertCount:      params.AlertCount,
			TruncatedAlerts: params.TruncatedAlerts,
			RejectedAlerts:  len(notes),
		}
		return nil
	})
	if err != nil {
		return AcceptResult{}, asBackpressure(err)
	}

	for _, n := range notes {
		s.metrics.countRejections(n.Reason.String())
	}
	return out, nil
}

// RecordBodyTooLarge writes the B1 rejection for a body the transport refused to
// read (§L.3.2). It is called by the handler before it answers 413.
//
// The oversized body itself is NOT stored — that is the entire point of the bound
// — so the evidence column carries the size and nothing else. A best-effort
// failure here is logged and swallowed: the 413 is already correct, and failing
// to write the audit row must not turn a permanent client fault into a 500.
func (s *Service) RecordBodyTooLarge(ctx context.Context, scope db.TenantScope, sourceID uuid.UUID, bytes int64) {
	raw, _ := json.Marshal(map[string]any{
		"reason":     domain.ReasonBodyTooLarge.String(),
		"limit":      domain.MaxBodyBytes,
		"body_bytes": bytes,
	})
	s.recordRejection(ctx, scope, domain.Rejection{
		ID:         id.New(),
		OrgID:      scope.OrgID(),
		SourceID:   sourceID,
		ReceivedAt: s.clk.Now(),
		Reason:     domain.ReasonBodyTooLarge,
		Detail:     fmt.Sprintf("body exceeded the %d byte limit", domain.MaxBodyBytes),
		Raw:        raw,
	})
}

// recordBodyRejection records a whole-body rejection: undecodable, or a source
// that will not accept pushes. There is no batch row for either, so `batch_id`
// stays NULL (ingest_rejections.batch_id is nullable for exactly this case).
func (s *Service) recordBodyRejection(
	ctx context.Context, scope db.TenantScope, cmd AcceptCommand,
	receivedAt time.Time, reason domain.Reason, detail string,
) {
	raw, _ := json.Marshal(map[string]any{
		"reason": reason.String(),
		"detail": detail,
		// A BOUNDED sample of the body, so an operator can see what arrived without
		// an 8 MiB blob landing in a rejection row — and without an unredacted
		// payload reaching disk in bulk. It is bytes we could not decode, so there
		// is no label structure to redact against.
		"body_sample": sampleOf(cmd.Body),
		"body_bytes":  len(cmd.Body),
	})
	s.recordRejection(ctx, scope, domain.Rejection{
		ID:         id.New(),
		OrgID:      scope.OrgID(),
		SourceID:   cmd.SourceID,
		ReceivedAt: receivedAt,
		Reason:     reason,
		Detail:     truncate(detail, maxDetailBytes),
		Raw:        raw,
	})
}

// recordRejection writes one rejection outside any transaction, best effort.
func (s *Service) recordRejection(ctx context.Context, scope db.TenantScope, r domain.Rejection) {
	// context.WithoutCancel: the rejection must land even when the client has
	// already hung up, because the row IS the audit trail for a request oto is
	// about to refuse. It is bounded by the pool's own 2 s statement timeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()

	if err := s.rejections.Record(ctx, scope, r); err != nil {
		s.log.ErrorContext(ctx, "ingest: could not record rejection",
			"source_id", r.SourceID, "reason", r.Reason.String(), "error", err)
		return
	}
	s.metrics.countRejections(r.Reason.String())
}

// asBackpressure maps any accept-transaction failure onto KindUnavailable.
//
// ⭐ THIS IS THE SINGLE MOST IMPORTANT ERROR MAPPING IN OTO. Alertmanager retries
// only 5xx; a 4xx or a 429 makes it discard the notification permanently and
// silently, during exactly the window when the customer's cluster is on fire
// (ADR 0007). So a unique violation, a check violation, a statement timeout and a
// dead connection all become the same thing here: 503 with a Retry-After, inside
// Alertmanager's own ~5-minute retry budget.
//
// The one exception is a context cancellation, which means the CLIENT hung up.
// There is nobody left to answer, and dressing that up as backpressure would
// pollute the shed metric with disconnects.
func asBackpressure(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return errs.Wrap(err, errs.KindUnavailable, CodeAcceptFailed,
		"oto could not durably record this batch right now").WithRetryAfter(domain.RetryAfter)
}

// batchRejectionEvidence is the `raw` column for a batch-level rejection. The
// offending element is the batch envelope's own counts, not an alert.
func batchRejectionEvidence(p domain.NewBatchParams, n decode.Note) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"reason":           n.Reason.String(),
		"detail":           n.Detail,
		"group_key":        p.GroupKey,
		"receiver":         p.Receiver,
		"alert_count":      p.AlertCount,
		"truncated_alerts": p.TruncatedAlerts,
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// topStatus normalises the envelope's batch-level status onto
// ingest_batches_status_ck, or "" when the upstream sent something else. An
// unrecognised value is dropped rather than stored: the column is CHECKed, and a
// check violation on the ingest path is a 500 where an alert belongs.
func topStatus(s string) string {
	switch s {
	case domain.TopStatusFiring, domain.TopStatusResolved:
		return s
	default:
		return ""
	}
}

const (
	// maxSampleBytes bounds the body excerpt kept on an undecodable rejection.
	maxSampleBytes = 1024
	// maxDetailBytes bounds the human detail string.
	maxDetailBytes = 2048
	// recordTimeout bounds a best-effort rejection write.
	recordTimeout = 2 * time.Second
)

func sampleOf(body []byte) string { return truncate(string(body), maxSampleBytes) }

// truncate cuts s to at most n bytes on a UTF-8 rune boundary. A half-rune is a
// value Postgres refuses in a JSONB column and a UI renders as a replacement
// character, so the cut is made where the runes end.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
