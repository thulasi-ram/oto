package repository

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// mapErr is the single place a SQLSTATE becomes an errs.Kind for this module
// (§L.9). A repository never validates a business rule; it does own this
// translation, and the constraint NAME is returned as the code because those
// names are a runtime contract (§D conventions).
//
// ⚠️ Everything here except NotFound must stay on a kind that the ingest handler
// maps to 503. `errs.KindConflict` would become a 409, and a 409 is a 4xx, and a
// 4xx makes Alertmanager delete the notification permanently (C4). The one unique
// constraint on this path — `ingest_dedup`'s primary key — is never surfaced as
// an error at all: it is swallowed by ON CONFLICT DO NOTHING, because a duplicate
// is the idempotency mechanism working rather than a failure (§G.5).
func mapErr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound("ingest_not_found", "no such ingest row")
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23514": // check_violation — layers 1-3 have a hole (§L.0)
			return errs.Wrap(err, errs.KindInternal, constraintCode(pg, "ingest_check_violation"),
				"an ingest row violated a database constraint")
		case "23505": // unique_violation
			return errs.Wrap(err, errs.KindInternal, constraintCode(pg, "ingest_unique_violation"),
				"an ingest row collided with an existing one")
		case "23503": // foreign_key_violation
			return errs.Wrap(err, errs.KindInternal, constraintCode(pg, "ingest_fk_violation"),
				"an ingest row references a missing row")
		}
	}
	return errs.Wrap(err, errs.KindUnavailable, "ingest_query_failed",
		fmt.Sprintf("could not %s", what))
}

// constraintCode returns the violated constraint's name, which §L.9 makes the
// machine-readable error code, falling back to a generic one when Postgres did
// not name it.
func constraintCode(pg *pgconn.PgError, fallback string) string {
	if pg.ConstraintName != "" {
		return pg.ConstraintName
	}
	return fallback
}
