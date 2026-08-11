package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// DedupRepository is the SQL over `ingest_dedup` (§C.5).
//
// The table is DELIBERATELY UNPARTITIONED (C14). A UNIQUE index on a partitioned
// table must include the partition key, so it can only enforce uniqueness WITHIN
// a partition — and this uniqueness has to be global, or an HA Alertmanager pair
// whose two deliveries straddle midnight would record the same batch twice. A
// small, aggressively pruned side table is the price of that guarantee.
type DedupRepository struct {
	q db.Querier
}

// NewDedupRepository builds the repository over the ingest pool.
func NewDedupRepository(q db.Querier) *DedupRepository { return &DedupRepository{q: q} }

func (r *DedupRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// claimSQL is ONE statement, and that is the whole design.
//
// The CTE attempts the insert; the outer SELECT reads back whichever row now owns
// the key — the one just written, or the one that beat us. A read-then-write
// would be a race two pods lose simultaneously, and the losing side would insert
// a second `ingest_batches` row for a payload that is already on disk.
//
// `ON CONFLICT DO NOTHING` is not error handling here: a duplicate is the
// idempotency mechanism working as designed (§G.5), so it must never surface as
// an error, least of all as a 409.
// ⭐ THE HORIZON IS A PREDICATE, NOT A SWEEPER'S SCHEDULE.
//
// The `ON CONFLICT DO UPDATE … WHERE ingest_dedup.seen_at < $5` is the whole of
// domain.DedupTTL as far as behaviour is concerned. It used to be DO NOTHING,
// which meant a key suppressed replays until the `retention.prune` job happened
// to delete it — so the real window was "ten minutes, or however long the sweeper
// has been down", and a `notify.evaluate` decision could not be reasoned about
// from constants. A row past the horizon is REFRESHED and the new batch wins,
// which is the same answer the sweeper would eventually have given, taken at read
// time where it is deterministic.
//
// A row inside the horizon fails the WHERE, so the CTE returns nothing and the
// outer SELECT reports the batch that already owns the key. The two branches are
// mutually exclusive by construction, which is why this is still one statement
// and still race-free: a read-then-write is a race two pods lose simultaneously.
//
// `ON CONFLICT` is not error handling here: a duplicate is the idempotency
// mechanism working as designed (§G.5), so it must never surface as an error,
// least of all as a 409.
const claimSQL = `
WITH claimed AS (
  INSERT INTO ingest_dedup (source_id, dedup_key, batch_id, seen_at)
  VALUES ($1, $2, $3, $4)
  ON CONFLICT (source_id, dedup_key) DO UPDATE
     SET batch_id = EXCLUDED.batch_id, seen_at = EXCLUDED.seen_at
   WHERE ingest_dedup.seen_at < $5
  RETURNING batch_id
)
SELECT batch_id, true  FROM claimed
UNION ALL
SELECT batch_id, false FROM ingest_dedup
 WHERE source_id = $1 AND dedup_key = $2
   AND NOT EXISTS (SELECT 1 FROM claimed)
LIMIT 1`

// Claim inserts the dedup key, or reports the batch that already owns it.
//
// A key last seen more than domain.DedupTTL ago does NOT suppress: the same alert
// set arriving again after the replay window is the same alert set FIRING again,
// and the whole point of `refire_grace` being at least twice this wide — its
// bound floor is `2 × DedupTTL`, and it now DEFAULTS to four times it (ADR 0026)
// — is that oto's state machine gets to decide which of those it is.
func (r *DedupRepository) Claim(
	ctx context.Context, sourceID uuid.UUID, dedupKey string, batchID uuid.UUID, at time.Time,
) (domain.DedupHit, error) {
	var hit domain.DedupHit
	err := r.db(ctx).QueryRow(ctx, claimSQL, sourceID, dedupKey, batchID, at, at.Add(-domain.DedupTTL)).
		Scan(&hit.BatchID, &hit.Inserted)

	if errors.Is(err, pgx.ErrNoRows) {
		// The conflicting row was pruned between the failed insert and the read.
		// That window is microseconds wide and this is the only thing in it, so
		// treating it as "we won" is both rare and safe: the batch is recorded once
		// and processing is idempotent regardless (§G.5).
		return domain.DedupHit{BatchID: batchID, Inserted: true}, nil
	}
	if err != nil {
		return domain.DedupHit{}, mapErr(err, "claim the batch dedup key")
	}
	return hit, nil
}

const pruneSQL = `DELETE FROM ingest_dedup WHERE seen_at < $1`

// Prune deletes rows past the TTL horizon (domain.DedupTTL).
//
// The horizon is passed in rather than computed as `now() - interval '10 minutes'`
// so the clock stays injectable — a sweeper whose window only exists in SQL is a
// sweeper no test can pin.
func (r *DedupRepository) Prune(ctx context.Context, before time.Time) (int64, error) {
	tag, err := r.db(ctx).Exec(ctx, pruneSQL, before)
	if err != nil {
		return 0, mapErr(err, "prune ingest dedup")
	}
	return tag.RowsAffected(), nil
}
