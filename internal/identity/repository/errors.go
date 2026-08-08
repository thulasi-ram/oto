package repository

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// mapErr is the single place a SQLSTATE becomes an errs.Kind for this module
// (SPEC §L.9). A repository never validates a business rule; it does own this
// translation, and the CONSTRAINT NAME becomes the machine code because those
// names are a runtime contract (CONTEXT.md §6).
//
// notFoundCode names what was not found — `user_not_found`, `token_not_found` —
// so a 404 is diagnosable without the caller learning anything it could not
// already infer from the URL it requested.
//
// ⚠️ No branch of this function may put a pgx type, a row struct or a SQL string
// into a message. Everything returned here is rendered to a caller.
func mapErr(err error, notFoundCode, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound(notFoundCode, "no such "+what)
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505": // unique_violation — a key the USER supplied (email, slug)
			return errs.Wrap(err, errs.KindConflict, constraintCode(pg, "identity_conflict"),
				"that "+what+" already exists")
		case "23503": // foreign_key_violation
			return errs.Wrap(err, errs.KindConflict, constraintCode(pg, "identity_fk_violation"),
				"a referenced row is missing or still in use")
		case "23514": // check_violation — layers 1-3 have a hole (§L.0)
			return errs.Wrap(err, errs.KindInternal, constraintCode(pg, "identity_check_violation"),
				"an internal error occurred")
		case "23502": // not_null_violation — a mapper bug
			return errs.Wrap(err, errs.KindInternal, constraintCode(pg, "identity_not_null_violation"),
				"an internal error occurred")
		case "40001", "40P01": // serialization_failure / deadlock_detected
			return errs.Wrap(err, errs.KindConflict, "identity_serialization_failure",
				"the request conflicted with a concurrent one; retry")
		case "57014": // query_canceled (statement_timeout)
			return errs.Wrap(err, errs.KindUnavailable, "identity_query_timeout",
				"the request could not be served in time; retry")
		case "53300": // too_many_connections
			return errs.Wrap(err, errs.KindUnavailable, "identity_overloaded",
				"the service is busy; retry")
		}
	}

	return errs.Wrap(err, errs.KindInternal, "identity_query_failed", "an internal error occurred")
}

// constraintCode returns the violated constraint's name, which §L.9 makes the
// machine-readable error code, falling back when Postgres did not name it.
func constraintCode(pg *pgconn.PgError, fallback string) string {
	if pg.ConstraintName != "" {
		return pg.ConstraintName
	}
	return fallback
}
