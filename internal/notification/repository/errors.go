package repository

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// SQLSTATEs this module has an opinion about.
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
	sqlStateCheckViolation      = "23514"
)

// IsUniqueViolation reports whether err is a 23505.
//
// It is exported because a 23505 on `notifications_idem_uniq` is NOT an error:
// it is the §C.7 idempotency mechanism WORKING, and the service has to be able to
// say so (§L.9).
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == sqlStateUniqueViolation
}

// ConstraintName returns the constraint a database error names, or "".
//
// Constraint names are a RUNTIME CONTRACT in this schema (§L.9): they come back
// as errs.Error.Code, so `notifications_idem_uniq` in a log line is enough to
// diagnose a duplicate without reproducing it.
func ConstraintName(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.ConstraintName
	}
	return ""
}

// mapErr is the single place a SQLSTATE becomes an errs.Kind for this module.
//
// The repository never validates a business rule — that is the domain's job —
// but it does own this translation, because the alternative is every caller
// growing its own opinion about what "23514" means.
func mapErr(err error, notFoundCode, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound(notFoundCode, "no such "+what)
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		name := pg.ConstraintName
		if name == "" {
			name = "constraint"
		}
		switch pg.Code {
		case sqlStateUniqueViolation:
			return errs.Wrap(err, errs.KindConflict, name, "the "+what+" already exists")
		case sqlStateForeignKeyViolation:
			return errs.Wrap(err, errs.KindConflict, name, "the "+what+" references a row that is gone")
		case sqlStateCheckViolation:
			// A check violation here means a value got past domain validation. That
			// is an oto bug, not a user error, and it must read as one.
			return errs.Wrap(err, errs.KindInternal, name,
				"the "+what+" violated a database constraint")
		}
	}
	return errs.Wrap(err, errs.KindInternal, "notification_query_failed",
		fmt.Sprintf("could not %s", what))
}
