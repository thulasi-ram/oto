package domain_test

import (
	"encoding/json"
	"fmt"
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

// TestNewAppendReportsMalformedJSONAsShapeRegardlessOfSize is the ONE documented
// exception to size-before-shape, and it is forced by compaction rather than
// chosen.
//
// The cap is measured on the COMPACTED payload, and compaction is the parse. Bytes
// that do not scan as one complete JSON value therefore have no compacted length
// to report — there is no size answer to give, at any size. They are reported as
// not-an-object, which is both true and the only actionable thing to say.
//
// Note the contrast with the test above: a huge but VALID payload still compacts,
// and is still reported as too large. Size-before-shape holds for everything that
// has a size.
func TestNewAppendReportsMalformedJSONAsShapeRegardlessOfSize(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
	}{
		{name: "tiny truncated object", in: `{"a":`},
		{name: "huge truncated object", in: `{"a":"` + strings.Repeat("x", 64*1024)},
		{name: "huge trailing garbage", in: `{}` + strings.Repeat("!", 64*1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, json.RawMessage(tc.in))
			requireValidationCode(t, err, "ui_event_payload_not_object")
		})
	}
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

// ------------------------------------------------- one measure, honestly taken

// TestPayloadCapMeasuresCompactedBytes is the regression for the two-rulers bug.
//
// MaxPayloadBytes used to bound `len(payload)` — the RAW JSON TEXT — while
// `ui_events_payload_ck` bounded `pg_column_size(payload)`, the STORED jsonb.
// Those are not the same quantity and they diverged in BOTH directions:
//
//   - TOO STRICT: jsonb discards insignificant whitespace, so a semantically tiny
//     object padded with 4 096 spaces was refused for a size it would never have
//     occupied. That half is asserted here — it is pure and deterministic.
//   - TOO LAX, and the harmful one: jsonb adds a varlena header, a container
//     header and a 4-byte JEntry per key AND per value, so an object accepted at
//     4 096 raw bytes stores far LARGER and tripped the CHECK — the 23514 → 500
//     that this check exists to prevent. That half cannot be proved in a pure
//     package, because only Postgres knows pg_column_size; it is proved against a
//     real database by TestUIEventPayloadAtTheGoCapStores in test/integration.
//
// The fix has two halves: NewAppend compacts before measuring AND STORES THE
// COMPACTED FORM (so what was weighed is what is written), and 00031 raised the
// DDL bound to 16384 so Go is reliably the stricter of the two.
func TestPayloadCapMeasuresCompactedBytes(t *testing.T) {
	t.Parallel()

	// DIRECTION 1 — too strict, now fixed. Over the cap as text, trivial as jsonb.
	padded := json.RawMessage(`{"a":1` + strings.Repeat(" ", domain.MaxPayloadBytes) + `}`)
	require.Greater(t, len(padded), domain.MaxPayloadBytes,
		"the fixture must be over the cap as raw text, or it proves nothing")

	ev, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, padded)
	require.NoError(t, err,
		"whitespace is insignificant to JSON and free in jsonb; refusing it was a rejection with no cause")

	// And it must be COMPACTED, not merely tolerated. Measuring the compact form
	// and then storing the padded bytes would be measuring one thing and writing
	// another — the same class of mistake, one layer down.
	assert.Equal(t, `{"a":1}`, string(ev.Payload),
		"the stored payload is the form that was measured")
	assert.LessOrEqual(t, len(ev.Payload), domain.MaxPayloadBytes)
}

// TestNewAppendStoresTheCompactedPayload — compaction is not only for oversized
// input. Every payload is stored compact, so the bytes Postgres receives are
// always the bytes NewAppend weighed.
func TestNewAppendStoresTheCompactedPayload(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "already compact is unchanged", in: `{"a":1}`, want: `{"a":1}`},
		{name: "indented", in: "{\n  \"a\": 1,\n  \"b\": [1, 2]\n}", want: `{"a":1,"b":[1,2]}`},
		{name: "leading and trailing space", in: "  \t{\"a\":1}\n", want: `{"a":1}`},
		{name: "empty object with space", in: `{ }`, want: `{}`},
		{
			// Compaction is whitespace removal and nothing else: it must not
			// renormalise numbers, reorder keys or unescape strings, all of which
			// would change what the client is told changed.
			name: "values are byte-preserved",
			in:   `{ "b" : 1.0 , "a" : "é<&>" }`,
			want: `{"b":1.0,"a":"é<&>"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ev, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, json.RawMessage(tc.in))
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(ev.Payload))
		})
	}
}

// TestNewAppendDoesNotAliasTheCallerPayload — the stored payload must be a fresh
// buffer. A producer that reuses one scratch buffer per event would otherwise be
// able to rewrite an envelope it has already handed over, and the row Postgres
// gets would not be the row that was validated.
func TestNewAppendDoesNotAliasTheCallerPayload(t *testing.T) {
	t.Parallel()

	buf := []byte(`{"state":"firing"}`)
	ev, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, buf)
	require.NoError(t, err)

	copy(buf, `{"state":"XXXXXXX"}`)
	assert.Equal(t, `{"state":"firing"}`, string(ev.Payload))
}

// TestPayloadCapsAreStricterInGoThanInTheDDL pins the RELATIONSHIP between the two
// bounds, which is the invariant — not their equality, which is what broke.
//
// They measure different quantities in different units (compact JSON text vs.
// stored jsonb), so they cannot be equal and must not be "synchronised". What must
// hold is the ordering: Go is the rule, the CHECK is the backstop, and no payload
// Go accepts can reach the CHECK. The worst-case arithmetic behind the specific
// numbers is on domain.MaxStoredPayloadBytes and is verified against a real
// Postgres by TestWorstCaseJSONBOverheadStoresUnderTheDDLCap.
func TestPayloadCapsAreStricterInGoThanInTheDDL(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 4096, domain.MaxPayloadBytes, "compact JSON text bytes")
	assert.Equal(t, 16384, domain.MaxStoredPayloadBytes,
		"stored jsonb bytes — ui_events_payload_ck as of 00031_ui_events_payload_ck.sql")

	// 2.61 is the measured worst-case stored/compact ratio (10680 / 4093).
	assert.Greater(t, float64(domain.MaxStoredPayloadBytes), 2.61*float64(domain.MaxPayloadBytes),
		"the DDL bound must exceed the WORST-CASE stored size of anything Go accepts, not just the Go bound")
}

// TestManySmallFieldsAtTheCapAreAccepted is the domain half of the "too lax"
// direction: the payload shape that used to pass Go and then be refused by the
// CHECK is still accepted here, unchanged. It is the DDL that moved.
//
// The storage half — that this object now actually survives an INSERT — is
// TestUIEventPayloadAtTheGoCapStores, which needs a real Postgres.
func TestManySmallFieldsAtTheCapAreAccepted(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteByte('{')
	for i := 0; b.Len() < domain.MaxPayloadBytes-16; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%03d":%d`, i, i%10)
	}
	// Pad the final value out so the object lands on EXACTLY the cap.
	b.WriteString(`,"z":` + strings.Repeat("9", domain.MaxPayloadBytes-b.Len()-6) + `}`)

	payload := json.RawMessage(b.String())
	require.Len(t, payload, domain.MaxPayloadBytes, "the fixture must sit exactly on the cap")

	ev, err := domain.NewAppend(domain.KindAlertUpserted, resourceID, payload)
	require.NoError(t, err)
	assert.Len(t, ev.Payload, domain.MaxPayloadBytes, "already compact: nothing to strip")
}
