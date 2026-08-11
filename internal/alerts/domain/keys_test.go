package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"testing"

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

// forgedReceiver and forgedLabels are an EXACT collision under the framing §C used
// before length prefixes: `field || 0x00` per field, with the canonical
// groupLabels written raw as the tail.
//
//	("a", {b: ""})  ->  61 | 00 | 00 00 00 01 62 00 00 00 00
//	(forged,  {} )  ->  61 00 00 00 00 01 62 00 00 00 | 00 |
//
// Both are the same eleven bytes. The forgery works because canon({b: ""}) ENDS in
// a zero length prefix, which absorbs the terminator the receiver would have
// contributed — so the "no field contains a NUL" assumption is not even needed to
// break it once the receiver is free-form, which it is: it comes verbatim from the
// operator's alertmanager.yml.
const (
	forgedReceiver = "a\x00\x00\x00\x00\x01b\x00\x00\x00"
	honestReceiver = "a"
)

// oldPreimage reproduces the pre-image the retired framing built, written from the
// FORMAT and not from any surviving code, so that the test can demonstrate the
// collision it protects against rather than assert its absence and hope.
func oldPreimage(orgID, sourceID uuid.UUID, receiver string, groupLabels Labels) []byte {
	var b []byte
	b = append(append(b, orgID[:]...), 0x00)
	b = append(append(b, sourceID[:]...), 0x00)
	b = append(append(b, receiver...), 0x00)
	return append(b, groupLabels.Canonical(nil)...)
}

// TestGroupKey_ReceiverAndLabelsAreSeparateFields proves the field boundary
// between `receiver` and the canonical groupLabels is real.
//
// It is a proof and not a coincidence because it first establishes that the
// witness IS a collision under the old framing — the two pre-images are asserted
// byte-equal — and only then asserts that the two GroupKeys are different today.
// A test that merely asserted the second half would have passed under the old
// framing too, for any witness that happened to be off by a byte.
func TestGroupKey_ReceiverAndLabelsAreSeparateFields(t *testing.T) {
	honestLabels := mustLabels(t, map[string]string{"b": ""})

	// 1. The witness is a genuine collision under the framing this replaced.
	require.Equal(t,
		oldPreimage(orgA, sourceA, honestReceiver, honestLabels),
		oldPreimage(orgA, sourceA, forgedReceiver, Labels{}),
		"the witness must actually collide under `field || 0x00`, or this test proves nothing")

	// 2. And the two are different AlertGroups under the framing in force.
	honest := ComputeGroupKey(orgA, sourceA, honestReceiver, honestLabels)
	forged := ComputeGroupKey(orgA, sourceA, forgedReceiver, Labels{})
	assert.NotEqual(t, honest, forged,
		"a receiver may not forge the leading bytes of the group labels")

	// The near-miss the earlier version of this test used: it also has to be
	// distinct, but it was distinct under the old framing as well.
	assert.NotEqual(t,
		ComputeGroupKey(orgA, sourceA, "a", mustLabels(t, map[string]string{"b": "1"})),
		ComputeGroupKey(orgA, sourceA, "a\x00\x00\x00\x00\x01b\x00\x00\x00\x011", Labels{}))
}

// TestGroupKey_PreImageIsLengthPrefixed pins the framing itself rather than a
// digest, so an edit to the pre-image cannot be mistaken for an unrelated change.
func TestGroupKey_PreImageIsLengthPrefixed(t *testing.T) {
	gl := mustLabels(t, map[string]string{"b": "1"})

	var want []byte
	want = append(want, 0x00, 0x00, 0x00, 0x10) // len(org_id_bytes) == 16
	want = append(want, orgA[:]...)
	want = append(want, 0x00, 0x00, 0x00, 0x10) // len(source_id_bytes) == 16
	want = append(want, sourceA[:]...)
	want = append(want, 0x00, 0x00, 0x00, 0x03) // len("rcv")
	want = append(want, "rcv"...)
	want = append(want, gl.Canonical(nil)...) // the tail is raw: it is the remainder
	sum := sha256.Sum256(want)

	assert.Equal(t,
		GroupKeyPrefix+encodeIdentity(sum[:]),
		ComputeGroupKey(orgA, sourceA, "rcv", gl).String(),
		"the §C.4 pre-image is uint32be(len(x))||x per field, with canon(groupLabels) raw")
}

// adversarialFields is the corpus for the free-form fields of a §C key —
// `receiver` and `expr`, which are operator- and Prometheus-authored text with no
// charset at all.
//
// Unlike adversarialValues in labels_test.go, this corpus DOES contain NUL: the
// point of the change is that a NUL in a free-form field is no longer structural.
// The rest attack the new framing rather than the old one: four-byte runs that
// could be read as a length prefix, decimal digits (had the prefix been text),
// empty, and multi-byte UTF-8 whose BYTE length differs from its rune count.
var adversarialFields = []string{
	"",
	"\x00",
	"\x00\x00\x00\x00",
	"\x00\x00\x00\x01",
	"a",
	"a\x00",
	"\x00a",
	"a\x00b",
	forgedReceiver,
	"a\x00\x00\x00\x00\x01b\x00\x00\x00\x011",
	"\x00\x00\x00\x01b\x00\x00\x00\x011",
	"4",
	"0004",
	"\xff",
	"日本語", // 3 runes, 9 bytes
	"☃",   // 1 rune, 3 bytes
	strings.Repeat("x", 300),
}

// adversarialLabelSets are the label sets the free-form fields are paired against.
// Their VALUES may not carry a NUL — NewLabels refuses it as a STORABILITY bound,
// not as sanitisation — so they attack the nesting instead: an empty value makes
// canon() end in a zero length prefix, which is exactly what absorbed the old
// terminator and made forgedReceiver work.
func adversarialLabelSets(t *testing.T) []map[string]string {
	t.Helper()
	return []map[string]string{
		nil,
		{"b": ""},
		{"b": "1"},
		{"b": "\x01\x02"},
		{"b": "\x01\x01\x01\x01"},
		{"b": "", "c": ""},
		{"bc": ""},
	}
}

// fieldsID renders a tuple of raw fields unambiguously, as the ground truth the
// injectivity properties compare against. strconv.Quote is unambiguous, and
// quoting EVERY component means no boundary between them can be smeared — which
// is the property under test, so it must not be derived from the framing itself.
func fieldsID(parts ...string) string {
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, strconv.Quote(p))
	}
	return strings.Join(quoted, "|")
}

func labelsID(t *testing.T, in map[string]string) string {
	t.Helper()
	names := make([]string, 0, len(in))
	for n := range in {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, strconv.Quote(n)+"="+strconv.Quote(in[n]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// TestGroupKey_IsInjectiveOverAdversarialReceivers is for §C.4 what
// TestCanonical_IsInjectiveOverAdversarialValues is for §C.1: no two DISTINCT
// (receiver, groupLabels) pairs may share a GroupKey.
//
// A collision is not a hash collision — SHA-256 is not being doubted — it is two
// unrelated Alertmanager notification groups becoming one AlertGroup, with one
// Slack thread and one generation counter between them.
func TestGroupKey_IsInjectiveOverAdversarialReceivers(t *testing.T) {
	sets := adversarialLabelSets(t)

	seen := map[string]string{}
	ids := map[string]struct{}{}

	for _, receiver := range adversarialFields {
		for _, in := range sets {
			gl := mustLabels(t, in)
			id := fieldsID(receiver) + " " + labelsID(t, in)
			ids[id] = struct{}{}

			key := ComputeGroupKey(orgA, sourceA, receiver, gl).String()
			if prev, dup := seen[key]; dup && prev != id {
				t.Fatalf("GroupKey collision:\n  %s\n  %s\nboth key to %s", prev, id, key)
			}
			seen[key] = id
		}
	}

	assert.Equal(t, len(ids), len(seen), "one GroupKey per distinct (receiver, groupLabels)")
	assert.Equal(t, len(adversarialFields)*len(sets), len(ids), "the corpus must have no duplicates")
}

// TestRuleFingerprint_IsInjectiveOverAdversarialExprs is the same property for
// §C.6. `expr` is free-form PromQL and annotation names and values are
// deliberately unconstrained (NewAnnotations bounds only count and length), so all
// three are attacked at once.
//
// A collision here is a rule edit that oto reports as no edit — the drift
// detection that is the headline differentiator, silently answering "no".
func TestRuleFingerprint_IsInjectiveOverAdversarialExprs(t *testing.T) {
	// The label sets are RAW MAPS, not constructed Labels: §C.6 is the one §C key
	// whose labels come from Prometheus rather than from oto's boundary, so the
	// corpus is free to carry the bytes NewLabels refuses — a NUL in a value, a
	// name outside the label charset — and it must, because those are exactly the
	// inputs the kernel now accepts through CanonMap.
	sets := append(adversarialLabelSets(t),
		map[string]string{"b": "\x00"},
		map[string]string{"b": "\x00\x00\x00\x01"},
		map[string]string{"b\x00c": ""},
	)
	annSets := []map[string]string{
		nil,
		{"summary": ""},
		{"summary": "\x00"},
		{"summary": "\x00\x00\x00\x01"},
		{"a\x00b": ""},
	}
	// Whole seconds, a fractional second, and a value that renders differently
	// under the two duration spellings §C.6 used to have (90.4 truncated to "90").
	durations := []float64{0, 1, 1.5, 600, 90.4}

	seen := map[string]string{}
	ids := map[string]struct{}{}

	for _, expr := range adversarialFields {
		for _, in := range sets {
			for _, an := range annSets {
				for _, d := range durations {
					id := fieldsID(expr, strconv.FormatFloat(d, 'f', -1, 64)) +
						" " + labelsID(t, in) + " " + labelsID(t, an)
					ids[id] = struct{}{}

					fp := ComputeRuleFingerprint(expr, d, 0, in, an).String()
					if prev, dup := seen[fp]; dup && prev != id {
						t.Fatalf("RuleFingerprint collision:\n  %s\n  %s\nboth address %s", prev, id, fp)
					}
					seen[fp] = id
				}
			}
		}
	}

	assert.Equal(t, len(ids), len(seen), "one rule_fingerprint per distinct rule definition")
	assert.Equal(t, len(adversarialFields)*len(sets)*len(annSets)*len(durations), len(ids))
}

// TestAlertKey_IsInjectiveOverClusterKeys covers the remaining §C.2 field.
// `cluster_key`'s charset already excludes NUL, so it was never forgeable — the
// property is asserted anyway, because "safe because of a charset elsewhere" is
// exactly the reasoning that made the old framing look sound.
func TestAlertKey_IsInjectiveOverClusterKeys(t *testing.T) {
	clusterKeys := []string{"a", "b", "ab", "a-b", "a.b", "a_b", "prod-eu", "prod-eu-1"}
	labelSets := []map[string]string{
		{"alertname": "X"},
		{"alertname": "X", "b": ""},
		{"alertname": "X", "b": "1"},
		{"alertname": "Xb"},
		{"alertname": "X", "b": "\x01\x02"},
	}

	seen := map[string]string{}
	for _, ck := range clusterKeys {
		for _, in := range labelSets {
			id := strconv.Quote(ck) + " " + labelsID(t, in)
			key := ComputeAlertKey(orgA, mustClusterKey(t, ck), mustLabelSet(t, in), nil).String()
			if prev, dup := seen[key]; dup && prev != id {
				t.Fatalf("AlertKey collision:\n  %s\n  %s\nboth key to %s", prev, id, key)
			}
			seen[key] = id
		}
	}
	assert.Len(t, seen, len(clusterKeys)*len(labelSets))
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
	labels := map[string]string{"severity": "critical"}
	annotations := map[string]string{"summary": "s"}

	base := ComputeRuleFingerprint("up == 0", 600, 0, labels, annotations)

	assert.Regexp(t, validate.PatternSHA256Hex, base.String())
	assert.Len(t, base.String(), 64)
	parsed, err := NewRuleFingerprint(base.String())
	require.NoError(t, err)
	assert.Equal(t, base, parsed)

	assert.Equal(t, base, ComputeRuleFingerprint("up == 0", 600, 0, labels, annotations),
		"content addressing is a pure function")

	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 1", 600, 0, labels, annotations))
	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 0", 660, 0, labels, annotations))
	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 0", 600, 60, labels, annotations))
	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 0", 600, 0,
		map[string]string{"severity": "warning"}, annotations))
	assert.NotEqual(t, base, ComputeRuleFingerprint("up == 0", 600, 0, labels,
		map[string]string{"summary": "s2"}))

	// `for` and `keep_firing_for` are distinct fields, not a commutative pair.
	assert.NotEqual(t,
		ComputeRuleFingerprint("e", 60, 120, nil, nil),
		ComputeRuleFingerprint("e", 120, 60, nil, nil))

	// Labels and annotations are separate fields even when one is empty.
	assert.NotEqual(t,
		ComputeRuleFingerprint("e", 0, 0, labels, nil),
		ComputeRuleFingerprint("e", 0, 0, nil, annotations))

	// A nil map and an empty map are the same rule: §C.6 hashes the entries, and
	// there are none either way.
	assert.Equal(t,
		ComputeRuleFingerprint("e", 0, 0, nil, nil),
		ComputeRuleFingerprint("e", 0, 0, map[string]string{}, map[string]string{}))
}

// TestComputeRuleFingerprint_DurationsAreShortestRoundTrip pins the rendering of
// `for` and `keep_firing_for`, which is the ONE place the two §C.6
// implementations ever disagreed.
//
// The kernel used to take a time.Duration and write int64(d/time.Second), so
// `for: 1s500ms` addressed as "1"; rules/domain took float seconds and wrote
// strconv.FormatFloat(f, 'f', -1, 64), so the same rule addressed as "1.5". Every
// stored rule_fingerprint was computed by the second one — NewSnapshot is the only
// production path — so the float rendering is the one that must survive, and the
// kernel's truncation was a latent re-keying bug rather than a design choice.
// TestFingerprintAgreesWithTheKernel could not see it: its corpus was whole
// seconds, the only inputs the truncating spelling could express.
func TestComputeRuleFingerprint_DurationsAreShortestRoundTrip(t *testing.T) {
	// Sub-second precision IS addressable: `for: 1s500ms` is a different rule
	// from `for: 1s`, and reporting them as the same rule would be missed drift.
	assert.NotEqual(t,
		ComputeRuleFingerprint("e", 90, 0, nil, nil),
		ComputeRuleFingerprint("e", 90.4, 0, nil, nil))
	assert.NotEqual(t,
		ComputeRuleFingerprint("e", 90, 0, nil, nil),
		ComputeRuleFingerprint("e", 91, 0, nil, nil))

	// 600 and 600.0 are one rule: the shortest round-tripping form is the same
	// string for both, which is why Prometheus reporting `duration: 600.0` after
	// reporting `600` does not read as an edit.
	assert.Equal(t,
		ComputeRuleFingerprint("e", 600, 0, nil, nil),
		ComputeRuleFingerprint("e", float64(600.0), 0, nil, nil))
}

// TestComputeRuleFingerprint_PreImage spells the §C.6 pre-image out by hand and
// hashes it independently. It is the golden vector for the clause: every other
// §C.6 test compares one call of the function against another, so a change to the
// framing would move every expected value with it and stay green. This one cannot
// move — it is written in literal bytes.
func TestComputeRuleFingerprint_PreImage(t *testing.T) {
	expr := "up == 0"
	labels := map[string]string{"b": "2", "a": "1"}
	ann := map[string]string{"summary": "s"}

	var pre []byte
	f := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		pre = append(pre, n[:]...)
		pre = append(pre, b...)
	}
	// canon(labels): names ASC, each name and value length-prefixed.
	canonLabels := []byte{
		0, 0, 0, 1, 'a', 0, 0, 0, 1, '1',
		0, 0, 0, 1, 'b', 0, 0, 0, 1, '2',
	}
	canonAnn := []byte{
		0, 0, 0, 7, 's', 'u', 'm', 'm', 'a', 'r', 'y', 0, 0, 0, 1, 's',
	}

	f([]byte(expr))
	f([]byte("600"))
	f([]byte("0"))
	f(canonLabels)
	pre = append(pre, canonAnn...) // the tail is written raw

	sum := sha256.Sum256(pre)
	assert.Equal(t, hex.EncodeToString(sum[:]),
		ComputeRuleFingerprint(expr, 600, 0, labels, ann).String())
}

// TestComputeIdempotencyKey_PreImage is the same golden vector for §C.7.
func TestComputeIdempotencyKey_PreImage(t *testing.T) {
	subject := uuid.MustParse("018f3a4b-0000-7000-8000-0000000000e5")

	var pre []byte
	f := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		pre = append(pre, n[:]...)
		pre = append(pre, b...)
	}
	org, subj := orgA, subject
	f(org[:])
	f([]byte("alert_group"))
	f(subj[:])
	f([]byte("all_resolved"))
	pre = append(pre, "7"...) // itoa(state_version) is the tail, written raw

	sum := sha256.Sum256(pre)
	assert.Equal(t, hex.EncodeToString(sum[:]),
		ComputeIdempotencyKey(orgA, "alert_group", subject, "all_resolved", 7).String())
}

// TestCanonMapAgreesWithLabelsCanonical is what is left of the C.6 cross-check
// once the two fingerprint implementations have become one: the interesting claim
// was never "the two digests match", it was "canonicalising a raw map and
// canonicalising a constructed Labels are the same function". That claim outlives
// the collapse, because CanonMap is the door §C.6 needs and Labels.Canonical is
// what §C.2 and §C.4 hash.
//
// It holds by construction now — CanonMap IS Labels.canonical, reached without the
// constructor — and the test stays so that a future edit to either cannot quietly
// fork them again.
func TestCanonMapAgreesWithLabelsCanonical(t *testing.T) {
	for _, in := range adversarialLabelSets(t) {
		labels := mustLabels(t, in)
		assert.Equal(t, labels.Canonical(nil), CanonMap(in), "§C.1 has one spelling: %v", in)
	}
	assert.Empty(t, CanonMap(nil), "no entries, no bytes")
	assert.Empty(t, CanonMap(map[string]string{}))
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
