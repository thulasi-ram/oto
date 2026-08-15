package repository

import (
	"github.com/thulasiram/oto/internal/platform/db"
)

// mapErr turns a database error into an errs.Kind for this package. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the codes it alone can name. `code` keeps the codes
// this package has always published per statement: `stats_quality_failed`,
// `stats_quality_scan_failed`, `stats_overview_failed`, `stats_rollup_failed`.
//
// The NotFound row is a formality: every read here is an aggregate, and an
// aggregate over nothing is a row of zeroes, not an absence.
//
// ⛔ `ComputedKeys` IS WHY A KEY VIOLATION STAYS A 500. The one write,
// RollupDay, upserts onto `alert_quality_daily`'s PRIMARY KEY as its own ON
// CONFLICT target, and every key in the row is derived from rows Postgres just
// returned. A `23505` reaching Go is a statement that drifted from the schema —
// §L.9 row 2's oto bug — and the rollup job has nothing to fix by changing the
// request.
func mapErr(err error, code, msg string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "not_found",
		NotFoundMessage:    "no such row",
		QueryFailed:        code,
		QueryFailedMessage: msg,
		ComputedKeys:       true,
	})
}
