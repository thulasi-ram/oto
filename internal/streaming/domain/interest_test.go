package domain_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/streaming/domain"
)

// allSelectors is the closed `resources` query vocabulary of SPEC §E.4. They are
// PLURAL and deliberately do not equal the Resource enum.
var allSelectors = []string{
	domain.SelectorAlerts,
	domain.SelectorGroups,
	domain.SelectorOccurrences,
	domain.SelectorEvents,
	domain.SelectorDeliveries,
	domain.SelectorSources,
}

func event(res domain.Resource, alert, group *uuid.UUID) domain.Event {
	return domain.Event{Seq: 1, Resource: res, AlertID: alert, GroupID: group}
}

// ------------------------------------------------------------ ParseInterest

func TestParseInterestAcceptsEverySelector(t *testing.T) {
	t.Parallel()

	wantResource := map[string]domain.Resource{
		domain.SelectorAlerts:      domain.ResourceAlert,
		domain.SelectorGroups:      domain.ResourceGroup,
		domain.SelectorOccurrences: domain.ResourceOccurrence,
		domain.SelectorEvents:      domain.ResourceAlertEvent,
		domain.SelectorDeliveries:  domain.ResourceDelivery,
		domain.SelectorSources:     domain.ResourceSource,
	}
	require.Len(t, wantResource, len(allSelectors))

	for sel, res := range wantResource {
		t.Run(sel, func(t *testing.T) {
			t.Parallel()

			in, err := domain.ParseInterest([]string{sel}, nil, nil)
			require.NoError(t, err)
			assert.False(t, in.AllResources(), "a selector was given, so this Interest filters")

			assert.True(t, in.Matches(event(res, nil, nil)), "%q must admit %q", sel, res)

			// It must admit that resource and no other.
			for _, other := range []domain.Resource{
				domain.ResourceAlert, domain.ResourceOccurrence, domain.ResourceGroup,
				domain.ResourceAlertEvent, domain.ResourceDelivery, domain.ResourceSource,
			} {
				if other == res {
					continue
				}
				assert.False(t, in.Matches(event(other, nil, nil)), "%q must not admit %q", sel, other)
			}
		})
	}
}

func TestParseInterestEmptyMeansEverything(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
	}{
		{name: "absent", in: nil},
		{name: "empty slice", in: []string{}},
		{name: "one empty value", in: []string{""}},
		{name: "whitespace only", in: []string{"   "}},
		{name: "commas only", in: []string{",,"}},
		{name: "empty values around a comma", in: []string{" , "}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in, err := domain.ParseInterest(tc.in, nil, nil)
			require.NoError(t, err)
			assert.True(t, in.AllResources(),
				"an EventSource opened with no filter must not be a silent no-op stream")

			for _, res := range []domain.Resource{
				domain.ResourceAlert, domain.ResourceOccurrence, domain.ResourceGroup,
				domain.ResourceAlertEvent, domain.ResourceDelivery, domain.ResourceSource,
			} {
				assert.True(t, in.Matches(event(res, nil, nil)), "must admit %q", res)
			}
		})
	}
}

func TestParseInterestSplitsAndTrims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
	}{
		{name: "one comma separated value", in: []string{"alerts,groups"}},
		{name: "repeated query parameters", in: []string{"alerts", "groups"}},
		{name: "mixed", in: []string{"alerts", "groups,"}},
		{name: "padded", in: []string{"  alerts , groups  "}},
		{name: "duplicates collapse", in: []string{"alerts,alerts,groups,groups"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in, err := domain.ParseInterest(tc.in, nil, nil)
			require.NoError(t, err)
			assert.False(t, in.AllResources())

			assert.True(t, in.Matches(event(domain.ResourceAlert, nil, nil)))
			assert.True(t, in.Matches(event(domain.ResourceGroup, nil, nil)))
			assert.False(t, in.Matches(event(domain.ResourceDelivery, nil, nil)))
		})
	}
}

// TestParseInterestRejectsUnknownSelectors — an unrecognised resource is refused
// with a field violation, never silently ignored. Silently dropping it would
// hand the client a stream that is quietly wider or narrower than it asked for.
func TestParseInterestRejectsUnknownSelectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []string
		badPart string
	}{
		{name: "a resource that does not exist", in: []string{"widgets"}, badPart: "widgets"},
		{name: "the singular form is not the query vocabulary", in: []string{"alert"}, badPart: "alert"},
		{name: "the Resource enum value is not a selector", in: []string{"alert_event"}, badPart: "alert_event"},
		{name: "wrong case", in: []string{"Alerts"}, badPart: "Alerts"},
		{name: "a bad value beside a good one still fails", in: []string{"alerts,widgets"}, badPart: "widgets"},
		{name: "a bad value before a good one still fails", in: []string{"widgets,alerts"}, badPart: "widgets"},
		{name: "a bad value in a second query parameter", in: []string{"alerts", "widgets"}, badPart: "widgets"},
		{name: "a wildcard is not a selector", in: []string{"*"}, badPart: "*"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in, err := domain.ParseInterest(tc.in, nil, nil)
			require.Error(t, err)
			assert.Equal(t, errs.KindValidation, errs.KindOf(err))
			assert.Equal(t, "stream_resource_invalid", errs.CodeOf(err))
			assert.Contains(t, err.Error(), tc.badPart)

			violations := errs.ViolationsOf(err)
			require.Len(t, violations, 1)
			assert.Equal(t, "resources", violations[0].Field, "violation fields are JSON names")
			assert.Equal(t, "oneof", violations[0].Code)
			assert.Contains(t, violations[0].Message, tc.badPart)

			assert.Equal(t, domain.Interest{}, in, "a rejected filter must not leak a usable Interest")
		})
	}
}

// --------------------------------------------------------------- Matches

func TestInterestMatches(t *testing.T) {
	t.Parallel()

	a := uuid.MustParse("0198c0de-0000-7000-8000-0000000000a1")
	otherAlert := uuid.MustParse("0198c0de-0000-7000-8000-0000000000a2")
	g := uuid.MustParse("0198c0de-0000-7000-8000-0000000000b1")
	otherGroup := uuid.MustParse("0198c0de-0000-7000-8000-0000000000b2")

	tests := []struct {
		name      string
		resources []string
		alertID   *uuid.UUID
		groupID   *uuid.UUID
		event     domain.Event
		want      bool
	}{
		{
			name:  "no filter at all admits everything",
			event: event(domain.ResourceDelivery, nil, nil), want: true,
		},

		// Resource narrowing.
		{
			name: "the resource is in the set", resources: []string{"alerts"},
			event: event(domain.ResourceAlert, nil, nil), want: true,
		},
		{
			name: "the resource is not in the set", resources: []string{"alerts"},
			event: event(domain.ResourceGroup, nil, nil), want: false,
		},

		// Alert narrowing.
		{
			name: "the alert id matches", alertID: &a,
			event: event(domain.ResourceAlert, &a, nil), want: true,
		},
		{
			name: "a different alert", alertID: &a,
			event: event(domain.ResourceAlert, &otherAlert, nil), want: false,
		},
		{
			name: "an event with no alert id cannot satisfy an alert filter", alertID: &a,
			event: event(domain.ResourceSource, nil, nil), want: false,
		},

		// Group narrowing.
		{
			name: "the group id matches", groupID: &g,
			event: event(domain.ResourceGroup, nil, &g), want: true,
		},
		{
			name: "a different group", groupID: &g,
			event: event(domain.ResourceGroup, nil, &otherGroup), want: false,
		},

		// The documented OR: a client watching one group wants that group's own
		// frames AND the frames of the alerts inside it.
		{
			name: "OR — the alert half is enough", alertID: &a, groupID: &g,
			event: event(domain.ResourceAlert, &a, nil), want: true,
		},
		{
			name: "OR — the group half is enough", alertID: &a, groupID: &g,
			event: event(domain.ResourceGroup, nil, &g), want: true,
		},
		{
			name: "OR — both halves", alertID: &a, groupID: &g,
			event: event(domain.ResourceAlertEvent, &a, &g), want: true,
		},
		{
			name: "OR — neither half", alertID: &a, groupID: &g,
			event: event(domain.ResourceAlertEvent, &otherAlert, &otherGroup), want: false,
		},
		{
			name: "an alert inside the watched group, carried on the payload", groupID: &g,
			event: event(domain.ResourceAlert, &a, &g), want: true,
		},

		// Resource and id together: both must admit it. The filter only narrows.
		{
			name:      "a matching id cannot rescue a rejected resource",
			resources: []string{"groups"}, alertID: &a,
			event: event(domain.ResourceAlert, &a, nil), want: false,
		},
		{
			name:      "a matching resource cannot rescue a rejected id",
			resources: []string{"alerts"}, alertID: &a,
			event: event(domain.ResourceAlert, &otherAlert, nil), want: false,
		},
		{
			name:      "both admit it",
			resources: []string{"alerts"}, alertID: &a,
			event: event(domain.ResourceAlert, &a, nil), want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in, err := domain.ParseInterest(tc.resources, tc.alertID, tc.groupID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, in.Matches(tc.event))
		})
	}
}

// TestZeroInterestMatchesEverything — the zero value is the unfiltered stream,
// which is what makes "org scoping is applied separately, server-side" safe: no
// Interest can ever be the thing that widens.
func TestZeroInterestMatchesEverything(t *testing.T) {
	t.Parallel()

	var in domain.Interest
	assert.True(t, in.AllResources())

	for _, res := range []domain.Resource{
		domain.ResourceAlert, domain.ResourceOccurrence, domain.ResourceGroup,
		domain.ResourceAlertEvent, domain.ResourceDelivery, domain.ResourceSource,
	} {
		assert.True(t, in.Matches(event(res, nil, nil)), "%q", res)
	}
}

// TestInterestCanOnlyNarrow is the property the type promises: for every legal
// filter and every event, an Interest admitting an event implies the unfiltered
// Interest admits it too. No combination of query parameters can widen a stream.
func TestInterestCanOnlyNarrow(t *testing.T) {
	t.Parallel()

	a := uuid.MustParse("0198c0de-0000-7000-8000-0000000000a1")
	g := uuid.MustParse("0198c0de-0000-7000-8000-0000000000b1")
	ids := []*uuid.UUID{nil, &a, &g}

	resourceSets := [][]string{nil, {"alerts"}, {"groups"}, {"alerts,groups"}, allSelectors}

	events := []domain.Event{}
	for _, res := range []domain.Resource{
		domain.ResourceAlert, domain.ResourceOccurrence, domain.ResourceGroup,
		domain.ResourceAlertEvent, domain.ResourceDelivery, domain.ResourceSource,
	} {
		for _, ev := range []domain.Event{
			event(res, nil, nil), event(res, &a, nil), event(res, nil, &g), event(res, &a, &g),
		} {
			events = append(events, ev)
		}
	}

	var unfiltered domain.Interest

	for _, rs := range resourceSets {
		for _, al := range ids {
			for _, gr := range ids {
				in, err := domain.ParseInterest(rs, al, gr)
				require.NoError(t, err)

				for _, ev := range events {
					if in.Matches(ev) {
						assert.True(t, unfiltered.Matches(ev),
							"interest(resources=%v, alert=%v, group=%v) admitted an event the unfiltered stream would not",
							rs, al, gr)
					}
				}
			}
		}
	}
}

// TestAllSelectorsIsTheSameWidthAsNoSelector — listing every resource explicitly
// must behave exactly like listing none, or the UI's "select all" checkbox would
// quietly differ from the default stream.
func TestAllSelectorsIsTheSameWidthAsNoSelector(t *testing.T) {
	t.Parallel()

	every, err := domain.ParseInterest(allSelectors, nil, nil)
	require.NoError(t, err)
	assert.False(t, every.AllResources(), "it does filter; it just happens to admit all six")

	var none domain.Interest

	for _, res := range []domain.Resource{
		domain.ResourceAlert, domain.ResourceOccurrence, domain.ResourceGroup,
		domain.ResourceAlertEvent, domain.ResourceDelivery, domain.ResourceSource,
	} {
		ev := event(res, nil, nil)
		assert.Equal(t, none.Matches(ev), every.Matches(ev), "%q", res)
	}
}
