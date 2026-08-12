package repository

import (
	"github.com/thulasiram/oto/internal/platform/db"
)

// mapErr turns a database error into an errs.Kind for this module. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the two codes it alone can name. A repository never
// validates a business rule; it does own this translation, and the CONSTRAINT
// NAME becomes the machine code because those names are a runtime contract
// (CONTEXT.md §6).
//
// notFoundCode names what was not found — `user_not_found`, `token_not_found` —
// so a 404 is diagnosable without the caller learning anything it could not
// already infer from the URL it requested.
//
// ⚠️ No message reaching here may carry a pgx type, a row struct or a SQL string:
// a 404's message is rendered to the caller, and §L.9(3) binds the rest.
func mapErr(err error, notFoundCode, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           notFoundCode,
		NotFoundMessage:    "no such " + what,
		QueryFailed:        "identity_query_failed",
		QueryFailedMessage: "an internal error occurred",
		Codes:              identityCodes,
	})
}

// identityCodes are the error codes this module has always published for the
// SQLSTATEs Postgres names no constraint for. They are a CONTRACT, not a
// convenience: `identity_serialization_failure` is what a client retrying a
// concurrent token mint branches on, and it never carried a constraint name to
// fall back to. Where a name exists — `users_email_uniq` on a `23505` — the name
// still wins, exactly as it did before.
var identityCodes = map[string]string{
	"23505": "identity_conflict",
	"23503": "identity_fk_violation",
	"23514": "identity_check_violation",
	"23502": "identity_not_null_violation",
	"40001": "identity_serialization_failure",
	"40P01": "identity_serialization_failure",
	"57014": "identity_query_timeout",
	"53300": "identity_overloaded",
}
