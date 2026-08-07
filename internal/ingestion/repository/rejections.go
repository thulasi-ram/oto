package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// RejectionRepository is the SQL over `ingest_rejections`.
//
// This table is why 202 is an honest answer to a partially bad payload. A 4xx
// would make Alertmanager delete the notification forever (§G.2), so oto records
// what it refused instead of refusing the request — and the per-source rejection
// feed built on `ingest_rejections_source_idx` is the whole point of the table.
// oto never silently drops.
type RejectionRepository struct {
	q db.Querier
}

// NewRejectionRepository builds the repository over the ingest pool.
func NewRejectionRepository(q db.Querier) *RejectionRepository { return &RejectionRepository{q: q} }

func (r *RejectionRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// insertRejectionsSQL writes N rows in ONE round trip.
//
// A 10 000-alert batch with a broken exporter behind it can reject thousands of
// elements, and thousands of individual INSERTs on the ingest pool would turn a
// recording mechanism into the outage it exists to document.
const insertRejectionsSQL = `
INSERT INTO ingest_rejections (id, org_id, source_id, batch_id, received_at, reason, detail, raw)
SELECT i, $2, sid, bid, ts, rsn, nullif(dt,''), rw
  FROM unnest($1::uuid[], $3::uuid[], $4::uuid[], $5::timestamptz[], $6::text[], $7::text[], $8::jsonb[])
    AS t(i, sid, bid, ts, rsn, dt, rw)`

// Record writes one rejection.
func (r *RejectionRepository) Record(ctx context.Context, s db.TenantScope, in domain.Rejection) error {
	return r.RecordBatch(ctx, s, []domain.Rejection{in})
}

// RecordBatch writes many rejections in one round trip.
func (r *RejectionRepository) RecordBatch(ctx context.Context, s db.TenantScope, in []domain.Rejection) error {
	if len(in) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, len(in))
	sources := make([]uuid.UUID, len(in))
	batches := make([]*uuid.UUID, len(in))
	times := make([]time.Time, len(in))
	reasons := make([]string, len(in))
	details := make([]string, len(in))
	raws := make([][]byte, len(in))

	for i, rj := range in {
		if !rj.Reason.Valid() {
			// The reason column is a closed CHECK. Refusing an unknown member here
			// turns a would-be 23514 (a 500, and on this path a lost alert) into a
			// programming error the caller can see.
			return errs.Newf(errs.KindInternal, "ingest_rejections_reason_ck",
				"%q is not a member of the rejection reason enum", rj.Reason)
		}
		ids[i] = rj.ID
		sources[i] = rj.SourceID
		batches[i] = rj.BatchID
		times[i] = rj.ReceivedAt
		reasons[i] = rj.Reason.String()
		details[i] = rj.Detail
		raws[i] = rj.Raw
		if len(raws[i]) == 0 {
			// The column is NOT NULL and the whole point of the table is the
			// evidence, so an empty object beats a failed insert.
			raws[i] = []byte(`{}`)
		}
	}

	if _, err := r.db(ctx).Exec(ctx, insertRejectionsSQL,
		ids, s.OrgID(), sources, batches, times, reasons, details, raws,
	); err != nil {
		return mapErr(err, "record ingest rejections")
	}
	return nil
}
