package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The read half of ingestion.
//
// ⭐ WHY THESE TWO METHODS EXIST AT ALL. Everything else in this service is the
// write path: accept durably, normalise later. That path already records every
// per-alert bound failure in `ingest_rejections` and every terminal batch failure
// on `ingest_batches.error` — and until these methods, nothing in the product
// could read either back. SPEC AC-6 and AC-38 promise a rejection is visible with
// a reason; CONTEXT.md §5 makes "delivery failure must be visible per alert" a
// requirement rather than a nicety. An operator whose alert never appeared had
// `psql` and a rate-by-reason counter, which is exactly the silence §C.9.1
// forbids.
//
// Both are pure reads on the tenant's own partitions. They are on the INGEST pool
// like the rest of this service, and they are the one thing here that a human
// waits on rather than an Alertmanager — see the note on ListRejections.

// ListRejections is the per-source rejection feed, newest first.
//
// ⚠️ It runs on the ingest pool (§G.10), which is sized and timed for webhooks:
// 2 s statement timeout, 500 ms to acquire. That is fine and it is deliberate —
// the query is a bounded index scan per partition with a hard `LIMIT` — but it is
// the reason this method must never grow a full-text search, an aggregate or an
// unbounded window. A dashboard query that can take seconds does not belong on
// the pool the accept path lives on.
func (s *Service) ListRejections(
	ctx context.Context, scope db.TenantScope, f domain.RejectionFilter, p db.Keyset,
) ([]domain.RejectionEntry, db.Cursor, error) {
	if f.SourceID == uuid.Nil {
		return nil, db.Cursor{}, errs.Validation("source_required",
			"a rejection feed is about one source",
			errs.Violation{Field: "source_id", Code: "required", Message: "a source id is required"})
	}
	for _, r := range f.Reasons {
		if !r.Valid() {
			// A closed enum, so an unknown member matches no row. Refusing it names
			// the caller's mistake; serving it would return an empty page that reads
			// as "nothing was rejected", which on this screen is the one answer that
			// must never be wrong.
			return nil, db.Cursor{}, errs.Validation("reason_invalid",
				"reason must be a member of the rejection reason enum",
				errs.Violation{Field: "reason", Code: "invalid", Message: r.String() + " is not a rejection reason"})
		}
	}
	return s.rejections.List(ctx, scope, f, p)
}

// ListFailedBatches lists the batches that were accepted and never processed —
// `failed`, `partial`, or both — with the reason each stopped.
//
// It is the batch-level half of the same question. A rejection says oto refused
// one element; this says oto took the whole body, answered 202, and then could
// not turn it into alerts. Both have to be readable or a 202 is a promise nobody
// can check.
func (s *Service) ListFailedBatches(
	ctx context.Context, scope db.TenantScope, f domain.BatchFailureFilter, p db.Keyset,
) ([]domain.BatchFailure, db.Cursor, error) {
	if f.SourceID == uuid.Nil {
		return nil, db.Cursor{}, errs.Validation("source_required",
			"a failed-batch feed is about one source",
			errs.Violation{Field: "source_id", Code: "required", Message: "a source id is required"})
	}
	for _, st := range f.Statuses {
		if !st.Troubled() {
			return nil, db.Cursor{}, errs.Validation("status_invalid",
				"status must be one of: failed, partial",
				errs.Violation{Field: "status", Code: "invalid", Message: st.String() + " is not a failure"})
		}
	}
	return s.batches.ListFailed(ctx, scope, f, p)
}
