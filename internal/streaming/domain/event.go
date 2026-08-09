package domain

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Kind is the closed set of UI event types (SPEC §E.4, openapi `UiEventKind`).
// It is the SSE `event:` field and the `kind` member of every frame.
type Kind string

// The persisted kinds. These are exactly `ui_events_kind_ck` in the DDL, and the
// three places that state this bound — this constant block, the DTO enum and the
// CHECK constraint — must stay identical.
const (
	KindAlertUpserted      Kind = "alert.upserted"
	KindOccurrenceUpserted Kind = "occurrence.upserted"
	KindGroupUpserted      Kind = "group.upserted"
	KindEventAppended      Kind = "event.appended"
	KindDeliveryUpdated    Kind = "delivery.updated"
	KindSourceHealth       Kind = "source.health"

	// KindResync is a STREAM-LEVEL kind and is never persisted to `ui_events` —
	// it is a statement about this connection, not about any resource, which is
	// why it is absent from ui_events_kind_ck. A client receiving it MUST refetch.
	KindResync Kind = "resync"
)

// Resource names which endpoint a client should re-read for detail
// (SPEC §E.4, `ui_events_res_ck`).
type Resource string

// The persisted resources.
const (
	ResourceAlert      Resource = "alert"
	ResourceOccurrence Resource = "occurrence"
	ResourceGroup      Resource = "group"
	ResourceAlertEvent Resource = "alert_event"
	ResourceDelivery   Resource = "delivery"
	ResourceSource     Resource = "source"
)

// kindResource is the one legal Resource for each Kind. The pairing is not free:
// a client switches on `kind` to pick a decoder and on `resource` to pick an
// endpoint, and a mismatched pair makes it do both wrongly.
var kindResource = map[Kind]Resource{
	KindAlertUpserted:      ResourceAlert,
	KindOccurrenceUpserted: ResourceOccurrence,
	KindGroupUpserted:      ResourceGroup,
	KindEventAppended:      ResourceAlertEvent,
	KindDeliveryUpdated:    ResourceDelivery,
	KindSourceHealth:       ResourceSource,
}

// MaxPayloadBytes is the hard cap on a frame payload, measured on the COMPACTED
// JSON — the payload with insignificant whitespace removed, which is exactly the
// form NewAppend stores and therefore exactly the bytes Postgres is handed.
//
// The envelope is a CHANGE NOTICE, not a resource: enough to update a list row
// without a refetch, and no more. A fat payload turns a storm into a bandwidth
// incident and defeats the point of a client that re-reads for detail.
//
// ⛔ THIS IS NOT THE SAME MEASURE AS `ui_events_payload_ck`, AND IT MUST NOT BE
// MADE TO LOOK LIKE ONE. The CHECK bounds `pg_column_size(payload)`, the size of
// the STORED jsonb; this bounds the length of the compact JSON text. jsonb is not
// text — see MaxStoredPayloadBytes for what it costs — so the two numbers
// deliberately DIFFER, and the relationship between them, not their equality, is
// the invariant: MaxPayloadBytes is chosen so that anything this constructor
// accepts is comfortably inside the CHECK. Go is the rule; the CHECK is the
// backstop for a writer that bypassed this constructor.
const MaxPayloadBytes = 4096

// MaxStoredPayloadBytes is the DDL backstop — `ui_events_payload_ck`, which is
// `pg_column_size(payload) <= 16384` as of 00031_ui_events_payload_ck.sql. It is
// restated here so that the two bounds can be compared in one place (and by
// TestPayloadCapsAreStricterInGoThanInTheDDL) rather than in two files that
// drifted apart once already.
//
// # WHY 16384 AND NOT 4096 — THE ARITHMETIC
//
// jsonb is a parsed binary form, not the text. For an OBJECT of N pairs Postgres
// stores (all figures measured against postgres:17-alpine, the pinned test image):
//
//	  4 bytes  varlena header
//	+ 4 bytes  JsonbContainer header
//	+ 4 bytes  JEntry per KEY   ─┐ 8N
//	+ 4 bytes  JEntry per VALUE ─┘
//	+ the key strings, unquoted
//	+ the values, each INTALIGNed to a 4-byte boundary
//
// Numbers are the expensive value shape: a jsonb number is a full `numeric`, and
// the smallest one is 8 bytes (`pg_column_size(1::numeric)` = 8) against a single
// byte of JSON text. Booleans and null cost nothing beyond their JEntry; strings
// cost their bytes. So the worst payload is the one with the MOST pairs and
// single-digit numeric values, and the pair count is what has to be maximised.
//
// Compact text per pair is `"k":1,` — 6 bytes for a one-character key, 7 for a
// two-character one — and the keys must be DISTINCT, because jsonb keeps only the
// last of a duplicate key and duplicates would therefore make the row SMALLER.
// A JSON string may hold 0x20..0x7F unescaped except `"` and `\`, so there are 94
// one-character keys and the rest must be two characters:
//
//	4096 >= 2 + 5*94 + 6*(N-94) + (N-1)  =>  7N - 93 <= 4096  =>  N <= 598
//
// and 598 pairs store as
//
//	8 (headers) + 8*598 (JEntries) + 1102 (94*1 + 504*2 key bytes)
//	  + 2 (alignment) + 8*598 (numerics)                          = 10680 bytes
//
// Confirmed, not derived: `pg_column_size()` on exactly that object, generated by
// TestWorstCaseJSONBOverheadStoresUnderTheDDLCap, returns 10680 for 4093 bytes of
// compact text — a ratio of 2.61. 16384 clears it by 5704 bytes (1.53x), it is a
// round 4x MaxPayloadBytes, and no payload NewAppend accepts can approach it.
//
// THE NUMBER THAT MATTERS IS THE UNCOMPRESSED ONE. A CHECK runs before TOASTing,
// so on INSERT — the only moment that can reject a write — it measures the full
// 10680-byte datum, even though that row compresses to ~1.5 kB on disk and
// `pg_column_size` reports the smaller figure once it is read back out of the
// heap. Sizing the bound against the compressed number would be sizing it against
// a measurement Postgres never applies to an incoming row.
const MaxStoredPayloadBytes = 16384

// ResyncReason is why a client must refetch (openapi `ResyncData.reason`).
type ResyncReason string

// The closed ResyncReason set.
const (
	// ResyncBufferOverflow: this connection fell behind and its buffer was
	// dropped. oto never blocks a writer for a slow reader.
	ResyncBufferOverflow ResyncReason = "buffer_overflow"
	// ResyncReplayWindowExceeded: the requested Last-Event-ID is older than the
	// 24-hour replay window, or the gap exceeds MaxReplayRows.
	ResyncReplayWindowExceeded ResyncReason = "replay_window_exceeded"
)

// Replay bounds from SPEC §E.4, binding.
const (
	// ReplayWindow is the retention of `ui_events`, and therefore exactly the
	// replay guarantee. The partitions are hourly and dropped at 24 hours; a
	// promise longer than the retention would be a promise oto cannot keep.
	ReplayWindow = 24 * time.Hour
	// MaxReplayRows caps one resume. Beyond it a `resync` is cheaper for both
	// sides than tens of thousands of frames the client will collapse anyway.
	MaxReplayRows = 10_000
)

// Event is one row of `ui_events` — the durable, ordered spine of the SSE stream.
type Event struct {
	// Seq is the BIGSERIAL, surfaced as the SSE `id:` and echoed back as
	// Last-Event-ID. STRICTLY MONOTONIC, NOT CONTIGUOUS: it is allocated from a
	// single sequence across every org, and a rolled-back transaction consumes a
	// value. A client that treats a gap as loss will resync forever.
	Seq        int64
	OrgID      uuid.UUID
	Kind       Kind
	Resource   Resource
	ResourceID uuid.UUID
	Payload    json.RawMessage
	At         time.Time

	// AlertID and GroupID are DERIVED from Payload at load time, not stored
	// columns. They exist so the hub can honour the `alert_id` / `group_id`
	// narrowing of §E.4 without re-parsing JSON once per subscriber per event.
	AlertID *uuid.UUID
	GroupID *uuid.UUID
}

// NewEvent is an append request. Seq and At are assigned by Postgres, so they are
// deliberately absent: a caller that could choose its own seq could break the
// only ordering guarantee the stream has.
type NewEvent struct {
	Kind       Kind
	Resource   Resource
	ResourceID uuid.UUID
	Payload    json.RawMessage
}

// NewAppend builds a validated append request.
//
// This is the layer-3 invariant of SPEC §L: if you can construct it, it is valid.
// There is no separate Validate() to forget to call.
func NewAppend(kind Kind, resourceID uuid.UUID, payload json.RawMessage) (NewEvent, error) {
	resource, ok := kindResource[kind]
	if !ok {
		return NewEvent{}, errs.Validation("ui_event_kind_invalid",
			"unknown ui event kind "+string(kind))
	}
	if resourceID == uuid.Nil {
		return NewEvent{}, errs.Validation("ui_event_resource_id_required",
			"a ui event must name the resource it is about")
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	// COMPACT FIRST, THEN MEASURE, THEN STORE WHAT WAS MEASURED. Whitespace is
	// insignificant to JSON and is discarded by jsonb, so bounding the raw text
	// refused a semantically tiny object padded with 4 kB of spaces — a rejection
	// with no cause. Measuring the compacted form removes that divergence, and
	// storing the compacted form is what makes the measurement honest: what was
	// weighed is exactly what Postgres is handed.
	compact, err := compactJSON(payload)
	if err != nil {
		// ORDERING: this necessarily precedes the size check, unlike the shape
		// check below. Compaction IS the parse, so bytes that do not scan as one
		// complete JSON value have no compacted length to measure — there is no
		// size answer to give. They are reported as not-an-object, which is the
		// true and the actionable statement about them.
		//
		// The documented "size before shape" ordering is unchanged for everything
		// that IS valid JSON: a 1 MB array still compacts, and is still reported as
		// too large rather than as the wrong shape.
		return NewEvent{}, errs.Validation("ui_event_payload_not_object",
			"ui event payload must be a JSON object")
	}
	if len(compact) > MaxPayloadBytes {
		return NewEvent{}, errs.Validation("ui_event_payload_too_large",
			"ui event payload exceeds the 4096-byte envelope cap")
	}
	if !isJSONObject(compact) {
		return NewEvent{}, errs.Validation("ui_event_payload_not_object",
			"ui event payload must be a JSON object")
	}

	return NewEvent{Kind: kind, Resource: resource, ResourceID: resourceID, Payload: compact}, nil
}

// ResourceFor returns the Resource a Kind is always about.
func ResourceFor(k Kind) (Resource, bool) {
	r, ok := kindResource[k]
	return r, ok
}

// compactJSON strips insignificant whitespace, returning the exact bytes jsonb
// would keep. It errors on anything that is not one complete, well-formed JSON
// value — including trailing garbage such as `{}{}`, which the scanner rejects
// once the first value has closed.
//
// The result is always a FRESH buffer, never a view onto the caller's slice, so a
// producer that reuses its payload buffer cannot rewrite an event it has already
// handed over.
func compactJSON(b json.RawMessage) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.Grow(len(b))
	if err := json.Compact(&buf, b); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

// isJSONObject reports whether b decodes as a JSON object, mirroring the DDL's
// `jsonb_typeof(payload) = 'object'`. Rejecting here rather than at the CHECK
// keeps a 23514 out of the ingest path, where it would be a 500.
func isJSONObject(b []byte) bool {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
}

// scopeProbe is the subset of every payload shape that carries a narrowing id.
// Only these two are declared: `resources`, `alert_id` and `group_id` are the
// only filters §E.4 permits, and decoding more would be decoding the payload
// twice for nothing.
type scopeProbe struct {
	AlertID *uuid.UUID `json:"alert_id"`
	GroupID *uuid.UUID `json:"group_id"`
}

// ScopeOf extracts the narrowing ids a payload carries, filling in the ones
// implied by the event's own resource.
//
// An `alert.upserted` frame's resource_id IS the alert id, and a `group.upserted`
// frame's IS the group id; the payload does not repeat them, so they are inferred
// here rather than being absent from the filter.
func ScopeOf(kind Kind, resourceID uuid.UUID, payload json.RawMessage) (alertID, groupID *uuid.UUID) {
	var p scopeProbe
	if len(payload) > 0 {
		// A malformed payload narrows nothing — but the zeroing is load-bearing,
		// not defensive. encoding/json ALLOCATES a *uuid.UUID before calling
		// UnmarshalText on it, so a payload carrying `"alert_id":"not-a-uuid"`
		// leaves p.AlertID non-nil and pointing at uuid.Nil. Discarding the error
		// without discarding p would then skip the resource-id fallback below
		// (guarded on `alertID == nil`) and scope the frame to uuid.Nil, which no
		// subscriber matches — a silently undelivered frame, which is the one
		// outcome durable resume exists to rule out (ADR 0010).
		if err := json.Unmarshal(payload, &p); err != nil {
			p = scopeProbe{}
		}
	}
	alertID, groupID = p.AlertID, p.GroupID

	switch kind {
	case KindAlertUpserted:
		if alertID == nil {
			id := resourceID
			alertID = &id
		}
	case KindGroupUpserted:
		if groupID == nil {
			id := resourceID
			groupID = &id
		}
	case KindOccurrenceUpserted, KindEventAppended, KindDeliveryUpdated, KindSourceHealth, KindResync:
	}
	return alertID, groupID
}
