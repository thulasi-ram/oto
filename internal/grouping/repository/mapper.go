package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// clampLimit applies the §E.1 page bounds, which live in `platform/db` because
// they bound `db.Keyset.Limit`.
func clampLimit(n int) int { return db.ClampLimit(n) }

// mapErr is the single place a SQLSTATE becomes an errs.Kind for this module
// (SPEC §L.9). The constraint name travels out as the error Code because §L.9
// makes constraint names a runtime contract.
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
		case "23505":
			return errs.Wrap(err, errs.KindConflict, code, "the row already exists")
		case "23503":
			return errs.Wrap(err, errs.KindConflict, code,
				"the row references something that is missing or in use")
		case "23514":
			return errs.Wrap(err, errs.KindInternal, code, "a row violated a database constraint")
		case "23502":
			return errs.Wrap(err, errs.KindInternal, code, "a required column was null")
		case "40001", "40P01":
			return errs.Wrap(err, errs.KindConflict, code, "the transaction conflicted; retry").
				WithRetryAfter(0)
		case "57014":
			return errs.Wrap(err, errs.KindUnavailable, code, "the query exceeded its time budget").
				WithRetryAfter(time.Second)
		case "53300":
			return errs.Wrap(err, errs.KindUnavailable, code, "the database is at capacity").
				WithRetryAfter(time.Second)
		}
	}
	return errs.Wrap(err, errs.KindInternal, "grouping_query_failed", fmt.Sprintf("could not %s", what))
}

// requireScope refuses a scope that names no tenant. A missing org_id predicate
// is a data leak, not a performance bug.
func requireScope(s db.TenantScope) error {
	if !s.Valid() {
		return errs.Internal("missing_tenant_scope", db.ErrNoTenant)
	}
	return nil
}

func requireID(field string, v uuid.UUID) error {
	if v == uuid.Nil {
		return errs.Internal("missing_"+field, errors.New("repository: "+field+" is required"))
	}
	return nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

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

func timeOrZero(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.UTC()
}

func nilTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// pageOf trims a slice fetched with limit+1 rows down to the page and reports
// whether a further page exists.
func pageOf[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

func nextCursor(sortKey time.Time, id uuid.UUID, hash string, hasMore bool) db.Cursor {
	if !hasMore {
		return db.Cursor{Hash: hash}
	}
	return db.Cursor{SortKey: sortKey.UTC(), ID: id, Hash: hash, HasMore: true}
}

// TxRunner runs a function inside one transaction. It is the concrete half of
// the port `grouping/service` declares, and it lives here because this is the
// layer permitted to name pgx.
type TxRunner struct{ pool *pgxpool.Pool }

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return &TxRunner{pool: pool} }

// InTx runs fn inside a transaction. It nests safely: a ctx already carrying a
// transaction joins it rather than opening a second.
func (r *TxRunner) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.Tx(ctx, r.pool, fn)
}
