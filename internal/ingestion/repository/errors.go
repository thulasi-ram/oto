package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thulasiram/oto/internal/platform/db"
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

// ------------------------------------------------------------- keyset helpers
//
// The same three helpers `alerts/repository` has, for the same reason: this
// package cannot import that one (two repositories never see each other), and
// re-deriving the limit+1 trick per query is how one List quietly stops
// reporting HasMore.

// clampLimit applies the §E.1 page bounds, which live in `platform/db` because
// they bound `db.Keyset.Limit`. Every List here calls it: a List that forgets is
// a List that asks Postgres for an unbounded scan.
func clampLimit(n int) int { return db.ClampLimit(n) }

// pageOf trims a slice fetched with limit+1 rows down to the page and reports
// whether a further page exists. The extra row is what makes HasMore honest
// without a COUNT — and there is no COUNT to be had here anyway, because these
// are partitioned tables and counting one means touching every partition.
func pageOf[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// nextCursor mints the cursor for the page after the one just returned. The hash
// travels through untouched: `platform/httpx` binds it to the filter and rejects
// a cursor presented against a different one (§E.1), and a repository is not the
// layer that knows what the filter looked like on the wire.
func nextCursor(sortKey time.Time, id uuid.UUID, hash string, hasMore bool) db.Cursor {
	if !hasMore {
		return db.Cursor{Hash: hash}
	}
	return db.Cursor{SortKey: sortKey.UTC(), ID: id, Hash: hash, HasMore: true}
}

// requireScope refuses a scope that names no tenant. A missing `org_id`
// predicate is a data leak, so it is refused here rather than defended against
// downstream.
func requireScope(s db.TenantScope) error {
	if !s.Valid() {
		return errs.Internal("missing_tenant_scope", db.ErrNoTenant)
	}
	return nil
}
