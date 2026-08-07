package domain

import (
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

// MaxPayloadBytes is the hard cap on a frame payload, matching
// `ui_events_payload_ck` (pg_column_size(payload) <= 4096).
//
// The envelope is a CHANGE NOTICE, not a resource: enough to update a list row
// without a refetch, and no more. A fat payload turns a storm into a bandwidth
// incident and defeats the point of a client that re-reads for detail.
const MaxPayloadBytes = 4096

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
	if len(payload) > MaxPayloadBytes {
		return NewEvent{}, errs.Validation("ui_event_payload_too_large",
			"ui event payload exceeds the 4096-byte envelope cap")
	}
	if !isJSONObject(payload) {
		return NewEvent{}, errs.Validation("ui_event_payload_not_object",
			"ui event payload must be a JSON object")
	}

	return NewEvent{Kind: kind, Resource: resource, ResourceID: resourceID, Payload: payload}, nil
}

// ResourceFor returns the Resource a Kind is always about.
func ResourceFor(k Kind) (Resource, bool) {
	r, ok := kindResource[k]
	return r, ok
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
		_ = json.Unmarshal(payload, &p) // a malformed payload simply narrows nothing
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
