package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/platform/db"
)

// mapErr turns a database error into an errs.Kind for this module. The §L.9
// table itself lives in `db.MapError` and is shared by every repository — this
// module contributes only the two codes it alone can name. The constraint name
// travels out as the error Code, because §L.9 makes constraint names a runtime
// contract: `channels_name_uniq` is what tells the API layer to say "that name is
// taken" rather than "conflict".
func mapErr(err error, notFoundCode, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           notFoundCode,
		NotFoundMessage:    "no such row",
		QueryFailed:        "channels_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
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
