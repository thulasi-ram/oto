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

// clampLimit applies the §E.1 page bounds, which live in `platform/db` because
// they bound `db.Keyset.Limit`. There is no OFFSET in this codebase.
func clampLimit(n int) int { return db.ClampLimit(n) }

// mapErr is the single place a SQLSTATE becomes an errs.Kind for this module
// (SPEC §L.9). The constraint name travels out as the error Code, because §L.9
// makes constraint names a runtime contract: `channels_name_uniq` is what tells
// the API layer to say "that name is taken" rather than "conflict".
func mapErr(err error, notFoundCode, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound(notFoundCode, "no such row")
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		code := pg.ConstraintName
		if code == "" {
			code = "sqlstate_" + pg.Code
		}
		switch pg.Code {
		case "23505": // unique_violation
			return errs.Wrap(err, errs.KindConflict, code, "that value is already in use")
		case "23503": // foreign_key_violation
			return errs.Wrap(err, errs.KindConflict, code,
				"the row references something that is missing or still in use")
		case "23514": // check_violation — a hole in layers 1-3
			return errs.Wrap(err, errs.KindInternal, code, "a row violated a database constraint")
		case "23502": // not_null_violation — a mapper bug
			return errs.Wrap(err, errs.KindInternal, code, "a required column was null")
		case "40001", "40P01": // serialization_failure, deadlock_detected
			return errs.Wrap(err, errs.KindConflict, code, "the transaction conflicted; retry").
				WithRetryAfter(0)
		case "57014": // query_canceled (statement_timeout)
			return errs.Wrap(err, errs.KindUnavailable, code, "the query exceeded its time budget").
				WithRetryAfter(time.Second)
		case "53300": // too_many_connections
			return errs.Wrap(err, errs.KindUnavailable, code, "the database is at capacity").
				WithRetryAfter(time.Second)
		}
	}
	return errs.Wrap(err, errs.KindInternal, "channels_query_failed", fmt.Sprintf("could not %s", what))
}

// requireScope refuses a scope that names no tenant. A missing org_id predicate
// is a data leak, so it is refused here rather than defended against downstream.
func requireScope(s db.TenantScope) error {
	if !s.Valid() {
		return errs.Internal("missing_tenant_scope", db.ErrNoTenant)
	}
	return nil
}

// requireID refuses a zero UUID reaching a NOT NULL column (§L.9(1)).
func requireID(field string, id uuid.UUID) error {
	if id == uuid.Nil {
		return errs.Internal("missing_"+field, fmt.Errorf("repository: %s is required", field))
	}
	return nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func idPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	v := id
	return &v
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// nilIfEmpty collapses "" onto SQL NULL. `channels.health_error` is CHECKed to be
// present exactly when the status is not healthy or unknown, so "" and absent are
// not interchangeable there.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// pageOf trims a slice fetched with limit+1 rows down to the page and reports
// whether a further page exists — HasMore without a COUNT.
func pageOf[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// nextCursor mints the cursor for the page after the one just returned.
func nextCursor(sortKey time.Time, id uuid.UUID, hash string, hasMore bool) db.Cursor {
	if !hasMore {
		return db.Cursor{Hash: hash}
	}
	return db.Cursor{SortKey: sortKey.UTC(), ID: id, Hash: hash, HasMore: true}
}
