package repository

import (
	"context"
	"encoding/json"
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
//
// ⭐ ON CONFLICT IS WHAT MAKES THIS TABLE REPLAY-SAFE. §G.5 promises that
// re-running a batch produces no second observation, and until 00047 the promise
// stopped at the observations: `ingest_rejections` had no uniqueness of any kind
// and minted a fresh id per row per attempt, so a batch with forty rejections
// replayed twice showed a hundred and twenty in the per-source feed — and the
// §G.6 retry budget could do it without a replay, because these rows are written
// outside the observation transactions on purpose and a retried chunk rewrites
// them.
//
// ⛔ THE ARBITER IS THE NATURAL KEY, NEVER THE PRIMARY KEY. `id` stays a uuidv7
// because `List` keysets on `(received_at, id)` and every rejection of one batch
// shares that `received_at` to the microsecond — the id is the only thing making
// that order total, and it works solely because a uuidv7 is time-ordered by
// minting. Deriving the id from the row's content was tried and it silently broke
// the paging. The dedupe identity therefore lives in its own constraint,
// `ingest_rejections_natural_uniq (received_at, batch_id, ordinal, reason)`, and
// the two concerns stop fighting over one column.
//
// `ordinal` is the row's position in this call, which is its position in the
// stored payload; a NULL `batch_id` never conflicts, which is correct, because
// those rows describe a body that never became a batch and can never be replayed.
const insertRejectionsSQL = `
INSERT INTO ingest_rejections (id, org_id, source_id, batch_id, received_at, reason, detail, raw, ordinal)
SELECT i, $2, sid, bid, ts, rsn, nullif(dt,''), rw, ord
  FROM unnest($1::uuid[], $3::uuid[], $4::uuid[], $5::timestamptz[], $6::text[], $7::text[], $8::jsonb[],
              $9::int[])
    AS t(i, sid, bid, ts, rsn, dt, rw, ord)
ON CONFLICT (received_at, batch_id, ordinal, reason) DO NOTHING`

// Record writes one rejection.
func (r *RejectionRepository) Record(ctx context.Context, s db.TenantScope, in domain.Rejection) error {
	return r.RecordBatch(ctx, s, []domain.Rejection{in})
}

// RecordBatch writes many rejections in one round trip.
//
// ⭐ THE SLICE POSITION IS PART OF THE ROW. `ordinal` is `i` and nothing cleverer:
// the caller built this slice by walking the stored payload in order, so the same
// bytes normalised again produce the same rejection at the same index, which is
// what lets `ingest_rejections_natural_uniq` recognise a replayed row as one it
// already has. It is a WRITE-SIDE identity and no read projects it.
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
	ordinals := make([]int32, len(in))

	for i, rj := range in {
		if !rj.Reason.Valid() {
			// The reason column is a closed CHECK. Refusing an unknown member here
			// turns a would-be 23514 (a 500, and on this path a lost alert) into a
			// programming error the caller can see.
			return errs.Newf(errs.KindInternal, "ingest_rejections_reason_ck",
				"%q is not a member of the rejection reason enum", rj.Reason)
		}
		ids[i] = rj.ID
		ordinals[i] = int32(i)
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
		ids, s.OrgID(), sources, batches, times, reasons, details, raws, ordinals,
	); err != nil {
		return mapErr(err, "record ingest rejections")
	}
	return nil
}

// ---------------------------------------------------------------------- reads

// rejectionColumns is the feed's projection, and `raw` is NOT in it.
//
// ⭐ THE LABEL SET IS EXTRACTED IN POSTGRES, not in Go, and the shape of the
// expression is the whole subtlety of this file. `raw` has FIVE writers and only
// one of them stores an alert:
//
//   - the per-alert path (`service.alertEvidence`) marshals a whole
//     `decode.Alert`, so the label set is `raw->'labels'`, an object of strings;
//   - its own re-encoding fallback writes `{"alertname":…,"error":…}`;
//   - the batch-level path (`service.batchRejectionEvidence`, B2) writes
//     `{"reason","detail","group_key","receiver","alert_count","truncated_alerts"}`;
//   - the B1 body-too-large path writes `{"reason","limit","body_bytes"}`;
//   - the undecodable / unknown-source path writes
//     `{"reason","detail","body_sample","body_bytes"}`.
//
// Only the first has a `labels` key at all, and that is not ambiguity — it is
// the truth that four of the five have no ALERT to name, because the bytes never
// became one, or because the rejection is about the batch rather than about any
// element in it. Those rows come back with an EMPTY label set, which is an
// answer; `reason` and `detail` carry the whole story for them.
//
// `jsonb_each_text` + `jsonb_object_agg` rather than passing `raw->'labels'`
// straight out: it flattens whatever is in there to an object of strings, so one
// row written by a future writer with a non-string label value cannot make the
// Go decode fail and take the entire feed dark with it. The CASE guards the
// set-returning function against a `labels` key that is not an object, which it
// would otherwise error on before any WHERE could filter it.
//
// Nothing here un-redacts anything. `alert_sources.redact_labels` is applied
// BEFORE the insert (`decode.Redactor`, §C.9.2) — a matched value is the literal
// `[redacted]` on disk — so this reads back exactly what was stored.
const rejectionColumns = `
       id, source_id, batch_id, received_at, reason, coalesce(detail, ''),
       coalesce((
         SELECT jsonb_object_agg(k, v)
           FROM jsonb_each_text(CASE WHEN jsonb_typeof(raw -> 'labels') = 'object'
                                     THEN raw -> 'labels' ELSE '{}'::jsonb END) AS t(k, v)
          WHERE v IS NOT NULL
       ), '{}'::jsonb)`

// listRejectionsSQL is the FIRST page: no position, so no bound on the partition
// key. See List for what that costs and why it is bounded.
const listRejectionsSQL = `
SELECT` + rejectionColumns + `
  FROM ingest_rejections
 WHERE org_id = $1 AND source_id = $2
   AND (cardinality($3::text[]) = 0 OR reason = ANY($3::text[]))
 ORDER BY received_at DESC, id DESC
 LIMIT $4`

// listRejectionsFromSQL is every SUBSEQUENT page.
//
// ⭐ THE `received_at <= $5` IS REDUNDANT AND IT IS THE POINT. The row comparison
// on the next line already implies it, but a row comparison is opaque to
// PARTITION PRUNING: the planner will not decompose `(received_at, id) < (a, b)`
// into a bound on the partition key, so without this line every page after the
// first still opens all ~22 daily partitions. Written as its own simple
// comparison, the planner prunes every partition whose range starts after the
// cursor — which, paging backwards through time from now, is most of them.
const listRejectionsFromSQL = `
SELECT` + rejectionColumns + `
  FROM ingest_rejections
 WHERE org_id = $1 AND source_id = $2
   AND (cardinality($3::text[]) = 0 OR reason = ANY($3::text[]))
   AND received_at <= $5
   AND (received_at, id) < ($5, $6)
 ORDER BY received_at DESC, id DESC
 LIMIT $4`

// List is THE PER-SOURCE REJECTION FEED — the screen this table exists for
// (§C.9.1), keyset-paginated and newest first.
//
// It rides `ingest_rejections_source_idx (org_id, source_id, received_at DESC)`:
// the predicate is the index's first two columns, the sort is its third, and the
// `id DESC` tiebreak is free because ids are uuidv7 and therefore already in
// received_at order.
//
// ⛔ THAT LAST SENTENCE IS AN INVARIANT, NOT AN OBSERVATION, and it is load-bearing
// for the keyset. Every rejection of one batch carries that batch's `received_at`
// to the microsecond, so within a batch the sort key is CONSTANT and `id` is the
// only thing making the order total. A uuidv7 id gives that order for free and in
// the same direction as `received_at`. An id derived from the row's content —
// which was tried, to make replay idempotent — is a hash with no ordering at all,
// and the paging immediately began to skip and repeat rows. Replay-safety belongs
// in `ingest_rejections_natural_uniq`, where it costs this nothing; the primary
// key must stay minted and time-ordered. `reason` is a filter applied over that
// index range rather
// than a fourth index column — the enum has fifteen members over a table
// retained for fourteen days, so narrowing further would buy nothing worth a
// migration.
//
// ⚠️ PARTITION SCOPE. `ingest_rejections` is PARTITION BY RANGE (received_at)
// with daily partitions, and the FIRST page carries no position and therefore no
// bound on the partition key. That page is a MergeAppend over every retained
// partition — bounded, because retention is `orgs.settings.raw_retention_days`
// (14 by default, so ~22 partitions counting the seven built ahead), and each
// one contributes an index scan that stops after `limit+1` rows. Every page
// after the first prunes, via the redundant comparison in listRejectionsFromSQL.
// A time-window argument would prune the first page too; it is deliberately not
// added here, because the operator arriving at this screen does not know when
// their alert went missing and a defaulted window would answer "no rejections"
// to a question about one that is older than the default.
func (r *RejectionRepository) List(
	ctx context.Context, s db.TenantScope, f domain.RejectionFilter, p db.Keyset,
) ([]domain.RejectionEntry, db.Cursor, error) {
	if err := requireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	if f.SourceID == uuid.Nil {
		// Without it there is no index to ride and the query degrades to a scan of
		// every partition for the org. Refused here rather than served slowly.
		return nil, db.Cursor{}, errs.New(errs.KindInternal, "rejection_source_required",
			"a rejection feed is about one source")
	}

	reasons := make([]string, 0, len(f.Reasons))
	for _, rs := range f.Reasons {
		if !rs.Valid() {
			// The column is a closed CHECK, so an unknown member cannot match a row.
			// Refusing beats silently returning an empty page that looks like "nothing
			// was rejected" — which on this screen is the one wrong answer.
			return nil, db.Cursor{}, errs.Newf(errs.KindInternal, "ingest_rejections_reason_ck",
				"%q is not a member of the rejection reason enum", rs)
		}
		reasons = append(reasons, rs.String())
	}

	limit := clampLimit(p.Limit)
	sql, args := listRejectionsSQL, []any{s.OrgID(), f.SourceID, reasons, limit + 1}
	if !p.Cursor.IsZero() {
		sql = listRejectionsFromSQL
		args = append(args, p.Cursor.SortKey.UTC(), p.Cursor.ID)
	}

	rows, err := r.db(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list ingest rejections")
	}
	defer rows.Close()

	collected := make([]domain.RejectionEntry, 0, limit+1)
	for rows.Next() {
		var (
			e      domain.RejectionEntry
			reason string
			labels []byte
		)
		if err := rows.Scan(&e.ID, &e.SourceID, &e.BatchID, &e.ReceivedAt,
			&reason, &e.Detail, &labels); err != nil {
			return nil, db.Cursor{}, mapErr(err, "scan ingest rejection")
		}
		e.Reason = domain.Reason(reason)
		e.Labels = map[string]string{}
		if len(labels) > 0 {
			if err := json.Unmarshal(labels, &e.Labels); err != nil {
				return nil, db.Cursor{}, errs.Internal("rejection_labels_undecodable", err)
			}
		}
		collected = append(collected, e)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "read ingest rejections")
	}

	page, hasMore := pageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := page[len(page)-1]
	return page, nextCursor(last.ReceivedAt, last.ID, p.Cursor.Hash, hasMore), nil
}
