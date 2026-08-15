package domain

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

func TestAllEventTypes_IsAClosedWellShapedSet(t *testing.T) {
	all := AllEventTypes()
	require.NotEmpty(t, all)

	seen := map[string]struct{}{}
	for _, ty := range all {
		s := ty.String()
		assert.Regexp(t, validate.PatternEventType, s,
			"ev_type_ck requires the <subject>.<fact> shape")
		_, dup := seen[s]
		assert.False(t, dup, "duplicate event type %q", s)
		seen[s] = struct{}{}

		// Every declared type parses.
		got, err := NewEventType(s)
		require.NoError(t, err)
		assert.Equal(t, ty, got)
		assert.False(t, ty.IsZero())
	}

	// The four states' terminal facts are distinct types: `occurrence.expired` is
	// never rendered as `occurrence.resolved`.
	assert.NotEqual(t, EventOccurrenceResolved, EventOccurrenceExpired)
	assert.Contains(t, seen, "occurrence.resolved")
	assert.Contains(t, seen, "occurrence.expired")

	// Nothing in the vocabulary names a scope-banned concept (CONTEXT.md §3).
	for s := range seen {
		// vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
		for _, banned := range []string{
			"incident", "escalat", "oncall", "on_call", "rota", "assign",
			"responder", "triage", "postmortem", "sla", "mtta", "mttr", "watcher", "subscriber", // vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
		} {
			assert.NotContains(t, s, banned, "event type %q uses banned vocabulary %q", s, banned)
		}
	}

	// `close`, `merge` and `dismiss` are banned OF AN ALERT specifically —
	// "group.closed" is a legitimate generation fact (§D.4.1).
	for s := range seen {
		if !strings.HasPrefix(s, "alert.") && !strings.HasPrefix(s, "occurrence.") {
			continue
		}
		for _, banned := range []string{"close", "merge", "dismiss", "reopen_by", "resolve_by"} {
			assert.NotContains(t, s, banned,
				"no human writes a signal's state: there is no %q", s)
		}
	}
}

// TestEventTypeRendersAsItsStringForEveryEncoder is the regression for the blank
// log line: `"type":{}` where the fact's name belongs.
//
// The three subjects are the three ways this value leaves the process — a JSON log
// record, a text log record, and any encoder handed a value carrying one — and all
// three route through `MarshalText`. `String()` covers only `fmt`, which is why
// having it was not enough.
func TestEventTypeRendersAsItsStringForEveryEncoder(t *testing.T) {
	typ := EventRuleLookupFailed

	b, err := json.Marshal(map[string]any{"type": typ})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"rule.lookup_failed"}`, string(b),
		"an EventType with no MarshalText encodes as {} — every field it has is unexported")

	var jsonOut, textOut strings.Builder
	slog.New(slog.NewJSONHandler(&jsonOut, nil)).Warn("could not record", "type", typ)
	slog.New(slog.NewTextHandler(&textOut, nil)).Warn("could not record", "type", typ)
	assert.Contains(t, jsonOut.String(), `"type":"rule.lookup_failed"`)
	assert.Contains(t, textOut.String(), "type=rule.lookup_failed")

	// Every member, not just the one that regressed.
	for _, ty := range AllEventTypes() {
		text, marshalErr := ty.MarshalText()
		require.NoError(t, marshalErr)
		assert.Equal(t, ty.String(), string(text))
	}
}

func TestNewEventType_RejectsInventedTypes(t *testing.T) {
	for _, in := range []string{
		"",
		"alert.invented",
		"occurrence.closed",
		"incident.created",
		"Alert.Created",
		"alert_created",
		"alert.created.extra",
	} {
		t.Run(in, func(t *testing.T) {
			got, err := NewEventType(in)
			requireKind(t, err, errs.KindValidation, "enum")
			assert.Contains(t, err.Error(), "SPEC amendment")
			assert.True(t, got.IsZero())
		})
	}
}

func validEventParams() EventParams {
	return EventParams{
		ID:           eventIDFix,
		OrgID:        orgA,
		AlertID:      alertID,
		OccurrenceID: occID,
		GroupID:      groupIDFix,
		Type:         EventOccurrenceOpened,
		At: ObservationTime{
			occurredAt: t0,
			recordedAt: t0.Add(time.Second),
		},
		Actor:     Actor{kind: ActorIngest},
		Summary:   "Occurrence opened",
		Payload:   map[string]any{"k": "v"},
		DedupeKey: "occ:x:opened",
	}
}

func TestNewEvent_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*EventParams)
		kind errs.Kind
		code string
	}{
		{name: "no id", mut: func(p *EventParams) { p.ID = uuid.Nil }, kind: errs.KindValidation, code: "required"},
		{name: "no org", mut: func(p *EventParams) { p.OrgID = uuid.Nil }, kind: errs.KindValidation, code: "required"},
		{name: "no type", mut: func(p *EventParams) { p.Type = EventType{} }, kind: errs.KindValidation, code: "required"},
		{
			name: "no subject at all",
			mut: func(p *EventParams) {
				p.AlertID, p.OccurrenceID, p.GroupID = uuid.Nil, uuid.Nil, uuid.Nil
			},
			kind: errs.KindValidation, code: "required",
		},
		{name: "no observation time", mut: func(p *EventParams) { p.At = ObservationTime{} }, kind: errs.KindValidation, code: "required"},
		{name: "no actor", mut: func(p *EventParams) { p.Actor = Actor{} }, kind: errs.KindValidation, code: "required"},
		{name: "blank summary", mut: func(p *EventParams) { p.Summary = "  \t\n" }, kind: errs.KindValidation, code: "not_blank"},
		{name: "empty summary", mut: func(p *EventParams) { p.Summary = "" }, kind: errs.KindValidation, code: "not_blank"},
		{
			name: "summary over the bound",
			mut:  func(p *EventParams) { p.Summary = strings.Repeat("s", MaxEventSummaryBytes+1) },
			kind: errs.KindValidation, code: "max_length",
		},
		{
			name: "dedupe key over the bound",
			mut:  func(p *EventParams) { p.DedupeKey = strings.Repeat("d", MaxDedupeKeyBytes+1) },
			kind: errs.KindValidation, code: "max_length",
		},
		{
			name: "a type outside the ev_type_ck shape",
			mut:  func(p *EventParams) { p.Type = EventType{s: "NotAShape"} },
			kind: errs.KindInternal, code: "event_type_shape",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validEventParams()
			tc.mut(&p)
			_, err := NewEvent(p)
			requireKind(t, err, tc.kind, tc.code)
		})
	}
}

func TestNewEvent_AnyOneSubjectIsEnough(t *testing.T) {
	for _, name := range []string{"alert", "occurrence", "group"} {
		t.Run("only "+name, func(t *testing.T) {
			p := validEventParams()
			p.AlertID, p.OccurrenceID, p.GroupID = uuid.Nil, uuid.Nil, uuid.Nil
			switch name {
			case "alert":
				p.AlertID = alertID
			case "occurrence":
				p.OccurrenceID = occID
			case "group":
				p.GroupID = groupIDFix
			}
			_, err := NewEvent(p)
			require.NoError(t, err)
		})
	}
}

func TestNewEvent_HappyPath(t *testing.T) {
	p := validEventParams()
	p.Summary = "  Occurrence opened  "

	ev, err := NewEvent(p)
	require.NoError(t, err)

	assert.Equal(t, eventIDFix, ev.ID())
	assert.Equal(t, orgA, ev.OrgID())
	assert.Equal(t, alertID, ev.AlertID())
	assert.Equal(t, occID, ev.OccurrenceID())
	assert.Equal(t, groupIDFix, ev.GroupID())
	assert.Equal(t, EventOccurrenceOpened, ev.Type())
	assert.Equal(t, "Occurrence opened", ev.Summary(), "the summary is trimmed")
	assert.Equal(t, "occ:x:opened", ev.DedupeKey())

	// ⭐ C12: two clocks, never conflated. Display OccurredAt; order by RecordedAt.
	assert.Equal(t, t0, ev.OccurredAt())
	assert.Equal(t, t0.Add(time.Second), ev.RecordedAt())
	assert.Equal(t, ev.OccurredAt(), ev.At().OccurredAt())
	assert.Equal(t, ev.RecordedAt(), ev.At().RecordedAt())
	assert.NotEqual(t, ev.OccurredAt(), ev.RecordedAt())
}

func TestNewEvent_EmptyDedupeKeyMeansAlwaysAppend(t *testing.T) {
	p := validEventParams()
	p.DedupeKey = ""
	ev, err := NewEvent(p)
	require.NoError(t, err)
	assert.Empty(t, ev.DedupeKey())
}

func TestEvent_PayloadIsCopiedInAndOut(t *testing.T) {
	// An AlertEvent is IMMUTABLE: it is never updated and never deleted.
	in := map[string]any{"k": "v"}
	p := validEventParams()
	p.Payload = in

	ev, err := NewEvent(p)
	require.NoError(t, err)

	in["k"] = "tampered"
	in["injected"] = true
	assert.Equal(t, "v", ev.Payload()["k"])
	assert.NotContains(t, ev.Payload(), "injected")

	out := ev.Payload()
	out["k"] = "tampered"
	assert.Equal(t, "v", ev.Payload()["k"])
}

func TestEvent_NilPayload(t *testing.T) {
	p := validEventParams()
	p.Payload = nil
	ev, err := NewEvent(p)
	require.NoError(t, err)
	assert.Nil(t, ev.Payload())
}

func TestEvent_ActorLabelIsDenormalisedOntoTheEvent(t *testing.T) {
	// The label is carried on the event so that a renamed or deleted user never
	// rewrites history.
	p := validEventParams()
	p.Actor = humanActor(t, "018f3a4b-0000-7000-8000-0000000004ff", "Ram T")
	ev, err := NewEvent(p)
	require.NoError(t, err)

	assert.Equal(t, ActorUser, ev.Actor().Kind())
	assert.Equal(t, "Ram T", ev.Actor().Label())
	assert.True(t, ev.Actor().Kind().IsHuman())
}
