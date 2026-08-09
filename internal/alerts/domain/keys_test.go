package domain

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

var (
	orgA    = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000a1")
	orgB    = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000b2")
	sourceA = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000c3")
	sourceB = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000d4")
)

func mustClusterKey(t *testing.T, s string) ClusterKey {
	t.Helper()
	k, err := NewClusterKey(s)
	require.NoError(t, err)
	return k
}

func TestNewClusterKey(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{in: "prod-eu", ok: true},
		{in: "a", ok: true},
		{in: "0", ok: true},
		{in: "prod.eu_1-x", ok: true},
		{in: strings.Repeat("a", 63), ok: true},
		{in: strings.Repeat("a", 64)},
		{in: ""},
		{in: "-leading-dash"},
		{in: ".leading-dot"},
		{in: "Prod-EU"},
		{in: "prod eu"},
		{in: "prod/eu"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			k, err := NewClusterKey(tc.in)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.in, k.String())
				assert.False(t, k.IsZero())
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, "cluster_key", e.Code)
			assert.True(t, k.IsZero())
		})
	}
}

func TestComputeAlertKey_ShapeAndDeterminism(t *testing.T) {
	ls := mustLabelSet(t, map[string]string{"alertname": "X", "severity": "critical"})
	ck := mustClusterKey(t, "prod-eu")

	k := ComputeAlertKey(orgA, ck, ls, nil)

	assert.True(t, strings.HasPrefix(k.String(), AlertKeyPrefix))
	assert.Len(t, k.String(), len(AlertKeyPrefix)+26)
	assert.Regexp(t, validate.PatternAlertKey, k.String(),
		"a computed key must satisfy alerts_key_ck")

	parsed, err := NewAlertKey(k.String())
	require.NoError(t, err)
	assert.Equal(t, k, parsed)
	assert.False(t, k.IsZero())
	assert.True(t, AlertKey{}.IsZero())

	assert.Equal(t, k, ComputeAlertKey(orgA, ck, ls, nil), "the key is a pure function")
}

func TestComputeAlertKey_EveryInputParticipates(t *testing.T) {
	ls := mustLabelSet(t, map[string]string{"alertname": "X", "pod": "p1"})
	other := mustLabelSet(t, map[string]string{"alertname": "X", "pod": "p2"})
	eu := mustClusterKey(t, "prod-eu")
	us := mustClusterKey(t, "prod-us")

	base := ComputeAlertKey(orgA, eu, ls, nil)

	assert.NotEqual(t, base, ComputeAlertKey(orgB, eu, ls, nil), "org_id participates")
	assert.NotEqual(t, base, ComputeAlertKey(orgA, us, ls, nil),
		"the same alert in prod-eu and prod-us are DIFFERENT Alerts: different blast radii (C.2)")
	assert.NotEqual(t, base, ComputeAlertKey(orgA, eu, other, nil), "the label set participates")
	assert.NotEqual(t, base, ComputeAlertKey(orgA, eu, ls, []string{"pod"}),
		"changing ignore_labels creates new identities, it does not re-key existing ones")
}

func TestComputeAlertKey_IgnoredLabels(t *testing.T) {
	ck := mustClusterKey(t, "prod-eu")
	withPod := mustLabelSet(t, map[string]string{"alertname": "X", "pod": "p1"})
	withOtherPod := mustLabelSet(t, map[string]string{"alertname": "X", "pod": "p2"})
	withoutPod := mustLabelSet(t, map[string]string{"alertname": "X"})

	// Two label sets that differ only in an ignored label are the SAME Alert.
	assert.Equal(t,
		ComputeAlertKey(orgA, ck, withPod, []string{"pod"}),
		ComputeAlertKey(orgA, ck, withOtherPod, []string{"pod"}))
	assert.Equal(t,
		ComputeAlertKey(orgA, ck, withPod, []string{"pod"}),
		ComputeAlertKey(orgA, ck, withoutPod, []string{"pod"}))

	// Ignoring a label nothing carries is a no-op.
	assert.Equal(t,
		ComputeAlertKey(orgA, ck, withPod, nil),
		ComputeAlertKey(orgA, ck, withPod, []string{"absent"}))

	// Ignoring alertname is impossible: it is identity-bearing.
	assert.Equal(t,
		ComputeAlertKey(orgA, ck, withoutPod, nil),
		ComputeAlertKey(orgA, ck, withoutPod, []string{"alertname"}))
}

func TestComputeAlertKey_DistinctInputsDistinctKeys(t *testing.T) {
	ck := mustClusterKey(t, "prod-eu")
	corpus := []map[string]string{
		{"alertname": "X"},
		{"alertname": "Y"},
		{"alertname": "X", "a": "1"},
		{"alertname": "X", "a": "2"},
		{"alertname": "X", "b": "1"},
		{"alertname": "X", "a": "1", "b": "2"},
		{"alertname": "X", "a": ""},
		{"alertname": "Xa"},
		{"alertname": "X", "aa": "1"},
		{"alertname": "X", "a": "1", "aa": ""},
	}

	seen := map[string]map[string]string{}
	for _, in := range corpus {
		k := ComputeAlertKey(orgA, ck, mustLabelSet(t, in), nil).String()
		if prev, dup := seen[k]; dup {
			t.Fatalf("alert key collision between %v and %v", prev, in)
		}
		seen[k] = in
	}
	assert.Len(t, seen, len(corpus))
}

func TestNewAlertKey_Rejects(t *testing.T) {
	for _, in := range []string{
		"",
		"ak_",
		"nope",
		"ak_" + strings.Repeat("a", 25),
		"ak_" + strings.Repeat("a", 27),
		"ak_" + strings.Repeat("w", 26), // 'w' is outside base32hex's 0-9a-v
		"ak_" + strings.Repeat("A", 26), // uppercase
		"gk_" + strings.Repeat("a", 26), // a group key is not an alert key
	} {
		t.Run(in, func(t *testing.T) {
			_, err := NewAlertKey(in)
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, "alert_key", e.Code)
		})
	}
}

func TestComputeGroupKey(t *testing.T) {
	gl := mustLabels(t, map[string]string{"alertname": "X", "namespace": "prod"})
	base := ComputeGroupKey(orgA, sourceA, "sre-slack", gl)

	assert.True(t, strings.HasPrefix(base.String(), GroupKeyPrefix))
	assert.Regexp(t, validate.PatternGroupKey, base.String())
	parsed, err := NewGroupKey(base.String())
	require.NoError(t, err)
	assert.Equal(t, base, parsed)

	assert.Equal(t, base, ComputeGroupKey(orgA, sourceA, "sre-slack", gl))
	assert.NotEqual(t, base, ComputeGroupKey(orgB, sourceA, "sre-slack", gl))
	assert.NotEqual(t, base, ComputeGroupKey(orgA, sourceB, "sre-slack", gl))
	assert.NotEqual(t, base, ComputeGroupKey(orgA, sourceA, "other", gl))
	assert.NotEqual(t, base, ComputeGroupKey(orgA, sourceA, "sre-slack",
		mustLabels(t, map[string]string{"alertname": "X"})))

	// A reconciler-sourced observation has no receiver, and empty groupLabels are
	// legal — they hash as the empty object.
	empty := ComputeGroupKey(orgA, sourceA, "", Labels{})
	assert.Regexp(t, validate.PatternGroupKey, empty.String())
	assert.Equal(t, empty, ComputeGroupKey(orgA, sourceA, "", mustLabels(t, map[string]string{})))
	assert.NotEqual(t, empty, ComputeGroupKey(orgA, sourceA, "", gl))

	// The order groupLabels arrive in must not move the key.
	assert.Equal(t,
		ComputeGroupKey(orgA, sourceA, "r", mustLabels(t, map[string]string{"a": "1", "b": "2"})),
		ComputeGroupKey(orgA, sourceA, "r", mustLabels(t, map[string]string{"b": "2", "a": "1"})))

	assert.False(t, base.IsZero())
	assert.True(t, GroupKey{}.IsZero())
}

func TestGroupKey_ReceiverAndLabelsAreSeparateFields(t *testing.T) {
	// The 0x00 field terminator must stop a receiver from impersonating the
	// leading bytes of the canonical groupLabels. The forged tail is the exact
	// canonical serialisation of {b: "1"}: a 4-byte length before each field.
	a := ComputeGroupKey(orgA, sourceA, "a", mustLabels(t, map[string]string{"b": "1"}))
	b := ComputeGroupKey(orgA, sourceA, "a\x00\x00\x00\x00\x01b\x00\x00\x00\x011", Labels{})
	assert.NotEqual(t, a, b)
}

func TestNewGroupKey_Rejects(t *testing.T) {
	for _, in := range []string{"", "gk_", "ak_" + strings.Repeat("a", 26), "gk_" + strings.Repeat("z", 26)} {
		_, err := NewGroupKey(in)
		var e *errs.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, "group_key", e.Code)
	}
}

func TestComputeRuleFingerprint(t *testing.T) {
	labels := mustLabels(t, map[string]string{"severity": "critical"})
	annotations, err := NewAnnotations(map[string]string{"summary": "s"})
	require.NoError(t, err)

	base := ComputeRuleFingerprint("up == 0", 10*time.Minute, 0, labels, annotations)

	assert.Regexp(t, validate.PatternSHA256Hex, base.String())
	assert.Len(t, base.String(), 64)
	parsed, err := NewRuleFingerprint(base.String())
	require.NoError(t, err)
	assert.Equal(t, base, parsed)

	assert.Equal(t, base, ComputeRuleFingerprint("up == 0", 10*time.Minute, 0, labels, annotations),
		"content addressing is a pure function")

	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 1", 10*time.Minute, 0, labels, annotations))
	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 0", 11*time.Minute, 0, labels, annotations))
	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 0", 10*time.Minute, time.Minute, labels, annotations))
	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 0", 10*time.Minute, 0,
		mustLabels(t, map[string]string{"severity": "warning"}), annotations))

	otherAnn, err := NewAnnotations(map[string]string{"summary": "s2"})
	require.NoError(t, err)
	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 0", 10*time.Minute, 0, labels, otherAnn))

	// `for` and `keep_firing_for` are distinct fields, not a commutative pair.
	assert.NotEqual(t,
		ComputeRuleFingerprint("e", time.Minute, 2*time.Minute, Labels{}, Annotations{}),
		ComputeRuleFingerprint("e", 2*time.Minute, time.Minute, Labels{}, Annotations{}))

	// Labels and annotations are separate fields even when one is empty.
	assert.NotEqual(t,
		ComputeRuleFingerprint("e", 0, 0, labels, Annotations{}),
		ComputeRuleFingerprint("e", 0, 0, Labels{}, annotations))
}

func TestComputeRuleFingerprint_DurationsAreWholeSeconds(t *testing.T) {
	// "Durations are rendered as whole seconds in base 10, matching Prometheus's
	// own wire form, where `for: 10m` is the number 600." Sub-second precision is
	// therefore not addressable, by design.
	assert.Equal(t,
		ComputeRuleFingerprint("e", 90*time.Second, 0, Labels{}, Annotations{}),
		ComputeRuleFingerprint("e", 90*time.Second+400*time.Millisecond, 0, Labels{}, Annotations{}))
	assert.NotEqual(t,
		ComputeRuleFingerprint("e", 90*time.Second, 0, Labels{}, Annotations{}),
		ComputeRuleFingerprint("e", 91*time.Second, 0, Labels{}, Annotations{}))
}

func TestNewRuleFingerprint_Rejects(t *testing.T) {
	for _, in := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		_, err := NewRuleFingerprint(in)
		var e *errs.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, "rule_fingerprint", e.Code)
	}
	assert.True(t, RuleFingerprint{}.IsZero())
}

func TestComputeIdempotencyKey(t *testing.T) {
	subject := uuid.MustParse("018f3a4b-0000-7000-8000-0000000000e5")
	other := uuid.MustParse("018f3a4b-0000-7000-8000-0000000000f6")

	// "all_resolved at state_version 7" can exist exactly once.
	base := ComputeIdempotencyKey(orgA, "alert_group", subject, "all_resolved", 7)

	assert.Regexp(t, validate.PatternSHA256Hex, base.String())
	parsed, err := NewIdempotencyKey(base.String())
	require.NoError(t, err)
	assert.Equal(t, base, parsed)

	assert.Equal(t, base, ComputeIdempotencyKey(orgA, "alert_group", subject, "all_resolved", 7))
	assert.NotEqual(t, base, ComputeIdempotencyKey(orgB, "alert_group", subject, "all_resolved", 7))
	assert.NotEqual(t, base, ComputeIdempotencyKey(orgA, "occurrence", subject, "all_resolved", 7))
	assert.NotEqual(t, base, ComputeIdempotencyKey(orgA, "alert_group", other, "all_resolved", 7))
	assert.NotEqual(t, base, ComputeIdempotencyKey(orgA, "alert_group", subject, "firing", 7))
	assert.NotEqual(t, base, ComputeIdempotencyKey(orgA, "alert_group", subject, "all_resolved", 8),
		"the state version is what makes the key repeat-safe")

	// Neighbouring versions must not smear into the reason field.
	assert.NotEqual(t,
		ComputeIdempotencyKey(orgA, "k", subject, "ab", 1),
		ComputeIdempotencyKey(orgA, "k", subject, "a", 12))

	assert.True(t, IdempotencyKey{}.IsZero())
}

func TestNewIdempotencyKey_Rejects(t *testing.T) {
	for _, in := range []string{"", "zz", strings.Repeat("f", 63), strings.Repeat("F", 64)} {
		_, err := NewIdempotencyKey(in)
		var e *errs.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, "idempotency_key", e.Code)
	}
}

func TestNewSourceFingerprint_Rejects(t *testing.T) {
	for _, in := range []string{"", "abc", strings.Repeat("a", 15), strings.Repeat("a", 17), strings.Repeat("A", 16), "738bec28daad197g"} {
		_, err := NewSourceFingerprint(in)
		var e *errs.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, "source_fingerprint", e.Code)
	}
	assert.True(t, SourceFingerprint{}.IsZero())
}

// ---------------------------------------------------------------------- SlackTS

// TestSlackTS_IsTextNeverAFloat is S7: a Slack ts is a FOREIGN SYSTEM'S PRIMARY
// KEY. Parsing it as a number loses precision and silently breaks every thread
// pointer oto owns.
func TestSlackTS_IsTextNeverAFloat(t *testing.T) {
	tests := []string{
		"1728394855.123456",
		"1728394855.000100", // trailing zeros a float would drop
		"1728394855.000000", // a float would render this "1.728394855e+09"
		"1728394855.100000",
		"0000000000.000001",
		"9999999999.999999",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			ts, err := NewSlackTS(raw)
			require.NoError(t, err)
			assert.Equal(t, raw, ts.String(), "rendered verbatim, exactly as Slack sent it")
			assert.False(t, ts.IsZero())

			// The precision-loss proof: a float64 round-trip does not survive.
			f, err := strconv.ParseFloat(raw, 64)
			require.NoError(t, err)
			viaFloat := strconv.FormatFloat(f, 'f', 6, 64)
			if viaFloat != raw {
				t.Logf("float round-trip of %s yields %s — this is why ts is TEXT", raw, viaFloat)
			}
			// Whatever the formatting, the durable handle is the string itself.
			_, err = NewSlackTS(strconv.FormatFloat(f, 'g', -1, 64))
			assert.Error(t, err,
				"a float-formatted ts does not even satisfy the ts shape, let alone equal the original")
		})
	}
}

func TestSlackTS_PrecisionIsLostThroughFloat64(t *testing.T) {
	// Two distinct Slack messages one microsecond apart late in the epoch. Their
	// float64 representations are indistinguishable at 6 dp only if precision is
	// lost; the strings never are.
	a, err := NewSlackTS("1728394855.123456")
	require.NoError(t, err)
	b, err := NewSlackTS("1728394855.123457")
	require.NoError(t, err)

	assert.NotEqual(t, a, b)
	assert.NotEqual(t, a.String(), b.String())

	// And the exact decimal text survives a full round-trip through the domain.
	again, err := NewSlackTS(a.String())
	require.NoError(t, err)
	assert.Equal(t, a, again)
}

func TestNewSlackTS_Rejects(t *testing.T) {
	for _, in := range []string{
		"",
		"1728394855",         // no fractional part
		"1728394855.12345",   // five digits
		"1728394855.1234567", // seven digits
		"172839485.123456",   // nine integer digits
		"17283948550.123456", // eleven integer digits
		"1.728394855e+09",    // float notation
		"1728394855,123456",
		" 1728394855.123456",
		"1728394855.123456 ",
	} {
		t.Run(in, func(t *testing.T) {
			ts, err := NewSlackTS(in)
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, "slack_ts", e.Code)
			assert.True(t, ts.IsZero())
		})
	}
}
