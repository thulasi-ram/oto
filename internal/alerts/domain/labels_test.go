package domain

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// mustLabels builds a Labels or fails the test.
func mustLabels(t *testing.T, in map[string]string) Labels {
	t.Helper()
	l, err := NewLabels(in)
	require.NoError(t, err)
	return l
}

// mustLabelSet builds a LabelSet or fails the test.
func mustLabelSet(t *testing.T, in map[string]string) LabelSet {
	t.Helper()
	s, err := NewLabelSet(in)
	require.NoError(t, err)
	return s
}

func TestNewLabels_Bounds(t *testing.T) {
	tooMany := map[string]string{}
	for i := range MaxLabels + 1 {
		tooMany["l"+strings.Repeat("x", i)] = "v"
	}

	// Five values of 4000 bytes each serialise to 5*(1+4000+2) = 20015 > B6.
	tooBig := map[string]string{}
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		_ = i
		tooBig[name] = strings.Repeat("v", 4000)
	}

	tests := []struct {
		name     string
		in       map[string]string
		wantCode string
	}{
		{name: "empty map is legal", in: map[string]string{}},
		{name: "nil map is legal", in: nil},
		{name: "happy path", in: map[string]string{"alertname": "X", "severity": "critical"}},
		{name: "empty value is legal", in: map[string]string{"a": ""}},
		{name: "leading underscore name is legal", in: map[string]string{"_a0": "v"}},
		{name: "unicode value is legal", in: map[string]string{"a": "日本語 ☃ – ok"}},

		{name: "too many labels", in: tooMany, wantCode: "too_many_labels"},
		{
			name:     "label name too large",
			in:       map[string]string{strings.Repeat("a", MaxLabelNameBytes+1): "v"},
			wantCode: "label_name_too_large",
		},
		{
			name:     "label name charset: leading digit",
			in:       map[string]string{"0bad": "v"},
			wantCode: "invalid_label_name",
		},
		{
			name:     "label name charset: dash",
			in:       map[string]string{"bad-name": "v"},
			wantCode: "invalid_label_name",
		},
		{
			name:     "label name charset: unicode",
			in:       map[string]string{"日本語": "v"},
			wantCode: "invalid_label_name",
		},
		{
			name:     "label name charset: empty",
			in:       map[string]string{"": "v"},
			wantCode: "invalid_label_name",
		},
		{
			name:     "label value too large",
			in:       map[string]string{"a": strings.Repeat("v", MaxLabelValueBytes+1)},
			wantCode: "label_value_too_large",
		},
		{name: "serialised set too large", in: tooBig, wantCode: "labelset_too_large"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := NewLabels(tc.in)
			if tc.wantCode == "" {
				require.NoError(t, err)
				assert.Equal(t, len(tc.in), l.Len())
				return
			}
			require.Error(t, err)
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, tc.wantCode, e.Code,
				"the error code is what ingest_rejections.reason records (§L.3.2)")
		})
	}
}

func TestLabels_IsImmutableOnceConstructed(t *testing.T) {
	in := map[string]string{"alertname": "X", "severity": "critical"}
	l := mustLabels(t, in)

	// Mutating the caller's map must not reach into the value object.
	in["severity"] = "warning"
	in["injected"] = "yes"

	sev, ok := l.Get("severity")
	require.True(t, ok)
	assert.Equal(t, "critical", sev)
	_, injected := l.Get("injected")
	assert.False(t, injected)

	// Map() hands out a copy.
	out := l.Map()
	out["severity"] = "info"
	sev, _ = l.Get("severity")
	assert.Equal(t, "critical", sev)
}

// TestCanonical_IsStableAcrossInsertionOrder is the load-bearing property: alert
// identity is derived from the canonical serialisation, so the same labels
// presented in any order must produce the same bytes and the same AlertKey.
func TestCanonical_IsStableAcrossInsertionOrder(t *testing.T) {
	names := []string{"alertname", "severity", "namespace", "service", "pod", "instance", "job"}
	values := map[string]string{
		"alertname": "KubePodCrashLooping",
		"severity":  "critical",
		"namespace": "prod",
		"service":   "checkout",
		"pod":       "checkout-7d9f",
		"instance":  "10.0.0.1:9100",
		"job":       "kubelet",
	}

	org := uuid.MustParse("018f3a4b-0000-7000-8000-000000000001")
	cluster, err := NewClusterKey("prod-eu")
	require.NoError(t, err)

	canon := map[string]struct{}{}
	keys := map[string]struct{}{}
	rng := rand.New(rand.NewSource(1))

	for range 200 {
		order := append([]string(nil), names...)
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

		in := make(map[string]string, len(order))
		for _, n := range order {
			in[n] = values[n]
		}
		ls := mustLabelSet(t, in)

		canon[string(ls.Canonical(nil))] = struct{}{}
		keys[ComputeAlertKey(org, cluster, ls, nil).String()] = struct{}{}
	}

	assert.Len(t, canon, 1, "canonical serialisation must not depend on map iteration order")
	assert.Len(t, keys, 1, "AlertKey must not depend on map iteration order")
}

func TestCanonical_ExactBytes(t *testing.T) {
	// SPEC §C.1: name || 0x01 || value || 0x02, sorted by name ASC in byte order.
	// This is frozen: changing it re-keys every Alert in every installation.
	ls := mustLabelSet(t, map[string]string{"alertname": "X", "b": "1", "c": "2"})
	assert.Equal(t,
		"alertname\x01X\x02b\x011\x02c\x012\x02",
		string(ls.Canonical(nil)))
}

func TestCanonical_SortIsByteOrderNotLocale(t *testing.T) {
	// Uppercase sorts before lowercase in byte order; a locale-aware collation
	// would interleave them.
	l := mustLabels(t, map[string]string{"Z": "1", "a": "2", "A": "3"})
	assert.Equal(t, []string{"A", "Z", "a"}, l.Names())
	assert.Equal(t, "A\x013\x02Z\x011\x02a\x012\x02", string(l.canonical()))
}

func TestCanonical_EmptyValueIsNotTheSameAsAbsentLabel(t *testing.T) {
	withEmpty := mustLabelSet(t, map[string]string{"alertname": "X", "team": ""})
	without := mustLabelSet(t, map[string]string{"alertname": "X"})

	assert.NotEqual(t, string(withEmpty.Canonical(nil)), string(without.Canonical(nil)))

	org := uuid.New()
	ck, err := NewClusterKey("c1")
	require.NoError(t, err)
	assert.NotEqual(t,
		ComputeAlertKey(org, ck, withEmpty, nil).String(),
		ComputeAlertKey(org, ck, without, nil).String(),
		`a label present-but-empty is a different Alert from one that is absent`)
}

func TestCanonical_UnicodeIsVerbatimNoCaseFolding(t *testing.T) {
	upper := mustLabelSet(t, map[string]string{"alertname": "X", "a": "ÉCHEC"})
	lower := mustLabelSet(t, map[string]string{"alertname": "X", "a": "échec"})
	assert.NotEqual(t, string(upper.Canonical(nil)), string(lower.Canonical(nil)),
		"the doc comment says: used verbatim, UTF-8, no case folding")

	// The bytes are the UTF-8 bytes, untouched.
	ls := mustLabelSet(t, map[string]string{"alertname": "日本語 ☃"})
	assert.Equal(t, "alertname\x01日本語 ☃\x02", string(ls.Canonical(nil)))
}

// ⛔ BUG. The canonical serialisation is NOT injective: label VALUES are written
// verbatim and the two structural separators (0x01, 0x02) are neither escaped nor
// forbidden in a value. NewLabels constrains label NAMES to
// `^[a-zA-Z_][a-zA-Z0-9_]*$` but places no charset constraint on values at all,
// so a value carrying the separators reproduces the framing of two labels.
//
// Two DIFFERENT label sets therefore serialise to the SAME byte string and get
// the SAME AlertKey — i.e. they become the same Alert, share one row, one
// timeline and one Slack thread. That contradicts:
//   - AlertKey's doc comment ("the identity of an Alert: the PRIMARY dedup key"),
//   - writeField's stated design intent ("so that two different field splits can
//     never produce the same byte string"),
//   - CONTEXT.md §3 ("Alert | The identity of a label set").
//
// Prometheus label values are arbitrary UTF-8 and may contain control bytes
// (`label_replace` over log- or exporter-derived text is the realistic route in).
// The same defect applies to GroupKey (groupLabels) and RuleFingerprint (rule
// labels and annotations), which hash the same serialisation.
func TestCanonical_SeparatorInValueMustNotCollide_BUG(t *testing.T) {
	t.Skip("BUG: canonical serialisation does not escape 0x01/0x02 in label values, so two distinct label sets collide onto one AlertKey (labels.go:175-193, NewLabels does not constrain the value charset)")

	twoLabels := mustLabelSet(t, map[string]string{
		"alertname": "X",
		"b":         "1",
		"c":         "2",
	})
	oneLabel := mustLabelSet(t, map[string]string{
		"alertname": "X",
		"b":         "1\x02c\x012",
	})

	require.Equal(t, 3, twoLabels.Len())
	require.Equal(t, 2, oneLabel.Len())

	assert.NotEqual(t,
		string(twoLabels.Canonical(nil)),
		string(oneLabel.Canonical(nil)),
		"distinct label sets must not share a canonical serialisation")

	org := uuid.New()
	ck, err := NewClusterKey("prod-eu")
	require.NoError(t, err)
	assert.NotEqual(t,
		ComputeAlertKey(org, ck, twoLabels, nil).String(),
		ComputeAlertKey(org, ck, oneLabel, nil).String(),
		"distinct label sets must be distinct Alerts")
}

func TestLabels_Without(t *testing.T) {
	l := mustLabels(t, map[string]string{"a": "1", "b": "2", "c": "3"})

	assert.Equal(t, 3, l.Without(nil).Len(), "removing nothing is the identity")
	assert.Equal(t, 3, l.Without([]string{}).Len())
	assert.Equal(t, 2, l.Without([]string{"b"}).Len())
	assert.Equal(t, 3, l.Without([]string{"zzz"}).Len(), "removing an absent label is a no-op")
	assert.Equal(t, 1, l.Without([]string{"a", "b"}).Len())
	assert.Equal(t, 3, l.Len(), "Without does not mutate the receiver")
}

func TestLabelSet_WithoutNeverDropsAlertname(t *testing.T) {
	ls := mustLabelSet(t, map[string]string{"alertname": "X", "pod": "p1"})

	stripped := ls.Without([]string{"alertname", "pod"})
	assert.Equal(t, "X", stripped.AlertName(),
		"alertname is identity-bearing; a LabelSet without one cannot exist")
	assert.Equal(t, 1, stripped.Len())

	// Canonical honours the same rule.
	assert.Equal(t, "alertname\x01X\x02", string(ls.Canonical([]string{"alertname", "pod"})))
}

func TestLabelSet_CanonicalIgnore(t *testing.T) {
	ls := mustLabelSet(t, map[string]string{"alertname": "X", "pod": "p1", "replica": "r2"})

	assert.Equal(t, "alertname\x01X\x02pod\x01p1\x02replica\x01r2\x02",
		string(ls.Canonical(nil)))
	assert.Equal(t, "alertname\x01X\x02replica\x01r2\x02",
		string(ls.Canonical([]string{"pod"})))
	assert.Equal(t, "alertname\x01X\x02",
		string(ls.Canonical([]string{"pod", "replica"})))
	assert.Equal(t, "alertname\x01X\x02pod\x01p1\x02replica\x01r2\x02",
		string(ls.Canonical([]string{"absent"})),
		"ignoring a label the set does not carry changes nothing")
}

func TestNewLabelSet_RequiresNonEmptyAlertname(t *testing.T) {
	tests := []struct {
		name     string
		in       map[string]string
		wantCode string
	}{
		{name: "present", in: map[string]string{"alertname": "X"}},
		{name: "absent", in: map[string]string{"severity": "critical"}, wantCode: "missing_alertname"},
		{name: "empty", in: map[string]string{"alertname": ""}, wantCode: "missing_alertname"},
		{name: "blank", in: map[string]string{"alertname": "   \t\n"}, wantCode: "missing_alertname"},
		{
			name:     "too long",
			in:       map[string]string{"alertname": strings.Repeat("A", MaxAlertNameBytes+1)},
			wantCode: "label_value_too_large",
		},
		{
			name: "at the bound",
			in:   map[string]string{"alertname": strings.Repeat("A", MaxAlertNameBytes)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ls, err := NewLabelSet(tc.in)
			if tc.wantCode == "" {
				require.NoError(t, err)
				assert.NotEmpty(t, ls.AlertName())
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, tc.wantCode, e.Code)
			assert.True(t, ls.IsZero())
		})
	}
}

func TestLabelSet_PromotedLabels(t *testing.T) {
	ls := mustLabelSet(t, map[string]string{
		"alertname": "X",
		"severity":  "p1",
		"namespace": "prod",
		"service":   "checkout",
	})
	assert.Equal(t, "X", ls.AlertName())
	assert.Equal(t, SeverityCritical, ls.Severity(), "p1 is an alias of critical (§L.4.2)")
	assert.Equal(t, "prod", ls.Namespace())
	assert.Equal(t, "checkout", ls.Service())

	bare := mustLabelSet(t, map[string]string{"alertname": "X"})
	assert.Equal(t, SeverityUnknown, bare.Severity(), "an absent severity is Unknown, never an error")
	assert.Equal(t, "", bare.Namespace())
	assert.Equal(t, "", bare.Service())
}

func TestLabels_SerialisedSizeMatchesCanonical(t *testing.T) {
	for _, in := range []map[string]string{
		{},
		{"a": ""},
		{"alertname": "X", "b": "日本語"},
		{"alertname": "X", "b": strings.Repeat("v", 1000)},
	} {
		l := mustLabels(t, in)
		assert.Equal(t, len(l.canonical()), l.SerialisedSize(),
			"B6 caps exactly the quantity Canonical produces")
	}
}

// TestLabels_FingerprintMatchesPrometheus pins §C.3 against the real thing.
//
// The expected values were produced by github.com/prometheus/common v0.70.1:
//
//	model.LabelSet{...}.Fingerprint().String()
//
// If oto's recomputation drifts from Alertmanager's, every /api/v2/alerts
// reconciliation join silently stops matching.
func TestLabels_FingerprintMatchesPrometheus(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want string
	}{
		{name: "empty", in: map[string]string{}, want: "cbf29ce484222325"},
		{name: "one label", in: map[string]string{"alertname": "X"}, want: "d2aeefc389ca7746"},
		{
			name: "three labels",
			in: map[string]string{
				"alertname": "KubePodCrashLooping",
				"namespace": "prod",
				"severity":  "critical",
			},
			want: "738bec28daad1979",
		},
		{
			name: "empty value",
			in:   map[string]string{"alertname": "X", "a": ""},
			want: "f71b8243e5a695d1",
		},
		{
			name: "unicode value",
			in:   map[string]string{"alertname": "X", "a": "日本語 ☃"},
			want: "e668172423059f4e",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mustLabels(t, tc.in).Fingerprint()
			assert.Equal(t, tc.want, got.String())
			assert.Len(t, got.String(), fingerprintHexLen, `rendered "%016x"`)

			parsed, err := NewSourceFingerprint(got.String())
			require.NoError(t, err, "a computed fingerprint must satisfy alerts_srcfp_ck")
			assert.Equal(t, got, parsed)
		})
	}
}

func TestLabels_FingerprintIsOverTheFullSetAndOrderIndependent(t *testing.T) {
	a := mustLabelSet(t, map[string]string{"alertname": "X", "pod": "p1"})
	b := mustLabelSet(t, map[string]string{"pod": "p1", "alertname": "X"})
	assert.Equal(t, a.Fingerprint(), b.Fingerprint())
	assert.Equal(t, a.Fingerprint(), ComputeSourceFingerprint(a))

	// Nothing is ignored: the fingerprint is the join key, never the identity.
	c := mustLabelSet(t, map[string]string{"alertname": "X"})
	assert.NotEqual(t, a.Fingerprint(), c.Fingerprint())
}

func TestNewAnnotations_Bounds(t *testing.T) {
	tooMany := map[string]string{}
	for i := range MaxAnnotations + 1 {
		tooMany["a"+strings.Repeat("x", i)] = "v"
	}

	tests := []struct {
		name     string
		in       map[string]string
		wantCode string
	}{
		{name: "empty", in: map[string]string{}},
		{
			name: "the name charset is deliberately unconstrained",
			in:   map[string]string{"grafana.com/dashboardUId": "abc", "日本語": "v", "": "v"},
		},
		{name: "too many", in: tooMany, wantCode: "too_many_annotations"},
		{
			name:     "name too large",
			in:       map[string]string{strings.Repeat("n", MaxLabelNameBytes+1): "v"},
			wantCode: "annotation_name_too_large",
		},
		{
			name:     "value too large",
			in:       map[string]string{"summary": strings.Repeat("v", MaxAnnotationValueBytes+1)},
			wantCode: "annotation_too_large",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewAnnotations(tc.in)
			if tc.wantCode == "" {
				require.NoError(t, err)
				assert.Equal(t, len(tc.in), a.Len())
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, tc.wantCode, e.Code)
		})
	}
}

func TestAnnotations_EqualAndCanonical(t *testing.T) {
	a, err := NewAnnotations(map[string]string{"summary": "s", "description": "d"})
	require.NoError(t, err)
	same, err := NewAnnotations(map[string]string{"description": "d", "summary": "s"})
	require.NoError(t, err)
	different, err := NewAnnotations(map[string]string{"summary": "s", "description": "d2"})
	require.NoError(t, err)
	empty, err := NewAnnotations(nil)
	require.NoError(t, err)

	assert.True(t, a.Equal(same))
	assert.False(t, a.Equal(different))
	assert.False(t, a.Equal(empty))
	assert.True(t, empty.Equal(Annotations{}))

	assert.Equal(t, "description\x01d\x02summary\x01s\x02", string(a.Canonical()))
	assert.Equal(t, string(a.Canonical()), string(same.Canonical()),
		"annotation order must not move rule_fingerprint (§C.6)")
	assert.Empty(t, empty.Canonical())
}

func TestAnnotations_MapIsACopy(t *testing.T) {
	in := map[string]string{"summary": "s"}
	a, err := NewAnnotations(in)
	require.NoError(t, err)

	in["summary"] = "tampered"
	got, _ := a.Get("summary")
	assert.Equal(t, "s", got)

	out := a.Map()
	out["summary"] = "tampered"
	got, _ = a.Get("summary")
	assert.Equal(t, "s", got)
}

func TestAnnotationsAreNotPartOfIdentity(t *testing.T) {
	// Two Alerts differing only in annotations are the SAME Alert: an operator
	// rewording a description must not create a new identity.
	ls := mustLabelSet(t, map[string]string{"alertname": "X"})
	org := uuid.New()
	ck, err := NewClusterKey("c1")
	require.NoError(t, err)

	// Annotations never enter ComputeAlertKey at all — there is no parameter for
	// them, and this test exists to keep it that way.
	assert.Equal(t,
		ComputeAlertKey(org, ck, ls, nil),
		ComputeAlertKey(org, ck, ls, nil))
}
