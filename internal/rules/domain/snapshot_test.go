package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/rules/domain"
)

var capturedAt = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func requireValidationCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	require.True(t, errors.Is(err, errs.ErrValidation), "want a validation error, got %v", err)
	var e *errs.Error
	require.True(t, errors.As(err, &e))
	assert.Equal(t, code, e.Code)
}

func validKey() domain.Key {
	return domain.Key{
		SourceID: "11111111-1111-1111-1111-111111111111",
		File:     "/etc/prometheus/rules/kube.yml",
		Group:    "kubernetes-apps",
		Name:     "KubePodCrashLooping",
	}
}

func validRecovery() domain.Recovery {
	return domain.Recovery{
		Origin:         domain.OriginPrometheusAPI,
		Strategy:       domain.StrategyRulesAPI,
		Confidence:     domain.ConfidenceExact,
		CandidateCount: 1,
		RuleName:       "KubePodCrashLooping",
		RuleGroup:      "kubernetes-apps",
		RuleFile:       "/etc/prometheus/rules/kube.yml",
		Expr:           `rate(kube_pod_container_status_restarts_total[15m]) > 0`,
		ForSeconds:     300,
		Labels:         map[string]string{"severity": "critical"},
		Annotations:    map[string]string{"summary": "pod restarting"},
		PrometheusURL:  "https://prom.internal",
	}
}

// ---------------------------------------------------------------------------
// Closed value sets
// ---------------------------------------------------------------------------

func TestOriginValid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   domain.Origin
		want bool
	}{
		{in: domain.OriginUnavailable, want: true},
		{in: domain.OriginGeneratorURL, want: true},
		{in: domain.OriginPrometheusAPI, want: true},
		{in: ""},
		{in: "generatorURL"},
		{in: "Unavailable"},
		{in: "rules_api"}, // a Strategy, not an Origin
	}

	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.in.Valid())
		})
	}

	// The wire values are a contract with rule_snapshots_origin_ck.
	assert.Equal(t, "unavailable", string(domain.OriginUnavailable))
	assert.Equal(t, "generator_url", string(domain.OriginGeneratorURL))
	assert.Equal(t, "prometheus_api", string(domain.OriginPrometheusAPI))
}

func TestConfidenceValid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   domain.Confidence
		want bool
	}{
		{in: domain.ConfidenceNone, want: true},
		{in: domain.ConfidenceExact, want: true},
		{in: domain.ConfidenceProbable, want: true},
		{in: domain.ConfidenceAmbiguous, want: true},
		{in: ""},
		{in: "Exact"},
		{in: "unknown"},
	}

	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.in.Valid())
		})
	}

	assert.Equal(t, "none", string(domain.ConfidenceNone))
	assert.Equal(t, "exact", string(domain.ConfidenceExact))
	assert.Equal(t, "probable", string(domain.ConfidenceProbable))
	assert.Equal(t, "ambiguous", string(domain.ConfidenceAmbiguous))
}

func TestStrategyValues(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "none", string(domain.StrategyNone))
	assert.Equal(t, "generator_url", string(domain.StrategyGeneratorURL))
	assert.Equal(t, "rules_api", string(domain.StrategyRulesAPI))
}

func TestKeyIsZero(t *testing.T) {
	t.Parallel()

	assert.True(t, domain.Key{}.IsZero())
	// A key with only a file and group names nothing.
	assert.True(t, domain.Key{File: "kube.yml", Group: "apps"}.IsZero())
	assert.False(t, domain.Key{SourceID: "s"}.IsZero())
	assert.False(t, domain.Key{Name: "X"}.IsZero())
}

func TestRecoveryRecovered(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   domain.Recovery
		want bool
	}{
		{
			name: "an expression from generatorURL",
			in:   domain.Recovery{Origin: domain.OriginGeneratorURL, Expr: "up == 0"},
			want: true,
		},
		{
			name: "an expression from the rules API",
			in:   domain.Recovery{Origin: domain.OriginPrometheusAPI, Expr: "up == 0"},
			want: true,
		},
		{
			name: "unavailable is the normal degraded path, not a failure",
			in:   domain.Recovery{Origin: domain.OriginUnavailable, Expr: "up == 0"},
		},
		{
			name: "an origin with no expression recovered nothing",
			in:   domain.Recovery{Origin: domain.OriginGeneratorURL},
		},
		{
			name: "a whitespace-only expression is no expression",
			in:   domain.Recovery{Origin: domain.OriginGeneratorURL, Expr: " \t\n "},
		},
		{
			name: "the zero Recovery",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.in.Recovered())
		})
	}
}

// ---------------------------------------------------------------------------
// Canon and Fingerprint
// ---------------------------------------------------------------------------

func TestCanon(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   map[string]string
		want string
	}{
		{name: "nil"},
		{name: "empty", in: map[string]string{}},
		{
			// SPEC §C.1: a 4-byte big-endian BYTE length before the name and
			// another before the value. No separators, so no value can forge
			// the framing of entries that are not there.
			name: "one entry",
			in:   map[string]string{"a": "1"},
			want: "\x00\x00\x00\x01a\x00\x00\x00\x011",
		},
		{
			name: "names ascend by byte order, not insertion order",
			in:   map[string]string{"zeta": "3", "alpha": "1", "Beta": "2"},
			want: "\x00\x00\x00\x04Beta\x00\x00\x00\x012" +
				"\x00\x00\x00\x05alpha\x00\x00\x00\x011" +
				"\x00\x00\x00\x04zeta\x00\x00\x00\x013",
		},
		{
			name: "an empty value still contributes its length prefix",
			in:   map[string]string{"a": "", "b": "1"},
			want: "\x00\x00\x00\x01a\x00\x00\x00\x00" +
				"\x00\x00\x00\x01b\x00\x00\x00\x011",
		},
		{
			name: "values are verbatim UTF-8 with no case folding",
			in:   map[string]string{"Severity": "Critical", "team": "SRE"},
			want: "\x00\x00\x00\x08Severity\x00\x00\x00\x08Critical" +
				"\x00\x00\x00\x04team\x00\x00\x00\x03SRE",
		},
		{
			name: "the old separators are ordinary content now",
			in:   map[string]string{"a": "1\x02c\x012"},
			want: "\x00\x00\x00\x01a\x00\x00\x00\x051\x02c\x012",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, domain.Canon(tc.in))
		})
	}
}

func TestCanonIsDeterministic(t *testing.T) {
	t.Parallel()

	m := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6"}
	first := domain.Canon(m)
	for i := 0; i < 200; i++ {
		require.Equal(t, first, domain.Canon(m))
	}
}

var hex64Re = regexp.MustCompile(domain.FingerprintPattern)

// TestFingerprintGolden pins the §C.6 wire format. A fingerprint is a content
// address that has to be recomputable anywhere and comparable byte for byte, so
// the digest itself is the contract, not an implementation detail.
//
// These values MOVED once, before oto's first release, when the pre-image's own
// fields went from 0x00-separated to length-prefixed — the same change that made
// canon() injective, applied one layer up so that a free-form `expr` cannot forge
// a field boundary. That re-fingerprinted every rule snapshot, deliberately and
// exactly once. They are frozen now.
func TestFingerprintGolden(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		"72f5f903e1ad514dd2449d62db96197f5a5f92c58ef86f0b264ad0e65e388fbf",
		domain.Fingerprint("", 0, 0, nil, nil))

	assert.Equal(t,
		"cbfac73aefd7e4c9c0a43ed2d87b290abfffcaf1554d265ca81169af46e7af5a",
		domain.Fingerprint("up == 0", 300, 0,
			map[string]string{"severity": "critical", "team": "sre"},
			map[string]string{"summary": "target down"}))
}

// TestFingerprintPreImageIsLengthPrefixed pins the framing itself, not just the
// digest, so a future edit that changes the pre-image cannot be mistaken for an
// unrelated hash difference.
//
// The pre-image of Fingerprint("", 0, 0, nil, nil) is five EMPTY fields: four
// length prefixes of zero and an empty remainder. Under the old 0x00 framing it
// was four NUL bytes.
func TestFingerprintPreImageIsLengthPrefixed(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte(
		"\x00\x00\x00\x00" + // len("")
			"\x00\x00\x00\x01" + "0" + // len("0") || "0"    (for_seconds)
			"\x00\x00\x00\x01" + "0" + // len("0") || "0"    (keep_firing_for)
			"\x00\x00\x00\x00", // len(canon(nil)) — canon(nil) is empty
	)) // canon(nil) annotations is the empty remainder
	assert.Equal(t, hex.EncodeToString(sum[:]), domain.Fingerprint("", 0, 0, nil, nil),
		"the §C.6 pre-image is uint32be(len(x))||x per field, with the annotations tail raw")
}

// TestFingerprintIsInjectiveOverAdversarialExprs is the property the framing
// exists to have, in the spirit of alerts/domain's canonical-serialisation
// injectivity test: no two DISTINCT rule definitions may share a content address.
//
// `expr` is free-form PromQL — nothing constrains its bytes — so the corpus is
// built from values chosen to attack the framing: NUL (which the old 0x00
// terminator reserved), four-byte runs that could be mistaken for a length
// prefix, decimal digits, empty, and multi-byte UTF-8 whose BYTE length differs
// from its rune count. A collision here would be two different rules reported as
// one, which is exactly the drift detection oto sells failing silently.
func TestFingerprintIsInjectiveOverAdversarialExprs(t *testing.T) {
	t.Parallel()

	adversarial := []string{
		"",
		"\x00",
		"\x00\x00\x00\x01",
		"up == 0",
		"up == 0\x00",
		"\x00up == 0",
		"a\x00\x00\x00\x00\x01b",
		"0",
		"00",
		"300",
		"\x00\x00\x00\x07up == 0",
		"日本語",
		"☃",
		strings.Repeat("x", 300),
	}
	labelSets := []map[string]string{
		nil,
		{"severity": "critical"},
		{"severity": "critical\x00team\x00sre"},
		{"severity": "", "team": ""},
	}
	annSets := []map[string]string{
		nil,
		{"summary": ""},
		{"summary": "\x00\x00\x00\x01"},
	}
	durations := []float64{0, 1, 300}

	seen := map[string]string{}
	for _, expr := range adversarial {
		for _, ls := range labelSets {
			for _, an := range annSets {
				for _, d := range durations {
					id := fmt.Sprintf("%q|%v|%q|%q", expr, d, canonID(ls), canonID(an))
					fp := domain.Fingerprint(expr, d, 0, ls, an)
					if prev, dup := seen[fp]; dup && prev != id {
						t.Fatalf("rule_fingerprint collision:\n  %s\n  %s\nboth address %s", prev, id, fp)
					}
					seen[fp] = id
				}
			}
		}
	}
	assert.Len(t, seen,
		len(adversarial)*len(labelSets)*len(annSets)*len(durations),
		"one content address per distinct rule definition")
}

// TestFingerprintAgreesWithTheKernel was written because §C.6 had TWO
// implementations that agreed only by luck. It survives the collapse to one,
// because what it asserts is still the thing that must never stop being true: the
// value this package returns is the kernel's value, for every input.
//
// It is close to a tautology today — domain.Fingerprint is three tokens of
// delegation — and that is precisely why it stays. The failure it guards against
// is not arithmetic drift, it is somebody re-inlining the digest here "to avoid
// the import", which is exactly how the pair came to exist the first time. The
// test goes red the moment the delegation stops.
//
// The corpus is no longer restricted to what both spellings could express. It now
// carries FRACTIONAL seconds and label values with a NUL — the inputs the old
// kernel spelling could not represent, and where the two implementations silently
// disagreed until this collapse.
func TestFingerprintAgreesWithTheKernel(t *testing.T) {
	t.Parallel()

	exprs := []string{"", "up == 0", "up{a=\"\x01\"} == 0", "日本語", strings.Repeat("x", 300)}
	labelSets := []map[string]string{
		nil,
		{"severity": "critical"},
		{"severity": "critical", "team": "sre"},
		{"severity": ""},
		{"severity": "\x01\x02"},
		{"severity": "\x00"},
		{"not a legal label name": "x"},
	}
	annSets := []map[string]string{
		nil,
		{"summary": "target down"},
		{"summary": ""},
		{"a\x00b": "\x00"},
	}

	for _, expr := range exprs {
		for _, ls := range labelSets {
			for _, an := range annSets {
				for _, secs := range []float64{0, 1, 1.5, 90.4, 300, 600} {
					want := alerts.ComputeRuleFingerprint(expr, secs, 0, ls, an).String()
					got := domain.Fingerprint(expr, secs, 0, ls, an)
					require.Equal(t, want, got,
						"§C.6 must have one value: expr=%q for=%gs labels=%v ann=%v", expr, secs, ls, an)
				}
			}
		}
	}
}

// TestCanonAgreesWithTheKernel is the other half, and the half that is NOT a
// tautology: Canon must be the same byte string alerts/domain produces for a
// label set that IS constructible, because §C.1 is one format shared by
// alert_key, group_key and this fingerprint. A raw map that NewLabels would refuse
// has no kernel value-object counterpart to compare against, so the overlap is
// where the claim lives.
func TestCanonAgreesWithTheKernel(t *testing.T) {
	t.Parallel()

	for _, m := range []map[string]string{
		nil,
		{},
		{"severity": "critical"},
		{"severity": "critical", "team": "sre"},
		{"a": "", "b": ""},
		{"ab": ""},
		{"severity": "\x01\x02"},
	} {
		labels, err := alerts.NewLabels(m)
		require.NoError(t, err)
		require.Equal(t, string(labels.Canonical(nil)), domain.Canon(m),
			"§C.1 must have one spelling: %v", m)
	}
}

// canonID renders a map unambiguously for use as a test identity. strconv.Quote
// on BOTH halves of every pair means no name/value boundary can be smeared, which
// is the property under test — so it must not be derived from domain.Canon.
func canonID(m map[string]string) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(strconv.Quote(n))
		b.WriteString("=")
		b.WriteString(strconv.Quote(m[n]))
		b.WriteString(";")
	}
	return b.String()
}

func TestFingerprintShape(t *testing.T) {
	t.Parallel()

	fp := domain.Fingerprint("up == 0", 300, 0, nil, nil)
	assert.Len(t, fp, 64)
	assert.True(t, hex64Re.MatchString(fp), "fingerprint must satisfy rule_snapshots_fp_ck: %q", fp)
	assert.Equal(t, strings.ToLower(fp), fp)
}

func TestFingerprintIsAContentAddress(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"severity": "critical", "team": "sre"}
	ann := map[string]string{"summary": "down"}
	base := domain.Fingerprint("up == 0", 300, 60, labels, ann)

	t.Run("insertion order does not matter", func(t *testing.T) {
		t.Parallel()
		reordered := map[string]string{"team": "sre", "severity": "critical"}
		assert.Equal(t, base, domain.Fingerprint("up == 0", 300, 60, reordered, ann))
	})

	t.Run("600 and 600.0 are the same rule", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t,
			domain.Fingerprint("up == 0", 600, 0, nil, nil),
			domain.Fingerprint("up == 0", 600.0, 0, nil, nil))
	})

	t.Run("a nil map and an empty map are the same rule", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t,
			domain.Fingerprint("up == 0", 1, 2, nil, nil),
			domain.Fingerprint("up == 0", 1, 2, map[string]string{}, map[string]string{}))
	})

	for _, tc := range []struct {
		name string
		got  string
	}{
		{name: "expr", got: domain.Fingerprint("up == 1", 300, 60, labels, ann)},
		{name: "for", got: domain.Fingerprint("up == 0", 301, 60, labels, ann)},
		{name: "keep_firing_for", got: domain.Fingerprint("up == 0", 300, 61, labels, ann)},
		{
			name: "a label value",
			got:  domain.Fingerprint("up == 0", 300, 60, map[string]string{"severity": "warning", "team": "sre"}, ann),
		},
		{
			name: "a new label",
			got:  domain.Fingerprint("up == 0", 300, 60, map[string]string{"severity": "critical", "team": "sre", "x": ""}, ann),
		},
		{
			name: "an annotation",
			got:  domain.Fingerprint("up == 0", 300, 60, labels, map[string]string{"summary": "DOWN"}),
		},
		{name: "whitespace in the expression", got: domain.Fingerprint("up==0", 300, 60, labels, ann)},
	} {
		t.Run("a change to "+tc.name+" changes the address", func(t *testing.T) {
			t.Parallel()
			assert.NotEqual(t, base, tc.got)
		})
	}
}

// TestFingerprintSeparatesLabelsFromAnnotations proves the 0x00 separator does
// its job: moving an entry from labels to annotations is a different rule.
func TestFingerprintSeparatesLabelsFromAnnotations(t *testing.T) {
	t.Parallel()

	asLabel := domain.Fingerprint("up == 0", 0, 0, map[string]string{"k": "v"}, nil)
	asAnnotation := domain.Fingerprint("up == 0", 0, 0, nil, map[string]string{"k": "v"})
	assert.NotEqual(t, asLabel, asAnnotation)
}

// ---------------------------------------------------------------------------
// NewSnapshot
// ---------------------------------------------------------------------------

func TestNewSnapshotHappyPath(t *testing.T) {
	t.Parallel()

	r := validRecovery()
	s := domain.NewSnapshot("org-1", validKey(), r, capturedAt)

	assert.Equal(t, "org-1", s.OrgID)
	assert.Equal(t, validKey(), s.Key)
	assert.Equal(t, r.Expr, s.Expr)
	assert.Equal(t, 300.0, s.ForSeconds)
	assert.Equal(t, domain.OriginPrometheusAPI, s.Origin)
	assert.Equal(t, domain.ConfidenceExact, s.Confidence)
	assert.Equal(t, 1, s.CandidateCount)
	assert.Equal(t, "https://prom.internal", s.PrometheusURL)
	assert.True(t, s.CapturedAt.Equal(capturedAt))
	assert.Equal(t,
		domain.Fingerprint(r.Expr, 300, 0, r.Labels, r.Annotations),
		s.Fingerprint)
	assert.True(t, s.Available())
	assert.False(t, s.Ambiguous())
	require.NoError(t, s.Validate())
}

// TestNewSnapshotUnavailableIsWellFormed: a Recovery that recovered nothing must
// become a storable row that says so, not an error and not an absent row.
func TestNewSnapshotUnavailableIsWellFormed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   domain.Recovery
	}{
		{
			name: "the zero Recovery",
			in:   domain.Recovery{RuleName: "KubePodCrashLooping"},
		},
		{
			name: "an explicitly unavailable recovery",
			in: domain.Recovery{
				Origin:   domain.OriginUnavailable,
				Strategy: domain.StrategyNone,
				RuleName: "KubePodCrashLooping",
			},
		},
		{
			name: "a whitespace-only expression is no expression",
			in: domain.Recovery{
				Origin:               domain.OriginPrometheusAPI,
				Expr:                 "   \n\t ",
				RuleName:             "KubePodCrashLooping",
				PrometheusURL:        "https://prom.internal",
				ForSeconds:           300,
				KeepFiringForSeconds: 60,
				Confidence:           domain.ConfidenceExact,
				CandidateCount:       1,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src"}, tc.in, capturedAt)

			// rule_snapshots_expr_ck binds the pair in BOTH directions.
			assert.Equal(t, domain.OriginUnavailable, s.Origin)
			assert.Equal(t, "", s.Expr)
			assert.False(t, s.Available())
			// A snapshot that knows nothing must not claim to know how it matched.
			assert.Equal(t, domain.ConfidenceNone, s.Confidence)
			assert.Equal(t, 0, s.CandidateCount)
			assert.Equal(t, "", s.PrometheusURL)
			assert.Equal(t, 0.0, s.ForSeconds)
			assert.Equal(t, 0.0, s.KeepFiringForSeconds)
			require.NoError(t, s.Validate(), "an unavailable snapshot must still be storable")
		})
	}
}

// TestNewSnapshotPromotesAnUnavailableOriginCarryingAnExpr is the other
// direction of rule_snapshots_expr_ck: an expression exists, so `unavailable` is
// a lie and is reconciled to the weakest honest origin.
func TestNewSnapshotPromotesAnUnavailableOriginCarryingAnExpr(t *testing.T) {
	t.Parallel()

	s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src", Name: "X"}, domain.Recovery{
		Origin:     domain.OriginUnavailable,
		Expr:       "up == 0",
		Confidence: domain.ConfidenceProbable,
		// A probable match with a candidate count, not the forced none/0.
		CandidateCount: 2,
	}, capturedAt)

	assert.Equal(t, domain.OriginGeneratorURL, s.Origin)
	assert.Equal(t, "up == 0", s.Expr)
	assert.Equal(t, domain.ConfidenceProbable, s.Confidence)
	assert.Equal(t, 2, s.CandidateCount)
	require.NoError(t, s.Validate())
}

func TestNewSnapshotClampsToTheDDLBounds(t *testing.T) {
	t.Parallel()

	t.Run("an oversized expression is clamped, not rejected", func(t *testing.T) {
		t.Parallel()
		r := validRecovery()
		r.Expr = strings.Repeat("x", domain.MaxExprBytes+1000)
		s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src", Name: "X"}, r, capturedAt)
		assert.Len(t, s.Expr, domain.MaxExprBytes)
		require.NoError(t, s.Validate())
	})

	t.Run("an expression exactly at the bound is untouched", func(t *testing.T) {
		t.Parallel()
		r := validRecovery()
		r.Expr = strings.Repeat("x", domain.MaxExprBytes)
		s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src", Name: "X"}, r, capturedAt)
		assert.Len(t, s.Expr, domain.MaxExprBytes)
		require.NoError(t, s.Validate())
	})

	t.Run("an oversized rule name is clamped", func(t *testing.T) {
		t.Parallel()
		r := validRecovery()
		r.RuleName = strings.Repeat("N", domain.MaxRuleNameBytes+1)
		s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src"}, r, capturedAt)
		assert.Len(t, s.Key.Name, domain.MaxRuleNameBytes)
		require.NoError(t, s.Validate())
	})

	t.Run("a rule name supplied on the Key is clamped too", func(t *testing.T) {
		t.Parallel()
		k := domain.Key{SourceID: "src", Name: strings.Repeat("N", domain.MaxRuleNameBytes+1)}
		s := domain.NewSnapshot("org-1", k, validRecovery(), capturedAt)
		assert.Len(t, s.Key.Name, domain.MaxRuleNameBytes)
		require.NoError(t, s.Validate())
	})

	t.Run("the expression is trimmed before it is measured", func(t *testing.T) {
		t.Parallel()
		r := validRecovery()
		r.Expr = "  up == 0  "
		s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src", Name: "X"}, r, capturedAt)
		assert.Equal(t, "up == 0", s.Expr)
	})

	t.Run("negative and NaN durations become zero", func(t *testing.T) {
		t.Parallel()
		r := validRecovery()
		r.ForSeconds = -1
		r.KeepFiringForSeconds = math.NaN()
		s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src", Name: "X"}, r, capturedAt)
		assert.Equal(t, 0.0, s.ForSeconds)
		assert.Equal(t, 0.0, s.KeepFiringForSeconds)
		require.NoError(t, s.Validate())
	})
}

func TestNewSnapshotKeyFallbacks(t *testing.T) {
	t.Parallel()

	r := validRecovery()
	r.RuleName = "FromRecovery"
	r.RuleGroup = "recovery-group"
	r.RuleFile = "recovery.yml"

	t.Run("an empty Key is filled from the Recovery", func(t *testing.T) {
		t.Parallel()
		s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src"}, r, capturedAt)
		assert.Equal(t, "FromRecovery", s.Key.Name)
		assert.Equal(t, "recovery-group", s.Key.Group)
		assert.Equal(t, "recovery.yml", s.Key.File)
		assert.Equal(t, "src", s.Key.SourceID)
	})

	t.Run("a populated Key wins", func(t *testing.T) {
		t.Parallel()
		s := domain.NewSnapshot("org-1", domain.Key{
			SourceID: "src", Name: "FromKey", Group: "key-group", File: "key.yml",
		}, r, capturedAt)
		assert.Equal(t, "FromKey", s.Key.Name)
		assert.Equal(t, "key-group", s.Key.Group)
		assert.Equal(t, "key.yml", s.Key.File)
	})

	t.Run("generatorURL knows the expression but not where it is written down", func(t *testing.T) {
		t.Parallel()
		s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src", Name: "X"}, domain.Recovery{
			Origin:         domain.OriginGeneratorURL,
			Strategy:       domain.StrategyGeneratorURL,
			Confidence:     domain.ConfidenceExact,
			CandidateCount: 1,
			Expr:           "up == 0",
		}, capturedAt)
		assert.Equal(t, "", s.Key.File)
		assert.Equal(t, "", s.Key.Group)
		require.NoError(t, s.Validate(), "an unlocated rule is still a storable snapshot")
	})
}

func TestNewSnapshotCopiesMaps(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"severity": "critical"}
	r := validRecovery()
	r.Labels = labels

	s := domain.NewSnapshot("org-1", validKey(), r, capturedAt)
	before := s.Fingerprint

	labels["severity"] = "warning"
	labels["injected"] = "yes"
	assert.Equal(t, map[string]string{"severity": "critical"}, s.Labels)
	assert.Equal(t, before, domain.Fingerprint(s.Expr, s.ForSeconds, s.KeepFiringForSeconds, s.Labels, s.Annotations))
}

func TestNewSnapshotNormalisesEmptyMapsAndTheClock(t *testing.T) {
	t.Parallel()

	r := validRecovery()
	r.Labels, r.Annotations = nil, nil
	tokyo := time.FixedZone("JST", 9*3600)

	s := domain.NewSnapshot("org-1", validKey(), r, capturedAt.In(tokyo))
	assert.NotNil(t, s.Labels)
	assert.NotNil(t, s.Annotations)
	assert.Empty(t, s.Labels)
	assert.Empty(t, s.Annotations)
	assert.Equal(t, time.UTC, s.CapturedAt.Location())
	assert.True(t, s.CapturedAt.Equal(capturedAt))
}

// TestNewSnapshotDeduplicatesByContent is what makes the history a list of EDITS
// rather than a list of fires: two captures of the same text, hours apart, share
// a content address.
func TestNewSnapshotDeduplicatesByContent(t *testing.T) {
	t.Parallel()

	a := domain.NewSnapshot("org-1", validKey(), validRecovery(), capturedAt)
	b := domain.NewSnapshot("org-1", validKey(), validRecovery(), capturedAt.Add(72*time.Hour))
	assert.Equal(t, a.Fingerprint, b.Fingerprint)

	edited := validRecovery()
	edited.ForSeconds = 600
	c := domain.NewSnapshot("org-1", validKey(), edited, capturedAt.Add(72*time.Hour))
	assert.NotEqual(t, a.Fingerprint, c.Fingerprint)
}

// TestNewSnapshotFingerprintIgnoresIdentity: the address is over the DEFINITION.
// Capturing the same rule text under a different key or org is the same content.
func TestNewSnapshotFingerprintIgnoresIdentity(t *testing.T) {
	t.Parallel()

	a := domain.NewSnapshot("org-1", validKey(), validRecovery(), capturedAt)

	other := validKey()
	other.SourceID = "22222222-2222-2222-2222-222222222222"
	b := domain.NewSnapshot("org-2", other, validRecovery(), capturedAt)

	assert.Equal(t, a.Fingerprint, b.Fingerprint)
}

// ---------------------------------------------------------------------------
// Snapshot.Validate
// ---------------------------------------------------------------------------

func validSnapshot() domain.Snapshot {
	return domain.NewSnapshot("org-1", validKey(), validRecovery(), capturedAt)
}

func TestSnapshotValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(s *domain.Snapshot)
		code   string
	}{
		{name: "the happy path", mutate: func(_ *domain.Snapshot) {}},
		{
			name:   "no org",
			mutate: func(s *domain.Snapshot) { s.OrgID = "" },
			code:   "rules_snapshot_no_org",
		},
		{
			name:   "no source",
			mutate: func(s *domain.Snapshot) { s.Key.SourceID = "" },
			code:   "rules_snapshot_no_source",
		},
		{
			name:   "an origin outside the closed set",
			mutate: func(s *domain.Snapshot) { s.Origin = "guessed" },
			code:   "rules_snapshot_bad_origin",
		},
		{
			name:   "an empty origin",
			mutate: func(s *domain.Snapshot) { s.Origin = "" },
			code:   "rules_snapshot_bad_origin",
		},
		{
			name:   "a confidence outside the closed set",
			mutate: func(s *domain.Snapshot) { s.Confidence = "maybe" },
			code:   "rules_snapshot_bad_confidence",
		},
		{
			name:   "a short fingerprint",
			mutate: func(s *domain.Snapshot) { s.Fingerprint = strings.Repeat("a", 63) },
			code:   "rules_snapshot_bad_fingerprint",
		},
		{
			name:   "a long fingerprint",
			mutate: func(s *domain.Snapshot) { s.Fingerprint = strings.Repeat("a", 65) },
			code:   "rules_snapshot_bad_fingerprint",
		},
		{
			name:   "an uppercase fingerprint",
			mutate: func(s *domain.Snapshot) { s.Fingerprint = strings.Repeat("A", 64) },
			code:   "rules_snapshot_bad_fingerprint",
		},
		{
			name:   "a non-hex fingerprint",
			mutate: func(s *domain.Snapshot) { s.Fingerprint = strings.Repeat("g", 64) },
			code:   "rules_snapshot_bad_fingerprint",
		},
		{
			name:   "an empty fingerprint",
			mutate: func(s *domain.Snapshot) { s.Fingerprint = "" },
			code:   "rules_snapshot_bad_fingerprint",
		},
		{
			name:   "an empty rule name",
			mutate: func(s *domain.Snapshot) { s.Key.Name = "" },
			code:   "rules_snapshot_bad_name",
		},
		{
			name:   "a whitespace-only rule name",
			mutate: func(s *domain.Snapshot) { s.Key.Name = "   " },
			code:   "rules_snapshot_bad_name",
		},
		{
			name:   "a rule name one byte over the bound",
			mutate: func(s *domain.Snapshot) { s.Key.Name = strings.Repeat("N", domain.MaxRuleNameBytes+1) },
			code:   "rules_snapshot_bad_name",
		},
		{
			name:   "a rule name exactly at the bound",
			mutate: func(s *domain.Snapshot) { s.Key.Name = strings.Repeat("N", domain.MaxRuleNameBytes) },
		},
		{
			name:   "a one-character rule name is the floor",
			mutate: func(s *domain.Snapshot) { s.Key.Name = "X" },
		},
		{
			name:   "unavailable with an expression",
			mutate: func(s *domain.Snapshot) { s.Origin = domain.OriginUnavailable },
			code:   "rules_snapshot_expr_origin_mismatch",
		},
		{
			name:   "an available origin with no expression",
			mutate: func(s *domain.Snapshot) { s.Expr = "" },
			code:   "rules_snapshot_expr_origin_mismatch",
		},
		{
			name:   "an available origin with a whitespace-only expression",
			mutate: func(s *domain.Snapshot) { s.Expr = "   " },
			code:   "rules_snapshot_expr_origin_mismatch",
		},
		{
			name:   "an expression one byte over the bound",
			mutate: func(s *domain.Snapshot) { s.Expr = strings.Repeat("x", domain.MaxExprBytes+1) },
			code:   "rules_snapshot_expr_too_large",
		},
		{
			name:   "an expression exactly at the bound",
			mutate: func(s *domain.Snapshot) { s.Expr = strings.Repeat("x", domain.MaxExprBytes) },
		},
		{
			name:   "a negative for",
			mutate: func(s *domain.Snapshot) { s.ForSeconds = -1 },
			code:   "rules_snapshot_negative_duration",
		},
		{
			name:   "a negative keep_firing_for",
			mutate: func(s *domain.Snapshot) { s.KeepFiringForSeconds = -0.5 },
			code:   "rules_snapshot_negative_duration",
		},
		{
			name:   "a zero for is legal",
			mutate: func(s *domain.Snapshot) { s.ForSeconds = 0 },
		},
		{
			name:   "prometheus_api with no prometheus_url",
			mutate: func(s *domain.Snapshot) { s.PrometheusURL = "" },
			code:   "rules_snapshot_missing_prometheus_url",
		},
		{
			name: "generator_url needs no prometheus_url",
			mutate: func(s *domain.Snapshot) {
				s.Origin = domain.OriginGeneratorURL
				s.PrometheusURL = ""
			},
		},
		{
			name:   "a negative candidate count",
			mutate: func(s *domain.Snapshot) { s.CandidateCount = -1 },
			code:   "rules_snapshot_negative_candidates",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := validSnapshot()
			tc.mutate(&s)
			err := s.Validate()
			if tc.code == "" {
				require.NoError(t, err)
				return
			}
			requireValidationCode(t, err, tc.code)
		})
	}
}

// TestSnapshotConfidenceIsLockedToCandidateCount pins rule_snapshots_conf_ck:
// "we guessed" must never be indistinguishable from "we knew".
func TestSnapshotConfidenceIsLockedToCandidateCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		conf  domain.Confidence
		count int
		ok    bool
	}{
		{conf: domain.ConfidenceNone, count: 0, ok: true},
		{conf: domain.ConfidenceNone, count: 1},
		{conf: domain.ConfidenceExact, count: 1, ok: true},
		{conf: domain.ConfidenceExact, count: 0},
		{conf: domain.ConfidenceExact, count: 2},
		{conf: domain.ConfidenceProbable, count: 1, ok: true},
		{conf: domain.ConfidenceProbable, count: 7, ok: true},
		{conf: domain.ConfidenceProbable, count: 0},
		{conf: domain.ConfidenceAmbiguous, count: 2, ok: true},
		{conf: domain.ConfidenceAmbiguous, count: 9, ok: true},
		{conf: domain.ConfidenceAmbiguous, count: 1},
		{conf: domain.ConfidenceAmbiguous, count: 0},
	}

	for _, tc := range cases {
		t.Run(string(tc.conf)+"/n="+strconv.Itoa(tc.count), func(t *testing.T) {
			t.Parallel()
			s := validSnapshot()
			s.Confidence, s.CandidateCount = tc.conf, tc.count
			err := s.Validate()
			if tc.ok {
				require.NoError(t, err)
				return
			}
			requireValidationCode(t, err, "rules_snapshot_confidence_mismatch")
		})
	}
}

func TestSnapshotAvailableAndAmbiguous(t *testing.T) {
	t.Parallel()

	s := validSnapshot()
	assert.True(t, s.Available())
	assert.False(t, s.Ambiguous())

	s.Origin = domain.OriginUnavailable
	assert.False(t, s.Available())

	// SPEC §D.6: an ambiguous match must be surfaceable, so the predicate is the
	// only thing standing between a guess and a claim.
	amb := validSnapshot()
	amb.Confidence, amb.CandidateCount = domain.ConfidenceAmbiguous, 3
	assert.True(t, amb.Ambiguous())
	require.NoError(t, amb.Validate())

	for _, c := range []domain.Confidence{domain.ConfidenceNone, domain.ConfidenceExact, domain.ConfidenceProbable} {
		s := validSnapshot()
		s.Confidence = c
		assert.False(t, s.Ambiguous())
	}
}

// TestNewSnapshotProducesAStorableRow is the property that matters at the write
// path: whatever a source hands back, the constructor's output passes Validate
// as long as the caller supplied an org, a source and a name.
func TestNewSnapshotProducesAStorableRow(t *testing.T) {
	t.Parallel()

	recoveries := []domain.Recovery{
		validRecovery(),
		{RuleName: "X"},
		{Origin: domain.OriginUnavailable, RuleName: "X", Confidence: domain.ConfidenceAmbiguous, CandidateCount: 4},
		{Origin: domain.OriginGeneratorURL, Expr: "up == 0", RuleName: "X", Confidence: domain.ConfidenceExact, CandidateCount: 1},
		{Origin: domain.OriginGeneratorURL, Expr: strings.Repeat("y", domain.MaxExprBytes*2), RuleName: "X", Confidence: domain.ConfidenceNone, CandidateCount: 0},
	}

	for i, r := range recoveries {
		s := domain.NewSnapshot("org-1", domain.Key{SourceID: "src"}, r, capturedAt)
		require.NoErrorf(t, s.Validate(), "recovery %d produced an unstorable snapshot: %+v", i, s)
		assert.True(t, hex64Re.MatchString(s.Fingerprint))
	}
}
