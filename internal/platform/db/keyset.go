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

// PageOf trims a slice fetched with limit+1 rows down to the page and reports
// whether a further page exists. Fetching one extra row is what makes HasMore
// honest without a COUNT — and on the partitioned tables there is no COUNT to
// be had anyway, because counting means touching every partition. It lives here,
// beside Cursor, because re-deriving the limit+1 trick per query is how one List
// quietly stops reporting HasMore.
func PageOf[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// NextCursor mints the cursor for the page after the one just returned. The
// hash travels through untouched: it binds the cursor to the filter it was
// minted under, `platform/httpx` rejects a cursor presented against a different
// filter (§E.1), and a repository is not the layer that knows what the filter
// looked like on the wire.
func NextCursor(sortKey time.Time, id uuid.UUID, hash string, hasMore bool) Cursor {
	if !hasMore {
		return Cursor{Hash: hash}
	}
	return Cursor{SortKey: sortKey.UTC(), ID: id, Hash: hash, HasMore: true}
}

// TimeWindow bounds an AlertEvent query.
//
// From is REQUIRED for every event query: `recorded_at` is the partition key of
// `alert_events`, and this is what prunes thirteen months of partitions down to
// the one the caller meant.
type TimeWindow struct {
	From time.Time
	To   time.Time // zero = now
}
