package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// mapErr turns a database error into an errs.Kind for this module. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the two codes it alone can name. The CONSTRAINT NAME
// travels out as the error Code because §L.9 makes constraint names a runtime
// contract: `alert_sources_name_uniq` is what lets the API say "that name is
// taken" instead of "conflict".
func mapErr(err error, notFoundCode, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           notFoundCode,
		NotFoundMessage:    "no such row",
		QueryFailed:        "sources_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// errsMissing builds the cause for a malformed row model.
func errsMissing(what string) error { return errors.New("repository: " + what) }

// ---------------------------------------------------------------- jsonb helpers

// jsonbMap renders a string map as a jsonb parameter. A nil map becomes `{}`
// rather than SQL NULL, because `alert_sources.inject_labels` is NOT NULL and
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

// ----------------------------------------------------------------- misc helpers

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// nilIfEmpty collapses "" onto SQL NULL. `alert_sources.prometheus_url` is
// CHECKed to be a well-formed URL when present, so "" and absent are not
// interchangeable there.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
