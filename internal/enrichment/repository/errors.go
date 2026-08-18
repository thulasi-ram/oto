package repository

import (
	"github.com/thulasiram/oto/internal/platform/db"
)

// mapErr turns a database error into an errs.Kind for this package. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the codes it alone can name. `code` keeps the
// read/write split this package has always published: `enrichment_query_failed`
// on a read, `enrichment_write_failed` on a write.
//
// The NotFound row is a formality here: every place a missing row MEANS
// something — a cache miss, an alert that vanished before its enrichment —
// answers it as a state before this function is reached, because absence is an
// answer on those paths, not a failure.
//
// ⛔ `ComputedKeys` IS WHY A KEY VIOLATION STAYS A 500. Every unique key this
// package can hit is absorbed before Go sees it: `enrichments`' minted `id`
// PRIMARY KEY and `enrichments_subject_uniq` are UpsertMany's ON CONFLICT
// target, and `enrichment_cache`'s `cache_key` PRIMARY KEY is Put's. The one
// foreign key a write can violate, `case_rule_fk` on BindRuleSnapshot, points at
// an append-only table whose id the enricher resolved moments earlier. A
// `23505` or `23503` reaching Go is a statement that drifted from the schema —
// §L.9 row 2's oto bug — never something the pipeline could fix by changing
// the request.
func mapErr(err error, code, msg string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "not_found",
		NotFoundMessage:    "no such row",
		QueryFailed:        code,
		QueryFailedMessage: msg,
		ComputedKeys:       true,
	})
}
