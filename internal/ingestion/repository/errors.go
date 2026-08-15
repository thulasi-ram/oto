package repository

import (
	"fmt"

	"github.com/thulasiram/oto/internal/platform/db"
)

// mapErr turns a database error into an errs.Kind for this module. The §L.9
// table itself lives in `db.MapError` and is shared by every repository; what
// this module contributes is the ⚠️ below, which no other module may set.
//
// ⚠️ NOTHING HERE MAY BE A 4xx. `errs.KindConflict` would become a 409, and a 409
// is a 4xx, and a 4xx makes Alertmanager delete the notification permanently
// (C4). That is what `Never4xx` buys: `23505` and `23503` become `KindInternal`,
// a serialization failure becomes `KindUnavailable` rather than a 409, and
// anything with no §L.9 row falls through to `KindUnavailable`. The one unique
// constraint on this path — `ingest_dedup`'s primary key — is never surfaced as
// an error at all: it is swallowed by ON CONFLICT DO NOTHING, because a duplicate
// is the idempotency mechanism working rather than a failure (§G.5), which is
// also why §L.9 row 2 calls a `23505` reaching Go here internal.
//
// ⚠️ AND THE SPLIT BETWEEN THE TWO 5xx IS ALSO THIS MODULE'S, not an accident.
// `Never4xx` keeps a CONSTRAINT VIOLATION — `23514`, `23505`, `23503` — on 500,
// because a statement that violates oto's own constraint is a defect somebody has
// to fix, and puts every other failure on 503 with a retry hint, `23502`
// included. The shared table calls a `23502` KindInternal, which is right
// everywhere else and would have been a silent 503→500 move here: on this path
// the alert is already on the wire, and 503 is how oto says "come back" while
// somebody fixes the mapper.
func mapErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "ingest_not_found",
		NotFoundMessage:    "no such ingest row",
		QueryFailed:        "ingest_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
		Never4xx:           true,
		Codes:              ingestCodes,
	})
}

// ingestCodes are this module's published codes for the constraint violations
// Postgres did not name. A named constraint still wins: `ingest_batches_err_ck`
// in a log line is what tells an operator which CHECK a batch tripped.
var ingestCodes = map[string]string{
	"23514": "ingest_check_violation",
	"23505": "ingest_unique_violation",
	"23503": "ingest_fk_violation",
}
