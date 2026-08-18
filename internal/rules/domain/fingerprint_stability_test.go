package domain_test

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/rules/domain"
)

// The fingerprint's two failure modes, and why both are fatal to the product.
//
// `rule_fingerprint` is the whole drift mechanism (ADR 0009): "the newest
// snapshot for this rule key has a different fingerprint than the one bound to
// the previous case". So it has exactly two ways to be wrong, and they
// fail in opposite directions:
//
//   - TOO UNSTABLE — the same rule, expressed the same way, addressed twice.
//     Every case then records a spurious `rule.definition_changed`, the
//     Slack thread grows a "the rule changed" reply that is a lie, and the drift
//     signal becomes noise an operator learns to ignore.
//   - TOO STABLE — a real edit that does not move the address. The threshold was
//     lowered from 90% to 70% two hours ago, oto stores one row, and the history
//     quietly says nothing changed. That is the ONE question this module exists
//     to answer, answered wrongly and silently.
//
// snapshot_test.go pins the §C.6 WIRE FORMAT — the golden digests, the
// length-prefixed pre-image, injectivity against adversarial `expr` bytes. This
// file pins the PRODUCT PROPERTY on top of it: which differences are semantic and
// which are not, asserted through NewSnapshot as well as through Fingerprint,
// because NewSnapshot is what the capture path actually calls.

// stableRecovery is a recovered rule with several labels and annotations, which
// is what makes ordering testable at all.
func stableRecovery() domain.Recovery {
	return domain.Recovery{
		Origin:               domain.OriginPrometheusAPI,
		Strategy:             domain.StrategyRulesAPI,
		Confidence:           domain.ConfidenceExact,
		CandidateCount:       1,
		RuleName:             "HighErrorRate",
		RuleGroup:            "checkout",
		RuleFile:             "/etc/prometheus/rules/checkout.yml",
		Expr:                 `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.05`,
		ForSeconds:           300,
		KeepFiringForSeconds: 120,
		Labels: map[string]string{
			"severity": "critical",
			"team":     "checkout",
			"tier":     "0",
			"service":  "checkout-api",
		},
		Annotations: map[string]string{
			"summary":     "error rate above 5%",
			"description": "checkout is returning 5xx",
			"runbook_url": "https://runbooks.example/checkout",
		},
		PrometheusURL: "https://prom.internal",
	}
}

func stableKey() domain.Key {
	return domain.Key{
		SourceID: "33333333-3333-3333-3333-333333333333",
		File:     "/etc/prometheus/rules/checkout.yml",
		Group:    "checkout",
		Name:     "HighErrorRate",
	}
}

func fingerprintOf(t *testing.T, r domain.Recovery) string {
	t.Helper()
	s := domain.NewSnapshot("org-1", stableKey(), r, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	require.NoError(t, s.Validate(), "the fixture must be storable, or the comparison is meaningless")
	return s.Fingerprint
}

// ---------------------------------------------------------------------------
// STABLE: differences that are not semantic
// ---------------------------------------------------------------------------

// TestFingerprintIsStableAcrossMapOrderingAtScale is the map-iteration hazard,
// exercised at a size where Go's randomised range order actually bites.
//
// The existing two-key reorder proves the sort is applied; seven names built in
// fifty different insertion orders proves it is applied to the WHOLE set. A
// fingerprint that varied with map ordering would re-address the same rule on
// most fires, which is the "spurious drift" failure in its purest form.
func TestFingerprintIsStableAcrossMapOrderingAtScale(t *testing.T) {
	t.Parallel()

	labelNames := []string{"severity", "team", "tier", "service", "region", "cluster", "shard"}
	labelValues := []string{"critical", "checkout", "0", "checkout-api", "eu-west-1", "prod", "7"}
	annNames := []string{"summary", "description", "runbook_url", "dashboard", "impact"}
	annValues := []string{"a", "b", "https://x.example", "https://d.example", "high"}

	build := func(names, values []string, order []int) map[string]string {
		m := make(map[string]string, len(order))
		for _, i := range order {
			m[names[i]] = values[i]
		}
		return m
	}

	first := ""
	for attempt := 0; attempt < 50; attempt++ {
		lo := rand.Perm(len(labelNames))
		ao := rand.Perm(len(annNames))

		r := stableRecovery()
		r.Labels = build(labelNames, labelValues, lo)
		r.Annotations = build(annNames, annValues, ao)

		got := fingerprintOf(t, r)
		if first == "" {
			first = got
			continue
		}
		require.Equalf(t, first, got,
			"insertion order %v/%v produced a second content address for one rule", lo, ao)
	}
	assert.True(t, hex64Re.MatchString(first))
}

// TestFingerprintIsStableAcrossDurationSpelling: `for` and `keep_firing_for` are
// float SECONDS on Prometheus's wire (ADR 0009), and a float has many spellings.
// The kernel renders them with FormatFloat(f, 'f', -1, 64) — the shortest form
// that round-trips — so every spelling of one value is one address.
func TestFingerprintIsStableAcrossDurationSpelling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b float64
	}{
		{name: "600 and 600.0", a: 600, b: 600.0},
		{name: "600 and 6e2", a: 600, b: 6e2},
		{name: "0.5 and 5e-1", a: 0.5, b: 5e-1},
		{name: "0.5 and 500ms as seconds", a: 0.5, b: 500.0 / 1000.0},
		{name: "1.5 and 3/2", a: 1.5, b: 3.0 / 2.0},
		{name: "0 and 0.0", a: 0, b: 0.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := stableRecovery(), stableRecovery()
			a.ForSeconds, b.ForSeconds = tc.a, tc.b
			assert.Equal(t, fingerprintOf(t, a), fingerprintOf(t, b))

			a, b = stableRecovery(), stableRecovery()
			a.KeepFiringForSeconds, b.KeepFiringForSeconds = tc.a, tc.b
			assert.Equal(t, fingerprintOf(t, a), fingerprintOf(t, b))
		})
	}
}

// TestNewSnapshotFingerprintIsStableAcrossNonSemanticInput walks the differences
// that reach NewSnapshot but must not reach the content address.
//
// These are asserted through NewSnapshot rather than through Fingerprint on
// purpose: the constructor trims, clamps and normalises before it hashes, and it
// is the constructor the capture path calls. Whitespace around `expr` is the
// interesting one — it IS a semantic difference to Fingerprint (see
// snapshot_test.go's "whitespace in the expression"), and it is NOT one here,
// because clamp() trims first.
func TestNewSnapshotFingerprintIsStableAcrossNonSemanticInput(t *testing.T) {
	t.Parallel()

	base := fingerprintOf(t, stableRecovery())

	cases := []struct {
		name   string
		mutate func(r *domain.Recovery)
	}{
		{
			name: "leading and trailing whitespace around the expression",
			mutate: func(r *domain.Recovery) {
				r.Expr = "  \t\n" + r.Expr + " \n "
			},
		},
		{
			name:   "a different match confidence",
			mutate: func(r *domain.Recovery) { r.Confidence, r.CandidateCount = domain.ConfidenceAmbiguous, 4 },
		},
		{
			name:   "a different recovery strategy",
			mutate: func(r *domain.Recovery) { r.Strategy = domain.StrategyGeneratorURL },
		},
		{
			name:   "a different Prometheus",
			mutate: func(r *domain.Recovery) { r.PrometheusURL = "https://prom-2.internal" },
		},
		{
			name:   "different recovery notes",
			mutate: func(r *domain.Recovery) { r.Notes = []string{"duplicate_alertname", "expr_divergence"} },
		},
		{
			name: "a rule that moved to another file and group",
			mutate: func(r *domain.Recovery) {
				r.RuleFile, r.RuleGroup = "/etc/prometheus/rules/other.yml", "other-group"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := stableRecovery()
			tc.mutate(&r)
			assert.Equal(t, base, fingerprintOf(t, r))
		})
	}

	// A lookup that returned no labels and one that returned an empty object are
	// the same rule, so a source that starts sending `{}` does not fork the
	// history. Asserted here through NewSnapshot, which normalises nil to an
	// empty map before it hashes.
	t.Run("nil maps and empty maps are one rule", func(t *testing.T) {
		t.Parallel()
		nilled, emptied := stableRecovery(), stableRecovery()
		nilled.Labels, nilled.Annotations = nil, nil
		emptied.Labels, emptied.Annotations = map[string]string{}, map[string]string{}
		assert.Equal(t, fingerprintOf(t, nilled), fingerprintOf(t, emptied))
		assert.NotEqual(t, base, fingerprintOf(t, nilled),
			"...and dropping the labels entirely IS a change")
	})
}

// TestSnapshotFingerprintIgnoresWhenAndWhereItWasCaptured: the address is over
// the DEFINITION. Two fires a week apart, through different sources, in different
// orgs, of the same rule text are ONE content address — which is exactly what
// makes `rule_snapshots` cost one row per rule text rather than one per fire.
func TestSnapshotFingerprintIgnoresWhenAndWhereItWasCaptured(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	a := domain.NewSnapshot("org-1", stableKey(), stableRecovery(), at)

	other := stableKey()
	other.SourceID = "44444444-4444-4444-4444-444444444444"
	b := domain.NewSnapshot("org-2", other, stableRecovery(), at.Add(7*24*time.Hour))

	assert.Equal(t, a.Fingerprint, b.Fingerprint)
	assert.NotEqual(t, a.Key, b.Key, "the identities differ; only the content matches")
}

// ---------------------------------------------------------------------------
// UNSTABLE: every semantic field
// ---------------------------------------------------------------------------

// TestNewSnapshotFingerprintChangesOnEverySemanticField is the other direction,
// and the one that matters at 3am: an edit oto cannot see is an alert history
// that lies about why the alert fired.
//
// Every case is a rule an operator would call DIFFERENT. Note the label and
// annotation cases: not just "a value changed" but a NAME changed, a name
// removed, and a key moved between the two maps — a rename is the most common
// shape of a real rule edit and the easiest for a lazy canonicalisation to miss.
func TestNewSnapshotFingerprintChangesOnEverySemanticField(t *testing.T) {
	t.Parallel()

	base := fingerprintOf(t, stableRecovery())

	cases := []struct {
		name   string
		mutate func(r *domain.Recovery)
	}{
		{
			name:   "the threshold in the expression",
			mutate: func(r *domain.Recovery) { r.Expr = mustReplace(r.Expr, "> 0.05", "> 0.07") },
		},
		{
			name:   "the range in the expression",
			mutate: func(r *domain.Recovery) { r.Expr = mustReplace(r.Expr, "[5m]", "[15m]") },
		},
		{name: "for", mutate: func(r *domain.Recovery) { r.ForSeconds = 600 }},
		{name: "keep_firing_for", mutate: func(r *domain.Recovery) { r.KeepFiringForSeconds = 0 }},
		{
			name:   "a label value",
			mutate: func(r *domain.Recovery) { r.Labels["severity"] = "warning" },
		},
		{
			name: "a label RENAMED",
			mutate: func(r *domain.Recovery) {
				r.Labels["criticality"] = r.Labels["severity"]
				delete(r.Labels, "severity")
			},
		},
		{
			name:   "a label added",
			mutate: func(r *domain.Recovery) { r.Labels["page"] = "true" },
		},
		{
			name:   "a label removed",
			mutate: func(r *domain.Recovery) { delete(r.Labels, "tier") },
		},
		{
			name:   "a label emptied rather than removed",
			mutate: func(r *domain.Recovery) { r.Labels["tier"] = "" },
		},
		{
			name:   "an annotation value",
			mutate: func(r *domain.Recovery) { r.Annotations["summary"] = "error rate above 7%" },
		},
		{
			name: "an annotation RENAMED",
			mutate: func(r *domain.Recovery) {
				r.Annotations["runbook"] = r.Annotations["runbook_url"]
				delete(r.Annotations, "runbook_url")
			},
		},
		{
			name:   "an annotation added",
			mutate: func(r *domain.Recovery) { r.Annotations["dashboard"] = "https://d.example" },
		},
		{
			name:   "an annotation removed",
			mutate: func(r *domain.Recovery) { delete(r.Annotations, "description") },
		},
		{
			name: "a key moved from labels to annotations",
			mutate: func(r *domain.Recovery) {
				r.Annotations["tier"] = r.Labels["tier"]
				delete(r.Labels, "tier")
			},
		},
	}

	seen := map[string]string{"base": base}
	for _, tc := range cases {
		r := stableRecovery()
		tc.mutate(&r)
		got := fingerprintOf(t, r)

		assert.NotEqualf(t, base, got, "changing %s must change the content address", tc.name)
		if prev, dup := seen[got]; dup {
			t.Errorf("two different rules share one content address: %q and %q", prev, tc.name)
		}
		seen[got] = tc.name
	}
}

// TestFingerprintDistinguishesFractionalSeconds is a REGRESSION TEST for a real
// latent defect, not a hypothetical.
//
// §C.6 had two implementations. The kernel's truncated `for` to whole seconds
// (`strconv.Itoa(int(forSeconds))`) while the live one in this package rendered
// it as a float, so `for: 1s500ms` and `for: 1s` were ONE content address in the
// spelling a reader would assume canonical. Only one of the two values was ever
// stored, which is why the disagreement went unnoticed until the two were
// cross-checked over inputs they could both express — fractional seconds were
// outside that overlap.
//
// c133981 collapsed them onto the float rendering. A sub-second `for` is not
// exotic: `for: 500ms` is legal Prometheus, and `/api/v1/rules` reports
// `duration` as a float number of seconds, so anything under one second lands
// here as a fraction. If this test goes green-to-red, somebody has reintroduced
// integer truncation and every sub-second rule edit has become invisible.
func TestFingerprintDistinguishesFractionalSeconds(t *testing.T) {
	t.Parallel()

	// Values that a truncating implementation would collapse onto the same
	// integer, plus the integer itself.
	durations := []float64{0, 0.001, 0.25, 0.5, 0.999, 1, 1.5, 1.999, 2, 59.5, 60}

	t.Run("for", func(t *testing.T) {
		t.Parallel()
		seen := map[string]float64{}
		for _, d := range durations {
			r := stableRecovery()
			r.ForSeconds = d
			fp := fingerprintOf(t, r)
			if prev, dup := seen[fp]; dup {
				t.Fatalf("for: %gs and for: %gs share a content address — `for` is being truncated", prev, d)
			}
			seen[fp] = d
		}
	})

	t.Run("keep_firing_for", func(t *testing.T) {
		t.Parallel()
		seen := map[string]float64{}
		for _, d := range durations {
			r := stableRecovery()
			r.KeepFiringForSeconds = d
			fp := fingerprintOf(t, r)
			if prev, dup := seen[fp]; dup {
				t.Fatalf("keep_firing_for: %gs and %gs share a content address", prev, d)
			}
			seen[fp] = d
		}
	})

	// The headline case, stated the way the bug report did.
	oneSecond, oneAndAHalf := stableRecovery(), stableRecovery()
	oneSecond.ForSeconds, oneAndAHalf.ForSeconds = 1, 1.5
	assert.NotEqual(t, fingerprintOf(t, oneSecond), fingerprintOf(t, oneAndAHalf),
		"`for: 1s` and `for: 1s500ms` are different rules and must have different addresses")

	// `for` and `keep_firing_for` are different FIELDS, not one duration: a rule
	// with for=1.5 is not a rule with keep_firing_for=1.5.
	forOnly, keepOnly := stableRecovery(), stableRecovery()
	forOnly.ForSeconds, forOnly.KeepFiringForSeconds = 1.5, 0
	keepOnly.ForSeconds, keepOnly.KeepFiringForSeconds = 0, 1.5
	assert.NotEqual(t, fingerprintOf(t, forOnly), fingerprintOf(t, keepOnly))
}

// TestFingerprintOverEmptyAndDegenerateMaps covers the boundary the framing
// exists to hold: an absent entry, a present-but-empty one, and a name/value
// boundary that could be smeared.
func TestFingerprintOverEmptyAndDegenerateMaps(t *testing.T) {
	t.Parallel()

	const expr = "up == 0"
	distinct := map[string]string{}
	add := func(name string, m map[string]string) {
		fp := domain.Fingerprint(expr, 0, 0, m, nil)
		if prev, dup := distinct[fp]; dup {
			t.Errorf("%s and %s share a content address", prev, name)
		}
		distinct[fp] = name
	}

	add("no labels at all", nil)
	add("one empty name with an empty value", map[string]string{"": ""})
	add("one name with an empty value", map[string]string{"a": ""})
	add("an empty name with a value", map[string]string{"": "a"})
	add(`{"ab":"c"}`, map[string]string{"ab": "c"})
	add(`{"a":"bc"}`, map[string]string{"a": "bc"})
	add(`{"a":"b","c":""}`, map[string]string{"a": "b", "c": ""})

	// nil and an empty map are the SAME rule, so that a lookup that returned no
	// labels and one that returned an empty object do not fork the history.
	assert.Equal(t,
		domain.Fingerprint(expr, 0, 0, nil, nil),
		domain.Fingerprint(expr, 0, 0, map[string]string{}, map[string]string{}))
}

// TestFingerprintNegativeZeroSeconds documents a WART, so that nobody discovers
// it as a mystery drift event.
//
// `nonNegative` rejects f < 0, and IEEE-754 negative zero is not less than zero,
// so it survives into the pre-image — where FormatFloat renders it "-0" rather
// than "0". A rule with `for: -0` therefore has a different content address from
// the same rule with `for: 0`.
//
// It is not reachable from Prometheus in practice (`/api/v1/rules` reports a
// duration of zero as `0`), which is why this is documented rather than fixed
// here: the fix belongs in production code and this task is forbidden from
// touching it. Recorded so the behaviour is a decision and not a surprise.
func TestFingerprintNegativeZeroSeconds(t *testing.T) {
	t.Parallel()

	negZero := math.Copysign(0, -1)
	require.False(t, negZero < 0, "negative zero is not negative, which is the whole trap")

	pos := stableRecovery()
	pos.ForSeconds = 0
	neg := stableRecovery()
	neg.ForSeconds = negZero

	assert.NotEqual(t, fingerprintOf(t, pos), fingerprintOf(t, neg),
		"KNOWN WART: `for: -0` addresses differently from `for: 0` because FormatFloat renders it \"-0\"")
}

// mustReplace is strings.Replace(s, old, new, 1) WITH A GUARD.
//
// A substitution that matched nothing would leave the rule unchanged, and the
// test above would then assert that an unchanged rule has a different
// fingerprint — failing for the right reason today and, once somebody edits the
// fixture expression, passing for no reason at all. The panic keeps the fixture
// and the assertion honest with each other.
func mustReplace(s, old, replacement string) string {
	idx := strings.Index(s, old)
	if idx < 0 {
		panic(fmt.Sprintf("fixture expression no longer contains %q", old))
	}
	return s[:idx] + replacement + s[idx+len(old):]
}
