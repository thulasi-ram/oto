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
		name       string
		from       string
		to         string
		structural bool
		numbers    []domain.NumberChange
	}{
		{
			name:    "a threshold moved",
			from:    "node_cpu_seconds > 90",
			to:      "node_cpu_seconds > 95",
			numbers: []domain.NumberChange{{Index: 0, Old: 90, New: 95}},
		},
		{
			name:    "two thresholds moved, reported by ordinal",
			from:    "a > 1 and b < 2",
			to:      "a > 3 and b < 4",
			numbers: []domain.NumberChange{{Index: 0, Old: 1, New: 3}, {Index: 1, Old: 2, New: 4}},
		},
		{
			name:    "only the second literal moved",
			from:    "a > 1 and b < 2",
			to:      "a > 1 and b < 5",
			numbers: []domain.NumberChange{{Index: 1, Old: 2, New: 5}},
		},
		{
			name:    "a decimal threshold",
			from:    "ratio > 0.95",
			to:      "ratio > 0.99",
			numbers: []domain.NumberChange{{Index: 0, Old: 0.95, New: 0.99}},
		},
		{
			name:    "scientific notation",
			from:    "bytes > 1e9",
			to:      "bytes > 2e9",
			numbers: []domain.NumberChange{{Index: 0, Old: 1e9, New: 2e9}},
		},
		{
			name: "a reformat is not a structural change",
			from: "a    >   90",
			to:   "a > 90",
		},
		{
			name: "a newline is whitespace too",
			from: "a\n>\n90",
			to:   "a > 90",
		},
		{
			name:       "a different metric is structural",
			from:       "node_cpu > 90",
			to:         "node_memory > 90",
			structural: true,
		},
		{
			name:       "a different aggregation is structural",
			from:       "sum(x) > 90",
			to:         "avg(x) > 90",
			structural: true,
		},
		{
			name:       "a new label matcher is structural",
			from:       `up{job="a"} == 0`,
			to:         `up{job="a",env="prod"} == 0`,
			structural: true,
		},
		{
			name:       "a literal appearing is structural, not a drift",
			from:       "a > 90",
			to:         "a > 90 and b > 5",
			structural: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := domain.Compare(snap(tc.from, 0, 0, nil, nil), snap(tc.to, 0, 0, nil, nil))
			require.True(t, d.ExprChanged)
			assert.Equal(t, tc.structural, d.ExprStructural)
			assert.Equal(t, tc.numbers, d.ExprNumbers)
			if tc.structural {
				assert.Nil(t, d.ExprNumbers,
					"the threshold-drift narrative must not be shown for a structural change")
			}
		})
	}
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

// TestBUGNumberLiteralMatchesDurations demonstrates a genuine defect.
//
// The doc comment on `numberLiteral` states it "deliberately does not match
// durations (`5m`), which are not thresholds". The pattern
// `-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?` carries no word boundary, so the `5` inside
// `[5m]` IS captured as a numeric literal. Widening a range selector therefore
// reports as an in-place threshold drift with ExprStructural=false — exactly the
// "confident wrong answer" the NumberChange doc comment says oto refuses to
// give. The same hole matches digits inside metric and label names
// (`http_5xx_total`, `node_cpu_seconds`).
func TestBUGNumberLiteralMatchesDurations(t *testing.T) {
	t.Skip("BUG: numberLiteral has no word boundary, so durations and digits inside identifiers " +
		"are extracted as thresholds, contradicting its own doc comment " +
		"(internal/rules/domain/diff.go:166 vs diff.go:36-47)")

	d := domain.Compare(
		snap("rate(http_requests_total[5m]) > 100", 0, 0, nil, nil),
		snap("rate(http_requests_total[10m]) > 100", 0, 0, nil, nil),
	)
	require.True(t, d.ExprChanged)
	assert.True(t, d.ExprStructural,
		"widening a range selector is not a threshold drift")
	assert.Nil(t, d.ExprNumbers)

	// And a metric rename that only differs in an embedded digit must not read as
	// a numeric drift either.
	d2 := domain.Compare(
		snap("http_4xx_total > 10", 0, 0, nil, nil),
		snap("http_5xx_total > 10", 0, 0, nil, nil),
	)
	assert.True(t, d2.ExprStructural)
	assert.Nil(t, d2.ExprNumbers)
}
