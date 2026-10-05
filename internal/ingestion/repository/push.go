package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// PushRepository stamps `source_health.last_push_at` from the webhook path.
//
// ⚠️ LAYERING NOTE, the same one SourceConfigRepository carries. The table belongs
// to the `sources` module, whose `SourceRepository.TouchPush` writes the same
// column — and was never called, which is why `last_push_at` stayed NULL on every
// source oto has ever had. It cannot simply be called from here: it lives on the
// GENERAL pool and upserts, and the accept transaction runs on the INGEST pool
// (§G.10) and must not take a row lock it does not need. This is the one column
// the ingest path writes outside its own tables, and it writes nothing else.
type PushRepository struct {
	q db.Querier
}

// NewPushRepository builds the repository over the ingest pool.
func NewPushRepository(q db.Querier) *PushRepository { return &PushRepository{q: q} }

func (r *PushRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// recordPushSQL is a THROTTLED plain UPDATE, and both halves of that are load-bearing.
//
//   - PLAIN UPDATE, NOT AN UPSERT. `INSERT … ON CONFLICT DO UPDATE … WHERE` locks
//     the conflicting row even when its WHERE is false, so an upsert would put
//     every accept from one source in line behind the last one's commit. A plain
//     UPDATE whose WHERE is false locks nothing. The row always exists in
//     production — `SourceRepository.Create` seeds it, because "not yet observed"
//     is the state that blocks the reaper — and a source somehow without one gets
//     it from the next reconcile pass rather than from here.
//   - THROTTLED by `$4`, the stamp minus domain.PushStampEvery. Inside the window
//     the statement is one primary-key probe that matches nothing.
//
// It deliberately does NOT move `status`, for the reason `TouchPush` gives: a push
// proves the source can reach oto and nothing about oto reaching the source, which
// is the question the reaper guard asks. `updated_at` is advanced monotonically,
// so a pod whose clock lags the prober's cannot move it backwards.
const recordPushSQL = `
UPDATE source_health
   SET last_push_at = $3,
       updated_at   = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND source_id = $2
   AND (last_push_at IS NULL OR last_push_at < $4)`

// RecordPush records that a webhook batch from this source was accepted at `at`.
//
// It joins the caller's transaction, so the stamp commits if and only if the
// batch does: `last_push_at` never names a push oto then failed to record.
func (r *PushRepository) RecordPush(ctx context.Context, s db.TenantScope, sourceID uuid.UUID, at time.Time) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	at = at.UTC()
	if _, err := r.db(ctx).Exec(ctx, recordPushSQL,
		s.OrgID(), sourceID, at, at.Add(-domain.PushStampEvery)); err != nil {
		return mapErr(err, "record the source's last push")
	}
	return nil
}
