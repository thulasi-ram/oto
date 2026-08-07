package db

import (
	"time"

	"github.com/google/uuid"
)

// Keyset is the pagination request every List repository method takes (SPEC §F.5).
//
// There is no OFFSET anywhere in this codebase. An offset scan degrades linearly
// with depth and, worse, silently skips or repeats rows when the underlying set
// changes between pages — which on an alert list it always is.
type Keyset struct {
	Limit  int
	Cursor Cursor
}

// Cursor is the opaque position of a keyset page.
//
// SortKey and ID together are the ordering tuple: every list index in the schema
// ends `(… DESC, id DESC)` so that a uuidv7 breaks ties deterministically. Hash
// binds the cursor to the filter it was minted under; a cursor presented against
// a different filter is REJECTED rather than silently producing a wrong page.
type Cursor struct {
	SortKey time.Time
	ID      uuid.UUID
	Hash    string
	HasMore bool
}

// IsZero reports whether the cursor names no position, meaning "first page".
func (c Cursor) IsZero() bool { return c.SortKey.IsZero() && c.ID == uuid.Nil }

// TimeWindow bounds an AlertEvent query.
//
// From is REQUIRED for every event query: `recorded_at` is the partition key of
// `alert_events`, and this is what prunes thirteen months of partitions down to
// the one the caller meant.
type TimeWindow struct {
	From time.Time
	To   time.Time // zero = now
}
