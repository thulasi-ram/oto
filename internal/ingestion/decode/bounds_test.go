package decode

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/domain"
)

// fixedNow is the injected clock for the B12/B13 windows. A real time.Now() here
// would make the timestamp bounds depend on when the suite runs.
var fixedNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// wireAlert is a minimal alert that passes every bound, so a test can perturb
// exactly one thing and attribute the outcome to it.
func wireAlert(labels, annotations map[string]string) Alert {
	if labels == nil {
		labels = map[string]string{"alertname": "X"}
	}
	return Alert{
		Status:      StatusFiring,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    fixedNow.Add(-time.Minute),
	}
}

func normalise(t *testing.T, a Alert) (Normalised, error) {
	t.Helper()
	return Normalise(a, AlertOptions{Now: fixedNow})
}

// reasons projects the notes onto the reasons they will be recorded under.
func reasons(notes []Note) []domain.Reason {
	out := make([]domain.Reason, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.Reason)
	}
	return out
}

// TestNormalise_UnstorableLabelValueRejectsTheAlert is B18 seen from layer 2, and
// it is the whole point of the change that added `invalid_label_value`.
//
// ⭐ THE ASSERTION THAT MATTERS IS THE REASON, NOT THE REJECTION. Rejecting was
// always right — Postgres `text` cannot hold these bytes, so the alert would have
// died at the INSERT — but the reason recorded was `undecodable`, which claims
// the BODY was not a webhook payload. `ingest_rejections` is the only place a
// rejected alert survives (§C.9.1), and an operator reads that one column.
func TestNormalise_UnstorableLabelValueRejectsTheAlert(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "a NUL", value: "pod-\x00-1"},
		{name: "a NUL alone", value: "\x00"},
		{name: "a lone 0xff", value: "\xff"},
		{name: "a truncated multi-byte rune", value: "ok-\xe6\x97"},
		{name: "a bare continuation byte", value: "\x80"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalise(t, wireAlert(
				map[string]string{"alertname": "X", "pod": tc.value}, nil))

			require.Error(t, err, "an unstorable label value must drop THAT ALERT")
			assert.Equal(t, domain.ReasonInvalidLabelValue, domain.ReasonFromError(err),
				"the recorded reason must name what was wrong, not degrade to undecodable")
			assert.NotEqual(t, domain.ReasonUndecodable, domain.ReasonFromError(err))
		})
	}
}

// TestNormalise_AStorableLabelValueIsUntouched is the other half of B18: oto
// refuses bytes Postgres cannot hold, and nothing else. Control bytes, the old
// canonical separators and a genuine U+FFFD all survive verbatim, because a label
// value is identity and oto does not edit identity.
func TestNormalise_AStorableLabelValueIsUntouched(t *testing.T) {
	value := "\x01\x02 日本語 \uFFFD \u00ff"

	n, err := normalise(t, wireAlert(map[string]string{"alertname": "X", "pod": value}, nil))
	require.NoError(t, err)

	got, ok := n.Labels.Get("pod")
	require.True(t, ok)
	assert.Equal(t, value, got, "a storable label value is stored verbatim")
}

// TestNormalise_UnstorableAnnotationValueIsSanitisedAndTheAlertKept is B19, and
// it is deliberately the OPPOSITE verdict to B18 above on the same bytes.
//
// An annotation is prose, never identity (§C.9.3), and the ingest policy for
// annotations is already truncate-and-keep (B7, B8). Dropping an alert because
// its `description` carried one bad byte would contradict that and lose the
// signal underneath the prose.
func TestNormalise_UnstorableAnnotationValueIsSanitisedAndTheAlertKept(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "a NUL", in: "disk at 91%\x00", want: "disk at 91%\uFFFD"},
		{name: "a lone 0xff", in: "a\xffb", want: "a\uFFFDb"},
		{name: "a truncated rune", in: "x\xe6\x97y", want: "x\uFFFD\uFFFDy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := normalise(t, wireAlert(nil, map[string]string{"summary": tc.in}))
			require.NoError(t, err, "an unstorable annotation must never cost the alert")

			assert.Equal(t, tc.want, n.Annotations["summary"],
				"the unstorable code points are replaced with U+FFFD")
			assert.Empty(t, alerts.UnstorableReason(n.Annotations["summary"]),
				"what leaves layer 2 must be storable at layer 6")
			assert.Equal(t, []domain.Reason{domain.ReasonAnnotationUnstorable}, reasons(n.Notes),
				"oto edited what the upstream sent, so it says so")
		})
	}
}

// TestNormalise_UnstorableAnnotationNameDropsThatAnnotationOnly.
//
// A name is a jsonb KEY, so it is dropped rather than sanitised: two different
// unstorable names sanitise to ONE string, and the second would silently
// overwrite the first — trading a recorded drop for an invisible one.
func TestNormalise_UnstorableAnnotationNameDropsThatAnnotationOnly(t *testing.T) {
	n, err := normalise(t, wireAlert(nil, map[string]string{
		"summary":      "kept",
		"desc\x00ript": "dropped",
		"desc\xffript": "also dropped",
	}))
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"summary": "kept"}, n.Annotations)
	assert.Equal(t, []domain.Reason{
		domain.ReasonAnnotationUnstorable,
		domain.ReasonAnnotationUnstorable,
	}, reasons(n.Notes), "one recorded note per dropped annotation")
}

// TestBoundAnnotations_NamesAreFilteredBeforeTheCountBound pins the ordering
// inside boundAnnotations: B19's name filter runs BEFORE B7.
//
// The other order is quietly wrong. An annotation that is about to be dropped for
// an unstorable name would first consume one of the 32 slots and evict a perfectly
// good annotation — losing prose to a bound that was never about prose.
func TestBoundAnnotations_NamesAreFilteredBeforeTheCountBound(t *testing.T) {
	in := map[string]string{"bad\x00name": "dropped"}
	for i := range alerts.MaxAnnotations {
		in["a"+strings.Repeat("x", i)] = "v"
	}
	require.Len(t, in, alerts.MaxAnnotations+1)

	var notes []Note
	out := boundAnnotations(in, &notes)

	assert.Len(t, out, alerts.MaxAnnotations, "every storable annotation survives")
	assert.NotContains(t, reasons(notes), domain.ReasonTooManyAnnotations,
		"the unstorable name must not have consumed a B7 slot")
	assert.Equal(t, []domain.Reason{domain.ReasonAnnotationUnstorable}, reasons(notes))
}

// TestBoundAnnotations_SanitisationRunsBeforeTruncation pins the other ordering,
// and this one is a correctness bound rather than a fairness one.
//
// U+FFFD is THREE bytes where the byte it replaces is one, so sanitising can grow
// a value by 3x. Truncating first and sanitising second would hand layer 6 a value
// longer than the B8 cap layer 2 claims to enforce.
func TestBoundAnnotations_SanitisationRunsBeforeTruncation(t *testing.T) {
	// Every byte is unstorable and the value already sits exactly on the cap, so
	// sanitising alone triples it to 49152 bytes.
	in := map[string]string{"summary": strings.Repeat("\xff", alerts.MaxAnnotationValueBytes)}

	var notes []Note
	out := boundAnnotations(in, &notes)

	got := out["summary"]
	assert.LessOrEqual(t, len(got), alerts.MaxAnnotationValueBytes,
		"what leaves layer 2 must satisfy B8, sanitisation included")
	assert.Empty(t, alerts.UnstorableReason(got))
	assert.True(t, strings.HasSuffix(got, TruncationMarker))
	assert.ElementsMatch(t,
		[]domain.Reason{domain.ReasonAnnotationUnstorable, domain.ReasonAnnotationTooLarge},
		reasons(notes),
		"both bounds fired and both are recorded")
}

// TestBoundAnnotations_StorableAnnotationsAreNeverRewritten. Sanitisation is the
// ONLY place oto edits what an upstream sent, so it must not fire on a value that
// merely looks unusual: control bytes and a genuine U+FFFD are storable.
func TestBoundAnnotations_StorableAnnotationsAreNeverRewritten(t *testing.T) {
	in := map[string]string{
		"summary":     "\x01\x02 control bytes are storable",
		"description": "a real \uFFFD is not damage",
		"日本語":         "unicode names are fine",
	}

	var notes []Note
	out := boundAnnotations(in, &notes)

	assert.Equal(t, in, out)
	assert.Empty(t, notes, "nothing was edited, so nothing is recorded")
}
