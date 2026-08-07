package repository

import (
	"context"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// QueueDepthRepository measures the backlog on the `ingest` queue.
//
// ⚠️ IT IS BUILT OVER THE GENERAL POOL, DELIBERATELY. This query exists to decide
// whether to shed load; spending an ingest connection to ask "am I out of ingest
// capacity?" would make the measurement part of the problem it measures.
type QueueDepthRepository struct {
	q db.Querier
}

// NewQueueDepthRepository builds the probe over the GENERAL pool.
func NewQueueDepthRepository(q db.Querier) *QueueDepthRepository {
	return &QueueDepthRepository{q: q}
}

// depthSQL counts only the states that represent unfinished work.
//
// `completed` outnumbers everything else by orders of magnitude and says nothing
// about backlog, and `running` is capacity in use rather than work waiting. What
// is left is what a shedding decision should actually be about: jobs that exist
// and have not been picked up.
const depthSQL = `
SELECT count(*)
  FROM river_job
 WHERE queue = $1
   AND state IN ('available','retryable','scheduled','pending')`

// Depth returns the number of unstarted `ingest` jobs.
func (r *QueueDepthRepository) Depth(ctx context.Context) (int, error) {
	var n int
	if err := db.FromContext(ctx, r.q).QueryRow(ctx, depthSQL, jobs.QueueIngest).Scan(&n); err != nil {
		return 0, mapErr(err, "measure the ingest queue depth")
	}
	return n, nil
}
