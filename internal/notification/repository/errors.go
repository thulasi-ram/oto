package repository

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thulasiram/oto/internal/platform/db"
)

// sqlStateUniqueViolation is the one SQLSTATE this module names outside the §L.9
// table, because IsUniqueViolation below asks a question the table cannot: not
// "what Kind is this?" but "is this the idempotency mechanism working?".
const sqlStateUniqueViolation = "23505"

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

// mapErr turns a database error into an errs.Kind for this module. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the two codes it alone can name.
//
// The repository never validates a business rule — that is the domain's job —
// but it does own this translation, because the alternative is every caller
// growing its own opinion about what "23514" means. This module used to spell
// three rows of the table and drop the rest onto `KindInternal`, which turned a
// statement timeout into a 500 where §L.9 says 503.
//
// One smaller change came with that, and it is deliberate. A `40001`/`40P01` is
// now a 409 rather than a 500: §L.1 defines KindConflict as "the caller must
// re-read and retry", which is what a serialization failure is, and eight of the
// ten copies already said so — this module simply never wrote the row. Delivery
// is job-driven, and `jobs.Classify` puts KindConflict and KindInternal in the
// same ClassRetryable, so no retry, backoff or dead-lettering behaviour moves;
// what changes is the status the read paths answer with.
func mapErr(err error, notFoundCode, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           notFoundCode,
		NotFoundMessage:    "no such " + what,
		QueryFailed:        "notification_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
		Codes:              notificationCodes,
	})
}

// notificationCodes is the fallback code for a constraint Postgres did not name,
// restored verbatim from this module's own copy of the table rather than
// improved: the bare word is a poor code, but it is the one that was published,
// and this ticket is not the place to change what a client reads. It is barely
// reachable — every unique and foreign key on these tables is named, and
// `notifications_idem_uniq` arriving as the Code is the whole point (§L.9).
var notificationCodes = map[string]string{
	"23505": "constraint",
	"23503": "constraint",
	"23514": "constraint",
}
