package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/rules/domain"
)

// snap builds a stored snapshot directly. Compare is a pure function over two
// rows, so the rows are what the tests construct.
func snap(expr string, forS, keepS float64, labels, annotations map[string]string) domain.Snapshot {
	return domain.Snapshot{
		OrgID:                "org-1",
		Key:                  validKey(),
		Expr:                 expr,
		ForSeconds:           forS,
		KeepFiringForSeconds: keepS,
		Labels:               labels,
		Annotations:          annotations,
		Origin:               domain.OriginPrometheusAPI,
		PrometheusURL:        "https://prom.internal",
		Confidence:           domain.ConfidenceExact,
		CandidateCount:       1,
		CapturedAt:           capturedAt,
		Fingerprint:          domain.Fingerprint(expr, forS, keepS, labels, annotations),
	}
}

func TestChangeKindValues(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "added", string(domain.ChangeAdded))
	assert.Equal(t, "removed", string(domain.ChangeRemoved))
	assert.Equal(t, "modified", string(domain.ChangeModified))
}

func TestNumberChangeDelta(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 5.0, domain.NumberChange{Old: 90, New: 95}.Delta())
	assert.Equal(t, -5.0, domain.NumberChange{Old: 95, New: 90}.Delta())
	assert.Equal(t, 0.0, domain.NumberChange{Old: 1, New: 1}.Delta())
}

// TestCompareOfIdenticalContent: two snapshots of the same rule text always diff
// to Changed=false, whenever they were captured.
func TestCompareOfIdenticalContent(t *testing.T) {
	t.Parallel()

	a := snap("up == 0", 300, 0, map[string]string{"severity": "critical"}, nil)
	b := a
	b.CapturedAt = capturedAt.AddDate(0, 6, 0)

	d := domain.Compare(a, b)
	assert.True(t, d.SameRule)
	assert.False(t, d.Changed)
	assert.True(t, d.Empty())
	assert.False(t, d.ExprChanged)
	assert.False(t, d.ExprStructural)
	assert.Nil(t, d.ExprNumbers)
	assert.False(t, d.ForChanged)
	assert.False(t, d.KeepFiringForChanged)
	assert.Empty(t, d.Labels)
	assert.Empty(t, d.Annotations)
	assert.False(t, d.OriginChanged)
	assert.Equal(t, a, d.From)
	assert.Equal(t, b, d.To)
}

// TestCompareAcrossRulesIsRecordedNotHidden: comparing two different rules is a
// category error, and Compare says so rather than pretending.
func TestCompareAcrossRulesIsRecordedNotHidden(t *testing.T) {
	t.Parallel()

	a := snap("up == 0", 0, 0, nil, nil)

	cases := []struct {
		name   string
		mutate func(k *domain.Key)
	}{
		{name: "a different source", mutate: func(k *domain.Key) { k.SourceID = "other" }},
		{name: "a different name", mutate: func(k *domain.Key) { k.Name = "Other" }},
		{name: "a different group", mutate: func(k *domain.Key) { k.Group = "other" }},
		{name: "a different file", mutate: func(k *domain.Key) { k.File = "other.yml" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := snap("up == 0", 0, 0, nil, nil)
			tc.mutate(&b.Key)
			assert.False(t, domain.Compare(a, b).SameRule)
		})
	}

	assert.True(t, domain.Compare(a, snap("up == 0", 0, 0, nil, nil)).SameRule)
}

func TestCompareDurationDeltasAreSigned(t *testing.T) {
	t.Parallel()

	from := snap("up == 0", 300, 60, nil, nil)
	to := snap("up == 0", 600, 30, nil, nil)

	d := domain.Compare(from, to)
	assert.True(t, d.Changed)
	assert.True(t, d.ForChanged)
	assert.Equal(t, 300.0, d.ForDelta)
	assert.True(t, d.KeepFiringForChanged)
	assert.Equal(t, -30.0, d.KeepFiringForDelta)
	assert.False(t, d.ExprChanged)

	// The reverse order is the same diff with the opposite sense.
	r := domain.Compare(to, from)
	assert.Equal(t, -300.0, r.ForDelta)
	assert.Equal(t, 30.0, r.KeepFiringForDelta)
}

func TestCompareOriginChanged(t *testing.T) {
	t.Parallel()

	// A generator_url capture knows no `for:`, so promoting to prometheus_api can
	// look like a threshold change when nothing was edited. The flag is what lets
	// the UI say so.
	from := snap("up == 0", 0, 0, nil, nil)
	from.Origin = domain.OriginGeneratorURL
	to := snap("up == 0", 300, 0, nil, nil)

	d := domain.Compare(from, to)
	assert.True(t, d.OriginChanged)
	assert.True(t, d.ForChanged)
	assert.Equal(t, 300.0, d.ForDelta)

	assert.False(t, domain.Compare(to, to).OriginChanged)
}

func TestDiffMaps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		from map[string]string
		to   map[string]string
		want []domain.MapChange
	}{
		{name: "both nil"},
		{name: "both empty", from: map[string]string{}, to: map[string]string{}},
		{
			name: "unchanged entries are not reported",
			from: map[string]string{"a": "1", "b": "2"},
			to:   map[string]string{"a": "1", "b": "2"},
		},
		{
			name: "added",
			to:   map[string]string{"team": "sre"},
			want: []domain.MapChange{{Kind: domain.ChangeAdded, Name: "team", New: "sre"}},
		},
		{
			name: "removed",
			from: map[string]string{"team": "sre"},
			want: []domain.MapChange{{Kind: domain.ChangeRemoved, Name: "team", Old: "sre"}},
		},
		{
			name: "modified",
			from: map[string]string{"severity": "warning"},
			to:   map[string]string{"severity": "critical"},
			want: []domain.MapChange{
				{Kind: domain.ChangeModified, Name: "severity", Old: "warning", New: "critical"},
			},
		},
		{
			name: "a value emptied is a modification, not a removal",
			from: map[string]string{"team": "sre"},
			to:   map[string]string{"team": ""},
			want: []domain.MapChange{{Kind: domain.ChangeModified, Name: "team", Old: "sre", New: ""}},
		},
		{
			name: "the union is byte-sorted, whichever side each name came from",
			from: map[string]string{"zeta": "1", "beta": "2", "gone": "3"},
			to:   map[string]string{"zeta": "9", "alpha": "0", "beta": "2"},
			want: []domain.MapChange{
				{Kind: domain.ChangeAdded, Name: "alpha", New: "0"},
				{Kind: domain.ChangeRemoved, Name: "gone", Old: "3"},
				{Kind: domain.ChangeModified, Name: "zeta", Old: "1", New: "9"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := domain.Compare(
				snap("up == 0", 0, 0, tc.from, tc.from),
				snap("up == 0", 0, 0, tc.to, tc.to),
			)
			assert.Equal(t, tc.want, d.Labels)
			assert.Equal(t, tc.want, d.Annotations)
		})
	}
}

// TestDiffMapsIsDeterministic: the change list feeds a rendered card, so a diff
// that reorders itself between two reads is a diff nobody can trust.
func TestDiffMapsIsDeterministic(t *testing.T) {
	t.Parallel()

	from := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	to := map[string]string{"a": "9", "c": "3", "f": "6", "g": "7", "h": "8"}

	first := domain.Compare(snap("x", 0, 0, from, nil), snap("x", 0, 0, to, nil)).Labels
	require.NotEmpty(t, first)
	for i := 0; i < 100; i++ {
		got := domain.Compare(snap("x", 0, 0, from, nil), snap("x", 0, 0, to, nil)).Labels
		require.Equal(t, first, got)
	}
}

// ---------------------------------------------------------------------------
// Expression comparison — the positional number claim and its safety condition
// ---------------------------------------------------------------------------

func TestCompareExpressionNumbers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		from    string
		to      string
		verdict domain.ExprVerdict
		numbers []domain.NumberChange
	}{
		{
			name:    "a threshold moved",
			from:    "node_cpu_seconds > 90",
			to:      "node_cpu_seconds > 95",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 90, New: 95}},
		},
		{
			name:    "a bare threshold, the case that must never regress",
			from:    "http_requests > 100",
			to:      "http_requests > 200",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 100, New: 200}},
		},
		{
			name:    "two thresholds moved, reported by ordinal",
			from:    "a > 1 and b < 2",
			to:      "a > 3 and b < 4",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 1, New: 3}, {Index: 1, Old: 2, New: 4}},
		},
		{
			name:    "only the second literal moved",
			from:    "a > 1 and b < 2",
			to:      "a > 1 and b < 5",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 1, Old: 2, New: 5}},
		},
		{
			name:    "a decimal threshold",
			from:    "ratio > 0.95",
			to:      "ratio > 0.99",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 0.95, New: 0.99}},
		},
		{
			name:    "a decimal with no leading zero",
			from:    "ratio > .5",
			to:      "ratio > .25",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 0.5, New: 0.25}},
		},
		{
			name:    "scientific notation",
			from:    "bytes > 1e9",
			to:      "bytes > 2e9",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 1e9, New: 2e9}},
		},
		{
			name:    "scientific notation with a signed exponent",
			from:    "ratio < 1.5e-3",
			to:      "ratio < 2.5e-3",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 1.5e-3, New: 2.5e-3}},
		},
		{
			// The sign belongs to the literal, so the delta has the sign an
			// operator would expect: -10 → -20 is a threshold moving DOWN.
			name:    "a negative threshold keeps its sign",
			from:    "delta(queue_depth) < -10",
			to:      "delta(queue_depth) < -20",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: -10, New: -20}},
		},
		{
			name:    "a negative decimal",
			from:    "temp < -0.5",
			to:      "temp < -1.25",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: -0.5, New: -1.25}},
		},
		{
			// A binary minus stays an operator: `x - 5` subtracts five, and the
			// literal is five, not minus five.
			name:    "a subtraction is not a negative literal",
			from:    "x - 5 > 0",
			to:      "x - 9 > 0",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 5, New: 9}},
		},
		{
			name:    "a reformat is not a structural change",
			from:    "a    >   90",
			to:      "a > 90",
			verdict: domain.ExprNumbersMoved,
		},
		{
			name:    "a newline is whitespace too",
			from:    "a\n>\n90",
			to:      "a > 90",
			verdict: domain.ExprNumbersMoved,
		},
		{
			name:    "a different metric is structural",
			from:    "node_cpu > 90",
			to:      "node_memory > 90",
			verdict: domain.ExprStructuralChange,
		},
		{
			name:    "a different aggregation is structural",
			from:    "sum(x) > 90",
			to:      "avg(x) > 90",
			verdict: domain.ExprStructuralChange,
		},
		{
			name:    "a new label matcher is structural",
			from:    `up{job="a"} == 0`,
			to:      `up{job="a",env="prod"} == 0`,
			verdict: domain.ExprStructuralChange,
		},
		{
			// A digit inside a matcher value is part of a string, not a number.
			name:    "a label matcher value that is a number is still a matcher",
			from:    `up{code="500"} == 0`,
			to:      `up{code="503"} == 0`,
			verdict: domain.ExprStructuralChange,
		},
		{
			name:    "a literal appearing is structural, not a drift",
			from:    "a > 90",
			to:      "a > 90 and b > 5",
			verdict: domain.ExprStructuralChange,
		},

		// ------------------------------------------------------------------
		// The digits oto refuses to read as thresholds.
		// ------------------------------------------------------------------
		{
			// THE BUG THIS TABLE EXISTS FOR. Widening a window makes an alert
			// slower, not less sensitive; "5 → 10" would say the opposite.
			name:    "a widened range window is not a threshold drift",
			from:    "rate(http_requests_total[5m]) > 100",
			to:      "rate(http_requests_total[10m]) > 100",
			verdict: domain.ExprUncharacterised,
		},
		{
			name:    "a subquery step",
			from:    "max_over_time(rate(x[5m])[30m:1m]) > 1",
			to:      "max_over_time(rate(x[5m])[30m:5m]) > 1",
			verdict: domain.ExprUncharacterised,
		},
		{
			name:    "a subquery window",
			from:    "max_over_time(rate(x[5m])[30m:1m]) > 1",
			to:      "max_over_time(rate(x[5m])[1h:1m]) > 1",
			verdict: domain.ExprUncharacterised,
		},
		{
			name:    "an offset",
			from:    "x offset 5m > 1",
			to:      "x offset 1w > 1",
			verdict: domain.ExprUncharacterised,
		},
		{
			name:    "an @ timestamp is not a threshold",
			from:    "x @ 1609746000 > 1",
			to:      "x @ 1640000000 > 1",
			verdict: domain.ExprUncharacterised,
		},
		{
			// Half an answer is worse than none: reporting 100 → 200 while
			// silently dropping [5m] → [10m] reads as "only the threshold moved".
			name:    "a threshold and a window in one edit is not half reported",
			from:    "rate(http_requests_total[5m]) > 100",
			to:      "rate(http_requests_total[10m]) > 200",
			verdict: domain.ExprUncharacterised,
		},
		{
			name:    "a compound duration",
			from:    "rate(x[1h30m]) > 1",
			to:      "rate(x[2h]) > 1",
			verdict: domain.ExprUncharacterised,
		},
		{
			// A digit inside a metric name is a metric name. Reported as
			// structural — it is a DIFFERENT METRIC — and never as "4 → 5".
			name:    "a digit inside a metric name is a different metric",
			from:    "http_4xx_total > 10",
			to:      "http_5xx_total > 10",
			verdict: domain.ExprStructuralChange,
		},
		{
			name:    "a digit inside a recording rule name",
			from:    "job:http_requests:rate5m > 1",
			to:      "job:http_requests:rate10m > 1",
			verdict: domain.ExprStructuralChange,
		},

		// ------------------------------------------------------------------
		// …and the thresholds it still reads precisely, alongside them.
		// ------------------------------------------------------------------
		{
			// The window is NOT in the Index space, so the threshold is index 0
			// and not index 1. This is what makes the ordinals usable.
			name:    "a threshold beside an untouched window is still exact",
			from:    "rate(http_requests_total[5m]) > 100",
			to:      "rate(http_requests_total[5m]) > 200",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 100, New: 200}},
		},
		{
			name:    "a threshold beside a digit-carrying metric name",
			from:    "http_5xx_total > 10",
			to:      "http_5xx_total > 25",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 10, New: 25}},
		},
		{
			name:    "a threshold in scientific notation beside a window",
			from:    "sum(rate(bytes_total[5m])) > 1e9",
			to:      "sum(rate(bytes_total[5m])) > 2e9",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 1e9, New: 2e9}},
		},
		{
			name:    "a function argument is a bare literal too",
			from:    "histogram_quantile(0.95, rate(x[5m])) > 1",
			to:      "histogram_quantile(0.99, rate(x[5m])) > 1",
			verdict: domain.ExprNumbersMoved,
			numbers: []domain.NumberChange{{Index: 0, Old: 0.95, New: 0.99}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := domain.Compare(snap(tc.from, 0, 0, nil, nil), snap(tc.to, 0, 0, nil, nil))
			require.True(t, d.ExprChanged)
			assert.Equal(t, tc.verdict, d.ExprVerdict)
			assert.Equal(t, tc.numbers, d.ExprNumbers)

			// ExprStructural is the derived "show no drift narrative" flag: true
			// for everything that is not a vouched-for numeric move.
			assert.Equal(t, tc.verdict != domain.ExprNumbersMoved, d.ExprStructural)
			if tc.verdict != domain.ExprNumbersMoved {
				assert.Nil(t, d.ExprNumbers,
					"no number claim may be made unless the verdict is ExprNumbersMoved")
			}
		})
	}
}

// TestCompareExpressionIsSymmetricInSense: reversing the arguments reverses each
// reported change and nothing else. A diff whose verdict depended on argument
// order would be a diff nobody could reason about.
func TestCompareExpressionIsSymmetricInSense(t *testing.T) {
	t.Parallel()

	from, to := snap("rate(x[5m]) > 100", 0, 0, nil, nil), snap("rate(x[5m]) > 200", 0, 0, nil, nil)

	forward := domain.Compare(from, to)
	backward := domain.Compare(to, from)
	assert.Equal(t, domain.ExprNumbersMoved, backward.ExprVerdict)
	assert.Equal(t, []domain.NumberChange{{Index: 0, Old: 100, New: 200}}, forward.ExprNumbers)
	assert.Equal(t, []domain.NumberChange{{Index: 0, Old: 200, New: 100}}, backward.ExprNumbers)
	assert.Equal(t, 100.0, forward.ExprNumbers[0].Delta())
	assert.Equal(t, -100.0, backward.ExprNumbers[0].Delta())
}

// TestCompareLeavesExpressionAloneWhenItDidNotChange: no positional claim is
// even attempted when the text is identical.
func TestCompareLeavesExpressionAloneWhenItDidNotChange(t *testing.T) {
	t.Parallel()

	d := domain.Compare(snap("a > 90", 0, 0, nil, nil), snap("a > 90", 300, 0, nil, nil))
	assert.False(t, d.ExprChanged)
	assert.False(t, d.ExprStructural)
	assert.Nil(t, d.ExprNumbers)
}

// TestDurationsAndEmbeddedDigitsAreNotThresholds is the regression test for the
// defect that motivated ExprVerdict.
//
// The old `numberLiteral` regexp carried no word boundary, so the `5` inside
// `[5m]` was extracted as a numeric literal and widening a range selector was
// reported as an in-place threshold drift — exactly the "confident wrong answer"
// the NumberChange doc comment says oto refuses to give. The same hole matched
// digits inside metric and label names (`http_5xx_total`).
//
// Now: a window edit is ExprUncharacterised (changed, no claim about what), a
// metric rename is ExprStructuralChange, and neither offers a number.
func TestDurationsAndEmbeddedDigitsAreNotThresholds(t *testing.T) {
	t.Parallel()

	d := domain.Compare(
		snap("rate(http_requests_total[5m]) > 100", 0, 0, nil, nil),
		snap("rate(http_requests_total[10m]) > 100", 0, 0, nil, nil),
	)
	require.True(t, d.ExprChanged)
	assert.True(t, d.ExprStructural,
		"widening a range selector is not a threshold drift")
	assert.Nil(t, d.ExprNumbers)
	assert.Equal(t, domain.ExprUncharacterised, d.ExprVerdict,
		"the window moved, and oto says so without claiming which number it was")

	// And a metric rename that only differs in an embedded digit must not read as
	// a numeric drift either.
	d2 := domain.Compare(
		snap("http_4xx_total > 10", 0, 0, nil, nil),
		snap("http_5xx_total > 10", 0, 0, nil, nil),
	)
	assert.True(t, d2.ExprStructural)
	assert.Nil(t, d2.ExprNumbers)
	assert.Equal(t, domain.ExprStructuralChange, d2.ExprVerdict)
}

// ---------------------------------------------------------------------------
// The seam
// ---------------------------------------------------------------------------

// TestExprVerdictNotComparedWhenExpressionIsUnchanged: the zero value means "no
// comparison happened", and it is what a Diff carries when only `for:` moved.
func TestExprVerdictNotComparedWhenExpressionIsUnchanged(t *testing.T) {
	t.Parallel()

	d := domain.Compare(snap("a > 90", 0, 0, nil, nil), snap("a > 90", 300, 0, nil, nil))
	require.False(t, d.ExprChanged)
	assert.Equal(t, domain.ExprNotCompared, d.ExprVerdict)
	assert.Equal(t, domain.ExprVerdict(""), d.ExprVerdict, "the zero value carries the meaning")
}

// alwaysUnsure is a stand-in for the parser-backed ExprComparer that has not been
// written yet: the point of the test is that substituting one is a CompareWith
// argument and touches nothing else.
type alwaysUnsure struct{ calls *int }

func (a alwaysUnsure) CompareExpr(_, _ string) domain.ExprDiff {
	*a.calls++
	return domain.ExprDiff{Verdict: domain.ExprUncharacterised}
}

// TestCompareWithSubstitutedExprComparer: a different implementation of the seam
// changes the expression verdict and NOTHING ELSE about the diff.
func TestCompareWithSubstitutedExprComparer(t *testing.T) {
	t.Parallel()

	from := snap("a > 90", 300, 0, map[string]string{"severity": "warning"}, nil)
	to := snap("a > 95", 600, 0, map[string]string{"severity": "critical"}, nil)

	calls := 0
	d := domain.CompareWith(from, to, alwaysUnsure{calls: &calls})
	assert.Equal(t, 1, calls, "the comparer is consulted once, and only for a changed expression")
	assert.Equal(t, domain.ExprUncharacterised, d.ExprVerdict)
	assert.True(t, d.ExprStructural)
	assert.Nil(t, d.ExprNumbers)

	// Everything the seam does not own is untouched by the substitution.
	shipped := domain.Compare(from, to)
	assert.Equal(t, shipped.Labels, d.Labels)
	assert.Equal(t, shipped.ForDelta, d.ForDelta)
	assert.Equal(t, shipped.Changed, d.Changed)
	assert.Equal(t, domain.ExprNumbersMoved, shipped.ExprVerdict)

	// An unchanged expression is not offered to the comparer at all.
	calls = 0
	domain.CompareWith(from, from, alwaysUnsure{calls: &calls})
	assert.Equal(t, 0, calls)
}

// TestLexicalExprComparerIsTheShippedSeam exercises the implementation directly,
// so the contract is tested where it is stated rather than only through Compare.
func TestLexicalExprComparerIsTheShippedSeam(t *testing.T) {
	t.Parallel()

	var c domain.ExprComparer = domain.LexicalExprComparer{}

	assert.Equal(t, domain.ExprDiff{
		Verdict: domain.ExprNumbersMoved,
		Numbers: []domain.NumberChange{{Index: 0, Old: 1, New: 2}},
	}, c.CompareExpr("a > 1", "a > 2"))

	assert.Equal(t, domain.ExprDiff{Verdict: domain.ExprUncharacterised},
		c.CompareExpr("rate(a[5m])", "rate(a[10m])"))
	assert.Equal(t, domain.ExprDiff{Verdict: domain.ExprStructuralChange},
		c.CompareExpr("a > 1", "b > 1"))

	// An empty expression is legal input: an `unavailable` snapshot has one, and
	// gaining a definition is a structural change, not a drift.
	assert.Equal(t, domain.ExprDiff{Verdict: domain.ExprStructuralChange},
		c.CompareExpr("", "a > 1"))

	// Determinism, because this feeds a rendered timeline.
	for i := 0; i < 50; i++ {
		require.Equal(t, c.CompareExpr("a > 1 and b[5m] > 2", "a > 3 and b[5m] > 2"),
			c.CompareExpr("a > 1 and b[5m] > 2", "a > 3 and b[5m] > 2"))
	}
}
