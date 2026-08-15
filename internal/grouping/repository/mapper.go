package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// mapErr turns a database error into an errs.Kind for this module. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the two codes it alone can name. The constraint name
// travels out as the error Code because §L.9 makes constraint names a runtime
// contract.
//
// Nothing about the mapping moved when this stopped being its own copy: the copy
// spelled all eight rows, with the same Kinds, the same codes and the same retry
// hints. Two messages were its own wording of a shared row — "the row already
// exists" for `23505` and "missing or in use" for `23503` — and they are now the
// table's, because a message that differs per module is a drift waiting to
// happen, not a fact about grouping.
func mapErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "not_found",
		NotFoundMessage:    "no such row",
		QueryFailed:        "grouping_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
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

// TxRunner is the concrete half of the port `grouping/service` declares; the
// runner itself is `db.TxRunner`, in the layer permitted to name pgx.
type TxRunner = db.TxRunner

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return db.NewTxRunner(pool) }
