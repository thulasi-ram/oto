package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Pagination bounds from SPEC §E.1. They are applied here because every List in
// this package needs them and a caller that forgets is a caller that asks
// Postgres for an unbounded scan.
const (
	// DefaultLimit is the page size when a caller asks for none.
	DefaultLimit = 50
	// MaxLimit is the hard ceiling on a page.
	MaxLimit = 200
)

// Sort keys accepted by the alert list (SPEC §E.3). Only these two exist, and
// they are plain strings so that the port declared in `alerts/service` needs no
// type from this package — a repository is never imported by a service.
const (
	sortLastSeenDesc  = "-last_seen_at"
	sortFirstSeenDesc = "-first_seen_at"
)

// clampLimit applies the §E.1 page bounds.
func clampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return n
	}
}

// mapErr is the single place a SQLSTATE becomes an errs.Kind for this module
// (SPEC §L.9). The repository never validates a business rule; it does own this
// translation, and the constraint name travels out as the error Code because
// §L.9 makes constraint names a runtime contract.
func mapErr(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound("not_found", "no such row")
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		code := pg.ConstraintName
		if code == "" {
			code = "sqlstate_" + pg.Code
		}
		switch pg.Code {
		case "23505": // unique_violation
			return errs.Wrap(err, errs.KindConflict, code, "the row already exists")
		case "23503": // foreign_key_violation
			return errs.Wrap(err, errs.KindConflict, code, "the row references something that is missing or in use")
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
	return errs.Wrap(err, errs.KindInternal, "alerts_query_failed", fmt.Sprintf("could not %s", what))
}

// requireScope refuses a scope that names no tenant. A missing org_id predicate
// is a data leak, so it is refused here rather than defended against downstream.
func requireScope(s db.TenantScope) error {
	if !s.Valid() {
		return errs.Internal("missing_tenant_scope", db.ErrNoTenant)
	}
	return nil
}

// requireID refuses a zero UUID reaching a NOT NULL column. §L.9(1): catch a
// mapper bug at the boundary rather than as an opaque 23502.
func requireID(field string, id uuid.UUID) error {
	if id == uuid.Nil {
		return errs.Internal("missing_"+field, fmt.Errorf("repository: %s is required", field))
	}
	return nil
}

// ---------------------------------------------------------------- jsonb helpers

// jsonbMap renders a string map as a jsonb parameter. A nil map becomes `{}`
// rather than SQL NULL, because every jsonb column in §D.4 is NOT NULL and
// CHECKed to be an object.
func jsonbMap(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return []byte(`{}`), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, errs.Internal("jsonb_encode_failed", err)
	}
	return b, nil
}

// jsonbAny renders an arbitrary payload as a jsonb parameter.
func jsonbAny(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte(`{}`), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, errs.Internal("jsonb_encode_failed", err)
	}
	return b, nil
}

// decodeStringMap reads a jsonb object of strings back into Go.
func decodeStringMap(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errs.Internal("jsonb_decode_failed", err)
	}
	return out, nil
}

// decodeAnyMap reads a jsonb object of arbitrary values back into Go.
func decodeAnyMap(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errs.Internal("jsonb_decode_failed", err)
	}
	return out, nil
}

// ----------------------------------------------------------------- time helpers

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func timeOrZero(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.UTC()
}

func idOrNil(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
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

// nilIfEmpty collapses a pointer to an empty string onto SQL NULL. Several §D.4
// columns are CHECKed to have a non-zero length when present, so "" and absent
// are not interchangeable there.
func nilIfEmpty(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}

// isNoRows reports whether err is pgx's empty-result sentinel.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// errsMissing builds the cause for a malformed row model. §L.9(1) says the
// repository rejects one before it reaches the driver, naming the field.
func errsMissing(what string) error { return errors.New("repository: " + what) }

// sortedKeys returns a map's keys in ascending order, so a filter built from a
// map produces the same SQL — and therefore the same prepared statement — every
// time.
func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ------------------------------------------------------------- keyset helpers

// pageOf trims a slice fetched with limit+1 rows down to the page, and reports
// whether a further page exists. Fetching one extra row is what makes HasMore
// honest without a COUNT.
func pageOf[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// nextCursor mints the cursor for the page after the one just returned. The hash
// binds it to the filter it was minted under; presenting it against a different
// filter is rejected by the caller (§E.1).
func nextCursor(sortKey time.Time, id uuid.UUID, hash string, hasMore bool) db.Cursor {
	if !hasMore {
		return db.Cursor{Hash: hash}
	}
	return db.Cursor{SortKey: sortKey.UTC(), ID: id, Hash: hash, HasMore: true}
}

// ------------------------------------------------------------------ tx runner

// TxRunner runs a function inside one transaction. It is the concrete half of
// the port `alerts/service` declares, and it lives here because this is the
// layer permitted to name pgx.
type TxRunner struct{ pool *pgxpool.Pool }

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return &TxRunner{pool: pool} }

// InTx runs fn inside a transaction, committing on nil and rolling back
// otherwise. It nests safely: a ctx already carrying a transaction joins it
// rather than opening a second.
func (r *TxRunner) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.Tx(ctx, r.pool, fn)
}
