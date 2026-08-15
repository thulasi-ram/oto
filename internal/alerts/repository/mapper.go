package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Sort keys accepted by the alert list (SPEC §E.3). Only these two exist, and
// they are plain strings so that the port declared in `alerts/service` needs no
// type from this package — a repository is never imported by a service.
const (
	sortLastSeenDesc  = "-last_seen_at"
	sortFirstSeenDesc = "-first_seen_at"
)

// mapErr turns a database error into an errs.Kind for this module. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the two codes it alone can name. A repository never
// validates a business rule; it does own this translation, and the constraint
// name travels out as the error Code because §L.9 makes constraint names a
// runtime contract.
//
// Nothing about the Kinds, the codes or the retry hints moved: the copy spelled
// all eight rows and spelled them the same way. TWO MESSAGES DID, and they are
// rendered rather than dropped, because `23505` and `23503` are 409s: "the row
// already exists" is now "that value is already in use", and "missing or in use"
// is now "missing or still in use". Both are this module's wording of a shared
// row rather than a fact about alerts, and `grouping` reworded identically for
// the same reason — but a 409 body reads differently after this change, so it is
// stated here rather than left to be found.
func mapErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "not_found",
		NotFoundMessage:    "no such row",
		QueryFailed:        "alerts_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
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

// decodeIntMap reads a `jsonb_object_agg(text, bigint)` aggregate.
func decodeIntMap(raw []byte) (map[string]int, error) {
	if len(raw) == 0 {
		return map[string]int{}, nil
	}
	out := map[string]int{}
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

// ------------------------------------------------------------------ tx runner

// TxRunner is the concrete half of the port `alerts/service` declares; the
// runner itself is `db.TxRunner`, in the layer permitted to name pgx.
type TxRunner = db.TxRunner

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return db.NewTxRunner(pool) }
