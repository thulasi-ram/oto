package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/streaming/domain"
)

var (
	resourceID = uuid.MustParse("0198c0de-0000-7000-8000-000000000001")
	alertID    = uuid.MustParse("0198c0de-0000-7000-8000-0000000000a1")
	groupID    = uuid.MustParse("0198c0de-0000-7000-8000-0000000000b1")
)

// persistedKinds is the closed set of ui_events_kind_ck, restated here so that a
// kind added to the constant block without a DDL migration fails a test rather
// than a CHECK constraint at 3 a.m.
var persistedKinds = []domain.Kind{
	domain.KindAlertUpserted,
	domain.KindOccurrenceUpserted,
	domain.KindGroupUpserted,
	domain.KindEventAppended,
	domain.KindDeliveryUpdated,
	domain.KindSourceHealth,
}

func requireValidationCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, errs.KindValidation, errs.KindOf(err))
	assert.Equal(t, code, errs.CodeOf(err))
}

// ------------------------------------------------------- kind ↔ resource

func TestResourceForEveryPersistedKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind domain.Kind
		want domain.Resource
	}{
		{domain.KindAlertUpserted, domain.ResourceAlert},
		{domain.KindOccurrenceUpserted, domain.ResourceOccurrence},
		{domain.KindGroupUpserted, domain.ResourceGroup},
		{domain.KindEventAppended, domain.ResourceAlertEvent},
		{domain.KindDeliveryUpdated, domain.ResourceDelivery},
		{domain.KindSourceHealth, domain.ResourceSource},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()

			got, ok := domain.ResourceFor(tc.kind)
			require.True(t, ok, "%q must have exactly one legal resource", tc.kind)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestKindResourcePairingIsTotalAndInjective: a client switches on kind to pick a
// decoder and on resource to pick an endpoint. Every persisted kind must resolve,
// and no two kinds may claim the same resource — otherwise the resource no longer
// identifies which endpoint to re-read.
func TestKindResourcePairingIsTotalAndInjective(t *testing.T) {
	t.Parallel()

	require.Len(t, persistedKinds, 6, "ui_events_kind_ck lists exactly six kinds")

	seen := map[domain.Resource]domain.Kind{}
	for _, k := range persistedKinds {
		res, ok := domain.ResourceFor(k)
		require.True(t, ok, "kind %q has no resource", k)

		if prev, dup := seen[res]; dup {
			t.Fatalf("kinds %q and %q both map to resource %q", prev, k, res)
		}
		seen[res] = k
	}

	// And every declared Resource is reachable — a resource no kind produces is
	// an endpoint a client would never be told to re-read.
	for _, r := range []domain.Resource{
		domain.ResourceAlert, domain.ResourceOccurrence, domain.ResourceGroup,
		domain.ResourceAlertEvent, domain.ResourceDelivery, domain.ResourceSource,
	} {
		_, ok := seen[r]
		assert.True(t, ok, "no kind produces resource %q", r)
	}
}

// TestResyncIsNotAPersistedKind — resync is a statement about this connection,
// not about any resource, which is why it is absent from ui_events_kind_ck. It
// must be REJECTED rather than silently stored with some default resource.
func TestResyncIsNotAPersistedKind(t *testing.T) {
	t.Parallel()

	_, ok := domain.ResourceFor(domain.KindResync)
	assert.False(t, ok, "resync has no resource because it is about the stream")

	_, err := domain.NewAppend(domain.KindResync, resourceID, nil)
	requireValidationCode(t, err, "ui_event_kind_invalid")
}

// ---------------------------------------------------------------- NewAppend

func TestNewAppendAcceptsEveryPersistedKind(t *testing.T) {
	t.Parallel()

	for _, k := range persistedKinds {
		t.Run(string(k), func(t *testing.T) {
			t.Parallel()

			ev, err := domain.NewAppend(k, resourceID, json.RawMessage(`{"state":"firing"}`))
			require.NoError(t, err)

			wantRes, _ := domain.ResourceFor(k)
			assert.Equal(t, k, ev.Kind)
			assert.Equal(t, wantRes, ev.Resource, "the constructor, not the caller, picks the resource")
			assert.Equal(t, resourceID, ev.ResourceID)
			assert.JSONEq(t, `{"state":"firing"}`, string(ev.Payload))
		})
	}
}

func TestNewAppendRejectsUnknownKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind domain.Kind
	}{
		{name: "empty kind", kind: domain.Kind("")},
		{name: "a kind that does not exist", kind: domain.Kind("alert.deleted")},
		{name: "the resource name instead of the kind", kind: domain.Kind("alert")},
		{name: "wrong case", kind: domain.Kind("Alert.Upserted")},
		{name: "underscore instead of dot", kind: domain.Kind("alert_upserted")},
		{name: "a stream-level kind", kind: domain.KindResync},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ev, err := domain.NewAppend(tc.kind, resourceID, json.RawMessage(`{}`))
			requireValidationCode(t, err, "ui_event_kind_invalid")
			assert.Contains(t, err.Error(), string(tc.kind), "the rejection must name the kind it refused")
			assert.Equal(t, domain.NewEvent{}, ev, "a rejected append must not leak a partly built event")
		})
	}
}

func TestNewAppendRequiresAResource(t *testing.T) {
	t.Parallel()

	_, err := domain.NewAppend(domain.KindAlertUpserted, uuid.Nil, json.RawMessage(`{}`))
	requireValidationCode(t, err, "ui_event_resource_id_required")
}

func TestNewAppendDefaultsAnAbsentPayloadToAnEmptyObject(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   json.RawMessage
	}{
		{name: "nil", in: nil},
		{name: "empty slice", in: json.RawMessage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ev, err := domain.NewAppend(domain.KindSourceHealth, resourceID, tc.in)
			require.NoError(t, err)
			assert.Equal(t, `{}`, string(ev.Payload),
				"the DDL requires jsonb_typeof(payload) = 'object'; an absent payload becomes {}")
		})
	}
}

// objectOfSize builds a valid JSON object whose serialised length is exactly n.
// `{"k":"` + pad + `"}` is 8 bytes of scaffolding.
func objectOfSize(t *testing.T, n int) json.RawMessage {
	t.Helper()
	require.Greater(t, n, 8)

	b := json.RawMessage(`{"k":"` + strings.Repeat("x", n-8) + `"}`)
	require.Len(t, b, n)
	return b
}

func TestNewAppendPayloadSizeBound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		size     int
		wantCode string
	}{
		{name: "well under the cap", size: 64},
		{name: "one byte under the cap", size: domain.MaxPayloadBytes - 1},
		{name: "exactly at the cap", size: domain.MaxPayloadBytes},
		{name: "one byte over the cap", size: domain.MaxPayloadBytes + 1, wantCode: "ui_event_payload_too_large"},
		{name: "far over the cap", size: 64 * 1024, wantCode: "ui_event_payload_too_large"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ev, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, objectOfSize(t, tc.size))
			if tc.wantCode == "" {
				require.NoError(t, err)
				assert.Len(t, ev.Payload, tc.size)
				return
			}
			requireValidationCode(t, err, tc.wantCode)
		})
	}
}

// TestNewAppendRejectsNonObjectPayloads mirrors the DDL's
// jsonb_typeof(payload) = 'object'. Rejecting here keeps a 23514 — which would
// be a 500 on the ingest path — out of the write path entirely.
func TestNewAppendRejectsNonObjectPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{name: "array", in: `[]`},
		{name: "array of objects", in: `[{"a":1}]`},
		{name: "string", in: `"firing"`},
		{name: "number", in: `123`},
		{name: "bool", in: `true`},
		{name: "json null", in: `null`},
		{name: "truncated object", in: `{"a":`},
		{name: "not json at all", in: `firing`},
		{name: "two objects", in: `{}{}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, json.RawMessage(tc.in))
			requireValidationCode(t, err, "ui_event_payload_not_object")
		})
	}
}

// TestNewAppendChecksSizeBeforeShape — an oversized payload is reported as too
// large even when it is also the wrong shape, so a client that sent a 1 MB array
// is told the actionable thing.
func TestNewAppendChecksSizeBeforeShape(t *testing.T) {
	t.Parallel()

	huge := json.RawMessage(`[` + strings.Repeat("1,", domain.MaxPayloadBytes) + `1]`)

	_, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, huge)
	requireValidationCode(t, err, "ui_event_payload_too_large")
}

// ------------------------------------------------------------------ ScopeOf

func TestScopeOf(t *testing.T) {
	t.Parallel()

	other := uuid.MustParse("0198c0de-0000-7000-8000-0000000000ff")

	tests := []struct {
		name      string
		kind      domain.Kind
		resID     uuid.UUID
		payload   string
		wantAlert *uuid.UUID
		wantGroup *uuid.UUID
	}{
		{
			// An alert.upserted frame's resource_id IS the alert id.
			name: "alert.upserted infers the alert id from its own resource id",
			kind: domain.KindAlertUpserted, resID: alertID, payload: `{}`,
			wantAlert: &alertID,
		},
		{
			name: "an explicit alert_id in the payload wins over the inference",
			kind: domain.KindAlertUpserted, resID: alertID, payload: `{"alert_id":"` + other.String() + `"}`,
			wantAlert: &other,
		},
		{
			name: "group.upserted infers the group id from its own resource id",
			kind: domain.KindGroupUpserted, resID: groupID, payload: `{}`,
			wantGroup: &groupID,
		},
		{
			name: "an explicit group_id wins over the inference",
			kind: domain.KindGroupUpserted, resID: groupID, payload: `{"group_id":"` + other.String() + `"}`,
			wantGroup: &other,
		},
		{
			// The resource_id of an occurrence frame is the OCCURRENCE id.
			// Inferring it as an alert id would narrow to the wrong thing.
			name: "occurrence.upserted infers nothing from its resource id",
			kind: domain.KindOccurrenceUpserted, resID: resourceID, payload: `{}`,
		},
		{
			name: "occurrence.upserted takes the alert id the payload carries",
			kind: domain.KindOccurrenceUpserted, resID: resourceID, payload: `{"alert_id":"` + alertID.String() + `"}`,
			wantAlert: &alertID,
		},
		{
			name: "event.appended carries both narrowing ids",
			kind: domain.KindEventAppended, resID: resourceID,
			payload:   `{"alert_id":"` + alertID.String() + `","group_id":"` + groupID.String() + `"}`,
			wantAlert: &alertID, wantGroup: &groupID,
		},
		{
			name: "delivery.updated infers nothing",
			kind: domain.KindDeliveryUpdated, resID: resourceID, payload: `{}`,
		},
		{
			name: "source.health is about no alert and no group",
			kind: domain.KindSourceHealth, resID: resourceID, payload: `{"status":"unhealthy"}`,
		},
		{
			name: "resync infers nothing",
			kind: domain.KindResync, resID: resourceID, payload: `{}`,
		},
		{
			// "a malformed payload simply narrows nothing"
			name: "a malformed payload narrows nothing rather than panicking",
			kind: domain.KindEventAppended, resID: resourceID, payload: `{"alert_id":`,
		},
		{
			name: "an empty payload on an inferring kind still infers",
			kind: domain.KindAlertUpserted, resID: alertID, payload: ``,
			wantAlert: &alertID,
		},
		{
			name: "a null alert_id falls back to the inference",
			kind: domain.KindAlertUpserted, resID: alertID, payload: `{"alert_id":null}`,
			wantAlert: &alertID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotAlert, gotGroup := domain.ScopeOf(tc.kind, tc.resID, json.RawMessage(tc.payload))

			if tc.wantAlert == nil {
				assert.Nil(t, gotAlert, "alert id")
			} else {
				require.NotNil(t, gotAlert, "alert id")
				assert.Equal(t, *tc.wantAlert, *gotAlert, "alert id")
			}

			if tc.wantGroup == nil {
				assert.Nil(t, gotGroup, "group id")
			} else {
				require.NotNil(t, gotGroup, "group id")
				assert.Equal(t, *tc.wantGroup, *gotGroup, "group id")
			}
		})
	}
}

// TestScopeOfDoesNotAliasTheResourceID — the inferred pointer must not alias the
// caller's variable, or a later mutation of resourceID would silently rewrite an
// already-scoped event.
func TestScopeOfDoesNotAliasTheResourceID(t *testing.T) {
	t.Parallel()

	id := alertID
	got, _ := domain.ScopeOf(domain.KindAlertUpserted, id, json.RawMessage(`{}`))
	require.NotNil(t, got)

	id = groupID
	assert.Equal(t, alertID, *got, "the scope must have copied, not aliased")
	assert.NotEqual(t, id, *got)
}

// TestBUG_ScopeOfKeepsAHalfDecodedIDFromAMalformedPayload
//
// ScopeOf's own comment says "a malformed payload simply narrows nothing", and
// ScopeOf's doc comment says the inferred ids exist so that an alert.upserted
// frame's resource_id IS used as the alert id when "the payload does not repeat
// them".
//
// Neither holds for a payload whose `alert_id` is present but not a UUID.
// encoding/json allocates the *uuid.UUID before UnmarshalText rejects the text,
// and the decode error is discarded, so the probe comes back holding a NON-NIL
// pointer to uuid.Nil. That is not "narrowing nothing" — it is narrowing to an
// id no event can ever carry, AND it suppresses the resource-id fallback,
// because the fallback is guarded on `alertID == nil`.
//
// Consequence: an `alert.upserted` frame carrying a corrupt alert_id is
// delivered to nobody who is watching that alert. A frame silently not
// delivered is the failure mode SSE resume exists to rule out.
func TestBUG_ScopeOfKeepsAHalfDecodedIDFromAMalformedPayload(t *testing.T) {
	// Regression for the fix at event.go:184-196: encoding/json allocates the
	// *uuid.UUID before UnmarshalText rejects the text, so discarding the decode
	// error without discarding the probe left a non-nil pointer to uuid.Nil and
	// suppressed the resource-id fallback.

	// Narrows to nothing, as documented.
	gotAlert, gotGroup := domain.ScopeOf(domain.KindEventAppended, resourceID,
		json.RawMessage(`{"alert_id":"not-a-uuid"}`))
	assert.Nil(t, gotAlert, "an unparseable alert_id must narrow nothing")
	assert.Nil(t, gotGroup)

	// And the inference must still fire for a kind whose resource_id IS the id.
	gotAlert, _ = domain.ScopeOf(domain.KindAlertUpserted, alertID,
		json.RawMessage(`{"alert_id":"not-a-uuid"}`))
	require.NotNil(t, gotAlert)
	assert.Equal(t, alertID, *gotAlert,
		"a corrupt payload id must fall back to the resource id, not silently unsubscribe every watcher") // vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
}

// ----------------------------------------------------------- resume bounds

// TestReplayBoundsMatchADR0010 pins the two numbers that ARE the resume promise:
// replay is bounded by the retention of ui_events (a promise longer than the
// retention is one oto cannot keep) and by 10 000 rows.
func TestReplayBoundsMatchADR0010(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 24*time.Hour, domain.ReplayWindow,
		"ui_events is dropped at 24 hours; the replay guarantee is exactly the retention")
	assert.Equal(t, 10_000, domain.MaxReplayRows)
	assert.Equal(t, 4096, domain.MaxPayloadBytes)
}

// TestResyncReasonsAreClosed — a client that cannot name why it must refetch
// cannot tell "you fell behind" from "your cursor is too old", and those have
// different operator meanings.
func TestResyncReasonsAreClosed(t *testing.T) {
	t.Parallel()

	assert.Equal(t, domain.ResyncReason("buffer_overflow"), domain.ResyncBufferOverflow)
	assert.Equal(t, domain.ResyncReason("replay_window_exceeded"), domain.ResyncReplayWindowExceeded)
	assert.NotEqual(t, domain.ResyncBufferOverflow, domain.ResyncReplayWindowExceeded)

	// The zero value is not a reason: `Resync == ""` is how a caller says "no
	// resync", so an empty reason must never be a legal one.
	assert.NotEqual(t, domain.ResyncReason(""), domain.ResyncBufferOverflow)
	assert.NotEqual(t, domain.ResyncReason(""), domain.ResyncReplayWindowExceeded)
}

// TestNewEventCannotCarryASeq documents the ordering guarantee structurally: the
// append request has no Seq and no At field, so no caller can choose its own
// cursor value. Seq monotonicity itself is Postgres's BIGSERIAL and is not
// observable from this package.
func TestNewEventCannotCarryASeq(t *testing.T) {
	t.Parallel()

	ev, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, nil)
	require.NoError(t, err)

	// Compile-time proof: this is the complete set of fields a producer controls.
	assert.Equal(t, domain.NewEvent{
		Kind:       domain.KindAlertUpserted,
		Resource:   domain.ResourceAlert,
		ResourceID: resourceID,
		Payload:    json.RawMessage(`{}`),
	}, ev)
}

// ---------------------------------------------------------------- known bug

// TestBUG_PayloadCapMeasuresRawBytesNotColumnSize
//
// MaxPayloadBytes is documented as "the hard cap on a frame payload, MATCHING
// ui_events_payload_ck (pg_column_size(payload) <= 4096)", and isJSONObject's
// comment states the purpose of checking here is that "a 23514 ... on the ingest
// path ... would be a 500". CONTEXT.md §5b requires a bound to be identical in
// all three places it lives.
//
// The check is `len(payload) > MaxPayloadBytes` on the RAW JSON TEXT. That is
// not pg_column_size of the stored jsonb, and the two diverge in both directions:
//
//   - too strict: jsonb discards insignificant whitespace, so a semantically
//     tiny object padded with 4 090 spaces is refused here and would have stored
//     in a couple of dozen bytes. (Asserted below — this half is deterministic.)
//   - too lax, and this is the harmful one: jsonb adds a varlena header, a
//     container header and a 4-byte JEntry per key and per value, so an object
//     accepted at exactly 4 096 raw bytes stores LARGER than 4 096 and trips
//     ui_events_payload_ck — the 23514 → 500 this code exists to prevent.
func TestBUG_PayloadCapMeasuresRawBytesNotColumnSize(t *testing.T) {
	t.Skip(`BUG: streaming/domain/event.go:139 bounds len(rawJSON) but its doc comment (event.go:59-65) claims to match ui_events_payload_ck, which bounds pg_column_size(jsonb). The two are not the same measure, so a 4096-byte compact payload passes NewAppend and can still violate the CHECK — the 23514/500 that event.go:157-159 says this check exists to prevent.`)

	// Too strict: whitespace is counted here but discarded by jsonb.
	padded := json.RawMessage(`{"a":1` + strings.Repeat(" ", domain.MaxPayloadBytes) + `}`)
	_, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, padded)
	requireValidationCode(t, err, "ui_event_payload_too_large")

	var compact any
	require.NoError(t, json.Unmarshal(padded, &compact))
	reencoded, err := json.Marshal(compact)
	require.NoError(t, err)
	assert.Less(t, len(reencoded), 32,
		"the payload postgres would actually store is tiny, yet the domain refused it")
}
