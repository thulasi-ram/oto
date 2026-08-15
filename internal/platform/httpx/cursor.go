package httpx

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Page limits from SPEC §E.1, under the names this layer has always used. The
// numbers are `platform/db`'s — the package that owns keyset pagination and the
// `Keyset.Limit` field these bound — so these are references, not a second
// spelling. There is no `total` on an unbounded collection and there is no OFFSET
// anywhere in this codebase.
const (
	// DefaultPageLimit is the page size a caller gets for asking for nothing.
	DefaultPageLimit = db.DefaultPageLimit
	// MaxPageLimit is the ceiling. A caller wanting more is going to page anyway.
	MaxPageLimit = db.MaxPageLimit
)

// cursorPayload is the wire form of a keyset position (SPEC §E.1):
// base64url of {"k":<sort key>,"id":"<uuid>","h":"<filter hash>"}.
//
// `h` is what makes a cursor honest. A cursor minted under `?severity=critical`
// and replayed against `?severity=warning` describes a position in a sequence
// that no longer exists; without the hash the server would happily serve a page
// from the middle of the wrong list and nothing would look wrong.
type cursorPayload struct {
	K time.Time `json:"k"`
	I string    `json:"id"`
	H string    `json:"h"`
}

// EncodeCursor renders a keyset position as an opaque token. A zero position
// encodes as "" — there is no cursor for "the first page".
func EncodeCursor(c db.Cursor) string {
	if c.IsZero() {
		return ""
	}
	b, err := json.Marshal(cursorPayload{K: c.SortKey.UTC(), I: c.ID.String(), H: c.Hash})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a token and binds it to filterHash.
//
// A cursor whose hash does not match the current filter set is rejected
// `400 cursor_filter_mismatch` rather than silently producing a wrong page
// (SPEC §E.1). An empty token means "first page" and is always valid.
func DecodeCursor(token, filterHash string) (db.Cursor, error) {
	if token == "" {
		return db.Cursor{Hash: filterHash}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if err != nil {
		return db.Cursor{}, errs.Malformed("malformed_cursor", "cursor is not a valid token")
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return db.Cursor{}, errs.Malformed("malformed_cursor", "cursor is not a valid token")
	}
	id, err := uuid.Parse(p.I)
	if err != nil {
		return db.Cursor{}, errs.Malformed("malformed_cursor", "cursor is not a valid token")
	}
	if p.H != filterHash {
		return db.Cursor{}, errs.Malformed("cursor_filter_mismatch",
			"this cursor was issued for a different filter; restart from the first page")
	}
	return db.Cursor{SortKey: p.K.UTC(), ID: id, Hash: filterHash}, nil
}

// Keyset assembles the pagination request a repository takes.
func Keyset(limit int, cursor db.Cursor) db.Keyset {
	return db.Keyset{Limit: db.ClampLimit(limit), Cursor: cursor}
}

// PageOf renders the page envelope for a returned cursor.
func PageOf(next db.Cursor, limit int) Page {
	p := Page{Limit: limit, HasMore: next.HasMore}
	if next.HasMore {
		p.NextCursor = EncodeCursor(next)
	}
	return p
}

// FilterHash is the stable digest a cursor is bound to.
//
// The parts are sorted before hashing so that `?a=1&b=2` and `?b=2&a=1` — the
// same filter, written twice — mint interchangeable cursors. A caller must never
// be told its cursor is invalid because it reordered its own query string.
func FilterHash(parts ...string) string {
	cp := append([]string(nil), parts...)
	sort.Strings(cp)
	sum := sha256.Sum256([]byte(strings.Join(cp, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// DefaultTimeWindow bounds an event query whose caller supplied no lower bound.
//
// `recorded_at` is the partition key of `alert_events`; an unbounded query scans
// thirteen months of partitions (SPEC §D.12b). Defaulting is not a convenience,
// it is what keeps a timeline read off the slow path.
const DefaultTimeWindow = 30 * 24 * time.Hour

// Window builds a db.TimeWindow, defaulting `from` to now-DefaultTimeWindow.
func Window(from, to, now time.Time) db.TimeWindow {
	if from.IsZero() {
		from = now.Add(-DefaultTimeWindow)
	}
	return db.TimeWindow{From: from.UTC(), To: to.UTC()}
}
