package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// batchRow is the row model of `ingest_batches`. Unexported, per the three-model
// rule: no DTO and no domain type may embed it.
type batchRow struct {
	id                 uuid.UUID
	orgID              uuid.UUID
	sourceID           uuid.UUID
	mode               string
	receivedAt         time.Time
	bodyBytes          int32
	checksum           []byte
	dedupKey           string
	amVersion          *string
	groupKey           *string
	receiver           *string
	notificationReason *string
	statusTop          *string
	alertCount         int32
	truncatedAlerts    int32
	payload            []byte
	status             string
	processedAt        *time.Time
	failure            *string
}

func (r batchRow) toDomain() domain.Batch {
	return domain.Batch{
		ID:                 r.id,
		OrgID:              r.orgID,
		SourceID:           r.sourceID,
		Mode:               domain.Mode(r.mode),
		ReceivedAt:         r.receivedAt,
		BodyBytes:          int(r.bodyBytes),
		Checksum:           r.checksum,
		DedupKey:           r.dedupKey,
		AMVersion:          deref(r.amVersion),
		GroupKey:           deref(r.groupKey),
		Receiver:           deref(r.receiver),
		NotificationReason: deref(r.notificationReason),
		StatusTop:          deref(r.statusTop),
		AlertCount:         int(r.alertCount),
		TruncatedAlerts:    int(r.truncatedAlerts),
		Payload:            r.payload,
		Status:             domain.Status(r.status),
		ProcessedAt:        r.processedAt,
		Error:              deref(r.failure),
	}
}

// BatchRepository is the SQL over `ingest_batches`.
//
// Every method joins the caller's transaction through db.FromContext. On the
// accept path that is not a convenience: the batch insert, the dedup claim and
// the job enqueue MUST commit together, or a 202 stops being a promise (§G.1).
type BatchRepository struct {
	q db.Querier
}

// NewBatchRepository builds the repository over a fallback querier, which on this
// path is THE INGEST POOL (§G.10) — never the general one.
func NewBatchRepository(q db.Querier) *BatchRepository { return &BatchRepository{q: q} }

func (r *BatchRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const insertBatchSQL = `
INSERT INTO ingest_batches (
  id, org_id, source_id, mode, received_at, body_bytes, checksum, dedup_key,
  am_version, group_key, receiver, notification_reason, status_top,
  alert_count, truncated_alerts, payload, status
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8,
  nullif($9,''), nullif($10,''), nullif($11,''), $12, nullif($13,''),
  $14, $15, $16, 'pending'
)
RETURNING id, org_id, source_id, mode, received_at, body_bytes, checksum, dedup_key,
          am_version, group_key, receiver, notification_reason, status_top,
          alert_count, truncated_alerts, payload, status, processed_at, error`

// Insert writes the pending batch row.
//
// `notification_reason` is passed through WITHOUT nullif: the column's documented
// contract is `""` when Alertmanager sent no such field (AM < 0.32.0), and an
// empty string is a different claim from NULL — one says "the upstream is too old
// to tell us", the other would say "we did not look".
func (r *BatchRepository) Insert(ctx context.Context, s db.TenantScope, in domain.NewBatchParams) (domain.Batch, error) {
	if len(in.Payload) == 0 {
		return domain.Batch{}, errs.New(errs.KindInternal, "ingest_empty_payload",
			"a batch payload must not be empty")
	}

	var row batchRow
	err := r.db(ctx).QueryRow(ctx, insertBatchSQL,
		in.ID, s.OrgID(), in.SourceID, string(in.Mode), in.ReceivedAt,
		int32(in.BodyBytes), in.Checksum, in.DedupKey,
		in.AMVersion, in.GroupKey, in.Receiver, in.NotificationReason, in.StatusTop,
		int32(in.AlertCount), int32(in.TruncatedAlerts), []byte(in.Payload),
	).Scan(
		&row.id, &row.orgID, &row.sourceID, &row.mode, &row.receivedAt,
		&row.bodyBytes, &row.checksum, &row.dedupKey,
		&row.amVersion, &row.groupKey, &row.receiver, &row.notificationReason, &row.statusTop,
		&row.alertCount, &row.truncatedAlerts, &row.payload,
		&row.status, &row.processedAt, &row.failure,
	)
	if err != nil {
		return domain.Batch{}, mapErr(err, "insert ingest batch")
	}
	return row.toDomain(), nil
}

const getBatchSQL = `
SELECT id, org_id, source_id, mode, received_at, body_bytes, checksum, dedup_key,
       am_version, group_key, receiver, notification_reason, status_top,
       alert_count, truncated_alerts, payload, status, processed_at, error
  FROM ingest_batches
 WHERE org_id = $1 AND id = $2 AND received_at = $3`

// Get loads one batch by its full primary key.
//
// `received_at` is not optional and is not a convenience: it is the PARTITION KEY
// of a daily-ranged table, and a query without it scans every retained partition
// instead of exactly one. The job payload carries it for this reason (§G.3).
func (r *BatchRepository) Get(ctx context.Context, s db.TenantScope, batchID uuid.UUID, receivedAt time.Time) (domain.Batch, error) {
	var row batchRow
	err := r.db(ctx).QueryRow(ctx, getBatchSQL, s.OrgID(), batchID, receivedAt).Scan(
		&row.id, &row.orgID, &row.sourceID, &row.mode, &row.receivedAt,
		&row.bodyBytes, &row.checksum, &row.dedupKey,
		&row.amVersion, &row.groupKey, &row.receiver, &row.notificationReason, &row.statusTop,
		&row.alertCount, &row.truncatedAlerts, &row.payload,
		&row.status, &row.processedAt, &row.failure,
	)
	if err != nil {
		return domain.Batch{}, mapErr(err, "load ingest batch")
	}
	return row.toDomain(), nil
}

const markProcessedSQL = `
UPDATE ingest_batches
   SET status = $4, processed_at = $5, error = nullif($6,'')
 WHERE org_id = $1 AND id = $2 AND received_at = $3`

// MarkProcessed closes a batch out, or moves it to `partial` mid-chunking.
//
// It sets `processed_at` unconditionally because ingest_batches_proc_ck ties the
// two together: every non-pending status REQUIRES a timestamp, and every pending
// one requires its absence. Passing a `failed` status without a reason would
// violate ingest_batches_err_ck, so that is refused here rather than in Postgres
// — a check violation on this path is a 500 where an alert belongs.
func (r *BatchRepository) MarkProcessed(
	ctx context.Context, s db.TenantScope, batchID uuid.UUID, receivedAt time.Time,
	status domain.Status, at time.Time, failure string,
) error {
	if status == domain.StatusFailed && failure == "" {
		return errs.New(errs.KindInternal, "ingest_batches_err_ck",
			"a failed batch must carry a reason")
	}
	if status == domain.StatusPending {
		return errs.New(errs.KindInternal, "ingest_batches_proc_ck",
			"a batch cannot be moved back to pending")
	}

	tag, err := r.db(ctx).Exec(ctx, markProcessedSQL, s.OrgID(), batchID, receivedAt,
		string(status), at, failure)
	if err != nil {
		return mapErr(err, "close out ingest batch")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("ingest_batch_not_found", "no such ingest batch")
	}
	return nil
}

const resolveOrgSQL = `SELECT org_id FROM ingest_batches WHERE id = $1 AND received_at = $2`

// ResolveOrg returns the org that owns a batch.
//
// ⚠️ This is the one method in the module without a TenantScope, and the doc
// comment on service.BatchRepository says why: the `ingest.process_batch` payload
// names `{batch_id, received_at}` and no org, so the org has to be discovered
// before a scope can exist. It reads ONE column of ONE row addressed by its full
// primary key and returns nothing else, and every call the worker makes
// afterwards is scoped by what it returns.
func (r *BatchRepository) ResolveOrg(ctx context.Context, batchID uuid.UUID, receivedAt time.Time) (uuid.UUID, error) {
	var orgID uuid.UUID
	if err := r.db(ctx).QueryRow(ctx, resolveOrgSQL, batchID, receivedAt).Scan(&orgID); err != nil {
		return uuid.Nil, mapErr(err, "resolve the batch's org")
	}
	return orgID, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
